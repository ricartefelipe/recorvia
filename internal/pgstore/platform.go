package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

// Platform implementa platformbilling.Store.
type Platform struct{ s *Store }

// Platform retorna o adaptador do faturamento da plataforma.
func (s *Store) Platform() *Platform { return &Platform{s: s} }

type platformTx struct{ base }

// InTenant abre a transação do tenant.
func (p *Platform) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(platformbilling.Tx) error) error {
	return p.s.inTenant(ctx, tenantID, func(b base) error { return fn(platformTx{b}) })
}

// Plans lista os planos ativos.
func (p *Platform) Plans(ctx context.Context) ([]platformbilling.Plan, error) {
	var out []platformbilling.Plan
	err := p.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		rows, err := q.ListPlatformPlans(ctx)
		for _, r := range rows {
			out = append(out, platformPlanFromRow(r))
		}
		return err
	})
	return out, err
}

func addonFromRow(r dbgen.PlatformAddon) platformbilling.Addon {
	return platformbilling.Addon{Code: r.Code, Name: r.Name, Description: r.Description, Price: brl(r.PriceAmount), Kind: r.Kind, Active: r.Active}
}

// Addons lista os serviços adicionais.
func (p *Platform) Addons(ctx context.Context) ([]platformbilling.Addon, error) {
	var out []platformbilling.Addon
	err := p.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		rows, err := q.ListPlatformAddons(ctx)
		for _, r := range rows {
			out = append(out, addonFromRow(r))
		}
		return err
	})
	return out, err
}

// TenantsDueStatement lista tenants com demonstrativo a fechar.
func (p *Platform) TenantsDueStatement(ctx context.Context, now time.Time) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := p.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		out, err = q.TenantsDuePlatformStatement(ctx, now)
		return err
	})
	return out, err
}

// AddonOrders lista pedidos de serviços adicionais.
func (p *Platform) AddonOrders(ctx context.Context, tenantID uuid.UUID) ([]platformbilling.AddonOrder, error) {
	var out []platformbilling.AddonOrder
	err := p.s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListAddonOrders(ctx, tenantID)
		for _, r := range rows {
			out = append(out, platformbilling.AddonOrder{ID: r.ID, TenantID: r.TenantID, AddonCode: r.AddonCode, Status: r.Status,
				Notes: r.Notes, Price: brl(r.PriceAmount), RequestedBy: r.RequestedBy, CreatedAt: r.CreatedAt})
		}
		return err
	})
	return out, err
}

// Statements lista demonstrativos.
func (p *Platform) Statements(ctx context.Context, tenantID uuid.UUID) ([]platformbilling.Statement, error) {
	var out []platformbilling.Statement
	err := p.s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListPlatformStatements(ctx, tenantID)
		for _, r := range rows {
			out = append(out, platformbilling.Statement{ID: r.ID, TenantID: r.TenantID, PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd,
				PlanCode: r.PlanCode, SubscriptionAmount: brl(r.SubscriptionAmount), AddonsAmount: brl(r.AddonsAmount),
				Total: brl(r.TotalAmount), Status: r.Status, CreatedAt: r.CreatedAt})
		}
		return err
	})
	return out, err
}

func (tx platformTx) Subscription(ctx context.Context) (platformbilling.Subscription, error) {
	r, err := tx.q.GetTenantPlatformSubscriptionForUpdate(ctx, tx.tenantID)
	if err != nil {
		return platformbilling.Subscription{}, notFound(err, "platform_subscription_not_found", "Assinatura da plataforma não encontrada.")
	}
	return platformbilling.Subscription{TenantID: r.TenantID, PlanCode: r.PlanCode, Status: platformbilling.Status(r.Status),
		TrialEndsAt: r.TrialEndsAt, PeriodStart: r.CurrentPeriodStart, PeriodEnd: r.CurrentPeriodEnd, Version: int(r.Version)}, nil
}

func (tx platformTx) UpdateSubscription(ctx context.Context, s platformbilling.Subscription) error {
	n, err := tx.q.UpdateTenantPlatformSubscription(ctx, dbgen.UpdateTenantPlatformSubscriptionParams{TenantID: tx.tenantID, PlanCode: s.PlanCode,
		Status: string(s.Status), TrialEndsAt: s.TrialEndsAt, CurrentPeriodStart: s.PeriodStart, CurrentPeriodEnd: s.PeriodEnd, Version: int32(s.Version)})
	if err != nil {
		return err
	}
	return versionConflict(n, "A assinatura da plataforma foi alterada por outra pessoa.")
}

func (tx platformTx) Plan(ctx context.Context, code string) (platformbilling.Plan, error) {
	r, err := tx.q.GetPlatformPlan(ctx, code)
	if err != nil {
		return platformbilling.Plan{}, notFound(err, "platform_plan_not_found", "Plano da plataforma não encontrado.")
	}
	return platformPlanFromRow(r), nil
}

func (tx platformTx) Addon(ctx context.Context, code string) (platformbilling.Addon, error) {
	r, err := tx.q.GetPlatformAddon(ctx, code)
	if err != nil {
		return platformbilling.Addon{}, notFound(err, "addon_not_found", "Serviço adicional não encontrado.")
	}
	return addonFromRow(r), nil
}

func (tx platformTx) Usage(ctx context.Context) (platformbilling.Usage, error) {
	return tx.platformUsage(ctx)
}

func (tx platformTx) InsertAddonOrder(ctx context.Context, o platformbilling.AddonOrder) error {
	return tx.q.InsertAddonOrder(ctx, dbgen.InsertAddonOrderParams{ID: o.ID, TenantID: tx.tenantID, AddonCode: o.AddonCode, Status: o.Status,
		Notes: o.Notes, PriceAmount: o.Price.Amount(), RequestedBy: o.RequestedBy, CreatedAt: o.CreatedAt})
}

func (tx platformTx) CompletedAddonsTotal(ctx context.Context, from, to time.Time) (money.Money, error) {
	v, err := tx.q.SumCompletedAddons(ctx, dbgen.SumCompletedAddonsParams{TenantID: tx.tenantID, PeriodStart: from, PeriodEnd: to})
	return brl(v), err
}

func (tx platformTx) InsertStatement(ctx context.Context, s platformbilling.Statement) (bool, error) {
	n, err := tx.q.InsertPlatformStatement(ctx, dbgen.InsertPlatformStatementParams{ID: s.ID, TenantID: tx.tenantID, PeriodStart: s.PeriodStart,
		PeriodEnd: s.PeriodEnd, PlanCode: s.PlanCode, SubscriptionAmount: s.SubscriptionAmount.Amount(), AddonsAmount: s.AddonsAmount.Amount(),
		TotalAmount: s.Total.Amount(), Currency: s.Total.Currency().Code(), Status: s.Status, CreatedAt: s.CreatedAt})
	return n == 1, err
}
