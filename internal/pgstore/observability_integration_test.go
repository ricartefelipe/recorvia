//go:build integration

package pgstore_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// As métricas de conexões do servidor (recorvia_db_server_connections e
// recorvia_db_server_max_connections) são lidas com o papel da aplicação, sem
// privilégio de leitura das estatísticas: a contagem precisa incluir as
// sessões de outros papéis e o limite precisa descontar as conexões
// reservadas.
func TestDatabaseConnectionsCountsAllClientsWithAppRole(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	pg := pgtest.Start(t)
	super, err := pgx.Connect(ctx, pg.SuperURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = super.Close(ctx) }()
	const extra = 3
	for range extra {
		c, err := pgx.Connect(ctx, pg.OwnerURL)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close(ctx) }()
	}

	open, available, err := env.Store.DatabaseConnections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Conexão da aplicação, superusuário e as conexões extras do dono.
	if open < extra+2 {
		t.Fatalf("conexões abertas = %d; esperado ao menos %d (sessões de outros papéis incluídas)", open, extra+2)
	}
	var want int64
	if err := super.QueryRow(ctx, `SELECT current_setting('max_connections')::int - current_setting('superuser_reserved_connections')::int
		- current_setting('reserved_connections')::int`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if available != want || available <= 0 {
		t.Fatalf("limite para papéis comuns = %d, esperado %d", available, want)
	}
}
