package pgstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// Subscriptions implementa subscriptions.Store.
type Subscriptions struct{ s *Store }

// Subscriptions retorna o adaptador de assinaturas.
func (s *Store) Subscriptions() *Subscriptions { return &Subscriptions{s: s} }

type subscriptionsTx struct{ base }

// InTenant abre a transação do tenant.
func (a *Subscriptions) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(subscriptions.Tx) error) error {
	return a.s.inTenant(ctx, tenantID, func(b base) error { return fn(subscriptionsTx{b}) })
}

func subscriptionFromRow(r dbgen.Subscription) (subscriptions.Subscription, error) {
	override, err := ratFromNumeric(r.PriceOverrideAmount)
	if err != nil {
		return subscriptions.Subscription{}, err
	}
	return subscriptions.Subscription{ID: r.ID, TenantID: r.TenantID, CustomerID: r.CustomerID, PlanID: r.PlanID, PriceVersionID: r.PriceVersionID,
		Status: subscriptions.Status(r.Status), Quantity: int(r.Quantity), Interval: calendar.Interval(r.BillingInterval), Timezone: r.Timezone,
		Anchor: dateFromPg(r.AnchorDate), TrialEndsAt: r.TrialEndsAt, StartedAt: r.StartedAt,
		CurrentPeriod: calendar.Period{Start: r.CurrentPeriodStart, End: r.CurrentPeriodEnd}, NextBillingIndex: int(r.NextBillingIndex),
		NextBillingAt: r.NextBillingAt, CancelAtPeriodEnd: r.CancelAtPeriodEnd, CanceledAt: r.CanceledAt, CancelReason: deref(r.CancelReason),
		EndedAt: r.EndedAt, SuspendedAt: r.SuspendedAt, SuspensionReason: deref(r.SuspensionReason), PaymentTermsDays: intFromPtr32(r.PaymentTermsDays),
		PriceOverride: override, PriceOverrideAdjustmentID: r.PriceOverrideAdjustmentID, PendingUsageStarts: pendingUsageFromPg(r.PendingUsageStarts),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Version: int(r.Version)}, nil
}

// pendingUsageFromPg normaliza a lista lida do banco: vazia vira nula e os
// instantes ficam em UTC.
func pendingUsageFromPg(starts []time.Time) []time.Time {
	if len(starts) == 0 {
		return nil
	}
	out := make([]time.Time, len(starts))
	for i, t := range starts {
		out[i] = t.UTC()
	}
	return out
}

// pendingUsageToPg devolve a lista a gravar; a coluna não aceita nulo.
func pendingUsageToPg(starts []time.Time) []time.Time {
	if starts == nil {
		return []time.Time{}
	}
	return starts
}

func (b base) insertSubscription(ctx context.Context, s subscriptions.Subscription) error {
	return b.q.InsertSubscription(ctx, dbgen.InsertSubscriptionParams{ID: s.ID, TenantID: b.tenantID, CustomerID: s.CustomerID, PlanID: s.PlanID,
		PriceVersionID: s.PriceVersionID, Status: string(s.Status), Quantity: int32(s.Quantity), BillingInterval: string(s.Interval),
		Timezone: s.Timezone, AnchorDate: dateToPg(s.Anchor), TrialEndsAt: s.TrialEndsAt, StartedAt: s.StartedAt,
		CurrentPeriodStart: s.CurrentPeriod.Start, CurrentPeriodEnd: s.CurrentPeriod.End, NextBillingIndex: int32(s.NextBillingIndex),
		NextBillingAt: s.NextBillingAt, CancelAtPeriodEnd: s.CancelAtPeriodEnd, CanceledAt: s.CanceledAt, CancelReason: strPtr(s.CancelReason),
		EndedAt: s.EndedAt, SuspendedAt: s.SuspendedAt, SuspensionReason: strPtr(s.SuspensionReason), PaymentTermsDays: intPtr32(s.PaymentTermsDays),
		CreatedAt: s.CreatedAt})
}

func (b base) updateSubscription(ctx context.Context, s subscriptions.Subscription) error {
	override, err := numericPtr(s.PriceOverride)
	if err != nil {
		return err
	}
	n, err := b.q.UpdateSubscription(ctx, dbgen.UpdateSubscriptionParams{TenantID: b.tenantID, ID: s.ID, PlanID: s.PlanID, PriceVersionID: s.PriceVersionID,
		Status: string(s.Status), Quantity: int32(s.Quantity), TrialEndsAt: s.TrialEndsAt, CurrentPeriodStart: s.CurrentPeriod.Start,
		CurrentPeriodEnd: s.CurrentPeriod.End, NextBillingIndex: int32(s.NextBillingIndex), NextBillingAt: s.NextBillingAt,
		CancelAtPeriodEnd: s.CancelAtPeriodEnd, CanceledAt: s.CanceledAt, CancelReason: strPtr(s.CancelReason), EndedAt: s.EndedAt,
		SuspendedAt: s.SuspendedAt, SuspensionReason: strPtr(s.SuspensionReason), PaymentTermsDays: intPtr32(s.PaymentTermsDays),
		PriceOverrideAmount: override, PriceOverrideAdjustmentID: s.PriceOverrideAdjustmentID, PendingUsageStarts: pendingUsageToPg(s.PendingUsageStarts),
		Version: int32(s.Version)})
	if err != nil {
		return err
	}
	return versionConflict(n, "A assinatura foi alterada por outra pessoa.")
}

func (b base) insertSubscriptionHistory(ctx context.Context, subscriptionID uuid.UUID, actor audit.Actor, ev []subscriptions.HistoryEvent) error {
	for _, e := range ev {
		details, err := json.Marshal(e.Details)
		if err != nil {
			return err
		}
		var from *string
		if e.From != nil {
			f := string(*e.From)
			from = &f
		}
		if err := b.q.InsertSubscriptionHistory(ctx, dbgen.InsertSubscriptionHistoryParams{ID: ids.New(), TenantID: b.tenantID,
			SubscriptionID: subscriptionID, Event: e.Event, FromStatus: from, ToStatus: string(e.To), OccurredAt: e.At,
			ActorType: string(actor.Type), ActorID: strPtr(actor.ID), Details: details}); err != nil {
			return err
		}
	}
	return nil
}

func (b base) customer(ctx context.Context, id uuid.UUID) (customers.Customer, error) {
	r, err := b.q.GetCustomer(ctx, dbgen.GetCustomerParams{TenantID: b.tenantID, ID: id})
	if err != nil {
		return customers.Customer{}, notFound(err, "customer_not_found", "Cliente não encontrado.")
	}
	return customerFromRow(r), nil
}

func (tx subscriptionsTx) Customer(ctx context.Context, id uuid.UUID) (customers.Customer, error) {
	return tx.customer(ctx, id)
}

func (tx subscriptionsTx) Plan(ctx context.Context, id uuid.UUID) (catalog.Plan, error) {
	return tx.plan(ctx, id)
}

func (tx subscriptionsTx) PriceVersion(ctx context.Context, id uuid.UUID) (catalog.PriceVersion, error) {
	return tx.priceVersion(ctx, id)
}

func (tx subscriptionsTx) PlatformPlan(ctx context.Context) (platformbilling.Plan, error) {
	return tx.platformPlan(ctx)
}

func (tx subscriptionsTx) PlatformUsage(ctx context.Context) (platformbilling.Usage, error) {
	return tx.platformUsage(ctx)
}

func (tx subscriptionsTx) Insert(ctx context.Context, s subscriptions.Subscription) error {
	return tx.insertSubscription(ctx, s)
}

func (tx subscriptionsTx) Get(ctx context.Context, id uuid.UUID, forUpdate bool) (subscriptions.Subscription, error) {
	return tx.subscription(ctx, id, forUpdate)
}

func (b base) subscription(ctx context.Context, id uuid.UUID, forUpdate bool) (subscriptions.Subscription, error) {
	var r dbgen.Subscription
	var err error
	if forUpdate {
		r, err = b.q.GetSubscriptionForUpdate(ctx, dbgen.GetSubscriptionForUpdateParams{TenantID: b.tenantID, ID: id})
	} else {
		r, err = b.q.GetSubscription(ctx, dbgen.GetSubscriptionParams{TenantID: b.tenantID, ID: id})
	}
	if err != nil {
		return subscriptions.Subscription{}, notFound(err, "subscription_not_found", "Assinatura não encontrada.")
	}
	return subscriptionFromRow(r)
}

func (tx subscriptionsTx) Update(ctx context.Context, s subscriptions.Subscription) error {
	return tx.updateSubscription(ctx, s)
}

func (tx subscriptionsTx) List(ctx context.Context, f subscriptions.ListFilter) (subscriptions.Page, error) {
	cts, cid, err := parseTimeCursor(f.Cursor)
	if err != nil {
		return subscriptions.Page{}, err
	}
	limit := pageLimit(f.Limit)
	rows, err := tx.q.ListSubscriptions(ctx, dbgen.ListSubscriptionsParams{TenantID: tx.tenantID, Status: strPtr(string(f.Status)),
		CustomerID: f.CustomerID, CursorTs: cts, CursorID: cid, RowLimit: int32(limit + 1)})
	if err != nil {
		return subscriptions.Page{}, err
	}
	var p subscriptions.Page
	for i, r := range rows {
		if i == limit {
			last := p.Items[len(p.Items)-1]
			p.NextCursor = timeCursor(last.CreatedAt, last.ID)
			break
		}
		sub, err := subscriptionFromRow(dbgen.Subscription{ID: r.ID, TenantID: r.TenantID, CustomerID: r.CustomerID, PlanID: r.PlanID,
			PriceVersionID: r.PriceVersionID, Status: r.Status, Quantity: r.Quantity, BillingInterval: r.BillingInterval, Timezone: r.Timezone,
			AnchorDate: r.AnchorDate, TrialEndsAt: r.TrialEndsAt, StartedAt: r.StartedAt, CurrentPeriodStart: r.CurrentPeriodStart,
			CurrentPeriodEnd: r.CurrentPeriodEnd, NextBillingIndex: r.NextBillingIndex, NextBillingAt: r.NextBillingAt,
			CancelAtPeriodEnd: r.CancelAtPeriodEnd, CanceledAt: r.CanceledAt, CancelReason: r.CancelReason, EndedAt: r.EndedAt,
			SuspendedAt: r.SuspendedAt, SuspensionReason: r.SuspensionReason, PaymentTermsDays: r.PaymentTermsDays, CreatedAt: r.CreatedAt,
			UpdatedAt: r.UpdatedAt, Version: r.Version, PriceOverrideAmount: r.PriceOverrideAmount, PriceOverrideAdjustmentID: r.PriceOverrideAdjustmentID,
			PendingUsageStarts: r.PendingUsageStarts})
		if err != nil {
			return subscriptions.Page{}, err
		}
		p.Items = append(p.Items, sub)
	}
	return p, nil
}

func (tx subscriptionsTx) InsertHistory(ctx context.Context, subscriptionID uuid.UUID, actor audit.Actor, ev []subscriptions.HistoryEvent) error {
	return tx.insertSubscriptionHistory(ctx, subscriptionID, actor, ev)
}

func (tx subscriptionsTx) History(ctx context.Context, subscriptionID uuid.UUID) ([]subscriptions.HistoryEntry, error) {
	rows, err := tx.q.ListSubscriptionHistory(ctx, dbgen.ListSubscriptionHistoryParams{TenantID: tx.tenantID, SubscriptionID: subscriptionID})
	if err != nil {
		return nil, err
	}
	out := make([]subscriptions.HistoryEntry, 0, len(rows))
	for _, r := range rows {
		d, err := jsonMap(r.Details)
		if err != nil {
			return nil, err
		}
		var from *subscriptions.Status
		if r.FromStatus != nil {
			f := subscriptions.Status(*r.FromStatus)
			from = &f
		}
		out = append(out, subscriptions.HistoryEntry{ID: r.ID, Event: r.Event, FromStatus: from, ToStatus: subscriptions.Status(r.ToStatus),
			OccurredAt: r.OccurredAt, ActorType: r.ActorType, ActorID: deref(r.ActorID), Details: d})
	}
	return out, nil
}

// Names retorna os nomes do cliente e do plano de uma assinatura (exibição).
func (a *Subscriptions) Names(ctx context.Context, tenantID, id uuid.UUID) (customer, plan string, err error) {
	err = a.s.inTenant(ctx, tenantID, func(b base) error {
		r, err := b.q.SubscriptionNames(ctx, dbgen.SubscriptionNamesParams{TenantID: tenantID, ID: id})
		customer, plan = r.CustomerName, r.PlanName
		return notFound(err, "subscription_not_found", "Assinatura não encontrada.")
	})
	return customer, plan, err
}

// refreshStanding volta a assinatura de past_due para active quando não há
// mais faturas vencidas em aberto.
func (b base) refreshStanding(ctx context.Context, subscriptionID uuid.UUID, actor audit.Actor, now time.Time) error {
	sub, err := b.subscription(ctx, subscriptionID, true)
	if err != nil {
		return err
	}
	if sub.Status != subscriptions.PastDue {
		return nil
	}
	overdue, err := b.q.HasOverdueInvoices(ctx, dbgen.HasOverdueInvoicesParams{TenantID: b.tenantID, SubscriptionID: &subscriptionID, Now: &now})
	if err != nil || overdue {
		return err
	}
	if err := sub.MarkCurrent(now); err != nil {
		return err
	}
	if err := b.updateSubscription(ctx, sub); err != nil {
		return err
	}
	return b.insertSubscriptionHistory(ctx, sub.ID, actor, sub.PopEvents())
}
