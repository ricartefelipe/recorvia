package pgstore

import (
	"context"
	"encoding/json"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// Billing implementa billing.Store e billing.CycleStore.
type Billing struct{ s *Store }

// Billing retorna o adaptador de faturamento.
func (s *Store) Billing() *Billing { return &Billing{s: s} }

type billingTx struct{ base }

// InTenant abre a transação de consulta e manutenção de faturas.
func (a *Billing) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(billing.Tx) error) error {
	return a.s.inTenant(ctx, tenantID, func(b base) error { return fn(billingTx{b}) })
}

// InCycleTx abre a transação do motor de recorrência.
func (a *Billing) InCycleTx(ctx context.Context, tenantID uuid.UUID, fn func(billing.CycleTx) error) error {
	return a.s.inTenant(ctx, tenantID, func(b base) error { return fn(billingTx{b}) })
}

// TenantsWithDueBilling descobre tenants com faturamento vencido.
func (a *Billing) TenantsWithDueBilling(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := a.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		out, err = q.TenantsWithDueBilling(ctx, dbgen.TenantsWithDueBillingParams{Now: now, MaxTenants: int32(limit)})
		return err
	})
	return out, err
}

func (b base) tenantSettings(ctx context.Context) (billing.TenantSettings, error) {
	t, err := b.q.GetTenant(ctx)
	if err != nil {
		return billing.TenantSettings{}, notFound(err, "organization_not_found", "Organização não encontrada.")
	}
	loc, err := time.LoadLocation(t.Timezone)
	if err != nil {
		return billing.TenantSettings{}, err
	}
	return billing.TenantSettings{Location: loc, InvoicePrefix: t.InvoicePrefix, PaymentTermsDays: int(t.PaymentTermsDays)}, nil
}

func (tx billingTx) TenantSettings(ctx context.Context) (billing.TenantSettings, error) {
	return tx.tenantSettings(ctx)
}

func (tx billingTx) ClaimDueSubscription(ctx context.Context, now time.Time, exclude []uuid.UUID) (subscriptions.Subscription, bool, error) {
	if exclude == nil {
		exclude = []uuid.UUID{}
	}
	r, err := tx.q.ClaimDueSubscription(ctx, dbgen.ClaimDueSubscriptionParams{TenantID: tx.tenantID, Now: &now, Exclude: exclude})
	if postgres.IsNoRows(err) {
		return subscriptions.Subscription{}, false, nil
	}
	if err != nil {
		return subscriptions.Subscription{}, false, err
	}
	return subscriptionFromRow(r), true, nil
}

func (tx billingTx) UpdateSubscription(ctx context.Context, s subscriptions.Subscription) error {
	return tx.updateSubscription(ctx, s)
}

func (tx billingTx) InsertSubscriptionHistory(ctx context.Context, id uuid.UUID, actor audit.Actor, ev []subscriptions.HistoryEvent) error {
	return tx.insertSubscriptionHistory(ctx, id, actor, ev)
}

func (tx billingTx) Plan(ctx context.Context, id uuid.UUID) (catalog.Plan, error) {
	return tx.plan(ctx, id)
}

func (tx billingTx) PriceVersion(ctx context.Context, id uuid.UUID) (catalog.PriceVersion, error) {
	return tx.priceVersion(ctx, id)
}

func (b base) customerSnapshot(ctx context.Context, id uuid.UUID, at time.Time) (customers.Snapshot, *int, error) {
	c, err := b.customer(ctx, id)
	if err != nil {
		return customers.Snapshot{}, nil, err
	}
	addr, err := customersTx{b}.Address(ctx, id)
	if err != nil {
		return customers.Snapshot{}, nil, err
	}
	return customers.SnapshotOf(c, addr, at), c.PaymentTermsDays, nil
}

func (tx billingTx) CustomerForInvoice(ctx context.Context, id uuid.UUID, at time.Time) (customers.Snapshot, *int, error) {
	return tx.customerSnapshot(ctx, id, at)
}

// UsageFor fecha o agregado do período para a métrica do preço, conforme a
// semântica de agregação da métrica, e vincula os eventos ao agregado.
func (tx billingTx) UsageFor(ctx context.Context, sub subscriptions.Subscription, price catalog.PriceVersion, period calendar.Period) ([]billing.UsageLine, error) {
	if price.MetricID == nil {
		return nil, nil
	}
	metric, err := tx.metric(ctx, *price.MetricID)
	if err != nil {
		return nil, err
	}
	existing, err := tx.q.GetUsageAggregate(ctx, dbgen.GetUsageAggregateParams{TenantID: tx.tenantID, SubscriptionID: sub.ID, MetricID: metric.ID, PeriodStart: period.Start})
	if err == nil {
		qty, err := ratFromNumeric(existing.Quantity)
		if err != nil {
			return nil, err
		}
		id := existing.ID
		return []billing.UsageLine{{MetricID: metric.ID, MetricCode: metric.Code, MetricName: metric.Name, Unit: metric.Unit,
			Aggregation: string(metric.Aggregation), Quantity: qty, AggregateID: &id}}, nil
	}
	if !postgres.IsNoRows(err) {
		return nil, err
	}
	qty, count, err := tx.aggregate(ctx, sub.ID, metric, period.Start)
	if err != nil {
		return nil, err
	}
	num, err := numericFromRat(qty)
	if err != nil {
		return nil, err
	}
	aggID := ids.New()
	if err := tx.q.InsertUsageAggregate(ctx, dbgen.InsertUsageAggregateParams{ID: aggID, TenantID: tx.tenantID, SubscriptionID: sub.ID, MetricID: metric.ID,
		PeriodStart: period.Start, PeriodEnd: period.End, Aggregation: string(metric.Aggregation), Quantity: num, EventCount: int32(count),
		PriceVersionID: &price.ID, ClosedAt: tx.store.clock.Now()}); err != nil {
		return nil, err
	}
	if err := tx.q.AttachUsageEventsToAggregate(ctx, dbgen.AttachUsageEventsToAggregateParams{AggregateID: &aggID, TenantID: tx.tenantID,
		SubscriptionID: sub.ID, MetricID: metric.ID, PeriodStart: period.Start}); err != nil {
		return nil, err
	}
	return []billing.UsageLine{{MetricID: metric.ID, MetricCode: metric.Code, MetricName: metric.Name, Unit: metric.Unit,
		Aggregation: string(metric.Aggregation), Quantity: qty, AggregateID: &aggID}}, nil
}

// aggregate calcula a quantidade agregada respeitando a semântica da métrica.
func (b base) aggregate(ctx context.Context, subID uuid.UUID, metric catalog.Metric, periodStart time.Time) (*big.Rat, int, error) {
	args := struct {
		TenantID, SubscriptionID, MetricID uuid.UUID
		PeriodStart                        time.Time
	}{b.tenantID, subID, metric.ID, periodStart}
	var (
		num   pgtype.Numeric
		count int32
		err   error
	)
	switch metric.Aggregation {
	case catalog.AggSum:
		var r dbgen.UsageAggregateSumRow
		r, err = b.q.UsageAggregateSum(ctx, dbgen.UsageAggregateSumParams(args))
		num, count = r.Quantity, r.EventCount
	case catalog.AggMax:
		var r dbgen.UsageAggregateMaxRow
		r, err = b.q.UsageAggregateMax(ctx, dbgen.UsageAggregateMaxParams(args))
		num, count = r.Quantity, r.EventCount
	case catalog.AggUniqueCount:
		var r dbgen.UsageAggregateUniqueCountRow
		r, err = b.q.UsageAggregateUniqueCount(ctx, dbgen.UsageAggregateUniqueCountParams(args))
		num, count = r.Quantity, r.EventCount
	case catalog.AggLast:
		var r dbgen.UsageAggregateLastRow
		r, err = b.q.UsageAggregateLast(ctx, dbgen.UsageAggregateLastParams(args))
		num, count = r.Quantity, r.EventCount
	default:
		return nil, 0, apperr.Invalid("invalid_aggregation", "Agregação desconhecida.")
	}
	if err != nil {
		return nil, 0, err
	}
	r, err := ratFromNumeric(num)
	if err != nil {
		return nil, 0, err
	}
	if r == nil {
		r = new(big.Rat)
	}
	return r, int(count), nil
}

func (tx billingTx) MarkUsageBilled(ctx context.Context, aggregateID, itemID uuid.UUID) error {
	return tx.q.SetUsageAggregateItem(ctx, dbgen.SetUsageAggregateItemParams{InvoiceItemID: &itemID, TenantID: tx.tenantID, ID: aggregateID})
}

func (tx billingTx) ActiveDiscount(ctx context.Context, subscriptionID uuid.UUID) (*billing.Discount, error) {
	r, err := tx.q.GetActiveDiscount(ctx, dbgen.GetActiveDiscountParams{TenantID: tx.tenantID, SubscriptionID: subscriptionID})
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d := &billing.Discount{ID: r.ID, CouponCode: r.CouponCode, Description: r.CouponName, AppliesToPlanIDs: r.AppliesToPlanIds}
	if r.PercentOffBp != nil {
		d.PercentBP = int64(*r.PercentOffBp)
	}
	if r.AmountOff != nil {
		m := brl(*r.AmountOff)
		d.AmountOff = &m
	}
	return d, nil
}

func (tx billingTx) ConsumeDiscountCycle(ctx context.Context, discountID uuid.UUID) error {
	return tx.q.ConsumeDiscountCycle(ctx, dbgen.ConsumeDiscountCycleParams{Now: ptrTime(tx.store.clock.Now()), TenantID: tx.tenantID, ID: discountID})
}

func (tx billingTx) PendingItems(ctx context.Context, subscriptionID uuid.UUID) ([]billing.PendingItem, error) {
	rows, err := tx.q.ListOpenPendingItems(ctx, dbgen.ListOpenPendingItemsParams{TenantID: tx.tenantID, SubscriptionID: subscriptionID})
	if err != nil {
		return nil, err
	}
	out := make([]billing.PendingItem, 0, len(rows))
	for _, r := range rows {
		qty, err := ratFromNumeric(r.Quantity)
		if err != nil {
			return nil, err
		}
		unit, err := ratFromNumeric(r.UnitAmount)
		if err != nil {
			return nil, err
		}
		details, err := jsonMap(r.Details)
		if err != nil {
			return nil, err
		}
		var p *calendar.Period
		if r.PeriodStart != nil && r.PeriodEnd != nil {
			p = &calendar.Period{Start: *r.PeriodStart, End: *r.PeriodEnd}
		}
		details["source"] = r.Source
		out = append(out, billing.PendingItem{ID: r.ID, Kind: billing.ItemKind(r.Kind), Description: r.Description, Amount: brl(r.Amount),
			Quantity: qty, UnitAmount: unit, Period: p, Details: details})
	}
	return out, nil
}

func (tx billingTx) MarkPendingItemsBilled(ctx context.Context, itemIDs []uuid.UUID, invoiceID uuid.UUID) error {
	return tx.q.MarkPendingItemsBilled(ctx, dbgen.MarkPendingItemsBilledParams{InvoiceID: &invoiceID, TenantID: tx.tenantID, Ids: itemIDs})
}

func (tx billingTx) CustomerCreditBalance(ctx context.Context, customerID uuid.UUID) (money.Money, error) {
	return tx.creditBalance(ctx, customerID)
}

func (tx billingTx) ConsumeCustomerCredit(ctx context.Context, customerID, invoiceID uuid.UUID, amount money.Money, _ time.Time) error {
	return tx.addCredit(ctx, customerID, -amount.Amount(), "consumption", &invoiceID, "Aplicado na emissão da fatura", nil)
}

func (tx billingTx) GrantCustomerCredit(ctx context.Context, customerID uuid.UUID, amount money.Money, reason string, by uuid.UUID, requestKey string) (uuid.UUID, bool, error) {
	return tx.addCreditKeyed(ctx, customerID, amount.Amount(), "grant", nil, reason, &by, requestKey)
}

func (tx billingTx) ReturnCustomerCredit(ctx context.Context, customerID, invoiceID uuid.UUID, amount int64, _ time.Time) error {
	return tx.addCredit(ctx, customerID, amount, "return", &invoiceID, "Devolvido no cancelamento da fatura", nil)
}

func (tx billingTx) CycleInvoiceExists(ctx context.Context, subscriptionID uuid.UUID, kind billing.Kind, periodStart time.Time) (bool, error) {
	return tx.q.CycleInvoiceExists(ctx, dbgen.CycleInvoiceExistsParams{TenantID: tx.tenantID, SubscriptionID: &subscriptionID, Kind: string(kind),
		BillingPeriodStart: &periodStart})
}

func (tx billingTx) NextInvoiceNumber(ctx context.Context) (int64, error) {
	n, err := tx.q.NextInvoiceNumber(ctx, tx.tenantID)
	if postgres.IsNoRows(err) {
		if err := tx.q.InitInvoiceSequence(ctx, tx.tenantID); err != nil {
			return 0, err
		}
		return tx.q.NextInvoiceNumber(ctx, tx.tenantID)
	}
	return n, err
}

func (tx billingTx) InsertInvoice(ctx context.Context, inv *billing.Invoice) error {
	snap, err := json.Marshal(inv.CustomerSnapshot)
	if err != nil {
		return err
	}
	var ps, pe *time.Time
	if inv.BillingPeriod != nil {
		ps, pe = &inv.BillingPeriod.Start, &inv.BillingPeriod.End
	}
	err = tx.q.InsertInvoice(ctx, dbgen.InsertInvoiceParams{ID: inv.ID, TenantID: tx.tenantID, CustomerID: inv.CustomerID, SubscriptionID: inv.SubscriptionID,
		Number: strPtr(inv.Number), Status: string(inv.Status), Kind: string(inv.Kind), Currency: inv.Currency.Code(), BillingPeriodStart: ps,
		BillingPeriodEnd: pe, IssuedAt: inv.IssuedAt, DueAt: inv.DueAt, Subtotal: inv.Subtotal.Amount(), DiscountTotal: inv.DiscountTotal.Amount(),
		Total: inv.Total.Amount(), CreditApplied: inv.CreditApplied.Amount(), AmountPaid: inv.AmountPaid.Amount(),
		AmountCredited: inv.AmountCredited.Amount(), AmountWrittenOff: inv.AmountWrittenOff.Amount(), AmountDue: inv.AmountDue.Amount(),
		CustomerSnapshot: snap, Memo: inv.Memo, PaidAt: inv.PaidAt, CreatedAt: inv.CreatedAt})
	if postgres.IsUniqueViolation(err, "invoices_subscription_period_key") {
		return apperr.Conflict("invoice_already_issued", "A fatura deste período já foi emitida.")
	}
	if err != nil {
		return err
	}
	for _, it := range inv.Items {
		qty, err := numericFromRat(it.Quantity)
		if err != nil {
			return err
		}
		unit, err := numericFromRat(it.UnitAmount)
		if err != nil {
			return err
		}
		snapshot, err := json.Marshal(it.PriceSnapshot)
		if err != nil {
			return err
		}
		var ips, ipe *time.Time
		if it.Period != nil {
			ips, ipe = &it.Period.Start, &it.Period.End
		}
		if err := tx.q.InsertInvoiceItem(ctx, dbgen.InsertInvoiceItemParams{ID: it.ID, TenantID: tx.tenantID, InvoiceID: inv.ID, Position: int32(it.Position),
			Kind: string(it.Kind), Description: it.Description, Quantity: qty, UnitAmount: unit, Amount: it.Amount.Amount(),
			DiscountAmount: it.DiscountAmount.Amount(), PeriodStart: ips, PeriodEnd: ipe, PlanID: it.PlanID, PriceVersionID: it.PriceVersionID,
			PriceSnapshot: snapshot, UsageAggregateID: it.UsageAggregateID, CreatedAt: inv.CreatedAt}); err != nil {
			return err
		}
	}
	return nil
}

func (b base) insertInvoiceHistory(ctx context.Context, invoiceID uuid.UUID, actor audit.Actor, changes []billing.StatusChange) error {
	for _, c := range changes {
		details, err := json.Marshal(c.Details)
		if err != nil {
			return err
		}
		var from *string
		if c.From != nil {
			f := string(*c.From)
			from = &f
		}
		if err := b.q.InsertInvoiceHistory(ctx, dbgen.InsertInvoiceHistoryParams{ID: ids.New(), TenantID: b.tenantID, InvoiceID: invoiceID,
			FromStatus: from, ToStatus: string(c.To), OccurredAt: c.At, ActorType: string(actor.Type), ActorID: strPtr(actor.ID),
			Reason: c.Reason, Details: details}); err != nil {
			return err
		}
	}
	return nil
}

func (tx billingTx) InsertStatusHistory(ctx context.Context, invoiceID uuid.UUID, actor audit.Actor, changes []billing.StatusChange) error {
	return tx.insertInvoiceHistory(ctx, invoiceID, actor, changes)
}

func invoiceFromRow(r dbgen.Invoice) (billing.Invoice, error) {
	cur, err := money.ParseCurrency(r.Currency)
	if err != nil {
		return billing.Invoice{}, err
	}
	var snap customers.Snapshot
	if err := json.Unmarshal(r.CustomerSnapshot, &snap); err != nil {
		return billing.Invoice{}, err
	}
	m := func(v int64) money.Money { return money.New(v, cur) }
	inv := billing.Invoice{ID: r.ID, TenantID: r.TenantID, CustomerID: r.CustomerID, SubscriptionID: r.SubscriptionID, Number: deref(r.Number),
		Status: billing.Status(r.Status), Kind: billing.Kind(r.Kind), Currency: cur, IssuedAt: r.IssuedAt, DueAt: r.DueAt,
		Subtotal: m(r.Subtotal), DiscountTotal: m(r.DiscountTotal), Total: m(r.Total), CreditApplied: m(r.CreditApplied),
		AmountPaid: m(r.AmountPaid), AmountCredited: m(r.AmountCredited), AmountWrittenOff: m(r.AmountWrittenOff), AmountDue: m(r.AmountDue),
		CustomerSnapshot: snap, Memo: r.Memo, PaidAt: r.PaidAt, VoidedAt: r.VoidedAt, VoidReason: deref(r.VoidReason),
		UncollectibleAt: r.UncollectibleAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Version: int(r.Version)}
	if r.BillingPeriodStart != nil && r.BillingPeriodEnd != nil {
		inv.BillingPeriod = &calendar.Period{Start: *r.BillingPeriodStart, End: *r.BillingPeriodEnd}
	}
	return inv, nil
}

func (b base) invoice(ctx context.Context, id uuid.UUID, forUpdate bool) (billing.Invoice, error) {
	var r dbgen.Invoice
	var err error
	if forUpdate {
		r, err = b.q.GetInvoiceForUpdate(ctx, dbgen.GetInvoiceForUpdateParams{TenantID: b.tenantID, ID: id})
	} else {
		r, err = b.q.GetInvoice(ctx, dbgen.GetInvoiceParams{TenantID: b.tenantID, ID: id})
	}
	if err != nil {
		return billing.Invoice{}, notFound(err, "invoice_not_found", "Fatura não encontrada.")
	}
	inv, err := invoiceFromRow(r)
	if err != nil {
		return billing.Invoice{}, err
	}
	items, err := b.q.ListInvoiceItems(ctx, dbgen.ListInvoiceItemsParams{TenantID: b.tenantID, InvoiceID: id})
	if err != nil {
		return billing.Invoice{}, err
	}
	for _, it := range items {
		qty, err := ratFromNumeric(it.Quantity)
		if err != nil {
			return billing.Invoice{}, err
		}
		unit, err := ratFromNumeric(it.UnitAmount)
		if err != nil {
			return billing.Invoice{}, err
		}
		snap, err := jsonMap(it.PriceSnapshot)
		if err != nil {
			return billing.Invoice{}, err
		}
		item := billing.Item{ID: it.ID, Position: int(it.Position), Kind: billing.ItemKind(it.Kind), Description: it.Description, Quantity: qty,
			UnitAmount: unit, Amount: money.New(it.Amount, inv.Currency), DiscountAmount: money.New(it.DiscountAmount, inv.Currency),
			PlanID: it.PlanID, PriceVersionID: it.PriceVersionID, PriceSnapshot: snap, UsageAggregateID: it.UsageAggregateID}
		if it.PeriodStart != nil && it.PeriodEnd != nil {
			item.Period = &calendar.Period{Start: *it.PeriodStart, End: *it.PeriodEnd}
		}
		inv.Items = append(inv.Items, item)
	}
	return inv, nil
}

func (tx billingTx) Invoice(ctx context.Context, id uuid.UUID, forUpdate bool) (billing.Invoice, error) {
	return tx.invoice(ctx, id, forUpdate)
}

func (b base) updateInvoiceProgress(ctx context.Context, inv *billing.Invoice) error {
	n, err := b.q.UpdateInvoiceProgress(ctx, dbgen.UpdateInvoiceProgressParams{TenantID: b.tenantID, ID: inv.ID, Status: string(inv.Status),
		AmountPaid: inv.AmountPaid.Amount(), AmountCredited: inv.AmountCredited.Amount(), AmountWrittenOff: inv.AmountWrittenOff.Amount(),
		AmountDue: inv.AmountDue.Amount(), PaidAt: inv.PaidAt, VoidedAt: inv.VoidedAt, VoidReason: strPtr(inv.VoidReason),
		UncollectibleAt: inv.UncollectibleAt, Version: int32(inv.Version)})
	if err != nil {
		return err
	}
	return versionConflict(n, "A fatura foi alterada por outra pessoa.")
}

func (tx billingTx) UpdateInvoiceProgress(ctx context.Context, inv *billing.Invoice) error {
	return tx.updateInvoiceProgress(ctx, inv)
}

// InvoiceListItem acrescenta o nome do cliente à fatura listada.
type InvoiceListItem struct {
	billing.Invoice
	CustomerName string
}

func (tx billingTx) List(ctx context.Context, f billing.ListFilter) (billing.Page, error) {
	cts, cid, err := parseTimeCursor(f.Cursor)
	if err != nil {
		return billing.Page{}, err
	}
	limit := pageLimit(f.Limit)
	rows, err := tx.q.ListInvoices(ctx, dbgen.ListInvoicesParams{TenantID: tx.tenantID, Status: strPtr(string(f.Status)), CustomerID: f.CustomerID,
		SubscriptionID: f.SubscriptionID, IssuedFrom: f.IssuedFrom, IssuedTo: f.IssuedTo, OnlyOverdue: f.Overdue, Now: &f.Now,
		CursorTs: cts, CursorID: cid, RowLimit: int32(limit + 1)})
	if err != nil {
		return billing.Page{}, err
	}
	var p billing.Page
	for i, r := range rows {
		if i == limit {
			last := p.Items[len(p.Items)-1]
			p.NextCursor = timeCursor(last.CreatedAt, last.ID)
			break
		}
		inv, err := invoiceFromRow(dbgen.Invoice{ID: r.ID, TenantID: r.TenantID, CustomerID: r.CustomerID, SubscriptionID: r.SubscriptionID,
			Number: r.Number, Status: r.Status, Kind: r.Kind, Currency: r.Currency, BillingPeriodStart: r.BillingPeriodStart,
			BillingPeriodEnd: r.BillingPeriodEnd, IssuedAt: r.IssuedAt, DueAt: r.DueAt, Subtotal: r.Subtotal, DiscountTotal: r.DiscountTotal,
			Total: r.Total, CreditApplied: r.CreditApplied, AmountPaid: r.AmountPaid, AmountCredited: r.AmountCredited,
			AmountWrittenOff: r.AmountWrittenOff, AmountDue: r.AmountDue, CustomerSnapshot: r.CustomerSnapshot, Memo: r.Memo, PaidAt: r.PaidAt,
			VoidedAt: r.VoidedAt, VoidReason: r.VoidReason, UncollectibleAt: r.UncollectibleAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			Version: r.Version})
		if err != nil {
			return billing.Page{}, err
		}
		p.Items = append(p.Items, inv)
	}
	return p, nil
}

func (tx billingTx) StatusHistory(ctx context.Context, invoiceID uuid.UUID) ([]billing.HistoryEntry, error) {
	rows, err := tx.q.ListInvoiceHistory(ctx, dbgen.ListInvoiceHistoryParams{TenantID: tx.tenantID, InvoiceID: invoiceID})
	if err != nil {
		return nil, err
	}
	out := make([]billing.HistoryEntry, 0, len(rows))
	for _, r := range rows {
		d, err := jsonMap(r.Details)
		if err != nil {
			return nil, err
		}
		var from *billing.Status
		if r.FromStatus != nil {
			f := billing.Status(*r.FromStatus)
			from = &f
		}
		out = append(out, billing.HistoryEntry{ID: r.ID, FromStatus: from, ToStatus: billing.Status(r.ToStatus), OccurredAt: r.OccurredAt,
			ActorType: r.ActorType, ActorID: deref(r.ActorID), Reason: r.Reason, Details: d})
	}
	return out, nil
}
