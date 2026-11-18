//go:build integration

package migrate_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ricartefelipe/recorvia/internal/infra/migrate"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

func exists(t *testing.T, pool *pgxpool.Pool, query string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), query).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// A migration de recebíveis e conciliação é reversível: o Down remove
// tabelas, colunas e funções e restaura as constraints anteriores; o Up
// seguinte recria tudo.
func TestReceivablesMigrationDownAndUp(t *testing.T) {
	ctx := context.Background()
	env := pgtest.Start(t)
	pool := pgtest.OwnerPool(t)
	reverted, err := migrate.DownTo(ctx, env.OwnerURL, 9)
	if err != nil || len(reverted) != 1 || reverted[0] != "00013_recebiveis_conciliacao.sql" {
		t.Fatalf("reversão: %v %v", reverted, err)
	}
	for _, q := range []string{
		`SELECT to_regclass('public.refunds') IS NULL AND to_regclass('public.disputes') IS NULL AND to_regclass('public.reconciliation_runs') IS NULL`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'charges' AND column_name = 'amount_refund_pending')`,
		`SELECT to_regprocedure('app_tenants_due_reconciliation(timestamptz, integer)') IS NULL`,
		`SELECT pg_get_constraintdef(oid) LIKE '%create_charge%refund%' AND pg_get_constraintdef(oid) NOT LIKE '%cancel_charge%'
		   FROM pg_constraint WHERE conname = 'payment_operations_kind_check'`,
	} {
		if !exists(t, pool, q) {
			t.Fatalf("esquema após o Down: %s", q)
		}
	}
	applied, err := migrate.Up(ctx, env.OwnerURL)
	if err != nil || len(applied) != 1 {
		t.Fatalf("reaplicação: %v %v", applied, err)
	}
	if !exists(t, pool, `SELECT to_regclass('public.reconciliation_discrepancies') IS NOT NULL
	                      AND EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'charges_refund_reservation_limit')`) {
		t.Fatal("esquema após o Up")
	}
}
