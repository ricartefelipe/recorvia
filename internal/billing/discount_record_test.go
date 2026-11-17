package billing

import (
	"math/big"
	"testing"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
)

// A emissão registra no snapshot de cada item com desconto o tipo, o
// identificador e o cupom do desconto aplicado; itens sem desconto (como o
// ajuste de correção de consumo, isento) não recebem o registro, e o mapa de
// detalhes do item pendente do chamador não é alterado. Fatura com
// mensalidade de R$ 50,00, excedente de R$ 10,00, ajuste manual de R$ 20,00
// e correção de consumo de R$ 3,00 (isenta).
func TestBuildCycleInvoiceRecordsDiscountOnItems(t *testing.T) {
	off := brl(400)
	cases := []struct {
		name      string
		discount  *Discount
		wantShare []int64 // desconto por item: mensalidade, excedente, ajuste manual, correção
		wantType  string
		wantKey   string
		wantValue any
	}{
		{"percentual", &Discount{ID: uuid.New(), CouponCode: "DEZ", PercentBP: 1000}, []int64{500, 100, 200, 0}, "percent",
			"discount_percent_bp", int64(1000)},
		{"valor fixo distribuído pelo maior resto", &Discount{ID: uuid.New(), CouponCode: "QUATRO", AmountOff: &off}, []int64{250, 50, 100, 0},
			"amount", "discount_amount_off", int64(400)},
		{"sem desconto", nil, []int64{0, 0, 0, 0}, "", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cc := newCycleCase(t, catalog.FlatPlusOverage, 1)
			manualDetails := map[string]any{"source": "manual"}
			pending := []PendingItem{
				{ID: uuid.New(), Kind: ItemAdjustment, Source: "manual", Description: "Implantação", Amount: brl(2000), Details: manualDetails},
				{ID: uuid.New(), Kind: ItemAdjustment, Source: PendingSourceUsageCorrection, Description: "Correção de consumo", Amount: brl(300),
					Details: map[string]any{"source": PendingSourceUsageCorrection}},
			}
			agg := uuid.New()
			inv, _ := cc.build(t, cc.billStep(1), []UsageLine{usageLine(1500, &agg)}, pending, c.discount)
			if len(inv.Items) != 4 {
				t.Fatalf("itens da fatura: %+v", inv.Items)
			}
			for i, it := range inv.Items {
				if it.DiscountAmount.Amount() != c.wantShare[i] {
					t.Fatalf("desconto do item %d (%s) = %v, esperado %d", i, it.Kind, it.DiscountAmount, c.wantShare[i])
				}
				snap := it.PriceSnapshot
				if c.wantShare[i] == 0 {
					for _, k := range []string{"discount_type", "discount_id", "discount_percent_bp", "discount_amount_off"} {
						if _, ok := snap[k]; ok {
							t.Fatalf("item %d sem desconto registra %s: %+v", i, k, snap)
						}
					}
					continue
				}
				if snap["discount_type"] != c.wantType || snap["discount_id"] != c.discount.ID || snap["coupon_code"] != c.discount.CouponCode ||
					snap[c.wantKey] != c.wantValue {
					t.Fatalf("snapshot do item %d com o desconto aplicado: %+v", i, snap)
				}
			}
			if inv.Items[0].PriceSnapshot["plan_code"] != "pro" || inv.Items[1].PriceSnapshot["metric_code"] != "api_calls" {
				t.Fatalf("o registro do desconto preserva o snapshot do preço: %+v | %+v", inv.Items[0].PriceSnapshot, inv.Items[1].PriceSnapshot)
			}
			if len(manualDetails) != 1 {
				t.Fatalf("os detalhes do item pendente do chamador não podem ser alterados: %+v", manualDetails)
			}
		})
	}
}

func TestDiscountType(t *testing.T) {
	off := brl(100)
	if (Discount{PercentBP: 500}).Type() != catalog.DiscountPercent || (Discount{AmountOff: &off}).Type() != catalog.DiscountAmount {
		t.Fatal("tipo do desconto")
	}
}

// Correção de consumo conforme o tipo do desconto registrado no item: o
// período fechado com 100 requisições (R$ 2,00) e R$ 1,00 de desconto no
// item. Desconto percentual mantém a proporção (50%); desconto de valor fixo
// mantém o valor absoluto de R$ 1,00 (nunca negativo); item sem desconto
// segue o bruto; item com desconto sem o tipo registrado (faturado antes do
// registro) usa a proporção.
func TestUsageCorrectorAppliesRecordedDiscountType(t *testing.T) {
	cases := []struct {
		name          string
		discount      int64
		discountType  catalog.DiscountType
		recount       int64
		wantCorrected int64
		wantDelta     int64
		wantRule      string
		wantRatio     string // "" quando a proporção não se aplica
		wantOutcome   string
	}{
		{"percentual mantém a proporção no aumento", 100, catalog.DiscountPercent, 150, 150, 50, correctionPercent, "0.5", "next_invoice"},
		{"percentual mantém a proporção na redução", 100, catalog.DiscountPercent, 60, 60, -40, correctionPercent, "0.5", "customer_credit"},
		// 300 bruto − 100 de desconto = 200; a proporção daria 150.
		{"valor fixo mantém o desconto absoluto no aumento", 100, catalog.DiscountAmount, 150, 200, 100, correctionAmount, "", "next_invoice"},
		// 120 bruto − 100 = 20; a proporção daria 60.
		{"valor fixo mantém o desconto absoluto na redução", 100, catalog.DiscountAmount, 60, 20, -80, correctionAmount, "", "customer_credit"},
		// 60 bruto − 100 seria negativo: o líquido fica em zero.
		{"valor fixo nunca deixa o líquido negativo", 100, catalog.DiscountAmount, 30, 0, -100, correctionAmount, "", "customer_credit"},
		{"valor fixo sem alteração líquida", 100, catalog.DiscountAmount, 100, 100, 0, correctionAmount, "", "no_effect"},
		{"item sem desconto segue o bruto", 0, "", 150, 300, 100, correctionNoDiscount, "0", "next_invoice"},
		{"tipo registrado sem desconto no item segue o bruto", 0, catalog.DiscountAmount, 150, 300, 100, correctionNoDiscount, "0", "next_invoice"},
		{"item anterior ao registro usa a proporção", 100, "", 150, 150, 50, correctionUnrecorded, "0.5", "next_invoice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCorrectionFixture(t)
			sub := f.addSub(t, f.tenantID, nil)
			g, agg := f.addLate(t, sub, 100, c.recount)
			f.store.itemDiscounts[*agg.InvoiceItemID] = brl(c.discount)
			if c.discountType != "" {
				f.store.itemDiscountTypes[*agg.InvoiceItemID] = c.discountType
			}

			if n := f.run(t); n != 1 {
				t.Fatalf("grupos corrigidos: %d", n)
			}
			d := f.correctionAudit(t).Details
			ratio, hasRatio := d["discount_ratio"]
			if d["corrected_amount"] != c.wantCorrected || d["corrected_gross_amount"] != c.recount*2 || d["delta"] != c.wantDelta ||
				d["previous_amount"] != 200-c.discount || d["discount_rule"] != c.wantRule || d["billed_discount_type"] != string(c.discountType) ||
				d["outcome"] != c.wantOutcome || hasRatio != (c.wantRatio != "") || (hasRatio && ratio != c.wantRatio) {
				t.Fatalf("auditoria da correção: %+v", d)
			}
			if stored := f.store.aggregates[lateKey(g)]; stored.CorrectedAmount.Amount() != c.wantCorrected {
				t.Fatalf("o agregado guarda o líquido corrigido: %v", stored.CorrectedAmount)
			}
			switch c.wantOutcome {
			case "next_invoice":
				if len(f.store.newPending) != 1 || f.store.newPending[0].Amount.Amount() != c.wantDelta {
					t.Fatalf("item pendente pela diferença líquida: %+v", f.store.newPending)
				}
			case "customer_credit":
				if len(f.store.grants) != 1 || f.store.grants[0].amount.Amount() != -c.wantDelta {
					t.Fatalf("crédito pela diferença líquida: %+v", f.store.grants)
				}
			default:
				if len(f.store.newPending) != 0 || len(f.store.grants) != 0 {
					t.Fatal("sem diferença líquida não há ajuste")
				}
			}
		})
	}

	t.Run("correções sucessivas com valor fixo partem do líquido corrigido", func(t *testing.T) {
		f := newCorrectionFixture(t)
		sub := f.addSub(t, f.tenantID, nil)
		g, agg := f.addLate(t, sub, 100, 150)
		f.store.itemDiscounts[*agg.InvoiceItemID] = brl(100)
		f.store.itemDiscountTypes[*agg.InvoiceItemID] = catalog.DiscountAmount
		f.run(t)

		f.store.late[f.tenantID] = []LateGroup{g}
		f.store.recompute[lateKey(g)] = recomputed{qty: big.NewRat(120, 1), events: 5}
		f.store.audits = nil
		f.run(t)
		// 240 bruto − 100 = 140 sobre os 200 da primeira correção.
		d := f.correctionAudit(t).Details
		if d["previous_amount"] != int64(200) || d["corrected_amount"] != int64(140) || d["delta"] != int64(-60) {
			t.Fatalf("segunda correção: %+v", d)
		}
	})
}
