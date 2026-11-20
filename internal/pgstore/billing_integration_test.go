//go:build integration

package pgstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

func countRows(t *testing.T, env *fixture.Env, tenantID uuid.UUID, query string, args ...any) int {
	t.Helper()
	var n int
	err := env.DB.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), query, args...).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Dois motores concorrentes sobre o mesmo tenant: cada assinatura é faturada
// exatamente uma vez e a numeração não repete nem salta.
func TestBillingEnginesRunConcurrentlyWithoutDuplicates(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 16, now)
	tn := env.NewTenant(t, "motor")
	plan := env.NewFlatPlan(t, tn, 15000)
	const subs = 12
	for range subs {
		c := env.NewCustomer(t, tn)
		env.NewSubscription(t, tn, c.ID, plan.ID, nil)
	}
	engines := []*billing.Engine{billing.NewEngine(env.Store.Billing(), env.Clock), billing.NewEngine(env.Store.Billing(), env.Clock)}
	var wg sync.WaitGroup
	errs := make(chan error, len(engines))
	for _, e := range engines {
		wg.Go(func() {
			if _, err := e.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("motor falhou: %v", err)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM invoices WHERE tenant_id = $1`, tn.ID); n != subs {
		t.Fatalf("faturas emitidas = %d, esperado %d", n, subs)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(DISTINCT number) FROM invoices WHERE tenant_id = $1`, tn.ID); n != subs {
		t.Fatalf("números distintos = %d", n)
	}
	if n := countRows(t, env, tn.ID, `SELECT max(split_part(number, '-', 3)::int) FROM invoices WHERE tenant_id = $1`, tn.ID); n != subs {
		t.Fatalf("numeração com lacunas: maior número %d", n)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, tn.ID, outbox.EventInvoiceIssued); n != subs {
		t.Fatalf("eventos invoice.issued = %d", n)
	}
	// Saldo do ledger: contas a receber = faturado.
	if n := countRows(t, env, tn.ID, `SELECT COALESCE(sum(CASE WHEN e.direction='debit' THEN e.amount ELSE -e.amount END),0)::int
		FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id WHERE e.tenant_id = $1 AND a.code = 'accounts_receivable'`, tn.ID); n != subs*15000 {
		t.Fatalf("contas a receber = %d", n)
	}
}

// Retomada após indisponibilidade: uma assinatura iniciada há três meses
// recebe as faturas atrasadas uma única vez; nova execução não gera nada.
func TestBillingCatchUpAfterSchedulerDowntime(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "catchup")
	plan := env.NewFlatPlan(t, tn, 20000)
	c := env.NewCustomer(t, tn)
	start := calendar.Date{Year: 2025, Month: time.December, Day: 31}
	sub := env.NewSubscription(t, tn, c.ID, plan.ID, &start)
	res, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor)
	if err != nil {
		t.Fatal(err)
	}
	// Períodos: 31/12, 31/01, 28/02 e 31/03 ainda não começou (agora é 10/03).
	if res.Invoices != 3 {
		t.Fatalf("faturas atrasadas = %d, esperado 3", res.Invoices)
	}
	res, err = env.Engine.RunTenant(ctx, tn.ID, tn.Actor)
	if err != nil || res.Invoices != 0 {
		t.Fatalf("segunda execução: %+v %v", res, err)
	}
	got, err := env.Subscriptions.Get(ctx, tn.ID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	if d := calendar.DateOf(*got.NextBillingAt, loc).String(); d != "2026-03-31" {
		t.Fatalf("próximo faturamento %s, esperado 2026-03-31 (âncora 31 preservada)", d)
	}
	// O índice único impede uma segunda fatura do mesmo período.
	err = env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, status, kind, billing_period_start, billing_period_end,
			subtotal, total, amount_due, customer_snapshot) SELECT gen_random_uuid(), tenant_id, customer_id, subscription_id, 'draft', kind,
			billing_period_start, billing_period_end, 0, 0, 0, customer_snapshot FROM invoices WHERE tenant_id = $1 LIMIT 1`, tn.ID)
		return err
	})
	if err == nil {
		t.Fatal("o banco aceitou fatura duplicada para o mesmo período")
	}
}

// Fatura emitida não pode ser reescrita (gatilho no banco).
func TestIssuedInvoiceCannotBeRewritten(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	tn := env.NewTenant(t, "guard")
	plan := env.NewFlatPlan(t, tn, 5000)
	env.NewSubscription(t, tn, env.NewCustomer(t, tn).ID, plan.ID, nil)
	if _, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE invoices SET total = 1, subtotal = 1, amount_due = 1 WHERE tenant_id = $1`,
		`DELETE FROM invoices WHERE tenant_id = $1`,
	} {
		err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, tn.ID)
			return err
		})
		if err == nil {
			t.Fatalf("o banco aceitou: %s", stmt)
		}
	}
}

// Rollback do caso de uso não deixa evento na outbox.
func TestRollbackDoesNotPublish(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	tn := env.NewTenant(t, "rollback")
	sentinel := errors.New("falha depois do evento")
	err := env.Store.Customers().InTenant(ctx, tn.ID, func(tx customersTx) error {
		if err := tx.Emit(ctx, outbox.New(tn.ID, outbox.EventCustomerCreated, "customer", uuid.New(), map[string]any{}, now)); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err=%v", err)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, tn.ID, outbox.EventCustomerCreated); n != 0 {
		t.Fatalf("evento persistido apesar do rollback: %d", n)
	}
}
