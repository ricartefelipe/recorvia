package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/tenantscan"
)

// Store descobre os tenants com dados vencidos e remove um lote por vez.
type Store interface {
	// Tenants devolve, em ordem crescente de ID e a partir de after
	// (exclusivo), até limit tenants com dados do tipo vencidos em cutoff.
	Tenants(ctx context.Context, kind Kind, cutoff time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error)
	// Purge remove, na transação do tenant, até limit registros do tipo
	// vencidos em cutoff. more indica que pode haver mais registros vencidos.
	Purge(ctx context.Context, tenantID uuid.UUID, kind Kind, cutoff, now time.Time, limit int) (purged int, more bool, err error)
}

// Result resume uma execução.
type Result struct {
	// Purged conta os registros removidos por tipo (todos os tipos aparecem,
	// inclusive com zero).
	Purged map[Kind]int
	// Incomplete lista os tipos interrompidos pelo orçamento ou pelo limite
	// de lotes com registros vencidos restantes (continuam na execução
	// seguinte).
	Incomplete []Kind
	// Failed lista os tipos com erro em algum tenant ou na descoberta.
	Failed []Kind
}

// Total soma os registros removidos.
func (r Result) Total() int {
	n := 0
	for _, v := range r.Purged {
		n += v
	}
	return n
}

// Service executa o expurgo por idade.
type Service struct {
	store     Store
	clock     clock.Clock
	policy    Policy
	log       *slog.Logger
	rotations map[Kind]*tenantscan.Rotation
}

// NewService cria o serviço; a política é validada aqui.
func NewService(store Store, c clock.Clock, p Policy, log *slog.Logger) (*Service, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	rotations := map[Kind]*tenantscan.Rotation{}
	for _, k := range Kinds() {
		rotations[k] = &tenantscan.Rotation{}
	}
	return &Service{store: store, clock: c, policy: p, log: log.With("component", "retention"), rotations: rotations}, nil
}

// Policy devolve a política em uso.
func (s *Service) Policy() Policy { return s.policy }

// Run executa uma passagem por todos os tipos. O orçamento da execução é
// dividido igualmente entre os tipos, de modo que o acúmulo de um não impede
// os demais; dentro de cada tipo os tenants são visitados em rodízio. Erros
// de um tenant ou de um tipo não interrompem os outros: o resultado traz o
// que foi removido e o erro reúne as falhas.
func (s *Service) Run(ctx context.Context) (Result, error) {
	res := Result{Purged: map[Kind]int{}}
	kinds := Kinds()
	slice := s.policy.RunBudget / time.Duration(len(kinds))
	var errs []error
	for _, k := range kinds {
		res.Purged[k] = 0
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		now := s.clock.Now()
		n, complete, err := s.runKind(ctx, k, now, now.Add(slice))
		res.Purged[k] = n
		if !complete {
			res.Incomplete = append(res.Incomplete, k)
		}
		if err != nil {
			res.Failed = append(res.Failed, k)
			errs = append(errs, fmt.Errorf("%s: %w", k, err))
		}
	}
	if total := res.Total(); total > 0 || len(res.Incomplete) > 0 {
		attrs := []any{slog.Int("total", total)}
		for _, k := range kinds {
			if res.Purged[k] > 0 {
				attrs = append(attrs, slog.Int(string(k), res.Purged[k]))
			}
		}
		if len(res.Incomplete) > 0 {
			attrs = append(attrs, slog.Any("incomplete", res.Incomplete))
		}
		s.log.InfoContext(ctx, "expurgo por retenção executado", attrs...)
	}
	return res, errors.Join(errs...)
}

// runKind remove os registros vencidos de um tipo em todos os tenants, até
// end. complete é falso quando o orçamento ou o limite de lotes interrompeu
// a passagem com registros vencidos restantes.
func (s *Service) runKind(ctx context.Context, k Kind, now, end time.Time) (purged int, complete bool, err error) {
	cutoff := now.Add(-s.policy.For(k))
	complete = true
	var errs []error
	page := func(ctx context.Context, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
		return s.store.Tenants(ctx, k, cutoff, after, limit)
	}
	more := func() bool {
		if s.clock.Now().Before(end) {
			return true
		}
		complete = false
		return false
	}
	scanErr := s.rotations[k].Each(ctx, s.policy.TenantPageSize, page, more, func(t uuid.UUID) {
		n, done, err := s.purgeTenant(ctx, k, t, cutoff, now, end)
		purged += n
		if !done {
			complete = false
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", t, err))
		}
	})
	if scanErr != nil {
		errs = append(errs, scanErr)
	}
	return purged, complete, errors.Join(errs...)
}

// purgeTenant remove lotes de um tenant até não restar registro vencido, o
// orçamento acabar ou o limite de lotes ser atingido. done é falso nos dois
// últimos casos.
func (s *Service) purgeTenant(ctx context.Context, k Kind, tenantID uuid.UUID, cutoff, now, end time.Time) (purged int, done bool, err error) {
	for range s.policy.MaxBatchesPerTenant {
		if err := ctx.Err(); err != nil {
			return purged, false, err
		}
		n, more, err := s.store.Purge(ctx, tenantID, k, cutoff, now, s.policy.BatchSize)
		if err != nil {
			return purged, false, err
		}
		purged += n
		if !more {
			return purged, true, nil
		}
		if !s.clock.Now().Before(end) {
			return purged, false, nil
		}
	}
	return purged, false, nil
}
