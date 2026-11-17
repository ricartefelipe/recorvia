package billing

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// ChangeTx são as operações de mudança de plano ou quantidade. Mudanças
// imediatas emitem a fatura de prorrata na mesma transação.
type ChangeTx interface {
	CycleTx
	Subscription(ctx context.Context, id uuid.UUID, forUpdate bool) (subscriptions.Subscription, error)
	PlatformPlan(ctx context.Context) (platformbilling.Plan, error)
	InsertScheduledChange(ctx context.Context, c subscriptions.ScheduledChange) error
	CancelPendingChanges(ctx context.Context, subscriptionID uuid.UUID, at time.Time) error
	AddCustomerCredit(ctx context.Context, customerID uuid.UUID, amount money.Money, kind, reason string, invoiceID *uuid.UUID) (uuid.UUID, error)
}

// ChangeStore abre transações de mudança.
type ChangeStore interface {
	InChangeTx(ctx context.Context, tenantID uuid.UUID, fn func(ChangeTx) error) error
}

// ChangeService implementa a troca de plano e de quantidade.
type ChangeService struct {
	store ChangeStore
	clock clock.Clock
}

// NewChangeService cria o serviço.
func NewChangeService(store ChangeStore, c clock.Clock) *ChangeService {
	return &ChangeService{store: store, clock: c}
}

// ChangeInput descreve a mudança. PlanID nulo mantém o plano (troca de
// quantidade); Quantity nula mantém a quantidade (ou 1 se o novo plano não
// for por unidade).
type ChangeInput struct {
	PlanID   *uuid.UUID
	Quantity *int
	Timing   subscriptions.ChangeTiming
}

// ChangeResult resume o efeito da mudança.
type ChangeResult struct {
	Subscription  subscriptions.Subscription
	Scheduled     *subscriptions.ScheduledChange
	Proration     *subscriptions.Proration
	InvoiceID     *uuid.UUID
	CreditGranted *money.Money
}

// Change aplica ou agenda a mudança.
//
// Política de prorrata (mudança imediata com o período corrente já
// faturado): diferença positiva gera fatura de prorrata imediata, com a
// cobrança proporcional do novo valor e o crédito da parte não usufruída;
// diferença negativa vira crédito do cliente, consumido nas próximas
// faturas; diferença zero não gera documento. Cupom percentual: o crédito
// desconta o percentual do cupom vigente no período pago e a cobrança, o
// percentual do cupom que continua valendo no novo plano (zero quando o
// cupom não vale para ele); cupom de valor fixo não é prorrateado.
//
// A troca da versão de preço cancela os reajustes programados da
// assinatura, que foram programados sobre o preço anterior.
func (s *ChangeService) Change(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, by *uuid.UUID, subscriptionID uuid.UUID,
	in ChangeInput, expectedVersion int) (ChangeResult, error) {
	var res ChangeResult
	err := s.store.InChangeTx(ctx, tenantID, func(tx ChangeTx) error {
		res = ChangeResult{}
		sub, err := tx.Subscription(ctx, subscriptionID, true)
		if err != nil {
			return err
		}
		if expectedVersion != 0 && sub.Version != expectedVersion {
			return apperr.PreconditionFailed("version_mismatch", "A assinatura foi alterada por outra pessoa. Recarregue e tente novamente.")
		}
		current, err := tx.PriceVersion(ctx, sub.PriceVersionID)
		if err != nil {
			return err
		}
		oldPlan, err := tx.Plan(ctx, sub.PlanID)
		if err != nil {
			return err
		}
		plan, price := oldPlan, current
		if in.PlanID != nil && *in.PlanID != sub.PlanID {
			if plan, err = tx.Plan(ctx, *in.PlanID); err != nil {
				return err
			}
			if plan.CurrentPriceVersionID == nil {
				return apperr.Conflict("plan_without_price", "O plano não possui preço vigente.")
			}
			if price, err = tx.PriceVersion(ctx, *plan.CurrentPriceVersionID); err != nil {
				return err
			}
		}
		qty := sub.Quantity
		switch {
		case in.Quantity != nil:
			qty = *in.Quantity
		case price.Model != catalog.PerUnit:
			qty = 1
		}
		if err := sub.ValidateChange(current, plan, price, qty, in.Timing); err != nil {
			return err
		}
		if price.HasUsage() {
			pplan, err := tx.PlatformPlan(ctx)
			if err != nil {
				return err
			}
			if err := platformbilling.RequireFeature(pplan, platformbilling.FeatureUsageBilling); err != nil {
				return err
			}
		}
		now := s.clock.Now()
		// Uma nova mudança substitui a agendada anteriormente.
		if err := tx.CancelPendingChanges(ctx, sub.ID, now); err != nil {
			return err
		}
		kind := subscriptions.QuantityChange
		if price.ID != sub.PriceVersionID {
			kind = subscriptions.PlanChange
		}
		action := "subscription." + map[subscriptions.ChangeKind]string{subscriptions.PlanChange: "plan_changed",
			subscriptions.QuantityChange: "quantity_changed"}[kind]
		details := map[string]any{"timing": in.Timing, "plan_id": plan.ID, "price_version": price.Version, "quantity": qty}
		if in.Timing == subscriptions.ChangeNextCycle {
			ch := subscriptions.ScheduledChange{ID: ids.New(), TenantID: tenantID, SubscriptionID: sub.ID, Kind: kind,
				EffectiveAt: sub.NextChangeAt(), Status: subscriptions.ChangePending, CreatedBy: by, CreatedAt: now}
			if kind == subscriptions.PlanChange {
				ch.NewPlanID, ch.NewPriceVersionID = &plan.ID, &price.ID
			}
			if qty != sub.Quantity || kind == subscriptions.QuantityChange {
				q := qty
				ch.NewQuantity = &q
			}
			if err := tx.InsertScheduledChange(ctx, ch); err != nil {
				return err
			}
			res.Scheduled = &ch
			action = "subscription.change_scheduled"
			details["scheduled_change_id"], details["effective_at"] = ch.ID, ch.EffectiveAt
			sub.RecordEvent("change_scheduled", now, map[string]any{"scheduled_change_id": ch.ID, "kind": kind, "effective_at": ch.EffectiveAt,
				"to_plan_id": plan.ID, "to_quantity": qty})
		} else {
			oldEff := sub.EffectivePrice(current)
			newEff := price
			if price.ID == sub.PriceVersionID {
				newEff = sub.EffectivePrice(price)
			}
			// O desconto é lido antes da troca: o cupom que não vale para o novo
			// plano é encerrado em seguida, mas ainda vale para o período pago.
			discount, err := tx.ActiveDiscount(ctx, sub.ID)
			if err != nil {
				return err
			}
			pr, prorate, err := subscriptions.ProrateChange(sub, oldEff, newEff, sub.Quantity, qty, now, prorationDiscount(discount, plan.ID))
			if err != nil {
				return err
			}
			oldQty := sub.Quantity
			chDetails := map[string]any{"timing": subscriptions.ChangeImmediately}
			if prorate {
				chDetails["proration_net"], chDetails["proration_fraction"] = pr.Net.Amount(), money.RatString(pr.Fraction)
			}
			event := "quantity_changed"
			if kind == subscriptions.PlanChange {
				event = "plan_changed"
			}
			sub.ApplyChange(plan, price, qty, event, now, chDetails)
			if kind == subscriptions.PlanChange {
				canceled, err := cancelAdjustmentsOnPriceChange(ctx, tx, &sub, now)
				if err != nil {
					return err
				}
				if len(canceled) > 0 {
					details["canceled_adjustment_ids"] = canceled
				}
			}
			if err := endInapplicableDiscount(ctx, tx, &sub, now); err != nil {
				return err
			}
			if prorate && !pr.Net.IsZero() {
				res.Proration = &pr
				if err := s.settleProration(ctx, tx, &sub, oldPlan, plan, oldEff, newEff, oldQty, qty, pr, discount, actor, now, &res); err != nil {
					return err
				}
				details["proration_net"] = pr.Net.Amount()
			}
		}
		if err := tx.UpdateSubscription(ctx, sub); err != nil {
			return err
		}
		sub.Version++
		if err := tx.InsertSubscriptionHistory(ctx, sub.ID, actor, sub.PopEvents()); err != nil {
			return err
		}
		if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventSubscriptionUpdated, "subscription", sub.ID,
			map[string]any{"subscription_id": sub.ID, "status": sub.Status, "change": action}, now)); err != nil {
			return err
		}
		res.Subscription = sub
		return tx.Audit(ctx, audit.New(tenantID, now, actor, action, "subscription", sub.ID.String(), details))
	})
	return res, err
}

// prorationDiscount extrai os percentuais de cupom da prorrata: o desconto
// vigente vale para o crédito do período pago e, se o cupom também valer
// para o novo plano, para a cobrança proporcional. Cupom de valor fixo não é
// prorrateado.
func prorationDiscount(d *Discount, newPlanID uuid.UUID) subscriptions.ProrationDiscount {
	if d == nil || d.AmountOff != nil || d.PercentBP <= 0 {
		return subscriptions.ProrationDiscount{}
	}
	pd := subscriptions.ProrationDiscount{OldBP: d.PercentBP}
	if d.AppliesTo(newPlanID) {
		pd.NewBP = d.PercentBP
	}
	return pd
}

// discountNote descreve o percentual de desconto aplicado a um item de
// prorrata ("desconto de 12,5%").
func discountNote(bp int64) string {
	if bp <= 0 {
		return ""
	}
	pct := strings.ReplaceAll(money.RatString(big.NewRat(bp, 100)), ".", ",")
	return fmt.Sprintf(", desconto de %s%%", pct)
}

// addDiscountSnapshot registra no snapshot do item de prorrata o cupom e o
// percentual aplicados (nada quando o percentual é zero).
func addDiscountSnapshot(snap map[string]any, d *Discount, bp int64) {
	if bp > 0 {
		snap["discount_percent_bp"], snap["coupon_code"], snap["discount_id"] = bp, d.CouponCode, d.ID
	}
}

func (s *ChangeService) settleProration(ctx context.Context, tx ChangeTx, sub *subscriptions.Subscription, oldPlan, newPlan catalog.Plan,
	oldPrice, newPrice catalog.PriceVersion, oldQty, newQty int, pr subscriptions.Proration, discount *Discount, actor audit.Actor, now time.Time,
	res *ChangeResult) error {
	settings, err := tx.TenantSettings(ctx)
	if err != nil {
		return err
	}
	span := fmt.Sprintf("%s, %d de %d dias", localRange(pr.Period, settings.Location), pr.RemainingDays, pr.PeriodDays)
	if pr.Net.IsNegative() {
		// Os percentuais de cupom já estão no crédito e na cobrança.
		credit, err := pr.Net.Neg()
		if err != nil {
			return err
		}
		reason := fmt.Sprintf("Crédito de prorrata: %s para %s (%s)", oldPlan.Name, newPlan.Name, span)
		creditID, err := tx.AddCustomerCredit(ctx, sub.CustomerID, credit, "proration", reason, nil)
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
		res.CreditGranted = &credit
		return tx.Audit(ctx, audit.New(sub.TenantID, now, actor, "customer_credit.proration", "customer", sub.CustomerID.String(),
			map[string]any{"subscription_id": sub.ID, "amount": credit.Amount()}))
	}
	snapshot, customerTerms, err := tx.CustomerForInvoice(ctx, sub.CustomerID, now)
	if err != nil {
		return err
	}
	subID := sub.ID
	period := pr.Period
	inv := NewDraft(ids.New(), sub.TenantID, sub.CustomerID, &subID, KindProration, pr.Net.Currency(), &period, snapshot, now)
	label := func(p catalog.Plan, pv catalog.PriceVersion, q int) string {
		if pv.Model == catalog.PerUnit {
			return fmt.Sprintf("%s: %d unidade(s)", p.Name, q)
		}
		return p.Name
	}
	base := map[string]any{"proration": true, "fraction": money.RatString(pr.Fraction), "remaining_days": pr.RemainingDays,
		"period_days": pr.PeriodDays}
	creditSnap := priceSnapshot(oldPlan, oldPrice)
	chargeSnap := priceSnapshot(newPlan, newPrice)
	for k, v := range base {
		creditSnap[k], chargeSnap[k] = v, v
	}
	if discount != nil {
		addDiscountSnapshot(creditSnap, discount, pr.Discount.OldBP)
		addDiscountSnapshot(chargeSnap, discount, pr.Discount.NewBP)
	}
	// A cobrança entra antes do crédito: o rascunho é recalculado a cada item
	// e o total parcial não pode ficar negativo.
	if err := inv.AddItem(Item{ID: ids.New(), Kind: ItemProration, Description: fmt.Sprintf("Cobrança proporcional de %s (%s%s)",
		label(newPlan, newPrice, newQty), span, discountNote(pr.Discount.NewBP)), Quantity: pr.Fraction, UnitAmount: pr.ChargeUnitAmount(),
		Amount: pr.Charge, Period: &period, PlanID: &newPlan.ID, PriceVersionID: &newPrice.ID, PriceSnapshot: chargeSnap}); err != nil {
		return err
	}
	if !pr.Credit.IsZero() {
		negCredit, err := pr.Credit.Neg()
		if err != nil {
			return err
		}
		if err := inv.AddItem(Item{ID: ids.New(), Kind: ItemProration, Description: fmt.Sprintf("Crédito proporcional de %s (%s%s)",
			label(oldPlan, oldPrice, oldQty), span, discountNote(pr.Discount.OldBP)), Quantity: pr.Fraction, UnitAmount: pr.CreditUnitAmount(),
			Amount: negCredit, Period: &period, PlanID: &oldPlan.ID, PriceVersionID: &oldPrice.ID, PriceSnapshot: creditSnap}); err != nil {
			return err
		}
	}
	terms := settings.PaymentTermsDays
	if customerTerms != nil {
		terms = *customerTerms
	}
	if sub.PaymentTermsDays != nil {
		terms = *sub.PaymentTermsDays
	}
	if err := issueDraft(ctx, tx, inv, terms, settings, actor, now); err != nil {
		return err
	}
	res.InvoiceID = &inv.ID
	return nil
}
