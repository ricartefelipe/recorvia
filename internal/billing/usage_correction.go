package billing

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// LateGroup identifica eventos atrasados de uma métrica num período já
// fechado.
type LateGroup struct {
	SubscriptionID uuid.UUID
	MetricID       uuid.UUID
	PeriodStart    time.Time
}

// ClosedAggregate é o agregado fechado de um período. Quantity e o item
// faturado nunca mudam; CorrectedQuantity e CorrectedAmount acumulam o
// resultado das correções posteriores.
type ClosedAggregate struct {
	ID                uuid.UUID
	SubscriptionID    uuid.UUID
	MetricID          uuid.UUID
	Period            calendar.Period
	Quantity          *big.Rat
	InvoiceItemID     *uuid.UUID
	PriceVersionID    *uuid.UUID
	CorrectedQuantity *big.Rat
	CorrectedAmount   *money.Money
}

// NewPendingItem é um ajuste a incluir na próxima fatura da assinatura.
type NewPendingItem struct {
	ID               uuid.UUID
	CustomerID       uuid.UUID
	SubscriptionID   uuid.UUID
	Kind             ItemKind
	Source           string
	Description      string
	Amount           money.Money
	Quantity         *big.Rat
	Period           *calendar.Period
	Details          map[string]any
	UsageAggregateID *uuid.UUID
}

// CorrectionTx são as operações da correção de consumo atrasado.
type CorrectionTx interface {
	ChangeTx
	LateUsageGroups(ctx context.Context, limit int) ([]LateGroup, error)
	ClosedAggregate(ctx context.Context, g LateGroup) (ClosedAggregate, error)
	Metric(ctx context.Context, id uuid.UUID) (catalog.Metric, error)
	RecomputeUsage(ctx context.Context, subscriptionID uuid.UUID, metric catalog.Metric, periodStart time.Time) (*big.Rat, int, error)
	InvoiceItemAmount(ctx context.Context, id uuid.UUID) (money.Money, error)
	SetAggregateCorrection(ctx context.Context, id uuid.UUID, qty *big.Rat, amount money.Money) error
	MarkLateCorrected(ctx context.Context, g LateGroup, aggregateID uuid.UUID, itemID *uuid.UUID, at time.Time) error
	InsertPendingItem(ctx context.Context, it NewPendingItem) error
}

// CorrectionStore abre transações e descobre tenants com consumo atrasado.
type CorrectionStore interface {
	InCorrectionTx(ctx context.Context, tenantID uuid.UUID, fn func(CorrectionTx) error) error
	TenantsWithLateUsage(ctx context.Context) ([]uuid.UUID, error)
}

// UsageCorrector corrige o faturamento de consumo recebido depois do
// fechamento do período, sem alterar a fatura emitida.
//
// Política: a quantidade do período é recalculada com todos os eventos
// (mesma semântica de agregação) e precificada pela versão usada no
// fechamento. A diferença em relação ao que já foi faturado (incluindo
// correções anteriores) vira item de ajuste na próxima fatura da assinatura
// quando positiva, ou crédito do cliente quando negativa. Se a assinatura
// não tiver próximo ciclo (cancelada ou com encerramento agendado), a
// diferença positiva é faturada imediatamente.
type UsageCorrector struct {
	store CorrectionStore
	clock clock.Clock
	// BatchSize limita os grupos corrigidos por transação.
	BatchSize int
}

// NewUsageCorrector cria o corretor.
func NewUsageCorrector(store CorrectionStore, c clock.Clock) *UsageCorrector {
	return &UsageCorrector{store: store, clock: c, BatchSize: 200}
}

var correctorActor = audit.System("billing.usage_correction")

// RunAll corrige os grupos pendentes de todos os tenants. Falha de um
// tenant não impede os demais; os erros são devolvidos juntos.
func (c *UsageCorrector) RunAll(ctx context.Context) (int, error) {
	tenants, err := c.store.TenantsWithLateUsage(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	var errs []error
	for _, t := range tenants {
		if err := ctx.Err(); err != nil {
			return total, errors.Join(append(errs, err)...)
		}
		n := 0
		err := c.store.InCorrectionTx(ctx, t, func(tx CorrectionTx) error {
			n = 0
			groups, err := tx.LateUsageGroups(ctx, c.BatchSize)
			if err != nil {
				return err
			}
			for _, g := range groups {
				if err := c.correct(ctx, tx, g); err != nil {
					return fmt.Errorf("consumo da assinatura %s: %w", g.SubscriptionID, err)
				}
				n++
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", t, err))
			continue
		}
		total += n
	}
	return total, errors.Join(errs...)
}

func (c *UsageCorrector) correct(ctx context.Context, tx CorrectionTx, g LateGroup) error {
	now := c.clock.Now()
	agg, err := tx.ClosedAggregate(ctx, g)
	if err != nil {
		return err
	}
	sub, err := tx.Subscription(ctx, g.SubscriptionID, true)
	if err != nil {
		return err
	}
	metric, err := tx.Metric(ctx, g.MetricID)
	if err != nil {
		return err
	}
	priceID := sub.PriceVersionID
	if agg.PriceVersionID != nil {
		priceID = *agg.PriceVersionID
	}
	price, err := tx.PriceVersion(ctx, priceID)
	if err != nil {
		return err
	}
	newQty, events, err := tx.RecomputeUsage(ctx, sub.ID, metric, g.PeriodStart)
	if err != nil {
		return err
	}
	newAmount, _, err := price.UsageCharge(newQty)
	if err != nil {
		return err
	}
	prevQty, prevAmount := agg.Quantity, money.Zero(price.Currency)
	switch {
	case agg.CorrectedAmount != nil:
		prevQty, prevAmount = agg.CorrectedQuantity, *agg.CorrectedAmount
	case agg.InvoiceItemID != nil:
		if prevAmount, err = tx.InvoiceItemAmount(ctx, *agg.InvoiceItemID); err != nil {
			return err
		}
	}
	delta, err := newAmount.Sub(prevAmount)
	if err != nil {
		return err
	}
	settings, err := tx.TenantSettings(ctx)
	if err != nil {
		return err
	}
	details := map[string]any{"aggregate_id": agg.ID, "metric_code": metric.Code, "aggregation": metric.Aggregation,
		"billed_quantity": money.RatString(agg.Quantity), "previous_quantity": money.RatString(prevQty),
		"corrected_quantity": money.RatString(newQty), "previous_amount": prevAmount.Amount(), "corrected_amount": newAmount.Amount(),
		"events": events, "price_version_id": price.ID}
	desc := fmt.Sprintf("Correção de consumo de %s: %s para %s %s (%s)", metric.Name, money.RatString(prevQty), money.RatString(newQty),
		metric.Unit, localRange(agg.Period, settings.Location))
	var itemID *uuid.UUID
	period := agg.Period
	switch {
	case delta.IsPositive() && subscriptionBillsAgain(sub):
		it := NewPendingItem{ID: ids.New(), CustomerID: sub.CustomerID, SubscriptionID: sub.ID, Kind: ItemAdjustment, Source: "usage_correction",
			Description: desc, Amount: delta, Period: &period, Details: details, UsageAggregateID: &agg.ID}
		if err := tx.InsertPendingItem(ctx, it); err != nil {
			return err
		}
		itemID = &it.ID
		details["outcome"] = "next_invoice"
	case delta.IsPositive():
		if err := c.invoiceNow(ctx, tx, sub, desc, delta, period, details, settings, now); err != nil {
			return err
		}
		details["outcome"] = "invoiced_now"
	case delta.IsNegative():
		credit, err := delta.Neg()
		if err != nil {
			return err
		}
		creditID, err := tx.AddCustomerCredit(ctx, sub.CustomerID, credit, "usage_correction", desc, nil)
		if err != nil {
			return err
		}
		t, ok, err := ledger.CustomerCreditGranted(ids.New(), sub.TenantID, creditID, credit, now)
		if err != nil {
			return err
		}
		if ok {
			if err := tx.PostLedger(ctx, t); err != nil {
				return err
			}
		}
		details["outcome"] = "customer_credit"
	default:
		details["outcome"] = "no_effect"
	}
	details["delta"] = delta.Amount()
	if err := tx.SetAggregateCorrection(ctx, agg.ID, newQty, newAmount); err != nil {
		return err
	}
	if err := tx.MarkLateCorrected(ctx, g, agg.ID, itemID, now); err != nil {
		return err
	}
	return tx.Audit(ctx, audit.New(sub.TenantID, now, correctorActor, "usage.corrected", "subscription", sub.ID.String(), details))
}

// subscriptionBillsAgain informa se a assinatura terá outro ciclo faturado
// (onde o item pendente será incluído).
func subscriptionBillsAgain(s subscriptions.Subscription) bool {
	switch s.Status {
	case subscriptions.Trialing, subscriptions.Active, subscriptions.PastDue:
		return !s.CancelAtPeriodEnd
	}
	return false
}

func (c *UsageCorrector) invoiceNow(ctx context.Context, tx CorrectionTx, sub subscriptions.Subscription, desc string, amount money.Money,
	period calendar.Period, details map[string]any, settings TenantSettings, now time.Time) error {
	snapshot, customerTerms, err := tx.CustomerForInvoice(ctx, sub.CustomerID, now)
	if err != nil {
		return err
	}
	subID := sub.ID
	inv := NewDraft(ids.New(), sub.TenantID, sub.CustomerID, &subID, KindManual, amount.Currency(), &period, snapshot, now)
	if err := inv.AddItem(Item{ID: ids.New(), Kind: ItemAdjustment, Description: desc, Quantity: big.NewRat(1, 1), UnitAmount: amount.Rat(),
		Amount: amount, Period: &period, PriceSnapshot: maps.Clone(details)}); err != nil {
		return err
	}
	terms := settings.PaymentTermsDays
	if customerTerms != nil {
		terms = *customerTerms
	}
	if sub.PaymentTermsDays != nil {
		terms = *sub.PaymentTermsDays
	}
	if err := issueDraft(ctx, tx, inv, terms, settings, correctorActor, now); err != nil {
		return err
	}
	details["invoice_id"] = inv.ID
	return nil
}
