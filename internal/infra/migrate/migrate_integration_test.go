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
	// As migrations posteriores são revertidas antes, da mais recente para a
	// mais antiga.
	reverted, err := migrate.DownTo(ctx, env.OwnerURL, 12)
	if err != nil || len(reverted) == 0 || reverted[len(reverted)-1] != "00013_recebiveis_conciliacao.sql" {
		t.Fatalf("reversão: %v %v", reverted, err)
	}
	for _, q := range []string{
		`SELECT to_regclass('public.refunds') IS NULL AND to_regclass('public.disputes') IS NULL AND to_regclass('public.reconciliation_runs') IS NULL
		    AND to_regclass('public.reconciliation_charge_checks') IS NULL`,
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
	if err != nil || len(applied) != len(reverted) {
		t.Fatalf("reaplicação: %v %v", applied, err)
	}
	if !exists(t, pool, `SELECT to_regclass('public.reconciliation_discrepancies') IS NOT NULL
	                      AND to_regclass('public.reconciliation_charge_checks') IS NOT NULL
	                      AND EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'charges_refund_reservation_limit'
	                                     AND pg_get_constraintdef(oid) LIKE '%amount_charged_back%')`) {
		t.Fatal("esquema após o Up")
	}
}

// A migration das métricas de fila é reversível pelo Goose: o Down remove as
// funções agregadas e os índices parciais, sem tocar nas tabelas, e o Up
// seguinte os recria com a execução restrita ao papel da aplicação.
func TestQueueMetricsMigrationDownAndUp(t *testing.T) {
	ctx := context.Background()
	env := pgtest.Start(t)
	pool := pgtest.OwnerPool(t)
	const objects = `(SELECT count(*) FROM pg_proc WHERE proname IN ('app_dead_letter_stats', 'app_notification_backlog_stats',
			'app_webhook_delivery_backlog_stats'))
		+ (SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname IN ('dead_letters_open_idx', 'notifications_failed_idx'))`
	if !exists(t, pool, `SELECT `+objects+` = 5`) {
		t.Fatal("funções e índices da migration ausentes antes do Down")
	}
	// As migrations posteriores, se houver, são revertidas antes.
	reverted, err := migrate.DownTo(ctx, env.OwnerURL, 14)
	if err != nil || len(reverted) == 0 || reverted[len(reverted)-1] != "00015_metricas_de_fila.sql" {
		t.Fatalf("reversão: %v %v", reverted, err)
	}
	for _, q := range []string{
		`SELECT ` + objects + ` = 0`,
		`SELECT to_regclass('public.dead_letters') IS NOT NULL AND to_regclass('public.notifications') IS NOT NULL
		    AND to_regclass('public.webhook_deliveries') IS NOT NULL`,
		`SELECT to_regprocedure('app_outbox_stats()') IS NOT NULL`,
	} {
		if !exists(t, pool, q) {
			t.Fatalf("esquema após o Down: %s", q)
		}
	}
	applied, err := migrate.Up(ctx, env.OwnerURL)
	if err != nil || len(applied) != len(reverted) {
		t.Fatalf("reaplicação: %v %v", applied, err)
	}
	for _, q := range []string{
		`SELECT ` + objects + ` = 5`,
		`SELECT has_function_privilege('recorvia_app', 'app_dead_letter_stats(timestamptz)', 'EXECUTE')
		    AND has_function_privilege('recorvia_app', 'app_notification_backlog_stats(timestamptz, timestamptz)', 'EXECUTE')
		    AND has_function_privilege('recorvia_app', 'app_webhook_delivery_backlog_stats(timestamptz)', 'EXECUTE')`,
		`SELECT NOT EXISTS (SELECT 1 FROM pg_proc p, aclexplode(p.proacl) a
		                     WHERE p.proname IN ('app_dead_letter_stats', 'app_notification_backlog_stats', 'app_webhook_delivery_backlog_stats')
		                       AND a.grantee = 0)`,
	} {
		if !exists(t, pool, q) {
			t.Fatalf("esquema após o Up: %s", q)
		}
	}
}
