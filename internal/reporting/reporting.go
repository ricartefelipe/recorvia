// Package reporting calcula os indicadores do dashboard financeiro. As
// fórmulas ficam explícitas em Definitions e são devolvidas à interface.
//
// Receita faturada (emitida) e recebida (paga) são indicadores distintos. O
// MRR considera apenas o componente recorrente fixo; consumo variável,
// excedente e valores avulsos ficam fora da recorrência.
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

// MRRComponent é a soma do componente fixo por periodicidade e modelo.
type MRRComponent struct {
	Interval     calendar.Interval
	Model        string
	FlatTotal    int64
	PerUnitTotal *big.Rat
}

// Raw são as agregações lidas do banco.
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
	ActiveNow          int
	CreatedInPeriod    int
	EndedInPeriod      int
	ActiveAtStart      int
	Refunds            int64
	Chargebacks        int64
	OpenRecoverables   int64
	UnknownOperations  int
	OpenDiscrepancies  int
}

// Dashboard são os indicadores calculados.
type Dashboard struct {
	Month                 string
	Currency              string
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
	OpenDiscrepancies     int
	UnknownOperations     int
	Definitions           map[string]string
}

// Definitions descreve as fórmulas exibidas na interface.
var Definitions = map[string]string{
	"mrr": "MRR: soma do componente recorrente fixo mensal das assinaturas ativas ou em atraso (valor fixo, ou preço unitário × quantidade); " +
		"planos anuais entram divididos por 12. Exclui trial, consumo medido, excedente e cobranças avulsas.",
	"arr":         "ARR: MRR × 12.",
	"billed":      "Faturado: total das faturas emitidas no mês (exclui canceladas). Separado em recorrente fixo e consumo.",
	"received":    "Recebido: soma dos pagamentos confirmados no mês (pela data de pagamento), via provedor ou registro manual.",
	"open":        "Recebíveis em aberto: saldo devedor das faturas em aberto ou parcialmente pagas.",
	"overdue":     "Recebíveis vencidos: parcela dos recebíveis em aberto com vencimento já ultrapassado.",
	"delinquency": "Inadimplência: saldo vencido das faturas com vencimento nos últimos 90 dias ÷ total dessas faturas.",
	"churn":       "Churn de contratos: assinaturas encerradas no mês ÷ assinaturas ativas (fora de trial) no início do mês.",
	"refunds":     "Reembolsos: valor líquido lançado no ledger no mês na conta de reembolsos (reembolsos efetivados, pela data da confirmação).",
	"chargebacks": "Chargebacks: valor líquido lançado no ledger no mês na conta de chargebacks (débitos menos créditos). A abertura da disputa " +
		"retém o valor contestado; a disputa ganha estorna a retenção e a perdida libera a parte reembolsada ou reservada a reembolso " +
		"e a parte que era pagamento excedente (crédito do cliente consumido ou valor a recuperar dele), " +
		"o que reduz o valor do mês em que ocorrem (negativo quando a disputa aberta em mês anterior é ganha). No acumulado " +
		"dos meses, somado aos reembolsos, nunca passa do valor pago. A baixa do valor a recuperar não altera este indicador.",
	"recoverables": "Valores a recuperar: saldo atual da conta de valores a recuperar de clientes, isto é, o crédito de pagamento " +
		"excedente que o cliente já havia usado quando o pagamento foi estornado por chargeback, menos as baixas. A baixa recebida " +
		"(paga pelo cliente fora do provedor) vai para o disponível liquidado e não entra em Recebido, que soma só pagamentos de " +
		"faturas; a baixa perdida é reconhecida como perda com inadimplência.",
}

func ratio(num, den int64) int64 {
	if den <= 0 || num <= 0 {
		return 0
	}
	return money.RoundRat(big.NewRat(num*10000, den), money.HalfEven).Int64()
}

// Compute aplica as fórmulas sobre as agregações.
func Compute(month string, raw Raw) (Dashboard, error) {
	mrr := new(big.Rat)
	for _, c := range raw.MRR {
		v := new(big.Rat)
		switch c.Model {
		case "flat", "flat_plus_overage":
			v.SetInt64(c.FlatTotal)
		case "per_unit":
			if c.PerUnitTotal != nil {
				v.Set(c.PerUnitTotal)
			}
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
		Month: month, Currency: money.BRL.Code(), MRR: mrrMoney.Amount(), ARR: arrMoney.Amount(), Billed: raw.Billed,
		BilledRecurring: raw.BilledRecurring, BilledUsage: raw.BilledUsage, Received: raw.Received, OpenReceivables: raw.OpenReceivables,
		OverdueReceivables: raw.OverdueReceivables, DelinquencyRateBP: ratio(raw.DelinquencyOpen, raw.DelinquencyBase),
		ChurnRateBP: ratio(int64(raw.EndedInPeriod), int64(raw.ActiveAtStart)), ActiveSubscriptions: raw.ActiveNow,
		NewSubscriptions: raw.CreatedInPeriod, CanceledSubscriptions: raw.EndedInPeriod, Refunds: raw.Refunds, Chargebacks: raw.Chargebacks,
		OpenRecoverables: raw.OpenRecoverables, OpenDiscrepancies: raw.OpenDiscrepancies, UnknownOperations: raw.UnknownOperations, Definitions: Definitions,
	}, nil
}

// Source lê as agregações.
type Source interface {
	Location(ctx context.Context, tenantID uuid.UUID) (*time.Location, error)
	Raw(ctx context.Context, tenantID uuid.UUID, from, to, now time.Time) (Raw, error)
}

// Service calcula o dashboard.
type Service struct {
	src   Source
	clock clock.Clock
}

// NewService cria o serviço.
func NewService(src Source, c clock.Clock) *Service { return &Service{src: src, clock: c} }

// MonthRange converte "AAAA-MM" no intervalo [início, fim) do mês no fuso.
func MonthRange(month string, loc *time.Location, now time.Time) (string, time.Time, time.Time, error) {
	if month == "" {
		d := calendar.DateOf(now, loc)
		month = fmt.Sprintf("%04d-%02d", d.Year, d.Month)
	}
	start, err := calendar.ParseDate(month + "-01")
	if err != nil {
		return "", time.Time{}, time.Time{}, apperr.Invalid("invalid_month", "Mês inválido.", apperr.FieldError{Field: "month", Message: "Use AAAA-MM."})
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
	raw, err := s.src.Raw(ctx, tenantID, from, to, now)
	if err != nil {
		return Dashboard{}, err
	}
	return Compute(month, raw)
}
