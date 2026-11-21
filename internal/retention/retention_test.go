package retention

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultPolicyIsValidAndConservative(t *testing.T) {
	p := DefaultPolicy()
	if err := p.Validate(); err != nil {
		t.Fatalf("política padrão inválida: %v", err)
	}
	for _, k := range Kinds() {
		if p.For(k) < Days(90) {
			t.Errorf("%s: prazo padrão de %s, menor que 90 dias", k, p.For(k))
		}
		if p.For(k) < Minimum(k) {
			t.Errorf("%s: prazo padrão abaixo do mínimo", k)
		}
	}
	if p.For("desconhecido") != 0 {
		t.Error("tipo desconhecido deveria ter prazo zero")
	}
	if Minimum("desconhecido") != Days(7) {
		t.Error("mínimo geral deveria ser de 7 dias")
	}
}

func TestKindsOrderKeepsDependencies(t *testing.T) {
	pos := map[Kind]int{}
	for i, k := range Kinds() {
		if _, dup := pos[k]; dup {
			t.Fatalf("tipo repetido: %s", k)
		}
		pos[k] = i
	}
	if len(pos) != 11 {
		t.Fatalf("tipos: %d", len(pos))
	}
	if pos[KindWebhookDeliveries] > pos[KindWebhookEvents] {
		t.Error("as entregas precisam sair antes dos eventos de webhook")
	}
	if pos[KindNotificationContents] > pos[KindNotifications] {
		t.Error("o conteúdo precisa sair antes das notificações")
	}
}

func TestPolicyValidate(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Policy)
		want   string // trecho esperado do erro; vazio = válida
	}{
		{"padrão", func(*Policy) {}, ""},
		{"mínimos exatos", func(p *Policy) {
			p.Inbox, p.DeadLetters, p.Webhooks, p.ProviderWebhookPayloads = Days(14), Days(30), Days(7), Days(7)
			p.NotificationContent, p.Notifications, p.PortalLinks, p.UsageEvents, p.Invitations = Days(7), Days(30), Days(1), Days(30), Days(7)
			p.BatchSize = MinBatchSize
		}, ""},
		{"máximos exatos", func(p *Policy) { p.Inbox, p.BatchSize = MaxRetention, MaxBatchSize }, ""},
		{"inbox abaixo do mínimo", func(p *Policy) { p.Inbox = Days(13) }, "prazo de inbox_messages deve ficar entre 14 e 3650 dias"},
		{"dead letters abaixo do mínimo", func(p *Policy) { p.DeadLetters = Days(29) }, "prazo de dead_letters"},
		{"webhooks abaixo do mínimo", func(p *Policy) { p.Webhooks = Days(6) }, "prazo de webhook_deliveries"},
		{"webhooks: eventos e segredos seguem o mesmo prazo", func(p *Policy) { p.Webhooks = Days(6) }, "prazo de webhook_endpoint_secrets"},
		{"corpo de webhooks recebidos abaixo do mínimo", func(p *Policy) { p.ProviderWebhookPayloads = Days(1) }, "prazo de provider_webhook_payloads"},
		{"conteúdo abaixo do mínimo", func(p *Policy) { p.NotificationContent = Days(6) }, "prazo de notification_contents"},
		{"notificações abaixo do mínimo", func(p *Policy) { p.Notifications, p.NotificationContent = Days(29), Days(7) }, "prazo de notifications"},
		{"links abaixo do mínimo", func(p *Policy) { p.PortalLinks = 12 * time.Hour }, "prazo de portal_links deve ficar entre 1 e 3650 dias"},
		{"consumo abaixo do mínimo", func(p *Policy) { p.UsageEvents = Days(29) }, "prazo de usage_events"},
		{"convites abaixo do mínimo", func(p *Policy) { p.Invitations = 0 }, "prazo de invitations"},
		{"acima do máximo", func(p *Policy) { p.DeadLetters = MaxRetention + day }, "prazo de dead_letters"},
		{"notificações antes do conteúdo", func(p *Policy) { p.NotificationContent, p.Notifications = Days(120), Days(100) },
			"não pode ser menor que o do conteúdo"},
		{"lote pequeno", func(p *Policy) { p.BatchSize = MinBatchSize - 1 }, "lote deve ficar entre 10 e 5000"},
		{"lote grande", func(p *Policy) { p.BatchSize = MaxBatchSize + 1 }, "lote deve ficar entre 10 e 5000"},
		{"orçamento", func(p *Policy) { p.RunBudget = 0 }, "orçamento de execução"},
		{"lotes por tenant", func(p *Policy) { p.MaxBatchesPerTenant = 0 }, "limite de lotes por tenant"},
		{"página de tenants", func(p *Policy) { p.TenantPageSize = -1 }, "página de tenants"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultPolicy()
			tc.change(&p)
			err := p.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("erro inesperado: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("erro %v, esperado conter %q", err, tc.want)
			}
		})
	}
}
