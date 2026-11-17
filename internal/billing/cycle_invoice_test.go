package billing

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

type cycleCase struct {
	plan  catalog.Plan
	price catalog.PriceVersion
	sub   subscriptions.Subscription
	sched calendar.Schedule
}

// newCycleCase cria uma assinatura iniciada em 16/10/2026 (períodos mensais
// a partir do dia 16).
func newCycleCase(t *testing.T, model catalog.PricingModel, quantity int) cycleCase {
	t.Helper()
	plan := fixturePlan()
	price := fixturePrice(plan, model)
	start := local(2026, time.October, 16, 9, 0)
	sub, err := subscriptions.New(subscriptions.NewInput{ID: uuid.New(), TenantID: uuid.New(), CustomerID: uuid.New(), Plan: plan, Price: price,
		Quantity: quantity, StartAt: start, Location: saoPaulo, Now: engineNow})
	if err != nil {
		t.Fatal(err)
	}
	sched, err := sub.Schedule()
	if err != nil {
		t.Fatal(err)
	}
	return cycleCase{plan: plan, price: price, sub: sub, sched: sched}
}

func (c cycleCase) billStep(n int) subscriptions.BillingStep {
	step := subscriptions.BillingStep{Kind: subscriptions.StepBill, Index: n, Period: c.sched.Period(n)}
	if n > 0 {
		prev := c.sched.Period(n - 1)
		step.UsagePeriod = &prev
	}
	return step
}

func (c cycleCase) build(t *testing.T, step subscriptions.BillingStep, usage []UsageLine, pending []PendingItem, d *Discount) (*Invoice, []usageItemLink) {
	t.Helper()
	inv, links, err := BuildCycleInvoice(c.sub, c.plan, c.price, step, customers.Snapshot{Name: "Padaria Central"}, usage, pending, d, saoPaulo, engineNow)
	if err != nil {
		t.Fatalf("BuildCycleInvoice: %v", err)
	}
	return inv, links
}

func usageLine(qty int64, agg *uuid.UUID) UsageLine {
	return UsageLine{MetricID: uuid.New(), MetricCode: "api_calls", MetricName: "Requisições", Unit: "requisicao", Aggregation: "sum",
		Quantity: big.NewRat(qty, 1), AggregateID: agg}
}

func TestBuildCycleInvoiceFlat(t *testing.T) {
	c := newCycleCase(t, catalog.Flat, 1)
	inv, links := c.build(t, c.billStep(1), nil, nil, nil)
	if inv == nil || len(links) != 0 {
		t.Fatalf("fatura de plano fixo: %v %v", inv, links)
	}
	if inv.Status != Draft || inv.Kind != KindSubscriptionCycle || *inv.BillingPeriod != c.sched.Period(1) || *inv.SubscriptionID != c.sub.ID {
		t.Fatalf("cabeçalho: %+v", inv)
	}
	if len(inv.Items) != 1 {
		t.Fatalf("itens: %+v", inv.Items)
	}
	it := inv.Items[0]
	if it.Kind != ItemSubscriptionFee || it.Amount.Amount() != 10000 || it.Quantity.Cmp(big.NewRat(1, 1)) != 0 || it.UnitAmount.Cmp(big.NewRat(10000, 1)) != 0 {
		t.Fatalf("item fixo: %+v", it)
	}
	if it.Description != "Plano Pro (16/11/2026 a 15/12/2026)" {
		t.Fatalf("descrição com o período local inclusivo: %q", it.Description)
	}
	if *it.Period != c.sched.Period(1) || *it.PlanID != c.plan.ID || *it.PriceVersionID != c.price.ID {
		t.Fatalf("vínculos do item: %+v", it)
	}
	want := map[string]any{"plan_code": "pro", "plan_name": "Plano Pro", "billing_interval": calendar.Monthly, "price_version": 2,
		"pricing_model": catalog.Flat, "currency": "BRL", "flat_amount": int64(10000)}
	for k, v := range want {
		if it.PriceSnapshot[k] != v {
			t.Fatalf("snapshot[%s]=%v, esperado %v", k, it.PriceSnapshot[k], v)
		}
	}
	if inv.Total.Amount() != 10000 {
		t.Fatalf("total: %v", inv.Total)
	}
}

func TestBuildCycleInvoiceSnapshotIsFrozen(t *testing.T) {
	c := newCycleCase(t, catalog.Flat, 1)
	inv, _ := c.build(t, c.billStep(0), nil, nil, nil)
	reajuste := brl(15000)
	c.price.FlatAmount = &reajuste
	c.plan.Name = "Plano Pro Max"
	if got := inv.Items[0].PriceSnapshot["flat_amount"]; got != int64(10000) {
		t.Fatalf("o snapshot do preço não pode acompanhar alterações posteriores: %v", got)
	}
	if got := inv.Items[0].PriceSnapshot["plan_name"]; got != "Plano Pro" {
		t.Fatalf("nome do plano congelado: %v", got)
	}
}

func TestBuildCycleInvoicePerUnit(t *testing.T) {
	c := newCycleCase(t, catalog.PerUnit, 3)
	inv, _ := c.build(t, c.billStep(0), nil, nil, nil)
	it := inv.Items[0]
	// 3 x 1234,5 centavos = 3703,5, arredondado uma única vez (half-even) para 3704.
	if it.Amount.Amount() != 3704 || it.Quantity.Cmp(big.NewRat(3, 1)) != 0 || it.UnitAmount.Cmp(big.NewRat(2469, 2)) != 0 {
		t.Fatalf("item por unidade: valor=%v qtd=%v unit=%v", it.Amount, it.Quantity, it.UnitAmount)
	}
	if !strings.HasPrefix(it.Description, "Plano Pro: 3 unidade(s) (") {
		t.Fatalf("descrição por unidade: %q", it.Description)
	}
	if it.PriceSnapshot["unit_amount_decimal"] != "1234.5" {
		t.Fatalf("snapshot do preço unitário: %v", it.PriceSnapshot["unit_amount_decimal"])
	}
}

func TestBuildCycleInvoiceUsage(t *testing.T) {
	c := newCycleCase(t, catalog.Usage, 1)
	agg := uuid.New()
	inv, links := c.build(t, c.billStep(1), []UsageLine{usageLine(1500, &agg), usageLine(10, nil)}, nil, nil)
	if inv == nil || len(inv.Items) != 2 {
		t.Fatalf("itens de consumo: %+v", inv)
	}
	it := inv.Items[0]
	if it.Kind != ItemUsage || it.Amount.Amount() != 525 || it.Quantity.Cmp(big.NewRat(1500, 1)) != 0 || *it.Period != c.sched.Period(0) {
		t.Fatalf("consumo do período anterior: %+v", it)
	}
	if !strings.Contains(it.Description, "Consumo de Requisições: 1500 requisicao (16/10/2026 a 15/11/2026)") {
		t.Fatalf("descrição: %q", it.Description)
	}
	if it.PriceSnapshot["aggregated_quantity"] != "1500" || it.PriceSnapshot["metric_code"] != "api_calls" ||
		it.PriceSnapshot["aggregation"] != "sum" || it.PriceSnapshot["plan_code"] != "pro" || it.PriceSnapshot["unit_amount_decimal"] != "0.35" {
		t.Fatalf("snapshot do consumo: %+v", it.PriceSnapshot)
	}
	if *it.UsageAggregateID != agg || len(links) != 1 || links[0] != (usageItemLink{aggregateID: agg, itemID: it.ID}) {
		t.Fatalf("vínculo do agregado: %+v", links)
	}
	// 10 x 0,35 = 3,5 centavos -> 4 (half-even).
	if inv.Items[1].Amount.Amount() != 4 || inv.Total.Amount() != 529 {
		t.Fatalf("arredondamento por item: %v total=%v", inv.Items[1].Amount, inv.Total)
	}
	for _, it := range inv.Items {
		if it.Kind == ItemSubscriptionFee {
			t.Fatal("plano por consumo não tem mensalidade")
		}
	}
}

func TestBuildCycleInvoiceUsageWithoutConsumption(t *testing.T) {
	c := newCycleCase(t, catalog.Usage, 1)
	cases := map[string]struct {
		step  subscriptions.BillingStep
		usage []UsageLine
	}{
		"primeiro período sem consumo anterior": {c.billStep(0), []UsageLine{usageLine(100, nil)}},
		"sem linhas de consumo":                 {c.billStep(1), nil},
		"consumo zero":                          {c.billStep(1), []UsageLine{usageLine(0, nil)}},
	}
	for name, cc := range cases {
		t.Run(name, func(t *testing.T) {
			if inv, _ := c.build(t, cc.step, cc.usage, nil, nil); inv != nil {
				t.Fatalf("sem consumo não há fatura: %+v", inv.Items)
			}
		})
	}
}

func TestBuildCycleInvoiceFlatPlusOverage(t *testing.T) {
	c := newCycleCase(t, catalog.FlatPlusOverage, 1)
	inv, _ := c.build(t, c.billStep(1), []UsageLine{usageLine(1500, nil)}, nil, nil)
	if len(inv.Items) != 2 {
		t.Fatalf("mensalidade e excedente: %+v", inv.Items)
	}
	fee, over := inv.Items[0], inv.Items[1]
	if fee.Kind != ItemSubscriptionFee || fee.Amount.Amount() != 5000 {
		t.Fatalf("mensalidade: %+v", fee)
	}
	// Somente o excedente: (1500 - 1000) x 2 centavos.
	if over.Kind != ItemOverage || over.Amount.Amount() != 1000 || over.Quantity.Cmp(big.NewRat(500, 1)) != 0 {
		t.Fatalf("excedente: %+v", over)
	}
	if !strings.Contains(over.Description, "Excedente de Requisições: 500 requisicao acima da franquia de 1000") {
		t.Fatalf("descrição do excedente: %q", over.Description)
	}
	if over.PriceSnapshot["aggregated_quantity"] != "1500" || over.PriceSnapshot["included_quantity"] != "1000" {
		t.Fatalf("snapshot com quantidade agregada e franquia: %+v", over.PriceSnapshot)
	}
	if inv.Total.Amount() != 6000 {
		t.Fatalf("total: %v", inv.Total)
	}

	within, _ := c.build(t, c.billStep(1), []UsageLine{usageLine(800, nil)}, nil, nil)
	if within.Total.Amount() != 5000 || !within.Items[1].Amount.IsZero() {
		t.Fatalf("consumo dentro da franquia não gera excedente: %+v", within.Items)
	}
}

func TestBuildCycleInvoiceEndAndFinalUsageSteps(t *testing.T) {
	c := newCycleCase(t, catalog.FlatPlusOverage, 1)
	usagePeriod := c.sched.Period(0)
	end := subscriptions.BillingStep{Kind: subscriptions.StepEnd, Index: 1, Period: c.sched.Period(1), UsagePeriod: &usagePeriod}
	inv, _ := c.build(t, end, []UsageLine{usageLine(1200, nil)}, nil, nil)
	if inv.Kind != KindFinalUsage || *inv.BillingPeriod != usagePeriod {
		t.Fatalf("encerramento fatura o período de consumo: %s %+v", inv.Kind, inv.BillingPeriod)
	}
	if len(inv.Items) != 1 || inv.Items[0].Kind != ItemOverage || inv.Items[0].Amount.Amount() != 400 {
		t.Fatalf("encerramento não cobra mensalidade: %+v", inv.Items)
	}

	partial := calendar.Period{Start: c.sched.Period(1).Start, End: c.sched.Period(1).Start.Add(5 * 24 * time.Hour)}
	final := subscriptions.BillingStep{Kind: subscriptions.StepFinalUsage, Index: 2, Period: partial, UsagePeriod: &partial}
	inv, _ = c.build(t, final, []UsageLine{usageLine(2000, nil)}, nil, nil)
	if inv.Kind != KindFinalUsage || *inv.BillingPeriod != partial || inv.Total.Amount() != 2000 {
		t.Fatalf("consumo final do período interrompido: %s %+v %v", inv.Kind, inv.BillingPeriod, inv.Total)
	}

	noUsage := subscriptions.BillingStep{Kind: subscriptions.StepEnd, Index: 0, Period: c.sched.Period(0)}
	if inv, _ := c.build(t, noUsage, nil, nil, nil); inv != nil {
		t.Fatal("encerramento sem período de consumo não gera fatura")
	}

	flat := newCycleCase(t, catalog.Flat, 1)
	if inv, _ := flat.build(t, end, nil, nil, nil); inv != nil {
		t.Fatal("plano fixo encerrado ao fim do período não gera fatura final")
	}
}

func TestBuildCycleInvoicePendingItems(t *testing.T) {
	c := newCycleCase(t, catalog.Flat, 1)
	period := c.sched.Period(0)
	pending := []PendingItem{
		{ID: uuid.New(), Kind: ItemProration, Description: "Prorrata de upgrade (20/10 a 15/11)", Amount: brl(1650), Period: &period,
			Details: map[string]any{"from_plan": "basic", "fraction": "27/31"}},
		{ID: uuid.New(), Kind: ItemAdjustment, Description: "Crédito de downgrade", Amount: brl(-650),
			Quantity: big.NewRat(2, 1), UnitAmount: big.NewRat(-325, 1)},
	}
	inv, _ := c.build(t, c.billStep(1), nil, pending, nil)
	if len(inv.Items) != 3 || inv.Total.Amount() != 11000 {
		t.Fatalf("itens pendentes somados à fatura: %+v total=%v", inv.Items, inv.Total)
	}
	pr := inv.Items[1]
	if pr.Kind != ItemProration || pr.Quantity.Cmp(big.NewRat(1, 1)) != 0 || pr.UnitAmount.Cmp(big.NewRat(1650, 1)) != 0 ||
		*pr.Period != period || pr.PriceSnapshot["fraction"] != "27/31" {
		t.Fatalf("prorrata sem quantidade explícita: %+v", pr)
	}
	adj := inv.Items[2]
	if adj.Quantity.Cmp(big.NewRat(2, 1)) != 0 || adj.UnitAmount.Cmp(big.NewRat(-325, 1)) != 0 {
		t.Fatalf("ajuste com quantidade e unitário informados: %+v", adj)
	}
}

func TestBuildCycleInvoiceDiscounts(t *testing.T) {
	c := newCycleCase(t, catalog.Flat, 1)
	pending := []PendingItem{{ID: uuid.New(), Kind: ItemProration, Description: "Prorrata", Amount: brl(1500)}}
	cases := []struct {
		name     string
		discount Discount
		want     int64
	}{
		{"percentual", Discount{ID: uuid.New(), PercentBP: 1000}, 1150},
		{"percentual com arredondamento", Discount{ID: uuid.New(), PercentBP: 333}, 383}, // 382,95 -> 383
		{"valor fixo", Discount{ID: uuid.New(), AmountOff: ptr(brl(2000))}, 2000},
		{"valor fixo acima do subtotal", Discount{ID: uuid.New(), AmountOff: ptr(brl(50000))}, 11500},
		{"cem por cento", Discount{ID: uuid.New(), PercentBP: 10000}, 11500},
	}
	for _, cc := range cases {
		t.Run(cc.name, func(t *testing.T) {
			d := cc.discount
			inv, _ := c.build(t, c.billStep(0), nil, pending, &d)
			if inv.DiscountTotal.Amount() != cc.want || inv.Total.Amount() != 11500-cc.want {
				t.Fatalf("desconto=%v total=%v, esperado desconto %d", inv.DiscountTotal, inv.Total, cc.want)
			}
			var perItem int64
			for _, it := range inv.Items {
				perItem += it.DiscountAmount.Amount()
			}
			if perItem != cc.want {
				t.Fatalf("desconto distribuído %d", perItem)
			}
		})
	}
}

func TestBuildCycleInvoiceWithCreditPendingItem(t *testing.T) {
	c := newCycleCase(t, catalog.Flat, 1)
	pending := []PendingItem{{ID: uuid.New(), Kind: ItemProration, Description: "Crédito de downgrade", Amount: brl(-4000)}}
	inv, _ := c.build(t, c.billStep(0), nil, pending, &Discount{ID: uuid.New(), PercentBP: 10000})
	// O desconto incide sobre o subtotal líquido (10000 - 4000).
	if inv.DiscountTotal.Amount() != 6000 || !inv.Total.IsZero() {
		t.Fatalf("desconto sobre subtotal líquido: desconto=%v total=%v", inv.DiscountTotal, inv.Total)
	}
}

func TestBuildCycleInvoiceErrors(t *testing.T) {
	t.Run("moeda do preço diferente da do valor fixo", func(t *testing.T) {
		c := newCycleCase(t, catalog.Flat, 1)
		other := money.New(100, money.Currency{})
		c.price.FlatAmount = &other
		if _, _, err := BuildCycleInvoice(c.sub, c.plan, c.price, c.billStep(0), customers.Snapshot{}, nil, nil, nil, saoPaulo, engineNow); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("esperava moedas diferentes: %v", err)
		}
	})
	t.Run("valor por unidade fora do intervalo", func(t *testing.T) {
		c := newCycleCase(t, catalog.PerUnit, 1)
		c.sub.Quantity = subscriptions.MaxQuantity
		c.price.UnitAmount = new(big.Rat).SetInt64(1 << 62)
		if _, _, err := BuildCycleInvoice(c.sub, c.plan, c.price, c.billStep(0), customers.Snapshot{}, nil, nil, nil, saoPaulo, engineNow); !errors.Is(err, money.ErrOverflow) {
			t.Fatalf("esperava overflow: %v", err)
		}
	})
	t.Run("consumo fora do intervalo", func(t *testing.T) {
		c := newCycleCase(t, catalog.Usage, 1)
		c.price.UnitAmount = new(big.Rat).SetInt64(1 << 62)
		if _, _, err := BuildCycleInvoice(c.sub, c.plan, c.price, c.billStep(1), customers.Snapshot{}, []UsageLine{usageLine(1000, nil)}, nil, nil, saoPaulo, engineNow); !errors.Is(err, money.ErrOverflow) {
			t.Fatalf("esperava overflow: %v", err)
		}
	})
	t.Run("total acima do limite", func(t *testing.T) {
		c := newCycleCase(t, catalog.Usage, 1)
		if _, _, err := BuildCycleInvoice(c.sub, c.plan, c.price, c.billStep(1), customers.Snapshot{}, []UsageLine{usageLine(money.MaxAmount*10, nil)}, nil, nil, saoPaulo, engineNow); err == nil {
			t.Fatal("total acima do limite deve ser rejeitado")
		}
	})
	t.Run("item pendente que deixa o total negativo", func(t *testing.T) {
		c := newCycleCase(t, catalog.Flat, 1)
		pending := []PendingItem{{ID: uuid.New(), Kind: ItemProration, Amount: brl(-20000)}}
		if _, _, err := BuildCycleInvoice(c.sub, c.plan, c.price, c.billStep(0), customers.Snapshot{}, nil, pending, nil, saoPaulo, engineNow); err == nil {
			t.Fatal("total negativo deve ser rejeitado")
		}
	})
	t.Run("desconto em moeda diferente", func(t *testing.T) {
		c := newCycleCase(t, catalog.Flat, 1)
		d := &Discount{ID: uuid.New(), AmountOff: ptr(money.New(100, money.Currency{}))}
		if _, _, err := BuildCycleInvoice(c.sub, c.plan, c.price, c.billStep(0), customers.Snapshot{}, nil, nil, d, saoPaulo, engineNow); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("esperava moedas diferentes: %v", err)
		}
	})
}

func TestDiscountAmount(t *testing.T) {
	base := brl(9999)
	if got, _ := (Discount{PercentBP: 1500}).Amount(base); got.Amount() != 1500 { // 1499,85 -> 1500
		t.Fatalf("15%% de 99,99: %v", got)
	}
	if got, _ := (Discount{AmountOff: ptr(brl(500))}).Amount(base); got.Amount() != 500 {
		t.Fatalf("valor fixo: %v", got)
	}
	if got, _ := (Discount{AmountOff: ptr(brl(50000))}).Amount(base); got.Amount() != 9999 {
		t.Fatalf("valor fixo limitado à base: %v", got)
	}
}

func TestIssuedEvent(t *testing.T) {
	inv := issued(t, 10000)
	sub := uuid.New()
	inv.SubscriptionID = &sub
	ev := IssuedEvent(inv, saoPaulo, engineNow)
	p := ev.Payload.(map[string]any)
	if ev.Type != "invoice.issued" || ev.AggregateType != "invoice" || ev.AggregateID != inv.ID || ev.TenantID != inv.TenantID {
		t.Fatalf("evento: %+v", ev)
	}
	if p["invoice_id"] != inv.ID || p["customer_id"] != inv.CustomerID || p["subscription_id"] != &sub || p["total"] != int64(10000) ||
		p["currency"] != "BRL" || p["kind"] != KindManual || p["due_date"] != "2026-11-21" || p["due_at"] != *inv.DueAt {
		t.Fatalf("payload: %+v", p)
	}
	draft := newDraft()
	if _, ok := IssuedEvent(draft, saoPaulo, engineNow).Payload.(map[string]any)["due_date"]; ok {
		t.Fatal("sem vencimento não há due_date")
	}
}
