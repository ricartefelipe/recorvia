//go:build integration

// Package pgtest inicia um PostgreSQL real com Testcontainers, executa o
// bootstrap de papéis e as migrations, e fornece conexões com o papel da
// aplicação (recorvia_app) para testes de integração e de isolamento.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ricartefelipe/recorvia/internal/infra/migrate"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
)

// Image é a imagem usada nos testes, igual à do Docker Compose.
const Image = "postgres:18.7-alpine"

const (
	superPassword = "super-secret-test"
	ownerPassword = "owner-secret-test"
	appPassword   = "app-secret-test-1"
	database      = "recorvia"
)

// Env descreve o banco iniciado.
type Env struct {
	SuperURL string
	OwnerURL string
	AppURL   string
	// ContainerID e DockerHost identificam o container e o daemon que o
	// executa (usados pelos testes que rodam ferramentas do PostgreSQL dentro
	// dele com o docker CLI, como pg_dump e pg_restore). O daemon pode diferir
	// do contexto atual do docker CLI.
	ContainerID string
	DockerHost  string
}

var (
	once    sync.Once
	shared  Env
	initErr error
)

// Start inicia (uma vez por processo de teste) o container e aplica as
// migrations. O container é encerrado pelo Ryuk do Testcontainers ao fim do
// processo.
func Start(t testing.TB) Env {
	t.Helper()
	once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		ctr, err := tcpostgres.Run(ctx, Image,
			tcpostgres.WithDatabase("postgres"),
			tcpostgres.WithUsername("postgres"),
			tcpostgres.WithPassword(superPassword),
			tcpostgres.BasicWaitStrategies(),
		)
		if err != nil {
			initErr = fmt.Errorf("pgtest: container: %w", err)
			return
		}
		superURL, err := ctr.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			initErr = err
			return
		}
		if err := migrate.Bootstrap(ctx, superURL, migrate.BootstrapOptions{
			Database: database, OwnerPassword: ownerPassword, AppPassword: appPassword,
		}); err != nil {
			initErr = err
			return
		}
		ownerURL, err := withCredentials(superURL, migrate.OwnerRole, ownerPassword)
		if err != nil {
			initErr = err
			return
		}
		appURL, err := withCredentials(superURL, migrate.AppRole, appPassword)
		if err != nil {
			initErr = err
			return
		}
		cli, err := testcontainers.NewDockerClientWithOpts(ctx)
		if err != nil {
			initErr = fmt.Errorf("pgtest: cliente docker: %w", err)
			return
		}
		dockerHost := cli.DaemonHost()
		if err := cli.Close(); err != nil {
			initErr = err
			return
		}
		shared = Env{SuperURL: superURL, OwnerURL: ownerURL, AppURL: appURL, ContainerID: ctr.GetContainerID(), DockerHost: dockerHost}
		if _, err := migrate.Up(ctx, shared.OwnerURL); err != nil {
			initErr = err
		}
	})
	if initErr != nil {
		t.Fatalf("pgtest: %v", initErr)
	}
	return shared
}

func withCredentials(raw, user, password string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("pgtest: URL do container: %w", err)
	}
	u.User = url.UserPassword(user, password)
	u.Path = "/" + database
	return u.String(), nil
}

// AppDB abre um pool com o papel da aplicação. maxConns=1 força a
// reutilização da mesma conexão entre transações.
func AppDB(t testing.TB, maxConns int32) *postgres.DB {
	t.Helper()
	env := Start(t)
	cfg, err := pgxpool.ParseConfig(env.AppURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return postgres.New(pool)
}

// OwnerPool abre um pool como dono das tabelas, para preparar cenários que a
// aplicação não pode executar (por exemplo, inspecionar dados de vários
// tenants em asserções).
func OwnerPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	env := Start(t)
	pool, err := pgxpool.New(context.Background(), env.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
