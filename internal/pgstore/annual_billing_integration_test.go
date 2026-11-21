//go:build integration

package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// annualPlan cria produto e plano anual de valor fixo.
func annualPlan(t *testing.T, env *fixture.Env, tn fixture.Tenant, amount int64) catalog.PlanDetails {
	t.Helper()
	ctx := context.Background()
	prod, err := env.Catalog.CreateProduct(ctx, tn.ID, tn.Actor, catalog.ProductInput{Name: "Produto anual"})
	if err != nil {
		t.Fatal(err)
	}
	uid := tn.Owner.ID
	p, err := env.Catalog.CreatePlan(ctx, tn.ID, tn.Actor, &uid, catalog.PlanInput{ProductID: prod.ID, Code: "anual-" + uuid.NewString()[:8],
		Name: "Plano Anual", Interval: calendar.Yearly}, catalog.PriceInput{Model: catalog.Flat, FlatAmount: &amount})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// annualInvoices devolve as faturas da assinatura com itens, da mais antiga
// para a mais recente.
func annualInvoices(t *testing.T, env *fixture.Env, tn fixture.Tenant, subID uuid.UUID) []billing.Details {
	t.Helper()
	ctx := context.Background()
	pg, err := env.Billing.List(ctx, tn.ID, billing.ListFilter{SubscriptionID: &subID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]billing.Details, 0, len(pg.Items))
	for i := len(pg.Items) - 1; i >= 0; i-- {
		d, err := env.Billing.Get(ctx, tn.ID, pg.Items[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// O motor fatura plano anual uma vez por ano, adiantado, com o período de um
// ano civil a partir da âncora no fuso do tenant; antes do aniversário não há
// nova fatura e, no aniversário, emite a do ano seguinte.
func TestBillingEngineIssuesAnnualPlanInvoice(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now) // 10/03/2026, 12:00 em São Paulo
	tn := env.NewTenant(t, "anual")
	plan := annualPlan(t, env, tn, 1_200_000)
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	start := calendar.Date{Year: 2025, Month: time.March, Day: 31}
	backdated := env.NewSubscription(t, tn, env.NewCustomer(t, tn).ID, plan.ID, &start)
	current := env.NewSubscription(t, tn, env.NewCustomer(t, tn).ID, plan.ID, nil)
	if backdated.Interval != calendar.Yearly || current.Interval != calendar.Yearly {
		t.Fatalf("assinaturas deveriam herdar a periodicidade anual: %s %s", backdated.Interval, current.Interval)
	}

	res, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor)
	if err != nil || res.Invoices != 2 {
		t.Fatalf("primeira execução: %+v %v (esperadas 2 faturas)", res, err)
	}
	if res, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil || res.Invoices != 0 {
		t.Fatalf("segunda execução no mesmo dia: %+v %v", res, err)
	}

	period := func(p *calendar.Period) string {
		if p == nil {
			return "sem período"
		}
		return calendar.DateOf(p.Start, loc).String() + " a " + calendar.DateOf(p.End, loc).String()
	}
	check := func(d billing.Details, want string) {
		t.Helper()
		if d.Status != billing.Open || d.Kind != billing.KindSubscriptionCycle || d.Total.Amount() != 1_200_000 || d.AmountDue.Amount() != 1_200_000 ||
			d.Number == "" || d.IssuedAt == nil || d.DueAt == nil {
			t.Fatalf("fatura anual: %+v", d.Invoice)
		}
		if got := period(d.BillingPeriod); got != want {
			t.Fatalf("período da fatura anual = %s, esperado %s", got, want)
		}
		if len(d.Items) != 1 || d.Items[0].Amount.Amount() != 1_200_000 || period(d.Items[0].Period) != want {
			t.Fatalf("item da fatura anual: %+v", d.Items)
		}
	}
	invs := annualInvoices(t, env, tn, backdated.ID)
	if len(invs) != 1 {
		t.Fatalf("assinatura retroativa: %d faturas, esperada 1", len(invs))
	}
	check(invs[0], "2025-03-31 a 2026-03-31")
	invs = annualInvoices(t, env, tn, current.ID)
	if len(invs) != 1 {
		t.Fatalf("assinatura iniciada hoje: %d faturas, esperada 1", len(invs))
	}
	check(invs[0], "2026-03-10 a 2027-03-10")

	got, err := env.Subscriptions.Get(ctx, tn.ID, backdated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != subscriptions.Active || got.NextBillingAt == nil || calendar.DateOf(*got.NextBillingAt, loc).String() != "2026-03-31" {
		t.Fatalf("próximo faturamento da assinatura anual: %+v", got)
	}

	// No aniversário da âncora, a assinatura retroativa recebe o ano seguinte.
	env.Clock.Set(time.Date(2026, time.March, 31, 15, 0, 0, 0, time.UTC))
	res, err = env.Engine.RunTenant(ctx, tn.ID, tn.Actor)
	if err != nil || res.Invoices != 1 {
		t.Fatalf("execução no aniversário: %+v %v (esperada 1 fatura)", res, err)
	}
	invs = annualInvoices(t, env, tn, backdated.ID)
	if len(invs) != 2 {
		t.Fatalf("assinatura retroativa após o aniversário: %d faturas", len(invs))
	}
	check(invs[1], "2026-03-31 a 2027-03-31")
	if n := len(annualInvoices(t, env, tn, current.ID)); n != 1 {
		t.Fatalf("assinatura iniciada em 10/03 não pode ser faturada antes do aniversário: %d faturas", n)
	}
}
