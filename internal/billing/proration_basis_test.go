package billing

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// A base do crédito da prorrata é o valor efetivamente faturado pelo
// componente fixo do período (item subscription_fee da fatura de ciclo,
// valor menos o desconto distribuído na emissão), e não o preço vigente com
// o cupom ativo no momento da mudança. A cobrança do novo plano usa o
// percentual do cupom ativo que vale para ele. Troca do Plano Básico
// (R$ 100,00) para o Pro (R$ 160,00) com 10 de 30 dias restantes.
func TestChangeImmediateCreditUsesBilledCycleFee(t *testing.T) {
	tenPercent := func() *Discount { return &Discount{ID: uuid.New(), CouponCode: "DEZ", PercentBP: 1000} }
	cases := []struct {
		name string
		// setup prepara a assinatura com o período 0 faturado e devolve o
		// item faturado esperado como base (nil quando não há base).
		setup      func(t *testing.T, f *changeFixture) (subscriptions.Subscription, *CycleFeeItem)
		wantBasis  string
		wantBase   int64
		wantCredit int64
		wantCharge int64
		wantDesc   string
	}{
		// O cupom foi aplicado depois da fatura do período: o cliente pagou
		// R$ 100,00 e recebe 1/3 disso; a cobrança tem os 10% do cupom.
		{"cupom aplicado no meio do período", func(t *testing.T, f *changeFixture) (subscriptions.Subscription, *CycleFeeItem) {
			sub := f.addSub(t, f.basic, 1, true, nil)
			f.store.discount = tenPercent()
			return sub, f.storedFee(sub)
		}, creditBasisCycleInvoice, 10000, 3333, 4800, "Crédito proporcional de Plano Básico (" + prorationSpan + ")"},
		// Cupom de uso único: o período foi faturado com 10% e o cupom já
		// foi encerrado; o crédito é de 1/3 dos R$ 90,00 pagos e a cobrança
		// não tem desconto.
		{"cupom de uso único encerrado após o faturamento", func(t *testing.T, f *changeFixture) (subscriptions.Subscription, *CycleFeeItem) {
			f.store.discount = tenPercent()
			sub := f.addSub(t, f.basic, 1, true, nil)
			f.store.discount = nil
			return sub, f.storedFee(sub)
		}, creditBasisCycleInvoice, 9000, 3000, 5333, "Crédito proporcional de Plano Básico (" + prorationSpan + ", líquido de desconto)"},
		{"fatura de ciclo cancelada não gera crédito", func(t *testing.T, f *changeFixture) (subscriptions.Subscription, *CycleFeeItem) {
			sub := f.addSub(t, f.basic, 1, true, nil)
			fee := f.storedFee(sub)
			fee.InvoiceStatus = Void
			f.store.cycleFees[cycleFeeKey(sub.ID, sub.CurrentPeriod.Start)] = *fee
			return sub, nil
		}, creditBasisNone, 0, 0, 5333, ""},
		{"período sem fatura de ciclo não gera crédito", func(t *testing.T, f *changeFixture) (subscriptions.Subscription, *CycleFeeItem) {
			sub := f.addSub(t, f.basic, 1, true, nil)
			delete(f.store.cycleFees, cycleFeeKey(sub.ID, sub.CurrentPeriod.Start))
			return sub, nil
		}, creditBasisNone, 0, 0, 5333, ""},
		{"fatura de ciclo paga continua valendo", func(t *testing.T, f *changeFixture) (subscriptions.Subscription, *CycleFeeItem) {
			sub := f.addSub(t, f.basic, 1, true, nil)
			fee := f.storedFee(sub)
			fee.InvoiceStatus = Paid
			f.store.cycleFees[cycleFeeKey(sub.ID, sub.CurrentPeriod.Start)] = *fee
			return sub, fee
		}, creditBasisCycleInvoice, 10000, 3333, 5333, "Crédito proporcional de Plano Básico (" + prorationSpan + ")"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newChangeFixture(t)
			sub, fee := c.setup(t, f)

			res := f.change(t, sub.ID, immediate(f.pro.plan.ID))
			pr := res.Proration
			net := c.wantCharge - c.wantCredit
			if pr == nil || pr.Credit.Amount() != c.wantCredit || pr.Charge.Amount() != c.wantCharge || pr.Net.Amount() != net ||
				pr.CreditBase.Cmp(big.NewRat(c.wantBase, 1)) != 0 {
				t.Fatalf("prorrata: crédito %d, cobrança %d, diferença %d e base %d esperados: %+v", c.wantCredit, c.wantCharge, net, c.wantBase, pr)
			}
			if res.InvoiceID == nil || len(f.store.invoices) != 1 {
				t.Fatalf("diferença positiva emite fatura de prorrata: %+v", res)
			}
			inv := f.store.invoices[0]
			if inv.Total.Amount() != net {
				t.Fatalf("total da prorrata: %v", inv.Total)
			}
			if c.wantCredit == 0 {
				if len(inv.Items) != 1 {
					t.Fatalf("sem base faturada, somente a cobrança: %+v", inv.Items)
				}
			} else {
				if len(inv.Items) != 2 {
					t.Fatalf("itens de cobrança e crédito: %+v", inv.Items)
				}
				credit := inv.Items[1]
				snap := credit.PriceSnapshot
				if credit.Amount.Amount() != -c.wantCredit || credit.Description != c.wantDesc || credit.UnitAmount.Cmp(big.NewRat(-c.wantBase, 1)) != 0 {
					t.Fatalf("item de crédito: %+v", credit)
				}
				if snap["credit_basis"] != c.wantBasis || snap["billed_invoice_id"] != fee.InvoiceID || snap["billed_item_id"] != fee.ItemID ||
					snap["billed_amount"] != fee.Amount.Amount() || snap["billed_discount_amount"] != fee.Discount.Amount() ||
					snap["credit_base_decimal"] != big.NewRat(c.wantBase, 1).RatString() {
					t.Fatalf("snapshot do crédito com a origem da base: %+v", snap)
				}
				if _, ok := snap["discount_percent_bp"]; ok {
					t.Fatalf("o crédito não usa o percentual do cupom ativo: %+v", snap)
				}
			}
			if len(f.store.prorations) != 1 {
				t.Fatalf("prorrata registrada: %+v", f.store.prorations)
			}
			rec := f.store.prorations[0]
			if rec.SubscriptionID != sub.ID || !samePeriod(rec.Period, sub.CurrentPeriod) || !rec.EffectiveAt.Equal(changeNow) ||
				rec.PriceVersionID != f.pro.price.ID || rec.Quantity != 1 || rec.RemainingDays != 10 || rec.PeriodDays != 30 ||
				rec.CreditBase.Cmp(big.NewRat(c.wantBase, 1)) != 0 || rec.Credit.Amount() != c.wantCredit || rec.Charge.Amount() != c.wantCharge ||
				rec.ChargeBase.Cmp(pr.ChargeUnitAmount()) != 0 || rec.InvoiceID == nil || *rec.InvoiceID != inv.ID {
				t.Fatalf("registro da prorrata: %+v", rec)
			}
			change := f.store.audits[len(f.store.audits)-1]
			if change.Details["proration_credit_basis"] != c.wantBasis || change.Details["proration_id"] != rec.ID {
				t.Fatalf("auditoria da mudança com a origem da base: %+v", change.Details)
			}
		})
	}
}

// storedFee devolve o item de componente fixo registrado para o período
// corrente da assinatura.
func (f *changeFixture) storedFee(sub subscriptions.Subscription) *CycleFeeItem {
	fee, ok := f.store.cycleFees[cycleFeeKey(sub.ID, sub.CurrentPeriod.Start)]
	if !ok {
		return nil
	}
	return &fee
}

// Cupom de valor fixo distribuído pela fatura de ciclo emitida pelo motor:
// R$ 30,00 sobre o componente fixo (R$ 100,00) e um ajuste pendente
// (R$ 50,00), repartidos pelo maior resto em R$ 20,00 e R$ 10,00. O crédito
// é de 1/3 dos R$ 80,00 faturados pelo componente fixo; a cobrança não
// prorrateia o valor fixo.
func TestChangeImmediateCreditWithFixedDiscountDistributedInCycleInvoice(t *testing.T) {
	f := newChangeFixture(t)
	off := brl(3000)
	f.store.discount = &Discount{ID: uuid.New(), CouponCode: "TRINTA", AmountOff: &off}
	sub := f.addSub(t, f.basic, 1, false, nil)
	f.store.pending[sub.ID] = []PendingItem{{ID: uuid.New(), Kind: ItemAdjustment, Source: "manual", Description: "Implantação",
		Amount: brl(5000)}}
	f.clock.Set(changeStart)
	if res, err := NewEngine(f.store, f.clock).RunTenant(context.Background(), f.tenantID, audit.System("teste")); err != nil || res.Invoices != 1 {
		t.Fatalf("faturamento do período: %+v %v", res, err)
	}
	cycle := f.store.invoices[0]
	fee := cycle.Items[0]
	if cycle.Kind != KindSubscriptionCycle || fee.Kind != ItemSubscriptionFee || fee.DiscountAmount.Amount() != 2000 ||
		cycle.Items[1].DiscountAmount.Amount() != 1000 {
		t.Fatalf("desconto de valor fixo distribuído na fatura de ciclo: %+v", cycle.Items)
	}

	f.clock.Set(changeNow)
	res := f.change(t, sub.ID, immediate(f.pro.plan.ID))
	pr := res.Proration
	// 8.000 × 1/3 = 2.666,67 → 2.667; cobrança 16.000 × 1/3 = 5.333.
	if pr == nil || pr.Credit.Amount() != 2667 || pr.Charge.Amount() != 5333 || pr.Net.Amount() != 2666 {
		t.Fatalf("prorrata sobre o valor faturado: %+v", pr)
	}
	if len(f.store.invoices) != 2 {
		t.Fatalf("fatura de ciclo e fatura de prorrata: %d", len(f.store.invoices))
	}
	credit := f.store.invoices[1].Items[1]
	if credit.Amount.Amount() != -2667 || credit.PriceSnapshot["billed_invoice_id"] != cycle.ID || credit.PriceSnapshot["billed_item_id"] != fee.ID ||
		credit.PriceSnapshot["billed_discount_amount"] != int64(2000) {
		t.Fatalf("crédito com a origem na fatura de ciclo: %+v", credit)
	}
}

// Duas mudanças imediatas no mesmo período: a segunda credita o componente
// cobrado na primeira (registrado na prorrata), e não o da fatura de ciclo.
// Básico (R$ 100,00) para Pro (R$ 160,00) em 21/11 (10 de 30 dias) e de volta
// para o Básico em 26/11 (5 de 30 dias).
func TestChangeImmediateTwiceInSamePeriodCreditsLatestComponent(t *testing.T) {
	second := local(2026, time.November, 26, 9, 0)
	cases := []struct {
		name               string
		discount           *Discount
		voidFirst          bool
		wantFirstNet       int64
		wantBasis          string
		wantCredit         int64
		wantCharge         int64
		wantCustomerCredit int64
	}{
		// 16.000 × 1/6 = 2.666,67 → 2.667; 10.000 × 1/6 = 1.666,67 → 1.667.
		{"sem desconto", nil, false, 2000, creditBasisProration, 2667, 1667, 1000},
		// Primeira cobrança: 16.000 × 0,9 = 14.400 por período; 14.400 × 1/6 =
		// 2.400; nova cobrança 10.000 × 0,9 × 1/6 = 1.500.
		{"cupom percentual nos dois planos", &Discount{ID: uuid.New(), CouponCode: "DEZ", PercentBP: 1000}, false, 1800, creditBasisProration,
			2400, 1500, 900},
		// A fatura da primeira prorrata foi cancelada: vale de novo a fatura de
		// ciclo (10.000 × 1/6 = 1.667), sem diferença.
		{"primeira prorrata cancelada volta à fatura de ciclo", nil, true, 2000, creditBasisCycleInvoice, 1667, 1667, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newChangeFixture(t)
			f.store.discount = c.discount
			sub := f.addSub(t, f.basic, 1, true, nil)

			first := f.change(t, sub.ID, immediate(f.pro.plan.ID))
			if first.Proration == nil || first.Proration.Net.Amount() != c.wantFirstNet || first.InvoiceID == nil {
				t.Fatalf("primeira mudança: %+v", first.Proration)
			}
			if c.voidFirst {
				f.store.invoices[0].Status = Void
			}
			f.clock.Set(second)
			res := f.change(t, sub.ID, immediate(f.basic.plan.ID))
			if len(f.store.prorations) != 2 {
				t.Fatalf("as duas prorratas são registradas: %+v", f.store.prorations)
			}
			rec := f.store.prorations[1]
			if rec.RemainingDays != 5 || rec.Credit.Amount() != c.wantCredit || rec.Charge.Amount() != c.wantCharge ||
				rec.CreditBase.Cmp(f.wantSecondBase(c.voidFirst, c.discount)) != 0 || rec.PriceVersionID != f.basic.price.ID {
				t.Fatalf("segunda prorrata: %+v", rec)
			}
			changeAudit := f.store.audits[len(f.store.audits)-1]
			if changeAudit.Details["proration_credit_basis"] != c.wantBasis {
				t.Fatalf("origem da base da segunda mudança: %+v", changeAudit.Details)
			}
			if c.wantCustomerCredit == 0 {
				if res.Proration != nil || res.CreditGranted != nil || len(f.store.grants) != 0 {
					t.Fatalf("sem diferença não há documento: %+v", res)
				}
				return
			}
			if res.CreditGranted == nil || res.CreditGranted.Amount() != c.wantCustomerCredit || len(f.store.grants) != 1 {
				t.Fatalf("crédito do cliente da segunda mudança: %+v", res)
			}
		})
	}
}

// wantSecondBase é a base esperada do crédito da segunda mudança: o valor
// cobrado pelo Pro na primeira prorrata, ou o valor faturado no ciclo
// quando a primeira prorrata foi cancelada.
func (f *changeFixture) wantSecondBase(voided bool, d *Discount) *big.Rat {
	if voided {
		return big.NewRat(10000, 1)
	}
	if d != nil {
		return big.NewRat(14400, 1)
	}
	return big.NewRat(16000, 1)
}

// A diferença zero não gera documento, mas a prorrata é registrada: o novo
// componente passa a ser a base de uma mudança seguinte no mesmo período.
func TestChangeImmediateWithZeroDifferenceRecordsProration(t *testing.T) {
	f := newChangeFixture(t)
	same := f.addOffer(newOffer("basico2", "Plano Básico 2", f.basic.plan.Interval, f.basic.price.Model, 10000))
	sub := f.addSub(t, f.basic, 1, true, nil)
	res := f.change(t, sub.ID, immediate(same.plan.ID))
	if res.Proration != nil || res.InvoiceID != nil || res.CreditGranted != nil {
		t.Fatalf("sem diferença não há documento: %+v", res)
	}
	if len(f.store.prorations) != 1 {
		t.Fatalf("prorrata registrada: %+v", f.store.prorations)
	}
	rec := f.store.prorations[0]
	if rec.InvoiceID != nil || rec.Credit.Amount() != 3333 || rec.Charge.Amount() != 3333 || rec.PriceVersionID != same.price.ID ||
		rec.ChargeBase.Cmp(big.NewRat(10000, 1)) != 0 {
		t.Fatalf("registro sem documento: %+v", rec)
	}
}

func TestBilledDocument(t *testing.T) {
	id := uuid.New()
	cases := []struct {
		name    string
		invoice *uuid.UUID
		status  Status
		want    bool
	}{
		{"sem fatura", nil, "", true},
		{"em aberto", &id, Open, true},
		{"paga", &id, Paid, true},
		{"parcialmente paga", &id, PartiallyPaid, true},
		{"incobrável", &id, Uncollectible, true},
		{"cancelada", &id, Void, false},
		{"rascunho", &id, Draft, false},
	}
	for _, c := range cases {
		if got := billedDocument(c.invoice, c.status); got != c.want {
			t.Errorf("%s: %v, esperado %v", c.name, got, c.want)
		}
	}
}
