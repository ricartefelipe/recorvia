package billing

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// Assinaturas iniciadas em 01/10/2026: período 0 de 01/10 a 31/10 e período
// 1 de 01/11 a 30/11. O motor roda em 01/11/2026 às 10h.
var (
	termsStart = local(2026, time.October, 1, 0, 0)
	termsNow   = local(2026, time.November, 1, 10, 0)
)

type termsFixture struct {
	store    *advancedFake
	clock    *clock.Manual
	engine   *Engine
	tenantID uuid.UUID
	actor    audit.Actor
	basic    offer // R$ 100,00
	pro      offer // R$ 160,00
	seats    offer // R$ 30,00 por unidade
	metered  offer // R$ 0,02 por unidade consumida
}

func newTermsFixture(t *testing.T) *termsFixture {
	t.Helper()
	f := &termsFixture{store: newAdvancedFake(), clock: clock.NewManual(termsNow), tenantID: uuid.New(), actor: audit.System("teste")}
	f.basic = f.add(newOffer("basico", "Plano Básico", calendar.Monthly, catalog.Flat, 10000))
	f.pro = f.add(newOffer("pro", "Plano Pro", calendar.Monthly, catalog.Flat, 16000))
	f.seats = f.add(newOffer("equipe", "Plano Equipe", calendar.Monthly, catalog.PerUnit, 3000))
	f.metered = f.add(newOffer("api", "Plano API", calendar.Monthly, catalog.Usage, 2))
	f.engine = NewEngine(f.store, f.clock)
	return f
}

func (f *termsFixture) add(o offer) offer {
	f.store.plans[o.plan.ID] = o.plan
	f.store.prices[o.price.ID] = o.price
	return o
}

// addSub cria a assinatura; billed informa se o período 0 já foi faturado.
func (f *termsFixture) addSub(t *testing.T, o offer, qty int, billed bool, mutate func(*subscriptions.Subscription)) subscriptions.Subscription {
	t.Helper()
	sub, err := subscriptions.New(subscriptions.NewInput{ID: uuid.New(), TenantID: f.tenantID, CustomerID: uuid.New(), Plan: o.plan, Price: o.price,
		Quantity: qty, StartAt: termsStart, Location: saoPaulo, Now: termsStart})
	if err != nil {
		t.Fatal(err)
	}
	if billed {
		if err := sub.ApplyStep(subscriptions.BillingStep{Kind: subscriptions.StepBill, Index: 0, Period: f.period(t, sub, 0)}, termsStart); err != nil {
			t.Fatal(err)
		}
	}
	sub.PopEvents()
	if mutate != nil {
		mutate(&sub)
	}
	f.store.subs = append(f.store.subs, sub)
	return sub
}

func (f *termsFixture) period(t *testing.T, sub subscriptions.Subscription, n int) calendar.Period {
	t.Helper()
	sched, err := sub.Schedule()
	if err != nil {
		t.Fatal(err)
	}
	return sched.Period(n)
}

// schedule agenda uma mudança para o início do período 1.
func (f *termsFixture) schedule(t *testing.T, sub subscriptions.Subscription, to *offer, qty *int) *subscriptions.ScheduledChange {
	t.Helper()
	ch := &subscriptions.ScheduledChange{ID: uuid.New(), TenantID: f.tenantID, SubscriptionID: sub.ID, Kind: subscriptions.QuantityChange,
		NewQuantity: qty, EffectiveAt: f.period(t, sub, 1).Start, Status: subscriptions.ChangePending, CreatedAt: termsStart}
	if to != nil {
		ch.Kind, ch.NewPlanID, ch.NewPriceVersionID = subscriptions.PlanChange, &to.plan.ID, &to.price.ID
	}
	f.store.changes[sub.ID] = ch
	return ch
}

func (f *termsFixture) adjustment(sub subscriptions.Subscription, effective calendar.Date, kind subscriptions.AdjustmentKind, value int64) subscriptions.PriceAdjustment {
	a := subscriptions.PriceAdjustment{ID: uuid.New(), TenantID: f.tenantID, SubscriptionID: sub.ID, EffectiveDate: effective, Kind: kind,
		Reason: "Reajuste contratual", Status: subscriptions.AdjustmentScheduled, CreatedAt: termsStart}
	switch kind {
	case subscriptions.AdjustPercent:
		bp := int(value)
		a.PercentBP = &bp
	case subscriptions.AdjustFixedAmount:
		m := brl(value)
		a.NewAmount = &m
	}
	f.store.adjustments[sub.ID] = append(f.store.adjustments[sub.ID], a)
	return a
}

func (f *termsFixture) run(t *testing.T) RunResult {
	t.Helper()
	res, err := f.engine.RunTenant(context.Background(), f.tenantID, f.actor)
	if err != nil {
		t.Fatalf("RunTenant: %v", err)
	}
	return res
}

func findAudit(entries []audit.Entry, action string) (audit.Entry, bool) {
	for _, e := range entries {
		if e.Action == action {
			return e, true
		}
	}
	return audit.Entry{}, false
}

func historyEvent(w []subHistoryWrite, event string) (subscriptions.HistoryEvent, bool) {
	for _, h := range w {
		for _, e := range h.events {
			if e.Event == event {
				return e, true
			}
		}
	}
	return subscriptions.HistoryEvent{}, false
}

func TestEngineAppliesScheduledPlanChangeAtCycleStart(t *testing.T) {
	f := newTermsFixture(t)
	aggID := uuid.New()
	sub := f.addSub(t, f.metered, 1, false, nil)
	ch := f.schedule(t, sub, &f.pro, nil)
	usagePeriod := f.period(t, sub, 0)
	f.store.usage = func(_ subscriptions.Subscription, p calendar.Period) []UsageLine {
		if !samePeriod(p, usagePeriod) {
			t.Fatalf("consumo consultado para período inesperado: %+v", p)
		}
		return []UsageLine{{MetricID: *f.metered.price.MetricID, MetricCode: "api_calls", MetricName: "Requisições", Unit: "requisicao",
			Aggregation: "sum", Quantity: big.NewRat(1500, 1), AggregateID: &aggID}}
	}

	res := f.run(t)
	if res.Invoices != 1 || len(f.store.invoices) != 1 {
		t.Fatalf("o período 0 (somente consumo) não gera fatura; o período 1 sim: %+v", res)
	}
	inv := f.store.invoices[0]
	if !samePeriod(*inv.BillingPeriod, f.period(t, sub, 1)) || len(inv.Items) != 2 {
		t.Fatalf("fatura do período 1: %+v", inv.Items)
	}
	fee, usage := inv.Items[0], inv.Items[1]
	if fee.Kind != ItemSubscriptionFee || fee.Amount.Amount() != 16000 || *fee.PlanID != f.pro.plan.ID || *fee.PriceVersionID != f.pro.price.ID {
		t.Fatalf("o componente fixo do novo período usa o novo plano: %+v", fee)
	}
	if usage.Kind != ItemUsage || usage.Amount.Amount() != 3000 || *usage.PlanID != f.metered.plan.ID ||
		*usage.PriceVersionID != f.metered.price.ID || usage.PriceSnapshot["plan_code"] != "api" || !samePeriod(*usage.Period, usagePeriod) {
		t.Fatalf("o consumo do período anterior usa a versão antiga: %+v", usage)
	}
	if len(f.store.usagePrices) != 1 || f.store.usagePrices[0] != f.metered.price.ID {
		t.Fatalf("a consulta de consumo recebe a versão antiga: %v", f.store.usagePrices)
	}
	if f.store.usageBilled[aggID] != usage.ID || inv.Total.Amount() != 19000 {
		t.Fatalf("vínculo do agregado e total: %v", inv.Total)
	}

	got := f.store.sub(sub.ID)
	if got.PlanID != f.pro.plan.ID || got.PriceVersionID != f.pro.price.ID || got.NextBillingIndex != 2 {
		t.Fatalf("assinatura no novo plano: %+v", got)
	}
	if f.store.resolved[ch.ID] != subscriptions.ChangeApplied || f.store.changes[sub.ID].Status != subscriptions.ChangeApplied {
		t.Fatal("a mudança agendada é resolvida como aplicada")
	}
	if len(f.store.termsCanceled) != 0 {
		t.Fatal("faturamento de ciclo não cancela condições programadas")
	}
	a, ok := findAudit(f.store.audits, "subscription.plan_changed")
	if !ok || a.Actor != audit.System("billing.engine") || a.Details["scheduled_change_id"] != ch.ID || a.Details["plan_id"] != f.pro.plan.ID ||
		a.Details["quantity"] != 1 {
		t.Fatalf("auditoria da mudança aplicada: %+v", f.store.audits)
	}
	ev, ok := historyEvent(f.store.subHistory, "plan_changed")
	if !ok || ev.Details["scheduled_change_id"] != ch.ID || ev.Details["timing"] != subscriptions.ChangeNextCycle ||
		ev.Details["period_start"] != f.period(t, sub, 1).Start || ev.Details["to_plan_id"] != f.pro.plan.ID {
		t.Fatalf("histórico da mudança aplicada: %+v", ev)
	}
}

func TestEngineAppliesScheduledQuantityChange(t *testing.T) {
	f := newTermsFixture(t)
	sub := f.addSub(t, f.seats, 2, true, nil)
	f.schedule(t, sub, nil, intPtr(5))

	f.run(t)
	inv := f.store.invoices[0]
	if len(inv.Items) != 1 || inv.Items[0].Amount.Amount() != 15000 ||
		inv.Items[0].Description != "Plano Equipe: 5 unidade(s) (01/11/2026 a 30/11/2026)" {
		t.Fatalf("o novo período é cobrado pela nova quantidade: %+v", inv.Items)
	}
	if got := f.store.sub(sub.ID); got.Quantity != 5 || got.PriceVersionID != f.seats.price.ID {
		t.Fatalf("quantidade aplicada: %+v", got)
	}
	if _, ok := findAudit(f.store.audits, "subscription.quantity_changed"); !ok {
		t.Fatalf("auditoria da troca de quantidade: %v", auditActions(f.store.audits))
	}
	if _, ok := historyEvent(f.store.subHistory, "quantity_changed"); !ok {
		t.Fatal("histórico da troca de quantidade")
	}
}

func TestEngineKeepsFutureScheduledChange(t *testing.T) {
	f := newTermsFixture(t)
	sub := f.addSub(t, f.basic, 1, true, nil)
	ch := f.schedule(t, sub, &f.pro, nil)
	ch.EffectiveAt = f.period(t, sub, 2).Start

	f.run(t)
	if f.store.invoices[0].Total.Amount() != 10000 || f.store.sub(sub.ID).PlanID != f.basic.plan.ID {
		t.Fatal("mudança futura não altera o período corrente")
	}
	if len(f.store.resolved) != 0 || f.store.changes[sub.ID].Status != subscriptions.ChangePending {
		t.Fatal("a mudança continua pendente")
	}

	f.clock.Set(f.period(t, sub, 2).Start.Add(time.Hour))
	f.run(t)
	if len(f.store.invoices) != 2 || f.store.invoices[1].Total.Amount() != 16000 || f.store.resolved[ch.ID] != subscriptions.ChangeApplied {
		t.Fatalf("a mudança vale no período 2: %+v", f.store.invoices)
	}
}

func TestEngineAppliesDueAdjustmentsInOrder(t *testing.T) {
	f := newTermsFixture(t)
	sub := f.addSub(t, f.basic, 1, true, nil)
	first := f.adjustment(sub, calendar.Date{Year: 2026, Month: time.October, Day: 20}, subscriptions.AdjustPercent, 1000)
	second := f.adjustment(sub, calendar.Date{Year: 2026, Month: time.November, Day: 1}, subscriptions.AdjustFixedAmount, 12500)
	future := f.adjustment(sub, calendar.Date{Year: 2026, Month: time.December, Day: 1}, subscriptions.AdjustPercent, 500)

	f.run(t)
	inv := f.store.invoices[0]
	fee := inv.Items[0]
	if fee.Amount.Amount() != 12500 || fee.UnitAmount.Cmp(big.NewRat(12500, 1)) != 0 {
		t.Fatalf("reajustes aplicados em ordem (10%% e depois R$ 125,00): %+v", fee)
	}
	if fee.PriceSnapshot["price_adjustment_id"] != second.ID || fee.PriceSnapshot["adjusted_fixed_amount"] != int64(12500) ||
		fee.PriceSnapshot["adjusted_amount_decimal"] != "12500" || fee.PriceSnapshot["flat_amount"] != int64(12500) {
		t.Fatalf("snapshot com o reajuste vigente: %+v", fee.PriceSnapshot)
	}
	adjs := f.store.adjustments[sub.ID]
	period1 := f.period(t, sub, 1).Start
	if adjs[0].Status != subscriptions.AdjustmentApplied || !isCents(adjs[0].PreviousAmount, 10000) || !isCents(adjs[0].ResultingAmount, 11000) ||
		!adjs[0].AppliedPeriodStart.Equal(period1) || adjs[0].ID != first.ID {
		t.Fatalf("primeiro reajuste: %+v", adjs[0])
	}
	if adjs[1].Status != subscriptions.AdjustmentApplied || !isCents(adjs[1].PreviousAmount, 11000) || !isCents(adjs[1].ResultingAmount, 12500) {
		t.Fatalf("segundo reajuste parte do valor já reajustado: %+v", adjs[1])
	}
	if adjs[2].Status != subscriptions.AdjustmentScheduled || adjs[2].ID != future.ID {
		t.Fatal("reajuste futuro continua programado")
	}
	var adjusted []any
	for _, a := range f.store.audits {
		if a.Action == "subscription.price_adjusted" {
			adjusted = append(adjusted, a.Details["adjustment_id"])
			if a.Actor != audit.System("billing.engine") {
				t.Fatalf("reajuste auditado pelo motor: %+v", a.Actor)
			}
		}
	}
	if len(adjusted) != 2 || adjusted[0] != first.ID || adjusted[1] != second.ID {
		t.Fatalf("auditoria dos reajustes em ordem: %v", adjusted)
	}
	got := f.store.sub(sub.ID)
	if !isCents(got.PriceOverride, 12500) || *got.PriceOverrideAdjustmentID != second.ID {
		t.Fatalf("valor reajustado gravado na assinatura: %+v", got.PriceOverride)
	}

	// No período seguinte, o reajuste programado parte do valor reajustado.
	f.clock.Set(local(2026, time.December, 1, 10, 0))
	f.run(t)
	if len(f.store.invoices) != 2 || f.store.invoices[1].Items[0].Amount.Amount() != 13125 {
		t.Fatalf("5%% sobre R$ 125,00 = R$ 131,25: %+v", f.store.invoices[1].Items)
	}
}

func TestEnginePlanChangeDropsPreviousAdjustment(t *testing.T) {
	f := newTermsFixture(t)
	adjID := uuid.New()
	sub := f.addSub(t, f.basic, 1, true, func(s *subscriptions.Subscription) {
		s.PriceOverride, s.PriceOverrideAdjustmentID = big.NewRat(12000, 1), &adjID
	})
	f.schedule(t, sub, &f.pro, nil)

	f.run(t)
	fee := f.store.invoices[0].Items[0]
	if fee.Amount.Amount() != 16000 {
		t.Fatalf("o novo plano é cobrado pelo preço contratado: %v", fee.Amount)
	}
	if _, ok := fee.PriceSnapshot["price_adjustment_id"]; ok {
		t.Fatal("o snapshot não referencia o reajuste do plano anterior")
	}
	if got := f.store.sub(sub.ID); got.PriceOverride != nil || got.PriceOverrideAdjustmentID != nil {
		t.Fatal("o reajuste anterior deixa de valer com a nova versão de preço")
	}
}

func TestEngineEndsDiscountNotApplicableToNewPlan(t *testing.T) {
	cases := []struct {
		name         string
		planIDs      func(f *termsFixture) []uuid.UUID
		wantEnded    bool
		wantDiscount int64
	}{
		{"cupom restrito ao plano anterior é encerrado", func(f *termsFixture) []uuid.UUID { return []uuid.UUID{f.basic.plan.ID} }, true, 0},
		{"cupom válido para o novo plano é mantido", func(f *termsFixture) []uuid.UUID { return []uuid.UUID{f.basic.plan.ID, f.pro.plan.ID} },
			false, 1600},
		{"cupom sem restrição de plano é mantido", func(*termsFixture) []uuid.UUID { return nil }, false, 1600},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTermsFixture(t)
			sub := f.addSub(t, f.basic, 1, true, nil)
			discount := &Discount{ID: uuid.New(), CouponCode: "DEZ", PercentBP: 1000, AppliesToPlanIDs: c.planIDs(f)}
			f.store.discount = discount
			f.schedule(t, sub, &f.pro, nil)

			f.run(t)
			if c.wantEnded != (len(f.store.discountEnded) == 1) {
				t.Fatalf("encerramento do desconto: %v", f.store.discountEnded)
			}
			inv := f.store.invoices[0]
			if inv.DiscountTotal.Amount() != c.wantDiscount || inv.Total.Amount() != 16000-c.wantDiscount {
				t.Fatalf("desconto=%v total=%v", inv.DiscountTotal, inv.Total)
			}
			ev, removed := historyEvent(f.store.subHistory, "discount_removed")
			if removed != c.wantEnded {
				t.Fatalf("histórico de remoção do desconto: %v", removed)
			}
			if removed && (ev.Details["discount_id"] != discount.ID || ev.Details["reason"] != "coupon_not_applicable_to_plan") {
				t.Fatalf("detalhes da remoção: %+v", ev.Details)
			}
			if !c.wantEnded && f.store.discountUses[discount.ID] != 1 {
				t.Fatal("o desconto mantido consome um ciclo")
			}
		})
	}
}

func TestEngineCancelsPendingTermsWhenEnding(t *testing.T) {
	t.Run("encerramento no fim do período", func(t *testing.T) {
		f := newTermsFixture(t)
		sub := f.addSub(t, f.basic, 1, true, func(s *subscriptions.Subscription) { s.CancelAtPeriodEnd = true })
		f.schedule(t, sub, &f.pro, nil)
		f.adjustment(sub, calendar.Date{Year: 2026, Month: time.November, Day: 1}, subscriptions.AdjustPercent, 1000)

		f.run(t)
		if len(f.store.termsCanceled) != 1 || f.store.termsCanceled[0] != sub.ID {
			t.Fatalf("condições programadas canceladas no encerramento: %v", f.store.termsCanceled)
		}
		got := f.store.sub(sub.ID)
		if got.Status != subscriptions.Canceled || got.PlanID != f.basic.plan.ID || got.PriceOverride != nil {
			t.Fatalf("encerrada sem aplicar mudança nem reajuste: %+v", got)
		}
		if len(f.store.resolved) != 0 || f.store.calls["PendingChange"] != 0 || len(f.store.invoices) != 0 {
			t.Fatal("o encerramento não aplica condições nem fatura componente fixo")
		}
	})
	t.Run("consumo final após cancelamento imediato", func(t *testing.T) {
		f := newTermsFixture(t)
		at := local(2026, time.October, 20, 12, 0)
		sub := f.addSub(t, f.metered, 1, true, func(s *subscriptions.Subscription) {
			if err := s.Cancel(subscriptions.CancelImmediately, "", true, false, at); err != nil {
				t.Fatal(err)
			}
			s.PopEvents()
		})
		f.store.usage = func(subscriptions.Subscription, calendar.Period) []UsageLine {
			return []UsageLine{{MetricName: "Requisições", Unit: "requisicao", Quantity: big.NewRat(100, 1)}}
		}

		f.run(t)
		if len(f.store.termsCanceled) != 1 || f.store.termsCanceled[0] != sub.ID {
			t.Fatalf("condições canceladas no consumo final: %v", f.store.termsCanceled)
		}
		if len(f.store.invoices) != 1 || f.store.invoices[0].Kind != KindFinalUsage || f.store.invoices[0].Total.Amount() != 200 {
			t.Fatalf("fatura de consumo final: %+v", f.store.invoices)
		}
	})
}

func TestEngineRollsBackScheduledTermsOnFailure(t *testing.T) {
	type scenario int
	const (
		// change: mudança de plano agendada (que cancela o reajuste programado).
		change scenario = iota
		// adjust: reajuste vencido sem mudança de plano.
		adjust
		// unapplicable: reajuste vencido em preço somente por consumo.
		unapplicable
	)
	cases := []struct {
		name     string
		scenario scenario
		method   string
		nth      int
	}{
		{"mudança pendente", change, "PendingChange", 0},
		{"novo plano", change, "Plan", 2},
		{"novo preço", change, "PriceVersion", 2},
		{"resolução da mudança", change, "ResolveChange", 0},
		{"cancelamento dos reajustes programados", change, "CancelScheduledAdjustments", 0},
		{"desconto vigente", change, "ActiveDiscount", 1},
		{"encerramento do desconto", change, "EndActiveDiscount", 0},
		{"auditoria da mudança", change, "Audit", 1},
		{"reajustes vencidos", change, "DueAdjustments", 0},
		{"releitura do plano", change, "Plan", 3},
		{"releitura do preço", change, "PriceVersion", 3},
		{"preço para reajuste", adjust, "PriceVersion", 2},
		{"marcação do reajuste", adjust, "MarkAdjustmentApplied", 0},
		{"auditoria do reajuste", adjust, "Audit", 1},
		{"cancelamento do reajuste inaplicável", unapplicable, "CancelAdjustment", 0},
		{"auditoria do reajuste cancelado", unapplicable, "Audit", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTermsFixture(t)
			o := f.basic
			if c.scenario == unapplicable {
				o = f.metered
			}
			sub := f.addSub(t, o, 1, true, nil)
			discount := &Discount{ID: uuid.New(), PercentBP: 1000, AppliesToPlanIDs: []uuid.UUID{f.basic.plan.ID}}
			f.store.discount = discount
			if c.scenario == change {
				f.schedule(t, sub, &f.pro, nil)
			}
			f.adjustment(sub, calendar.Date{Year: 2026, Month: time.November, Day: 1}, subscriptions.AdjustPercent, 1000)
			boom := errors.New("falha em " + c.method)
			f.store.errs[c.method] = boom
			if c.nth > 0 {
				f.store.failAt[c.method] = c.nth
			}

			_, err := f.engine.RunTenant(context.Background(), f.tenantID, f.actor)
			if !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			assertTermsUntouched(t, f, sub, discount)
		})
	}
	t.Run("reajuste inválido", func(t *testing.T) {
		f := newTermsFixture(t)
		sub := f.addSub(t, f.basic, 1, true, nil)
		f.adjustment(sub, calendar.Date{Year: 2026, Month: time.November, Day: 1}, "indice_desconhecido", 0)
		_, err := f.engine.RunTenant(context.Background(), f.tenantID, f.actor)
		wantAppErr(t, err, apperr.KindInvalid, "invalid_adjustment")
		assertTermsUntouched(t, f, sub, nil)
	})
	t.Run("cancelamento das condições no encerramento", func(t *testing.T) {
		f := newTermsFixture(t)
		sub := f.addSub(t, f.basic, 1, true, func(s *subscriptions.Subscription) { s.CancelAtPeriodEnd = true })
		boom := errors.New("falha ao cancelar condições")
		f.store.errs["CancelPendingTerms"] = boom
		if _, err := f.engine.RunTenant(context.Background(), f.tenantID, f.actor); !errors.Is(err, boom) {
			t.Fatalf("esperava %v, obtive %v", boom, err)
		}
		if got := f.store.sub(sub.ID); got.Status != subscriptions.Active || len(f.store.termsCanceled) != 0 {
			t.Fatalf("o encerramento é desfeito: %+v", got)
		}
	})
}

func assertTermsUntouched(t *testing.T, f *termsFixture, sub subscriptions.Subscription, discount *Discount) {
	t.Helper()
	got := f.store.sub(sub.ID)
	if got.PlanID != sub.PlanID || got.PriceVersionID != sub.PriceVersionID || got.NextBillingIndex != sub.NextBillingIndex || got.PriceOverride != nil {
		t.Fatalf("assinatura alterada: %+v", got)
	}
	if len(f.store.invoices) != 0 || len(f.store.audits) != 0 || len(f.store.events) != 0 || len(f.store.posted) != 0 || len(f.store.subHistory) != 0 {
		t.Fatal("nenhuma fatura, auditoria, evento ou histórico pode restar")
	}
	if ch := f.store.changes[sub.ID]; ch != nil && ch.Status != subscriptions.ChangePending {
		t.Fatalf("a mudança continua pendente: %s", ch.Status)
	}
	for _, a := range f.store.adjustments[sub.ID] {
		if a.Status != subscriptions.AdjustmentScheduled {
			t.Fatalf("o reajuste continua programado: %s", a.Status)
		}
	}
	if len(f.store.resolved) != 0 || len(f.store.discountEnded) != 0 || f.store.discount != discount {
		t.Fatal("o desconto e a resolução da mudança devem ser restaurados")
	}
}

func TestScheduledChangeFromServiceAppliedByEngine(t *testing.T) {
	f := newChangeFixture(t)
	sub := f.addSub(t, f.basic, 1, true, nil)
	ch := f.change(t, sub.ID, nextCycle(f.pro.plan.ID)).Scheduled

	f.clock.Set(local(2026, time.December, 1, 10, 0))
	engine := NewEngine(f.store, f.clock)
	res, err := engine.RunTenant(context.Background(), f.tenantID, audit.System("teste"))
	if err != nil || res.Invoices != 1 {
		t.Fatalf("RunTenant: %+v %v", res, err)
	}
	inv := f.store.invoices[0]
	if inv.Total.Amount() != 16000 || *inv.Items[0].PlanID != f.pro.plan.ID || !inv.BillingPeriod.Start.Equal(local(2026, time.December, 1, 0, 0)) ||
		!inv.BillingPeriod.End.Equal(local(2027, time.January, 1, 0, 0)) {
		t.Fatalf("o ciclo de dezembro é faturado no novo plano: %+v", inv)
	}
	if f.store.resolved[ch.ID] != subscriptions.ChangeApplied || f.store.sub(sub.ID).PlanID != f.pro.plan.ID {
		t.Fatal("a mudança agendada pelo serviço é aplicada pelo motor")
	}
}

func TestDiscountAppliesTo(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	cases := []struct {
		name  string
		plans []uuid.UUID
		plan  uuid.UUID
		want  bool
	}{
		{"sem restrição", nil, a, true},
		{"plano elegível", []uuid.UUID{b, a}, a, true},
		{"plano não elegível", []uuid.UUID{b}, a, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (Discount{AppliesToPlanIDs: c.plans}).AppliesTo(c.plan); got != c.want {
				t.Fatalf("AppliesTo = %v, esperado %v", got, c.want)
			}
		})
	}
}

func TestBuildCycleInvoiceSkipsUsageWithoutMeteredPrice(t *testing.T) {
	c := newCycleCase(t, catalog.Flat, 1)
	flatPlan, flatPrice := c.plan, c.price
	metered := newCycleCase(t, catalog.Usage, 1)
	cases := []struct {
		name      string
		line      func() UsageLine
		wantItems int
	}{
		{"preço do período sem consumo", func() UsageLine { return usageLine(500, nil) }, 1},
		{"versão anterior sem consumo", func() UsageLine {
			u := usageLine(500, nil)
			u.Plan, u.Price = &flatPlan, &flatPrice
			return u
		}, 1},
		{"versão anterior com consumo", func() UsageLine {
			u := usageLine(500, nil)
			u.Plan, u.Price = &metered.plan, &metered.price
			return u
		}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv, _, err := BuildCycleInvoice(c.sub, c.plan, c.price, c.billStep(1), customers.Snapshot{Name: "Padaria Central"},
				[]UsageLine{tc.line()}, nil, nil, saoPaulo, engineNow)
			if err != nil {
				t.Fatal(err)
			}
			if len(inv.Items) != tc.wantItems {
				t.Fatalf("itens: %+v", inv.Items)
			}
			if tc.wantItems == 2 && (*inv.Items[1].PriceVersionID != metered.price.ID || inv.Items[1].PriceSnapshot["pricing_model"] != catalog.Usage) {
				t.Fatalf("consumo precificado pela versão da linha: %+v", inv.Items[1])
			}
		})
	}
}
