package pgstore

import (
	"context"
	"encoding/json"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// --- condições programadas (compartilhadas) ---

func scheduledChangeFromRow(r dbgen.SubscriptionScheduledChange) subscriptions.ScheduledChange {
	return subscriptions.ScheduledChange{ID: r.ID, TenantID: r.TenantID, SubscriptionID: r.SubscriptionID, Kind: subscriptions.ChangeKind(r.Kind),
		NewPlanID: r.NewPlanID, NewPriceVersionID: r.NewPriceVersionID, NewQuantity: intFromPtr32(r.NewQuantity), EffectiveAt: r.EffectiveAt,
		Status: subscriptions.ChangeStatus(r.Status), CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, ResolvedAt: r.ResolvedAt}
}

func optMoney(v *int64) *money.Money {
	if v == nil {
		return nil
	}
	m := brl(*v)
	return &m
}

func optAmount(m *money.Money) *int64 {
	if m == nil {
		return nil
	}
	v := m.Amount()
	return &v
}

func adjustmentFromRow(r dbgen.SubscriptionPriceAdjustment) subscriptions.PriceAdjustment {
	return subscriptions.PriceAdjustment{ID: r.ID, TenantID: r.TenantID, SubscriptionID: r.SubscriptionID, EffectiveDate: dateFromPg(r.EffectiveDate),
		Kind: subscriptions.AdjustmentKind(r.Kind), PercentBP: intFromPtr32(r.PercentBp), NewAmount: optMoney(r.NewAmount), IndexName: r.IndexName,
		Reason: r.Reason, Status: subscriptions.AdjustmentStatus(r.Status), PreviousAmount: optMoney(r.PreviousAmount),
		ResultingAmount: optMoney(r.ResultingAmount), AppliedPeriodStart: r.AppliedPeriodStart, AppliedAt: r.AppliedAt, CanceledAt: r.CanceledAt,
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt}
}

func (b base) pendingChange(ctx context.Context, subscriptionID uuid.UUID) (*subscriptions.ScheduledChange, error) {
	r, err := b.q.GetPendingScheduledChange(ctx, dbgen.GetPendingScheduledChangeParams{TenantID: b.tenantID, SubscriptionID: subscriptionID})
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := scheduledChangeFromRow(r)
	return &c, nil
}

func (b base) cancelPendingTerms(ctx context.Context, subscriptionID uuid.UUID, at time.Time) error {
	if _, err := b.q.CancelPendingScheduledChanges(ctx, dbgen.CancelPendingScheduledChangesParams{ResolvedAt: &at, TenantID: b.tenantID,
		SubscriptionID: subscriptionID}); err != nil {
		return err
	}
	_, err := b.q.CancelScheduledPriceAdjustments(ctx, dbgen.CancelScheduledPriceAdjustmentsParams{CanceledAt: &at, TenantID: b.tenantID,
		SubscriptionID: subscriptionID})
	return err
}

func (b base) endActiveDiscount(ctx context.Context, subscriptionID uuid.UUID, at time.Time) (bool, error) {
	n, err := b.q.EndActiveDiscount(ctx, dbgen.EndActiveDiscountParams{Now: &at, TenantID: b.tenantID, SubscriptionID: subscriptionID})
	return n > 0, err
}

// --- motor de recorrência, mudanças, correções e notas de crédito ---

// InChangeTx abre a transação de mudança de plano ou quantidade.
func (a *Billing) InChangeTx(ctx context.Context, tenantID uuid.UUID, fn func(billing.ChangeTx) error) error {
	return a.s.inTenant(ctx, tenantID, func(b base) error { return fn(billingTx{b}) })
}

// InCorrectionTx abre a transação de correção de consumo.
func (a *Billing) InCorrectionTx(ctx context.Context, tenantID uuid.UUID, fn func(billing.CorrectionTx) error) error {
	return a.s.inTenant(ctx, tenantID, func(b base) error { return fn(billingTx{b}) })
}

// InCreditNoteTx abre a transação de notas de crédito.
func (a *Billing) InCreditNoteTx(ctx context.Context, tenantID uuid.UUID, fn func(billing.CreditNoteTx) error) error {
	return a.s.inTenant(ctx, tenantID, func(b base) error { return fn(billingTx{b}) })
}

// TenantsWithLateUsage descobre tenants com consumo atrasado a corrigir.
func (a *Billing) TenantsWithLateUsage(ctx context.Context) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := a.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		out, err = q.TenantsWithLateUsage(ctx)
		return err
	})
	return out, err
}

func (tx billingTx) PendingChange(ctx context.Context, subscriptionID uuid.UUID) (*subscriptions.ScheduledChange, error) {
	return tx.pendingChange(ctx, subscriptionID)
}

func (tx billingTx) ResolveChange(ctx context.Context, id uuid.UUID, status subscriptions.ChangeStatus, at time.Time) error {
	n, err := tx.q.ResolveScheduledChange(ctx, dbgen.ResolveScheduledChangeParams{Status: string(status), ResolvedAt: &at, TenantID: tx.tenantID, ID: id})
	if err != nil {
		return err
	}
	return versionConflict(n, "A mudança agendada já foi resolvida.")
}

func (tx billingTx) DueAdjustments(ctx context.Context, subscriptionID uuid.UUID, onDate calendar.Date) ([]subscriptions.PriceAdjustment, error) {
	rows, err := tx.q.DuePriceAdjustments(ctx, dbgen.DuePriceAdjustmentsParams{TenantID: tx.tenantID, SubscriptionID: subscriptionID, OnDate: dateToPg(onDate)})
	if err != nil {
		return nil, err
	}
	out := make([]subscriptions.PriceAdjustment, len(rows))
	for i, r := range rows {
		out[i] = adjustmentFromRow(r)
	}
	return out, nil
}

func (tx billingTx) MarkAdjustmentApplied(ctx context.Context, a subscriptions.PriceAdjustment) error {
	n, err := tx.q.MarkPriceAdjustmentApplied(ctx, dbgen.MarkPriceAdjustmentAppliedParams{PreviousAmount: optAmount(a.PreviousAmount),
		ResultingAmount: optAmount(a.ResultingAmount), AppliedPeriodStart: a.AppliedPeriodStart, AppliedAt: a.AppliedAt, TenantID: tx.tenantID, ID: a.ID})
	if err != nil {
		return err
	}
	return versionConflict(n, "O reajuste já foi aplicado ou cancelado.")
}

func (tx billingTx) CancelPendingTerms(ctx context.Context, subscriptionID uuid.UUID, at time.Time) error {
	return tx.cancelPendingTerms(ctx, subscriptionID, at)
}

func (tx billingTx) EndActiveDiscount(ctx context.Context, subscriptionID uuid.UUID, at time.Time) (bool, error) {
	return tx.endActiveDiscount(ctx, subscriptionID, at)
}

func (tx billingTx) Subscription(ctx context.Context, id uuid.UUID, forUpdate bool) (subscriptions.Subscription, error) {
	return tx.subscription(ctx, id, forUpdate)
}

func (tx billingTx) PlatformPlan(ctx context.Context) (platformbilling.Plan, error) {
	return tx.platformPlan(ctx)
}

func (tx billingTx) InsertScheduledChange(ctx context.Context, c subscriptions.ScheduledChange) error {
	return tx.q.InsertScheduledChange(ctx, dbgen.InsertScheduledChangeParams{ID: c.ID, TenantID: tx.tenantID, SubscriptionID: c.SubscriptionID,
		Kind: string(c.Kind), NewPlanID: c.NewPlanID, NewPriceVersionID: c.NewPriceVersionID, NewQuantity: intPtr32(c.NewQuantity),
		EffectiveAt: c.EffectiveAt, CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt})
}

func (tx billingTx) CancelPendingChanges(ctx context.Context, subscriptionID uuid.UUID, at time.Time) error {
	_, err := tx.q.CancelPendingScheduledChanges(ctx, dbgen.CancelPendingScheduledChangesParams{ResolvedAt: &at, TenantID: tx.tenantID,
		SubscriptionID: subscriptionID})
	return err
}

func (tx billingTx) AddCustomerCredit(ctx context.Context, customerID uuid.UUID, amount money.Money, kind, reason string, invoiceID *uuid.UUID) (uuid.UUID, error) {
	id, _, err := tx.addCreditKeyed(ctx, customerID, amount.Amount(), kind, invoiceID, reason, nil, "")
	return id, err
}

func (tx billingTx) LateUsageGroups(ctx context.Context, limit int) ([]billing.LateGroup, error) {
	rows, err := tx.q.LateUsageGroups(ctx, dbgen.LateUsageGroupsParams{TenantID: tx.tenantID, RowLimit: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]billing.LateGroup, len(rows))
	for i, r := range rows {
		out[i] = billing.LateGroup{SubscriptionID: r.SubscriptionID, MetricID: r.MetricID, PeriodStart: r.PeriodStart}
	}
	return out, nil
}

func (tx billingTx) ClosedAggregate(ctx context.Context, g billing.LateGroup) (billing.ClosedAggregate, error) {
	r, err := tx.q.GetUsageAggregateForUpdate(ctx, dbgen.GetUsageAggregateForUpdateParams{TenantID: tx.tenantID, SubscriptionID: g.SubscriptionID,
		MetricID: g.MetricID, PeriodStart: g.PeriodStart})
	if err != nil {
		return billing.ClosedAggregate{}, notFound(err, "usage_aggregate_not_found", "Agregado de consumo não encontrado.")
	}
	qty, err := ratFromNumeric(r.Quantity)
	if err != nil {
		return billing.ClosedAggregate{}, err
	}
	corrected, err := ratFromNumeric(r.CorrectedQuantity)
	if err != nil {
		return billing.ClosedAggregate{}, err
	}
	return billing.ClosedAggregate{ID: r.ID, SubscriptionID: r.SubscriptionID, MetricID: r.MetricID,
		Period: calendar.Period{Start: r.PeriodStart, End: r.PeriodEnd}, Quantity: qty, InvoiceItemID: r.InvoiceItemID,
		PriceVersionID: r.PriceVersionID, CorrectedQuantity: corrected, CorrectedAmount: optMoney(r.CorrectedAmount)}, nil
}

func (tx billingTx) Metric(ctx context.Context, id uuid.UUID) (catalog.Metric, error) {
	return tx.metric(ctx, id)
}

func (tx billingTx) RecomputeUsage(ctx context.Context, subscriptionID uuid.UUID, metric catalog.Metric, periodStart time.Time) (*big.Rat, int, error) {
	return tx.aggregate(ctx, subscriptionID, metric, periodStart)
}

func (tx billingTx) InvoiceItemAmount(ctx context.Context, id uuid.UUID) (money.Money, error) {
	v, err := tx.q.GetInvoiceItemAmount(ctx, dbgen.GetInvoiceItemAmountParams{TenantID: tx.tenantID, ID: id})
	if err != nil {
		return money.Money{}, notFound(err, "invoice_item_not_found", "Item de fatura não encontrado.")
	}
	return brl(v), nil
}

func (tx billingTx) SetAggregateCorrection(ctx context.Context, id uuid.UUID, qty *big.Rat, amount money.Money) error {
	num, err := numericFromRat(qty)
	if err != nil {
		return err
	}
	v := amount.Amount()
	return tx.q.SetUsageAggregateCorrection(ctx, dbgen.SetUsageAggregateCorrectionParams{CorrectedQuantity: num, CorrectedAmount: &v,
		TenantID: tx.tenantID, ID: id})
}

func (tx billingTx) MarkLateCorrected(ctx context.Context, g billing.LateGroup, aggregateID uuid.UUID, itemID *uuid.UUID, at time.Time) error {
	_, err := tx.q.MarkLateUsageCorrected(ctx, dbgen.MarkLateUsageCorrectedParams{AggregateID: &aggregateID, CorrectedAt: &at, CorrectionItemID: itemID,
		TenantID: tx.tenantID, SubscriptionID: g.SubscriptionID, MetricID: g.MetricID, PeriodStart: g.PeriodStart})
	return err
}

func (tx billingTx) InsertPendingItem(ctx context.Context, it billing.NewPendingItem) error {
	details, err := json.Marshal(it.Details)
	if err != nil {
		return err
	}
	qty, err := numericPtr(it.Quantity)
	if err != nil {
		return err
	}
	var ps, pe *time.Time
	if it.Period != nil {
		ps, pe = &it.Period.Start, &it.Period.End
	}
	if err := tx.q.InsertPendingItem(ctx, dbgen.InsertPendingItemParams{ID: it.ID, TenantID: tx.tenantID, CustomerID: it.CustomerID,
		SubscriptionID: it.SubscriptionID, Kind: string(it.Kind), Source: it.Source, Description: it.Description, Amount: it.Amount.Amount(),
		Quantity: qty, PeriodStart: ps, PeriodEnd: pe, Details: details, UsageAggregateID: it.UsageAggregateID, CreatedAt: tx.store.clock.Now()}); err != nil {
		return err
	}
	return nil
}

func creditNoteFromRow(r dbgen.CreditNote) billing.CreditNote {
	return billing.CreditNote{ID: r.ID, TenantID: r.TenantID, InvoiceID: r.InvoiceID, Number: r.Number, Amount: brl(r.Amount), Reason: r.Reason,
		CreatedBy: r.CreatedBy, RequestKey: deref(r.RequestKey), CreatedAt: r.CreatedAt}
}

func (tx billingTx) InsertCreditNote(ctx context.Context, cn billing.CreditNote) (bool, error) {
	n, err := tx.q.InsertCreditNote(ctx, dbgen.InsertCreditNoteParams{ID: cn.ID, TenantID: tx.tenantID, InvoiceID: cn.InvoiceID, Number: cn.Number,
		Amount: cn.Amount.Amount(), Currency: cn.Amount.Currency().Code(), Reason: cn.Reason, CreatedBy: cn.CreatedBy, RequestKey: strPtr(cn.RequestKey),
		CreatedAt: cn.CreatedAt})
	return n == 1, err
}

func (tx billingTx) CreditNoteByRequestKey(ctx context.Context, key string) (billing.CreditNote, error) {
	r, err := tx.q.GetCreditNoteByRequestKey(ctx, dbgen.GetCreditNoteByRequestKeyParams{TenantID: tx.tenantID, RequestKey: &key})
	if err != nil {
		return billing.CreditNote{}, notFound(err, "credit_note_not_found", "Nota de crédito não encontrada.")
	}
	return creditNoteFromRow(r), nil
}

func (tx billingTx) CountCreditNotes(ctx context.Context, invoiceID uuid.UUID) (int, error) {
	n, err := tx.q.CountInvoiceCreditNotes(ctx, dbgen.CountInvoiceCreditNotesParams{TenantID: tx.tenantID, InvoiceID: invoiceID})
	return int(n), err
}

func (tx billingTx) RefreshSubscriptionStanding(ctx context.Context, subscriptionID uuid.UUID, actor audit.Actor, now time.Time) error {
	return tx.refreshStanding(ctx, subscriptionID, actor, now)
}

// CreditNotes lista as notas de crédito de uma fatura.
func (a *Billing) CreditNotes(ctx context.Context, tenantID, invoiceID uuid.UUID) ([]billing.CreditNote, error) {
	var out []billing.CreditNote
	err := a.s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListInvoiceCreditNotes(ctx, dbgen.ListInvoiceCreditNotesParams{TenantID: b.tenantID, InvoiceID: invoiceID})
		if err != nil {
			return err
		}
		out = make([]billing.CreditNote, len(rows))
		for i, r := range rows {
			out[i] = creditNoteFromRow(r)
		}
		return nil
	})
	return out, err
}

// --- cancelamento de condições a partir do serviço de assinaturas ---

func (tx subscriptionsTx) CancelPendingTerms(ctx context.Context, subscriptionID uuid.UUID, at time.Time) error {
	return tx.cancelPendingTerms(ctx, subscriptionID, at)
}

// --- condições comerciais da assinatura ---

type termsTx struct{ base }

// InTermsTx abre a transação de cupons aplicados e reajustes.
func (a *Subscriptions) InTermsTx(ctx context.Context, tenantID uuid.UUID, fn func(subscriptions.TermsTx) error) error {
	return a.s.inTenant(ctx, tenantID, func(b base) error { return fn(termsTx{b}) })
}

func (tx termsTx) Get(ctx context.Context, id uuid.UUID, forUpdate bool) (subscriptions.Subscription, error) {
	return tx.subscription(ctx, id, forUpdate)
}

func (tx termsTx) PriceVersion(ctx context.Context, id uuid.UUID) (catalog.PriceVersion, error) {
	return tx.priceVersion(ctx, id)
}

func (tx termsTx) CouponByCode(ctx context.Context, code string) (catalog.Coupon, error) {
	r, err := tx.q.GetCouponByCodeForUpdate(ctx, dbgen.GetCouponByCodeForUpdateParams{TenantID: tx.tenantID, Code: code})
	if err != nil {
		return catalog.Coupon{}, notFound(err, "coupon_not_found", "Cupom não encontrado.")
	}
	return couponFromRow(r), nil
}

func (tx termsTx) RedeemCoupon(ctx context.Context, couponID uuid.UUID) (bool, error) {
	n, err := tx.q.RedeemCoupon(ctx, dbgen.RedeemCouponParams{TenantID: tx.tenantID, ID: couponID})
	return n == 1, err
}

func (tx termsTx) ActiveDiscount(ctx context.Context, subscriptionID uuid.UUID) (*subscriptions.Discount, error) {
	r, err := tx.q.GetActiveDiscount(ctx, dbgen.GetActiveDiscountParams{TenantID: tx.tenantID, SubscriptionID: subscriptionID})
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &subscriptions.Discount{ID: r.ID, SubscriptionID: r.SubscriptionID, CouponID: r.CouponID, CouponCode: r.CouponCode,
		RemainingCycles: intFromPtr32(r.RemainingCycles), Status: subscriptions.DiscountStatus(r.Status), CreatedAt: r.CreatedAt}, nil
}

func (tx termsTx) InsertDiscount(ctx context.Context, d subscriptions.Discount, appliedBy *uuid.UUID) error {
	err := tx.q.InsertSubscriptionDiscount(ctx, dbgen.InsertSubscriptionDiscountParams{ID: d.ID, TenantID: tx.tenantID, SubscriptionID: d.SubscriptionID,
		CouponID: d.CouponID, RemainingCycles: intPtr32(d.RemainingCycles), AppliedBy: appliedBy, CreatedAt: d.CreatedAt})
	if postgres.IsUniqueViolation(err, "subscription_discounts_active_key") {
		return apperr.Conflict("discount_already_active", "A assinatura já tem um desconto ativo.")
	}
	return err
}

func (tx termsTx) EndActiveDiscount(ctx context.Context, subscriptionID uuid.UUID, at time.Time) (bool, error) {
	return tx.endActiveDiscount(ctx, subscriptionID, at)
}

func (tx termsTx) InsertAdjustment(ctx context.Context, a subscriptions.PriceAdjustment) error {
	err := tx.q.InsertPriceAdjustment(ctx, dbgen.InsertPriceAdjustmentParams{ID: a.ID, TenantID: tx.tenantID, SubscriptionID: a.SubscriptionID,
		EffectiveDate: dateToPg(a.EffectiveDate), Kind: string(a.Kind), PercentBp: intPtr32(a.PercentBP), NewAmount: optAmount(a.NewAmount),
		IndexName: a.IndexName, Reason: a.Reason, CreatedBy: a.CreatedBy, CreatedAt: a.CreatedAt})
	if postgres.IsUniqueViolation(err, "subscription_price_adjustments_date_key") {
		return apperr.Conflict("adjustment_date_taken", "Já existe um reajuste programado para esta data.")
	}
	return err
}

func (tx termsTx) Adjustments(ctx context.Context, subscriptionID uuid.UUID) ([]subscriptions.PriceAdjustment, error) {
	rows, err := tx.q.ListPriceAdjustments(ctx, dbgen.ListPriceAdjustmentsParams{TenantID: tx.tenantID, SubscriptionID: subscriptionID})
	if err != nil {
		return nil, err
	}
	out := make([]subscriptions.PriceAdjustment, len(rows))
	for i, r := range rows {
		out[i] = adjustmentFromRow(r)
	}
	return out, nil
}

func (tx termsTx) Adjustment(ctx context.Context, subscriptionID, id uuid.UUID) (subscriptions.PriceAdjustment, error) {
	r, err := tx.q.GetPriceAdjustmentForUpdate(ctx, dbgen.GetPriceAdjustmentForUpdateParams{TenantID: tx.tenantID, SubscriptionID: subscriptionID, ID: id})
	if err != nil {
		return subscriptions.PriceAdjustment{}, notFound(err, "adjustment_not_found", "Reajuste não encontrado.")
	}
	return adjustmentFromRow(r), nil
}

func (tx termsTx) CancelAdjustment(ctx context.Context, a subscriptions.PriceAdjustment) error {
	n, err := tx.q.CancelPriceAdjustment(ctx, dbgen.CancelPriceAdjustmentParams{CanceledAt: a.CanceledAt, TenantID: tx.tenantID, ID: a.ID})
	if err != nil {
		return err
	}
	return versionConflict(n, "O reajuste já foi aplicado ou cancelado.")
}

func (tx termsTx) ScheduledChanges(ctx context.Context, subscriptionID uuid.UUID) ([]subscriptions.ScheduledChange, error) {
	rows, err := tx.q.ListScheduledChanges(ctx, dbgen.ListScheduledChangesParams{TenantID: tx.tenantID, SubscriptionID: subscriptionID})
	if err != nil {
		return nil, err
	}
	out := make([]subscriptions.ScheduledChange, len(rows))
	for i, r := range rows {
		out[i] = scheduledChangeFromRow(r)
	}
	return out, nil
}

func (tx termsTx) InsertHistory(ctx context.Context, subscriptionID uuid.UUID, actor audit.Actor, ev []subscriptions.HistoryEvent) error {
	return tx.insertSubscriptionHistory(ctx, subscriptionID, actor, ev)
}

func (tx termsTx) Emit(ctx context.Context, e outbox.Event) error { return tx.base.Emit(ctx, e) }

// --- cupons ---

type couponTx struct{ base }

// InCouponTx abre a transação de cupons.
func (c *Catalog) InCouponTx(ctx context.Context, tenantID uuid.UUID, fn func(catalog.CouponTx) error) error {
	return c.s.inTenant(ctx, tenantID, func(b base) error { return fn(couponTx{b}) })
}

func couponFromRow(r dbgen.Coupon) catalog.Coupon {
	return catalog.Coupon{ID: r.ID, TenantID: r.TenantID, Code: r.Code, Name: r.Name, Type: catalog.DiscountType(r.DiscountType),
		PercentOffBP: intFromPtr32(r.PercentOffBp), AmountOff: optMoney(r.AmountOff), Currency: money.BRL, Duration: catalog.CouponDuration(r.Duration),
		DurationCycles: intFromPtr32(r.DurationCycles), MaxRedemptions: intFromPtr32(r.MaxRedemptions), RedemptionsCount: int(r.RedemptionsCount),
		ValidFrom: r.ValidFrom, ValidUntil: r.ValidUntil, AppliesToPlanIDs: r.AppliesToPlanIds, Status: catalog.Status(r.Status),
		CreatedAt: r.CreatedAt, Version: int(r.Version)}
}

func (tx couponTx) InsertCoupon(ctx context.Context, c catalog.Coupon) error {
	err := tx.q.InsertCoupon(ctx, dbgen.InsertCouponParams{ID: c.ID, TenantID: tx.tenantID, Code: c.Code, Name: c.Name, DiscountType: string(c.Type),
		PercentOffBp: intPtr32(c.PercentOffBP), AmountOff: optAmount(c.AmountOff), Currency: c.Currency.Code(), Duration: string(c.Duration),
		DurationCycles: intPtr32(c.DurationCycles), MaxRedemptions: intPtr32(c.MaxRedemptions), ValidFrom: c.ValidFrom, ValidUntil: c.ValidUntil,
		AppliesToPlanIds: c.AppliesToPlanIDs, CreatedAt: c.CreatedAt})
	if postgres.IsUniqueViolation(err, "coupons_code_key") {
		return apperr.Conflict("coupon_code_taken", "Já existe um cupom com este código.")
	}
	return err
}

func (tx couponTx) Coupon(ctx context.Context, id uuid.UUID, forUpdate bool) (catalog.Coupon, error) {
	var (
		r   dbgen.Coupon
		err error
	)
	if forUpdate {
		r, err = tx.q.GetCouponForUpdate(ctx, dbgen.GetCouponForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetCoupon(ctx, dbgen.GetCouponParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return catalog.Coupon{}, notFound(err, "coupon_not_found", "Cupom não encontrado.")
	}
	return couponFromRow(r), nil
}

func (tx couponTx) Coupons(ctx context.Context) ([]catalog.Coupon, error) {
	rows, err := tx.q.ListCoupons(ctx, tx.tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]catalog.Coupon, len(rows))
	for i, r := range rows {
		out[i] = couponFromRow(r)
	}
	return out, nil
}

func (tx couponTx) ArchiveCoupon(ctx context.Context, id uuid.UUID) error {
	n, err := tx.q.ArchiveCoupon(ctx, dbgen.ArchiveCouponParams{TenantID: tx.tenantID, ID: id})
	if err != nil {
		return err
	}
	return versionConflict(n, "O cupom foi alterado por outra pessoa.")
}

func (tx couponTx) CountPlans(ctx context.Context, planIDs []uuid.UUID) (int, error) {
	n, err := tx.q.CountPlansByIDs(ctx, dbgen.CountPlansByIDsParams{TenantID: tx.tenantID, Ids: planIDs})
	return int(n), err
}

// --- consumo medido ---

// Metering é o adaptador do módulo de consumo.
type Metering struct{ s *Store }

// Metering retorna o adaptador de consumo.
func (s *Store) Metering() *Metering { return &Metering{s: s} }

type meteringTx struct{ base }

// InTenant abre a transação do tenant.
func (m *Metering) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(metering.Tx) error) error {
	return m.s.inTenant(ctx, tenantID, func(b base) error { return fn(meteringTx{b}) })
}

func (tx meteringTx) Subscription(ctx context.Context, id uuid.UUID) (subscriptions.Subscription, error) {
	return tx.subscription(ctx, id, false)
}

func (tx meteringTx) PriceVersion(ctx context.Context, id uuid.UUID) (catalog.PriceVersion, error) {
	return tx.priceVersion(ctx, id)
}

func (tx meteringTx) Metric(ctx context.Context, id uuid.UUID) (catalog.Metric, error) {
	return tx.metric(ctx, id)
}

func (tx meteringTx) MetricByCode(ctx context.Context, code string) (catalog.Metric, error) {
	r, err := tx.q.GetMetricByCode(ctx, dbgen.GetMetricByCodeParams{TenantID: tx.tenantID, Code: code})
	if err != nil {
		return catalog.Metric{}, notFound(err, "metric_not_found", "Métrica não encontrada.")
	}
	return metricFromRow(r)
}

func usageEventFromRow(r dbgen.UsageEvent, itemID *uuid.UUID) (metering.Event, error) {
	qty, err := ratFromNumeric(r.Quantity)
	if err != nil {
		return metering.Event{}, err
	}
	props, err := jsonMap(r.Properties)
	if err != nil {
		return metering.Event{}, err
	}
	return metering.Event{ID: r.ID, TenantID: r.TenantID, ExternalID: r.ExternalID, CustomerID: r.CustomerID, SubscriptionID: r.SubscriptionID,
		MetricID: r.MetricID, Quantity: qty, UniqueKey: deref(r.UniqueKey), OccurredAt: r.OccurredAt, ReceivedAt: r.ReceivedAt,
		PeriodStart: r.PeriodStart, Status: metering.Status(r.Status), AggregateID: r.AggregateID, InvoiceItemID: itemID, Properties: props}, nil
}

func (tx meteringTx) EventByExternalID(ctx context.Context, externalID string) (metering.Event, error) {
	r, err := tx.q.GetUsageEventByExternalID(ctx, dbgen.GetUsageEventByExternalIDParams{TenantID: tx.tenantID, ExternalID: externalID})
	if err != nil {
		return metering.Event{}, notFound(err, "usage_event_not_found", "Evento de consumo não encontrado.")
	}
	return usageEventFromRow(r, nil)
}

func (tx meteringTx) InsertEvent(ctx context.Context, e metering.Event) (bool, error) {
	qty, err := numericFromRat(e.Quantity)
	if err != nil {
		return false, err
	}
	props := []byte("{}")
	if len(e.Properties) > 0 {
		if props, err = json.Marshal(e.Properties); err != nil {
			return false, err
		}
	}
	n, err := tx.q.InsertUsageEvent(ctx, dbgen.InsertUsageEventParams{ID: e.ID, TenantID: tx.tenantID, ExternalID: e.ExternalID, CustomerID: e.CustomerID,
		SubscriptionID: e.SubscriptionID, MetricID: e.MetricID, Quantity: qty, UniqueKey: strPtr(e.UniqueKey), OccurredAt: e.OccurredAt,
		ReceivedAt: e.ReceivedAt, PeriodStart: e.PeriodStart, Status: string(e.Status), Properties: props})
	return n == 1, err
}

func (tx meteringTx) PeriodClosed(ctx context.Context, subscriptionID, metricID uuid.UUID, periodStart time.Time) (bool, error) {
	return tx.q.UsageAggregateExists(ctx, dbgen.UsageAggregateExistsParams{TenantID: tx.tenantID, SubscriptionID: subscriptionID, MetricID: metricID,
		PeriodStart: periodStart})
}

func (tx meteringTx) Aggregate(ctx context.Context, subscriptionID uuid.UUID, metric catalog.Metric, periodStart time.Time) (*big.Rat, int, error) {
	return tx.aggregate(ctx, subscriptionID, metric, periodStart)
}

func (tx meteringTx) ListEvents(ctx context.Context, f metering.ListFilter) (metering.Page, error) {
	ts, id, err := parseTimeCursor(f.Cursor)
	if err != nil {
		return metering.Page{}, err
	}
	rows, err := tx.q.ListUsageEvents(ctx, dbgen.ListUsageEventsParams{TenantID: tx.tenantID, SubscriptionID: f.SubscriptionID, MetricID: f.MetricID,
		CursorTs: ts, CursorID: id, RowLimit: int32(f.Limit + 1)})
	if err != nil {
		return metering.Page{}, err
	}
	var p metering.Page
	for i, r := range rows {
		if i == f.Limit {
			last := p.Items[len(p.Items)-1]
			p.NextCursor = timeCursor(last.ReceivedAt, last.ID)
			break
		}
		ev, err := usageEventFromRow(dbgen.UsageEvent{ID: r.ID, TenantID: r.TenantID, ExternalID: r.ExternalID, CustomerID: r.CustomerID,
			SubscriptionID: r.SubscriptionID, MetricID: r.MetricID, Quantity: r.Quantity, UniqueKey: r.UniqueKey, OccurredAt: r.OccurredAt,
			ReceivedAt: r.ReceivedAt, PeriodStart: r.PeriodStart, Status: r.Status, AggregateID: r.AggregateID, Properties: r.Properties},
			r.AggregateItemID)
		if err != nil {
			return metering.Page{}, err
		}
		p.Items = append(p.Items, ev)
	}
	return p, nil
}
