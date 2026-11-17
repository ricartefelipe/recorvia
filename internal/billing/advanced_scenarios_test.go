package billing

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// Cenários complementares do faturamento avançado: combinações de mudança de
// plano, condições programadas, correção de consumo e notas de crédito.

const prorationSpan = "21/11/2026 a 30/11/2026, 10 de 30 dias"

func TestChangeImmediateIncreaseWithPreviousFixedCharge(t *testing.T) {
	percent := &Discount{ID: uuid.New(), CouponCode: "DEZ", PercentBP: 1000}
	fixedOff := brl(5000)
	amountOff := &Discount{ID: uuid.New(), CouponCode: "CINQUENTA", AmountOff: &fixedOff}
	upgrade := func(f *changeFixture) (offer, int, ChangeInput) { return f.basic, 1, immediate(f.pro.plan.ID) }
	// O cupom vale só para o plano anterior: reduz o crédito do período pago e
	// é encerrado na troca, sem desconto na cobrança do novo plano.
	restrictedUpgrade := func(f *changeFixture) (offer, int, ChangeInput) {
		f.store.discount = &Discount{ID: uuid.New(), CouponCode: "BASICO10", PercentBP: 1000, AppliesToPlanIDs: []uuid.UUID{f.basic.plan.ID}}
		return upgrade(f)
	}
	moreSeats := func(f *changeFixture) (offer, int, ChangeInput) {
		return f.seats, 2, ChangeInput{Quantity: intPtr(5), Timing: subscriptions.ChangeImmediately}
	}
	basicCredit := "Crédito proporcional de Plano Básico (" + prorationSpan + ")"
	proCharge := "Cobrança proporcional de Plano Pro (" + prorationSpan + ")"
	cases := []struct {
		name           string
		setup          func(f *changeFixture) (offer, int, ChangeInput)
		discount       *Discount
		balance        int64
		wantCredit     int64
		wantCharge     int64
		wantDiscount   int64
		wantTotal      int64
		wantDue        int64
		wantQty        int
		wantCreditDesc string
		wantChargeDesc string
	}{
		{"troca para plano maior", upgrade, nil, 0, 3333, 5333, 0, 2000, 2000, 1, basicCredit, proCharge},
		{"aumento de quantidade", moreSeats, nil, 0, 2000, 5000, 0, 3000, 3000, 5,
			"Crédito proporcional de Plano Equipe: 2 unidade(s) (" + prorationSpan + ")",
			"Cobrança proporcional de Plano Equipe: 5 unidade(s) (" + prorationSpan + ")"},
		// 10% nos dois lados: crédito 10.000 × 1/3 × 0,9 = 3.000; cobrança 16.000 × 1/3 × 0,9 = 4.800.
		{"cupom percentual reduz crédito e cobrança", upgrade, percent, 0, 3000, 4800, 0, 1800, 1800, 1,
			"Crédito proporcional de Plano Básico (" + prorationSpan + ", desconto de 10%)",
			"Cobrança proporcional de Plano Pro (" + prorationSpan + ", desconto de 10%)"},
		{"cupom de valor fixo não é prorrateado", upgrade, amountOff, 0, 3333, 5333, 0, 2000, 2000, 1, basicCredit, proCharge},
		{"cupom que não vale para o novo plano reduz só o crédito", restrictedUpgrade, nil, 0, 3000, 5333, 0, 2333, 2333, 1,
			"Crédito proporcional de Plano Básico (" + prorationSpan + ", desconto de 10%)", proCharge},
		{"crédito do cliente é consumido", upgrade, nil, 500, 3333, 5333, 0, 2000, 1500, 1, basicCredit, proCharge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newChangeFixture(t)
			f.store.discount = c.discount
			from, qty, in := c.setup(f)
			sub := f.addSub(t, from, qty, true, nil)
			if c.balance > 0 {
				f.store.credit[sub.CustomerID] = brl(c.balance)
			}

			res := f.change(t, sub.ID, in)
			net := c.wantCharge - c.wantCredit
			pr := res.Proration
			if pr == nil || pr.Credit.Amount() != c.wantCredit || pr.Charge.Amount() != c.wantCharge || pr.Net.Amount() != net {
				t.Fatalf("prorrata: crédito %d, cobrança %d e diferença %d esperados: %+v", c.wantCredit, c.wantCharge, net, pr)
			}
			if res.InvoiceID == nil || res.CreditGranted != nil || len(f.store.invoices) != 1 {
				t.Fatalf("diferença positiva emite uma fatura de prorrata: %+v", res)
			}
			inv := f.store.invoices[0]
			if inv.ID != *res.InvoiceID || inv.Kind != KindProration || inv.Status != Open || len(inv.Items) != 2 {
				t.Fatalf("fatura de prorrata com itens de crédito e cobrança: %+v", inv)
			}
			// A cobrança vem antes do crédito: o total parcial do rascunho nunca fica negativo.
			charge, credit := inv.Items[0], inv.Items[1]
			if credit.Kind != ItemProration || credit.Amount.Amount() != -c.wantCredit || credit.Description != c.wantCreditDesc ||
				*credit.PriceVersionID != sub.PriceVersionID {
				t.Fatalf("item de crédito proporcional: %+v", credit)
			}
			if charge.Kind != ItemProration || charge.Amount.Amount() != c.wantCharge || charge.Description != c.wantChargeDesc {
				t.Fatalf("item de cobrança proporcional: %+v", charge)
			}
			if inv.Subtotal.Amount() != net || inv.DiscountTotal.Amount() != c.wantDiscount || inv.Total.Amount() != c.wantTotal ||
				inv.AmountDue.Amount() != c.wantDue {
				t.Fatalf("subtotal=%v desconto=%v total=%v saldo=%v", inv.Subtotal, inv.DiscountTotal, inv.Total, inv.AmountDue)
			}
			assertConservation(t, inv)
			if n := ledgerNet(t, f.store.posted); n[ledger.BilledRevenue] != -c.wantTotal || n[ledger.AccountsReceivable] != c.wantDue {
				t.Fatalf("ledger da fatura de prorrata: %+v", n)
			}
			got := f.store.sub(sub.ID)
			if got.Quantity != c.wantQty || got.PriceVersionID != res.Subscription.PriceVersionID || res.Subscription.Version != sub.Version+1 {
				t.Fatalf("assinatura alterada imediatamente: quantidade=%d versão=%d", got.Quantity, res.Subscription.Version)
			}
		})
	}
}

func TestChangeImmediateCancelsScheduledChange(t *testing.T) {
	f := newChangeFixture(t)
	sub := f.addSub(t, f.seats, 2, true, nil)
	scheduled := f.change(t, sub.ID, ChangeInput{Quantity: intPtr(5), Timing: subscriptions.ChangeNextCycle}).Scheduled
	f.clock.Advance(time.Hour)

	res := f.change(t, sub.ID, ChangeInput{Quantity: intPtr(1), Timing: subscriptions.ChangeImmediately})
	ch := f.store.changes[sub.ID]
	if ch.ID != scheduled.ID || ch.Status != subscriptions.ChangeCanceled || ch.ResolvedAt == nil || !ch.ResolvedAt.Equal(f.clock.Now()) {
		t.Fatalf("a mudança imediata cancela a agendada: %+v", ch)
	}
	if res.CreditGranted == nil || res.CreditGranted.Amount() != 1000 || res.Subscription.Quantity != 1 {
		t.Fatalf("redução de 2 para 1 unidade com crédito de R$ 10,00: %+v", res)
	}

	f.clock.Set(local(2026, time.December, 1, 10, 0))
	if _, err := NewEngine(f.store, f.clock).RunTenant(context.Background(), f.tenantID, audit.System("teste")); err != nil {
		t.Fatalf("RunTenant: %v", err)
	}
	if len(f.store.invoices) != 1 {
		t.Fatalf("fatura de dezembro: %+v", f.store.invoices)
	}
	inv := f.store.invoices[0]
	if inv.Items[0].Amount.Amount() != 3000 || inv.CreditApplied.Amount() != 1000 || inv.AmountDue.Amount() != 2000 {
		t.Fatalf("o ciclo seguinte usa a quantidade da mudança imediata e consome o crédito: %+v", inv)
	}
	if f.store.sub(sub.ID).Quantity != 1 || len(f.store.resolved) != 0 {
		t.Fatal("a mudança cancelada não pode ser aplicada pelo motor")
	}
}

func TestChangeNextCycleDuringTrialAppliesAtFirstBilledPeriod(t *testing.T) {
	f := newChangeFixture(t)
	sub := f.addSub(t, f.basic, 1, false, func(in *subscriptions.NewInput) { in.TrialDays = 30 })
	if sub.Status != subscriptions.Trialing {
		t.Fatalf("assinatura em avaliação: %s", sub.Status)
	}
	trialEnd := local(2026, time.December, 1, 0, 0)

	res := f.change(t, sub.ID, nextCycle(f.pro.plan.ID))
	if res.Scheduled == nil || !res.Scheduled.EffectiveAt.Equal(trialEnd) {
		t.Fatalf("na avaliação, a mudança vale no fim da avaliação: %+v", res.Scheduled)
	}

	f.clock.Set(trialEnd.Add(10 * time.Hour))
	run, err := NewEngine(f.store, f.clock).RunTenant(context.Background(), f.tenantID, audit.System("teste"))
	if err != nil || run.Invoices != 1 {
		t.Fatalf("RunTenant: %+v %v", run, err)
	}
	inv := f.store.invoices[0]
	if !inv.BillingPeriod.Start.Equal(trialEnd) || inv.Total.Amount() != 16000 || *inv.Items[0].PlanID != f.pro.plan.ID {
		t.Fatalf("o primeiro período faturado já usa o novo plano: %+v", inv)
	}
	if f.store.resolved[res.Scheduled.ID] != subscriptions.ChangeApplied {
		t.Fatal("a mudança agendada na avaliação é aplicada")
	}
}

func TestChangeValidationOfTargetPlan(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *changeFixture) ChangeInput
		kind  apperr.Kind
		code  string
		field string
	}{
		{"assinatura suspensa", func(f *changeFixture) ChangeInput {
			f.store.subs[0].Status = subscriptions.Suspended
			return immediate(f.pro.plan.ID)
		}, apperr.KindConflict, "subscription_suspended", ""},
		{"plano arquivado", func(f *changeFixture) ChangeInput {
			p := f.store.plans[f.pro.plan.ID]
			p.Status = catalog.Archived
			f.store.plans[p.ID] = p
			return nextCycle(p.ID)
		}, apperr.KindInvalid, "invalid_change", "plan_id"},
		{"preço vigente de outro plano", func(f *changeFixture) ChangeInput {
			p := f.store.plans[f.pro.plan.ID]
			p.CurrentPriceVersionID = &f.free.price.ID
			f.store.plans[p.ID] = p
			return immediate(p.ID)
		}, apperr.KindInvalid, "invalid_change", "plan_id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newChangeFixture(t)
			f.addSub(t, f.basic, 1, true, nil)
			in := c.setup(f)
			before := f.store.subs[0]
			_, err := f.svc.Change(context.Background(), f.tenantID, f.actor, &f.userID, before.ID, in, 0)
			wantAppErr(t, err, c.kind, c.code)
			if e, _ := apperr.As(err); c.field != "" && (len(e.Fields) != 1 || e.Fields[0].Field != c.field) {
				t.Fatalf("campo do erro: %+v", e.Fields)
			}
			f.assertNoChangeEffects(t, before)
		})
	}
}

// A mudança agendada para novembro troca a versão de preço: o reajuste
// programado para dezembro foi definido sobre o plano anterior e é cancelado
// na mesma recuperação de períodos, com registro no histórico.
func TestEngineCatchUpChangeCancelsLaterAdjustment(t *testing.T) {
	f := newTermsFixture(t)
	sub := f.addSub(t, f.basic, 1, true, nil)
	ch := f.schedule(t, sub, &f.pro, nil)
	adj := f.adjustment(sub, calendar.Date{Year: 2026, Month: time.December, Day: 1}, subscriptions.AdjustPercent, 1000)
	f.clock.Set(local(2026, time.December, 1, 10, 0))

	res := f.run(t)
	if res.Invoices != 2 || len(f.store.invoices) != 2 {
		t.Fatalf("novembro e dezembro faturados na mesma execução: %+v", res)
	}
	nov, dec := f.store.invoices[0], f.store.invoices[1]
	if !samePeriod(*nov.BillingPeriod, f.period(t, sub, 1)) || nov.Total.Amount() != 16000 || *nov.Items[0].PlanID != f.pro.plan.ID {
		t.Fatalf("novembro já no novo plano, sem reajuste: %+v", nov.Items)
	}
	if !samePeriod(*dec.BillingPeriod, f.period(t, sub, 2)) || dec.Total.Amount() != 16000 {
		t.Fatalf("dezembro pelo preço contratado do novo plano: %+v", dec.Items)
	}
	if _, ok := dec.Items[0].PriceSnapshot["price_adjustment_id"]; ok {
		t.Fatal("o reajuste do plano anterior não vale para o novo plano")
	}
	if len(f.store.resolved) != 1 || f.store.resolved[ch.ID] != subscriptions.ChangeApplied {
		t.Fatalf("a mudança é aplicada uma única vez: %+v", f.store.resolved)
	}
	canceled := f.store.adjustments[sub.ID][0]
	if canceled.ID != adj.ID || canceled.Status != subscriptions.AdjustmentCanceled ||
		canceled.CancelReason != subscriptions.AdjustmentCanceledPriceChanged || canceled.CanceledAt == nil {
		t.Fatalf("reajuste programado cancelado pela troca de preço: %+v", canceled)
	}
	ev, ok := historyEvent(f.store.subHistory, "price_adjustment_canceled")
	if !ok || ev.Details["adjustment_id"] != adj.ID || ev.Details["reason"] != subscriptions.AdjustmentCanceledPriceChanged {
		t.Fatalf("histórico do cancelamento: %+v", ev)
	}
	a, ok := findAudit(f.store.audits, "subscription.plan_changed")
	if ids, _ := a.Details["canceled_adjustment_ids"].([]uuid.UUID); !ok || len(ids) != 1 || ids[0] != adj.ID {
		t.Fatalf("auditoria da mudança com os reajustes cancelados: %+v", a.Details)
	}
	if _, ok := findAudit(f.store.audits, "subscription.price_adjusted"); ok {
		t.Fatal("nenhum reajuste aplicado")
	}
	if got := f.store.sub(sub.ID); got.NextBillingIndex != 3 || got.PriceOverride != nil {
		t.Fatalf("assinatura após a recuperação dos períodos: %+v", got)
	}
}

func TestEngineDueAdjustmentAfterChangeToUsageOnlyPlan(t *testing.T) {
	cases := []struct {
		name      string
		effective calendar.Date
	}{
		{"reajuste na data da mudança", calendar.Date{Year: 2026, Month: time.November, Day: 1}},
		{"reajuste posterior à mudança", calendar.Date{Year: 2026, Month: time.December, Day: 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTermsFixture(t)
			sub := f.addSub(t, f.basic, 1, true, nil)
			adj := f.adjustment(sub, c.effective, subscriptions.AdjustPercent, 1000)
			ch := f.schedule(t, sub, &f.metered, nil)
			f.clock.Set(local(2026, time.December, 1, 10, 0))

			if _, err := f.engine.RunTenant(context.Background(), f.tenantID, f.actor); err != nil {
				t.Fatalf("reajuste sem componente fixo não pode bloquear o ciclo: %v", err)
			}
			got := f.store.sub(sub.ID)
			if got.NextBillingIndex != 3 || got.PlanID != f.metered.plan.ID || got.PriceOverride != nil {
				t.Fatalf("assinatura faturada no plano por consumo: %+v", got)
			}
			if f.store.resolved[ch.ID] != subscriptions.ChangeApplied {
				t.Fatal("a mudança agendada é aplicada")
			}
			for _, a := range f.store.adjustments[sub.ID] {
				if a.ID == adj.ID && (a.Status != subscriptions.AdjustmentCanceled || a.CancelReason != subscriptions.AdjustmentCanceledPriceChanged) {
					t.Fatalf("o reajuste é cancelado pela troca da versão de preço: %+v", a)
				}
			}
		})
	}
}

// Reajuste vencido que não pode valer para o preço vigente é cancelado com o
// motivo, sem interromper o faturamento do ciclo.
func TestEngineCancelsUnapplicableAdjustment(t *testing.T) {
	cases := []struct {
		name   string
		offer  func(f *termsFixture) offer
		bp     int64
		reason subscriptions.AdjustmentCancelReason
		total  int64
	}{
		{"preço somente por consumo", func(f *termsFixture) offer { return f.metered }, 1000,
			subscriptions.AdjustmentCanceledNoFixedComponent, 0},
		{"valor resultante acima do limite", func(f *termsFixture) offer {
			return f.add(newOffer("enterprise", "Plano Enterprise", calendar.Monthly, catalog.Flat, money.MaxAmount/5))
		}, 100000, subscriptions.AdjustmentCanceledAmountLimit, money.MaxAmount / 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTermsFixture(t)
			o := c.offer(f)
			sub := f.addSub(t, o, 1, true, nil)
			adj := f.adjustment(sub, calendar.Date{Year: 2026, Month: time.November, Day: 1}, subscriptions.AdjustPercent, c.bp)

			f.run(t)
			got := f.store.sub(sub.ID)
			if got.NextBillingIndex != 2 || got.PriceOverride != nil {
				t.Fatalf("o ciclo avança sem reajuste: %+v", got)
			}
			stored := f.store.adjustments[sub.ID][0]
			if stored.ID != adj.ID || stored.Status != subscriptions.AdjustmentCanceled || stored.CancelReason != c.reason ||
				stored.CanceledAt == nil || stored.ResultingAmount != nil {
				t.Fatalf("reajuste cancelado com motivo: %+v", stored)
			}
			ev, ok := historyEvent(f.store.subHistory, "price_adjustment_canceled")
			if !ok || ev.Details["adjustment_id"] != adj.ID || ev.Details["reason"] != c.reason {
				t.Fatalf("histórico do cancelamento: %+v", ev)
			}
			a, ok := findAudit(f.store.audits, "subscription.price_adjustment_canceled")
			if !ok || a.Actor != audit.System("billing.engine") || a.Details["adjustment_id"] != adj.ID || a.Details["reason"] != c.reason {
				t.Fatalf("auditoria do cancelamento: %+v", f.store.audits)
			}
			var total int64
			for _, inv := range f.store.invoices {
				total += inv.Total.Amount()
			}
			if total != c.total {
				t.Fatalf("faturado pelo preço contratado: %d", total)
			}
		})
	}
}

// useOveragePrice troca o plano do fixture por R$ 50,00 com franquia de 1.000
// requisições e excedente de R$ 0,02 por requisição.
func (f *correctionFixture) useOveragePrice() {
	o := newOffer("api-franquia", "Plano API com franquia", calendar.Monthly, catalog.Flat, 5000)
	metricID := f.metric.ID
	o.price.Model, o.price.UnitAmount, o.price.MetricID, o.price.IncludedQuantity =
		catalog.FlatPlusOverage, big.NewRat(2, 1), &metricID, big.NewRat(1000, 1)
	f.metered = o
	f.store.plans[o.plan.ID], f.store.prices[o.price.ID] = o.plan, o.price
}

func TestUsageCorrectorOverageFranchise(t *testing.T) {
	cases := []struct {
		name          string
		billed        int64
		recount       int64
		wantOutcome   string
		wantDelta     int64
		wantCorrected int64
	}{
		{"consumo abaixo da franquia passa a ter excedente", 900, 1200, "next_invoice", 400, 400},
		{"excedente menor vira crédito", 1100, 1050, "customer_credit", -100, 100},
		{"consumo volta para dentro da franquia", 1100, 900, "customer_credit", -200, 0},
		{"variação dentro da franquia não tem efeito", 900, 950, "no_effect", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCorrectionFixture(t)
			f.useOveragePrice()
			sub := f.addSub(t, f.tenantID, nil)
			g, agg := f.addLate(t, sub, c.billed, c.recount)
			// O item de excedente do fechamento cobra somente o que passou da franquia.
			billedAmount := max(c.billed-1000, 0) * 2
			f.store.itemAmounts[*agg.InvoiceItemID] = brl(billedAmount)

			if n := f.run(t); n != 1 {
				t.Fatalf("grupos corrigidos: %d", n)
			}
			d := f.correctionAudit(t).Details
			if d["outcome"] != c.wantOutcome || d["delta"] != c.wantDelta || d["previous_amount"] != billedAmount ||
				d["corrected_amount"] != c.wantCorrected || d["price_version_id"] != f.metered.price.ID {
				t.Fatalf("auditoria da correção com franquia: %+v", d)
			}
			if stored := f.store.aggregates[lateKey(g)]; stored.CorrectedAmount.Amount() != c.wantCorrected {
				t.Fatalf("valor corrigido do agregado: %v", stored.CorrectedAmount)
			}
			switch c.wantOutcome {
			case "next_invoice":
				want := "Correção de consumo de Requisições: 900 para 1200 requisicao (01/10/2026 a 31/10/2026)"
				if len(f.store.newPending) != 1 || f.store.newPending[0].Amount.Amount() != c.wantDelta || f.store.newPending[0].Description != want {
					t.Fatalf("item pendente somente pelo excedente: %+v", f.store.newPending)
				}
			case "customer_credit":
				if len(f.store.grants) != 1 || f.store.grants[0].amount.Amount() != -c.wantDelta || len(f.store.newPending) != 0 {
					t.Fatalf("crédito pela redução do excedente: %+v", f.store.grants)
				}
			default:
				if len(f.store.newPending) != 0 || len(f.store.grants) != 0 || len(f.store.invoices) != 0 || len(f.store.posted) != 0 {
					t.Fatal("variação dentro da franquia não gera ajuste")
				}
			}
		})
	}
}

func TestUsageCorrectionAdjustmentBilledOnNextCycle(t *testing.T) {
	f := newCorrectionFixture(t)
	sub := f.addSub(t, f.tenantID, nil)
	_, agg := f.addLate(t, sub, 100, 150)
	f.run(t)
	if len(f.store.newPending) != 1 {
		t.Fatalf("item de ajuste pendente: %+v", f.store.newPending)
	}
	pending := f.store.newPending[0]

	f.clock.Set(local(2026, time.December, 1, 10, 0))
	res, err := NewEngine(f.store, f.clock).RunTenant(context.Background(), f.tenantID, audit.System("teste"))
	if err != nil || res.Invoices != 1 {
		t.Fatalf("RunTenant: %+v %v", res, err)
	}
	inv := f.store.invoices[0]
	sched, err := sub.Schedule()
	if err != nil {
		t.Fatal(err)
	}
	if inv.Kind != KindSubscriptionCycle || !samePeriod(*inv.BillingPeriod, sched.Period(2)) || len(inv.Items) != 1 || inv.Total.Amount() != 100 {
		t.Fatalf("a correção entra na fatura do ciclo seguinte: %+v", inv)
	}
	it := inv.Items[0]
	if it.Kind != ItemAdjustment || it.Amount.Amount() != 100 || it.Description != correctionDesc || !samePeriod(*it.Period, agg.Period) {
		t.Fatalf("item de ajuste com o período corrigido: %+v", it)
	}
	if f.store.pendingBilled[pending.ID] != inv.ID {
		t.Fatal("o item pendente fica vinculado à fatura que o cobrou")
	}

	f.clock.Advance(time.Hour)
	if _, err := NewEngine(f.store, f.clock).RunTenant(context.Background(), f.tenantID, audit.System("teste")); err != nil {
		t.Fatalf("RunTenant: %v", err)
	}
	if len(f.store.invoices) != 1 {
		t.Fatal("o ajuste não pode ser cobrado duas vezes")
	}
}

func TestCreditNoteOnPartiallyPaidInvoiceKeepsStatus(t *testing.T) {
	f := newCreditNoteFixture(t)
	inv := f.addInvoice(t, 4000, true)
	if inv.Status != PartiallyPaid {
		t.Fatalf("fatura parcialmente paga: %s", inv.Status)
	}

	cn := f.issue(t, inv.ID, 1000, "")
	got := f.store.invoices[inv.ID]
	if got.Status != PartiallyPaid || got.AmountDue.Amount() != 5000 || got.AmountPaid.Amount() != 4000 || got.AmountCredited.Amount() != 1000 ||
		got.PaidAt != nil {
		t.Fatalf("a nota reduz o saldo sem quitar a fatura: %+v", got)
	}
	assertConservation(t, &got)
	ch := f.store.statusWrites[0].changes
	if len(ch) != 1 || ch[0].From == nil || *ch[0].From != PartiallyPaid || ch[0].To != PartiallyPaid || ch[0].Reason != "credit_note" ||
		ch[0].Details["credit_note_id"] != cn.ID {
		t.Fatalf("histórico sem mudança de situação: %+v", ch)
	}
	if len(f.store.events) != 1 || f.store.events[0].Type != outbox.EventInvoiceCredited {
		t.Fatalf("somente o evento de nota de crédito: %+v", f.store.events)
	}
	if p := f.store.events[0].Payload.(map[string]any); p["status"] != PartiallyPaid || p["amount_due"] != int64(5000) {
		t.Fatalf("payload do evento: %+v", p)
	}
	if len(f.store.refreshed) != 0 {
		t.Fatal("fatura com saldo não altera a situação da assinatura")
	}
}

func TestCreditNoteRepeatedKeyIgnoresNewAmount(t *testing.T) {
	f := newCreditNoteFixture(t)
	inv := f.addInvoice(t, 0, true)
	first := f.issue(t, inv.ID, 3000, "pedido-1")
	writes, events, posted, audits := len(f.store.statusWrites), len(f.store.events), len(f.store.posted), len(f.store.audits)

	again := f.issue(t, inv.ID, 5000, "pedido-1")
	if again != first || again.Amount.Amount() != 3000 {
		t.Fatalf("a mesma chave devolve a nota já emitida: %+v", again)
	}
	got := f.store.invoices[inv.ID]
	if got.AmountCredited.Amount() != 3000 || got.AmountDue.Amount() != 7000 || len(f.store.notes) != 1 {
		t.Fatalf("a fatura é creditada uma única vez: %+v", got)
	}
	if len(f.store.statusWrites) != writes || len(f.store.events) != events || len(f.store.posted) != posted || len(f.store.audits) != audits {
		t.Fatal("a repetição não pode gerar novos efeitos")
	}
}
