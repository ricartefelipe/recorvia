// Package reporting calcula os indicadores do dashboard financeiro. As
// fórmulas ficam explícitas em Indicators, são devolvidas à interface e estão
// documentadas em docs/arquitetura/indicadores.md (ADR-074).
//
// Receita faturada (emitida) e recebida (paga) são indicadores distintos. O
// MRR considera apenas o componente recorrente fixo, líquido de descontos
// recorrentes; consumo variável, excedente e valores avulsos ficam fora da
// recorrência. Cada indicador tem uma base temporal (Basis): fluxo do mês,
// posição no fim do mês ou posição atual.
package reporting

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// MRRComponent é o componente recorrente fixo de uma periodicidade: soma
// exata do componente fixo vigente (com reajuste) e soma exata dos descontos
// recorrentes, em centavos.
type MRRComponent struct {
	Interval calendar.Interval
	Gross    *big.Rat
	Discount *big.Rat
}

// Raw são as agregações lidas do banco. Os campos "AtPosition" e os saldos
// de recebíveis, inadimplência e valores a recuperar são do instante da
// posição; MRR, operações desconhecidas e divergências são da posição atual.
type Raw struct {
	MRR                []MRRComponent
	Billed             int64
	BilledRecurring    int64
	BilledUsage        int64
	Received           int64
	OpenReceivables    int64
	OverdueReceivables int64
	DelinquencyBase    int64
	DelinquencyOpen    int64
	ActiveAtPosition   int
	StartedInPeriod    int
	EndedInPeriod      int
	ActiveAtStart      int
	ChurnedInPeriod    int
	Refunds            int64
	Chargebacks        int64
	OpenRecoverables   int64
	// PendingProviderReceipt é o saldo, na posição, dos pagamentos
	// confirmados no provedor e ainda não creditados por ele (ADR-080).
	PendingProviderReceipt int64
	UnknownOperations      int
	OpenDiscrepancies      int
}

// Basis é a base temporal de um indicador.
type Basis string

const (
	// BasisMonth: fluxo do mês consultado, no intervalo [início, fim).
	BasisMonth Basis = "month"
	// BasisMonthEnd: posição no último instante do mês consultado ou, no mês
	// corrente, no momento da consulta (Dashboard.PositionAt).
	BasisMonthEnd Basis = "month_end"
	// BasisCurrent: posição atual, seja qual for o mês consultado (não há
	// histórico que permita reconstruí-la com fidelidade).
	BasisCurrent Basis = "current"
)

// Dashboard são os indicadores calculados.
type Dashboard struct {
	Month                 string
	Currency              string
	PositionAt            time.Time
	MRR                   int64
	ARR                   int64
	Billed                int64
	BilledRecurring       int64
	BilledUsage           int64
	Received              int64
	OpenReceivables       int64
	OverdueReceivables    int64
	DelinquencyRateBP     int64
	ChurnRateBP           int64
	ActiveSubscriptions   int
	NewSubscriptions      int
	CanceledSubscriptions int
	Refunds               int64
	Chargebacks           int64
	OpenRecoverables      int64
	// PendingProviderReceipt: parte de Recebido ainda não creditada pelo
	// provedor, em PositionAt.
	PendingProviderReceipt int64
	OpenDiscrepancies      int
	UnknownOperations      int
	Definitions            map[string]string
	Bases                  map[string]Basis
}

// Indicator descreve um indicador devolvido pelo painel. Key é o nome do
// campo na resposta da API e a chave em Definitions e Bases.
type Indicator struct {
	Key        string
	Basis      Basis
	Definition string
}

const (
	monthEndNote = " Posição no último instante do mês consultado (no mês corrente, no momento da consulta)."
	currentNote  = " Posição atual: não muda com o mês consultado."
)

// Indicators são os indicadores do painel, na ordem da resposta, com as
// fórmulas exibidas na interface (docs/arquitetura/indicadores.md).
var Indicators = []Indicator{
	{"mrr", BasisCurrent, "MRR: soma, nas assinaturas ativas ou em atraso, do componente recorrente fixo mensal líquido de " +
		"descontos recorrentes. Componente fixo: valor fixo do plano (modelos fixo e fixo com excedente) ou preço por unidade × " +
		"quantidade, com o reajuste vigente da assinatura. Desconto recorrente: cupom ativo de duração permanente ou por ciclos " +
		"(o de uso único fica fora), percentual sobre o componente fixo ou valor fixo limitado a ele. Planos anuais entram " +
		"divididos por 12. Exclui trial, suspensas, consumo medido, excedente e cobranças avulsas." + currentNote},
	{"arr", BasisCurrent, "ARR: MRR × 12, calculado sobre o valor exato do MRR e arredondado uma única vez." + currentNote},
	{"billed", BasisMonth, "Receita faturada: total, após descontos, das faturas emitidas no mês (pela data de emissão); exclui " +
		"rascunhos e faturas canceladas. Inclui recorrente, consumo, prorratas e ajustes."},
	{"billed_recurring", BasisMonth, "Faturado recorrente: parte da receita faturada nos itens de mensalidade (componente fixo do " +
		"ciclo), líquida dos descontos distribuídos a esses itens."},
	{"billed_usage", BasisMonth, "Faturado por consumo: parte da receita faturada nos itens de consumo medido e de excedente de " +
		"franquia, líquida dos descontos distribuídos a esses itens."},
	{"received", BasisMonth, "Receita recebida: soma dos pagamentos confirmados no mês (pela data de pagamento), via provedor ou " +
		"registro manual. Inclui os pagamentos confirmados pelo provedor e ainda não creditados por ele (por exemplo, cartão no " +
		"prazo de recebimento), mostrados em A receber do provedor."},
	{"pending_provider_receipt", BasisMonthEnd, "A receber do provedor: saldo da conta de pagamentos confirmados no provedor, isto " +
		"é, pagamentos já contados em Receita recebida cujo valor o provedor ainda não creditou. Quando o provedor informa o " +
		"recebimento, o valor bruto passa aos valores em liquidação, a taxa é reconhecida e o líquido vai para o disponível " +
		"liquidado (ADR-080)." + monthEndNote},
	{"open_receivables", BasisMonthEnd, "Recebíveis em aberto: saldo de contas a receber no ledger por fatura (emissão menos " +
		"crédito aplicado, pagamentos, notas de crédito e baixas como incobrável, com os estornos), somadas as faturas com saldo." +
		monthEndNote},
	{"overdue_receivables", BasisMonthEnd, "Recebíveis vencidos: parte dos recebíveis em aberto das faturas com vencimento até o " +
		"instante da posição." + monthEndNote},
	{"delinquency_rate_bp", BasisMonthEnd, "Inadimplência: saldo em aberto das faturas com vencimento nos 90 dias anteriores à " +
		"posição ÷ total dessas faturas (emitidas e não canceladas até a posição), em pontos-base." + monthEndNote},
	{"churn_rate_bp", BasisMonth, "Churn de contratos: das assinaturas vigentes fora de trial no início do mês (ativas, em atraso " +
		"ou suspensas pela situação registrada no histórico ou, antes do primeiro registro, pelas datas de início e de fim do " +
		"trial), as encerradas no mês ÷ total delas, em pontos-base. Canceladas durante o trial e iniciadas no próprio mês não " +
		"entram no numerador nem no denominador."},
	{"active_subscriptions", BasisMonthEnd, "Assinaturas ativas: assinaturas em trial, ativas ou em atraso, pela situação " +
		"registrada no histórico (antes do primeiro registro, pelas datas de início e de fim do trial); suspensas e encerradas " +
		"ficam fora. Uma assinatura cadastrada depois com início retroativo passa a contar, então o valor de um mês encerrado " +
		"pode mudar." + monthEndNote},
	{"new_subscriptions", BasisMonth, "Novas no mês: assinaturas com início do contrato no mês, inclusive as iniciadas em trial. " +
		"O início retroativo conta no mês do início, então o valor de um mês encerrado pode mudar quando uma assinatura " +
		"retroativa é cadastrada depois."},
	{"canceled_subscriptions", BasisMonth, "Canceladas no mês: assinaturas encerradas no mês (pela data de encerramento), " +
		"inclusive as canceladas durante o trial e as iniciadas no próprio mês."},
	{"refunds", BasisMonth, "Reembolsos: valor líquido lançado no ledger no mês na conta de reembolsos (reembolsos efetivados, " +
		"pela data da confirmação)."},
	{"chargebacks", BasisMonth, "Chargebacks: valor líquido lançado no ledger no mês na conta de chargebacks (débitos menos " +
		"créditos). A abertura da disputa retém o valor contestado; a disputa ganha estorna a retenção e a perdida libera a parte " +
		"reembolsada ou reservada a reembolso e a parte que era pagamento excedente (crédito do cliente consumido ou valor a " +
		"recuperar dele), o que reduz o valor do mês em que ocorrem (negativo quando a disputa aberta em mês anterior é ganha). No " +
		"acumulado dos meses, somado aos reembolsos, nunca passa do valor pago. A baixa do valor a recuperar não altera este indicador."},
	{"open_recoverables", BasisMonthEnd, "Valores a recuperar: saldo da conta de valores a recuperar de clientes, isto é, o " +
		"crédito de pagamento excedente que o cliente já havia usado quando o pagamento foi estornado por chargeback, menos as " +
		"baixas. A baixa recebida (paga pelo cliente fora do provedor) vai para o disponível liquidado e não entra em Recebido, " +
		"que soma só pagamentos de faturas; a baixa perdida é reconhecida como perda com inadimplência." + monthEndNote},
	{"open_discrepancies", BasisCurrent, "Divergências abertas: divergências de conciliação ainda sem resolução nem descarte, " +
		"de qualquer mês." + currentNote},
	{"unknown_operations", BasisCurrent, "Operações com resultado desconhecido: operações de pagamento (criação de cobrança ou " +
		"reembolso) cujo resultado no provedor ainda não foi confirmado." + currentNote},
}

// Definitions descreve as fórmulas exibidas na interface, por indicador.
var Definitions = func() map[string]string {
	m := make(map[string]string, len(Indicators))
	for _, i := range Indicators {
		m[i.Key] = i.Definition
	}
	return m
}()

// Bases informa a base temporal de cada indicador.
var Bases = func() map[string]Basis {
	m := make(map[string]Basis, len(Indicators))
	for _, i := range Indicators {
		m[i.Key] = i.Basis
	}
	return m
}()

func ratio(num, den int64) int64 {
	if den <= 0 || num <= 0 {
		return 0
	}
	return money.RoundRat(big.NewRat(num*10000, den), money.HalfEven).Int64()
}

// Compute aplica as fórmulas sobre as agregações.
func Compute(month string, positionAt time.Time, raw Raw) (Dashboard, error) {
	mrr := new(big.Rat)
	for _, c := range raw.MRR {
		v := new(big.Rat)
		if c.Gross != nil {
			v.Set(c.Gross)
		}
		if c.Discount != nil {
			v.Sub(v, c.Discount)
		}
		if v.Sign() < 0 {
			v.SetInt64(0)
		}
		if c.Interval == calendar.Yearly {
			v.Quo(v, big.NewRat(12, 1))
		}
		mrr.Add(mrr, v)
	}
	mrrMoney, err := money.FromRat(mrr, money.BRL, money.HalfEven)
	if err != nil {
		return Dashboard{}, err
	}
	arrMoney, err := money.FromRat(new(big.Rat).Mul(mrr, big.NewRat(12, 1)), money.BRL, money.HalfEven)
	if err != nil {
		return Dashboard{}, err
	}
	return Dashboard{
		Month: month, Currency: money.BRL.Code(), PositionAt: positionAt, MRR: mrrMoney.Amount(), ARR: arrMoney.Amount(),
		Billed: raw.Billed, BilledRecurring: raw.BilledRecurring, BilledUsage: raw.BilledUsage, Received: raw.Received,
		OpenReceivables: raw.OpenReceivables, OverdueReceivables: raw.OverdueReceivables,
		DelinquencyRateBP: ratio(raw.DelinquencyOpen, raw.DelinquencyBase),
		ChurnRateBP:       ratio(int64(raw.ChurnedInPeriod), int64(raw.ActiveAtStart)), ActiveSubscriptions: raw.ActiveAtPosition,
		NewSubscriptions: raw.StartedInPeriod, CanceledSubscriptions: raw.EndedInPeriod, Refunds: raw.Refunds, Chargebacks: raw.Chargebacks,
		OpenRecoverables: raw.OpenRecoverables, PendingProviderReceipt: raw.PendingProviderReceipt,
		OpenDiscrepancies: raw.OpenDiscrepancies, UnknownOperations: raw.UnknownOperations,
		Definitions: Definitions, Bases: Bases,
	}, nil
}

// Window é o período consultado: o mês [From, To) e o instante das
// posições (PositionAt, inclusive). Current indica o mês corrente, em que a
// posição é o momento da consulta e pode ser lida do estado atual.
type Window struct {
	From, To   time.Time
	PositionAt time.Time
	Current    bool
}

// Source lê as agregações.
type Source interface {
	Location(ctx context.Context, tenantID uuid.UUID) (*time.Location, error)
	Raw(ctx context.Context, tenantID uuid.UUID, w Window) (Raw, error)
}

// Service calcula o dashboard.
type Service struct {
	src   Source
	clock clock.Clock
}

// NewService cria o serviço.
func NewService(src Source, c clock.Clock) *Service { return &Service{src: src, clock: c} }

// MonthRange converte "AAAA-MM" no intervalo [início, fim) do mês no fuso.
// Mês futuro é recusado: não há fluxo nem posição a mostrar.
func MonthRange(month string, loc *time.Location, now time.Time) (string, time.Time, time.Time, error) {
	if loc == nil {
		return "", time.Time{}, time.Time{}, calendar.ErrNilLocation
	}
	today := calendar.DateOf(now, loc)
	current := fmt.Sprintf("%04d-%02d", today.Year, today.Month)
	if month == "" {
		month = current
	}
	start, err := calendar.ParseDate(month + "-01")
	if err != nil {
		return "", time.Time{}, time.Time{}, apperr.Invalid("invalid_month", "Mês inválido.", apperr.FieldError{Field: "month", Message: "Use AAAA-MM."})
	}
	if month > current {
		return "", time.Time{}, time.Time{}, apperr.Invalid("future_month", "O mês ainda não começou.",
			apperr.FieldError{Field: "month", Message: "Informe o mês corrente ou um mês anterior."})
	}
	s, err := calendar.NewScheduleFromAnchor(calendar.Monthly, start, loc)
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	p := s.Period(0)
	return month, p.Start, p.End, nil
}

// Dashboard calcula os indicadores do mês informado (ou do mês corrente).
func (s *Service) Dashboard(ctx context.Context, tenantID uuid.UUID, month string) (Dashboard, error) {
	loc, err := s.src.Location(ctx, tenantID)
	if err != nil {
		return Dashboard{}, err
	}
	now := s.clock.Now()
	month, from, to, err := MonthRange(month, loc, now)
	if err != nil {
		return Dashboard{}, err
	}
	at := PositionAt(to, now)
	raw, err := s.src.Raw(ctx, tenantID, Window{From: from, To: to, PositionAt: at, Current: now.Before(to)})
	if err != nil {
		return Dashboard{}, err
	}
	return Compute(month, at, raw)
}

// PositionAt é o instante das posições do mês que termina em to: o momento
// da consulta enquanto o mês não terminou (mês corrente); depois, o último
// instante do mês
// (to menos a resolução de microssegundo do PostgreSQL), de modo que um
// evento registrado no início do mês seguinte não entre.
func PositionAt(to, now time.Time) time.Time {
	if now.Before(to) {
		return now
	}
	return to.Add(-time.Microsecond)
}
