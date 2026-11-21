//go:build integration

package pgstore_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// aggregationCase descreve o ciclo de uma métrica medida: eventos aceitos e
// rejeitados do período 0 (março a abril), quantidade esperada no
// fechamento, eventos atrasados com o ajuste esperado de cada um e consumo
// do período 1.
type aggregationCase struct {
	aggregation  catalog.Aggregation
	closing      []aggEvent
	rejected     []aggEvent
	closingQty   string
	closingCount int
	late         []lateStep
	nextPeriod   aggEvent
	nextQty      string
}

type aggEvent struct {
	id, qty, key string
	at           time.Time
}

// lateStep é um evento atrasado do período 0 e o efeito esperado da
// correção: delta 0 significa nenhum ajuste.
type lateStep struct {
	event         aggEvent
	wantDelta     int64
	wantCorrected string
}

// runAggregationCase fecha o período 0 com os eventos do caso e confere a
// quantidade faturada no item de consumo e no agregado; em seguida ingere os
// eventos atrasados um a um, roda a correção e confere o ajuste gerado (ou
// a ausência dele); por fim, fecha o período 1 e confere que a fatura
// seguinte inclui exatamente os ajustes.
func runAggregationCase(t *testing.T, a *advEnv, tn fixture.Tenant, tc aggregationCase) {
	t.Helper()
	ctx := context.Background()
	const unitCents = 100
	metric, plan := a.newMeteredPlan(t, tn, tc.aggregation, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "100"})
	sub := a.newSubscriptionQty(t, tn, plan.ID, 1)
	a.runEngine(t, tn, 0)
	toInput := func(e aggEvent) metering.EventInput {
		return metering.EventInput{ExternalID: e.id, SubscriptionID: sub.ID, MetricCode: metric.Code, Quantity: e.qty, UniqueKey: e.key,
			OccurredAt: e.at}
	}
	period0 := spTime(t, 2026, time.March, 10, 0)
	period1 := spTime(t, 2026, time.April, 10, 0)
	period2 := spTime(t, 2026, time.May, 10, 0)

	a.Clock.Set(spTime(t, 2026, time.April, 5, 9))
	for _, e := range tc.closing {
		wantOutcomes(t, a.ingest(t, tn, toInput(e)), metering.OutcomeAccepted)
	}
	for _, e := range tc.rejected {
		wantOutcomes(t, a.ingest(t, tn, toInput(e)), metering.OutcomeRejected)
	}

	// Fechamento do período 0.
	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	a.runEngine(t, tn, 1)
	closing := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, period1)
	items := a.itemsOf(t, tn.ID, closing.ID)
	if len(items) != 1 || items[0].Kind != string(billing.ItemUsage) || !ratEqual(t, items[0].Quantity, tc.closingQty) {
		t.Fatalf("itens do fechamento: %+v; esperado um item de consumo com quantidade %s", items, tc.closingQty)
	}
	closingAmount := wholeQty(t, tc.closingQty) * unitCents
	if items[0].Amount != closingAmount || closing.Total != closingAmount {
		t.Errorf("valor do fechamento: item %d, fatura %d; esperado %d", items[0].Amount, closing.Total, closingAmount)
	}
	if got := items[0].snap(t, "aggregation"); got != string(tc.aggregation) {
		t.Errorf("agregação no snapshot = %q, esperado %q", got, tc.aggregation)
	}
	if got := items[0].snap(t, "aggregated_quantity"); !ratEqual(t, got, tc.closingQty) {
		t.Errorf("quantidade agregada no snapshot = %s, esperado %s", got, tc.closingQty)
	}
	agg := a.aggregateOf(t, tn.ID, sub.ID, period0)
	if !ratEqual(t, agg.Quantity, tc.closingQty) || agg.EventCount != tc.closingCount || agg.InvoiceItemID == nil ||
		*agg.InvoiceItemID != items[0].ID || agg.CorrectedAmount != nil {
		t.Fatalf("agregado fechado: %+v; esperado quantidade %s com %d eventos", agg, tc.closingQty, tc.closingCount)
	}

	// Eventos atrasados do período 0, corrigidos um a um.
	a.Clock.Set(spTime(t, 2026, time.April, 10, 10))
	var wantPending []int64
	noEffect, nextInvoice := 0, 0
	for _, step := range tc.late {
		wantOutcomes(t, a.ingest(t, tn, toInput(step.event)), metering.OutcomeLate)
		if _, err := a.Corrector.RunAll(ctx); err != nil {
			t.Fatalf("correção do evento %s: %v", step.event.id, err)
		}
		if step.wantDelta > 0 {
			wantPending = append(wantPending, step.wantDelta)
			nextInvoice++
		} else {
			noEffect++
		}
		pending := a.pendingItemsOf(t, tn.ID, sub.ID)
		if len(pending) != len(wantPending) {
			t.Fatalf("após o evento atrasado %s: %d itens pendentes (%+v), esperado %d", step.event.id, len(pending), pending, len(wantPending))
		}
		for i, p := range pending {
			if p.Amount != wantPending[i] || p.Kind != string(billing.ItemAdjustment) || p.Source != billing.PendingSourceUsageCorrection ||
				p.UsageAggregateID == nil || *p.UsageAggregateID != agg.ID {
				t.Errorf("item pendente %d após o evento %s: %+v; esperado ajuste de %d", i, step.event.id, p, wantPending[i])
			}
		}
		ev := a.usageEvent(t, tn.ID, step.event.id)
		if ev.Status != string(metering.Late) || ev.CorrectedAt == nil || ev.AggregateID == nil || *ev.AggregateID != agg.ID {
			t.Errorf("evento atrasado %s após a correção: %+v", step.event.id, ev)
		}
		switch {
		case step.wantDelta > 0 && (ev.CorrectionItemID == nil || *ev.CorrectionItemID != pending[len(pending)-1].ID):
			t.Errorf("evento atrasado %s sem vínculo com o ajuste: %+v", step.event.id, ev)
		case step.wantDelta == 0 && ev.CorrectionItemID != nil:
			t.Errorf("evento atrasado %s sem efeito vinculado a item: %+v", step.event.id, ev)
		}
		c := a.aggregateOf(t, tn.ID, sub.ID, period0)
		wantAmount := wholeQty(t, step.wantCorrected) * unitCents
		if !ratEqual(t, c.Quantity, tc.closingQty) || c.CorrectedQuantity == nil || !ratEqual(t, *c.CorrectedQuantity, step.wantCorrected) ||
			c.CorrectedAmount == nil || *c.CorrectedAmount != wantAmount {
			t.Errorf("agregado após o evento %s: %+v; esperado quantidade corrigida %s e valor %d", step.event.id, c, step.wantCorrected, wantAmount)
		}
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'usage.corrected'
		AND details->>'outcome' = 'no_effect' AND (details->>'delta')::int = 0 AND details->>'aggregation' = $2`, tn.ID, string(tc.aggregation)); n != noEffect {
		t.Errorf("correções sem efeito auditadas = %d, esperado %d", n, noEffect)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'usage.corrected'
		AND details->>'outcome' = 'next_invoice'`, tn.ID); n != nextInvoice {
		t.Errorf("correções com ajuste auditadas = %d, esperado %d", n, nextInvoice)
	}
	if after := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, period1); after != closing {
		t.Errorf("a correção alterou a fatura emitida: antes %+v depois %+v", closing, after)
	}

	// Período 1: a agregação recomeça e a fatura seguinte leva os ajustes.
	a.Clock.Set(spTime(t, 2026, time.April, 25, 9))
	wantOutcomes(t, a.ingest(t, tn, toInput(tc.nextPeriod)), metering.OutcomeAccepted)
	a.Clock.Set(spTime(t, 2026, time.May, 10, 9))
	a.runEngine(t, tn, 1)
	next := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, period2)
	var adjustments, usage int64
	for _, it := range a.itemsOf(t, tn.ID, next.ID) {
		switch billing.ItemKind(it.Kind) {
		case billing.ItemAdjustment:
			adjustments += it.Amount
		case billing.ItemUsage:
			usage += it.Amount
			if !ratEqual(t, it.Quantity, tc.nextQty) {
				t.Errorf("quantidade do período 1 = %s, esperado %s", it.Quantity, tc.nextQty)
			}
		}
	}
	var wantAdjustments int64
	for _, d := range wantPending {
		wantAdjustments += d
	}
	wantUsage := wholeQty(t, tc.nextQty) * unitCents
	if adjustments != wantAdjustments || usage != wantUsage || next.Total != wantAdjustments+wantUsage {
		t.Errorf("fatura seguinte: ajustes %d, consumo %d, total %d; esperado %d, %d e %d", adjustments, usage, next.Total,
			wantAdjustments, wantUsage, wantAdjustments+wantUsage)
	}
}

// wholeQty converte uma quantidade inteira escrita em texto para int64.
func wholeQty(t *testing.T, qty string) int64 {
	t.Helper()
	r, ok := new(big.Rat).SetString(qty)
	if !ok || !r.IsInt() || !r.Num().IsInt64() {
		t.Fatalf("quantidade %q não é inteira", qty)
	}
	return r.Num().Int64()
}

// Métrica "max" (aceite do RF-07: a métrica max não soma eventos): o
// fechamento fatura o pico do período, não a soma nem a contagem; um evento
// atrasado abaixo do máximo não gera ajuste, um acima gera ajuste somente
// pela diferença entre o novo máximo e o já faturado, e um evento posterior
// abaixo do máximo corrigido também não gera ajuste.
func TestUsageMaxAggregationBillsPeakAtClosingAndLateCorrection(t *testing.T) {
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "agg-max")
	runAggregationCase(t, a, tn, aggregationCase{
		aggregation: catalog.AggMax,
		// Soma 150, contagem 3, último valor 50: só o máximo dá 70.
		closing: []aggEvent{
			{id: "max-1", qty: "30", at: spTime(t, 2026, time.March, 15, 10)},
			{id: "max-2", qty: "70", at: spTime(t, 2026, time.March, 25, 10)},
			{id: "max-3", qty: "50", at: spTime(t, 2026, time.April, 4, 10)},
		},
		closingQty:   "70",
		closingCount: 3,
		late: []lateStep{
			// Abaixo do máximo: sem ajuste (a soma faturaria mais 60).
			{event: aggEvent{id: "max-4", qty: "60", at: spTime(t, 2026, time.April, 9, 22)}, wantCorrected: "70"},
			// Novo máximo 95: ajuste de (95 − 70) × 100 = 2.500, não 9.500.
			{event: aggEvent{id: "max-5", qty: "95", at: spTime(t, 2026, time.April, 8, 12)}, wantDelta: 2500, wantCorrected: "95"},
			// Acima do faturado no fechamento, abaixo do máximo corrigido.
			{event: aggEvent{id: "max-6", qty: "80", at: spTime(t, 2026, time.April, 9, 12)}, wantCorrected: "95"},
		},
		nextPeriod: aggEvent{id: "max-7", qty: "10", at: spTime(t, 2026, time.April, 20, 15)},
		nextQty:    "10",
	})
}

// Métrica "unique_count": o fechamento fatura a quantidade de unique_key
// distintas (não a soma, a contagem de eventos nem o máximo); evento sem
// unique_key é rejeitado; um evento atrasado com chave já contada não gera
// ajuste e uma chave nova gera ajuste de uma unidade; a contagem recomeça a
// cada período.
func TestUsageUniqueCountAggregationBillsDistinctKeysAtClosingAndLateCorrection(t *testing.T) {
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "agg-unique")
	runAggregationCase(t, a, tn, aggregationCase{
		aggregation: catalog.AggUniqueCount,
		// Evento sem unique_key é rejeitado na ingestão.
		rejected: []aggEvent{{id: "uc-sem-chave", qty: "1", at: spTime(t, 2026, time.March, 20, 8)}},
		// Chaves a, b, a, c com quantidades 5, 1, 7, 2: soma 15, contagem
		// 4, máximo 7; só a contagem distinta dá 3.
		closing: []aggEvent{
			{id: "uc-1", qty: "5", key: "usuario-a", at: spTime(t, 2026, time.March, 15, 10)},
			{id: "uc-2", qty: "1", key: "usuario-b", at: spTime(t, 2026, time.March, 18, 10)},
			{id: "uc-3", qty: "7", key: "usuario-a", at: spTime(t, 2026, time.March, 25, 10)},
			{id: "uc-4", qty: "2", key: "usuario-c", at: spTime(t, 2026, time.April, 4, 10)},
		},
		closingQty:   "3",
		closingCount: 4,
		late: []lateStep{
			// Chave já contada, com quantidade acima do máximo: sem ajuste.
			{event: aggEvent{id: "uc-5", qty: "9", key: "usuario-b", at: spTime(t, 2026, time.April, 9, 22)}, wantCorrected: "3"},
			// Chave nova: ajuste de uma unidade (100), não de 4 unidades.
			{event: aggEvent{id: "uc-6", qty: "4", key: "usuario-d", at: spTime(t, 2026, time.April, 8, 12)}, wantDelta: 100, wantCorrected: "4"},
			// Chave contada na correção anterior: sem ajuste.
			{event: aggEvent{id: "uc-7", qty: "1", key: "usuario-d", at: spTime(t, 2026, time.April, 9, 12)}, wantCorrected: "4"},
		},
		// usuario-a, já contado no período 0, volta a contar no período 1.
		nextPeriod: aggEvent{id: "uc-8", qty: "3", key: "usuario-a", at: spTime(t, 2026, time.April, 20, 15)},
		nextQty:    "1",
	})
}
