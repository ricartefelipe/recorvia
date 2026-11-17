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
// resultado das correções posteriores (CorrectedAmount é o valor líquido de
// desconto).
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
	// BilledItem devolve o valor bruto, o desconto distribuído e o tipo do
	// desconto registrado no snapshot de um item faturado.
	BilledItem(ctx context.Context, id uuid.UUID) (BilledItem, error)
	SetAggregateCorrection(ctx context.Context, id uuid.UUID, qty *big.Rat, amount money.Money) error
	MarkLateCorrected(ctx context.Context, g LateGroup, aggregateID uuid.UUID, itemID *uuid.UUID, at time.Time) error
	InsertPendingItem(ctx context.Context, it NewPendingItem) error
}

// BilledItem é o item faturado de um agregado de consumo. DiscountType é o
// tipo do desconto registrado no snapshot na emissão; fica vazio em itens
// faturados antes desse registro.
type BilledItem struct {
	Amount       money.Money
	Discount     money.Money
	DiscountType catalog.DiscountType
}

// Regras de desconto aplicadas ao novo valor na correção de consumo.
const (
	// correctionNoDiscount: item faturado sem desconto; vale o novo bruto.
	correctionNoDiscount = "none"
	// correctionPercent: desconto percentual; o novo valor mantém a
	// proporção líquida do item.
	correctionPercent = "percent"
	// correctionAmount: desconto de valor fixo; o novo valor mantém o valor
	// absoluto descontado no item (nunca negativo).
	correctionAmount = "amount"
	// correctionUnrecorded: item com desconto sem o tipo registrado
	// (faturado antes do registro); vale a proporção líquida do item.
	correctionUnrecorded = "unrecorded_ratio"
)

// correctedNet calcula o novo valor líquido do consumo corrigido a partir
// do valor bruto exato (e do mesmo valor arredondado) e do item faturado,
// com arredondamento único. ratio é a proporção de desconto das regras
// proporcionais e da regra sem desconto (nula na regra de valor fixo).
func correctedNet(grossExact *big.Rat, gross money.Money, billed BilledItem) (net money.Money, rule string, ratio *big.Rat, err error) {
	if !billed.Discount.IsPositive() || !billed.Amount.IsPositive() {
		return gross, correctionNoDiscount, new(big.Rat), nil
	}
	if billed.DiscountType == catalog.DiscountAmount {
		if net, err = gross.Sub(billed.Discount); err != nil {
			return money.Money{}, "", nil, err
		}
		if net.IsNegative() {
			net = money.Zero(gross.Currency())
		}
		return net, correctionAmount, nil, nil
	}
	rule = correctionPercent
	if billed.DiscountType != catalog.DiscountPercent {
		rule = correctionUnrecorded
	}
	// Proporção líquida do item faturado: (valor − desconto) ÷ valor.
	netRatio := new(big.Rat).SetFrac(big.NewInt(billed.Amount.Amount()-billed.Discount.Amount()), big.NewInt(billed.Amount.Amount()))
	if net, err = money.FromRat(new(big.Rat).Mul(grossExact, netRatio), gross.Currency(), money.DefaultRounding); err != nil {
		return money.Money{}, "", nil, err
	}
	return net, rule, new(big.Rat).Sub(big.NewRat(1, 1), netRatio), nil
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
// fechamento. A comparação usa valores líquidos de desconto: o valor
// faturado é o do item menos o desconto distribuído nele, e o novo valor
// segue o tipo do desconto registrado no item na emissão (correctedNet):
// desconto percentual mantém a proporção líquida do item; desconto de valor
// fixo mantém o valor absoluto já descontado (novo bruto menos o mesmo
// desconto, nunca negativo); item sem desconto segue o bruto. Itens com
// desconto faturados antes do registro do tipo usam a proporção. O novo
// valor é arredondado uma única vez. A diferença em relação ao que já foi
// faturado (incluindo correções anteriores) vira item de ajuste na próxima
// fatura da assinatura quando positiva (sem novo desconto), ou crédito do
// cliente quando negativa. Se a assinatura não tiver próximo ciclo
// (cancelada ou com encerramento agendado), a diferença positiva é faturada
// imediatamente.
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
	_, billable, err := price.UsageCharge(newQty)
	if err != nil {
		return err
	}
	billed := BilledItem{Amount: money.Zero(price.Currency), Discount: money.Zero(price.Currency)}
	if agg.InvoiceItemID != nil {
		if billed, err = tx.BilledItem(ctx, *agg.InvoiceItemID); err != nil {
			return err
		}
	}
	grossExact := new(big.Rat)
	if billable.Sign() > 0 {
		grossExact.Mul(price.UnitAmount, billable)
	}
	newGross, err := money.FromRat(grossExact, price.Currency, money.DefaultRounding)
	if err != nil {
		return err
	}
	newAmount, rule, discountRatio, err := correctedNet(grossExact, newGross, billed)
	if err != nil {
		return err
	}
	prevQty, prevAmount := agg.Quantity, money.Zero(price.Currency)
	switch {
	case agg.CorrectedAmount != nil:
		prevQty, prevAmount = agg.CorrectedQuantity, *agg.CorrectedAmount
	case agg.InvoiceItemID != nil:
		if prevAmount, err = billed.Amount.Sub(billed.Discount); err != nil {
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
		"corrected_gross_amount": newGross.Amount(), "billed_amount": billed.Amount.Amount(), "billed_discount_amount": billed.Discount.Amount(),
		"billed_discount_type": string(billed.DiscountType), "discount_rule": rule, "events": events, "price_version_id": price.ID}
	if discountRatio != nil {
		details["discount_ratio"] = money.RatString(discountRatio)
	}
	desc := fmt.Sprintf("Correção de consumo de %s: %s para %s %s (%s)", metric.Name, money.RatString(prevQty), money.RatString(newQty),
		metric.Unit, localRange(agg.Period, settings.Location))
	var itemID *uuid.UUID
	period := agg.Period
	switch {
	case delta.IsPositive() && subscriptionBillsAgain(sub):
		it := NewPendingItem{ID: ids.New(), CustomerID: sub.CustomerID, SubscriptionID: sub.ID, Kind: ItemAdjustment, Source: PendingSourceUsageCorrection,
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
