package metering

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// --- Ingest: período pendente depois de uma reativação ---

// reactivatedEnv monta a assinatura faturada até o período 1 (10/02 a
// 10/03), suspensa em 25/02 e reativada em 12/04 (período 3): o passo 2, que
// fecharia o consumo do período 1, é pulado com o período 2, inteiramente
// suspenso. O histórico de suspensões só é registrado com history.
func reactivatedEnv(t *testing.T, history bool) *env {
	t.Helper()
	e := newEnv()
	suspendedAt := time.Date(2026, time.February, 25, 12, 0, 0, 0, time.UTC)
	reactivatedAt := time.Date(2026, time.April, 12, 12, 0, 0, 0, time.UTC)
	e.editSub(func(s *subscriptions.Subscription) {
		s.NextBillingIndex = 2
		s.CurrentPeriod = calendar.Period{Start: p1Start, End: p2Start}
		if err := s.Suspend("", suspendedAt); err != nil {
			t.Fatal(err)
		}
		if err := s.Reactivate(reactivatedAt); err != nil {
			t.Fatal(err)
		}
	})
	if e.sub.NextBillingIndex != 3 || len(e.sub.PendingUsageStarts) != 1 || !e.sub.PendingUsageStarts[0].Equal(p1Start) {
		t.Fatalf("estado da reativação: índice %d, pendentes %v", e.sub.NextBillingIndex, e.sub.PendingUsageStarts)
	}
	if history {
		e.st().suspensions[e.sub.ID] = []SuspensionChange{{At: suspendedAt, Suspended: true}, {At: reactivatedAt}}
	}
	e.clock.Set(time.Date(2026, time.April, 15, 12, 0, 0, 0, time.UTC))
	return e
}

func TestIngestPendingUsagePeriodAfterReactivation(t *testing.T) {
	cases := []struct {
		name       string
		history    bool
		closed     bool
		at         time.Time
		want       Outcome
		wantError  string
		wantPeriod time.Time
	}{
		{"consumo anterior à suspensão no período em que ela começou", true, false, lateAt, OutcomeAccepted, "", p1Start},
		{"consumo durante a suspensão", true, false, occurredAt, OutcomeRejected, msgSuspended, time.Time{}},
		{"consumo depois da reativação", true, false, time.Date(2026, time.April, 13, 9, 0, 0, 0, time.UTC), OutcomeAccepted, "", p2End},
		// Sem o histórico, a regra do período também recusa o período
		// inteiramente suspenso: ele nunca será fechado.
		{"período inteiramente suspenso", false, false, occurredAt, OutcomeRejected, msgBilledNoMetric, time.Time{}},
		{"período anterior ao pendente", true, false, time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC), OutcomeRejected,
			msgBilledNoMetric, time.Time{}},
		// Depois do fechamento pelo primeiro passo após a reativação, valem
		// as regras de evento atrasado (tolerância contada do fim do período).
		{"período pendente já fechado", true, true, lateAt, OutcomeRejected, msgOutOfGrace, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := reactivatedEnv(t, tc.history)
			if tc.closed {
				e.closePeriod(p1Start)
			}
			in := e.input()
			in.OccurredAt = tc.at
			r := e.ingest(t, in)[0]
			assertOutcome(t, r, tc.want)
			if r.Error != tc.wantError {
				t.Fatalf("mensagem = %q, esperava %q", r.Error, tc.wantError)
			}
			if tc.want == OutcomeAccepted {
				if ev := e.st().events[in.ExternalID]; !ev.PeriodStart.Equal(tc.wantPeriod) {
					t.Fatalf("período do evento = %v, esperava %v", ev.PeriodStart, tc.wantPeriod)
				}
			}
		})
	}
}

// --- Ingest: mudança agendada em vigor e ainda não aplicada ---

// changeEnv monta a assinatura com o período 1 faturado e o passo 2 ainda
// não executado pelo motor (agora = 15/03, período 2), com uma segunda
// métrica e uma versão de preço que a cobra.
func changeEnv() (*env, catalog.Metric, catalog.PriceVersion) {
	e := newEnv()
	e.editSub(func(s *subscriptions.Subscription) {
		s.NextBillingIndex = 2
		s.CurrentPeriod = calendar.Period{Start: p1Start, End: p2Start}
	})
	other := catalog.Metric{ID: uuid.New(), TenantID: e.tenantID, Code: "storage.gb", Name: "Armazenamento", Unit: "gb",
		Aggregation: catalog.AggSum, LateGraceHours: 72, Status: catalog.Active}
	e.st().metrics[other.ID] = other
	price := catalog.PriceVersion{ID: uuid.New(), TenantID: e.tenantID, PlanID: uuid.New(), Version: 1, Model: catalog.Usage,
		UnitAmount: big.NewRat(2, 1), MetricID: &other.ID, Currency: money.BRL}
	e.st().prices[price.ID] = price
	return e, other, price
}

func (e *env) scheduleChange(priceID *uuid.UUID, effectiveAt time.Time) {
	ch := &subscriptions.ScheduledChange{ID: uuid.New(), TenantID: e.tenantID, SubscriptionID: e.sub.ID, Kind: subscriptions.QuantityChange,
		NewQuantity: new(1), EffectiveAt: effectiveAt, Status: subscriptions.ChangePending, CreatedAt: lateAt}
	if priceID != nil {
		ch.Kind, ch.NewPlanID, ch.NewPriceVersionID, ch.NewQuantity = subscriptions.PlanChange, new(uuid.New()), priceID, nil
	}
	e.st().changes[e.sub.ID] = ch
}

func TestIngestMetricOfScheduledChangeInForce(t *testing.T) {
	cases := []struct {
		name      string
		toPrice   bool
		effective time.Time
		newMetric bool
		at        time.Time
		want      Outcome
		wantError string
	}{
		{"métrica da nova versão no período em que a mudança vale", true, p2Start, true, occurredAt, OutcomeAccepted, ""},
		{"métrica anterior no período em que a mudança vale", true, p2Start, false, occurredAt, OutcomeRejected, msgNotInPrice},
		{"métrica anterior no período anterior à mudança", true, p2Start, false, lateAt, OutcomeAccepted, ""},
		{"métrica da nova versão no período anterior à mudança", true, p2Start, true, lateAt, OutcomeRejected, msgNotInPrice},
		{"mudança com data efetiva posterior ao início do período", true, p2End, false, occurredAt, OutcomeAccepted, ""},
		{"métrica nova antes da data efetiva", true, p2End, true, occurredAt, OutcomeRejected, msgNotInPrice},
		{"mudança somente de quantidade", false, p2Start, false, occurredAt, OutcomeAccepted, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, other, price := changeEnv()
			if tc.toPrice {
				e.scheduleChange(&price.ID, tc.effective)
			} else {
				e.scheduleChange(nil, tc.effective)
			}
			in := e.input()
			in.OccurredAt = tc.at
			if tc.newMetric {
				in.MetricCode, in.Unit = other.Code, other.Unit
			}
			r := e.ingest(t, in)[0]
			assertOutcome(t, r, tc.want)
			if r.Error != tc.wantError {
				t.Fatalf("mensagem = %q, esperava %q", r.Error, tc.wantError)
			}
		})
	}
	t.Run("mudança consultada uma vez por assinatura no lote", func(t *testing.T) {
		e, other, price := changeEnv()
		e.scheduleChange(&price.ID, p2Start)
		var batch []EventInput
		for i := range 3 {
			in := e.input()
			in.ExternalID, in.MetricCode, in.Unit = fmt.Sprintf("evt-%d", i), other.Code, other.Unit
			batch = append(batch, in)
		}
		for _, r := range e.ingest(t, batch...) {
			assertOutcome(t, r, OutcomeAccepted)
		}
		if e.store.calls["PendingChange"] != 1 {
			t.Fatalf("mudança consultada %d vezes, esperava 1", e.store.calls["PendingChange"])
		}
	})
}

// --- CurrentUsage: mudança agendada em vigor ---

func TestCurrentUsageUsesScheduledChangeInForce(t *testing.T) {
	cases := []struct {
		name       string
		effective  time.Time
		wantNew    bool
		wantAmount int64
	}{
		{"mudança em vigor no início do período corrente", p2Start, true, 20},
		{"mudança para o período seguinte", p2End, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, other, price := changeEnv()
			e.scheduleChange(&price.ID, tc.effective)
			e.st().events["evt-novo"] = Event{ID: uuid.New(), ExternalID: "evt-novo", SubscriptionID: e.sub.ID, MetricID: other.ID,
				Quantity: big.NewRat(10, 1), PeriodStart: p2Start, Status: Accepted}
			u, err := e.svc.CurrentUsage(context.Background(), e.tenantID, e.sub.ID)
			if err != nil {
				t.Fatalf("CurrentUsage: %v", err)
			}
			want := e.metric.ID
			if tc.wantNew {
				want = other.ID
			}
			if len(u.Metrics) != 1 || u.Metrics[0].Metric.ID != want || u.Metrics[0].EstimatedAmount.Amount() != tc.wantAmount {
				t.Fatalf("consumo estimado: %+v, esperava a métrica %s com %d centavos", u.Metrics, want, tc.wantAmount)
			}
		})
	}
}
