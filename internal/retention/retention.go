// Package retention remove, por idade, os dados técnicos que deixaram de ser
// necessários: registros de deduplicação, mensagens tratadas, históricos de
// entrega, conteúdos de notificações, corpos de webhooks recebidos, links do
// portal, convites vencidos e eventos de consumo já faturados.
//
// O que pode ser removido é decidido no banco (funções app_retention_tenants
// e app_retention_purge): nada pendente, aberto, em andamento, não faturado
// ou dentro da tolerância de correção é tocado, qualquer que seja o prazo
// configurado. Este pacote decide quando e quanto: calcula o corte de cada
// tipo pelo relógio injetado, percorre os tenants em rodízio e remove em
// lotes limitados, cada lote na sua própria transação do tenant.
//
// Registros financeiros e de auditoria (ledger, faturas, itens, pagamentos,
// notas de crédito, créditos, histórico de assinaturas e trilha de auditoria)
// não têm tipo de retenção: são mantidos de propósito (ADR-060).
package retention

import (
	"errors"
	"fmt"
	"time"
)

// Kind é um tipo de dado sujeito a expurgo. O valor é o rótulo kind das
// métricas e o parâmetro das funções do banco.
type Kind string

// Tipos de dado com expurgo automático.
const (
	// KindInboxMessages são os registros de deduplicação dos consumidores.
	KindInboxMessages Kind = "inbox_messages"
	// KindDeadLetters são as mensagens não processadas já tratadas
	// (reprocessadas ou descartadas).
	KindDeadLetters Kind = "dead_letters"
	// KindWebhookDeliveries são as entregas concluídas de webhooks enviados
	// (com as tentativas).
	KindWebhookDeliveries Kind = "webhook_deliveries"
	// KindWebhookEvents são os eventos de webhooks enviados sem entregas
	// restantes.
	KindWebhookEvents Kind = "webhook_events"
	// KindWebhookEndpointSecrets são os segredos de assinatura vencidos.
	KindWebhookEndpointSecrets Kind = "webhook_endpoint_secrets"
	// KindProviderWebhookPayloads são os corpos dos webhooks recebidos dos
	// provedores e já processados (a linha permanece, sem o corpo).
	KindProviderWebhookPayloads Kind = "provider_webhook_payloads"
	// KindNotificationContents são os conteúdos cifrados das notificações
	// encerradas (a linha permanece, sem o conteúdo).
	KindNotificationContents Kind = "notification_contents"
	// KindNotifications são as notificações encerradas, com o histórico.
	KindNotifications Kind = "notifications"
	// KindPortalLinks são os links do portal com a validade encerrada.
	KindPortalLinks Kind = "portal_links"
	// KindUsageEvents são os eventos de consumo de períodos já faturados e
	// fora da tolerância de correção.
	KindUsageEvents Kind = "usage_events"
	// KindInvitations são os convites não aceitos com a validade encerrada.
	KindInvitations Kind = "invitations"
)

// Kinds lista os tipos na ordem de execução. As entregas de webhooks vêm
// antes dos eventos (um evento só sai quando não tem mais entregas) e o
// conteúdo das notificações antes das notificações.
func Kinds() []Kind {
	return []Kind{KindInboxMessages, KindDeadLetters, KindWebhookDeliveries, KindWebhookEvents, KindWebhookEndpointSecrets,
		KindProviderWebhookPayloads, KindNotificationContents, KindNotifications, KindPortalLinks, KindUsageEvents, KindInvitations}
}

const day = 24 * time.Hour

// Days converte dias em duração.
func Days(n int) time.Duration { return time.Duration(n) * day }

// MaxRetention é o maior prazo aceito para qualquer tipo.
const MaxRetention = 3650 * day

// Limites do tamanho do lote e padrões de execução.
const (
	MinBatchSize     = 10
	MaxBatchSize     = 5000
	DefaultBatchSize = 500
	// DefaultRunBudget limita o tempo de uma execução; o restante fica para
	// a execução seguinte.
	DefaultRunBudget = 2 * time.Minute
	// DefaultMaxBatchesPerTenant limita os lotes de um tipo por tenant numa
	// execução, para que um tenant com muito acúmulo não retenha os demais.
	DefaultMaxBatchesPerTenant = 20
)

// Policy define o prazo de retenção de cada grupo de dados (contado do
// instante em que o registro deixou de ser necessário) e os limites de
// execução.
type Policy struct {
	// Inbox: registros de deduplicação, a partir do processamento.
	Inbox time.Duration
	// DeadLetters: mensagens tratadas, a partir do tratamento.
	DeadLetters time.Duration
	// Webhooks: entregas concluídas (a partir da conclusão), eventos sem
	// entregas (a partir da criação) e segredos vencidos (a partir do
	// vencimento).
	Webhooks time.Duration
	// ProviderWebhookPayloads: corpo dos webhooks recebidos, a partir do
	// processamento.
	ProviderWebhookPayloads time.Duration
	// NotificationContent: conteúdo das notificações, a partir do
	// encerramento (envio, falha, cancelamento ou supressão).
	NotificationContent time.Duration
	// Notifications: notificações e histórico, a partir do encerramento.
	Notifications time.Duration
	// PortalLinks: links do portal, a partir do fim da validade.
	PortalLinks time.Duration
	// UsageEvents: eventos de consumo faturados, a partir do fim da
	// tolerância de atraso do período.
	UsageEvents time.Duration
	// Invitations: convites não aceitos, a partir do fim da validade.
	Invitations time.Duration

	// BatchSize é o número máximo de registros por transação.
	BatchSize int
	// RunBudget limita o tempo de uma execução, dividido igualmente entre
	// os tipos.
	RunBudget time.Duration
	// MaxBatchesPerTenant limita os lotes de um tipo por tenant e execução.
	MaxBatchesPerTenant int
	// TenantPageSize é o tamanho das páginas de descoberta (zero usa o
	// padrão de tenantscan).
	TenantPageSize int
}

// DefaultPolicy é a política padrão, conservadora: nada é removido antes de
// 90 dias, o histórico de notificações fica um ano e o consumo faturado, 400
// dias (mais que um ciclo anual).
func DefaultPolicy() Policy {
	return Policy{
		Inbox: Days(90), DeadLetters: Days(180), Webhooks: Days(90), ProviderWebhookPayloads: Days(90),
		NotificationContent: Days(90), Notifications: Days(365), PortalLinks: Days(90), UsageEvents: Days(400), Invitations: Days(90),
		BatchSize: DefaultBatchSize, RunBudget: DefaultRunBudget, MaxBatchesPerTenant: DefaultMaxBatchesPerTenant,
	}
}

// For devolve o prazo de retenção do tipo (zero para tipo desconhecido).
func (p Policy) For(k Kind) time.Duration {
	switch k {
	case KindInboxMessages:
		return p.Inbox
	case KindDeadLetters:
		return p.DeadLetters
	case KindWebhookDeliveries, KindWebhookEvents, KindWebhookEndpointSecrets:
		return p.Webhooks
	case KindProviderWebhookPayloads:
		return p.ProviderWebhookPayloads
	case KindNotificationContents:
		return p.NotificationContent
	case KindNotifications:
		return p.Notifications
	case KindPortalLinks:
		return p.PortalLinks
	case KindUsageEvents:
		return p.UsageEvents
	case KindInvitations:
		return p.Invitations
	}
	return 0
}

// Minimum devolve o menor prazo aceito para o tipo:
//   - inbox: 14 dias, o dobro da retenção da outbox publicada (7 dias), de
//     modo que uma mensagem reentregue pelo broker ou reprocessada depois de
//     uma restauração ainda encontre o registro de deduplicação;
//   - dead letters tratadas e notificações: 30 dias de histórico de operação;
//   - consumo faturado: 30 dias além da tolerância de atraso, para apurar
//     contestações do período recém-faturado;
//   - links do portal: 1 dia depois da validade;
//   - demais: 7 dias.
func Minimum(k Kind) time.Duration {
	switch k {
	case KindInboxMessages:
		return Days(14)
	case KindDeadLetters, KindNotifications, KindUsageEvents:
		return Days(30)
	case KindPortalLinks:
		return Days(1)
	}
	return Days(7)
}

// Validate verifica os prazos mínimos e máximos e os limites de execução.
func (p Policy) Validate() error {
	var errs []error
	for _, k := range Kinds() {
		if d := p.For(k); d < Minimum(k) || d > MaxRetention {
			errs = append(errs, fmt.Errorf("retention: prazo de %s deve ficar entre %d e %d dias", k, int(Minimum(k)/day), int(MaxRetention/day)))
		}
	}
	if p.Notifications < p.NotificationContent {
		errs = append(errs, errors.New("retention: o prazo das notificações não pode ser menor que o do conteúdo"))
	}
	if p.BatchSize < MinBatchSize || p.BatchSize > MaxBatchSize {
		errs = append(errs, fmt.Errorf("retention: lote deve ficar entre %d e %d registros", MinBatchSize, MaxBatchSize))
	}
	if p.RunBudget <= 0 {
		errs = append(errs, errors.New("retention: orçamento de execução deve ser positivo"))
	}
	if p.MaxBatchesPerTenant <= 0 {
		errs = append(errs, errors.New("retention: limite de lotes por tenant deve ser positivo"))
	}
	if p.TenantPageSize < 0 {
		errs = append(errs, errors.New("retention: tamanho da página de tenants inválido"))
	}
	return errors.Join(errs...)
}
