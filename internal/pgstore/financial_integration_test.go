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
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// Valores no limite exato de money.MaxAmount (10^14 centavos) na escala do
// preço unitário: o preço por unidade da versão de preço, o reajuste
// (valores anterior e resultante), o valor vigente da assinatura e o valor
// unitário do item faturado são gravados e lidos sem perda.
func TestAmountLimitStoredAtExactBoundary(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-limite")
	limit := big.NewRat(money.MaxAmount, 1)

	perUnit := a.newPerUnitPlan(t, tn, "100000000000000")
	got, err := a.Catalog.Plan(ctx, tn.ID, perUnit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Prices) != 1 || got.Prices[0].UnitAmount == nil || got.Prices[0].UnitAmount.Cmp(limit) != 0 {
		t.Fatalf("preço por unidade no limite lido do banco: %+v", got.Prices)
	}

	plan := a.newFlatPlan(t, tn, 10000)
	sub := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
	a.runEngine(t, tn, 1)
	maxAmount := money.MaxAmount
	adj, err := a.Terms.ScheduleAdjustment(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, subscriptions.AdjustmentInput{
		EffectiveDate: calendar.Date{Year: 2026, Month: time.April, Day: 10}, Kind: subscriptions.AdjustFixedAmount, NewAmount: &maxAmount,
		Reason: "Reajuste no limite"})
	if err != nil {
		t.Fatalf("reajuste no limite exato: %v", err)
	}
	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	a.runEngine(t, tn, 1)

	inv := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, spTime(t, 2026, time.April, 10, 0))
	if inv.Total != money.MaxAmount || inv.AmountDue != money.MaxAmount {
		t.Fatalf("fatura no limite: %+v", inv)
	}
	items := a.itemsOf(t, tn.ID, inv.ID)
	if len(items) != 1 || items[0].Amount != money.MaxAmount {
		t.Fatalf("item no limite: %+v", items)
	}
	texts := map[string]*string{
		"valor unitário do item": a.queryText(t, tn.ID, `SELECT unit_amount::text FROM invoice_items WHERE tenant_id = $1 AND id = $2`,
			tn.ID, items[0].ID),
		"valor anterior do reajuste": a.queryText(t, tn.ID, `SELECT previous_amount::text FROM subscription_price_adjustments
			WHERE tenant_id = $1 AND id = $2`, tn.ID, adj.ID),
		"valor resultante do reajuste": a.queryText(t, tn.ID, `SELECT resulting_amount::text FROM subscription_price_adjustments
			WHERE tenant_id = $1 AND id = $2`, tn.ID, adj.ID),
		"valor vigente da assinatura": a.queryText(t, tn.ID, `SELECT price_override_amount::text FROM subscriptions WHERE tenant_id = $1 AND id = $2`,
			tn.ID, sub.ID),
	}
	want := map[string]string{"valor unitário do item": "100000000000000", "valor anterior do reajuste": "10000",
		"valor resultante do reajuste": "100000000000000", "valor vigente da assinatura": "100000000000000"}
	for name, v := range texts {
		if v == nil || !ratEqual(t, *v, want[name]) {
			t.Errorf("%s = %v, esperado %s", name, v, want[name])
		}
	}
	adjPage, err := a.Terms.Adjustments(ctx, tn.ID, sub.ID, 100, "")
	adjs := adjPage.Items
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 1 || adjs[0].Status != subscriptions.AdjustmentApplied || adjs[0].ResultingAmount.Cmp(limit) != 0 {
		t.Errorf("reajuste aplicado lido do banco: %+v", adjs)
	}
	current, err := a.Subscriptions.Get(ctx, tn.ID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.PriceOverride == nil || current.PriceOverride.Cmp(limit) != 0 {
		t.Errorf("valor vigente da assinatura lido do banco: %v", current.PriceOverride)
	}
}

// prorationEnv cria tenant, planos de R$ 100,00 e R$ 160,00 e uma assinatura
// do primeiro, com o cupom informado aplicado antes da fatura do período
// (nil para nenhum), e emite a fatura de ciclo de 10/03 a 10/04.
func prorationEnv(t *testing.T, coupon *catalog.CouponInput) (*advEnv, fixture.Tenant, subscriptions.Subscription, catalog.PlanDetails,
	catalog.PlanDetails) {
	t.Helper()
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-base-prorrata")
	basic, pro := a.newFlatPlan(t, tn, 10000), a.newFlatPlan(t, tn, 16000)
	sub := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, basic.ID, nil)
	if coupon != nil {
		c, err := a.Coupons.Create(ctx, tn.ID, tn.Actor, *coupon)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Terms.ApplyCoupon(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, c.Code); err != nil {
			t.Fatal(err)
		}
	}
	a.runEngine(t, tn, 1)
	return a, tn, sub, basic, pro
}

// A base do crédito é o item do componente fixo da fatura de ciclo do
// período (valor menos o desconto distribuído na emissão), lido do banco.
// Período de 10/03 a 10/04 (31 dias), troca de R$ 100,00 para R$ 160,00 em
// 31/03: fração 10/31; cobrança de 16.000 × 10/31 = 5.161 sem cupom ativo
// que valha para o novo plano.
func TestImmediateChangeCreditUsesBilledCycleFee(t *testing.T) {
	ctx := context.Background()
	pct, off := 1000, int64(3000)
	cases := []struct {
		name       string
		coupon     *catalog.CouponInput
		void       bool
		wantCycle  int64
		wantBase   int64
		wantCredit int64
		wantBasis  string
	}{
		// Cupom de uso único: a fatura do período teve 10% e o cupom foi
		// encerrado em seguida; o crédito é de 9.000 × 10/31 = 2.903.
		{"cupom de uso único encerrado após o faturamento", &catalog.CouponInput{Code: "BOASVINDAS", Name: "Boas-vindas",
			Type: catalog.DiscountPercent, PercentOffBP: &pct, Duration: catalog.DurationOnce}, false, 9000, 9000, 2903, "cycle_invoice"},
		// Desconto de valor fixo no item: 7.000 × 10/31 = 2.258.
		{"desconto de valor fixo distribuído no item", &catalog.CouponInput{Code: "TRINTA", Name: "Trinta reais",
			Type: catalog.DiscountAmount, AmountOff: &off, Duration: catalog.DurationForever}, false, 7000, 7000, 2258, "cycle_invoice"},
		{"fatura de ciclo cancelada não gera crédito", nil, true, 10000, 0, 0, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, tn, sub, _, pro := prorationEnv(t, tc.coupon)
			cycle := a.invoicesOf(t, tn.ID, sub.ID)[0]
			if cycle.Total != tc.wantCycle {
				t.Fatalf("fatura de ciclo: %+v", cycle)
			}
			if tc.void {
				if _, err := a.Billing.Void(ctx, tn.ID, tn.Actor, cycle.ID, "Emitida com dados incorretos", cycle.Version); err != nil {
					t.Fatal(err)
				}
			}
			if tc.coupon != nil && tc.coupon.Type == catalog.DiscountPercent {
				terms, err := a.Terms.Terms(ctx, tn.ID, sub.ID)
				if err != nil || terms.Discount != nil {
					t.Fatalf("o cupom de uso único deveria estar encerrado: %+v %v", terms.Discount, err)
				}
			}
			a.Clock.Set(spTime(t, 2026, time.March, 31, 12))
			res, err := a.Changes.Change(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID,
				billing.ChangeInput{PlanID: &pro.ID, Timing: subscriptions.ChangeImmediately}, 0)
			if err != nil {
				t.Fatal(err)
			}
			pr := res.Proration
			if pr == nil || pr.Credit.Amount() != tc.wantCredit || pr.Charge.Amount() != 5161 || pr.Net.Amount() != 5161-tc.wantCredit {
				t.Fatalf("prorrata: %+v", pr)
			}
			inv := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindProration, spTime(t, 2026, time.March, 31, 0))
			items := a.itemsOf(t, tn.ID, inv.ID)
			if inv.Total != 5161-tc.wantCredit {
				t.Errorf("total da prorrata = %d", inv.Total)
			}
			if tc.wantCredit == 0 {
				if len(items) != 1 {
					t.Errorf("sem base faturada, somente a cobrança: %+v", items)
				}
			} else if len(items) != 2 || items[1].Amount != -tc.wantCredit || items[1].snap(t, "credit_basis") != tc.wantBasis ||
				items[1].snap(t, "billed_invoice_id") != cycle.ID.String() || items[1].snap(t, "billed_amount") != "10000" {
				t.Errorf("item de crédito com a origem da base: %+v", items)
			}
			if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM subscription_prorations WHERE tenant_id = $1 AND subscription_id = $2
				AND credit_amount = $3 AND credit_base = $4 AND charge_base = 16000 AND invoice_id = $5`, tn.ID, sub.ID, tc.wantCredit,
				tc.wantBase, inv.ID); n != 1 {
				t.Errorf("prorrata registrada = %d, esperado 1", n)
			}
			if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'subscription.plan_changed'
				AND details->>'proration_credit_basis' = $2`, tn.ID, tc.wantBasis); n != 1 {
				t.Errorf("auditoria com a origem da base = %d, esperado 1", n)
			}
		})
	}
}

// Duas mudanças imediatas no mesmo período: a segunda credita o valor
// cobrado pelo componente da primeira (registro da prorrata), e não o da
// fatura de ciclo. Básico para Pro em 31/03 (10/31) e de volta em 05/04
// (5/31): crédito de 16.000 × 5/31 = 2.581, cobrança de 10.000 × 5/31 =
// 1.613, crédito do cliente de 968. O registro da prorrata é imutável.
func TestImmediateChangesInSamePeriodUseRecordedProration(t *testing.T) {
	ctx := context.Background()
	a, tn, sub, basic, pro := prorationEnv(t, nil)
	change := func(planID uuid.UUID, at time.Time) billing.ChangeResult {
		t.Helper()
		a.Clock.Set(at)
		res, err := a.Changes.Change(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, billing.ChangeInput{PlanID: &planID, Timing: subscriptions.ChangeImmediately}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	first := change(pro.ID, spTime(t, 2026, time.March, 31, 12))
	if first.Proration == nil || first.Proration.Net.Amount() != 1935 {
		t.Fatalf("primeira mudança: %+v", first.Proration)
	}
	second := change(basic.ID, spTime(t, 2026, time.April, 5, 12))
	if second.Proration == nil || second.Proration.Credit.Amount() != 2581 || second.Proration.Charge.Amount() != 1613 ||
		second.CreditGranted == nil || second.CreditGranted.Amount() != 968 {
		t.Fatalf("segunda mudança: %+v %v", second.Proration, second.CreditGranted)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM subscription_prorations WHERE tenant_id = $1 AND subscription_id = $2
		AND remaining_days = 5 AND period_days = 31 AND credit_base = 16000 AND charge_base = 10000 AND invoice_id IS NULL`, tn.ID, sub.ID); n != 1 {
		t.Errorf("segunda prorrata registrada = %d, esperado 1", n)
	}
	if bal := a.creditBalance(t, tn.ID, sub.CustomerID); bal != 968 {
		t.Errorf("crédito do cliente = %d, esperado 968", bal)
	}
	// RLS com a política padrão: outro tenant não enxerga os registros.
	other := a.NewTenant(t, "adv-base-prorrata-outro")
	if n := countRows(t, a.Env, other.ID, `SELECT count(*) FROM subscription_prorations`); n != 0 {
		t.Errorf("outro tenant enxerga %d prorratas", n)
	}
	if n := countRows(t, a.Env, other.ID, `SELECT count(*) FROM subscription_prorations WHERE tenant_id = $1`, tn.ID); n != 0 {
		t.Errorf("outro tenant enxerga %d prorratas filtrando pelo tenant", n)
	}
	var rls bool
	var policies int
	if err := pgtest.OwnerPool(t).QueryRow(ctx, `SELECT c.relrowsecurity,
		(SELECT count(*) FROM pg_policies p WHERE p.tablename = c.relname AND p.policyname = 'tenant_isolation')::int
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = 'subscription_prorations'`).
		Scan(&rls, &policies); err != nil || !rls || policies != 1 {
		t.Errorf("subscription_prorations: rls=%v políticas=%d erro=%v", rls, policies, err)
	}
	var affected int64
	err := a.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE subscription_prorations SET credit_amount = 0 WHERE tenant_id = $1`, tn.ID)
		affected = tag.RowsAffected()
		return err
	})
	if err == nil || affected != 0 {
		t.Errorf("o registro da prorrata não pode ser alterado: %v (%d linhas)", err, affected)
	}
}

// Correção de consumo com cupom de valor fixo (R$ 10,00 por fatura): o item
// de consumo registra o tipo do desconto na emissão e a correção mantém o
// valor absoluto descontado. Fechamento com 50 unidades de R$ 1,00 (5.000
// com 1.000 de desconto); recontagem para 30 unidades vale 3.000 − 1.000 =
// 2.000 (crédito de 2.000; a proporção daria 2.400) e, depois, para 45
// unidades, 4.500 − 1.000 = 3.500 (ajuste de 1.500).
func TestLateUsageCorrectionKeepsFixedDiscountAmount(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-consumo-valor-fixo")
	metric, plan := a.newMeteredPlan(t, tn, catalog.AggLast, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "100"})
	sub := a.newSubscriptionQty(t, tn, plan.ID, 1)
	off := int64(1000)
	coupon, err := a.Coupons.Create(ctx, tn.ID, tn.Actor, catalog.CouponInput{Code: "DEZREAIS", Name: "Dez reais por fatura",
		Type: catalog.DiscountAmount, AmountOff: &off, Duration: catalog.DurationForever})
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
	wantOutcomes(t, a.ingest(t, tn, event("fixo-1", "50", spTime(t, 2026, time.March, 20, 10))), metering.OutcomeAccepted)
	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	a.runEngine(t, tn, 1)
	closing := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, spTime(t, 2026, time.April, 10, 0))
	items := a.itemsOf(t, tn.ID, closing.ID)
	if closing.Total != 4000 || len(items) != 1 || items[0].DiscountAmount != 1000 || items[0].snap(t, "discount_type") != "amount" ||
		items[0].snap(t, "coupon_code") != "DEZREAIS" || items[0].snap(t, "discount_amount_off") != "1000" {
		t.Fatalf("fechamento com o desconto de valor fixo registrado: %+v %+v", closing, items)
	}
	period0 := spTime(t, 2026, time.March, 10, 0)

	wantOutcomes(t, a.ingest(t, tn, event("fixo-2", "30", spTime(t, 2026, time.April, 9, 22))), metering.OutcomeLate)
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if bal := a.creditBalance(t, tn.ID, sub.CustomerID); bal != 2000 {
		t.Errorf("crédito pela redução = %d, esperado 2000", bal)
	}
	if c := a.aggregateOf(t, tn.ID, sub.ID, period0); c.CorrectedAmount == nil || *c.CorrectedAmount != 2000 {
		t.Errorf("valor líquido corrigido: %+v", c)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'usage.corrected'
		AND (details->>'corrected_amount')::int = 2000 AND details->>'billed_discount_type' = 'amount' AND details->>'discount_rule' = 'amount'
		AND NOT details ? 'discount_ratio'`, tn.ID); n != 1 {
		t.Errorf("auditoria da correção com desconto de valor fixo = %d, esperado 1", n)
	}

	wantOutcomes(t, a.ingest(t, tn, event("fixo-3", "45", spTime(t, 2026, time.April, 9, 23))), metering.OutcomeLate)
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	pending := a.pendingItemsOf(t, tn.ID, sub.ID)
	if len(pending) != 1 || pending[0].Amount != 1500 || pending[0].Source != billing.PendingSourceUsageCorrection {
		t.Fatalf("ajuste pela diferença líquida: %+v", pending)
	}
}
