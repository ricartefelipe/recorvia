package catalog

import (
	"context"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// Tx são as operações dentro da transação do tenant.
type Tx interface {
	InsertProduct(ctx context.Context, p Product) error
	Product(ctx context.Context, id uuid.UUID, forUpdate bool) (Product, error)
	UpdateProduct(ctx context.Context, p Product) error
	// Products, Metrics e Plans devolvem uma página da listagem (limite e
	// cursor opaco; ADR-085).
	Products(ctx context.Context, status Status, limit int, cursor string) (Page[Product], error)

	InsertMetric(ctx context.Context, m Metric) error
	Metric(ctx context.Context, id uuid.UUID) (Metric, error)
	Metrics(ctx context.Context, limit int, cursor string) (Page[Metric], error)

	InsertPlan(ctx context.Context, p Plan) error
	Plan(ctx context.Context, id uuid.UUID, forUpdate bool) (Plan, error)
	UpdatePlan(ctx context.Context, p Plan) error
	Plans(ctx context.Context, status Status, productID *uuid.UUID, limit int, cursor string) (Page[Plan], error)

	InsertPriceVersion(ctx context.Context, pv PriceVersion) error
	PriceVersions(ctx context.Context, planID uuid.UUID) ([]PriceVersion, error)
	PriceVersion(ctx context.Context, id uuid.UUID) (PriceVersion, error)
	MaxPriceVersion(ctx context.Context, planID uuid.UUID) (int, error)

	Audit(ctx context.Context, e audit.Entry) error
}

// Page é uma página de uma listagem do catálogo; NextCursor vazio indica a
// última página.
type Page[T any] struct {
	Items      []T
	NextCursor string
}

// Store abre transações do tenant.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
}

// Service implementa os casos de uso do catálogo.
type Service struct {
	store Store
	clock clock.Clock
}

// NewService cria o serviço.
func NewService(store Store, c clock.Clock) *Service { return &Service{store: store, clock: c} }

// CreateProduct cadastra um produto.
func (s *Service) CreateProduct(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, in ProductInput) (Product, error) {
	now := s.clock.Now()
	p, err := NewProduct(ids.New(), tenantID, in, now)
	if err != nil {
		return Product{}, err
	}
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if err := tx.InsertProduct(ctx, p); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "catalog.product_created", "product", p.ID.String(), nil))
	})
	return p, err
}

// UpdateProduct altera nome, descrição e situação de um produto.
func (s *Service) UpdateProduct(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, in ProductInput, status Status, expectedVersion int) (Product, error) {
	var p Product
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if p, err = tx.Product(ctx, id, true); err != nil {
			return err
		}
		if p.Version != expectedVersion {
			return apperr.PreconditionFailed("version_mismatch", "O produto foi alterado por outra pessoa. Recarregue e tente novamente.")
		}
		upd, err := NewProduct(p.ID, tenantID, in, p.CreatedAt)
		if err != nil {
			return err
		}
		if status != Active && status != Archived {
			return apperr.Invalid("invalid_status", "Situação inválida.", apperr.FieldError{Field: "status", Message: "Use active ou archived."})
		}
		p.Name, p.Description, p.Status = upd.Name, upd.Description, status
		if err := tx.UpdateProduct(ctx, p); err != nil {
			return err
		}
		p.Version++
		return tx.Audit(ctx, audit.New(tenantID, s.clock.Now(), actor, "catalog.product_updated", "product", id.String(), map[string]any{"status": status}))
	})
	return p, err
}

// Products lista produtos, dos criados mais recentemente para os mais
// antigos, em páginas por cursor.
func (s *Service) Products(ctx context.Context, tenantID uuid.UUID, status Status, limit int, cursor string) (Page[Product], error) {
	var out Page[Product]
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		out, err = tx.Products(ctx, status, limit, cursor)
		return err
	})
	return out, err
}

// CreateMetric cadastra uma métrica de consumo.
func (s *Service) CreateMetric(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, in MetricInput) (Metric, error) {
	now := s.clock.Now()
	m, err := NewMetric(ids.New(), tenantID, in, now)
	if err != nil {
		return Metric{}, err
	}
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if err := tx.InsertMetric(ctx, m); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "catalog.metric_created", "metric", m.ID.String(),
			map[string]any{"code": m.Code, "aggregation": m.Aggregation}))
	})
	return m, err
}

// Metrics lista métricas pelo código, em páginas por cursor.
func (s *Service) Metrics(ctx context.Context, tenantID uuid.UUID, limit int, cursor string) (Page[Metric], error) {
	var out Page[Metric]
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		out, err = tx.Metrics(ctx, limit, cursor)
		return err
	})
	return out, err
}

// PlanDetails reúne o plano e suas versões de preço.
type PlanDetails struct {
	Plan
	Prices []PriceVersion
}

// CreatePlan cadastra um plano com a primeira versão de preço.
func (s *Service) CreatePlan(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, actorUser *uuid.UUID, in PlanInput, price PriceInput) (PlanDetails, error) {
	now := s.clock.Now()
	p, err := NewPlan(ids.New(), tenantID, in, now)
	if err != nil {
		return PlanDetails{}, err
	}
	var out PlanDetails
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		prod, err := tx.Product(ctx, in.ProductID, false)
		if err != nil {
			return err
		}
		if prod.Status != Active {
			return apperr.Conflict("product_archived", "O produto está arquivado.")
		}
		var metric *Metric
		if price.MetricID != nil {
			m, err := tx.Metric(ctx, *price.MetricID)
			if err != nil && !apperr.IsKind(err, apperr.KindNotFound) {
				return err
			}
			if err == nil {
				metric = &m
			}
		}
		pv, err := NewPriceVersion(ids.New(), tenantID, p.ID, 1, money.BRL, price, metric, actorUser, now)
		if err != nil {
			return err
		}
		p.CurrentPriceVersionID = &pv.ID
		if err := tx.InsertPlan(ctx, p); err != nil {
			return err
		}
		if err := tx.InsertPriceVersion(ctx, pv); err != nil {
			return err
		}
		out = PlanDetails{Plan: p, Prices: []PriceVersion{pv}}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "catalog.plan_created", "plan", p.ID.String(),
			map[string]any{"code": p.Code, "pricing_model": pv.Model, "price_version": 1}))
	})
	return out, err
}

// UpdatePlanInput são os campos editáveis de um plano (preço muda por versão).
type UpdatePlanInput struct {
	Name        string
	Description string
	TrialDays   int
	Status      Status
}

// UpdatePlan altera dados descritivos do plano. Não altera preços.
func (s *Service) UpdatePlan(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, in UpdatePlanInput, expectedVersion int) (Plan, error) {
	var p Plan
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if p, err = tx.Plan(ctx, id, true); err != nil {
			return err
		}
		if p.Version != expectedVersion {
			return apperr.PreconditionFailed("version_mismatch", "O plano foi alterado por outra pessoa. Recarregue e tente novamente.")
		}
		chk, err := NewPlan(p.ID, tenantID, PlanInput{ProductID: p.ProductID, Code: p.Code, Name: in.Name, Description: in.Description,
			Interval: p.Interval, TrialDays: in.TrialDays}, p.CreatedAt)
		if err != nil {
			return err
		}
		if in.Status != Active && in.Status != Archived {
			return apperr.Invalid("invalid_status", "Situação inválida.", apperr.FieldError{Field: "status", Message: "Use active ou archived."})
		}
		p.Name, p.Description, p.TrialDays, p.Status = chk.Name, chk.Description, chk.TrialDays, in.Status
		if err := tx.UpdatePlan(ctx, p); err != nil {
			return err
		}
		p.Version++
		return tx.Audit(ctx, audit.New(tenantID, s.clock.Now(), actor, "catalog.plan_updated", "plan", id.String(), map[string]any{"status": in.Status}))
	})
	return p, err
}

// AddPriceVersion cria uma nova versão de preço, que passa a ser a vigente
// para novas assinaturas. Assinaturas existentes mantêm sua versão. A troca
// do preço vigente altera o plano: exige a versão esperada do plano
// (If-Match) e devolve a versão do plano depois da gravação.
func (s *Service) AddPriceVersion(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, actorUser *uuid.UUID, planID uuid.UUID, in PriceInput,
	expectedVersion int) (PriceVersion, int, error) {
	now := s.clock.Now()
	var pv PriceVersion
	planVersion := 0
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		p, err := tx.Plan(ctx, planID, true)
		if err != nil {
			return err
		}
		if p.Status != Active {
			return apperr.Conflict("plan_archived", "O plano está arquivado.")
		}
		var metric *Metric
		if in.MetricID != nil {
			m, err := tx.Metric(ctx, *in.MetricID)
			if err != nil && !apperr.IsKind(err, apperr.KindNotFound) {
				return err
			}
			if err == nil {
				metric = &m
			}
		}
		last, err := tx.MaxPriceVersion(ctx, planID)
		if err != nil {
			return err
		}
		pv, err = NewPriceVersion(ids.New(), tenantID, planID, last+1, p.Currency, in, metric, actorUser, now)
		if err != nil {
			return err
		}
		if err := tx.InsertPriceVersion(ctx, pv); err != nil {
			return err
		}
		p.CurrentPriceVersionID, p.Version = &pv.ID, expectedVersion
		if err := tx.UpdatePlan(ctx, p); err != nil {
			return err
		}
		planVersion = expectedVersion + 1
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "catalog.price_version_created", "plan", planID.String(),
			map[string]any{"price_version": pv.Version, "pricing_model": pv.Model}))
	})
	if err != nil {
		return PriceVersion{}, 0, err
	}
	return pv, planVersion, nil
}

// Plan retorna o plano com todas as versões de preço.
func (s *Service) Plan(ctx context.Context, tenantID, id uuid.UUID) (PlanDetails, error) {
	var out PlanDetails
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		p, err := tx.Plan(ctx, id, false)
		if err != nil {
			return err
		}
		prices, err := tx.PriceVersions(ctx, id)
		if err != nil {
			return err
		}
		out = PlanDetails{Plan: p, Prices: prices}
		return nil
	})
	return out, err
}

// Plans lista planos, dos criados mais recentemente para os mais antigos, em
// páginas por cursor.
func (s *Service) Plans(ctx context.Context, tenantID uuid.UUID, status Status, productID *uuid.UUID, limit int, cursor string) (Page[Plan], error) {
	var out Page[Plan]
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		out, err = tx.Plans(ctx, status, productID, limit, cursor)
		return err
	})
	return out, err
}
