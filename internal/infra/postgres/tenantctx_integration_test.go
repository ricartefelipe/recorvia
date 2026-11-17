//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

func insertTenant(ctx context.Context, t *testing.T, db *postgres.DB, id uuid.UUID, slug string) {
	t.Helper()
	err := db.InTenantTx(ctx, id, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $3)`, id, slug, "Org "+slug)
		return err
	})
	if err != nil {
		t.Fatalf("insert tenant %s: %v", slug, err)
	}
}

// TestAppRoleHasNoPrivilegedAttributes garante que a aplicação não conecta
// como superusuário, não ignora RLS e não é dona das tabelas.
func TestAppRoleHasNoPrivilegedAttributes(t *testing.T) {
	ctx := context.Background()
	db := pgtest.AppDB(t, 2)
	var super, bypass bool
	var user string
	err := db.Pool().QueryRow(ctx, `SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&user, &super, &bypass)
	if err != nil {
		t.Fatal(err)
	}
	if user != "recorvia_app" || super || bypass {
		t.Fatalf("papel=%s super=%v bypassrls=%v", user, super, bypass)
	}
	var owned int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = current_user`).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if owned != 0 {
		t.Fatalf("papel da aplicação é dono de %d tabelas", owned)
	}
	if _, err := db.Pool().Exec(ctx, `CREATE TABLE intrusa (id int)`); !postgres.IsInsufficientPrivilege(err) {
		t.Fatalf("papel da aplicação não deveria criar tabelas: %v", err)
	}
}

// TestTenantContextIsolationAndConnectionReuse usa um pool de uma conexão
// para provar que o contexto definido numa transação não vaza para a
// próxima transação na mesma conexão física.
func TestTenantContextIsolationAndConnectionReuse(t *testing.T) {
	ctx := context.Background()
	db := pgtest.AppDB(t, 1)
	a, b := uuid.New(), uuid.New()
	insertTenant(ctx, t, db, a, "org-a-"+a.String()[:8])
	insertTenant(ctx, t, db, b, "org-b-"+b.String()[:8])

	for _, tenant := range []uuid.UUID{a, b} {
		err := db.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id FROM tenants`)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			if err != nil {
				return err
			}
			if len(ids) != 1 || ids[0] != tenant {
				t.Errorf("tenant %s enxerga %v", tenant, ids)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Mesma conexão física, sem contexto: nada visível e configuração vazia.
	err := db.InTx(ctx, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("sem contexto de tenant, %d linhas visíveis", n)
		}
		var current *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT app_current_tenant()`).Scan(&current); err != nil {
			return err
		}
		if current != nil {
			t.Errorf("contexto vazou para a conexão reutilizada: %v", *current)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Gravar com tenant diferente do contexto é negado pela política.
	err = db.InTenantTx(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET name = 'invasão' WHERE id = $1`, b)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE tenants SET name = 'invasão' WHERE id = $1`, b)
		if err == nil && tag.RowsAffected() != 0 {
			t.Errorf("atualizou linha de outro tenant")
		}
		_, err = tx.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1, 'org-x-forjada', 'Forjada')`, uuid.New())
		return err
	})
	if !postgres.IsInsufficientPrivilege(err) {
		t.Fatalf("esperava violação de política RLS, obtive %v", err)
	}
}

func TestMissingTenantIsRejected(t *testing.T) {
	db := pgtest.AppDB(t, 1)
	called := false
	err := db.InTenantTx(context.Background(), uuid.Nil, func(pgx.Tx) error { called = true; return nil })
	if !errors.Is(err, postgres.ErrMissingTenant) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

// TestRollbackOnErrorAndCanceledContext garante que erro ou cancelamento
// descartam a transação e que a conexão continua utilizável.
func TestRollbackOnErrorAndCanceledContext(t *testing.T) {
	ctx := context.Background()
	db := pgtest.AppDB(t, 1)
	id := uuid.New()
	sentinel := errors.New("falha de negócio")
	err := db.InTenantTx(ctx, id, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1, $2, 'Temp')`, id, "tmp-"+id.String()[:8]); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err=%v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	err = db.InTenantTx(cctx, id, func(tx pgx.Tx) error {
		cancel()
		_, err := tx.Exec(cctx, `SELECT 1`)
		return err
	})
	if err == nil {
		t.Fatal("transação com contexto cancelado deveria falhar")
	}
	err = db.InTenantTx(ctx, id, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("rollback não aconteceu: %d linhas", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("conexão não utilizável após cancelamento: %v", err)
	}
}

func TestSlugAvailabilityDoesNotExposeTenants(t *testing.T) {
	ctx := context.Background()
	db := pgtest.AppDB(t, 1)
	id := uuid.New()
	slug := "slug-" + id.String()[:8]
	insertTenant(ctx, t, db, id, slug)
	err := db.InTx(ctx, func(tx pgx.Tx) error {
		var free bool
		if err := tx.QueryRow(ctx, `SELECT app_tenant_slug_available($1)`, slug).Scan(&free); err != nil {
			return err
		}
		if free {
			t.Error("slug existente informado como disponível")
		}
		return tx.QueryRow(ctx, `SELECT app_tenant_slug_available('slug-inexistente-x')`).Scan(&free)
	})
	if err != nil {
		t.Fatal(err)
	}
}
