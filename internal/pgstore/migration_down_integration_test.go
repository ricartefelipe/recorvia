//go:build integration

package pgstore_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/db"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// A seção Down da migration de cobrança e notificações remove tudo o que a
// Up criou e a Up pode ser reaplicada em seguida (executado numa transação
// desfeita ao fim, como dono das tabelas).
func TestCollectionsMigrationDownAndUpAgain(t *testing.T) {
	ctx := context.Background()
	raw, err := db.Migrations.ReadFile("migrations/00011_cobranca_e_notificacoes.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("migration sem seção Down")
	}
	owner := pgtest.OwnerPool(t)
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tables := []string{"collection_policies", "notification_templates", "notifications", "notification_events", "invoice_delinquencies",
		"collection_suspensions", "webhook_endpoints", "webhook_endpoint_secrets", "webhook_events", "webhook_deliveries", "webhook_delivery_attempts"}
	count := func() int {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = ANY ($1)`, tables).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != len(tables) {
		t.Fatal("tabelas da migration ausentes antes do Down")
	}
	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatalf("Down: %v", err)
	}
	var funcs int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname IN ('app_tenants_for_collections', 'app_tenants_with_due_notifications',
		'app_tenants_with_due_webhook_deliveries')`).Scan(&funcs); err != nil {
		t.Fatal(err)
	}
	if count() != 0 || funcs != 0 {
		t.Fatalf("Down deixou objetos: %d tabelas, %d funções", count(), funcs)
	}
	if _, err := tx.Exec(ctx, strings.TrimPrefix(up, "-- +goose Up")); err != nil {
		t.Fatalf("Up após Down: %v", err)
	}
	if count() != len(tables) {
		t.Fatal("Up após Down não recriou as tabelas")
	}
}

// Históricos são imutáveis para a aplicação e o segredo de webhook só tem a
// validade alterável; vínculos entre tenants são recusados pelas FKs compostas.
func TestCollectionsTablesPrivilegesAndCompositeKeys(t *testing.T) {
	ctx := context.Background()
	app := pgtest.AppDB(t, 2)
	for _, table := range []string{"notification_events", "webhook_events", "webhook_delivery_attempts"} {
		for _, priv := range []string{"UPDATE", "DELETE"} {
			var has bool
			if err := app.Pool().QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, table, priv).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has {
				t.Errorf("papel da aplicação tem %s em %s", priv, table)
			}
		}
	}
	for col, want := range map[string]bool{"expires_at": true, "secret_ciphertext": false, "secret_key_id": false, "endpoint_id": false} {
		var has bool
		if err := app.Pool().QueryRow(ctx, `SELECT has_column_privilege(current_user, 'webhook_endpoint_secrets', $1, 'UPDATE')`, col).Scan(&has); err != nil {
			t.Fatal(err)
		}
		if has != want {
			t.Errorf("UPDATE em webhook_endpoint_secrets.%s = %v, esperado %v", col, has, want)
		}
	}
	env := fixture.New(t, 2, now)
	a, b := env.NewTenant(t, "fk-notif-a"), env.NewTenant(t, "fk-notif-b")
	custA := env.NewCustomer(t, a)
	err := env.DB.InTenantTx(ctx, b.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO notifications (id, tenant_id, business_key, template_key, subject, status, status_reason, send_after,
			max_attempts, customer_id, created_at, updated_at) VALUES ($1, $2, 'k-cross', 'invoice_issued', 'x', 'suppressed', 'no_recipient', now(), 1, $3, now(), now())`,
			ids.New(), b.ID, custA.ID)
		return err
	})
	if !postgres.IsForeignKeyViolation(err) {
		t.Fatalf("notificação com cliente de outro tenant: %v", err)
	}
}
