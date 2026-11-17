package billing

import (
	"context"
	"maps"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

type creditGrant struct {
	id, customerID uuid.UUID
	amount         money.Money
	kind, reason   string
}

type lateMark struct {
	group       LateGroup
	aggregateID uuid.UUID
	itemID      *uuid.UUID
	at          time.Time
}

type recomputed struct {
	qty    *big.Rat
	events int
}

// advData é o estado mutável próprio do advancedFake, restaurado junto com
// cycleData quando a transação falha.
type advData struct {
	scheduled       []subscriptions.ScheduledChange
	canceledChanges []uuid.UUID
	grants          []creditGrant
	aggregates      map[string]ClosedAggregate
	late            map[uuid.UUID][]LateGroup
	marks           []lateMark
	newPending      []NewPendingItem
	usagePrices     []uuid.UUID
}

func (d advData) cloneAdvanced() advData {
	c := d
	c.scheduled = slices.Clone(d.scheduled)
	c.canceledChanges = slices.Clone(d.canceledChanges)
	c.grants = slices.Clone(d.grants)
	c.aggregates = maps.Clone(d.aggregates)
	c.late = maps.Clone(d.late)
	for k, v := range c.late {
		c.late[k] = slices.Clone(v)
	}
	c.marks = slices.Clone(d.marks)
	c.newPending = slices.Clone(d.newPending)
	c.usagePrices = slices.Clone(d.usagePrices)
	return c
}

// advancedFake estende o cycleFake com as operações de mudança de plano,
// correção de consumo atrasado e condições programadas. Implementa
// CycleStore, ChangeStore e CorrectionStore com transações atômicas: em caso
// de erro, todo o estado observável é restaurado.
//
// failAt permite falhar somente na n-ésima chamada (1 = primeira) de um
// método sobrescrito aqui; sem entrada em failAt, errs vale para todas as
// chamadas, como no cycleFake.
type advancedFake struct {
	*cycleFake
	advData
	platformPlan   platformbilling.Plan
	metrics        map[uuid.UUID]catalog.Metric
	recompute      map[string]recomputed
	itemAmounts    map[uuid.UUID]money.Money
	itemDiscounts  map[uuid.UUID]money.Money
	lateTenants    []uuid.UUID
	lateTenantsErr error
	calls          map[string]int
	failAt         map[string]int
}

func newAdvancedFake() *advancedFake {
	return &advancedFake{
		cycleFake: newCycleFake(),
		advData:   advData{aggregates: map[string]ClosedAggregate{}, late: map[uuid.UUID][]LateGroup{}},
		platformPlan: platformbilling.Plan{Code: "growth", Name: "Growth", Active: true,
			Features: []platformbilling.Feature{platformbilling.FeatureUsageBilling}},
		metrics: map[uuid.UUID]catalog.Metric{}, recompute: map[string]recomputed{}, itemAmounts: map[uuid.UUID]money.Money{},
		itemDiscounts: map[uuid.UUID]money.Money{}, calls: map[string]int{}, failAt: map[string]int{},
	}
}

func lateKey(g LateGroup) string {
	return g.SubscriptionID.String() + "|" + g.MetricID.String() + "|" + g.PeriodStart.UTC().Format(time.RFC3339Nano)
}

func (s *advancedFake) hit(m string) error {
	s.calls[m]++
	if n, ok := s.failAt[m]; ok && s.calls[m] != n {
		return nil
	}
	return s.fail(m)
}

func cloneChanges(in map[uuid.UUID]*subscriptions.ScheduledChange) map[uuid.UUID]*subscriptions.ScheduledChange {
	out := make(map[uuid.UUID]*subscriptions.ScheduledChange, len(in))
	for k, v := range in {
		cp := *v
		out[k] = &cp
	}
	return out
}

func cloneAdjustments(in map[uuid.UUID][]subscriptions.PriceAdjustment) map[uuid.UUID][]subscriptions.PriceAdjustment {
	out := make(map[uuid.UUID][]subscriptions.PriceAdjustment, len(in))
	for k, v := range in {
		out[k] = slices.Clone(v)
	}
	return out
}

// atomic executa fn como uma transação do tenant.
func (s *advancedFake) atomic(tenantID uuid.UUID, fn func() error) error {
	s.txCount++
	if err := s.failTenant[tenantID]; err != nil {
		return err
	}
	s.current = tenantID
	data, adv := s.clone(), s.cloneAdvanced() // cycleData e advData
	discount, changes, adjustments := s.discount, cloneChanges(s.changes), cloneAdjustments(s.adjustments)
	resolved, canceled, ended := maps.Clone(s.resolved), slices.Clone(s.termsCanceled), slices.Clone(s.discountEnded)
	pending := maps.Clone(s.pending)
	if err := fn(); err != nil {
		s.cycleData, s.advData = data, adv
		s.discount, s.changes, s.adjustments = discount, changes, adjustments
		s.resolved, s.termsCanceled, s.discountEnded = resolved, canceled, ended
		s.pending = pending
		return err
	}
	return nil
}

func (s *advancedFake) InCycleTx(_ context.Context, tenantID uuid.UUID, fn func(CycleTx) error) error {
	return s.atomic(tenantID, func() error { return fn(s) })
}

func (s *advancedFake) InChangeTx(_ context.Context, tenantID uuid.UUID, fn func(ChangeTx) error) error {
	return s.atomic(tenantID, func() error { return fn(s) })
}

func (s *advancedFake) InCorrectionTx(_ context.Context, tenantID uuid.UUID, fn func(CorrectionTx) error) error {
	return s.atomic(tenantID, func() error { return fn(s) })
}

func (s *advancedFake) TenantsWithLateUsage(context.Context) ([]uuid.UUID, error) {
	return s.lateTenants, s.lateTenantsErr
}

// Métodos do cycleFake sobrescritos para permitir falha na n-ésima chamada.

func (s *advancedFake) Plan(_ context.Context, id uuid.UUID) (catalog.Plan, error) {
	if err := s.hit("Plan"); err != nil {
		return catalog.Plan{}, err
	}
	return s.plans[id], nil
}

func (s *advancedFake) PriceVersion(_ context.Context, id uuid.UUID) (catalog.PriceVersion, error) {
	if err := s.hit("PriceVersion"); err != nil {
		return catalog.PriceVersion{}, err
	}
	return s.prices[id], nil
}

func (s *advancedFake) ActiveDiscount(context.Context, uuid.UUID) (*Discount, error) {
	if err := s.hit("ActiveDiscount"); err != nil {
		return nil, err
	}
	return s.discount, nil
}

func (s *advancedFake) Audit(_ context.Context, e audit.Entry) error {
	if err := s.hit("Audit"); err != nil {
		return err
	}
	s.audits = append(s.audits, e)
	return nil
}

func (s *advancedFake) UsageFor(ctx context.Context, sub subscriptions.Subscription, price catalog.PriceVersion, period calendar.Period) ([]UsageLine, error) {
	s.usagePrices = append(s.usagePrices, price.ID)
	return s.cycleFake.UsageFor(ctx, sub, price, period)
}

// InsertInvoice reproduz o índice único do banco, que vale somente para
// faturas de ciclo e de consumo final.
func (s *advancedFake) InsertInvoice(ctx context.Context, inv *Invoice) error {
	if inv.Kind == KindSubscriptionCycle || inv.Kind == KindFinalUsage {
		return s.cycleFake.InsertInvoice(ctx, inv)
	}
	if err := s.hit("InsertInvoice"); err != nil {
		return err
	}
	cp := *inv
	s.invoices = append(s.invoices, &cp)
	return nil
}

// ChangeTx.

func (s *advancedFake) Subscription(_ context.Context, id uuid.UUID, _ bool) (subscriptions.Subscription, error) {
	if err := s.hit("Subscription"); err != nil {
		return subscriptions.Subscription{}, err
	}
	for _, sub := range s.subs {
		if sub.ID == id && sub.TenantID == s.current {
			return sub, nil
		}
	}
	return subscriptions.Subscription{}, apperr.NotFound("subscription_not_found", "Assinatura não encontrada.")
}

func (s *advancedFake) PlatformPlan(context.Context) (platformbilling.Plan, error) {
	if err := s.hit("PlatformPlan"); err != nil {
		return platformbilling.Plan{}, err
	}
	return s.platformPlan, nil
}

func (s *advancedFake) InsertScheduledChange(_ context.Context, c subscriptions.ScheduledChange) error {
	if err := s.hit("InsertScheduledChange"); err != nil {
		return err
	}
	s.scheduled = append(s.scheduled, c)
	cp := c
	s.changes[c.SubscriptionID] = &cp
	return nil
}

func (s *advancedFake) CancelPendingChanges(_ context.Context, subscriptionID uuid.UUID, at time.Time) error {
	if err := s.hit("CancelPendingChanges"); err != nil {
		return err
	}
	s.canceledChanges = append(s.canceledChanges, subscriptionID)
	if c := s.changes[subscriptionID]; c != nil && c.Status == subscriptions.ChangePending {
		c.Status, c.ResolvedAt = subscriptions.ChangeCanceled, &at
	}
	return nil
}

func (s *advancedFake) AddCustomerCredit(_ context.Context, customerID uuid.UUID, amount money.Money, kind, reason string, _ *uuid.UUID) (uuid.UUID, error) {
	if err := s.hit("AddCustomerCredit"); err != nil {
		return uuid.Nil, err
	}
	balance, ok := s.credit[customerID]
	if !ok {
		balance = money.Zero(amount.Currency())
	}
	total, err := balance.Add(amount)
	if err != nil {
		return uuid.Nil, err
	}
	s.credit[customerID] = total
	id := uuid.New()
	s.grants = append(s.grants, creditGrant{id: id, customerID: customerID, amount: amount, kind: kind, reason: reason})
	return id, nil
}

// CorrectionTx.

func (s *advancedFake) LateUsageGroups(_ context.Context, limit int) ([]LateGroup, error) {
	if err := s.hit("LateUsageGroups"); err != nil {
		return nil, err
	}
	groups := s.late[s.current]
	return slices.Clone(groups[:min(limit, len(groups))]), nil
}

func (s *advancedFake) ClosedAggregate(_ context.Context, g LateGroup) (ClosedAggregate, error) {
	if err := s.hit("ClosedAggregate"); err != nil {
		return ClosedAggregate{}, err
	}
	agg, ok := s.aggregates[lateKey(g)]
	if !ok {
		return ClosedAggregate{}, apperr.NotFound("usage_aggregate_not_found", "Agregado de consumo não encontrado.")
	}
	return agg, nil
}

func (s *advancedFake) Metric(_ context.Context, id uuid.UUID) (catalog.Metric, error) {
	if err := s.hit("Metric"); err != nil {
		return catalog.Metric{}, err
	}
	return s.metrics[id], nil
}

func (s *advancedFake) RecomputeUsage(_ context.Context, subscriptionID uuid.UUID, metric catalog.Metric, periodStart time.Time) (*big.Rat, int, error) {
	if err := s.hit("RecomputeUsage"); err != nil {
		return nil, 0, err
	}
	r, ok := s.recompute[lateKey(LateGroup{SubscriptionID: subscriptionID, MetricID: metric.ID, PeriodStart: periodStart})]
	if !ok {
		return new(big.Rat), 0, nil
	}
	return new(big.Rat).Set(r.qty), r.events, nil
}

func (s *advancedFake) InvoiceItemAmounts(_ context.Context, id uuid.UUID) (money.Money, money.Money, error) {
	if err := s.hit("InvoiceItemAmounts"); err != nil {
		return money.Money{}, money.Money{}, err
	}
	m, ok := s.itemAmounts[id]
	if !ok {
		return money.Money{}, money.Money{}, apperr.NotFound("invoice_item_not_found", "Item de fatura não encontrado.")
	}
	d, ok := s.itemDiscounts[id]
	if !ok {
		d = money.Zero(m.Currency())
	}
	return m, d, nil
}

func (s *advancedFake) SetAggregateCorrection(_ context.Context, id uuid.UUID, qty *big.Rat, amount money.Money) error {
	if err := s.hit("SetAggregateCorrection"); err != nil {
		return err
	}
	for k, agg := range s.aggregates {
		if agg.ID == id {
			q, a := new(big.Rat).Set(qty), amount
			agg.CorrectedQuantity, agg.CorrectedAmount = q, &a
			s.aggregates[k] = agg
			return nil
		}
	}
	return apperr.NotFound("usage_aggregate_not_found", "Agregado de consumo não encontrado.")
}

func (s *advancedFake) MarkLateCorrected(_ context.Context, g LateGroup, aggregateID uuid.UUID, itemID *uuid.UUID, at time.Time) error {
	if err := s.hit("MarkLateCorrected"); err != nil {
		return err
	}
	s.marks = append(s.marks, lateMark{group: g, aggregateID: aggregateID, itemID: itemID, at: at})
	var left []LateGroup
	for _, p := range s.late[s.current] {
		if lateKey(p) != lateKey(g) {
			left = append(left, p)
		}
	}
	s.late[s.current] = left
	return nil
}

func (s *advancedFake) InsertPendingItem(_ context.Context, it NewPendingItem) error {
	if err := s.hit("InsertPendingItem"); err != nil {
		return err
	}
	s.newPending = append(s.newPending, it)
	// O item passa a ser lido pelo motor como pendente da assinatura.
	s.pending[it.SubscriptionID] = append(slices.Clone(s.pending[it.SubscriptionID]), PendingItem{ID: it.ID, Kind: it.Kind, Source: it.Source,
		Description: it.Description, Amount: it.Amount, Quantity: it.Quantity, Period: it.Period, Details: it.Details})
	return nil
}

var (
	_ CycleStore      = (*advancedFake)(nil)
	_ ChangeStore     = (*advancedFake)(nil)
	_ CorrectionStore = (*advancedFake)(nil)
	_ CorrectionTx    = (*advancedFake)(nil)
)
