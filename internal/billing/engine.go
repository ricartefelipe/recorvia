package billing

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// TenantSettings são as configurações do tenant usadas na emissão.
type TenantSettings struct {
	Location         *time.Location
	InvoicePrefix    string
	PaymentTermsDays int
}

// UsageLine é o consumo agregado de uma métrica num período.
type UsageLine struct {
	MetricID    uuid.UUID
	MetricCode  string
	MetricName  string
	Unit        string
	Aggregation string
	Quantity    *big.Rat
	AggregateID *uuid.UUID
}

// Discount é um desconto recorrente vigente na assinatura.
type Discount struct {
	ID          uuid.UUID
	CouponCode  string
	PercentBP   int64
	AmountOff   *money.Money
	Description string
}

// Amount calcula o desconto sobre a base informada.
func (d Discount) Amount(base money.Money) (money.Money, error) {
	if d.AmountOff != nil {
		return money.Min(*d.AmountOff, base)
	}
	return base.MulRat(money.Percent(d.PercentBP), money.DefaultRounding)
}

// PendingItem é um ajuste (por exemplo, prorrata ou correção de consumo)
// aguardando a próxima fatura da assinatura.
type PendingItem struct {
	ID          uuid.UUID
	Kind        ItemKind
	Description string
	Amount      money.Money
	Quantity    *big.Rat
	UnitAmount  *big.Rat
	Period      *calendar.Period
	Details     map[string]any
}

// CycleTx são as operações do motor de recorrência na transação do tenant.
type CycleTx interface {
	TenantSettings(ctx context.Context) (TenantSettings, error)
	// ClaimDueSubscription reivindica uma assinatura vencida, ignorando as
	// que falharam nesta execução.
	ClaimDueSubscription(ctx context.Context, now time.Time, exclude []uuid.UUID) (subscriptions.Subscription, bool, error)
	UpdateSubscription(ctx context.Context, s subscriptions.Subscription) error
	InsertSubscriptionHistory(ctx context.Context, subscriptionID uuid.UUID, actor audit.Actor, ev []subscriptions.HistoryEvent) error
	Plan(ctx context.Context, id uuid.UUID) (catalog.Plan, error)
	PriceVersion(ctx context.Context, id uuid.UUID) (catalog.PriceVersion, error)
	CustomerForInvoice(ctx context.Context, id uuid.UUID, at time.Time) (customers.Snapshot, *int, error)
	UsageFor(ctx context.Context, sub subscriptions.Subscription, price catalog.PriceVersion, period calendar.Period) ([]UsageLine, error)
	MarkUsageBilled(ctx context.Context, aggregateID uuid.UUID, invoiceItemID uuid.UUID) error
	ActiveDiscount(ctx context.Context, subscriptionID uuid.UUID) (*Discount, error)
	ConsumeDiscountCycle(ctx context.Context, discountID uuid.UUID) error
	PendingItems(ctx context.Context, subscriptionID uuid.UUID) ([]PendingItem, error)
	MarkPendingItemsBilled(ctx context.Context, ids []uuid.UUID, invoiceID uuid.UUID) error
	CustomerCreditBalance(ctx context.Context, customerID uuid.UUID) (money.Money, error)
	ConsumeCustomerCredit(ctx context.Context, customerID, invoiceID uuid.UUID, amount money.Money, at time.Time) error
	CycleInvoiceExists(ctx context.Context, subscriptionID uuid.UUID, kind Kind, periodStart time.Time) (bool, error)
	NextInvoiceNumber(ctx context.Context) (int64, error)
	InsertInvoice(ctx context.Context, inv *Invoice) error
	InsertStatusHistory(ctx context.Context, invoiceID uuid.UUID, actor audit.Actor, changes []StatusChange) error
	PostLedger(ctx context.Context, t ledger.Transaction) error
	Audit(ctx context.Context, e audit.Entry) error
	Emit(ctx context.Context, e outbox.Event) error
}

// CycleStore abre transações do motor e descobre tenants com trabalho.
type CycleStore interface {
	InCycleTx(ctx context.Context, tenantID uuid.UUID, fn func(CycleTx) error) error
	TenantsWithDueBilling(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error)
}

// Clock é o relógio usado pelo motor.
type Clock interface{ Now() time.Time }

// Engine é o motor de recorrência. Cada assinatura vencida é processada em
// uma transação própria, reivindicada com FOR UPDATE SKIP LOCKED: várias
// instâncias do worker podem rodar em paralelo sem faturar duas vezes. A
// constraint de fatura única por assinatura e período é a última barreira.
type Engine struct {
	store CycleStore
	clock Clock
	// MaxCatchUp limita quantos períodos atrasados são faturados por
	// transação; o restante é processado na próxima reivindicação.
	MaxCatchUp int
	// MaxSubscriptionsPerRun limita o trabalho de uma execução por tenant.
	MaxSubscriptionsPerRun int
}

// NewEngine cria o motor.
func NewEngine(store CycleStore, c Clock) *Engine {
	return &Engine{store: store, clock: c, MaxCatchUp: 24, MaxSubscriptionsPerRun: 500}
}

// RunResult resume uma execução.
type RunResult struct {
	Subscriptions int
	Invoices      int
	InvoiceIDs    []uuid.UUID
}

var engineActor = audit.System("billing.engine")

// RunAll processa todos os tenants com faturamento vencido.
func (e *Engine) RunAll(ctx context.Context) (RunResult, error) {
	tenants, err := e.store.TenantsWithDueBilling(ctx, e.clock.Now(), 1000)
	if err != nil {
		return RunResult{}, err
	}
	var total RunResult
	for _, t := range tenants {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		r, err := e.RunTenant(ctx, t, engineActor)
		total.Subscriptions += r.Subscriptions
		total.Invoices += r.Invoices
		total.InvoiceIDs = append(total.InvoiceIDs, r.InvoiceIDs...)
		if err != nil {
			return total, fmt.Errorf("billing: tenant %s: %w", t, err)
		}
	}
	return total, nil
}

// RunTenant processa as assinaturas vencidas de um tenant. A falha de uma
// assinatura desfaz apenas a transação dela: as demais continuam sendo
// processadas e os erros são devolvidos juntos ao final (a assinatura com
// falha é tentada de novo na próxima execução).
func (e *Engine) RunTenant(ctx context.Context, tenantID uuid.UUID, actor audit.Actor) (RunResult, error) {
	var (
		res    RunResult
		failed []uuid.UUID
		errs   []error
		seen   = map[uuid.UUID]bool{}
	)
	for range e.MaxSubscriptionsPerRun {
		if err := ctx.Err(); err != nil {
			return res, errors.Join(append(errs, err)...)
		}
		var claimed uuid.UUID
		var created []uuid.UUID
		err := e.store.InCycleTx(ctx, tenantID, func(tx CycleTx) error {
			created = nil
			now := e.clock.Now()
			sub, ok, err := tx.ClaimDueSubscription(ctx, now, failed)
			if err != nil || !ok {
				return err
			}
			claimed = sub.ID
			created, err = e.processSubscription(ctx, tx, &sub, actor, now)
			return err
		})
		if err != nil {
			if claimed == uuid.Nil || ctx.Err() != nil {
				return res, errors.Join(append(errs, err)...)
			}
			failed = append(failed, claimed)
			errs = append(errs, fmt.Errorf("assinatura %s: %w", claimed, err))
			continue
		}
		if claimed == uuid.Nil {
			break
		}
		if !seen[claimed] {
			seen[claimed] = true
			res.Subscriptions++
		}
		res.Invoices += len(created)
		res.InvoiceIDs = append(res.InvoiceIDs, created...)
	}
	return res, errors.Join(errs...)
}

func (e *Engine) processSubscription(ctx context.Context, tx CycleTx, sub *subscriptions.Subscription, actor audit.Actor, now time.Time) ([]uuid.UUID, error) {
	settings, err := tx.TenantSettings(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := tx.Plan(ctx, sub.PlanID)
	if err != nil {
		return nil, err
	}
	price, err := tx.PriceVersion(ctx, sub.PriceVersionID)
	if err != nil {
		return nil, err
	}
	var created []uuid.UUID
	for range e.MaxCatchUp {
		step, ok, err := sub.NextStep(now)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		inv, err := e.issueForStep(ctx, tx, sub, plan, price, step, settings, actor, now)
		if err != nil {
			return nil, err
		}
		if inv != nil {
			created = append(created, inv.ID)
		}
		if err := sub.ApplyStep(step, now); err != nil {
			return nil, err
		}
	}
	if err := tx.UpdateSubscription(ctx, *sub); err != nil {
		return nil, err
	}
	return created, tx.InsertSubscriptionHistory(ctx, sub.ID, actor, sub.PopEvents())
}

func localRange(p calendar.Period, loc *time.Location) string {
	start := calendar.DateOf(p.Start, loc)
	end := calendar.DateOf(p.End, loc).AddDays(-1)
	return fmt.Sprintf("%02d/%02d/%04d a %02d/%02d/%04d", start.Day, start.Month, start.Year, end.Day, end.Month, end.Year)
}

func priceSnapshot(plan catalog.Plan, pv catalog.PriceVersion) map[string]any {
	s := map[string]any{
		"plan_code": plan.Code, "plan_name": plan.Name, "billing_interval": plan.Interval,
		"price_version": pv.Version, "pricing_model": pv.Model, "currency": pv.Currency.Code(),
	}
	if pv.FlatAmount != nil {
		s["flat_amount"] = pv.FlatAmount.Amount()
	}
	if pv.UnitAmount != nil {
		s["unit_amount_decimal"] = money.RatString(pv.UnitAmount)
	}
	if pv.IncludedQuantity != nil {
		s["included_quantity"] = money.RatString(pv.IncludedQuantity)
	}
	return s
}

// BuildCycleInvoice monta o rascunho de um passo de faturamento. É pura
// (sem persistência) para permitir testes de cálculo isolados.
func BuildCycleInvoice(sub subscriptions.Subscription, plan catalog.Plan, price catalog.PriceVersion, step subscriptions.BillingStep,
	snapshot customers.Snapshot, usage []UsageLine, pending []PendingItem, discount *Discount, loc *time.Location, now time.Time) (*Invoice, []usageItemLink, error) {
	kind := KindSubscriptionCycle
	period := step.Period
	if step.Kind != subscriptions.StepBill {
		kind = KindFinalUsage
		if step.UsagePeriod == nil {
			return nil, nil, nil
		}
		period = *step.UsagePeriod
	}
	subID := sub.ID
	inv := NewDraft(ids.New(), sub.TenantID, sub.CustomerID, &subID, kind, price.Currency, &period, snapshot, now)
	snap := priceSnapshot(plan, price)
	if step.Kind == subscriptions.StepBill && price.HasFixed() {
		amount, qty, unit, err := price.FixedCharge(sub.Quantity)
		if err != nil {
			return nil, nil, err
		}
		p := step.Period
		desc := fmt.Sprintf("%s (%s)", plan.Name, localRange(p, loc))
		if price.Model == catalog.PerUnit {
			desc = fmt.Sprintf("%s: %d unidade(s) (%s)", plan.Name, sub.Quantity, localRange(p, loc))
		}
		if err := inv.AddItem(Item{ID: ids.New(), Kind: ItemSubscriptionFee, Description: desc, Quantity: qty, UnitAmount: unit,
			Amount: amount, Period: &p, PlanID: &plan.ID, PriceVersionID: &price.ID, PriceSnapshot: snap}); err != nil {
			return nil, nil, err
		}
	}
	var links []usageItemLink
	if price.HasUsage() && step.UsagePeriod != nil {
		up := *step.UsagePeriod
		for _, u := range usage {
			amount, billable, err := price.UsageCharge(u.Quantity)
			if err != nil {
				return nil, nil, err
			}
			kindItem := ItemUsage
			desc := fmt.Sprintf("Consumo de %s: %s %s (%s)", u.MetricName, money.RatString(billable), u.Unit, localRange(up, loc))
			if price.Model == catalog.FlatPlusOverage {
				kindItem = ItemOverage
				desc = fmt.Sprintf("Excedente de %s: %s %s acima da franquia de %s (%s)", u.MetricName, money.RatString(billable), u.Unit,
					money.RatString(price.IncludedQuantity), localRange(up, loc))
			}
			us := map[string]any{"aggregated_quantity": money.RatString(u.Quantity), "aggregation": u.Aggregation, "metric_code": u.MetricCode}
			for k, v := range snap {
				us[k] = v
			}
			item := Item{ID: ids.New(), Kind: kindItem, Description: desc, Quantity: billable, UnitAmount: price.UnitAmount,
				Amount: amount, Period: &up, PlanID: &plan.ID, PriceVersionID: &price.ID, PriceSnapshot: us, UsageAggregateID: u.AggregateID}
			if err := inv.AddItem(item); err != nil {
				return nil, nil, err
			}
			if u.AggregateID != nil {
				links = append(links, usageItemLink{aggregateID: *u.AggregateID, itemID: item.ID})
			}
		}
	}
	for _, p := range pending {
		qty, unit := p.Quantity, p.UnitAmount
		if qty == nil {
			qty = big.NewRat(1, 1)
		}
		if unit == nil {
			unit = p.Amount.Rat()
		}
		if err := inv.AddItem(Item{ID: ids.New(), Kind: p.Kind, Description: p.Description, Quantity: qty, UnitAmount: unit,
			Amount: p.Amount, Period: p.Period, PriceSnapshot: p.Details}); err != nil {
			return nil, nil, err
		}
	}
	if !inv.HasChargeableItems() {
		return nil, nil, nil
	}
	if discount != nil {
		base, err := inv.Subtotal.Sub(inv.DiscountTotal)
		if err != nil {
			return nil, nil, err
		}
		d, err := discount.Amount(base)
		if err != nil {
			return nil, nil, err
		}
		if _, err := inv.ApplyDiscount(d); err != nil {
			return nil, nil, err
		}
	}
	return inv, links, nil
}

type usageItemLink struct{ aggregateID, itemID uuid.UUID }

func (e *Engine) issueForStep(ctx context.Context, tx CycleTx, sub *subscriptions.Subscription, plan catalog.Plan, price catalog.PriceVersion,
	step subscriptions.BillingStep, settings TenantSettings, actor audit.Actor, now time.Time) (*Invoice, error) {
	kind := KindSubscriptionCycle
	periodStart := step.Period.Start
	if step.Kind != subscriptions.StepBill {
		kind = KindFinalUsage
		if step.UsagePeriod == nil {
			return nil, nil
		}
		periodStart = step.UsagePeriod.Start
	}
	exists, err := tx.CycleInvoiceExists(ctx, sub.ID, kind, periodStart)
	if err != nil || exists {
		return nil, err
	}
	snapshot, customerTerms, err := tx.CustomerForInvoice(ctx, sub.CustomerID, now)
	if err != nil {
		return nil, err
	}
	var usage []UsageLine
	if price.HasUsage() && step.UsagePeriod != nil {
		if usage, err = tx.UsageFor(ctx, *sub, price, *step.UsagePeriod); err != nil {
			return nil, err
		}
	}
	var pending []PendingItem
	var discount *Discount
	if step.Kind == subscriptions.StepBill {
		if pending, err = tx.PendingItems(ctx, sub.ID); err != nil {
			return nil, err
		}
		if discount, err = tx.ActiveDiscount(ctx, sub.ID); err != nil {
			return nil, err
		}
	}
	inv, links, err := BuildCycleInvoice(*sub, plan, price, step, snapshot, usage, pending, discount, settings.Location, now)
	if err != nil || inv == nil {
		return nil, err
	}
	credit, err := tx.CustomerCreditBalance(ctx, sub.CustomerID)
	if err != nil {
		return nil, err
	}
	used, err := inv.ApplyCredit(credit)
	if err != nil {
		return nil, err
	}
	terms := settings.PaymentTermsDays
	if customerTerms != nil {
		terms = *customerTerms
	}
	if sub.PaymentTermsDays != nil {
		terms = *sub.PaymentTermsDays
	}
	seq, err := tx.NextInvoiceNumber(ctx)
	if err != nil {
		return nil, err
	}
	number := FormatNumber(settings.InvoicePrefix, calendar.DateOf(now, settings.Location).Year, seq)
	if err := inv.Issue(number, now, DueAt(now, terms, settings.Location)); err != nil {
		return nil, err
	}
	if err := tx.InsertInvoice(ctx, inv); err != nil {
		return nil, err
	}
	if err := tx.InsertStatusHistory(ctx, inv.ID, actor, inv.PopChanges()); err != nil {
		return nil, err
	}
	for _, l := range links {
		if err := tx.MarkUsageBilled(ctx, l.aggregateID, l.itemID); err != nil {
			return nil, err
		}
	}
	if len(pending) > 0 {
		pendingIDs := make([]uuid.UUID, len(pending))
		for i, p := range pending {
			pendingIDs[i] = p.ID
		}
		if err := tx.MarkPendingItemsBilled(ctx, pendingIDs, inv.ID); err != nil {
			return nil, err
		}
	}
	if discount != nil && !inv.DiscountTotal.IsZero() {
		if err := tx.ConsumeDiscountCycle(ctx, discount.ID); err != nil {
			return nil, err
		}
	}
	if used.IsPositive() {
		if err := tx.ConsumeCustomerCredit(ctx, sub.CustomerID, inv.ID, used, now); err != nil {
			return nil, err
		}
	}
	if t, ok, err := ledger.InvoiceIssued(ids.New(), inv.TenantID, inv.ID, inv.Number, inv.Total, inv.CreditApplied, now); err != nil {
		return nil, err
	} else if ok {
		if err := tx.PostLedger(ctx, t); err != nil {
			return nil, err
		}
	}
	if err := tx.Emit(ctx, IssuedEvent(inv, settings.Location, now)); err != nil {
		return nil, err
	}
	return inv, tx.Audit(ctx, audit.New(inv.TenantID, now, actor, "invoice.issued", "invoice", inv.ID.String(),
		map[string]any{"number": inv.Number, "total": inv.Total.Amount(), "kind": inv.Kind, "subscription_id": sub.ID}))
}

// IssuedEvent monta o evento de emissão consumido pela cobrança e pelas
// notificações.
func IssuedEvent(inv *Invoice, loc *time.Location, now time.Time) outbox.Event {
	payload := map[string]any{
		"invoice_id": inv.ID, "number": inv.Number, "customer_id": inv.CustomerID, "subscription_id": inv.SubscriptionID,
		"total": inv.Total.Amount(), "amount_due": inv.AmountDue.Amount(), "currency": inv.Currency.Code(), "kind": inv.Kind,
		"status": inv.Status,
	}
	if inv.DueAt != nil {
		payload["due_date"] = DueDate(*inv.DueAt, loc).String()
		payload["due_at"] = *inv.DueAt
	}
	return outbox.New(inv.TenantID, outbox.EventInvoiceIssued, "invoice", inv.ID, payload, now)
}
