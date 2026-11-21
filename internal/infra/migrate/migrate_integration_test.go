//go:build integration

package migrate_test

import (
	"context"
	"slices"
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
// seguinte recria tudo. As migrations posteriores (como a 00014, de retenção
// e anonimização) são revertidas antes dela, pelo Goose, e reaplicadas depois.
func TestReceivablesMigrationDownAndUp(t *testing.T) {
	ctx := context.Background()
	env := pgtest.Start(t)
	pool := pgtest.OwnerPool(t)
	reverted, err := migrate.DownTo(ctx, env.OwnerURL, 12)
	if err != nil || len(reverted) == 0 || reverted[len(reverted)-1] != "00013_recebiveis_conciliacao.sql" ||
		!slices.Contains(reverted, "00014_retencao_e_anonimizacao.sql") {
		t.Fatalf("reversão: %v %v", reverted, err)
	}
	for _, q := range []string{
		`SELECT to_regclass('public.refunds') IS NULL AND to_regclass('public.disputes') IS NULL AND to_regclass('public.reconciliation_runs') IS NULL
		    AND to_regclass('public.reconciliation_charge_checks') IS NULL`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'charges' AND column_name = 'amount_refund_pending')`,
		`SELECT to_regprocedure('app_tenants_due_reconciliation(timestamptz, integer)') IS NULL`,
		`SELECT to_regprocedure('app_retention_purge(text, uuid, timestamptz, timestamptz, integer)') IS NULL
		    AND to_regprocedure('app_retention_tenants(text, timestamptz, uuid, integer)') IS NULL
		    AND to_regprocedure('app_reject_mutation_unless_purge()') IS NULL`,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE column_name IN ('content_purged_at', 'payload_purged_at', 'events_purged_at'))`,
		`SELECT pg_get_constraintdef(oid) LIKE '%create_charge%refund%' AND pg_get_constraintdef(oid) NOT LIKE '%cancel_charge%'
		   FROM pg_constraint WHERE conname = 'payment_operations_kind_check'`,
	} {
		if !exists(t, pool, q) {
			t.Fatalf("esquema após o Down: %s", q)
		}
	}
	applied, err := migrate.Up(ctx, env.OwnerURL)
	if err != nil || len(applied) != len(reverted) {
		t.Fatalf("reaplicação: %v %v", applied, err)
	}
	if !exists(t, pool, `SELECT to_regclass('public.reconciliation_discrepancies') IS NOT NULL
	                      AND to_regclass('public.reconciliation_charge_checks') IS NOT NULL
	                      AND EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'charges_refund_reservation_limit'
	                                     AND pg_get_constraintdef(oid) LIKE '%amount_charged_back%')
	                      AND to_regprocedure('app_retention_purge(text, uuid, timestamptz, timestamptz, integer)') IS NOT NULL`) {
		t.Fatal("esquema após o Up")
	}
}
