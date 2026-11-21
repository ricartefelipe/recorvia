//go:build integration

package pgstore_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
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

// Fatura emitida não pode ser reescrita nem excluída (gatilho no banco). O
// papel da aplicação nem tem privilégio de DELETE; o gatilho é exercitado como
// dono das tabelas, que tem todos os privilégios e não está sujeito ao RLS,
// de modo que a recusa vem do próprio gatilho (P0001), com a mensagem dele.
func TestIssuedInvoiceCannotBeRewritten(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	tn := env.NewTenant(t, "guard")
	plan := env.NewFlatPlan(t, tn, 5000)
	cust := env.NewCustomer(t, tn)
	env.NewSubscription(t, tn, cust.ID, plan.ID, nil)
	if _, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	owner := pgtest.OwnerPool(t)
	var issuedID uuid.UUID
	var issuer []byte
	if err := owner.QueryRow(ctx, `SELECT id, issuer_snapshot FROM invoices WHERE tenant_id = $1 AND status = 'open'`, tn.ID).
		Scan(&issuedID, &issuer); err != nil {
		t.Fatal(err)
	}
	// O motor grava o emitente na emissão, como estava a organização.
	if !strings.Contains(string(issuer), `"name": "Org guard"`) && !strings.Contains(string(issuer), `"name":"Org guard"`) {
		t.Fatalf("emitente gravado na emissão: %s", issuer)
	}
	// O papel da aplicação não exclui faturas: falta de privilégio, antes do gatilho.
	err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM invoices WHERE tenant_id = $1 AND id = $2`, tn.ID, issuedID)
		return err
	})
	if !postgres.IsInsufficientPrivilege(err) {
		t.Fatalf("DELETE pelo papel da aplicação deveria faltar privilégio: %v", err)
	}
	cases := []struct {
		name, stmt, message string
	}{
		{"valores", `UPDATE invoices SET total = 1, subtotal = 1, amount_due = 1 WHERE id = $1`, "não pode ser reescrita"},
		{"observação", `UPDATE invoices SET memo = 'observação alterada depois da emissão' WHERE id = $1`, "não pode ser reescrita"},
		{"snapshot do cliente", `UPDATE invoices SET customer_snapshot = '{}' WHERE id = $1`, "não pode ser reescrita"},
		{"criação", `UPDATE invoices SET created_at = created_at - interval '1 day' WHERE id = $1`, "não pode ser reescrita"},
		{"emitente", `UPDATE invoices SET issuer_snapshot = '{"name": "Outra organização"}' WHERE id = $1`, "não pode ser reescrita"},
		{"emitente removido", `UPDATE invoices SET issuer_snapshot = NULL WHERE id = $1`, "não pode ser reescrita"},
		{"exclusão", `DELETE FROM invoices WHERE id = $1`, "não pode ser excluída"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := owner.Exec(ctx, tc.stmt, issuedID)
			var pgErr *pgconn.PgError
			if !postgres.IsRaisedException(err) || !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, tc.message) {
				t.Fatalf("o gatilho deveria recusar com %q: %v", tc.message, err)
			}
		})
	}
	// Fatura cancelada não tem saldo devedor (constraint de consistência).
	if _, err := owner.Exec(ctx, `UPDATE invoices SET status = 'void', voided_at = now() WHERE id = $1`, issuedID); !postgres.IsNamedCheckViolation(err,
		"invoices_due_consistency") {
		t.Fatalf("cancelada com saldo deveria violar a constraint: %v", err)
	}
	// O andamento continua permitido: a situação muda com o saldo coerente.
	if _, err := owner.Exec(ctx, `UPDATE invoices SET status = 'paid', amount_paid = total, amount_due = 0, paid_at = now() WHERE id = $1`,
		issuedID); err != nil {
		t.Fatalf("andamento da fatura emitida recusado: %v", err)
	}
	// Rascunho pode ser alterado e excluído (ramo DELETE que devolve OLD).
	draftID := ids.New()
	if _, err := owner.Exec(ctx, `INSERT INTO invoices (id, tenant_id, customer_id, status, kind, subtotal, total, amount_due, customer_snapshot)
		VALUES ($1, $2, $3, 'draft', 'manual', 500, 500, 500, '{}')`, draftID, tn.ID, cust.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE invoices SET memo = 'rascunho revisado' WHERE id = $1`, draftID); err != nil {
		t.Fatalf("alteração do rascunho: %v", err)
	}
	tag, err := owner.Exec(ctx, `DELETE FROM invoices WHERE id = $1`, draftID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("exclusão do rascunho: %v %v", tag, err)
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
