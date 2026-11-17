// Package migrate cria os papéis do banco (bootstrap) e aplica as migrations
// Goose embutidas. O bootstrap exige um superusuário e é executado uma vez por
// ambiente; as migrations rodam como recorvia_owner; a aplicação conecta como
// recorvia_app.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // driver database/sql exigido pelo Goose
	"github.com/pressly/goose/v3"

	"github.com/ricartefelipe/recorvia/db"
)

// Nomes fixos dos papéis; as migrations concedem privilégios a eles.
const (
	OwnerRole = "recorvia_owner"
	AppRole   = "recorvia_app"
)

// BootstrapOptions define senhas e banco de destino.
type BootstrapOptions struct {
	Database      string
	OwnerPassword string
	AppPassword   string
}

func (o BootstrapOptions) validate() error {
	var errs []error
	if o.Database == "" {
		errs = append(errs, errors.New("nome do banco obrigatório"))
	}
	for name, p := range map[string]string{"owner": o.OwnerPassword, "app": o.AppPassword} {
		if len(p) < 12 {
			errs = append(errs, fmt.Errorf("senha do papel %s deve ter ao menos 12 caracteres", name))
		}
		if strings.ContainsAny(p, "'\\\x00") {
			errs = append(errs, fmt.Errorf("senha do papel %s contém caracteres não permitidos", name))
		}
	}
	return errors.Join(errs...)
}

// literal produz um literal SQL seguro para senhas já validadas (sem aspas
// simples, barra invertida ou NUL). Comandos DDL não aceitam parâmetros.
func literal(s string) string { return "'" + s + "'" }

// Bootstrap cria (ou atualiza a senha de) recorvia_owner e recorvia_app, cria
// o banco com dono recorvia_owner e restringe privilégios padrão. É
// idempotente.
func Bootstrap(ctx context.Context, superURL string, o BootstrapOptions) (err error) {
	if err := o.validate(); err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, superURL)
	if err != nil {
		return fmt.Errorf("bootstrap: conexão de superusuário: %w", err)
	}
	defer func() { err = errors.Join(err, conn.Close(context.WithoutCancel(ctx))) }()

	roles := []struct {
		name, password, attrs string
	}{
		{OwnerRole, o.OwnerPassword, "LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION"},
		{AppRole, o.AppPassword, "LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION NOINHERIT"},
	}
	for _, r := range roles {
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", r.name).Scan(&exists); err != nil {
			return fmt.Errorf("bootstrap: consulta de papel: %w", err)
		}
		verb := "CREATE"
		if exists {
			verb = "ALTER"
		}
		stmt := fmt.Sprintf("%s ROLE %s %s PASSWORD %s", verb, pgx.Identifier{r.name}.Sanitize(), r.attrs, literal(r.password))
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("bootstrap: papel %s: %w", r.name, err)
		}
	}

	var dbExists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", o.Database).Scan(&dbExists); err != nil {
		return fmt.Errorf("bootstrap: consulta de banco: %w", err)
	}
	dbIdent := pgx.Identifier{o.Database}.Sanitize()
	if !dbExists {
		if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s OWNER %s", dbIdent, OwnerRole)); err != nil {
			return fmt.Errorf("bootstrap: criação do banco: %w", err)
		}
	}
	for _, stmt := range []string{
		fmt.Sprintf("REVOKE ALL ON DATABASE %s FROM PUBLIC", dbIdent),
		fmt.Sprintf("GRANT CONNECT, TEMPORARY ON DATABASE %s TO %s", dbIdent, AppRole),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", dbIdent, OwnerRole),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("bootstrap: privilégios do banco: %w", err)
		}
	}

	cfg, err := pgx.ParseConfig(superURL)
	if err != nil {
		return fmt.Errorf("bootstrap: URL: %w", err)
	}
	cfg.Database = o.Database
	target, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("bootstrap: conexão ao banco de destino: %w", err)
	}
	defer func() { err = errors.Join(err, target.Close(context.WithoutCancel(ctx))) }()
	for _, stmt := range []string{
		"REVOKE ALL ON SCHEMA public FROM PUBLIC",
		fmt.Sprintf("GRANT USAGE, CREATE ON SCHEMA public TO %s", OwnerRole),
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", AppRole),
	} {
		if _, err := target.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("bootstrap: privilégios do schema: %w", err)
		}
	}
	return nil
}

func provider(sqlDB *sql.DB) (*goose.Provider, error) {
	fsys, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, sqlDB, fsys)
}

// Up aplica todas as migrations pendentes como recorvia_owner.
func Up(ctx context.Context, ownerURL string) (applied []string, err error) {
	sqlDB, err := sql.Open("pgx", ownerURL)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, sqlDB.Close()) }()
	p, err := provider(sqlDB)
	if err != nil {
		return nil, err
	}
	results, err := p.Up(ctx)
	applied = make([]string, 0, len(results))
	for _, r := range results {
		applied = append(applied, r.Source.Path)
	}
	if err != nil {
		return applied, fmt.Errorf("migrate: %w", err)
	}
	return applied, nil
}

// Status descreve cada migration e se foi aplicada.
func Status(ctx context.Context, ownerURL string) (lines []string, err error) {
	sqlDB, err := sql.Open("pgx", ownerURL)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, sqlDB.Close()) }()
	p, err := provider(sqlDB)
	if err != nil {
		return nil, err
	}
	st, err := p.Status(ctx)
	if err != nil {
		return nil, err
	}
	lines = make([]string, 0, len(st))
	for _, s := range st {
		lines = append(lines, fmt.Sprintf("%-40s %s", s.Source.Path, s.State))
	}
	return lines, nil
}
