//go:build integration

package pgstore_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// envOwner abre conexão como dono das tabelas para preparar cenários que a
// aplicação não pode executar (por exemplo, antecipar available_at).
func envOwner(t *testing.T) *pgxpool.Pool { return pgtest.OwnerPool(t) }
