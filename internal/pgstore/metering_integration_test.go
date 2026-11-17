//go:build integration

package pgstore_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// Evento aceito que ficou sem agregado num período já fechado (gravado
// enquanto o fechamento era confirmado): a correção de consumo o encontra,
// fatura a diferença por ajuste e o vincula ao agregado, sem mudar a
// situação do evento nem a fatura emitida. Eventos aceitos de períodos ainda
// abertos não entram na correção.
func TestUnattachedAcceptedUsageInClosedPeriodIsCorrected(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-consumo-orfao")
	metric, plan := a.newMeteredPlan(t, tn, catalog.AggSum, catalog.PriceInput{Model: catalog.FlatPlusOverage, FlatAmount: new(int64(5000)),
		UnitAmount: "2", IncludedQuantity: "0"})
	sub := a.newSubscriptionQty(t, tn, plan.ID, 1)
	a.runEngine(t, tn, 1)
	event := func(id, qty string, at time.Time) metering.EventInput {
		return metering.EventInput{ExternalID: id, SubscriptionID: sub.ID, MetricCode: metric.Code, Quantity: qty, OccurredAt: at}
	}
	period0 := spTime(t, 2026, time.March, 10, 0)
	a.Clock.Set(spTime(t, 2026, time.April, 5, 9))
	wantOutcomes(t, a.ingest(t, tn, event("orfao-1", "100", spTime(t, 2026, time.March, 20, 10))), metering.OutcomeAccepted)
	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	a.runEngine(t, tn, 1)
	agg := a.aggregateOf(t, tn.ID, sub.ID, period0)
	closing := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, spTime(t, 2026, time.April, 10, 0))

	// Estado deixado por uma ingestão confirmada depois da vinculação dos
	// eventos ao agregado.
	a.inTenant(t, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO usage_events (id, tenant_id, external_id, customer_id, subscription_id, metric_id, quantity,
			occurred_at, received_at, period_start, status) VALUES ($1, $2, 'orfao-2', $3, $4, $5, 50, $6, $7, $8, 'accepted')`,
			ids.New(), tn.ID, sub.CustomerID, sub.ID, metric.ID, spTime(t, 2026, time.April, 9, 20), a.Clock.Now(), period0)
		return err
	})
	a.Clock.Set(spTime(t, 2026, time.April, 10, 10))
	wantOutcomes(t, a.ingest(t, tn, event("aberto-1", "7", spTime(t, 2026, time.April, 10, 8))), metering.OutcomeAccepted)

	tenants, err := a.Store.Billing().TenantsWithLateUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(tenants, tn.ID) {
		t.Fatalf("tenant com evento aceito sem agregado em período fechado não foi encontrado: %v", tenants)
	}
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	pending := a.pendingItemsOf(t, tn.ID, sub.ID)
	if len(pending) != 1 || pending[0].Amount != 100 || pending[0].Source != "usage_correction" || pending[0].UsageAggregateID == nil ||
		*pending[0].UsageAggregateID != agg.ID {
		t.Fatalf("itens pendentes: %+v", pending)
	}
	ev := a.usageEvent(t, tn.ID, "orfao-2")
	if ev.Status != string(metering.Accepted) || ev.AggregateID == nil || *ev.AggregateID != agg.ID || ev.CorrectedAt == nil ||
		ev.CorrectionItemID == nil || *ev.CorrectionItemID != pending[0].ID {
		t.Errorf("evento corrigido: %+v", ev)
	}
	if open := a.usageEvent(t, tn.ID, "aberto-1"); open.AggregateID != nil || open.CorrectedAt != nil {
		t.Errorf("evento de período aberto entrou na correção: %+v", open)
	}
	if c := a.aggregateOf(t, tn.ID, sub.ID, period0); c.CorrectedAmount == nil || *c.CorrectedAmount != 300 || !ratEqual(t, c.Quantity, "100") {
		t.Errorf("agregado corrigido: %+v", c)
	}
	if after := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, closing.PeriodStart); after != closing {
		t.Errorf("a correção alterou a fatura emitida: %+v", after)
	}

	// Nada mais a corrigir: o tenant sai da descoberta e uma nova execução
	// não gera ajuste.
	if tenants, err = a.Store.Billing().TenantsWithLateUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(tenants, tn.ID) {
		t.Errorf("tenant continua na descoberta depois da correção")
	}
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := a.pendingItemsOf(t, tn.ID, sub.ID); len(got) != 1 {
		t.Errorf("itens pendentes após nova execução = %d, esperado 1", len(got))
	}
}

// Caractere NUL em textos ou em properties é rejeitado por evento; o lote
// não falha e os demais eventos são gravados.
func TestUsageIngestionRejectsNULPerEvent(t *testing.T) {
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-consumo-nul")
	metric, plan := a.newMeteredPlan(t, tn, catalog.AggSum, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "1"})
	sub := a.newSubscriptionQty(t, tn, plan.ID, 1)
	a.Clock.Set(spTime(t, 2026, time.March, 20, 12))
	at := spTime(t, 2026, time.March, 20, 10)
	event := func(id string) metering.EventInput {
		return metering.EventInput{ExternalID: id, SubscriptionID: sub.ID, MetricCode: metric.Code, Quantity: "1", OccurredAt: at}
	}
	nulID := event("nul\x00id")
	nulCode := event("nul-codigo")
	nulCode.MetricCode = metric.Code + "\x00"
	nulKey := event("nul-chave")
	nulKey.UniqueKey = "chave\x00"
	nulValue := event("nul-valor")
	nulValue.Properties = map[string]any{"nota": "a\x00b"}
	nulName := event("nul-nome")
	nulName.Properties = map[string]any{"a\x00": "b"}
	literal := event("barra-literal")
	literal.Properties = map[string]any{"nota": `\u0000`}

	batch := []metering.EventInput{nulID, nulCode, nulKey, nulValue, nulName, literal, event("valido")}
	res := a.ingest(t, tn, batch...)
	wantOutcomes(t, res, metering.OutcomeRejected, metering.OutcomeRejected, metering.OutcomeRejected, metering.OutcomeRejected,
		metering.OutcomeRejected, metering.OutcomeAccepted, metering.OutcomeAccepted)
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM usage_events WHERE tenant_id = $1`, tn.ID); n != 2 {
		t.Errorf("eventos gravados = %d, esperado 2", n)
	}
	// O texto literal é gravado e lido sem alteração: o reenvio é duplicata.
	wantOutcomes(t, a.ingest(t, tn, literal), metering.OutcomeDuplicate)
}

// Consumo ocorrido durante a suspensão é rejeitado também depois da
// reativação (decisão pelo histórico da assinatura); o intervalo suspenso é
// [suspensão, reativação).
func TestUsageDuringSuspensionRejectedAfterReactivation(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-consumo-suspensao")
	metric, plan := a.newMeteredPlan(t, tn, catalog.AggSum, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "1"})
	sub := a.newSubscriptionQty(t, tn, plan.ID, 1)
	a.runEngine(t, tn, 0)
	event := func(id string, at time.Time) metering.EventInput {
		return metering.EventInput{ExternalID: id, SubscriptionID: sub.ID, MetricCode: metric.Code, Quantity: "1", OccurredAt: at}
	}
	suspendedAt := spTime(t, 2026, time.March, 15, 12)
	reactivatedAt := spTime(t, 2026, time.March, 20, 12)

	a.Clock.Set(suspendedAt)
	current, err := a.Subscriptions.Get(ctx, tn.ID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	suspended, err := a.Subscriptions.Suspend(ctx, tn.ID, tn.Actor, sub.ID, "Inadimplência", current.Version)
	if err != nil {
		t.Fatal(err)
	}
	a.Clock.Set(spTime(t, 2026, time.March, 17, 12))
	wantOutcomes(t, a.ingest(t, tn, event("susp-durante-1", spTime(t, 2026, time.March, 16, 10))), metering.OutcomeRejected)

	a.Clock.Set(reactivatedAt)
	if _, err := a.Subscriptions.Reactivate(ctx, tn.ID, tn.Actor, sub.ID, suspended.Version); err != nil {
		t.Fatal(err)
	}
	a.Clock.Set(spTime(t, 2026, time.March, 21, 12))
	res := a.ingest(t, tn,
		event("susp-antes", spTime(t, 2026, time.March, 14, 10)),
		event("susp-inicio", suspendedAt),
		event("susp-durante-2", spTime(t, 2026, time.March, 16, 10)),
		event("susp-reativacao", reactivatedAt),
		event("susp-depois", spTime(t, 2026, time.March, 20, 15)),
	)
	wantOutcomes(t, res, metering.OutcomeAccepted, metering.OutcomeRejected, metering.OutcomeRejected, metering.OutcomeAccepted,
		metering.OutcomeAccepted)
	for _, r := range res {
		if r.Outcome == metering.OutcomeRejected && r.Error != "Assinatura suspensa: consumo não é faturável durante a suspensão." {
			t.Errorf("%s: motivo %q", r.ExternalID, r.Error)
		}
	}
}

// Mudança no próximo ciclo de um plano com consumo para um plano de valor
// fixo: o evento atrasado do período anterior é aceito como late (a métrica
// vale para o período fechado) e corrigido pela versão de preço do
// fechamento; o reenvio de um evento já gravado continua duplicata; consumo
// do novo período é rejeitado.
func TestLateUsageAfterScheduledPlanChangeUsesClosedPeriod(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-consumo-troca")
	metric, metered := a.newMeteredPlan(t, tn, catalog.AggSum, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "2.5"})
	flat := a.newFlatPlan(t, tn, 10000)
	sub := a.newSubscriptionQty(t, tn, metered.ID, 1)
	a.runEngine(t, tn, 0)
	if _, err := a.Changes.Change(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, billing.ChangeInput{PlanID: &flat.ID,
		Timing: subscriptions.ChangeNextCycle}, 0); err != nil {
		t.Fatal(err)
	}
	event := func(id, qty string, at time.Time) metering.EventInput {
		return metering.EventInput{ExternalID: id, SubscriptionID: sub.ID, MetricCode: metric.Code, Quantity: qty, OccurredAt: at}
	}
	first := event("troca-1", "100", spTime(t, 2026, time.March, 20, 10))
	a.Clock.Set(spTime(t, 2026, time.April, 5, 9))
	wantOutcomes(t, a.ingest(t, tn, first), metering.OutcomeAccepted)

	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	a.runEngine(t, tn, 1)
	period0 := spTime(t, 2026, time.March, 10, 0)
	agg := a.aggregateOf(t, tn.ID, sub.ID, period0)
	if agg.PriceVersionID == nil || *agg.PriceVersionID != *metered.CurrentPriceVersionID {
		t.Fatalf("agregado do período anterior: %+v", agg)
	}

	a.Clock.Set(spTime(t, 2026, time.April, 10, 10))
	res := a.ingest(t, tn,
		event("troca-2", "40", spTime(t, 2026, time.April, 9, 20)),
		first,
		event("troca-3", "1", spTime(t, 2026, time.April, 10, 8)),
	)
	wantOutcomes(t, res, metering.OutcomeLate, metering.OutcomeDuplicate, metering.OutcomeRejected)
	if res[2].Error != "A métrica não faz parte do preço vigente da assinatura." {
		t.Errorf("consumo do novo período: motivo %q", res[2].Error)
	}
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	pending := a.pendingItemsOf(t, tn.ID, sub.ID)
	if len(pending) != 1 || pending[0].Amount != 100 || pending[0].UsageAggregateID == nil || *pending[0].UsageAggregateID != agg.ID {
		t.Fatalf("correção pela versão do fechamento (40 × 2,5): %+v", pending)
	}
}

// Mudança no próximo ciclo de um plano de valor fixo para um plano com
// consumo: o período anterior foi faturado sem a métrica, então o evento
// dele recebido depois da troca é rejeitado (não seria faturado nem
// corrigido); o consumo do novo período é aceito.
func TestUsageForPeriodBilledWithoutMetricIsRejected(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-consumo-sem-metrica")
	flat := a.newFlatPlan(t, tn, 10000)
	metric, metered := a.newMeteredPlan(t, tn, catalog.AggSum, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "1"})
	sub := a.newSubscriptionQty(t, tn, flat.ID, 1)
	a.runEngine(t, tn, 1)
	if _, err := a.Changes.Change(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, billing.ChangeInput{PlanID: &metered.ID,
		Timing: subscriptions.ChangeNextCycle}, 0); err != nil {
		t.Fatal(err)
	}
	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	a.runEngine(t, tn, 0)

	a.Clock.Set(spTime(t, 2026, time.April, 11, 9))
	event := func(id string, at time.Time) metering.EventInput {
		return metering.EventInput{ExternalID: id, SubscriptionID: sub.ID, MetricCode: metric.Code, Quantity: "1", OccurredAt: at}
	}
	res := a.ingest(t, tn, event("sem-metrica-1", spTime(t, 2026, time.April, 9, 20)), event("sem-metrica-2", spTime(t, 2026, time.April, 10, 8)))
	wantOutcomes(t, res, metering.OutcomeRejected, metering.OutcomeAccepted)
	if res[0].Error != "Período já faturado sem consumo desta métrica." {
		t.Errorf("motivo %q", res[0].Error)
	}
}
