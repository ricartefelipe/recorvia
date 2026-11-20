//go:build integration

package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

var now = time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)

// Toda tabela com tenant_id deve ter RLS habilitada e política de isolamento.
func TestEveryTenantTableHasRLS(t *testing.T) {
	ctx := context.Background()
	owner := pgtest.OwnerPool(t)
	rows, err := owner.Query(ctx, `
		SELECT c.relname, c.relrowsecurity, EXISTS (SELECT 1 FROM pg_policies p WHERE p.tablename = c.relname)
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'r'
		   AND EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name string
		var rls, policy bool
		if err := rows.Scan(&name, &rls, &policy); err != nil {
			t.Fatal(err)
		}
		n++
		if !rls || !policy {
			t.Errorf("tabela %s sem RLS (rls=%v, política=%v)", name, rls, policy)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 30 {
		t.Fatalf("esperava ao menos 30 tabelas de tenant, encontrei %d", n)
	}
}

// O papel da aplicação não pode alterar nem apagar registros imutáveis.
func TestAppRoleCannotMutateImmutableTables(t *testing.T) {
	ctx := context.Background()
	db := pgtest.AppDB(t, 2)
	for _, table := range []string{"ledger_entries", "ledger_transactions", "audit_log", "payments", "plan_price_versions", "invoice_items"} {
		for _, priv := range []string{"UPDATE", "DELETE"} {
			var has bool
			if err := db.Pool().QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, table, priv).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has {
				t.Errorf("papel da aplicação tem %s em %s", priv, table)
			}
		}
	}
}

func TestLedgerRejectsUnbalancedAndImmutableAtDatabase(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	tn := env.NewTenant(t, "ledger")
	var accountID uuid.UUID
	txID := ids.New()
	err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE tenant_id = $1 AND code = 'cash'`, tn.ID).Scan(&accountID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, tenant_id, kind, description, occurred_at, reference_type, reference_id, idempotency_key)
			VALUES ($1, $2, 'teste', 'desbalanceado', now(), 'teste', $1, $3)`, txID, tn.ID, "teste:"+txID.String()); err != nil {
			return err
		}
		for _, dir := range []string{"debit", "credit"} {
			amount := 100
			if dir == "credit" {
				amount = 99
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (id, tenant_id, transaction_id, account_id, direction, amount, currency)
				VALUES ($1, $2, $3, $4, $5, $6, 'BRL')`, ids.New(), tn.ID, txID, accountID, dir, amount); err != nil {
				return err
			}
		}
		return nil
	})
	if !postgres.IsRaisedException(err) {
		t.Fatalf("lançamento desbalanceado deveria falhar no commit: %v", err)
	}
	err = env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE ledger_accounts SET name = 'x' WHERE tenant_id = $1`, tn.ID)
		return err
	})
	if !postgres.IsInsufficientPrivilege(err) {
		t.Fatalf("contas do ledger não devem ser alteradas pela aplicação: %v", err)
	}
}

// FKs compostas impedem vincular entidades de tenants diferentes, mesmo que
// a consulta informe IDs válidos do outro tenant.
func TestCompositeForeignKeysBlockCrossTenantRelations(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	a, b := env.NewTenant(t, "fk-a"), env.NewTenant(t, "fk-b")
	custA := env.NewCustomer(t, a)
	err := env.DB.InTenantTx(ctx, b.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO customer_contacts (id, tenant_id, customer_id, name, email) VALUES ($1, $2, $3, 'Invasor', 'x@y.test')`,
			ids.New(), b.ID, custA.ID)
		return err
	})
	if !postgres.IsForeignKeyViolation(err) {
		t.Fatalf("esperava violação de FK composta, obtive %v", err)
	}
}

// Serviços de um tenant não enxergam recursos de outro (404), inclusive com
// o pool reutilizando a mesma conexão física.
func TestServicesIsolateTenantsOnSharedConnection(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 1, now)
	a, b := env.NewTenant(t, "iso-a"), env.NewTenant(t, "iso-b")
	custA := env.NewCustomer(t, a)
	plan := env.NewFlatPlan(t, a, 10000)
	if _, err := env.Customers.Get(ctx, b.ID, custA.ID); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("tenant B obteve cliente do tenant A: %v", err)
	}
	if _, err := env.Catalog.Plan(ctx, b.ID, plan.ID); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("tenant B obteve plano do tenant A: %v", err)
	}
	pageB, err := env.Customers.List(ctx, b.ID, customersFilter())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range pageB.Items {
		if c.TenantID != b.ID {
			t.Fatalf("lista do tenant B contém cliente de outro tenant")
		}
	}
	if _, err := env.Tenancy.ResolveAccess(ctx, b.ID, a.Owner.ID); !apperr.IsKind(err, apperr.KindForbidden) {
		t.Fatalf("dono do tenant A acessou tenant B: %v", err)
	}
}
