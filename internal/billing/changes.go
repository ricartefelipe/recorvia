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
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
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
	// CycleFeeItem devolve o item do componente fixo da fatura de ciclo do
	// período iniciado em periodStart, com a situação da fatura; nil quando
	// não há fatura de ciclo com componente fixo para o período.
	CycleFeeItem(ctx context.Context, subscriptionID uuid.UUID, periodStart time.Time) (*CycleFeeItem, error)
	// PeriodProrations lista as prorratas do período iniciado em
	// periodStart, em ordem cronológica.
	PeriodProrations(ctx context.Context, subscriptionID uuid.UUID, periodStart time.Time) ([]ProrationRecord, error)
	InsertProration(ctx context.Context, r ProrationRecord) error
}

// CycleFeeItem é o item do componente fixo (subscription_fee) da fatura de
// ciclo de um período: valor bruto e desconto distribuído na emissão.
type CycleFeeItem struct {
	InvoiceID     uuid.UUID
	InvoiceStatus Status
	ItemID        uuid.UUID
	Amount        money.Money
	Discount      money.Money
}

// ProrationRecord registra a prorrata de uma mudança imediata: o componente
// fixo que passou a valer no período (versão de preço e quantidade), as
// bases e os valores do crédito e da cobrança e a fatura de prorrata emitida
// (nula quando a diferença virou crédito do cliente ou foi zero).
// ChargeBase é o valor cobrado pelo novo componente para o período integral,
// já com o desconto percentual da cobrança: base do crédito de uma nova
// mudança no mesmo período.
type ProrationRecord struct {
	ID             uuid.UUID
	SubscriptionID uuid.UUID
	// Period é o período de cobrança corrente (não o trecho prorrateado).
	Period         calendar.Period
	EffectiveAt    time.Time
	PriceVersionID uuid.UUID
	Quantity       int
	RemainingDays  int
	PeriodDays     int
	CreditBase     *big.Rat
	Credit         money.Money
	ChargeBase     *big.Rat
	Charge         money.Money
	InvoiceID      *uuid.UUID
	// InvoiceStatus é a situação atual da fatura de prorrata (vazia sem
	// fatura).
	InvoiceStatus Status
	CreatedAt     time.Time
}

// billedDocument informa se o documento que faturou o componente fixo ainda
// vale: sem fatura (diferença convertida em crédito do cliente ou nula) ou
// com fatura emitida e não cancelada.
func billedDocument(invoiceID *uuid.UUID, status Status) bool {
	return invoiceID == nil || (status != Void && status != Draft)
}

// Origens da base do crédito de prorrata.
const (
	creditBasisCycleInvoice = "cycle_invoice"
	creditBasisProration    = "proration"
	creditBasisNone         = "none"
)

// creditBasis é a base do crédito de prorrata: o valor efetivamente
// faturado pelo componente fixo vigente para o período integral (nulo sem
// faturamento válido) e os dados de origem para o snapshot e a auditoria.
type creditBasis struct {
	billed  *big.Rat
	source  string
	details map[string]any
}

// billedFixedCharge determina a base do crédito de prorrata do período
// corrente. A última prorrata do período cujo documento ainda vale define o
// componente vigente e o valor cobrado por ele (uma mudança anterior no
// mesmo período já substituiu o componente da fatura de ciclo). Sem
// prorrata válida, vale o item do componente fixo da fatura de ciclo do
// período (valor menos o desconto distribuído na emissão), se a fatura não
// foi cancelada. Sem nenhum dos dois, não há crédito.
//
// A validade é avaliada em cadeia, em ordem cronológica: uma prorrata com
// fatura vale se a fatura vale; uma prorrata sem fatura (diferença
// convertida em crédito do cliente ou nula) vale somente se a base de que
// ela partiu ainda vale. Assim, cancelar a fatura de ciclo invalida as
// prorratas sem fatura derivadas dela.
func billedFixedCharge(ctx context.Context, tx ChangeTx, sub subscriptions.Subscription) (creditBasis, error) {
	item, err := tx.CycleFeeItem(ctx, sub.ID, sub.CurrentPeriod.Start)
	if err != nil {
		return creditBasis{}, err
	}
	prorations, err := tx.PeriodProrations(ctx, sub.ID, sub.CurrentPeriod.Start)
	if err != nil {
		return creditBasis{}, err
	}
	basis := creditBasis{source: creditBasisNone, details: map[string]any{"credit_basis": creditBasisNone}}
	if item != nil && !billedDocument(&item.InvoiceID, item.InvoiceStatus) {
		basis.details["voided_invoice_id"] = item.InvoiceID
	}
	if item != nil && billedDocument(&item.InvoiceID, item.InvoiceStatus) {
		net, err := item.Amount.Sub(item.Discount)
		if err != nil {
			return creditBasis{}, err
		}
		basis = creditBasis{billed: net.Rat(), source: creditBasisCycleInvoice, details: map[string]any{"credit_basis": creditBasisCycleInvoice,
			"billed_invoice_id": item.InvoiceID, "billed_item_id": item.ItemID, "billed_amount": item.Amount.Amount(),
			"billed_discount_amount": item.Discount.Amount()}}
	}
	for _, p := range prorations {
		valid := basis.billed != nil
		if p.InvoiceID != nil {
			valid = billedDocument(p.InvoiceID, p.InvoiceStatus)
		}
		if !valid || p.ChargeBase == nil {
			continue
		}
		details := map[string]any{"credit_basis": creditBasisProration, "billed_proration_id": p.ID}
		if p.InvoiceID != nil {
			details["billed_invoice_id"] = *p.InvoiceID
		}
		basis = creditBasis{billed: new(big.Rat).Set(p.ChargeBase), source: creditBasisProration, details: details}
	}
	return basis, nil
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
// faturas; diferença zero não gera documento. O crédito incide sobre o
// valor efetivamente faturado pelo componente fixo vigente no período
// (billedFixedCharge): o item da fatura de ciclo líquido do desconto
// distribuído na emissão, ou a cobrança da prorrata anterior do mesmo
// período; sem faturamento válido (fatura de ciclo cancelada ou inexistente),
// não há crédito. A cobrança desconta o percentual do cupom ativo que vale
// para o novo plano (zero quando o cupom não vale para ele); cupom de valor
// fixo não é prorrateado na cobrança. Toda prorrata é registrada
// (ProrationRecord), também quando a diferença é zero.
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
		// Uma mudança agendada cuja data efetiva já passou está em vigor e
		// será aplicada pelo motor no fechamento do ciclo; substituí-la agora
		// desvincularia o consumo já aceito pela versão dela.
		pending, err := tx.PendingChange(ctx, sub.ID)
		if err != nil {
			return err
		}
		if pending != nil && !pending.EffectiveAt.After(now) {
			return apperr.Conflict("scheduled_change_in_effect",
				"A mudança agendada já está em vigor e será aplicada no processamento do ciclo; tente novamente em instantes.")
		}
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
			// plano é encerrado em seguida.
			discount, err := tx.ActiveDiscount(ctx, sub.ID)
			if err != nil {
				return err
			}
			prorates, err := sub.ProratesAt(now)
			if err != nil {
				return err
			}
			var basis creditBasis
			if prorates {
				if basis, err = billedFixedCharge(ctx, tx, sub); err != nil {
					return err
				}
			}
			pr, prorate, err := subscriptions.ProrateChange(sub, oldEff, newEff, sub.Quantity, qty, now,
				subscriptions.ProrationBasis{Billed: basis.billed, NewBP: chargeDiscountBP(discount, plan.ID)})
			if err != nil {
				return err
			}
			oldQty, period := sub.Quantity, sub.CurrentPeriod
			chDetails := map[string]any{"timing": subscriptions.ChangeImmediately}
			if prorate {
				chDetails["proration_net"], chDetails["proration_fraction"] = pr.Net.Amount(), money.RatString(pr.Fraction)
				chDetails["proration_credit_basis"] = basis.source
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
				if err := s.settleProration(ctx, tx, &sub, oldPlan, plan, oldEff, newEff, oldQty, qty, pr, basis, discount, actor, now, &res); err != nil {
					return err
				}
				details["proration_net"] = pr.Net.Amount()
			}
			if prorate {
				rec := ProrationRecord{ID: ids.New(), SubscriptionID: sub.ID, Period: period, EffectiveAt: now, PriceVersionID: price.ID,
					Quantity: qty, RemainingDays: pr.RemainingDays, PeriodDays: pr.PeriodDays, CreditBase: pr.CreditBase, Credit: pr.Credit,
					ChargeBase: pr.ChargeUnitAmount(), Charge: pr.Charge, InvoiceID: res.InvoiceID, CreatedAt: now}
				if err := tx.InsertProration(ctx, rec); err != nil {
					return err
				}
				details["proration_id"], details["proration_credit_basis"] = rec.ID, basis.source
				if v, ok := basis.details["voided_invoice_id"]; ok && basis.source == creditBasisNone {
					details["proration_voided_invoice_id"] = v
				}
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

// chargeDiscountBP é o percentual de desconto da cobrança proporcional: o
// do cupom ativo, se for percentual e valer para o novo plano. Cupom de
// valor fixo não é prorrateado.
func chargeDiscountBP(d *Discount, newPlanID uuid.UUID) int64 {
	if d == nil || d.AmountOff != nil || d.PercentBP <= 0 || !d.AppliesTo(newPlanID) {
		return 0
	}
	return d.PercentBP
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

// creditNote indica no item de crédito que o valor faturado tinha desconto
// (base menor que o componente fixo pelo preço vigente).
func creditNote(pr subscriptions.Proration) string {
	if pr.CreditBase != nil && pr.CreditBase.Cmp(pr.OldAmount.Rat()) < 0 {
		return ", líquido de desconto"
	}
	return ""
}

// addDiscountSnapshot registra no snapshot do item de cobrança proporcional
// o cupom e o percentual aplicados (nada quando o percentual é zero).
func addDiscountSnapshot(snap map[string]any, d *Discount, bp int64) {
	if d != nil && bp > 0 {
		snap["discount_percent_bp"], snap["coupon_code"], snap["discount_id"] = bp, d.CouponCode, d.ID
		snap["discount_type"] = string(catalog.DiscountPercent)
	}
}

func (s *ChangeService) settleProration(ctx context.Context, tx ChangeTx, sub *subscriptions.Subscription, oldPlan, newPlan catalog.Plan,
	oldPrice, newPrice catalog.PriceVersion, oldQty, newQty int, pr subscriptions.Proration, basis creditBasis, discount *Discount, actor audit.Actor,
	now time.Time, res *ChangeResult) error {
	settings, err := tx.TenantSettings(ctx)
	if err != nil {
		return err
	}
	span := fmt.Sprintf("%s, %d de %d dias", localRange(pr.Period, settings.Location), pr.RemainingDays, pr.PeriodDays)
	if pr.Net.IsNegative() {
		// O desconto já está na base do crédito e na cobrança.
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
			map[string]any{"subscription_id": sub.ID, "amount": credit.Amount(), "credit_basis": basis.source}))
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
	addDiscountSnapshot(chargeSnap, discount, pr.NewDiscountBP)
	for k, v := range basis.details {
		creditSnap[k] = v
	}
	creditSnap["credit_base_decimal"] = money.RatString(pr.CreditBase)
	// A cobrança entra antes do crédito: o rascunho é recalculado a cada item
	// e o total parcial não pode ficar negativo.
	if err := inv.AddItem(Item{ID: ids.New(), Kind: ItemProration, Description: fmt.Sprintf("Cobrança proporcional de %s (%s%s)",
		label(newPlan, newPrice, newQty), span, discountNote(pr.NewDiscountBP)), Quantity: pr.Fraction, UnitAmount: pr.ChargeUnitAmount(),
		Amount: pr.Charge, Period: &period, PlanID: &newPlan.ID, PriceVersionID: &newPrice.ID, PriceSnapshot: chargeSnap}); err != nil {
		return err
	}
	if !pr.Credit.IsZero() {
		negCredit, err := pr.Credit.Neg()
		if err != nil {
			return err
		}
		if err := inv.AddItem(Item{ID: ids.New(), Kind: ItemProration, Description: fmt.Sprintf("Crédito proporcional de %s (%s%s)",
			label(oldPlan, oldPrice, oldQty), span, creditNote(pr)), Quantity: pr.Fraction, UnitAmount: pr.CreditUnitAmount(),
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
