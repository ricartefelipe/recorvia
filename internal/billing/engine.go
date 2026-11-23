package billing

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"sync"
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
	// Issuer são os dados atuais da organização, gravados em cada fatura
	// emitida.
	Issuer Issuer
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
	// Plan e Price identificam a versão que precifica o consumo quando ela
	// difere da versão do componente fixo (mudança de plano no início do
	// período: o consumo do período anterior usa o preço anterior).
	Plan  *catalog.Plan
	Price *catalog.PriceVersion
	// Period é o período de consumo da linha; nulo usa o período de consumo
	// do passo (UsagePeriod).
	Period *calendar.Period
}

// Discount é um desconto recorrente vigente na assinatura.
type Discount struct {
	ID               uuid.UUID
	CouponCode       string
	PercentBP        int64
	AmountOff        *money.Money
	Description      string
	AppliesToPlanIDs []uuid.UUID
}

// AppliesTo informa se o desconto vale para o plano.
func (d Discount) AppliesTo(planID uuid.UUID) bool {
	return len(d.AppliesToPlanIDs) == 0 || slices.Contains(d.AppliesToPlanIDs, planID)
}

// Amount calcula o desconto sobre a base informada.
func (d Discount) Amount(base money.Money) (money.Money, error) {
	if d.AmountOff != nil {
		return money.Min(*d.AmountOff, base)
	}
	return base.MulRat(money.Percent(d.PercentBP), money.DefaultRounding)
}

// Type é o tipo do desconto: valor fixo por fatura ou percentual.
func (d Discount) Type() catalog.DiscountType {
	if d.AmountOff != nil {
		return catalog.DiscountAmount
	}
	return catalog.DiscountPercent
}

// recordItemDiscounts registra no snapshot de cada item com desconto o tipo,
// o identificador e o cupom do desconto aplicado na emissão. A correção de
// consumo usa o tipo para recalcular o valor líquido do item: o percentual
// mantém a proporção e o valor fixo mantém o valor absoluto descontado. O
// snapshot é copiado para não alterar mapas compartilhados com o chamador.
func recordItemDiscounts(inv *Invoice, d Discount) {
	for i := range inv.Items {
		it := &inv.Items[i]
		if !it.DiscountAmount.IsPositive() {
			continue
		}
		snap := maps.Clone(it.PriceSnapshot)
		if snap == nil {
			snap = map[string]any{}
		}
		snap["discount_id"], snap["discount_type"], snap["coupon_code"] = d.ID, string(d.Type()), d.CouponCode
		if d.AmountOff != nil {
			snap["discount_amount_off"] = d.AmountOff.Amount()
		} else {
			snap["discount_percent_bp"] = d.PercentBP
		}
		it.PriceSnapshot = snap
	}
}

// PendingSourceUsageCorrection é a origem dos itens pendentes gerados pela
// correção de consumo atrasado.
const PendingSourceUsageCorrection = "usage_correction"

// PendingItem é um ajuste (por exemplo, prorrata ou correção de consumo)
// aguardando a próxima fatura da assinatura. Source identifica a origem.
type PendingItem struct {
	ID          uuid.UUID
	Kind        ItemKind
	Source      string
	Description string
	Amount      money.Money
	Quantity    *big.Rat
	UnitAmount  *big.Rat
	Period      *calendar.Period
	Details     map[string]any
}

// discountExempt informa se o item pendente fica fora dos descontos da
// fatura: a correção de consumo já considera o desconto do item corrigido e
// não recebe um novo desconto.
func (p PendingItem) discountExempt() bool { return p.Source == PendingSourceUsageCorrection }

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

	// Condições programadas aplicadas no início de cada ciclo.
	PendingChange(ctx context.Context, subscriptionID uuid.UUID) (*subscriptions.ScheduledChange, error)
	ResolveChange(ctx context.Context, id uuid.UUID, status subscriptions.ChangeStatus, at time.Time) error
	DueAdjustments(ctx context.Context, subscriptionID uuid.UUID, onDate calendar.Date) ([]subscriptions.PriceAdjustment, error)
	MarkAdjustmentApplied(ctx context.Context, a subscriptions.PriceAdjustment) error
	// CancelAdjustment grava o cancelamento de um reajuste programado
	// (situação, instante e motivo já definidos no reajuste).
	CancelAdjustment(ctx context.Context, a subscriptions.PriceAdjustment) error
	// CancelScheduledAdjustments cancela todos os reajustes programados da
	// assinatura com o motivo informado e devolve os identificadores.
	CancelScheduledAdjustments(ctx context.Context, subscriptionID uuid.UUID, reason subscriptions.AdjustmentCancelReason,
		at time.Time) ([]uuid.UUID, error)
	CancelPendingTerms(ctx context.Context, subscriptionID uuid.UUID, at time.Time) error
	EndActiveDiscount(ctx context.Context, subscriptionID uuid.UUID, at time.Time) (bool, error)
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
	// Concurrency é o número de tenants processados ao mesmo tempo por
	// RunAll (cada um em sequência, uma transação por assinatura). Com 1, os
	// tenants são processados um depois do outro. O teste de carga (ADR-088)
	// mostrou o motor limitado pela latência de cada transação, não pelo
	// banco: tenants em paralelo multiplicam a vazão do fechamento.
	Concurrency int
}

// NewEngine cria o motor.
func NewEngine(store CycleStore, c Clock) *Engine {
	return &Engine{store: store, clock: c, MaxCatchUp: 24, MaxSubscriptionsPerRun: 500, Concurrency: 1}
}

// RunResult resume uma execução.
type RunResult struct {
	Subscriptions int
	Invoices      int
	InvoiceIDs    []uuid.UUID
	// More indica que algum tenant parou no limite de assinaturas por
	// execução (MaxSubscriptionsPerRun) depois de processar esse número de
	// assinaturas distintas, e pode ter mais assinaturas vencidas. Uma
	// assinatura reivindicada de novo na mesma execução (que não avançou)
	// não conta: sem isso, uma assinatura presa manteria More verdadeiro em
	// toda execução.
	More bool
}

var engineActor = audit.System("billing.engine")

// RunAll processa todos os tenants com faturamento vencido, até Concurrency
// ao mesmo tempo. A falha de um tenant interrompe a execução: nenhum tenant
// novo é iniciado, os que estão em andamento terminam e o resultado parcial
// volta com o erro (os tenants restantes ficam para a próxima execução).
func (e *Engine) RunAll(ctx context.Context) (RunResult, error) {
	tenants, err := e.store.TenantsWithDueBilling(ctx, e.clock.Now(), 1000)
	if err != nil {
		return RunResult{}, err
	}
	if e.Concurrency <= 1 || len(tenants) <= 1 {
		var total RunResult
		for _, t := range tenants {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			r, err := e.RunTenant(ctx, t, engineActor)
			total.add(r)
			if err != nil {
				return total, fmt.Errorf("billing: tenant %s: %w", t, err)
			}
		}
		return total, nil
	}
	var (
		mu    sync.Mutex
		total RunResult
		errs  []error
		next  int
		wg    sync.WaitGroup
	)
	// claim devolve o próximo tenant a processar, ou falso depois de uma
	// falha ou do cancelamento.
	claim := func() (uuid.UUID, bool) {
		mu.Lock()
		defer mu.Unlock()
		if len(errs) > 0 || next >= len(tenants) {
			return uuid.Nil, false
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			return uuid.Nil, false
		}
		next++
		return tenants[next-1], true
	}
	for range min(e.Concurrency, len(tenants)) {
		wg.Go(func() {
			for {
				t, ok := claim()
				if !ok {
					return
				}
				r, err := e.RunTenant(ctx, t, engineActor)
				mu.Lock()
				total.add(r)
				if err != nil {
					errs = append(errs, fmt.Errorf("billing: tenant %s: %w", t, err))
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return total, errors.Join(errs...)
}

func (r *RunResult) add(o RunResult) {
	r.Subscriptions += o.Subscriptions
	r.Invoices += o.Invoices
	r.InvoiceIDs = append(r.InvoiceIDs, o.InvoiceIDs...)
	r.More = r.More || o.More
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
	for i := range e.MaxSubscriptionsPerRun {
		if err := ctx.Err(); err != nil {
			return res, errors.Join(append(errs, err)...)
		}
		// A última volta do limite sempre reivindica: se achar uma
		// assinatura, pode haver outras.
		last := i == e.MaxSubscriptionsPerRun-1
		res.More = false
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
		res.More = last && res.Subscriptions >= e.MaxSubscriptionsPerRun
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
		// O consumo do período anterior é precificado pela versão vigente
		// naquele período, mesmo que uma mudança valha a partir deste.
		pricing := usagePricing{plan: plan, price: price}
		if step.Kind == subscriptions.StepBill {
			applied, err := e.applyTerms(ctx, tx, sub, step, now)
			if err != nil {
				return nil, err
			}
			if applied != nil {
				if plan, err = tx.Plan(ctx, sub.PlanID); err != nil {
					return nil, err
				}
				if price, err = tx.PriceVersion(ctx, sub.PriceVersionID); err != nil {
					return nil, err
				}
				pricing.change = &pricedChange{effectiveAt: applied.EffectiveAt, plan: plan, price: price}
			}
		} else if err := tx.CancelPendingTerms(ctx, sub.ID, now); err != nil {
			return nil, err
		}
		inv, err := e.issueForStep(ctx, tx, sub, plan, price, pricing, step, settings, actor, now)
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

// usagePricing escolhe a versão de preço que precifica cada período de
// consumo de um passo. A mudança agendada aplicada no passo vale para os
// períodos iniciados na data efetiva ou depois; isso só acontece depois de
// uma reativação que pulou passos, quando a mudança passou a valer num
// período cujo passo foi pulado (por exemplo, consumo pendente de um
// faturamento atrasado na suspensão). Os demais períodos usam a versão
// vigente antes do passo.
type usagePricing struct {
	plan   catalog.Plan
	price  catalog.PriceVersion
	change *pricedChange
}

// pricedChange é a mudança agendada aplicada no passo, com o plano e a
// versão de preço resultantes.
type pricedChange struct {
	effectiveAt time.Time
	plan        catalog.Plan
	price       catalog.PriceVersion
}

func (u usagePricing) forPeriod(p calendar.Period) (catalog.Plan, catalog.PriceVersion) {
	if u.change != nil && !u.change.effectiveAt.After(p.Start) {
		return u.change.plan, u.change.price
	}
	return u.plan, u.price
}

// applyTerms aplica, no início do período do passo, a mudança agendada e os
// reajustes vencidos. Devolve a mudança aplicada (nula se nenhuma), que pode
// trocar plano, versão de preço e quantidade.
//
// Quando a mudança troca a versão de preço, os reajustes programados são
// cancelados (foram programados sobre o preço anterior). Um reajuste vencido
// que não pode valer para o preço vigente (preço somente por consumo ou valor
// acima do limite) é cancelado com o motivo, sem interromper o ciclo.
func (e *Engine) applyTerms(ctx context.Context, tx CycleTx, sub *subscriptions.Subscription, step subscriptions.BillingStep,
	now time.Time) (*subscriptions.ScheduledChange, error) {
	var applied *subscriptions.ScheduledChange
	ch, err := tx.PendingChange(ctx, sub.ID)
	if err != nil {
		return nil, err
	}
	if ch != nil && !ch.EffectiveAt.After(step.Period.Start) {
		planID, priceID, qty := sub.PlanID, sub.PriceVersionID, sub.Quantity
		if ch.NewPlanID != nil && ch.NewPriceVersionID != nil {
			planID, priceID = *ch.NewPlanID, *ch.NewPriceVersionID
		}
		if ch.NewQuantity != nil {
			qty = *ch.NewQuantity
		}
		plan, err := tx.Plan(ctx, planID)
		if err != nil {
			return nil, err
		}
		price, err := tx.PriceVersion(ctx, priceID)
		if err != nil {
			return nil, err
		}
		event := "quantity_changed"
		if ch.Kind == subscriptions.PlanChange {
			event = "plan_changed"
		}
		priceChanged := price.ID != sub.PriceVersionID
		sub.ApplyChange(plan, price, qty, event, now, map[string]any{"scheduled_change_id": ch.ID, "timing": subscriptions.ChangeNextCycle,
			"period_start": step.Period.Start})
		if err := tx.ResolveChange(ctx, ch.ID, subscriptions.ChangeApplied, now); err != nil {
			return nil, err
		}
		details := map[string]any{"scheduled_change_id": ch.ID, "plan_id": plan.ID, "price_version": price.Version, "quantity": qty}
		if priceChanged {
			canceled, err := cancelAdjustmentsOnPriceChange(ctx, tx, sub, now)
			if err != nil {
				return nil, err
			}
			if len(canceled) > 0 {
				details["canceled_adjustment_ids"] = canceled
			}
		}
		if err := endInapplicableDiscount(ctx, tx, sub, now); err != nil {
			return nil, err
		}
		if err := tx.Audit(ctx, audit.New(sub.TenantID, now, engineActor, "subscription."+event, "subscription", sub.ID.String(), details)); err != nil {
			return nil, err
		}
		applied = ch
	}
	loc, err := sub.Location()
	if err != nil {
		return nil, err
	}
	adjs, err := tx.DueAdjustments(ctx, sub.ID, calendar.DateOf(step.Period.Start, loc))
	if err != nil || len(adjs) == 0 {
		return applied, err
	}
	price, err := tx.PriceVersion(ctx, sub.PriceVersionID)
	if err != nil {
		return nil, err
	}
	for i := range adjs {
		a := &adjs[i]
		adjusted, err := a.ApplyOrCancel(sub, price, step.Period.Start, now)
		if err != nil {
			return nil, err
		}
		if !adjusted {
			if err := tx.CancelAdjustment(ctx, *a); err != nil {
				return nil, err
			}
			if err := tx.Audit(ctx, audit.New(sub.TenantID, now, engineActor, "subscription.price_adjustment_canceled", "subscription",
				sub.ID.String(), map[string]any{"adjustment_id": a.ID, "reason": a.CancelReason})); err != nil {
				return nil, err
			}
			continue
		}
		if err := tx.MarkAdjustmentApplied(ctx, *a); err != nil {
			return nil, err
		}
		if err := tx.Audit(ctx, audit.New(sub.TenantID, now, engineActor, "subscription.price_adjusted", "subscription", sub.ID.String(),
			map[string]any{"adjustment_id": a.ID, "previous_amount": roundedCents(a.PreviousAmount), "resulting_amount": roundedCents(a.ResultingAmount),
				"previous_amount_decimal": money.RatString(a.PreviousAmount), "resulting_amount_decimal": money.RatString(a.ResultingAmount)})); err != nil {
			return nil, err
		}
	}
	return applied, nil
}

// roundedCents arredonda um valor exato a centavos (half-even) para exibição
// e auditoria.
func roundedCents(r *big.Rat) int64 { return money.RoundRat(r, money.DefaultRounding).Int64() }

// cancelAdjustmentsOnPriceChange cancela os reajustes programados quando a
// versão de preço da assinatura muda (mudança agendada aplicada pelo motor
// ou mudança imediata) e registra o cancelamento no histórico.
func cancelAdjustmentsOnPriceChange(ctx context.Context, tx interface {
	CancelScheduledAdjustments(ctx context.Context, subscriptionID uuid.UUID, reason subscriptions.AdjustmentCancelReason,
		at time.Time) ([]uuid.UUID, error)
}, sub *subscriptions.Subscription, now time.Time) ([]uuid.UUID, error) {
	ids, err := tx.CancelScheduledAdjustments(ctx, sub.ID, subscriptions.AdjustmentCanceledPriceChanged, now)
	if err != nil {
		return nil, err
	}
	sub.RecordAdjustmentsCanceled(ids, subscriptions.AdjustmentCanceledPriceChanged, now)
	return ids, nil
}

// endInapplicableDiscount encerra o desconto cujo cupom não vale para o novo
// plano da assinatura.
func endInapplicableDiscount(ctx context.Context, tx interface {
	ActiveDiscount(ctx context.Context, subscriptionID uuid.UUID) (*Discount, error)
	EndActiveDiscount(ctx context.Context, subscriptionID uuid.UUID, at time.Time) (bool, error)
}, sub *subscriptions.Subscription, now time.Time) error {
	d, err := tx.ActiveDiscount(ctx, sub.ID)
	if err != nil || d == nil || d.AppliesTo(sub.PlanID) {
		return err
	}
	if _, err := tx.EndActiveDiscount(ctx, sub.ID, now); err != nil {
		return err
	}
	sub.RecordEvent("discount_removed", now, map[string]any{"discount_id": d.ID, "reason": "coupon_not_applicable_to_plan"})
	return nil
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

// stepInvoice define o tipo e o período da fatura de um passo. No
// encerramento (StepEnd e StepFinalUsage), a fatura de consumo final cobre os
// períodos de consumo quando o preço tem consumo medido; sem consumo a
// faturar, os itens pendentes vão para uma fatura de ajuste (manual) com o
// período coberto pelos itens. ok é falso quando não há o que faturar.
func stepInvoice(step subscriptions.BillingStep, price catalog.PriceVersion, pending []PendingItem) (kind Kind, period *calendar.Period, ok bool) {
	switch {
	case step.Kind == subscriptions.StepBill:
		p := step.Period
		return KindSubscriptionCycle, &p, true
	case step.UsagePeriod != nil && price.HasUsage():
		return KindFinalUsage, step.UsageSpan(), true
	case len(pending) > 0:
		return KindManual, pendingSpan(pending), true
	}
	return "", nil, false
}

// pendingSpan é o menor período que contém os períodos dos itens pendentes
// (nulo se nenhum item informa período).
func pendingSpan(pending []PendingItem) *calendar.Period {
	var span *calendar.Period
	for _, p := range pending {
		switch {
		case p.Period == nil:
		case span == nil:
			s := *p.Period
			span = &s
		default:
			if p.Period.Start.Before(span.Start) {
				span.Start = p.Period.Start
			}
			if p.Period.End.After(span.End) {
				span.End = p.Period.End
			}
		}
	}
	return span
}

// BuildCycleInvoice monta o rascunho de um passo de faturamento. É pura
// (sem persistência) para permitir testes de cálculo isolados.
func BuildCycleInvoice(sub subscriptions.Subscription, plan catalog.Plan, price catalog.PriceVersion, step subscriptions.BillingStep,
	snapshot customers.Snapshot, usage []UsageLine, pending []PendingItem, discount *Discount, loc *time.Location, now time.Time) (*Invoice, []usageItemLink, error) {
	kind, period, ok := stepInvoice(step, price, pending)
	if !ok {
		return nil, nil, nil
	}
	subID := sub.ID
	inv := NewDraft(ids.New(), sub.TenantID, sub.CustomerID, &subID, kind, price.Currency, period, snapshot, now)
	fixed := sub.EffectivePrice(price)
	snap := priceSnapshot(plan, fixed)
	if sub.PriceOverrideAdjustmentID != nil && sub.PriceOverride != nil {
		snap["price_adjustment_id"] = *sub.PriceOverrideAdjustmentID
		snap["adjusted_fixed_amount"] = roundedCents(sub.PriceOverride)
		snap["adjusted_amount_decimal"] = money.RatString(sub.PriceOverride)
	}
	if step.Kind == subscriptions.StepBill && price.HasFixed() {
		amount, qty, unit, err := fixed.FixedCharge(sub.Quantity)
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
	if step.UsagePeriod != nil && kind != KindManual {
		for _, u := range usage {
			up := *step.UsagePeriod
			if u.Period != nil {
				up = *u.Period
			}
			lplan, lprice, lsnap := plan, price, snap
			if u.Price != nil && u.Plan != nil {
				lplan, lprice, lsnap = *u.Plan, *u.Price, priceSnapshot(*u.Plan, *u.Price)
			}
			if !lprice.HasUsage() {
				continue
			}
			amount, billable, err := lprice.UsageCharge(u.Quantity)
			if err != nil {
				return nil, nil, err
			}
			kindItem := ItemUsage
			desc := fmt.Sprintf("Consumo de %s: %s %s (%s)", u.MetricName, money.RatString(billable), u.Unit, localRange(up, loc))
			if lprice.Model == catalog.FlatPlusOverage {
				kindItem = ItemOverage
				desc = fmt.Sprintf("Excedente de %s: %s %s acima da franquia de %s (%s)", u.MetricName, money.RatString(billable), u.Unit,
					money.RatString(lprice.IncludedQuantity), localRange(up, loc))
			}
			us := map[string]any{"aggregated_quantity": money.RatString(u.Quantity), "aggregation": u.Aggregation, "metric_code": u.MetricCode}
			for k, v := range lsnap {
				us[k] = v
			}
			item := Item{ID: ids.New(), Kind: kindItem, Description: desc, Quantity: billable, UnitAmount: lprice.UnitAmount,
				Amount: amount, Period: &up, PlanID: &lplan.ID, PriceVersionID: &lprice.ID, PriceSnapshot: us, UsageAggregateID: u.AggregateID}
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
			Amount: p.Amount, Period: p.Period, PriceSnapshot: p.Details, DiscountExempt: p.discountExempt()}); err != nil {
			return nil, nil, err
		}
	}
	if !inv.HasChargeableItems() {
		return nil, nil, nil
	}
	if discount != nil {
		base, err := inv.DiscountableBase()
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
		recordItemDiscounts(inv, *discount)
	}
	return inv, links, nil
}

type usageItemLink struct{ aggregateID, itemID uuid.UUID }

// stepUsage fecha os períodos de consumo do passo, cada um precificado pela
// versão vigente nele. Linhas precificadas por versão diferente da do
// componente fixo levam plano e versão próprios.
func stepUsage(ctx context.Context, tx CycleTx, sub subscriptions.Subscription, fixed catalog.PriceVersion, pricing usagePricing,
	step subscriptions.BillingStep) ([]UsageLine, error) {
	var out []UsageLine
	for _, p := range step.UsagePeriods() {
		uplan, uprice := pricing.forPeriod(p)
		if !uprice.HasUsage() {
			continue
		}
		lines, err := tx.UsageFor(ctx, sub, uprice, p)
		if err != nil {
			return nil, err
		}
		for i := range lines {
			lines[i].Period = &p
			if uprice.ID != fixed.ID {
				lines[i].Plan, lines[i].Price = &uplan, &uprice
			}
		}
		out = append(out, lines...)
	}
	return out, nil
}

// issueForStep emite a fatura do passo. Itens pendentes entram na fatura do
// ciclo e também na de encerramento (StepEnd e StepFinalUsage); sem consumo
// a faturar no encerramento, viram uma fatura de ajuste.
func (e *Engine) issueForStep(ctx context.Context, tx CycleTx, sub *subscriptions.Subscription, plan catalog.Plan, price catalog.PriceVersion,
	pricing usagePricing, step subscriptions.BillingStep, settings TenantSettings, actor audit.Actor, now time.Time) (*Invoice, error) {
	pending, err := tx.PendingItems(ctx, sub.ID)
	if err != nil {
		return nil, err
	}
	kind, period, ok := stepInvoice(step, pricing.price, pending)
	if !ok {
		return nil, nil
	}
	// Faturas de ciclo e de consumo final são únicas por assinatura e
	// período; a fatura de ajuste é protegida pela marcação dos itens.
	if kind != KindManual {
		exists, err := tx.CycleInvoiceExists(ctx, sub.ID, kind, period.Start)
		if err != nil || exists {
			return nil, err
		}
	}
	snapshot, customerTerms, err := tx.CustomerForInvoice(ctx, sub.CustomerID, now)
	if err != nil {
		return nil, err
	}
	var usage []UsageLine
	if kind != KindManual {
		if usage, err = stepUsage(ctx, tx, *sub, price, pricing, step); err != nil {
			return nil, err
		}
	}
	var discount *Discount
	if step.Kind == subscriptions.StepBill {
		if discount, err = tx.ActiveDiscount(ctx, sub.ID); err != nil {
			return nil, err
		}
	}
	inv, links, err := BuildCycleInvoice(*sub, plan, price, step, snapshot, usage, pending, discount, settings.Location, now)
	if err != nil || inv == nil {
		return nil, err
	}
	terms := settings.PaymentTermsDays
	if customerTerms != nil {
		terms = *customerTerms
	}
	if sub.PaymentTermsDays != nil {
		terms = *sub.PaymentTermsDays
	}
	if err := issueDraft(ctx, tx, inv, terms, settings, actor, now); err != nil {
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
	return inv, nil
}

// IssueTx são as operações de emissão de uma fatura montada.
type IssueTx interface {
	CustomerCreditBalance(ctx context.Context, customerID uuid.UUID) (money.Money, error)
	ConsumeCustomerCredit(ctx context.Context, customerID, invoiceID uuid.UUID, amount money.Money, at time.Time) error
	NextInvoiceNumber(ctx context.Context) (int64, error)
	InsertInvoice(ctx context.Context, inv *Invoice) error
	InsertStatusHistory(ctx context.Context, invoiceID uuid.UUID, actor audit.Actor, changes []StatusChange) error
	PostLedger(ctx context.Context, t ledger.Transaction) error
	Audit(ctx context.Context, e audit.Entry) error
	Emit(ctx context.Context, e outbox.Event) error
}

// issueDraft emite um rascunho: aplica o crédito disponível do cliente,
// numera, define o vencimento, grava fatura, histórico, lançamento no
// ledger, evento de emissão e auditoria, tudo na transação corrente.
func issueDraft(ctx context.Context, tx IssueTx, inv *Invoice, termsDays int, settings TenantSettings, actor audit.Actor, now time.Time) error {
	credit, err := tx.CustomerCreditBalance(ctx, inv.CustomerID)
	if err != nil {
		return err
	}
	used, err := inv.ApplyCredit(credit)
	if err != nil {
		return err
	}
	seq, err := tx.NextInvoiceNumber(ctx)
	if err != nil {
		return err
	}
	number := FormatNumber(settings.InvoicePrefix, calendar.DateOf(now, settings.Location).Year, seq)
	if err := inv.Issue(number, now, DueAt(now, termsDays, settings.Location)); err != nil {
		return err
	}
	// Cópia do emitente, inclusive do endereço: a fatura guarda os dados da
	// emissão, que não mudam com alterações posteriores da organização.
	issuer := settings.Issuer
	if issuer.Address != nil {
		a := *issuer.Address
		issuer.Address = &a
	}
	inv.Issuer = &issuer
	if err := tx.InsertInvoice(ctx, inv); err != nil {
		return err
	}
	if err := tx.InsertStatusHistory(ctx, inv.ID, actor, inv.PopChanges()); err != nil {
		return err
	}
	if used.IsPositive() {
		if err := tx.ConsumeCustomerCredit(ctx, inv.CustomerID, inv.ID, used, now); err != nil {
			return err
		}
	}
	if t, ok, err := ledger.InvoiceIssued(ids.New(), inv.TenantID, inv.ID, inv.Number, inv.Total, inv.CreditApplied, now); err != nil {
		return err
	} else if ok {
		if err := tx.PostLedger(ctx, t); err != nil {
			return err
		}
	}
	if err := tx.Emit(ctx, IssuedEvent(inv, settings.Location, now)); err != nil {
		return err
	}
	details := map[string]any{"number": inv.Number, "total": inv.Total.Amount(), "kind": inv.Kind}
	if inv.SubscriptionID != nil {
		details["subscription_id"] = *inv.SubscriptionID
	}
	return tx.Audit(ctx, audit.New(inv.TenantID, now, actor, "invoice.issued", "invoice", inv.ID.String(), details))
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
