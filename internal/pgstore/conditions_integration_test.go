//go:build integration

package pgstore_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// queryText executa uma consulta de uma linha e uma coluna textual no tenant.
func (a *advEnv) queryText(t *testing.T, tenantID uuid.UUID, query string, args ...any) *string {
	t.Helper()
	var out *string
	a.inTenant(t, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), query, args...).Scan(&out)
	})
	return out
}

// Reajuste percentual de preço por unidade com fração de centavo: o
// percentual incide sobre o preço unitário exato (R$ 0,045 × 1,05 =
// R$ 0,04725), gravado em numeric com a escala do preço, e o total do item é
// arredondado uma única vez (100.000 × 4,725 centavos = R$ 4.725,00).
func TestPerUnitPercentAdjustmentKeepsFractionalUnitPrice(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-reajuste-unidade")
	plan := a.newPerUnitPlan(t, tn, "4.5")
	sub := a.newSubscriptionQty(t, tn, plan.ID, 100_000)
	a.runEngine(t, tn, 1)

	pct := 500
	adj, err := a.Terms.ScheduleAdjustment(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, subscriptions.AdjustmentInput{
		EffectiveDate: calendar.Date{Year: 2026, Month: time.April, Day: 1}, Kind: subscriptions.AdjustPercent, PercentBP: &pct,
		Reason: "Reajuste anual"})
	if err != nil {
		t.Fatal(err)
	}
	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	a.runEngine(t, tn, 1)

	invs := a.invoicesOf(t, tn.ID, sub.ID)
	if len(invs) != 2 || invs[0].Total != 450000 || invs[1].Total != 472500 {
		t.Fatalf("faturas antes e depois do reajuste: %+v", invs)
	}
	items := a.itemsOf(t, tn.ID, invs[1].ID)
	if len(items) != 1 || items[0].snap(t, "price_adjustment_id") != adj.ID.String() || !ratEqual(t, items[0].snap(t, "unit_amount_decimal"), "4.725") ||
		!ratEqual(t, items[0].snap(t, "adjusted_amount_decimal"), "4.725") {
		t.Fatalf("item com o preço unitário reajustado: %+v", items)
	}
	unit := a.queryText(t, tn.ID, `SELECT unit_amount::text FROM invoice_items WHERE tenant_id = $1 AND id = $2`, tn.ID, items[0].ID)
	if unit == nil || !ratEqual(t, *unit, "4.725") {
		t.Errorf("valor unitário do item = %v, esperado 4,725 centavos", unit)
	}
	prev := a.queryText(t, tn.ID, `SELECT previous_amount::text FROM subscription_price_adjustments WHERE tenant_id = $1 AND id = $2`, tn.ID, adj.ID)
	res := a.queryText(t, tn.ID, `SELECT resulting_amount::text FROM subscription_price_adjustments WHERE tenant_id = $1 AND id = $2`, tn.ID, adj.ID)
	override := a.queryText(t, tn.ID, `SELECT price_override_amount::text FROM subscriptions WHERE tenant_id = $1 AND id = $2`, tn.ID, sub.ID)
	if prev == nil || res == nil || override == nil || !ratEqual(t, *prev, "4.5") || !ratEqual(t, *res, "4.725") || !ratEqual(t, *override, "4.725") {
		t.Errorf("valores gravados: anterior %v resultante %v assinatura %v", prev, res, override)
	}
	adjPage, err := a.Terms.Adjustments(ctx, tn.ID, sub.ID, 100, "")
	adjs := adjPage.Items
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 1 || adjs[0].PreviousAmount.Cmp(big.NewRat(9, 2)) != 0 || adjs[0].ResultingAmount.Cmp(big.NewRat(189, 40)) != 0 {
		t.Errorf("reajuste lido do banco: %+v", adjs)
	}
	got, err := a.Subscriptions.Get(ctx, tn.ID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PriceOverride == nil || got.PriceOverride.Cmp(big.NewRat(189, 40)) != 0 {
		t.Errorf("preço unitário vigente da assinatura: %v", got.PriceOverride)
	}
	page, err := a.Subscriptions.List(ctx, tn.ID, subscriptions.ListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].PriceOverride == nil || page.Items[0].PriceOverride.Cmp(big.NewRat(189, 40)) != 0 {
		t.Errorf("listagem com o preço reajustado: %+v", page.Items)
	}

	// O valor resultante acima do limite (total do componente fixo com a
	// quantidade contratada) é rejeitado já no agendamento.
	if _, err := a.Terms.ScheduleAdjustment(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, subscriptions.AdjustmentInput{
		EffectiveDate: calendar.Date{Year: 2026, Month: time.May, Day: 1}, Kind: subscriptions.AdjustFixedAmount, NewAmount: new(int64(1_000_000_000_000)),
		Reason: "Valor excessivo"}); errCode(err) != "invalid_adjustment" {
		t.Errorf("reajuste acima do limite aceito: %v", err)
	}
}

// A troca da versão de preço cancela os reajustes programados, tanto na
// mudança imediata quanto na mudança agendada aplicada pelo motor, com o
// motivo gravado e o evento no histórico.
func TestPriceVersionChangeCancelsScheduledAdjustments(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-reajuste-troca")
	basic, pro := a.newFlatPlan(t, tn, 10000), a.newFlatPlan(t, tn, 16000)
	immediateSub := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, basic.ID, nil)
	scheduledSub := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, basic.ID, nil)
	a.runEngine(t, tn, 2)

	schedule := func(sub subscriptions.Subscription, day calendar.Date) subscriptions.PriceAdjustment {
		t.Helper()
		pct := 1000
		adj, err := a.Terms.ScheduleAdjustment(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, subscriptions.AdjustmentInput{
			EffectiveDate: day, Kind: subscriptions.AdjustPercent, PercentBP: &pct, Reason: "Reajuste do plano básico"})
		if err != nil {
			t.Fatal(err)
		}
		return adj
	}
	immediateAdj := schedule(immediateSub, calendar.Date{Year: 2026, Month: time.May, Day: 1})
	scheduledAdj := schedule(scheduledSub, calendar.Date{Year: 2026, Month: time.April, Day: 10})

	a.Clock.Set(spTime(t, 2026, time.March, 31, 12))
	if _, err := a.Changes.Change(ctx, tn.ID, tn.Actor, &tn.Owner.ID, immediateSub.ID,
		billing.ChangeInput{PlanID: &pro.ID, Timing: subscriptions.ChangeImmediately}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Changes.Change(ctx, tn.ID, tn.Actor, &tn.Owner.ID, scheduledSub.ID,
		billing.ChangeInput{PlanID: &pro.ID, Timing: subscriptions.ChangeNextCycle}, 0); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM subscription_price_adjustments WHERE tenant_id = $1 AND id = $2
		AND status = 'canceled' AND cancel_reason = 'price_version_changed' AND canceled_at IS NOT NULL`, tn.ID, immediateAdj.ID); n != 1 {
		t.Errorf("mudança imediata não cancelou o reajuste programado")
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM subscription_price_adjustments WHERE tenant_id = $1 AND id = $2
		AND status = 'scheduled'`, tn.ID, scheduledAdj.ID); n != 1 {
		t.Errorf("o agendamento da mudança não pode cancelar o reajuste antes de a mudança valer")
	}

	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	a.runEngine(t, tn, 2)
	next := findInvoice(t, a.invoicesOf(t, tn.ID, scheduledSub.ID), billing.KindSubscriptionCycle, spTime(t, 2026, time.April, 10, 0))
	if next.Total != 16000 {
		t.Errorf("o novo plano é cobrado pelo preço contratado, sem o reajuste do plano anterior: %d", next.Total)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM subscription_price_adjustments WHERE tenant_id = $1 AND id = $2
		AND status = 'canceled' AND cancel_reason = 'price_version_changed'`, tn.ID, scheduledAdj.ID); n != 1 {
		t.Errorf("mudança aplicada pelo motor não cancelou o reajuste programado")
	}
	for _, c := range []struct {
		sub subscriptions.Subscription
		adj subscriptions.PriceAdjustment
	}{{immediateSub, immediateAdj}, {scheduledSub, scheduledAdj}} {
		if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM subscription_history WHERE tenant_id = $1 AND subscription_id = $2
			AND event = 'price_adjustment_canceled' AND details->>'adjustment_id' = $3 AND details->>'reason' = 'price_version_changed'`,
			tn.ID, c.sub.ID, c.adj.ID.String()); n != 1 {
			t.Errorf("histórico do cancelamento do reajuste %s = %d, esperado 1", c.adj.ID, n)
		}
		got, err := a.Subscriptions.Get(ctx, tn.ID, c.sub.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.PlanID != pro.ID || got.PriceOverride != nil {
			t.Errorf("assinatura no novo plano sem reajuste: %+v", got)
		}
	}
}

// Item pendente em aberto é faturado no fechamento: o cancelamento imediato
// de plano sem consumo agenda o passo final, que emite uma fatura de ajuste.
func TestPendingItemsBilledWhenSubscriptionEndsImmediately(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-pendentes-encerramento")
	plan := a.newFlatPlan(t, tn, 10000)
	withPending := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
	withoutPending := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
	a.runEngine(t, tn, 2)

	pendingID := uuid.New()
	periodStart, periodEnd := spTime(t, 2026, time.February, 10, 0), spTime(t, 2026, time.March, 10, 0)
	a.inTenant(t, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO pending_invoice_items (id, tenant_id, customer_id, subscription_id, kind, source, description,
			amount, period_start, period_end, details, created_at)
			VALUES ($1, $2, $3, $4, 'adjustment', 'usage_correction', 'Correção de consumo do período anterior', 4000, $5, $6, '{}', $7)`,
			pendingID, tn.ID, withPending.CustomerID, withPending.ID, periodStart, periodEnd, a.Clock.Now())
		return err
	})

	a.Clock.Set(spTime(t, 2026, time.March, 20, 12))
	canceled, err := a.Subscriptions.Cancel(ctx, tn.ID, tn.Actor, withPending.ID, subscriptions.CancelImmediately, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.NextBillingAt == nil || !canceled.NextBillingAt.Equal(a.Clock.Now()) {
		t.Fatalf("itens pendentes agendam o fechamento: %v", canceled.NextBillingAt)
	}
	other, err := a.Subscriptions.Cancel(ctx, tn.ID, tn.Actor, withoutPending.ID, subscriptions.CancelImmediately, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if other.NextBillingAt != nil {
		t.Fatalf("sem itens pendentes nem consumo, não há fechamento: %v", other.NextBillingAt)
	}

	a.runEngine(t, tn, 1)
	invs := a.invoicesOf(t, tn.ID, withPending.ID)
	if len(invs) != 2 {
		t.Fatalf("faturas da assinatura: %+v", invs)
	}
	adj := invs[0]
	if adj.Kind != string(billing.KindManual) || adj.Total != 4000 || !adj.PeriodStart.Equal(periodStart) || !adj.PeriodEnd.Equal(periodEnd) {
		t.Fatalf("fatura de ajuste com o item pendente: %+v", adj)
	}
	items := a.itemsOf(t, tn.ID, adj.ID)
	if len(items) != 1 || items[0].Kind != string(billing.ItemAdjustment) || items[0].Amount != 4000 || items[0].DiscountAmount != 0 {
		t.Fatalf("itens da fatura de ajuste: %+v", items)
	}
	pending := a.pendingItemsOf(t, tn.ID, withPending.ID)
	if len(pending) != 1 || pending[0].InvoiceID == nil || *pending[0].InvoiceID != adj.ID {
		t.Fatalf("item pendente vinculado à fatura de ajuste: %+v", pending)
	}
	got, err := a.Subscriptions.Get(ctx, tn.ID, withPending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != subscriptions.Canceled || got.NextBillingAt != nil {
		t.Fatalf("assinatura encerrada: %+v", got)
	}
	a.Clock.Set(spTime(t, 2026, time.March, 21, 12))
	a.runEngine(t, tn, 0)
}

// Correção de consumo com cupom de 50% no período fechado: a comparação usa o
// valor líquido do item, o novo valor recebe a mesma proporção de desconto e
// o ajuste positivo não recebe novo desconto na fatura seguinte.
func TestLateUsageCorrectionUsesNetOfDiscount(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-consumo-desconto")
	metric, plan := a.newMeteredPlan(t, tn, catalog.AggLast, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "100"})
	sub := a.newSubscriptionQty(t, tn, plan.ID, 1)
	pct := 5000
	coupon, err := a.Coupons.Create(ctx, tn.ID, tn.Actor, catalog.CouponInput{Code: "METADE", Name: "Metade do valor",
		Type: catalog.DiscountPercent, PercentOffBP: &pct, Duration: catalog.DurationForever})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Terms.ApplyCoupon(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, coupon.Code); err != nil {
		t.Fatal(err)
	}
	a.runEngine(t, tn, 0)
	event := func(id, qty string, at time.Time) metering.EventInput {
		return metering.EventInput{ExternalID: id, SubscriptionID: sub.ID, MetricCode: metric.Code, Quantity: qty, OccurredAt: at}
	}
	a.Clock.Set(spTime(t, 2026, time.April, 6, 9))
	wantOutcomes(t, a.ingest(t, tn,
		event("arm-1", "50", spTime(t, 2026, time.March, 20, 10)),
		event("arm-2", "40", spTime(t, 2026, time.April, 5, 10)),
	), metering.OutcomeAccepted, metering.OutcomeAccepted)
	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	a.runEngine(t, tn, 1)
	closing := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, spTime(t, 2026, time.April, 10, 0))
	if closing.Subtotal != 4000 || closing.DiscountTotal != 2000 || closing.Total != 2000 {
		t.Fatalf("fechamento com 50%% de desconto: %+v", closing)
	}
	// O item de consumo registra o tipo e o cupom do desconto aplicado na emissão.
	if items := a.itemsOf(t, tn.ID, closing.ID); len(items) != 1 || items[0].snap(t, "discount_type") != "percent" ||
		items[0].snap(t, "coupon_code") != "METADE" || items[0].snap(t, "discount_percent_bp") != "5000" {
		t.Fatalf("snapshot do item com o desconto: %+v", items)
	}
	period0 := spTime(t, 2026, time.March, 10, 0)

	// 30 unidades valem 3.000 bruto e 1.500 líquido: crédito de 500 (2.000 − 1.500).
	wantOutcomes(t, a.ingest(t, tn, event("arm-3", "30", spTime(t, 2026, time.April, 9, 22))), metering.OutcomeLate)
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if bal := a.creditBalance(t, tn.ID, sub.CustomerID); bal != 500 {
		t.Errorf("crédito pelo líquido cobrado a mais = %d, esperado 500", bal)
	}
	if c := a.aggregateOf(t, tn.ID, sub.ID, period0); c.CorrectedAmount == nil || *c.CorrectedAmount != 1500 {
		t.Errorf("o agregado guarda o valor líquido corrigido: %+v", c)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'usage.corrected'
		AND (details->>'previous_amount')::int = 2000 AND (details->>'corrected_amount')::int = 1500
		AND (details->>'corrected_gross_amount')::int = 3000 AND details->>'discount_ratio' = '0.5'
		AND details->>'billed_discount_type' = 'percent' AND details->>'discount_rule' = 'percent'`, tn.ID); n != 1 {
		t.Errorf("auditoria da correção líquida = %d, esperado 1", n)
	}

	// 45 unidades valem 2.250 líquido: ajuste de 750 sobre os 1.500 corrigidos.
	wantOutcomes(t, a.ingest(t, tn, event("arm-4", "45", spTime(t, 2026, time.April, 9, 23))), metering.OutcomeLate)
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	pending := a.pendingItemsOf(t, tn.ID, sub.ID)
	if len(pending) != 1 || pending[0].Amount != 750 || pending[0].Source != billing.PendingSourceUsageCorrection {
		t.Fatalf("item pendente pelo líquido: %+v", pending)
	}

	a.Clock.Set(spTime(t, 2026, time.April, 25, 9))
	wantOutcomes(t, a.ingest(t, tn, event("arm-5", "25", spTime(t, 2026, time.April, 20, 10))), metering.OutcomeAccepted)
	a.Clock.Set(spTime(t, 2026, time.May, 10, 9))
	a.runEngine(t, tn, 1)
	next := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, spTime(t, 2026, time.May, 10, 0))
	// Consumo de 2.500 com 1.250 de desconto; ajuste de 750 sem novo desconto.
	if next.Subtotal != 3250 || next.DiscountTotal != 1250 || next.Total != 2000 || next.CreditApplied != 500 || next.AmountDue != 1500 {
		t.Errorf("fatura seguinte: %+v", next)
	}
	for _, it := range a.itemsOf(t, tn.ID, next.ID) {
		switch billing.ItemKind(it.Kind) {
		case billing.ItemAdjustment:
			if it.Amount != 750 || it.DiscountAmount != 0 {
				t.Errorf("ajuste da correção com novo desconto: %+v", it)
			}
		case billing.ItemUsage:
			if it.Amount != 2500 || it.DiscountAmount != 1250 {
				t.Errorf("consumo do período com desconto: %+v", it)
			}
		}
	}
}
