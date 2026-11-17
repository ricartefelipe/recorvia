package platformbilling

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// AddonOrder é a solicitação de um serviço adicional (ex.: implantação assistida).
type AddonOrder struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	AddonCode   string
	Status      string
	Notes       string
	Price       money.Money
	RequestedBy uuid.UUID
	CreatedAt   time.Time
}

// Addon é um serviço adicional ofertado.
type Addon struct {
	Code        string
	Name        string
	Description string
	Price       money.Money
	Kind        string
	Active      bool
}

// Tx são as operações de persistência dentro da transação do tenant.
type Tx interface {
	Subscription(ctx context.Context) (Subscription, error)
	UpdateSubscription(ctx context.Context, s Subscription) error
	Plan(ctx context.Context, code string) (Plan, error)
	Addon(ctx context.Context, code string) (Addon, error)
	Usage(ctx context.Context) (Usage, error)
	InsertAddonOrder(ctx context.Context, o AddonOrder) error
	CompletedAddonsTotal(ctx context.Context, from, to time.Time) (money.Money, error)
	InsertStatement(ctx context.Context, s Statement) (bool, error)
	TenantLocation(ctx context.Context) (*time.Location, error)
	Audit(ctx context.Context, e audit.Entry) error
}

// Store abre transações de tenant e lê o catálogo global de planos.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
	Plans(ctx context.Context) ([]Plan, error)
	Addons(ctx context.Context) ([]Addon, error)
	TenantsDueStatement(ctx context.Context, now time.Time) ([]uuid.UUID, error)
}

// Service expõe os casos de uso do faturamento da plataforma.
type Service struct {
	store Store
	clock clock.Clock
}

// NewService cria o serviço.
func NewService(store Store, c clock.Clock) *Service { return &Service{store: store, clock: c} }

// Overview reúne plano, assinatura e consumo de limites do tenant.
type Overview struct {
	Plan         Plan
	Subscription Subscription
	Usage        Usage
}

// Overview retorna a situação do tenant na plataforma.
func (s *Service) Overview(ctx context.Context, tenantID uuid.UUID) (Overview, error) {
	var out Overview
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		sub, err := tx.Subscription(ctx)
		if err != nil {
			return err
		}
		plan, err := tx.Plan(ctx, sub.PlanCode)
		if err != nil {
			return err
		}
		usage, err := tx.Usage(ctx)
		if err != nil {
			return err
		}
		out = Overview{Plan: plan, Subscription: sub, Usage: usage}
		return nil
	})
	return out, err
}

// Plans lista os planos disponíveis.
func (s *Service) Plans(ctx context.Context) ([]Plan, error) { return s.store.Plans(ctx) }

// Addons lista os serviços adicionais.
func (s *Service) Addons(ctx context.Context) ([]Addon, error) { return s.store.Addons(ctx) }

// ChangePlan troca o plano da plataforma, validando o consumo atual contra os
// limites do plano de destino. A mudança vale a partir do período corrente.
func (s *Service) ChangePlan(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, planCode string) (Subscription, error) {
	var out Subscription
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		sub, err := tx.Subscription(ctx)
		if err != nil {
			return err
		}
		if sub.PlanCode == planCode {
			out = sub
			return nil
		}
		target, err := tx.Plan(ctx, planCode)
		if err != nil {
			return err
		}
		usage, err := tx.Usage(ctx)
		if err != nil {
			return err
		}
		if err := CanChangeTo(target, usage); err != nil {
			return err
		}
		previous := sub.PlanCode
		sub.PlanCode = target.Code
		if err := tx.UpdateSubscription(ctx, sub); err != nil {
			return err
		}
		sub.Version++
		out = sub
		return tx.Audit(ctx, audit.New(tenantID, s.clock.Now(), actor, "platform.plan_changed", "platform_subscription", tenantID.String(),
			map[string]any{"from": previous, "to": target.Code}))
	})
	return out, err
}

// RequestAddon registra o pedido de serviço adicional.
func (s *Service) RequestAddon(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, requester uuid.UUID, code, notes string) (AddonOrder, error) {
	if len([]rune(notes)) > 2000 {
		return AddonOrder{}, apperr.Invalid("addon_notes_too_long", "As observações devem ter no máximo 2.000 caracteres.")
	}
	var out AddonOrder
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		addon, err := tx.Addon(ctx, code)
		if err != nil {
			return err
		}
		if !addon.Active {
			return apperr.Invalid("addon_inactive", "Serviço adicional indisponível.")
		}
		out = AddonOrder{
			ID: ids.New(), TenantID: tenantID, AddonCode: addon.Code, Status: "requested", Notes: notes,
			Price: addon.Price, RequestedBy: requester, CreatedAt: s.clock.Now(),
		}
		if err := tx.InsertAddonOrder(ctx, out); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, out.CreatedAt, actor, "platform.addon_requested", "platform_addon_order", out.ID.String(),
			map[string]any{"addon": addon.Code}))
	})
	return out, err
}

// CloseDueStatements fecha os demonstrativos de períodos encerrados e avança
// as assinaturas. É idempotente: um demonstrativo por tenant e período.
func (s *Service) CloseDueStatements(ctx context.Context) (int, error) {
	now := s.clock.Now()
	tenants, err := s.store.TenantsDueStatement(ctx, now)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, tenantID := range tenants {
		if err := ctx.Err(); err != nil {
			return closed, err
		}
		err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
			loc, err := tx.TenantLocation(ctx)
			if err != nil {
				return err
			}
			sub, err := tx.Subscription(ctx)
			if err != nil {
				return err
			}
			// Recupera períodos atrasados (agendador indisponível), limitando
			// o trabalho por transação.
			for i := 0; i < 24 && !sub.PeriodEnd.After(now) && sub.Status != StatusCanceled; i++ {
				plan, err := tx.Plan(ctx, sub.PlanCode)
				if err != nil {
					return err
				}
				addons, err := tx.CompletedAddonsTotal(ctx, sub.PeriodStart, sub.PeriodEnd)
				if err != nil {
					return err
				}
				st, err := CloseStatement(ids.New(), sub, plan, addons, now)
				if err != nil {
					return err
				}
				inserted, err := tx.InsertStatement(ctx, st)
				if err != nil {
					return err
				}
				if inserted {
					closed++
				}
				if err := sub.Advance(loc); err != nil {
					return err
				}
			}
			return tx.UpdateSubscription(ctx, sub)
		})
		if err != nil {
			return closed, err
		}
	}
	return closed, nil
}
