package billing

import (
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// overageOffer é um plano de R$ 50,00 com excedente de R$ 0,03 por unidade
// (franquia zero).
func overageOffer() offer {
	o := newOffer("franquia", "Plano Franquia", calendar.Monthly, catalog.FlatPlusOverage, 0)
	metric := uuid.New()
	flat := brl(5000)
	o.price.FlatAmount, o.price.UnitAmount, o.price.IncludedQuantity, o.price.MetricID = &flat, big.NewRat(3, 1), new(big.Rat), &metric
	return o
}

// suspendReactivate suspende e reativa a assinatura nos instantes informados.
func suspendReactivate(t *testing.T, s *subscriptions.Subscription, suspendAt, reactivateAt time.Time) {
	t.Helper()
	if err := s.Suspend("inadimplência", suspendAt); err != nil {
		t.Fatal(err)
	}
	if err := s.Reactivate(reactivateAt); err != nil {
		t.Fatal(err)
	}
	s.PopEvents()
}

// usageByPeriod devolve, para cada período informado, uma linha de consumo
// com a quantidade e o agregado correspondentes; outro período falha o
// teste.
func usageByPeriod(t *testing.T, metricID uuid.UUID, qty int64, periods map[calendar.Period]uuid.UUID) func(subscriptions.Subscription, calendar.Period) []UsageLine {
	return func(_ subscriptions.Subscription, p calendar.Period) []UsageLine {
		aggID, ok := periods[p]
		if !ok {
			t.Errorf("consumo consultado para período inesperado: %+v", p)
			return nil
		}
		return []UsageLine{{MetricID: metricID, MetricCode: "api_calls", MetricName: "Requisições", Unit: "requisicao", Aggregation: "sum",
			Quantity: big.NewRat(qty, 1), AggregateID: &aggID}}
	}
}

// Suspensão no meio do período 1 (já faturado) e reativação no período 3: o
// passo da reativação fatura o componente fixo do período 3 e o consumo do
// período 1 anterior à suspensão; o período 2, inteiramente suspenso, não é
// consultado.
func TestEngineBillsUsageBeforeSuspensionAfterReactivation(t *testing.T) {
	f := newTermsFixture(t)
	plan := f.add(overageOffer())
	var p1, p3 calendar.Period
	sub := f.addSub(t, plan, 1, true, func(s *subscriptions.Subscription) {
		sched, err := s.Schedule()
		if err != nil {
			t.Fatal(err)
		}
		p1, p3 = sched.Period(1), sched.Period(3)
		if err := s.ApplyStep(subscriptions.BillingStep{Kind: subscriptions.StepBill, Index: 1, Period: p1}, p1.Start); err != nil {
			t.Fatal(err)
		}
		suspendReactivate(t, s, local(2026, time.November, 10, 12, 0), local(2027, time.January, 5, 9, 0))
	})
	aggID := uuid.New()
	f.store.usage = usageByPeriod(t, *plan.price.MetricID, 100, map[calendar.Period]uuid.UUID{p1: aggID})
	f.clock.Set(local(2027, time.January, 5, 10, 0))

	if res := f.run(t); res.Invoices != 1 {
		t.Fatalf("faturas emitidas = %d, esperado 1", res.Invoices)
	}
	inv := f.store.invoices[0]
	if inv.Kind != KindSubscriptionCycle || !samePeriod(*inv.BillingPeriod, p3) || len(inv.Items) != 2 {
		t.Fatalf("fatura do período da reativação: %+v", inv)
	}
	fee, usage := inv.Items[0], inv.Items[1]
	if fee.Kind != ItemSubscriptionFee || fee.Amount.Amount() != 5000 || !samePeriod(*fee.Period, p3) {
		t.Fatalf("componente fixo do período da reativação: %+v", fee)
	}
	if usage.Kind != ItemOverage || usage.Amount.Amount() != 300 || !samePeriod(*usage.Period, p1) ||
		!strings.Contains(usage.Description, "(01/11/2026 a 30/11/2026)") {
		t.Fatalf("consumo do período em que a suspensão começou: %+v", usage)
	}
	if !slices.Equal(f.store.usageQueries, []calendar.Period{p1}) || f.store.usageBilled[aggID] != usage.ID {
		t.Fatalf("somente o período 1 é fechado: consultas %v, vínculos %v", f.store.usageQueries, f.store.usageBilled)
	}
	if got := f.store.sub(sub.ID); got.NextBillingIndex != 4 || got.PendingUsageStarts != nil {
		t.Fatalf("o passo limpa o consumo pendente: índice %d, pendentes %v", got.NextBillingIndex, got.PendingUsageStarts)
	}
}

// Faturamento atrasado na suspensão (somente o período 0 faturado) e mudança
// agendada para o período 1: os períodos 0, 1 e 2 ficam pendentes, cada um
// precificado pela versão vigente nele (0 pela anterior, 1 e 2 pela nova).
func TestEnginePricesEachPendingUsagePeriodWithItsVersion(t *testing.T) {
	f := newTermsFixture(t)
	next := f.add(newOffer("api-plus", "Plano API Plus", calendar.Monthly, catalog.Usage, 5))
	var periods []calendar.Period
	sub := f.addSub(t, f.metered, 1, true, func(s *subscriptions.Subscription) {
		sched, err := s.Schedule()
		if err != nil {
			t.Fatal(err)
		}
		for i := range 5 {
			periods = append(periods, sched.Period(i))
		}
		suspendReactivate(t, s, local(2026, time.December, 10, 12, 0), local(2027, time.February, 5, 9, 0))
	})
	f.schedule(t, sub, &next, nil)
	aggs := map[calendar.Period]uuid.UUID{periods[0]: uuid.New(), periods[1]: uuid.New(), periods[2]: uuid.New()}
	f.store.usage = usageByPeriod(t, *f.metered.price.MetricID, 1000, aggs)
	f.clock.Set(local(2027, time.February, 5, 10, 0))

	if res := f.run(t); res.Invoices != 1 {
		t.Fatalf("faturas emitidas = %d, esperado 1", res.Invoices)
	}
	inv := f.store.invoices[0]
	if !samePeriod(*inv.BillingPeriod, periods[4]) || len(inv.Items) != 3 || inv.Total.Amount() != 12000 {
		t.Fatalf("fatura do período da reativação: %+v", inv)
	}
	want := []struct {
		period calendar.Period
		offer  offer
		amount int64
	}{{periods[0], f.metered, 2000}, {periods[1], next, 5000}, {periods[2], next, 5000}}
	for i, w := range want {
		it := inv.Items[i]
		if !samePeriod(*it.Period, w.period) || *it.PriceVersionID != w.offer.price.ID || *it.PlanID != w.offer.plan.ID || it.Amount.Amount() != w.amount {
			t.Errorf("item %d: período %+v, versão %s, valor %d; esperava %+v pela versão %s com %d", i, *it.Period, *it.PriceVersionID,
				it.Amount.Amount(), w.period, w.offer.price.ID, w.amount)
		}
	}
	if !slices.Equal(f.store.usagePrices, []uuid.UUID{f.metered.price.ID, next.price.ID, next.price.ID}) {
		t.Fatalf("versões usadas no fechamento: %v", f.store.usagePrices)
	}
}

// Cancelamento imediato depois da reativação e antes do motor: a fatura de
// consumo final cobre o consumo pendente e o do período interrompido.
func TestEngineFinalUsageIncludesPendingUsage(t *testing.T) {
	f := newTermsFixture(t)
	canceledAt := local(2027, time.January, 6, 9, 0)
	var p1, interrupted calendar.Period
	sub := f.addSub(t, f.metered, 1, true, func(s *subscriptions.Subscription) {
		sched, err := s.Schedule()
		if err != nil {
			t.Fatal(err)
		}
		p1, interrupted = sched.Period(1), calendar.Period{Start: sched.Period(3).Start, End: canceledAt}
		if err := s.ApplyStep(subscriptions.BillingStep{Kind: subscriptions.StepBill, Index: 1, Period: p1}, p1.Start); err != nil {
			t.Fatal(err)
		}
		suspendReactivate(t, s, local(2026, time.November, 10, 12, 0), local(2027, time.January, 5, 9, 0))
		if err := s.Cancel(subscriptions.CancelImmediately, "", true, false, canceledAt); err != nil {
			t.Fatal(err)
		}
	})
	f.store.usage = usageByPeriod(t, *f.metered.price.MetricID, 100, map[calendar.Period]uuid.UUID{p1: uuid.New(), interrupted: uuid.New()})
	f.clock.Set(canceledAt.Add(time.Hour))

	if res := f.run(t); res.Invoices != 1 {
		t.Fatalf("faturas emitidas = %d, esperado 1", res.Invoices)
	}
	inv := f.store.invoices[0]
	if inv.Kind != KindFinalUsage || !samePeriod(*inv.BillingPeriod, calendar.Period{Start: p1.Start, End: canceledAt}) || len(inv.Items) != 2 {
		t.Fatalf("fatura de consumo final: %+v", inv)
	}
	if !samePeriod(*inv.Items[0].Period, p1) || !samePeriod(*inv.Items[1].Period, interrupted) || inv.Total != money.New(400, money.BRL) {
		t.Fatalf("itens do consumo final: %+v", inv.Items)
	}
	if got := f.store.sub(sub.ID); got.NextBillingAt != nil || got.PendingUsageStarts != nil {
		t.Fatalf("o fechamento final encerra o faturamento: %v %v", got.NextBillingAt, got.PendingUsageStarts)
	}
}
