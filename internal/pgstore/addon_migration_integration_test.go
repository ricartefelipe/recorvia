//go:build integration

package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ricartefelipe/recorvia/internal/infra/migrate"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// A migration 00021 preserva a cobrança feita pelo fechamento anterior a
// ela: o pedido concluído e já cobrado (updated_at dentro do período de um
// demonstrativo existente) fica vinculado a esse demonstrativo e não é
// cobrado de novo; o concluído ainda não cobrado entra no próximo
// fechamento. Um Down seguido de Up mantém os vínculos feitos depois.
func TestAddonMigrationKeepsBillingOfOldOrders(t *testing.T) {
	ctx := context.Background()
	env := pgtest.Isolated(t)
	fx := fixture.NewOn(t, pgtest.AppDBOf(t, env, 4), now)
	owner, err := pgxpool.New(ctx, env.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	tn := fx.NewTenant(t, "migracao-adicional")
	request := func() uuid.UUID {
		o, err := fx.Platform.RequestAddon(ctx, tn.ID, tn.Actor, tn.Owner.ID, "implantacao_assistida", "")
		if err != nil {
			t.Fatal(err)
		}
		return o.ID
	}
	billedA, pendingB, laterC := request(), request(), request()
	ov, err := fx.Platform.Overview(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	p0 := ov.Subscription
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	p1 := p0
	if err := p1.Advance(loc); err != nil {
		t.Fatal(err)
	}

	// Versão 20: o fechamento antigo cobrou A no demonstrativo do primeiro
	// período (pelo updated_at) e avançou a assinatura; B foi concluído no
	// período seguinte, ainda aberto.
	if _, err := migrate.DownTo(ctx, env.OwnerURL, 20); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	statementP0 := uuid.New()
	exec(`UPDATE platform_addon_orders SET status = 'completed', updated_at = $2 WHERE id = $1`, billedA, p0.PeriodStart.Add(time.Hour))
	exec(`INSERT INTO platform_statements (id, tenant_id, period_start, period_end, plan_code, subscription_amount, addons_amount,
		total_amount, currency, status, created_at) VALUES ($1, $2, $3, $4, 'essencial', 0, 250000, 250000, 'BRL', 'open', $4)`,
		statementP0, tn.ID, p0.PeriodStart, p0.PeriodEnd)
	exec(`UPDATE tenant_platform_subscriptions SET status = $2, current_period_start = $3, current_period_end = $4 WHERE tenant_id = $1`,
		tn.ID, string(p1.Status), p1.PeriodStart, p1.PeriodEnd)
	exec(`UPDATE platform_addon_orders SET status = 'completed', updated_at = $2 WHERE id = $1`, pendingB, p1.PeriodStart.Add(2*time.Hour))

	if _, err := migrate.Up(ctx, env.OwnerURL); err != nil {
		t.Fatal(err)
	}
	links := func() map[uuid.UUID]*uuid.UUID {
		t.Helper()
		orders, _, err := fx.Store.Platform().AddonOrders(ctx, tn.ID, 100, "")
		if err != nil {
			t.Fatal(err)
		}
		out := map[uuid.UUID]*uuid.UUID{}
		for _, o := range orders {
			out[o.ID] = o.StatementID
		}
		return out
	}
	if l := links(); l[billedA] == nil || *l[billedA] != statementP0 || l[pendingB] != nil {
		t.Fatalf("vínculos depois da migration: A=%v B=%v", l[billedA], l[pendingB])
	}
	fx.Clock.Set(p1.PeriodEnd.Add(time.Minute))
	if _, err := fx.Platform.CloseDueStatements(ctx); err != nil {
		t.Fatal(err)
	}
	sts, _, err := fx.Store.Platform().Statements(ctx, tn.ID, 100, "")
	if err != nil || len(sts) != 2 {
		t.Fatalf("demonstrativos: %+v %v", sts, err)
	}
	if sts[0].AddonsAmount.Amount() != 250000 || sts[0].Total.Amount() != 19900+250000 {
		t.Fatalf("o fechamento novo cobra só B (A já foi cobrado): %+v", sts[0])
	}
	statementP1 := sts[0].ID

	// C é concluído no fluxo novo e fica para o próximo fechamento; Down e Up
	// mantêm A e B nos demonstrativos que os cobraram e C sem cobrança.
	if _, err := fx.Platform.CompleteAddonOrder(ctx, platformbilling.Staff{UserID: newUser(t, fx).ID, DisplayName: "Equipe"}, tn.ID, laterC, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.DownTo(ctx, env.OwnerURL, 20); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Up(ctx, env.OwnerURL); err != nil {
		t.Fatal(err)
	}
	if l := links(); l[billedA] == nil || *l[billedA] != statementP0 || l[pendingB] == nil || *l[pendingB] != statementP1 || l[laterC] != nil {
		t.Fatalf("vínculos depois de Down e Up: A=%v B=%v C=%v", l[billedA], l[pendingB], l[laterC])
	}
	ov, err = fx.Platform.Overview(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	fx.Clock.Set(ov.Subscription.PeriodEnd.Add(time.Minute))
	if _, err := fx.Platform.CloseDueStatements(ctx); err != nil {
		t.Fatal(err)
	}
	sts, _, err = fx.Store.Platform().Statements(ctx, tn.ID, 100, "")
	if err != nil || len(sts) != 3 || sts[0].AddonsAmount.Amount() != 250000 {
		t.Fatalf("o fechamento seguinte cobra só C: %+v %v", sts, err)
	}
	var total int64
	for _, s := range sts {
		total += s.AddonsAmount.Amount()
	}
	if total != 3*250000 {
		t.Fatalf("cada pedido é cobrado uma vez: %d", total)
	}
}
