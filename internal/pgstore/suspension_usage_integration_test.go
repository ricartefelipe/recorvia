//go:build integration

package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

const (
	msgUsageSuspended = "Assinatura suspensa: consumo não é faturável durante a suspensão."
	msgUsageNotPriced = "A métrica não faz parte do preço vigente da assinatura."
)

// Suspensão no meio do período 1 (já faturado) e reativação dois períodos
// depois: o consumo anterior à suspensão, inclusive o recebido depois da
// reativação, é faturado pelo motor na fatura do período da reativação; o
// período inteiramente suspenso não tem consumo nem agregado. Depois do
// fechamento, o faturamento segue com o período da reativação.
func TestUsageBeforeSuspensionBilledAfterReactivation(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-consumo-reativacao")
	metric, plan := a.newMeteredPlan(t, tn, catalog.AggSum, catalog.PriceInput{Model: catalog.FlatPlusOverage, FlatAmount: new(int64(5000)),
		UnitAmount: "2", IncludedQuantity: "0"})
	sub := a.newSubscriptionQty(t, tn, plan.ID, 1)
	a.runEngine(t, tn, 1)
	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	a.runEngine(t, tn, 1)
	event := func(id, qty string, at time.Time) metering.EventInput {
		return metering.EventInput{ExternalID: id, SubscriptionID: sub.ID, MetricCode: metric.Code, Quantity: qty, OccurredAt: at}
	}
	period1 := spTime(t, 2026, time.April, 10, 0)
	period2 := spTime(t, 2026, time.May, 10, 0)
	period3 := spTime(t, 2026, time.June, 10, 0)

	a.Clock.Set(spTime(t, 2026, time.April, 20, 12))
	wantOutcomes(t, a.ingest(t, tn, event("antes-1", "100", spTime(t, 2026, time.April, 20, 10))), metering.OutcomeAccepted)
	a.Clock.Set(spTime(t, 2026, time.April, 25, 12))
	current, err := a.Subscriptions.Get(ctx, tn.ID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	suspended, err := a.Subscriptions.Suspend(ctx, tn.ID, tn.Actor, sub.ID, "Inadimplência", current.Version)
	if err != nil {
		t.Fatal(err)
	}
	a.Clock.Set(spTime(t, 2026, time.June, 15, 12))
	if _, err := a.Subscriptions.Reactivate(ctx, tn.ID, tn.Actor, sub.ID, suspended.Version); err != nil {
		t.Fatal(err)
	}
	reactivated, err := a.Subscriptions.Get(ctx, tn.ID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reactivated.NextBillingIndex != 3 || len(reactivated.PendingUsageStarts) != 1 || !reactivated.PendingUsageStarts[0].Equal(period1) {
		t.Fatalf("reativação: índice %d, consumo pendente %v", reactivated.NextBillingIndex, reactivated.PendingUsageStarts)
	}

	// Antes do motor: o consumo anterior à suspensão ainda é aceito; o do
	// período suspenso não.
	a.Clock.Set(spTime(t, 2026, time.June, 15, 13))
	res := a.ingest(t, tn,
		event("antes-2", "50", spTime(t, 2026, time.April, 22, 10)),
		event("suspenso", "70", spTime(t, 2026, time.May, 20, 10)),
		event("depois", "1", spTime(t, 2026, time.June, 15, 12)),
	)
	wantOutcomes(t, res, metering.OutcomeAccepted, metering.OutcomeRejected, metering.OutcomeAccepted)
	if res[1].Error != msgUsageSuspended {
		t.Errorf("consumo do período suspenso: motivo %q", res[1].Error)
	}

	a.runEngine(t, tn, 1)
	inv := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, period3)
	items := a.itemsOf(t, tn.ID, inv.ID)
	if len(items) != 2 || items[0].Kind != string(billing.ItemSubscriptionFee) || items[0].Amount != 5000 {
		t.Fatalf("itens da fatura do período da reativação: %+v", items)
	}
	agg := a.aggregateOf(t, tn.ID, sub.ID, period1)
	if items[1].Kind != string(billing.ItemOverage) || items[1].Amount != 300 || items[1].UsageAggregateID == nil || *items[1].UsageAggregateID != agg.ID ||
		!ratEqual(t, items[1].Quantity, "150") {
		t.Fatalf("consumo anterior à suspensão (150 × R$ 0,02): %+v", items[1])
	}
	if !ratEqual(t, agg.Quantity, "150") || agg.EventCount != 2 || agg.InvoiceItemID == nil || *agg.InvoiceItemID != items[1].ID {
		t.Fatalf("agregado do período em que a suspensão começou: %+v", agg)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM usage_aggregates WHERE tenant_id = $1 AND subscription_id = $2 AND period_start = $3`,
		tn.ID, sub.ID, period2); n != 0 {
		t.Errorf("o período inteiramente suspenso não é fechado: %d agregados", n)
	}
	for _, id := range []string{"antes-1", "antes-2"} {
		if ev := a.usageEvent(t, tn.ID, id); ev.AggregateID == nil || *ev.AggregateID != agg.ID {
			t.Errorf("evento %s não vinculado ao agregado: %+v", id, ev)
		}
	}
	billed, err := a.Subscriptions.Get(ctx, tn.ID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if billed.NextBillingIndex != 4 || billed.PendingUsageStarts != nil {
		t.Fatalf("depois do passo: índice %d, consumo pendente %v", billed.NextBillingIndex, billed.PendingUsageStarts)
	}

	// Fechado o período, o evento tardio segue a tolerância de atraso.
	wantOutcomes(t, a.ingest(t, tn, event("antes-3", "5", spTime(t, 2026, time.April, 23, 10))), metering.OutcomeRejected)

	a.Clock.Set(spTime(t, 2026, time.July, 10, 9))
	a.runEngine(t, tn, 1)
	if agg3 := a.aggregateOf(t, tn.ID, sub.ID, period3); !ratEqual(t, agg3.Quantity, "1") || agg3.EventCount != 1 {
		t.Fatalf("o período da reativação é fechado no passo seguinte: %+v", agg3)
	}
}

// Mudança agendada já em vigor e ainda não aplicada pelo motor: o consumo do
// novo período é validado pela versão de preço da mudança; o do período
// anterior, pela vigente. Depois que o motor aplica a mudança, o novo período
// é faturado pela nova métrica.
func TestUsageOfScheduledChangeInForceBeforeEngine(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-consumo-mudanca-vigente")
	oldMetric, oldPlan := a.newMeteredPlan(t, tn, catalog.AggSum, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "1"})
	newMetric, newPlan := a.newMeteredPlan(t, tn, catalog.AggSum, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "3"})
	sub := a.newSubscriptionQty(t, tn, oldPlan.ID, 1)
	a.runEngine(t, tn, 0)
	if _, err := a.Changes.Change(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, billing.ChangeInput{PlanID: &newPlan.ID,
		Timing: subscriptions.ChangeNextCycle}, 0); err != nil {
		t.Fatal(err)
	}
	event := func(id string, m catalog.Metric, qty string, at time.Time) metering.EventInput {
		return metering.EventInput{ExternalID: id, SubscriptionID: sub.ID, MetricCode: m.Code, Quantity: qty, OccurredAt: at}
	}
	previous := spTime(t, 2026, time.April, 9, 20)
	inForce := spTime(t, 2026, time.April, 10, 8)

	// O período 1 começou em 10/04; o motor ainda não rodou.
	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	res := a.ingest(t, tn,
		event("vigente-nova", newMetric, "10", inForce),
		event("vigente-anterior", oldMetric, "10", inForce),
		event("anterior-anterior", oldMetric, "4", previous),
		event("anterior-nova", newMetric, "4", previous),
	)
	wantOutcomes(t, res, metering.OutcomeAccepted, metering.OutcomeRejected, metering.OutcomeAccepted, metering.OutcomeRejected)
	for _, r := range []metering.Result{res[1], res[3]} {
		if r.Error != msgUsageNotPriced {
			t.Errorf("%s: motivo %q", r.ExternalID, r.Error)
		}
	}
	usage, err := a.Metering.CurrentUsage(ctx, tn.ID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage.Metrics) != 1 || usage.Metrics[0].Metric.ID != newMetric.ID || usage.Metrics[0].EstimatedAmount.Amount() != 30 {
		t.Fatalf("consumo estimado pela versão da mudança: %+v", usage.Metrics)
	}

	// O motor aplica a mudança e fecha o período 0 pela versão anterior.
	a.runEngine(t, tn, 1)
	if agg0 := a.aggregateOf(t, tn.ID, sub.ID, spTime(t, 2026, time.March, 10, 0)); !ratEqual(t, agg0.Quantity, "4") ||
		agg0.PriceVersionID == nil || *agg0.PriceVersionID != *oldPlan.CurrentPriceVersionID {
		t.Fatalf("período anterior fechado pela versão anterior: %+v", agg0)
	}
	a.Clock.Set(spTime(t, 2026, time.May, 10, 9))
	a.runEngine(t, tn, 1)
	period1 := spTime(t, 2026, time.April, 10, 0)
	agg1 := a.aggregateOf(t, tn.ID, sub.ID, period1)
	if !ratEqual(t, agg1.Quantity, "10") || agg1.PriceVersionID == nil || *agg1.PriceVersionID != *newPlan.CurrentPriceVersionID {
		t.Fatalf("período da mudança fechado pela nova versão: %+v", agg1)
	}
	inv := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, spTime(t, 2026, time.May, 10, 0))
	if items := a.itemsOf(t, tn.ID, inv.ID); len(items) != 1 || items[0].Amount != 30 || items[0].UsageAggregateID == nil ||
		*items[0].UsageAggregateID != agg1.ID {
		t.Fatalf("consumo do período da mudança (10 × R$ 0,03): %+v", items)
	}
}
