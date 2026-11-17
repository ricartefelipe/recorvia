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
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// Período 0 das assinaturas de teste: 01/11/2026 a 30/11/2026 (30 dias).
// A mudança ocorre em 21/11/2026 às 15h: restam 10 de 30 dias (fração 1/3).
var (
	changeStart = local(2026, time.November, 1, 0, 0)
	changeNow   = local(2026, time.November, 21, 15, 0)
)

// offer é um plano com a versão de preço vigente.
type offer struct {
	plan  catalog.Plan
	price catalog.PriceVersion
}

func newOffer(code, name string, interval calendar.Interval, model catalog.PricingModel, amount int64) offer {
	priceID := uuid.New()
	plan := catalog.Plan{ID: uuid.New(), Code: code, Name: name, Interval: interval, Currency: money.BRL, Status: catalog.Active,
		CurrentPriceVersionID: &priceID, Version: 1}
	pv := catalog.PriceVersion{ID: priceID, PlanID: plan.ID, Version: 1, Model: model, Currency: money.BRL}
	switch model {
	case catalog.Flat:
		m := brl(amount)
		pv.FlatAmount = &m
	case catalog.PerUnit:
		pv.UnitAmount = big.NewRat(amount, 1)
	case catalog.Usage:
		metric := uuid.New()
		pv.UnitAmount = big.NewRat(amount, 1)
		pv.MetricID = &metric
	}
	return offer{plan: plan, price: pv}
}

type changeFixture struct {
	store    *advancedFake
	clock    *clock.Manual
	svc      *ChangeService
	tenantID uuid.UUID
	userID   uuid.UUID
	actor    audit.Actor
	basic    offer // R$ 100,00
	pro      offer // R$ 160,00
	free     offer // R$ 0,00
	seats    offer // R$ 30,00 por unidade
	metered  offer // R$ 0,02 por unidade consumida
	yearly   offer
}

func newChangeFixture(t *testing.T) *changeFixture {
	t.Helper()
	f := &changeFixture{store: newAdvancedFake(), clock: clock.NewManual(changeNow), tenantID: uuid.New(), userID: uuid.New()}
	f.actor = audit.User(f.userID, "Ana Souza")
	f.basic = f.addOffer(newOffer("basico", "Plano Básico", calendar.Monthly, catalog.Flat, 10000))
	f.pro = f.addOffer(newOffer("pro", "Plano Pro", calendar.Monthly, catalog.Flat, 16000))
	f.free = f.addOffer(newOffer("gratis", "Plano Gratuito", calendar.Monthly, catalog.Flat, 0))
	f.seats = f.addOffer(newOffer("equipe", "Plano Equipe", calendar.Monthly, catalog.PerUnit, 3000))
	f.metered = f.addOffer(newOffer("api", "Plano API", calendar.Monthly, catalog.Usage, 2))
	f.yearly = f.addOffer(newOffer("anual", "Plano Anual", calendar.Yearly, catalog.Flat, 100000))
	f.svc = NewChangeService(f.store, f.clock)
	return f
}

func (f *changeFixture) addOffer(o offer) offer {
	f.store.plans[o.plan.ID] = o.plan
	f.store.prices[o.price.ID] = o.price
	return o
}

// addSub cria uma assinatura iniciada em 01/11/2026; billed indica se o
// período 0 já foi faturado (cobrança antecipada).
func (f *changeFixture) addSub(t *testing.T, o offer, qty int, billed bool, mutate func(*subscriptions.NewInput)) subscriptions.Subscription {
	t.Helper()
	in := subscriptions.NewInput{ID: uuid.New(), TenantID: f.tenantID, CustomerID: uuid.New(), Plan: o.plan, Price: o.price, Quantity: qty,
		StartAt: changeStart, Location: saoPaulo, Now: changeStart}
	if mutate != nil {
		mutate(&in)
	}
	sub, err := subscriptions.New(in)
	if err != nil {
		t.Fatalf("subscriptions.New: %v", err)
	}
	if billed {
		sched, err := sub.Schedule()
		if err != nil {
			t.Fatal(err)
		}
		if err := sub.ApplyStep(subscriptions.BillingStep{Kind: subscriptions.StepBill, Index: 0, Period: sched.Period(0)}, changeStart); err != nil {
			t.Fatal(err)
		}
	}
	sub.PopEvents()
	f.store.subs = append(f.store.subs, sub)
	return sub
}

func (f *changeFixture) change(t *testing.T, subID uuid.UUID, in ChangeInput) ChangeResult {
	t.Helper()
	res, err := f.svc.Change(context.Background(), f.tenantID, f.actor, &f.userID, subID, in, 0)
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	return res
}

func immediate(planID uuid.UUID) ChangeInput {
	return ChangeInput{PlanID: &planID, Timing: subscriptions.ChangeImmediately}
}

func nextCycle(planID uuid.UUID) ChangeInput {
	return ChangeInput{PlanID: &planID, Timing: subscriptions.ChangeNextCycle}
}

// assertNoChangeEffects verifica que nada foi gravado pela mudança.
func (f *changeFixture) assertNoChangeEffects(t *testing.T, before subscriptions.Subscription) {
	t.Helper()
	s := f.store
	if len(s.invoices) != 0 || len(s.posted) != 0 || len(s.events) != 0 || len(s.audits) != 0 || len(s.grants) != 0 ||
		len(s.scheduled) != 0 || len(s.subHistory) != 0 || len(s.creditUses) != 0 || s.seq != 0 || s.subUpdates != 0 {
		t.Fatalf("nenhum efeito deveria ser gravado: faturas=%d lançamentos=%d eventos=%d auditorias=%d créditos=%d agendamentos=%d",
			len(s.invoices), len(s.posted), len(s.events), len(s.audits), len(s.grants), len(s.scheduled))
	}
	got := s.sub(before.ID)
	if got.PlanID != before.PlanID || got.PriceVersionID != before.PriceVersionID || got.Quantity != before.Quantity || got.Version != before.Version {
		t.Fatalf("assinatura alterada: plano=%s quantidade=%d versão=%d", got.PlanID, got.Quantity, got.Version)
	}
}

// samePeriod compara instantes, independentemente da representação do fuso.
func samePeriod(a, b calendar.Period) bool { return a.Start.Equal(b.Start) && a.End.Equal(b.End) }

func historyEvents(w []subHistoryWrite) []string {
	var out []string
	for _, h := range w {
		for _, e := range h.events {
			out = append(out, e.Event)
		}
	}
	return out
}

func auditActions(entries []audit.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Action
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestChangeNextCycleSchedulesPlanChange(t *testing.T) {
	f := newChangeFixture(t)
	sub := f.addSub(t, f.basic, 1, true, nil)

	res := f.change(t, sub.ID, nextCycle(f.pro.plan.ID))
	ch := res.Scheduled
	if ch == nil || res.Proration != nil || res.InvoiceID != nil || res.CreditGranted != nil {
		t.Fatalf("agendamento não tem prorrata nem fatura: %+v", res)
	}
	periodEnd := local(2026, time.December, 1, 0, 0)
	if ch.Kind != subscriptions.PlanChange || ch.Status != subscriptions.ChangePending || !ch.EffectiveAt.Equal(periodEnd) ||
		ch.TenantID != f.tenantID || ch.SubscriptionID != sub.ID || *ch.CreatedBy != f.userID || !ch.CreatedAt.Equal(changeNow) {
		t.Fatalf("mudança agendada para o fim do período corrente: %+v", ch)
	}
	if *ch.NewPlanID != f.pro.plan.ID || *ch.NewPriceVersionID != f.pro.price.ID || ch.NewQuantity != nil {
		t.Fatalf("a versão de preço fica fixada no agendamento e a quantidade não muda: %+v", ch)
	}
	if len(f.store.scheduled) != 1 || f.store.scheduled[0].ID != ch.ID {
		t.Fatalf("mudança gravada: %+v", f.store.scheduled)
	}
	if len(f.store.canceledChanges) != 1 || f.store.canceledChanges[0] != sub.ID {
		t.Fatal("o agendamento substitui mudanças pendentes da assinatura")
	}
	got := f.store.sub(sub.ID)
	if got.PlanID != f.basic.plan.ID || got.PriceVersionID != f.basic.price.ID || res.Subscription.PlanID != f.basic.plan.ID {
		t.Fatal("o plano só muda no início do próximo ciclo")
	}
	if res.Subscription.Version != sub.Version+1 || f.store.subUpdates != 1 {
		t.Fatalf("a assinatura é gravada com nova versão: %d", res.Subscription.Version)
	}
	if ev := historyEvents(f.store.subHistory); !equalStrings(ev, []string{"change_scheduled"}) {
		t.Fatalf("histórico: %v", ev)
	}
	if hist := f.store.subHistory[0]; hist.actor != f.actor || hist.events[0].Details["scheduled_change_id"] != ch.ID {
		t.Fatalf("histórico com ator e mudança: %+v", hist)
	}
	if len(f.store.events) != 1 || f.store.events[0].Type != outbox.EventSubscriptionUpdated ||
		f.store.events[0].Payload.(map[string]any)["change"] != "subscription.change_scheduled" {
		t.Fatalf("evento de assinatura: %+v", f.store.events)
	}
	if len(f.store.audits) != 1 {
		t.Fatalf("auditoria: %+v", f.store.audits)
	}
	a := f.store.audits[0]
	if a.Action != "subscription.change_scheduled" || a.Actor != f.actor || a.Details["scheduled_change_id"] != ch.ID ||
		a.Details["plan_id"] != f.pro.plan.ID || a.Details["timing"] != subscriptions.ChangeNextCycle {
		t.Fatalf("auditoria do agendamento: %+v", a)
	}
	if len(f.store.invoices) != 0 || len(f.store.posted) != 0 || len(f.store.grants) != 0 {
		t.Fatal("agendamento não emite fatura nem concede crédito")
	}
}

func TestChangeNextCycleQuantity(t *testing.T) {
	t.Run("troca de quantidade mantém o plano", func(t *testing.T) {
		f := newChangeFixture(t)
		sub := f.addSub(t, f.seats, 2, true, nil)
		res := f.change(t, sub.ID, ChangeInput{Quantity: intPtr(5), Timing: subscriptions.ChangeNextCycle})
		ch := res.Scheduled
		if ch == nil || ch.Kind != subscriptions.QuantityChange || ch.NewQuantity == nil || *ch.NewQuantity != 5 ||
			ch.NewPlanID != nil || ch.NewPriceVersionID != nil {
			t.Fatalf("mudança de quantidade agendada: %+v", ch)
		}
		if f.store.audits[0].Details["quantity"] != 5 || f.store.sub(sub.ID).Quantity != 2 {
			t.Fatalf("a quantidade só muda no próximo ciclo: %+v", f.store.audits[0].Details)
		}
	})
	t.Run("plano por unidade sem quantidade informada mantém a quantidade", func(t *testing.T) {
		f := newChangeFixture(t)
		team := f.addOffer(newOffer("equipe-plus", "Plano Equipe Plus", calendar.Monthly, catalog.PerUnit, 4000))
		sub := f.addSub(t, f.seats, 3, true, nil)
		res := f.change(t, sub.ID, nextCycle(team.plan.ID))
		ch := res.Scheduled
		if ch == nil || ch.Kind != subscriptions.PlanChange || ch.NewQuantity != nil || *ch.NewPlanID != team.plan.ID {
			t.Fatalf("quantidade omitida é mantida no plano por unidade: %+v", ch)
		}
		if f.store.audits[0].Details["quantity"] != 3 {
			t.Fatalf("auditoria com a quantidade mantida: %+v", f.store.audits[0].Details)
		}
	})
	t.Run("plano por unidade para plano fixo agenda quantidade 1", func(t *testing.T) {
		f := newChangeFixture(t)
		sub := f.addSub(t, f.seats, 4, true, nil)
		res := f.change(t, sub.ID, nextCycle(f.pro.plan.ID))
		ch := res.Scheduled
		if ch == nil || ch.Kind != subscriptions.PlanChange || ch.NewQuantity == nil || *ch.NewQuantity != 1 {
			t.Fatalf("troca para plano sem quantidade agenda quantidade 1: %+v", ch)
		}
	})
}

func TestChangeNextCycleReplacesPendingChange(t *testing.T) {
	f := newChangeFixture(t)
	sub := f.addSub(t, f.basic, 1, true, nil)
	first := f.change(t, sub.ID, nextCycle(f.pro.plan.ID)).Scheduled
	f.clock.Advance(time.Hour)
	second := f.change(t, sub.ID, nextCycle(f.free.plan.ID)).Scheduled

	if len(f.store.canceledChanges) != 2 || len(f.store.scheduled) != 2 {
		t.Fatalf("cada agendamento cancela os pendentes antes de gravar: %v", f.store.canceledChanges)
	}
	if f.store.scheduled[0].ID != first.ID || f.store.scheduled[1].ID != second.ID {
		t.Fatal("as duas mudanças ficam registradas")
	}
	pending, err := f.store.PendingChange(context.Background(), sub.ID)
	if err != nil || pending == nil || pending.ID != second.ID || *pending.NewPlanID != f.free.plan.ID {
		t.Fatalf("somente a última mudança fica pendente: %+v %v", pending, err)
	}
}

func TestChangeNextCycleToMeteredPlanRequiresPlatformFeature(t *testing.T) {
	t.Run("com recurso", func(t *testing.T) {
		f := newChangeFixture(t)
		sub := f.addSub(t, f.basic, 1, true, nil)
		res := f.change(t, sub.ID, nextCycle(f.metered.plan.ID))
		if res.Scheduled == nil || *res.Scheduled.NewPriceVersionID != f.metered.price.ID {
			t.Fatalf("agendamento para plano com consumo: %+v", res.Scheduled)
		}
		if f.store.calls["PlatformPlan"] != 1 {
			t.Fatal("o plano da plataforma deve ser consultado para preços com consumo")
		}
	})
	t.Run("sem recurso", func(t *testing.T) {
		f := newChangeFixture(t)
		f.store.platformPlan.Features = nil
		sub := f.addSub(t, f.basic, 1, true, nil)
		_, err := f.svc.Change(context.Background(), f.tenantID, f.actor, &f.userID, sub.ID, nextCycle(f.metered.plan.ID), 0)
		wantAppErr(t, err, apperr.KindForbidden, "platform_feature_unavailable")
		f.assertNoChangeEffects(t, sub)
	})
	t.Run("plano fixo não consulta a plataforma", func(t *testing.T) {
		f := newChangeFixture(t)
		sub := f.addSub(t, f.basic, 1, true, nil)
		f.change(t, sub.ID, nextCycle(f.pro.plan.ID))
		if f.store.calls["PlatformPlan"] != 0 {
			t.Fatal("preço sem consumo não depende do plano da plataforma")
		}
	})
}

func TestChangeImmediateUpgradeIssuesProrationInvoice(t *testing.T) {
	f := newChangeFixture(t)
	sub := f.addSub(t, f.basic, 1, true, nil)

	res := f.change(t, sub.ID, immediate(f.pro.plan.ID))
	pr := res.Proration
	if pr == nil || pr.Fraction.Cmp(big.NewRat(1, 3)) != 0 || pr.RemainingDays != 10 || pr.PeriodDays != 30 {
		t.Fatalf("prorrata de 10 de 30 dias: %+v", pr)
	}
	if pr.Credit.Amount() != 3333 || pr.Charge.Amount() != 5333 || pr.Net.Amount() != 2000 {
		t.Fatalf("crédito R$ 33,33, cobrança R$ 53,33 e diferença R$ 20,00: %v %v %v", pr.Credit, pr.Charge, pr.Net)
	}
	if res.InvoiceID == nil || res.CreditGranted != nil || len(f.store.invoices) != 1 {
		t.Fatalf("diferença positiva emite fatura de prorrata: %+v", res)
	}
	inv := f.store.invoices[0]
	wantPeriod := calendar.Period{Start: local(2026, time.November, 21, 0, 0), End: local(2026, time.December, 1, 0, 0)}
	if inv.ID != *res.InvoiceID || inv.Kind != KindProration || inv.Status != Open || inv.Number != "FAT-2026-000001" ||
		!samePeriod(*inv.BillingPeriod, wantPeriod) || *inv.SubscriptionID != sub.ID || inv.CustomerID != sub.CustomerID {
		t.Fatalf("cabeçalho da fatura de prorrata: %+v", inv)
	}
	if len(inv.Items) != 2 {
		t.Fatalf("itens de crédito e cobrança proporcionais: %+v", inv.Items)
	}
	// A cobrança entra antes do crédito: o total parcial do rascunho nunca fica negativo.
	charge, credit := inv.Items[0], inv.Items[1]
	if credit.Kind != ItemProration || credit.Amount.Amount() != -3333 || *credit.PlanID != f.basic.plan.ID ||
		credit.Description != "Crédito proporcional de Plano Básico (21/11/2026 a 30/11/2026, 10 de 30 dias)" {
		t.Fatalf("item de crédito: %+v", credit)
	}
	if charge.Kind != ItemProration || charge.Amount.Amount() != 5333 || *charge.PlanID != f.pro.plan.ID ||
		charge.Description != "Cobrança proporcional de Plano Pro (21/11/2026 a 30/11/2026, 10 de 30 dias)" {
		t.Fatalf("item de cobrança: %+v", charge)
	}
	if credit.UnitAmount.Cmp(big.NewRat(-10000, 1)) != 0 || charge.UnitAmount.Cmp(big.NewRat(16000, 1)) != 0 ||
		credit.Quantity.Cmp(big.NewRat(1, 3)) != 0 || charge.Quantity.Cmp(big.NewRat(1, 3)) != 0 {
		t.Fatalf("fração e valores unitários dos itens: %+v | %+v", credit, charge)
	}
	if inv.Total.Amount() != 2000 || inv.AmountDue.Amount() != 2000 || !inv.DiscountTotal.IsZero() || !inv.DueAt.Equal(DueAt(changeNow, 5, saoPaulo)) {
		t.Fatalf("total e vencimento: %v %v", inv.Total, inv.DueAt)
	}
	if net := ledgerNet(t, f.store.posted); net[ledger.AccountsReceivable] != 2000 || net[ledger.BilledRevenue] != -2000 {
		t.Fatalf("ledger da prorrata: %+v", net)
	}
	got := f.store.sub(sub.ID)
	if got.PlanID != f.pro.plan.ID || got.PriceVersionID != f.pro.price.ID {
		t.Fatal("mudança imediata troca o plano")
	}
	if acts := auditActions(f.store.audits); !equalStrings(acts, []string{"invoice.issued", "subscription.plan_changed"}) {
		t.Fatalf("auditorias: %v", acts)
	}
}

func TestChangeImmediateUpgradeWithoutPreviousFixedCharge(t *testing.T) {
	percent := &Discount{ID: uuid.New(), CouponCode: "DEZ", PercentBP: 1000}
	fixedOff := brl(5000)
	amountOff := &Discount{ID: uuid.New(), CouponCode: "CINQUENTA", AmountOff: &fixedOff}
	cases := []struct {
		name         string
		discount     *Discount
		credit       int64
		customerDays *int
		subDays      *int
		wantCharge   int64
		wantTotal    int64
		wantDue      int64
		wantTermDays int
	}{
		{"sem desconto", nil, 0, nil, nil, 5333, 5333, 5333, 5},
		// 16.000 × 1/3 × 0,9 = 4.800, arredondado uma única vez.
		{"cupom percentual reduz a cobrança proporcional", percent, 0, nil, nil, 4800, 4800, 4800, 5},
		{"cupom de valor fixo não é prorrateado", amountOff, 0, nil, nil, 5333, 5333, 5333, 5},
		{"crédito do cliente é consumido", nil, 1000, nil, nil, 5333, 5333, 4333, 5},
		{"prazo do cliente", nil, 0, intPtr(10), nil, 5333, 5333, 5333, 10},
		{"prazo da assinatura prevalece", nil, 0, intPtr(10), intPtr(15), 5333, 5333, 5333, 15},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newChangeFixture(t)
			f.store.discount = c.discount
			f.store.customerTerms = c.customerDays
			sub := f.addSub(t, f.free, 1, true, func(in *subscriptions.NewInput) { in.PaymentTermsDays = c.subDays })
			if c.credit > 0 {
				f.store.credit[sub.CustomerID] = brl(c.credit)
			}

			res := f.change(t, sub.ID, immediate(f.pro.plan.ID))
			if res.Proration == nil || res.Proration.Net.Amount() != c.wantCharge || !res.Proration.Credit.IsZero() {
				t.Fatalf("prorrata sem crédito do plano anterior: %+v", res.Proration)
			}
			if res.InvoiceID == nil || len(f.store.invoices) != 1 {
				t.Fatalf("fatura de prorrata: %+v", res)
			}
			inv := f.store.invoices[0]
			it := inv.Items[0]
			if len(inv.Items) != 1 || it.Amount.Amount() != c.wantCharge || it.Kind != ItemProration ||
				it.Quantity.Cmp(big.NewRat(1, 3)) != 0 || *it.PriceVersionID != f.pro.price.ID {
				t.Fatalf("somente a cobrança proporcional do novo plano: %+v", inv.Items)
			}
			if exact := new(big.Rat).Mul(it.Quantity, it.UnitAmount); money.RoundRat(exact, money.DefaultRounding).Int64() != c.wantCharge {
				t.Fatalf("quantidade × valor unitário arredonda para o valor do item: %s", exact.RatString())
			}
			if it.PriceSnapshot["proration"] != true || it.PriceSnapshot["remaining_days"] != 10 || it.PriceSnapshot["plan_code"] != "pro" {
				t.Fatalf("snapshot do item de prorrata: %+v", it.PriceSnapshot)
			}
			wantDesc := "Cobrança proporcional de Plano Pro (21/11/2026 a 30/11/2026, 10 de 30 dias)"
			if c.discount == percent {
				wantDesc = "Cobrança proporcional de Plano Pro (21/11/2026 a 30/11/2026, 10 de 30 dias, desconto de 10%)"
				if it.PriceSnapshot["discount_percent_bp"] != int64(1000) || it.PriceSnapshot["coupon_code"] != "DEZ" ||
					it.PriceSnapshot["discount_id"] != percent.ID {
					t.Fatalf("snapshot com o cupom da cobrança: %+v", it.PriceSnapshot)
				}
			} else if _, ok := it.PriceSnapshot["discount_percent_bp"]; ok {
				t.Fatalf("sem cupom percentual, o snapshot não registra desconto: %+v", it.PriceSnapshot)
			}
			if it.Description != wantDesc {
				t.Fatalf("descrição do item: %q", it.Description)
			}
			if !inv.DiscountTotal.IsZero() || inv.Total.Amount() != c.wantTotal || inv.AmountDue.Amount() != c.wantDue {
				t.Fatalf("o percentual já está no item: desconto=%v total=%v saldo=%v", inv.DiscountTotal, inv.Total, inv.AmountDue)
			}
			if !inv.DueAt.Equal(DueAt(changeNow, c.wantTermDays, saoPaulo)) {
				t.Fatalf("vencimento %v, esperado prazo de %d dias", inv.DueAt, c.wantTermDays)
			}
			if c.credit > 0 && (len(f.store.creditUses) != 1 || f.store.creditUses[0].amount.Amount() != c.credit ||
				f.store.credit[sub.CustomerID].Amount() != 0) {
				t.Fatalf("consumo do crédito: %+v", f.store.creditUses)
			}
			if c.discount != nil && f.store.discountUses[c.discount.ID] != 0 {
				t.Fatal("a fatura de prorrata não consome ciclo do desconto")
			}
			if res.Subscription.PlanID != f.pro.plan.ID || f.store.audits[1].Details["proration_net"] != c.wantCharge {
				t.Fatalf("assinatura e auditoria da mudança: %+v", f.store.audits)
			}
		})
	}
}

func TestChangeImmediateDowngradeGrantsCredit(t *testing.T) {
	// Do Pro (R$ 160,00) para o Básico (R$ 100,00) com 10 de 30 dias restantes:
	// crédito de 16.000 × 1/3 × (1 − p_antigo) e cobrança de 10.000 × 1/3 × (1 − p_novo).
	cases := []struct {
		name       string
		discount   func(f *changeFixture) *Discount
		wantCredit int64
		wantEnded  bool
	}{
		{"sem desconto", func(*changeFixture) *Discount { return nil }, 2000, false},
		{"cupom percentual reduz crédito e cobrança", func(*changeFixture) *Discount {
			return &Discount{ID: uuid.New(), CouponCode: "DEZ", PercentBP: 1000}
		}, 1800, false},
		{"cupom de valor fixo não é prorrateado", func(*changeFixture) *Discount {
			off := brl(3000)
			return &Discount{ID: uuid.New(), CouponCode: "TRINTA", AmountOff: &off}
		}, 2000, false},
		{"cupom de 100% nos dois planos não gera diferença", func(*changeFixture) *Discount {
			return &Discount{ID: uuid.New(), CouponCode: "CORTESIA", PercentBP: 10000}
		}, 0, false},
		// O período pago teve 10% de desconto e o novo plano não tem: 4.800 − 3.333.
		{"cupom que não vale para o novo plano é encerrado", func(f *changeFixture) *Discount {
			return &Discount{ID: uuid.New(), CouponCode: "PRO10", PercentBP: 1000, AppliesToPlanIDs: []uuid.UUID{f.pro.plan.ID}}
		}, 1467, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newChangeFixture(t)
			f.store.discount = c.discount(f)
			sub := f.addSub(t, f.pro, 1, true, nil)

			res := f.change(t, sub.ID, immediate(f.basic.plan.ID))
			if res.InvoiceID != nil || len(f.store.invoices) != 0 {
				t.Fatalf("diferença negativa ou nula não emite fatura: %+v", res)
			}
			if (c.wantCredit == 0) != (res.Proration == nil) || (res.Proration != nil && res.Proration.Net.Amount() != -c.wantCredit) {
				t.Fatalf("diferença da prorrata: %+v", res.Proration)
			}
			if got := f.store.sub(sub.ID); got.PlanID != f.basic.plan.ID || got.PriceVersionID != f.basic.price.ID {
				t.Fatal("o plano é trocado imediatamente")
			}
			if c.wantEnded != (len(f.store.discountEnded) == 1) {
				t.Fatalf("encerramento do desconto: %v", f.store.discountEnded)
			}
			events := historyEvents(f.store.subHistory)
			wantEvents := []string{"plan_changed"}
			if c.wantEnded {
				wantEvents = append(wantEvents, "discount_removed")
			}
			if !equalStrings(events, wantEvents) {
				t.Fatalf("histórico: %v", events)
			}
			if c.wantCredit == 0 {
				if res.CreditGranted != nil || len(f.store.grants) != 0 || len(f.store.posted) != 0 {
					t.Fatalf("crédito nulo não é concedido: %+v", f.store.grants)
				}
				if acts := auditActions(f.store.audits); !equalStrings(acts, []string{"subscription.plan_changed"}) {
					t.Fatalf("auditorias: %v", acts)
				}
				return
			}
			if res.CreditGranted == nil || res.CreditGranted.Amount() != c.wantCredit {
				t.Fatalf("crédito concedido: %v", res.CreditGranted)
			}
			if len(f.store.grants) != 1 {
				t.Fatalf("créditos: %+v", f.store.grants)
			}
			g := f.store.grants[0]
			if g.customerID != sub.CustomerID || g.amount.Amount() != c.wantCredit || g.kind != "proration" ||
				g.reason != "Crédito de prorrata: Plano Pro para Plano Básico (21/11/2026 a 30/11/2026, 10 de 30 dias)" {
				t.Fatalf("crédito de prorrata: %+v", g)
			}
			if f.store.credit[sub.CustomerID].Amount() != c.wantCredit {
				t.Fatal("o crédito fica disponível para as próximas faturas")
			}
			if len(f.store.posted) != 1 || f.store.posted[0].IdempotencyKey != "customer_credit.granted:"+g.id.String() {
				t.Fatalf("lançamento do crédito: %+v", f.store.posted)
			}
			if net := ledgerNet(t, f.store.posted); net[ledger.RevenueAdjustments] != c.wantCredit || net[ledger.CustomerCredits] != -c.wantCredit {
				t.Fatalf("ledger do crédito: %+v", net)
			}
			if acts := auditActions(f.store.audits); !equalStrings(acts, []string{"customer_credit.proration", "subscription.plan_changed"}) {
				t.Fatalf("auditorias: %v", acts)
			}
			if d := f.store.audits[0].Details; d["amount"] != c.wantCredit || d["subscription_id"] != sub.ID {
				t.Fatalf("auditoria do crédito: %+v", d)
			}
			if f.store.audits[1].Details["proration_net"] != -c.wantCredit {
				t.Fatalf("auditoria da mudança: %+v", f.store.audits[1].Details)
			}
		})
	}
}

func TestChangeImmediateQuantity(t *testing.T) {
	cases := []struct {
		name       string
		from       int
		override   int64
		to         *int
		plan       func(f *changeFixture) *uuid.UUID
		wantKind   string
		wantQty    int
		wantNet    int64
		wantCredit int64
	}{
		{"redução de quantidade vira crédito", 5, 0, intPtr(2), func(*changeFixture) *uuid.UUID { return nil },
			"subscription.quantity_changed", 2, -3000, 3000},
		{"reajuste vigente é usado nos dois lados", 5, 3300, intPtr(2), func(*changeFixture) *uuid.UUID { return nil },
			"subscription.quantity_changed", 2, -3300, 3300},
		{"troca para plano sem quantidade usa quantidade 1", 5, 0, nil, func(f *changeFixture) *uuid.UUID { return &f.basic.plan.ID },
			"subscription.plan_changed", 1, -1667, 1667},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newChangeFixture(t)
			adjID := uuid.New()
			sub := f.addSub(t, f.seats, c.from, true, nil)
			if c.override > 0 {
				f.store.subs[0].PriceOverride, f.store.subs[0].PriceOverrideAdjustmentID = big.NewRat(c.override, 1), &adjID
			}
			res := f.change(t, sub.ID, ChangeInput{PlanID: c.plan(f), Quantity: c.to, Timing: subscriptions.ChangeImmediately})
			if res.Proration == nil || res.Proration.Net.Amount() != c.wantNet {
				t.Fatalf("diferença da prorrata: %+v", res.Proration)
			}
			if res.CreditGranted == nil || res.CreditGranted.Amount() != c.wantCredit {
				t.Fatalf("crédito: %v", res.CreditGranted)
			}
			got := f.store.sub(sub.ID)
			if got.Quantity != c.wantQty || res.Subscription.Quantity != c.wantQty {
				t.Fatalf("quantidade: %d", got.Quantity)
			}
			if c.override > 0 && !isCents(got.PriceOverride, c.override) {
				t.Fatal("troca de quantidade na mesma versão de preço mantém o reajuste")
			}
			if acts := auditActions(f.store.audits); acts[len(acts)-1] != c.wantKind {
				t.Fatalf("ação auditada: %v", acts)
			}
			if f.store.events[0].Payload.(map[string]any)["change"] != c.wantKind {
				t.Fatalf("evento: %+v", f.store.events[0].Payload)
			}
		})
	}
}

func TestChangeImmediateWithoutProration(t *testing.T) {
	basic2 := newOffer("basico2", "Plano Básico 2", calendar.Monthly, catalog.Flat, 10000)
	cases := []struct {
		name   string
		billed bool
		trial  int
		target func(f *changeFixture) uuid.UUID
		wantPr bool
	}{
		{"em avaliação", false, 30, func(f *changeFixture) uuid.UUID { return f.pro.plan.ID }, false},
		{"período corrente ainda não faturado", false, 0, func(f *changeFixture) uuid.UUID { return f.pro.plan.ID }, false},
		{"mesmo valor não gera diferença", true, 0, func(f *changeFixture) uuid.UUID { return basic2.plan.ID }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newChangeFixture(t)
			f.addOffer(basic2)
			sub := f.addSub(t, f.basic, 1, c.billed, func(in *subscriptions.NewInput) { in.TrialDays = c.trial })
			target := c.target(f)
			res := f.change(t, sub.ID, immediate(target))
			if res.Proration != nil || res.InvoiceID != nil || res.CreditGranted != nil || res.Scheduled != nil {
				t.Fatalf("sem prorrata: %+v", res)
			}
			if len(f.store.invoices) != 0 || len(f.store.grants) != 0 || len(f.store.posted) != 0 {
				t.Fatal("nenhuma fatura ou crédito")
			}
			if got := f.store.sub(sub.ID); got.PlanID != target || got.Status != sub.Status {
				t.Fatalf("plano trocado sem alterar a situação: %+v", got)
			}
			if _, ok := f.store.audits[0].Details["proration_net"]; ok || f.store.audits[0].Action != "subscription.plan_changed" {
				t.Fatalf("auditoria sem prorrata: %+v", f.store.audits[0])
			}
		})
	}
}

func TestChangeVersionCheck(t *testing.T) {
	f := newChangeFixture(t)
	sub := f.addSub(t, f.basic, 1, true, nil)
	_, err := f.svc.Change(context.Background(), f.tenantID, f.actor, &f.userID, sub.ID, nextCycle(f.pro.plan.ID), sub.Version+2)
	wantAppErr(t, err, apperr.KindPreconditionFailed, "version_mismatch")
	f.assertNoChangeEffects(t, sub)

	res, err := f.svc.Change(context.Background(), f.tenantID, f.actor, &f.userID, sub.ID, nextCycle(f.pro.plan.ID), sub.Version)
	if err != nil || res.Subscription.Version != sub.Version+1 {
		t.Fatalf("versão esperada confere: %v %d", err, res.Subscription.Version)
	}
}

func TestChangeValidation(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *changeFixture, sub *subscriptions.Subscription) (uuid.UUID, ChangeInput)
		kind  apperr.Kind
		code  string
	}{
		{"assinatura inexistente", func(f *changeFixture, _ *subscriptions.Subscription) (uuid.UUID, ChangeInput) {
			return uuid.New(), immediate(f.pro.plan.ID)
		}, apperr.KindNotFound, "subscription_not_found"},
		{"assinatura de outro tenant", func(f *changeFixture, sub *subscriptions.Subscription) (uuid.UUID, ChangeInput) {
			f.store.subs[0].TenantID = uuid.New()
			return sub.ID, immediate(f.pro.plan.ID)
		}, apperr.KindNotFound, "subscription_not_found"},
		{"plano sem preço vigente", func(f *changeFixture, sub *subscriptions.Subscription) (uuid.UUID, ChangeInput) {
			p := f.store.plans[f.pro.plan.ID]
			p.CurrentPriceVersionID = nil
			f.store.plans[p.ID] = p
			return sub.ID, immediate(p.ID)
		}, apperr.KindConflict, "plan_without_price"},
		{"assinatura cancelada", func(f *changeFixture, sub *subscriptions.Subscription) (uuid.UUID, ChangeInput) {
			f.store.subs[0].Status = subscriptions.Canceled
			return sub.ID, immediate(f.pro.plan.ID)
		}, apperr.KindConflict, "invalid_transition"},
		{"cancelamento agendado", func(f *changeFixture, sub *subscriptions.Subscription) (uuid.UUID, ChangeInput) {
			f.store.subs[0].CancelAtPeriodEnd = true
			return sub.ID, immediate(f.pro.plan.ID)
		}, apperr.KindConflict, "cancel_scheduled"},
		{"momento inválido", func(f *changeFixture, sub *subscriptions.Subscription) (uuid.UUID, ChangeInput) {
			return sub.ID, ChangeInput{PlanID: &f.pro.plan.ID, Timing: "amanha"}
		}, apperr.KindInvalid, "invalid_timing"},
		{"sem alteração", func(f *changeFixture, sub *subscriptions.Subscription) (uuid.UUID, ChangeInput) {
			return sub.ID, immediate(f.basic.plan.ID)
		}, apperr.KindInvalid, "no_change"},
		{"plano com consumo exige o próximo ciclo", func(f *changeFixture, sub *subscriptions.Subscription) (uuid.UUID, ChangeInput) {
			return sub.ID, immediate(f.metered.plan.ID)
		}, apperr.KindInvalid, "immediate_change_requires_fixed_price"},
		{"periodicidade diferente", func(f *changeFixture, sub *subscriptions.Subscription) (uuid.UUID, ChangeInput) {
			return sub.ID, nextCycle(f.yearly.plan.ID)
		}, apperr.KindInvalid, "invalid_change"},
		{"quantidade em plano sem unidade", func(f *changeFixture, sub *subscriptions.Subscription) (uuid.UUID, ChangeInput) {
			return sub.ID, ChangeInput{PlanID: &f.pro.plan.ID, Quantity: intPtr(3), Timing: subscriptions.ChangeNextCycle}
		}, apperr.KindInvalid, "invalid_change"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newChangeFixture(t)
			sub := f.addSub(t, f.basic, 1, true, nil)
			id, in := c.setup(f, &sub)
			before := f.store.subs[0]
			_, err := f.svc.Change(context.Background(), f.tenantID, f.actor, &f.userID, id, in, 0)
			wantAppErr(t, err, c.kind, c.code)
			f.assertNoChangeEffects(t, before)
		})
	}
	t.Run("quantidade fora do limite", func(t *testing.T) {
		f := newChangeFixture(t)
		sub := f.addSub(t, f.seats, 2, true, nil)
		_, err := f.svc.Change(context.Background(), f.tenantID, f.actor, &f.userID, sub.ID,
			ChangeInput{Quantity: intPtr(0), Timing: subscriptions.ChangeImmediately}, 0)
		wantAppErr(t, err, apperr.KindInvalid, "invalid_change")
		f.assertNoChangeEffects(t, sub)
	})
}

func TestChangeRollsBackOnFailure(t *testing.T) {
	type scenario func(t *testing.T, f *changeFixture) (subscriptions.Subscription, ChangeInput)
	upgrade := func(t *testing.T, f *changeFixture) (subscriptions.Subscription, ChangeInput) {
		f.store.discount = &Discount{ID: uuid.New(), CouponCode: "DEZ", PercentBP: 1000}
		sub := f.addSub(t, f.free, 1, true, nil)
		f.store.credit[sub.CustomerID] = brl(1000)
		return sub, immediate(f.pro.plan.ID)
	}
	downgrade := func(t *testing.T, f *changeFixture) (subscriptions.Subscription, ChangeInput) {
		f.store.discount = &Discount{ID: uuid.New(), CouponCode: "PRO10", PercentBP: 1000, AppliesToPlanIDs: []uuid.UUID{f.pro.plan.ID}}
		return f.addSub(t, f.pro, 1, true, nil), immediate(f.basic.plan.ID)
	}
	schedule := func(t *testing.T, f *changeFixture) (subscriptions.Subscription, ChangeInput) {
		return f.addSub(t, f.basic, 1, true, nil), nextCycle(f.metered.plan.ID)
	}
	cases := []struct {
		name     string
		scenario scenario
		method   string
		nth      int
	}{
		{"leitura da assinatura", upgrade, "Subscription", 0},
		{"preço atual", upgrade, "PriceVersion", 1},
		{"plano atual", upgrade, "Plan", 1},
		{"novo plano", upgrade, "Plan", 2},
		{"novo preço", upgrade, "PriceVersion", 2},
		{"cancelamento de pendentes", upgrade, "CancelPendingChanges", 0},
		{"desconto antes da prorrata", upgrade, "ActiveDiscount", 1},
		{"desconto na troca", upgrade, "ActiveDiscount", 2},
		{"cancelamento dos reajustes", upgrade, "CancelScheduledAdjustments", 0},
		{"configuração do tenant", upgrade, "TenantSettings", 0},
		{"dados do cliente", upgrade, "CustomerForInvoice", 0},
		{"saldo de crédito", upgrade, "CustomerCreditBalance", 0},
		{"numeração", upgrade, "NextInvoiceNumber", 0},
		{"gravação da fatura", upgrade, "InsertInvoice", 0},
		{"histórico da fatura", upgrade, "InsertStatusHistory", 0},
		{"consumo do crédito", upgrade, "ConsumeCustomerCredit", 0},
		{"lançamento da fatura", upgrade, "PostLedger", 0},
		{"evento da fatura", upgrade, "Emit", 0},
		{"auditoria da fatura", upgrade, "Audit", 1},
		{"auditoria da mudança", upgrade, "Audit", 2},
		{"gravação da assinatura", upgrade, "UpdateSubscription", 0},
		{"histórico da assinatura", upgrade, "InsertSubscriptionHistory", 0},
		{"encerramento do desconto", downgrade, "EndActiveDiscount", 0},
		{"configuração na prorrata negativa", downgrade, "TenantSettings", 0},
		{"concessão do crédito", downgrade, "AddCustomerCredit", 0},
		{"lançamento do crédito", downgrade, "PostLedger", 0},
		{"auditoria do crédito", downgrade, "Audit", 1},
		{"evento da assinatura", downgrade, "Emit", 0},
		{"plano da plataforma", schedule, "PlatformPlan", 0},
		{"gravação do agendamento", schedule, "InsertScheduledChange", 0},
		{"cancelamento antes do agendamento", schedule, "CancelPendingChanges", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newChangeFixture(t)
			sub, in := c.scenario(t, f)
			discount := f.store.discount
			credit := f.store.credit[sub.CustomerID]
			boom := errors.New("falha em " + c.method)
			f.store.errs[c.method] = boom
			if c.nth > 0 {
				f.store.failAt[c.method] = c.nth
			}

			_, err := f.svc.Change(context.Background(), f.tenantID, f.actor, &f.userID, sub.ID, in, 0)
			if !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			f.assertNoChangeEffects(t, sub)
			if f.store.discount != discount || len(f.store.discountEnded) != 0 || len(f.store.canceledChanges) != 0 {
				t.Fatal("o desconto e as mudanças pendentes devem ser restaurados")
			}
			if f.store.credit[sub.CustomerID] != credit {
				t.Fatalf("saldo de crédito alterado: %v", f.store.credit[sub.CustomerID])
			}
		})
	}
}

func TestChangeImmediateUpgradeToPerUnitPlanDescribesQuantity(t *testing.T) {
	f := newChangeFixture(t)
	sub := f.addSub(t, f.free, 1, true, nil)

	res := f.change(t, sub.ID, ChangeInput{PlanID: &f.seats.plan.ID, Quantity: intPtr(3), Timing: subscriptions.ChangeImmediately})
	if res.Proration == nil || res.Proration.NewAmount.Amount() != 9000 || res.Proration.Net.Amount() != 3000 || len(f.store.invoices) != 1 {
		t.Fatalf("um terço de 3 unidades de R$ 30,00: %+v", res.Proration)
	}
	it := f.store.invoices[0].Items[0]
	if it.Amount.Amount() != 3000 || it.UnitAmount.Cmp(big.NewRat(9000, 1)) != 0 ||
		it.Description != "Cobrança proporcional de Plano Equipe: 3 unidade(s) (21/11/2026 a 30/11/2026, 10 de 30 dias)" {
		t.Fatalf("item proporcional do plano por unidade: %+v", it)
	}
	if got := f.store.sub(sub.ID); got.PlanID != f.seats.plan.ID || got.Quantity != 3 {
		t.Fatalf("assinatura no plano por unidade: %+v", got)
	}
}
