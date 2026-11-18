package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// Collections implementa collections.Store.
type Collections struct{ s *Store }

// Collections retorna o adaptador da régua de cobrança.
func (s *Store) Collections() *Collections { return &Collections{s: s} }

type collectionsTx struct{ base }

// InTenant abre a transação do tenant.
func (a *Collections) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(collections.Tx) error) error {
	return a.s.inTenant(ctx, tenantID, func(b base) error { return fn(collectionsTx{b}) })
}

// TenantsForCollections descobre tenants com trabalho de cobrança.
func (a *Collections) TenantsForCollections(ctx context.Context, horizon time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := a.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		out, err = q.TenantsForCollections(ctx, dbgen.TenantsForCollectionsParams{Horizon: horizon, AfterTenant: after, MaxTenants: int32(limit)})
		return err
	})
	return out, err
}

// Suspensions lista as suspensões da política de uma assinatura (consulta e testes).
func (a *Collections) Suspensions(ctx context.Context, tenantID, subscriptionID uuid.UUID) ([]collections.Suspension, error) {
	var out []collections.Suspension
	err := a.s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListCollectionSuspensions(ctx, dbgen.ListCollectionSuspensionsParams{TenantID: tenantID, SubscriptionID: subscriptionID})
		for _, r := range rows {
			out = append(out, suspensionFromRow(r))
		}
		return err
	})
	return out, err
}

func (tx collectionsTx) CustomTemplate(ctx context.Context, key notifications.TemplateKey) (*notifications.Template, error) {
	return tx.customTemplate(ctx, key)
}

func (tx collectionsTx) NotificationExists(ctx context.Context, businessKey string) (bool, error) {
	return tx.notificationExists(ctx, businessKey)
}

func (tx collectionsTx) InsertNotification(ctx context.Context, n *notifications.Notification) (bool, error) {
	return tx.insertNotification(ctx, n)
}

func (tx collectionsTx) CancelReminders(ctx context.Context, invoiceID uuid.UUID, reason string, now time.Time) (int, error) {
	return tx.cancelReminders(ctx, invoiceID, reason, now)
}

func (tx collectionsTx) Organization(ctx context.Context) (notifications.Organization, error) {
	return tx.organization(ctx)
}

func (tx collectionsTx) CustomerRecipients(ctx context.Context, customerID uuid.UUID) ([]string, error) {
	return tx.customerRecipients(ctx, customerID)
}

func (tx collectionsTx) PlatformPlan(ctx context.Context) (platformbilling.Plan, error) {
	return tx.platformPlan(ctx)
}

// --- políticas ---

func policyFromRow(r dbgen.CollectionPolicy) collections.Policy {
	offsets := make([]int, len(r.ReminderOffsetsDays))
	for i, o := range r.ReminderOffsetsDays {
		offsets[i] = int(o)
	}
	return collections.Policy{ID: r.ID, TenantID: r.TenantID, Name: r.Name, IsDefault: r.IsDefault, OffsetsDays: offsets,
		Window:            notifications.Window{StartHour: int(r.SendWindowStartHour), EndHour: int(r.SendWindowEndHour)},
		MaxMessagesPerDay: int(r.MaxMessagesPerDay), SuspendAfterDays: intFromPtr32(r.SuspendAfterDays), ReactivateOnPayment: r.ReactivateOnPayment,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Version: int(r.Version)}
}

func offsetsToPg(o []int) []int32 {
	out := make([]int32, len(o))
	for i, v := range o {
		out[i] = int32(v)
	}
	return out
}

func policyNameTaken(err error) error {
	if postgres.IsUniqueViolation(err, "collection_policies_name_key") {
		return apperr.Conflict("collection_policy_name_taken", "Já existe uma política de cobrança com este nome.")
	}
	return err
}

func (tx collectionsTx) LockPolicies(ctx context.Context) error {
	return tx.q.LockCollectionPolicies(ctx, "collection_policies:"+tx.tenantID.String())
}

func (tx collectionsTx) Policies(ctx context.Context) ([]collections.Policy, error) {
	rows, err := tx.q.ListCollectionPolicies(ctx, tx.tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]collections.Policy, 0, len(rows))
	for _, r := range rows {
		out = append(out, policyFromRow(r))
	}
	return out, nil
}

func (tx collectionsTx) Policy(ctx context.Context, id uuid.UUID, forUpdate bool) (collections.Policy, error) {
	var r dbgen.CollectionPolicy
	var err error
	if forUpdate {
		r, err = tx.q.GetCollectionPolicyForUpdate(ctx, dbgen.GetCollectionPolicyForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetCollectionPolicy(ctx, dbgen.GetCollectionPolicyParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return collections.Policy{}, notFound(err, "collection_policy_not_found", "Política de cobrança não encontrada.")
	}
	return policyFromRow(r), nil
}

func (tx collectionsTx) DefaultPolicy(ctx context.Context) (*collections.Policy, error) {
	r, err := tx.q.GetDefaultCollectionPolicy(ctx, tx.tenantID)
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p := policyFromRow(r)
	return &p, nil
}

func (tx collectionsTx) InsertPolicy(ctx context.Context, p collections.Policy, createdBy *uuid.UUID) error {
	return policyNameTaken(tx.q.InsertCollectionPolicy(ctx, dbgen.InsertCollectionPolicyParams{ID: p.ID, TenantID: tx.tenantID, Name: p.Name,
		IsDefault: p.IsDefault, ReminderOffsetsDays: offsetsToPg(p.OffsetsDays), SendWindowStartHour: int32(p.Window.StartHour),
		SendWindowEndHour: int32(p.Window.EndHour), MaxMessagesPerDay: int32(p.MaxMessagesPerDay), SuspendAfterDays: intPtr32(p.SuspendAfterDays),
		ReactivateOnPayment: p.ReactivateOnPayment, CreatedBy: createdBy, CreatedAt: p.CreatedAt}))
}

func (tx collectionsTx) UpdatePolicy(ctx context.Context, p collections.Policy, expectedVersion int) error {
	n, err := tx.q.UpdateCollectionPolicy(ctx, dbgen.UpdateCollectionPolicyParams{Name: p.Name, IsDefault: p.IsDefault,
		ReminderOffsetsDays: offsetsToPg(p.OffsetsDays), SendWindowStartHour: int32(p.Window.StartHour), SendWindowEndHour: int32(p.Window.EndHour),
		MaxMessagesPerDay: int32(p.MaxMessagesPerDay), SuspendAfterDays: intPtr32(p.SuspendAfterDays), ReactivateOnPayment: p.ReactivateOnPayment,
		UpdatedAt: p.UpdatedAt, TenantID: tx.tenantID, ID: p.ID, Version: int32(expectedVersion)})
	if err != nil {
		return policyNameTaken(err)
	}
	return versionConflict(n, "A política foi alterada por outra pessoa.")
}

func (tx collectionsTx) ClearDefaultPolicy(ctx context.Context, keepID uuid.UUID) error {
	return tx.q.ClearDefaultCollectionPolicy(ctx, dbgen.ClearDefaultCollectionPolicyParams{UpdatedAt: tx.store.clock.Now(), TenantID: tx.tenantID,
		KeepID: keepID})
}

// --- faturas e inadimplência ---

func (tx collectionsTx) ReminderCandidates(ctx context.Context, horizon time.Time, after *collections.InvoiceCursor, limit int) ([]notifications.InvoiceInfo, error) {
	p := dbgen.ReminderCandidatesParams{TenantID: tx.tenantID, Horizon: &horizon, RowLimit: int32(limit)}
	if after != nil {
		p.AfterDueAt, p.AfterID = &after.DueAt, &after.ID
	}
	rows, err := tx.q.ReminderCandidates(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]notifications.InvoiceInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, invoiceInfoFromRow(dbgen.GetInvoiceNotificationInfoRow(r)))
	}
	return out, nil
}

func (tx collectionsTx) Invoice(ctx context.Context, id uuid.UUID) (notifications.InvoiceInfo, error) {
	return tx.invoiceInfo(ctx, id)
}

func (tx collectionsTx) LockInvoice(ctx context.Context, id uuid.UUID) (notifications.InvoiceInfo, error) {
	return tx.lockInvoiceInfo(ctx, id)
}

func (tx collectionsTx) LockOverdueInvoices(ctx context.Context, subscriptionID uuid.UUID, now time.Time) ([]notifications.InvoiceInfo, error) {
	rows, err := tx.q.LockOverdueSubscriptionInvoices(ctx, dbgen.LockOverdueSubscriptionInvoicesParams{TenantID: tx.tenantID,
		SubscriptionID: &subscriptionID, Now: &now})
	if err != nil {
		return nil, err
	}
	out := make([]notifications.InvoiceInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, invoiceInfoFromRow(dbgen.GetInvoiceNotificationInfoRow(r)))
	}
	return out, nil
}

func (tx collectionsTx) SubscriptionsToReview(ctx context.Context, now time.Time, suspendBefore *time.Time, limit int) ([]uuid.UUID, error) {
	return tx.q.SubscriptionsToReview(ctx, dbgen.SubscriptionsToReviewParams{TenantID: tx.tenantID, Now: &now, SuspendBefore: suspendBefore,
		RowLimit: int32(limit)})
}

func (tx collectionsTx) UnmarkedOverdueInvoices(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	return tx.q.UnmarkedOverdueInvoices(ctx, dbgen.UnmarkedOverdueInvoicesParams{TenantID: tx.tenantID, Now: &now, RowLimit: int32(limit)})
}

func (tx collectionsTx) SuspendedByPolicy(ctx context.Context, limit int) ([]uuid.UUID, error) {
	return tx.q.SuspendedByPolicy(ctx, dbgen.SuspendedByPolicyParams{TenantID: tx.tenantID, RowLimit: int32(limit)})
}

func (tx collectionsTx) MarkDelinquent(ctx context.Context, inv notifications.InvoiceInfo, now time.Time) (bool, error) {
	n, err := tx.q.InsertInvoiceDelinquency(ctx, dbgen.InsertInvoiceDelinquencyParams{TenantID: tx.tenantID, InvoiceID: inv.ID,
		SubscriptionID: inv.SubscriptionID, DueAt: *inv.DueAt, DetectedAt: now})
	return n == 1, err
}

func (tx collectionsTx) ResolveDelinquencies(ctx context.Context, now time.Time) (int, error) {
	n, err := tx.q.ResolveInvoiceDelinquencies(ctx, dbgen.ResolveInvoiceDelinquenciesParams{Now: &now, TenantID: tx.tenantID})
	return int(n), err
}

// --- assinaturas e suspensões ---

func (tx collectionsTx) Subscription(ctx context.Context, id uuid.UUID, forUpdate bool) (subscriptions.Subscription, error) {
	return tx.subscription(ctx, id, forUpdate)
}

func (tx collectionsTx) UpdateSubscription(ctx context.Context, s subscriptions.Subscription) error {
	return tx.updateSubscription(ctx, s)
}

func (tx collectionsTx) InsertSubscriptionHistory(ctx context.Context, subscriptionID uuid.UUID, actor audit.Actor, ev []subscriptions.HistoryEvent) error {
	return tx.insertSubscriptionHistory(ctx, subscriptionID, actor, ev)
}

func suspensionFromRow(r dbgen.CollectionSuspension) collections.Suspension {
	return collections.Suspension{ID: r.ID, SubscriptionID: r.SubscriptionID, PolicyID: r.PolicyID, InvoiceID: r.InvoiceID,
		DaysOverdue: int(r.DaysOverdue), Status: r.Status, SuspendedAt: r.SuspendedAt, EndedAt: r.EndedAt, EndReason: deref(r.EndReason)}
}

func (tx collectionsTx) ActiveSuspension(ctx context.Context, subscriptionID uuid.UUID) (*collections.Suspension, error) {
	r, err := tx.q.GetActiveCollectionSuspension(ctx, dbgen.GetActiveCollectionSuspensionParams{TenantID: tx.tenantID, SubscriptionID: subscriptionID})
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s := suspensionFromRow(r)
	return &s, nil
}

func (tx collectionsTx) ReleasedSuspensionFor(ctx context.Context, subscriptionID, invoiceID uuid.UUID) (bool, error) {
	return tx.q.SuspensionReleasedForInvoice(ctx, dbgen.SuspensionReleasedForInvoiceParams{TenantID: tx.tenantID, SubscriptionID: subscriptionID,
		InvoiceID: invoiceID})
}

func (tx collectionsTx) InsertSuspension(ctx context.Context, s collections.Suspension) error {
	return tx.q.InsertCollectionSuspension(ctx, dbgen.InsertCollectionSuspensionParams{ID: s.ID, TenantID: tx.tenantID, SubscriptionID: s.SubscriptionID,
		PolicyID: s.PolicyID, InvoiceID: s.InvoiceID, DaysOverdue: int32(s.DaysOverdue), SuspendedAt: s.SuspendedAt})
}

func (tx collectionsTx) EndSuspension(ctx context.Context, s collections.Suspension) error {
	n, err := tx.q.EndCollectionSuspension(ctx, dbgen.EndCollectionSuspensionParams{Status: s.Status, EndedAt: s.EndedAt, EndReason: strPtr(s.EndReason),
		TenantID: tx.tenantID, ID: s.ID})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.Conflict("collection_suspension_ended", "A suspensão já foi encerrada.")
	}
	return nil
}
