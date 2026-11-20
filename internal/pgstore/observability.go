package pgstore

import (
	"context"

	"github.com/ricartefelipe/recorvia/internal/dbgen"
)

// DatabaseConnections retorna as conexões de clientes abertas no servidor
// PostgreSQL (todas as aplicações) e o limite disponível para papéis comuns,
// para as métricas de saturação do banco.
func (s *Store) DatabaseConnections(ctx context.Context) (open, available int64, err error) {
	err = s.inGlobal(ctx, func(q *dbgen.Queries) error {
		r, err := q.DatabaseConnectionStats(ctx)
		open, available = r.Connections, r.AvailableConnections
		return err
	})
	return open, available, err
}
