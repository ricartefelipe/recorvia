package billing

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// Correção de consumo pelo valor líquido do item faturado: o período fechado
// com 100 requisições (R$ 2,00) e o desconto distribuído no item.
func TestUsageCorrectorComparesNetOfDiscount(t *testing.T) {
	cases := []struct {
		name          string
		billed        int64 // quantidade faturada (R$ 0,02 por unidade)
		discount      int64 // desconto distribuído no item faturado
		recount       int64
		wantPrev      int64
		wantCorrected int64
		wantGross     int64
		wantDelta     int64
		wantOutcome   string
		wantRatio     string
	}{
		// Item de 200 com 100 de desconto: o cliente pagou 100. Recontagem de 60
		// unidades vale 120 bruto e 60 líquido: crédito de 40, não de 80.
		{"redução com cupom de 50% credita o líquido", 100, 100, 60, 100, 60, 120, -40, "customer_credit", "0.5"},
		{"aumento com cupom de 50% cobra o líquido", 100, 100, 150, 100, 150, 300, 50, "next_invoice", "0.5"},
		{"cupom de 100% não gera ajuste", 100, 200, 150, 0, 0, 300, 0, "no_effect", "1"},
		// 105 unidades (210) com 63 de desconto (30%): 107 unidades valem 214 bruto
		// e 214 × 0,7 = 149,8 líquido, arredondado uma única vez para 150.
		{"proporção aplicada com arredondamento único", 105, 63, 107, 147, 150, 214, 3, "next_invoice", "0.3"},
		{"sem desconto compara o valor do item", 100, 0, 80, 200, 160, 160, -40, "customer_credit", "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCorrectionFixture(t)
			sub := f.addSub(t, f.tenantID, nil)
			g, agg := f.addLate(t, sub, c.billed, c.recount)
			f.store.itemDiscounts[*agg.InvoiceItemID] = brl(c.discount)

			if n := f.run(t); n != 1 {
				t.Fatalf("grupos corrigidos: %d", n)
			}
			d := f.correctionAudit(t).Details
			if d["outcome"] != c.wantOutcome || d["previous_amount"] != c.wantPrev || d["corrected_amount"] != c.wantCorrected ||
				d["corrected_gross_amount"] != c.wantGross || d["delta"] != c.wantDelta || d["billed_amount"] != c.billed*2 ||
				d["billed_discount_amount"] != c.discount || d["discount_ratio"] != c.wantRatio {
				t.Fatalf("auditoria da correção líquida: %+v", d)
			}
			if stored := f.store.aggregates[lateKey(g)]; stored.CorrectedAmount.Amount() != c.wantCorrected {
				t.Fatalf("o agregado guarda o valor líquido corrigido: %v", stored.CorrectedAmount)
			}
			switch c.wantOutcome {
			case "customer_credit":
				if len(f.store.grants) != 1 || f.store.grants[0].amount.Amount() != -c.wantDelta {
					t.Fatalf("crédito pelo líquido cobrado a mais: %+v", f.store.grants)
				}
			case "next_invoice":
				if len(f.store.newPending) != 1 || f.store.newPending[0].Amount.Amount() != c.wantDelta ||
					f.store.newPending[0].Source != PendingSourceUsageCorrection || f.store.newPending[0].Details["discount_ratio"] != c.wantRatio {
					t.Fatalf("item pendente pelo líquido: %+v", f.store.newPending)
				}
			default:
				if len(f.store.grants) != 0 || len(f.store.newPending) != 0 || len(f.store.invoices) != 0 {
					t.Fatal("sem diferença líquida não há ajuste")
				}
			}
		})
	}

	t.Run("correção seguinte parte do líquido corrigido com a mesma proporção", func(t *testing.T) {
		f := newCorrectionFixture(t)
		sub := f.addSub(t, f.tenantID, nil)
		g, agg := f.addLate(t, sub, 100, 150)
		f.store.itemDiscounts[*agg.InvoiceItemID] = brl(100)
		f.run(t)

		f.store.late[f.tenantID] = []LateGroup{g}
		f.store.recompute[lateKey(g)] = recomputed{qty: big.NewRat(120, 1), events: 5}
		f.store.audits = nil
		f.run(t)
		d := f.correctionAudit(t).Details
		if d["previous_amount"] != int64(150) || d["corrected_amount"] != int64(120) || d["delta"] != int64(-30) {
			t.Fatalf("segunda correção: %+v", d)
		}
		if len(f.store.grants) != 1 || f.store.grants[0].amount.Amount() != 30 {
			t.Fatalf("crédito da segunda correção: %+v", f.store.grants)
		}
	})
}

// O item pendente da correção já tem a proporção de desconto do período
// corrigido e não recebe o desconto vigente na fatura seguinte.
func TestUsageCorrectionPendingItemIsNotDiscountedAgain(t *testing.T) {
	f := newCorrectionFixture(t)
	sub := f.addSub(t, f.tenantID, nil)
	_, agg := f.addLate(t, sub, 100, 150)
	f.store.itemDiscounts[*agg.InvoiceItemID] = brl(100)
	f.run(t)
	if len(f.store.newPending) != 1 || f.store.newPending[0].Amount.Amount() != 50 {
		t.Fatalf("item pendente líquido: %+v", f.store.newPending)
	}

	f.store.discount = &Discount{ID: uuid.New(), CouponCode: "METADE", PercentBP: 5000}
	f.store.usage = func(subscriptions.Subscription, calendar.Period) []UsageLine {
		return []UsageLine{{MetricName: "Requisições", Unit: "requisicao", Quantity: big.NewRat(1000, 1)}}
	}
	f.clock.Set(local(2026, time.December, 1, 10, 0))
	if _, err := NewEngine(f.store, f.clock).RunTenant(context.Background(), f.tenantID, audit.System("teste")); err != nil {
		t.Fatal(err)
	}
	if len(f.store.invoices) != 1 {
		t.Fatalf("fatura do ciclo: %+v", f.store.invoices)
	}
	inv := f.store.invoices[0]
	var usage, adjustment Item
	for _, it := range inv.Items {
		switch it.Kind {
		case ItemUsage:
			usage = it
		case ItemAdjustment:
			adjustment = it
		}
	}
	// Consumo de 2.000 com 50% de desconto; ajuste de 50 sem novo desconto.
	if usage.Amount.Amount() != 2000 || usage.DiscountAmount.Amount() != 1000 || adjustment.Amount.Amount() != 50 ||
		!adjustment.DiscountAmount.IsZero() {
		t.Fatalf("desconto somente sobre o consumo: consumo=%+v ajuste=%+v", usage, adjustment)
	}
	if inv.DiscountTotal.Amount() != 1000 || inv.Total.Amount() != 1050 {
		t.Fatalf("totais: desconto %v total %v", inv.DiscountTotal, inv.Total)
	}
	assertConservation(t, inv)
}

func TestInvoiceDiscountSkipsExemptItems(t *testing.T) {
	inv := NewDraft(uuid.New(), uuid.New(), uuid.New(), nil, KindSubscriptionCycle, money.BRL, nil, customers.Snapshot{}, engineNow)
	one := big.NewRat(1, 1)
	for _, it := range []Item{
		{ID: uuid.New(), Kind: ItemSubscriptionFee, Description: "Mensalidade", Quantity: one, UnitAmount: big.NewRat(10000, 1), Amount: brl(10000)},
		{ID: uuid.New(), Kind: ItemAdjustment, Description: "Correção", Quantity: one, UnitAmount: big.NewRat(4000, 1), Amount: brl(4000),
			DiscountExempt: true},
	} {
		if err := inv.AddItem(it); err != nil {
			t.Fatal(err)
		}
	}
	base, err := inv.DiscountableBase()
	if err != nil || base.Amount() != 10000 {
		t.Fatalf("base sem o item isento: %v %v", base, err)
	}
	applied, err := inv.ApplyDiscount(brl(12000))
	if err != nil || applied.Amount() != 10000 {
		t.Fatalf("o desconto é limitado aos itens não isentos: %v %v", applied, err)
	}
	if inv.Items[0].DiscountAmount.Amount() != 10000 || !inv.Items[1].DiscountAmount.IsZero() || inv.Total.Amount() != 4000 {
		t.Fatalf("o item isento mantém o valor integral: %+v", inv.Items)
	}
	if again, err := inv.ApplyDiscount(brl(100)); err != nil || !again.IsZero() {
		t.Fatalf("sem base restante não há desconto: %v %v", again, err)
	}
}

// Itens pendentes entram também na fatura de encerramento; sem consumo a
// faturar, viram fatura de ajuste com o período dos itens.
func TestEngineBillsPendingItemsWhenSubscriptionEnds(t *testing.T) {
	correctionPeriod := calendar.Period{Start: local(2026, time.October, 1, 0, 0), End: local(2026, time.November, 1, 0, 0)}
	pendingItem := func() PendingItem {
		p := correctionPeriod
		return PendingItem{ID: uuid.New(), Kind: ItemAdjustment, Source: PendingSourceUsageCorrection,
			Description: "Correção de consumo de Requisições", Amount: brl(4000), Period: &p}
	}
	cases := []struct {
		name       string
		offer      func(f *termsFixture) offer
		mutate     func(t *testing.T, s *subscriptions.Subscription)
		usage      int64
		wantKind   Kind
		wantTotal  int64
		wantItems  int
		wantPeriod func(f *termsFixture, sub subscriptions.Subscription) *calendar.Period
	}{
		{"encerramento ao fim do período com consumo", func(f *termsFixture) offer { return f.metered },
			func(_ *testing.T, s *subscriptions.Subscription) { s.CancelAtPeriodEnd = true }, 1000, KindFinalUsage, 6000, 2,
			func(f *termsFixture, sub subscriptions.Subscription) *calendar.Period {
				p := f.period(t, sub, 0)
				return &p
			}},
		{"encerramento ao fim do período sem consumo", func(f *termsFixture) offer { return f.basic },
			func(_ *testing.T, s *subscriptions.Subscription) { s.CancelAtPeriodEnd = true }, 0, KindManual, 4000, 1,
			func(*termsFixture, subscriptions.Subscription) *calendar.Period { return &correctionPeriod }},
		{"cancelamento imediato de plano sem consumo", func(f *termsFixture) offer { return f.basic },
			func(t *testing.T, s *subscriptions.Subscription) {
				if err := s.Cancel(subscriptions.CancelImmediately, "", false, true, local(2026, time.October, 20, 12, 0)); err != nil {
					t.Fatal(err)
				}
				s.PopEvents()
			}, 0, KindManual, 4000, 1,
			func(*termsFixture, subscriptions.Subscription) *calendar.Period { return &correctionPeriod }},
		{"cancelamento imediato no início do período com consumo", func(f *termsFixture) offer { return f.metered },
			func(t *testing.T, s *subscriptions.Subscription) {
				if err := s.Cancel(subscriptions.CancelImmediately, "", true, true, s.CurrentPeriod.Start); err != nil {
					t.Fatal(err)
				}
				s.PopEvents()
			}, 1000, KindManual, 4000, 1,
			func(*termsFixture, subscriptions.Subscription) *calendar.Period { return &correctionPeriod }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTermsFixture(t)
			o := c.offer(f)
			sub := f.addSub(t, o, 1, true, func(s *subscriptions.Subscription) { c.mutate(t, s) })
			p := pendingItem()
			f.store.pending[sub.ID] = []PendingItem{p}
			f.store.discount = &Discount{ID: uuid.New(), CouponCode: "METADE", PercentBP: 5000}
			if c.usage > 0 {
				f.store.usage = func(subscriptions.Subscription, calendar.Period) []UsageLine {
					return []UsageLine{{MetricName: "Requisições", Unit: "requisicao", Quantity: big.NewRat(c.usage, 1)}}
				}
			}

			res := f.run(t)
			if res.Invoices != 1 || len(f.store.invoices) != 1 {
				t.Fatalf("uma fatura de encerramento: %+v", res)
			}
			inv := f.store.invoices[0]
			wantPeriod := c.wantPeriod(f, sub)
			if inv.Kind != c.wantKind || inv.Total.Amount() != c.wantTotal || len(inv.Items) != c.wantItems || !inv.DiscountTotal.IsZero() ||
				inv.BillingPeriod == nil || !samePeriod(*inv.BillingPeriod, *wantPeriod) {
				t.Fatalf("fatura de encerramento: tipo=%s total=%v itens=%d período=%+v", inv.Kind, inv.Total, len(inv.Items), inv.BillingPeriod)
			}
			last := inv.Items[len(inv.Items)-1]
			if last.Kind != ItemAdjustment || last.Amount.Amount() != 4000 || !samePeriod(*last.Period, correctionPeriod) {
				t.Fatalf("item pendente faturado: %+v", last)
			}
			if f.store.pendingBilled[p.ID] != inv.ID {
				t.Fatal("o item pendente fica vinculado à fatura de encerramento")
			}
			if got := f.store.sub(sub.ID); got.Status != subscriptions.Canceled || got.NextBillingAt != nil {
				t.Fatalf("assinatura encerrada sem próximo faturamento: %+v", got)
			}

			f.clock.Advance(24 * time.Hour)
			f.run(t)
			if len(f.store.invoices) != 1 {
				t.Fatal("o item pendente não pode ser cobrado de novo")
			}
		})
	}
}

func TestBuildCycleInvoiceClosingStep(t *testing.T) {
	c := newCycleCase(t, catalog.Flat, 1)
	p1 := calendar.Period{Start: local(2026, time.September, 1, 0, 0), End: local(2026, time.October, 1, 0, 0)}
	p2 := calendar.Period{Start: local(2026, time.October, 1, 0, 0), End: local(2026, time.November, 1, 0, 0)}
	end := subscriptions.BillingStep{Kind: subscriptions.StepEnd, Index: 1}
	cases := []struct {
		name       string
		pending    []PendingItem
		wantNil    bool
		wantPeriod *calendar.Period
	}{
		{"sem itens pendentes não há fatura", nil, true, nil},
		{"período coberto pelos itens", []PendingItem{
			{ID: uuid.New(), Kind: ItemAdjustment, Description: "Ajuste de outubro", Amount: brl(300), Period: &p2},
			{ID: uuid.New(), Kind: ItemAdjustment, Description: "Ajuste avulso", Amount: brl(200)},
			{ID: uuid.New(), Kind: ItemAdjustment, Description: "Ajuste de setembro", Amount: brl(100), Period: &p1},
		}, false, &calendar.Period{Start: p1.Start, End: p2.End}},
		{"itens sem período", []PendingItem{{ID: uuid.New(), Kind: ItemAdjustment, Description: "Ajuste avulso", Amount: brl(200)}},
			false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv, _, err := BuildCycleInvoice(c.sub, c.plan, c.price, end, customers.Snapshot{Name: "Padaria Central"},
				[]UsageLine{usageLine(500, nil)}, tc.pending, nil, saoPaulo, engineNow)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantNil {
				if inv != nil {
					t.Fatalf("nenhuma fatura esperada: %+v", inv)
				}
				return
			}
			if inv.Kind != KindManual || len(inv.Items) != len(tc.pending) {
				t.Fatalf("fatura de ajuste somente com os itens pendentes: %+v", inv.Items)
			}
			if (tc.wantPeriod == nil) != (inv.BillingPeriod == nil) || (tc.wantPeriod != nil && !samePeriod(*inv.BillingPeriod, *tc.wantPeriod)) {
				t.Fatalf("período da fatura de ajuste: %+v", inv.BillingPeriod)
			}
		})
	}
}

// Mudança imediata com troca da versão de preço cancela os reajustes
// programados; troca de quantidade na mesma versão os mantém.
func TestChangeImmediateCancelsScheduledAdjustments(t *testing.T) {
	cases := []struct {
		name       string
		from       func(f *changeFixture) offer
		qty        int
		in         func(f *changeFixture) ChangeInput
		wantCancel bool
	}{
		{"troca de plano", func(f *changeFixture) offer { return f.pro }, 1, func(f *changeFixture) ChangeInput { return immediate(f.basic.plan.ID) }, true},
		{"troca de quantidade na mesma versão", func(f *changeFixture) offer { return f.seats }, 3,
			func(*changeFixture) ChangeInput {
				return ChangeInput{Quantity: intPtr(2), Timing: subscriptions.ChangeImmediately}
			}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newChangeFixture(t)
			sub := f.addSub(t, c.from(f), c.qty, true, nil)
			bp := 1000
			adj := subscriptions.PriceAdjustment{ID: uuid.New(), TenantID: f.tenantID, SubscriptionID: sub.ID,
				EffectiveDate: calendar.Date{Year: 2026, Month: time.December, Day: 1}, Kind: subscriptions.AdjustPercent, PercentBP: &bp,
				Reason: "Reajuste anual", Status: subscriptions.AdjustmentScheduled, CreatedAt: changeStart}
			f.store.adjustments[sub.ID] = []subscriptions.PriceAdjustment{adj}

			f.change(t, sub.ID, c.in(f))
			stored := f.store.adjustments[sub.ID][0]
			_, recorded := historyEvent(f.store.subHistory, "price_adjustment_canceled")
			ids, _ := f.store.audits[len(f.store.audits)-1].Details["canceled_adjustment_ids"].([]uuid.UUID)
			if !c.wantCancel {
				if stored.Status != subscriptions.AdjustmentScheduled || recorded || len(ids) != 0 {
					t.Fatalf("o reajuste continua programado: %+v", stored)
				}
				return
			}
			if stored.Status != subscriptions.AdjustmentCanceled || stored.CancelReason != subscriptions.AdjustmentCanceledPriceChanged ||
				stored.CanceledAt == nil || !stored.CanceledAt.Equal(changeNow) {
				t.Fatalf("reajuste cancelado pela troca de preço: %+v", stored)
			}
			ev, _ := historyEvent(f.store.subHistory, "price_adjustment_canceled")
			if !recorded || ev.Details["adjustment_id"] != adj.ID || ev.Details["reason"] != subscriptions.AdjustmentCanceledPriceChanged {
				t.Fatalf("histórico do cancelamento: %+v", ev)
			}
			if len(ids) != 1 || ids[0] != adj.ID {
				t.Fatalf("auditoria da mudança com o reajuste cancelado: %+v", f.store.audits)
			}
		})
	}
}

func TestDiscountNote(t *testing.T) {
	cases := map[int64]string{0: "", -10: "", 1000: ", desconto de 10%", 1250: ", desconto de 12,5%", 5: ", desconto de 0,05%"}
	for bp, want := range cases {
		if got := discountNote(bp); got != want {
			t.Errorf("discountNote(%d) = %q, esperado %q", bp, got, want)
		}
	}
}
