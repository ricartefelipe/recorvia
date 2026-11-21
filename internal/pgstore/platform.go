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

func addonOrderFromRow(r dbgen.PlatformAddonOrder) platformbilling.AddonOrder {
	return platformbilling.AddonOrder{ID: r.ID, TenantID: r.TenantID, AddonCode: r.AddonCode, Status: platformbilling.AddonStatus(r.Status),
		Notes: r.Notes, Price: brl(r.PriceAmount), RequestedBy: r.RequestedBy, CreatedAt: r.CreatedAt, ScheduledFor: r.ScheduledFor,
		CompletedAt: r.CompletedAt, CanceledAt: r.CanceledAt, CancelReason: deref(r.CancelReason), HandledBy: r.HandledBy,
		StatementID: r.StatementID, Version: int(r.Version)}
}

// AddonOrders lista pedidos de serviços adicionais.
func (p *Platform) AddonOrders(ctx context.Context, tenantID uuid.UUID) ([]platformbilling.AddonOrder, error) {
	var out []platformbilling.AddonOrder
	err := p.s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListAddonOrders(ctx, tenantID)
		for _, r := range rows {
			out = append(out, addonOrderFromRow(r))
		}
		return err
	})
	return out, err
}

// StaffAddonOrders lista os pedidos de todas as organizações para a equipe
// da plataforma (funções SECURITY DEFINER sem dados dos clientes dos
// tenants), com o nome da organização e do serviço.
func (p *Platform) StaffAddonOrders(ctx context.Context, query platformbilling.StaffAddonQuery) ([]platformbilling.StaffAddonOrder, error) {
	var out []platformbilling.StaffAddonOrder
	err := p.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		args := dbgen.StaffAddonOrdersParams{RowLimit: int32(query.Limit)}
		if query.Status != "" {
			v := string(query.Status)
			args.Status = &v
		}
		if query.After != nil {
			at, id := query.After.At, query.After.ID
			args.AfterAt, args.AfterID = &at, &id
		}
		rows, err := q.StaffAddonOrders(ctx, args)
		if err != nil || len(rows) == 0 {
			return err
		}
		tenantIDs := make([]uuid.UUID, 0, len(rows))
		seen := map[uuid.UUID]bool{}
		for _, r := range rows {
			if !seen[r.TenantID] {
				seen[r.TenantID] = true
				tenantIDs = append(tenantIDs, r.TenantID)
			}
		}
		names, err := q.PlatformTenantNames(ctx, tenantIDs)
		if err != nil {
			return err
		}
		byTenant := make(map[uuid.UUID]dbgen.PlatformTenantNamesRow, len(names))
		for _, n := range names {
			byTenant[n.TenantID] = n
		}
		addons, err := q.ListAllPlatformAddons(ctx)
		if err != nil {
			return err
		}
		addonNames := make(map[string]string, len(addons))
		for _, a := range addons {
			addonNames[a.Code] = a.Name
		}
		out = make([]platformbilling.StaffAddonOrder, 0, len(rows))
		for _, r := range rows {
			t := byTenant[r.TenantID]
			out = append(out, platformbilling.StaffAddonOrder{AddonOrder: addonOrderFromRow(r), TenantName: t.Name, TenantSlug: t.Slug,
				AddonName: addonNames[r.AddonCode]})
		}
		return nil
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
		TrialEndsAt: r.TrialEndsAt, Anchor: dateFromPg(r.AnchorDate), PeriodStart: r.CurrentPeriodStart, PeriodEnd: r.CurrentPeriodEnd,
		Version: int(r.Version)}, nil
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
	return tx.q.InsertAddonOrder(ctx, dbgen.InsertAddonOrderParams{ID: o.ID, TenantID: tx.tenantID, AddonCode: o.AddonCode, Status: string(o.Status),
		Notes: o.Notes, PriceAmount: o.Price.Amount(), RequestedBy: o.RequestedBy, CreatedAt: o.CreatedAt})
}

func (tx platformTx) PendingInvitations(ctx context.Context, now time.Time) (int, error) {
	n, err := tx.q.CountPendingInvitations(ctx, dbgen.CountPendingInvitationsParams{TenantID: tx.tenantID, Now: now})
	return int(n), err
}

func (tx platformTx) AddonOrder(ctx context.Context, id uuid.UUID) (platformbilling.AddonOrder, error) {
	r, err := tx.q.GetAddonOrderForUpdate(ctx, dbgen.GetAddonOrderForUpdateParams{TenantID: tx.tenantID, ID: id})
	if err != nil {
		return platformbilling.AddonOrder{}, notFound(err, "addon_order_not_found", "Pedido de serviço adicional não encontrado.")
	}
	return addonOrderFromRow(r), nil
}

func (tx platformTx) UpdateAddonOrder(ctx context.Context, o platformbilling.AddonOrder) error {
	n, err := tx.q.UpdateAddonOrder(ctx, dbgen.UpdateAddonOrderParams{TenantID: tx.tenantID, ID: o.ID, Status: string(o.Status),
		ScheduledFor: o.ScheduledFor, CompletedAt: o.CompletedAt, CanceledAt: o.CanceledAt, CancelReason: strPtr(o.CancelReason),
		HandledBy: o.HandledBy, UpdatedAt: tx.store.clock.Now(), Version: int32(o.Version)})
	if err != nil {
		return err
	}
	return versionConflict(n, "O pedido foi alterado por outra pessoa.")
}

func (tx platformTx) UnbilledAddonsTotal(ctx context.Context, before time.Time) (money.Money, error) {
	v, err := tx.q.SumUnbilledCompletedAddons(ctx, dbgen.SumUnbilledCompletedAddonsParams{TenantID: tx.tenantID, Before: &before})
	return brl(v), err
}

func (tx platformTx) MarkAddonsBilled(ctx context.Context, statementID uuid.UUID, before time.Time) error {
	return tx.q.MarkAddonsBilled(ctx, dbgen.MarkAddonsBilledParams{TenantID: tx.tenantID, StatementID: &statementID, Before: &before})
}

func (tx platformTx) InsertStatement(ctx context.Context, s platformbilling.Statement) (bool, error) {
	n, err := tx.q.InsertPlatformStatement(ctx, dbgen.InsertPlatformStatementParams{ID: s.ID, TenantID: tx.tenantID, PeriodStart: s.PeriodStart,
		PeriodEnd: s.PeriodEnd, PlanCode: s.PlanCode, SubscriptionAmount: s.SubscriptionAmount.Amount(), AddonsAmount: s.AddonsAmount.Amount(),
		TotalAmount: s.Total.Amount(), Currency: s.Total.Currency().Code(), Status: s.Status, CreatedAt: s.CreatedAt})
	return n == 1, err
}
