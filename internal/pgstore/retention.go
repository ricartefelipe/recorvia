package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/retention"
)

// Retention implementa retention.Store sobre as funções app_retention_tenants
// (descoberta, sem contexto de tenant) e app_retention_purge (remoção, na
// transação do tenant).
type Retention struct{ s *Store }

// Retention retorna o adaptador do expurgo por idade.
func (s *Store) Retention() *Retention { return &Retention{s: s} }

// Tenants descobre os tenants com dados do tipo vencidos em cutoff.
func (r *Retention) Tenants(ctx context.Context, kind retention.Kind, cutoff time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := r.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		out, err = q.RetentionTenants(ctx, dbgen.RetentionTenantsParams{Kind: string(kind), Cutoff: cutoff, AfterTenant: after,
			MaxTenants: int32(limit)})
		return err
	})
	return out, err
}

// Purge remove um lote do tipo na transação do tenant.
func (r *Retention) Purge(ctx context.Context, tenantID uuid.UUID, kind retention.Kind, cutoff, now time.Time, limit int) (int, bool, error) {
	var row dbgen.RetentionPurgeRow
	err := r.s.inTenant(ctx, tenantID, func(b base) error {
		var err error
		row, err = b.q.RetentionPurge(ctx, dbgen.RetentionPurgeParams{Kind: string(kind), TenantID: tenantID, Cutoff: cutoff, Now: now,
			BatchSize: int32(limit)})
		return err
	})
	return int(row.Purged), row.More, err
}
