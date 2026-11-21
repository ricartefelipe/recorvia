//go:build integration

package pgstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// historyInvoices é o volume de faturas antigas, pagas antes da posição, de
// cada cenário de desempenho.
const historyInvoices = 3000

// seedPaidHistory grava, como superusuário e sem gatilhos (preparação de
// volume, não regra de negócio), faturas pagas antes de paidBefore com os
// lançamentos de emissão e de recebimento, os pagamentos e uma nota de
// crédito por fatura (só volume, sem lançamento), e atualiza as estatísticas
// das tabelas.
func seedPaidHistory(t *testing.T, env pgtest.Env, tn fixture.Tenant, customerID string, paidBefore time.Time) {
	t.Helper()
	ctx := context.Background()
	app, err := url.Parse(env.AppURL)
	if err != nil {
		t.Fatal(err)
	}
	super, err := url.Parse(env.SuperURL)
	if err != nil {
		t.Fatal(err)
	}
	super.Path = app.Path
	conn, err := pgx.Connect(ctx, super.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }() // conexão de preparação: falha de fechamento não afeta o teste
	stmts := []string{
		`SET session_replication_role = replica`,
		`INSERT INTO invoices (id, tenant_id, customer_id, number, status, kind, issued_at, due_at, subtotal, total, amount_paid, amount_due,
		                       customer_snapshot, paid_at, created_at)
		 SELECT gen_random_uuid(), $1, $2, 'HIST-' || g, 'paid', 'manual', $3::timestamptz - make_interval(hours => g + 2),
		        $3::timestamptz - make_interval(hours => g), 1000, 1000, 1000, 0, '{}', $3::timestamptz - make_interval(hours => g + 1),
		        $3::timestamptz - make_interval(hours => g + 2)
		   FROM generate_series(1, $4::int) g`,
		`INSERT INTO payments (id, tenant_id, invoice_id, customer_id, amount, method, source, paid_at, created_at)
		 SELECT gen_random_uuid(), tenant_id, id, customer_id, 1000, 'pix', 'manual', paid_at, paid_at
		   FROM invoices WHERE tenant_id = $1 AND number LIKE 'HIST-%'`,
		`INSERT INTO ledger_transactions (id, tenant_id, kind, description, occurred_at, reference_type, reference_id, idempotency_key)
		 SELECT gen_random_uuid(), tenant_id, 'invoice.issued', 'Emissão', issued_at, 'invoice', id, 'invoice.issued:' || id
		   FROM invoices WHERE tenant_id = $1 AND number LIKE 'HIST-%'
		 UNION ALL
		 SELECT gen_random_uuid(), tenant_id, 'payment.manual', 'Recebimento', paid_at, 'payment', id, 'payment.manual:' || id
		   FROM payments WHERE tenant_id = $1 AND invoice_id IN (SELECT id FROM invoices WHERE tenant_id = $1 AND number LIKE 'HIST-%')`,
		`INSERT INTO ledger_entries (id, tenant_id, transaction_id, account_id, direction, amount)
		 SELECT gen_random_uuid(), t.tenant_id, t.id, a.id, d.direction, 1000
		   FROM ledger_transactions t
		   CROSS JOIN LATERAL (VALUES
		     (CASE t.reference_type WHEN 'invoice' THEN 'accounts_receivable' ELSE 'cash' END, 'debit'),
		     (CASE t.reference_type WHEN 'invoice' THEN 'billed_revenue' ELSE 'accounts_receivable' END, 'credit')) AS d(code, direction)
		   JOIN ledger_accounts a ON a.tenant_id = t.tenant_id AND a.code = d.code
		  WHERE t.tenant_id = $1 AND t.description IN ('Emissão', 'Recebimento')`,
		`INSERT INTO credit_notes (id, tenant_id, invoice_id, number, amount, reason, created_at)
		 SELECT gen_random_uuid(), tenant_id, id, 'NC-' || number, 1, 'Volume de teste', paid_at
		   FROM invoices WHERE tenant_id = $1 AND number LIKE 'HIST-%'`,
		`ANALYZE invoices, payments, credit_notes, ledger_transactions, ledger_entries, ledger_accounts`,
	}
	for i, q := range stmts {
		var args []any
		switch i {
		case 1:
			args = []any{tn.ID, customerID, paidBefore, historyInvoices}
		case 2, 3, 4, 5:
			args = []any{tn.ID}
		}
		if _, err := conn.Exec(ctx, q, args...); err != nil {
			t.Fatalf("preparação %d: %v", i, err)
		}
	}
}

// literal cita um texto como literal SQL.
func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// planNode é um nó do EXPLAIN (FORMAT JSON).
type planNode struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	IndexName    string     `json:"Index Name"`
	ActualRows   float64    `json:"Actual Rows"`
	ActualLoops  float64    `json:"Actual Loops"`
	Plans        []planNode `json:"Plans"`
}

func (n planNode) walk(fn func(planNode)) {
	fn(n)
	for _, c := range n.Plans {
		c.walk(fn)
	}
}

// A posição de um mês encerrado não percorre o histórico do ledger: com
// milhares de faturas pagas antes da posição, a consulta lê só as partidas
// das faturas que podiam ter saldo, por índice. A execução é medida pelas
// estatísticas da própria transação e pelo plano (EXPLAIN ANALYZE) da mesma
// instrução preparada que a aplicação executa.
func TestDashboardReceivablesAtDoesNotScanLedgerHistory(t *testing.T) {
	ctx := context.Background()
	env := pgtest.Isolated(t)
	fx := fixture.NewOn(t, pgtest.AppDBOf(t, env, 2), now)
	tn := fx.NewTenant(t, "painel-volume")
	plan := fx.NewFlatPlan(t, tn, 10000)
	for range 2 {
		fx.NewSubscription(t, tn, fx.NewCustomer(t, tn).ID, plan.ID, nil)
	}
	if res, err := fx.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil || res.Invoices != 2 {
		t.Fatalf("faturas abertas: %+v %v", res, err)
	}
	seedPaidHistory(t, env, tn, fx.NewCustomer(t, tn).ID.String(), now.Add(-24*time.Hour))

	position := now.Add(time.Hour)
	since := position.AddDate(0, 0, -90)
	err := fx.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		var arEntries int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
		                             WHERE e.tenant_id = $1 AND a.code = 'accounts_receivable'`, tn.ID).Scan(&arEntries); err != nil {
			return err
		}
		if arEntries < 2*historyInvoices {
			t.Fatalf("histórico de contas a receber = %d partidas", arEntries)
		}
		// Estatísticas da transação antes e depois da consulta medida.
		stats := func() (map[string][2]int64, error) {
			rows, err := tx.Query(ctx, `SELECT relname, seq_scan, COALESCE(idx_tup_fetch, 0) + seq_tup_read FROM pg_stat_xact_user_tables
			                             WHERE relname IN ('ledger_entries', 'ledger_transactions', 'payments', 'invoices', 'credit_notes')`)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := map[string][2]int64{}
			for rows.Next() {
				var name string
				var scans, tuples int64
				if err := rows.Scan(&name, &scans, &tuples); err != nil {
					return nil, err
				}
				out[name] = [2]int64{scans, tuples}
			}
			return out, rows.Err()
		}
		before, err := stats()
		if err != nil {
			return err
		}
		got, err := dbgen.New(tx).DashboardReceivablesAt(ctx, dbgen.DashboardReceivablesAtParams{TenantID: tn.ID, PositionAt: &position, Since: &since})
		if err != nil {
			return err
		}
		if got.OpenTotal != 20000 {
			t.Errorf("recebíveis em aberto = %d, esperado 20000 (as duas faturas abertas)", got.OpenTotal)
		}
		after, err := stats()
		if err != nil {
			return err
		}
		for _, rel := range []string{"ledger_entries", "ledger_transactions", "payments", "invoices", "credit_notes"} {
			scans, tuples := after[rel][0]-before[rel][0], after[rel][1]-before[rel][1]
			if scans != 0 || tuples > 50 {
				t.Errorf("%s: %d varreduras sequenciais e %d linhas lidas (histórico de %d faturas)", rel, scans, tuples, historyInvoices)
			}
		}

		var name string
		if err := tx.QueryRow(ctx, `SELECT name FROM pg_prepared_statements WHERE statement LIKE '-- name: DashboardReceivablesAt %'`).Scan(&name); err != nil {
			return err
		}
		// Parâmetros na ordem da instrução gerada: posição, início da janela e organização.
		// EXECUTE não aceita parâmetros da instrução externa: os valores vão
		// como literais tipados.
		args := fmt.Sprintf("(%s::timestamptz, %s::timestamptz, %s::uuid)", literal(position.Format(time.RFC3339Nano)),
			literal(since.Format(time.RFC3339Nano)), literal(tn.ID.String()))
		var raw []byte
		if err := tx.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE `+pgx.Identifier{name}.Sanitize()+args).Scan(&raw); err != nil {
			return err
		}
		var text strings.Builder
		rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE) EXECUTE `+pgx.Identifier{name}.Sanitize()+args)
		if err != nil {
			return err
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			text.WriteString(line + "\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var plans []struct {
			Plan planNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("plano: %v %s", err, raw)
		}
		defer func() {
			if t.Failed() {
				t.Logf("plano:\n%s", text.String())
			}
		}()
		plans[0].Plan.walk(func(n planNode) {
			switch n.RelationName {
			case "ledger_entries", "ledger_transactions", "payments", "invoices", "credit_notes":
			default:
				return
			}
			if n.NodeType == "Seq Scan" || n.IndexName == "ledger_entries_account_idx" || n.IndexName == "ledger_transactions_tenant_time_idx" {
				t.Errorf("plano percorre o histórico: %s em %s (%s)", n.NodeType, n.RelationName, n.IndexName)
			}
			if rows := n.ActualRows * n.ActualLoops; rows > 50 {
				t.Errorf("plano lê %.0f linhas de %s (%s)", rows, n.RelationName, n.IndexName)
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// historyFetched devolve quantas linhas do histórico de assinaturas a
// transação já leu (varredura ou índice).
func historyFetched(ctx context.Context, tx pgx.Tx) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(sum(COALESCE(idx_tup_fetch, 0) + seq_tup_read), 0)::bigint FROM pg_stat_xact_user_tables
	                          WHERE relname = 'subscription_history'`).Scan(&n)
	return n, err
}

// A contagem de assinaturas do mês não consulta o histórico das assinaturas
// encerradas antes do mês: elas não entram em nenhum indicador.
func TestDashboardSubscriptionCountsSkipsSubscriptionsEndedBeforeMonth(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "painel-encerradas")
	plan := a.newFlatPlan(t, tn, 10000)
	const ended = 15
	for range ended {
		s := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
		a.cancelNow(t, tn, s.ID)
	}
	stays := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
	a.Clock.Set(spTime(t, 2026, time.May, 2, 9))
	if d := a.dashboard(t, tn, "2026-04"); d.ActiveSubscriptions != 1 || d.CanceledSubscriptions != 0 || d.NewSubscriptions != 0 {
		t.Fatalf("abril: %+v", d)
	}
	if d := a.dashboard(t, tn, "2026-03"); d.ActiveSubscriptions != 1 || d.CanceledSubscriptions != ended || d.NewSubscriptions != ended+1 {
		t.Fatalf("março: ativas=%d canceladas=%d novas=%d", d.ActiveSubscriptions, d.CanceledSubscriptions, d.NewSubscriptions)
	}

	from, to := spTime(t, 2026, time.April, 1, 0), spTime(t, 2026, time.May, 1, 0)
	position := to.Add(-time.Microsecond)
	a.inTenant(t, tn.ID, func(tx pgx.Tx) error {
		before, err := historyFetched(ctx, tx)
		if err != nil {
			return err
		}
		counts, err := dbgen.New(tx).DashboardSubscriptionCounts(ctx, dbgen.DashboardSubscriptionCountsParams{TenantID: tn.ID, FromTs: from,
			ToTs: &to, PositionAt: &position})
		if err != nil {
			return err
		}
		after, err := historyFetched(ctx, tx)
		if err != nil {
			return err
		}
		if counts.ActiveAtPosition != 1 || counts.ActiveAtStart != 1 {
			t.Errorf("contagens de abril: %+v", counts)
		}
		// Só a assinatura que permanece consulta o histórico (um registro
		// por instante: início do mês e posição).
		if n := after - before; n > 4 {
			t.Errorf("linhas do histórico lidas = %d; as %d encerradas em março não deveriam ser consultadas", n, ended)
		}
		// O contador da transação mede as leituras: ler o histórico inteiro
		// soma todas as linhas.
		var total int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM subscription_history WHERE tenant_id = $1 AND event <> ''`, tn.ID).Scan(&total); err != nil {
			return err
		}
		final, err := historyFetched(ctx, tx)
		if err != nil {
			return err
		}
		if total < 2*ended || final-after < total {
			t.Errorf("contador do histórico: %d linhas lidas para %d registros", final-after, total)
		}
		return nil
	})
	if got, err := a.Subscriptions.Get(ctx, tn.ID, stays.ID); err != nil || got.Status != "active" {
		t.Fatalf("assinatura que permanece: %v %v", got.Status, err)
	}
}
