package billing

import (
	"context"
	"errors"
	"maps"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/address"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

type creditUse struct {
	customerID, invoiceID uuid.UUID
	amount                money.Money
}

type subHistoryWrite struct {
	subscriptionID uuid.UUID
	actor          audit.Actor
	events         []subscriptions.HistoryEvent
}

// cycleData é o estado mutável do armazenamento falso; é copiado no início
// de cada transação e restaurado se a transação falhar.
type cycleData struct {
	subs           []subscriptions.Subscription
	discountUses   map[uuid.UUID]int
	pendingBilled  map[uuid.UUID]uuid.UUID
	credit         map[uuid.UUID]money.Money
	creditUses     []creditUse
	seq            int64
	invoices       []*Invoice
	invoiceKeys    map[string]bool
	statusHistory  map[uuid.UUID][]StatusChange
	historyActors  []audit.Actor
	usageBilled    map[uuid.UUID]uuid.UUID
	posted         []ledger.Transaction
	audits         []audit.Entry
	events         []outbox.Event
	subHistory     []subHistoryWrite
	subUpdates     int
	pendingQueries int
	usageQueries   []calendar.Period
}

func (d cycleData) clone() cycleData {
	c := d
	c.subs = slices.Clone(d.subs)
	c.discountUses = maps.Clone(d.discountUses)
	c.pendingBilled = maps.Clone(d.pendingBilled)
	c.credit = maps.Clone(d.credit)
	c.creditUses = slices.Clone(d.creditUses)
	c.invoices = slices.Clone(d.invoices)
	c.invoiceKeys = maps.Clone(d.invoiceKeys)
	c.statusHistory = maps.Clone(d.statusHistory)
	c.historyActors = slices.Clone(d.historyActors)
	c.usageBilled = maps.Clone(d.usageBilled)
	c.posted = slices.Clone(d.posted)
	c.audits = slices.Clone(d.audits)
	c.events = slices.Clone(d.events)
	c.subHistory = slices.Clone(d.subHistory)
	c.usageQueries = slices.Clone(d.usageQueries)
	return c
}

// cycleFake implementa CycleStore e CycleTx em memória.
type cycleFake struct {
	cycleData
	settings      TenantSettings
	tenants       []uuid.UUID
	tenantsErr    error
	tenantsLimit  int
	plans         map[uuid.UUID]catalog.Plan
	prices        map[uuid.UUID]catalog.PriceVersion
	snapshot      customers.Snapshot
	customerTerms *int
	usage         func(sub subscriptions.Subscription, period calendar.Period) []UsageLine
	discount      *Discount
	pending       map[uuid.UUID][]PendingItem
	existing      map[string]bool
	errs          map[string]error
	failTenant    map[uuid.UUID]error
	current       uuid.UUID
	txCount       int
	// condições programadas
	changes       map[uuid.UUID]*subscriptions.ScheduledChange
	adjustments   map[uuid.UUID][]subscriptions.PriceAdjustment
	resolved      map[uuid.UUID]subscriptions.ChangeStatus
	termsCanceled []uuid.UUID
	discountEnded []uuid.UUID
}

// issuerAddress é o endereço da organização nas configurações do fake.
var issuerAddress = address.Address{Street: "Av. Paulista", Number: "1000", District: "Bela Vista", City: "São Paulo", State: "SP",
	PostalCode: "01310100", Country: "BR"}

func newCycleFake() *cycleFake {
	addr := issuerAddress
	return &cycleFake{
		cycleData: cycleData{discountUses: map[uuid.UUID]int{}, pendingBilled: map[uuid.UUID]uuid.UUID{}, credit: map[uuid.UUID]money.Money{},
			invoiceKeys: map[string]bool{}, statusHistory: map[uuid.UUID][]StatusChange{}, usageBilled: map[uuid.UUID]uuid.UUID{}},
		settings: TenantSettings{Location: saoPaulo, InvoicePrefix: "FAT", PaymentTermsDays: 5,
			Issuer: Issuer{Name: "Acme Serviços", LegalName: "Acme Serviços Ltda.", DocumentNumber: "11444777000161", Address: &addr}},
		plans: map[uuid.UUID]catalog.Plan{}, prices: map[uuid.UUID]catalog.PriceVersion{},
		snapshot: customers.Snapshot{Name: "Padaria Central", DocumentType: "cnpj", DocumentNumber: "11222333000181"},
		pending:  map[uuid.UUID][]PendingItem{}, existing: map[string]bool{}, errs: map[string]error{}, failTenant: map[uuid.UUID]error{},
		changes: map[uuid.UUID]*subscriptions.ScheduledChange{}, adjustments: map[uuid.UUID][]subscriptions.PriceAdjustment{},
		resolved: map[uuid.UUID]subscriptions.ChangeStatus{},
	}
}

func invoiceKey(subID uuid.UUID, kind Kind, start time.Time) string {
	return subID.String() + "|" + string(kind) + "|" + start.UTC().Format(time.RFC3339Nano)
}

func (s *cycleFake) InCycleTx(_ context.Context, tenantID uuid.UUID, fn func(CycleTx) error) error {
	s.txCount++
	if err := s.failTenant[tenantID]; err != nil {
		return err
	}
	s.current = tenantID
	backup := s.clone()
	if err := fn(s); err != nil {
		s.cycleData = backup
		return err
	}
	return nil
}

func (s *cycleFake) TenantsWithDueBilling(_ context.Context, _ time.Time, limit int) ([]uuid.UUID, error) {
	s.tenantsLimit = limit
	return s.tenants, s.tenantsErr
}

func (s *cycleFake) fail(m string) error { return s.errs[m] }

func (s *cycleFake) TenantSettings(context.Context) (TenantSettings, error) {
	return s.settings, s.fail("TenantSettings")
}

func (s *cycleFake) ClaimDueSubscription(_ context.Context, now time.Time, exclude []uuid.UUID) (subscriptions.Subscription, bool, error) {
	if err := s.fail("ClaimDueSubscription"); err != nil {
		return subscriptions.Subscription{}, false, err
	}
	for _, sub := range s.subs {
		if sub.TenantID == s.current && sub.BillingDue(now) && !slices.Contains(exclude, sub.ID) {
			return sub, true, nil
		}
	}
	return subscriptions.Subscription{}, false, nil
}

func (s *cycleFake) UpdateSubscription(_ context.Context, sub subscriptions.Subscription) error {
	if err := s.fail("UpdateSubscription"); err != nil {
		return err
	}
	for i := range s.subs {
		if s.subs[i].ID == sub.ID {
			s.subs[i] = sub
			s.subUpdates++
			return nil
		}
	}
	return errors.New("assinatura inexistente")
}

func (s *cycleFake) InsertSubscriptionHistory(_ context.Context, id uuid.UUID, actor audit.Actor, ev []subscriptions.HistoryEvent) error {
	if err := s.fail("InsertSubscriptionHistory"); err != nil {
		return err
	}
	s.subHistory = append(s.subHistory, subHistoryWrite{id, actor, ev})
	return nil
}

func (s *cycleFake) Plan(_ context.Context, id uuid.UUID) (catalog.Plan, error) {
	return s.plans[id], s.fail("Plan")
}

func (s *cycleFake) PriceVersion(_ context.Context, id uuid.UUID) (catalog.PriceVersion, error) {
	return s.prices[id], s.fail("PriceVersion")
}

func (s *cycleFake) CustomerForInvoice(context.Context, uuid.UUID, time.Time) (customers.Snapshot, *int, error) {
	return s.snapshot, s.customerTerms, s.fail("CustomerForInvoice")
}

func (s *cycleFake) UsageFor(_ context.Context, sub subscriptions.Subscription, _ catalog.PriceVersion, period calendar.Period) ([]UsageLine, error) {
	s.usageQueries = append(s.usageQueries, period)
	if err := s.fail("UsageFor"); err != nil {
		return nil, err
	}
	if s.usage == nil {
		return nil, nil
	}
	return s.usage(sub, period), nil
}

func (s *cycleFake) MarkUsageBilled(_ context.Context, aggregateID, itemID uuid.UUID) error {
	if err := s.fail("MarkUsageBilled"); err != nil {
		return err
	}
	s.usageBilled[aggregateID] = itemID
	return nil
}

func (s *cycleFake) ActiveDiscount(context.Context, uuid.UUID) (*Discount, error) {
	return s.discount, s.fail("ActiveDiscount")
}

func (s *cycleFake) ConsumeDiscountCycle(_ context.Context, id uuid.UUID) error {
	if err := s.fail("ConsumeDiscountCycle"); err != nil {
		return err
	}
	s.discountUses[id]++
	return nil
}

func (s *cycleFake) PendingItems(_ context.Context, subID uuid.UUID) ([]PendingItem, error) {
	s.pendingQueries++
	if err := s.fail("PendingItems"); err != nil {
		return nil, err
	}
	var out []PendingItem
	for _, p := range s.pending[subID] {
		if _, billed := s.pendingBilled[p.ID]; !billed {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *cycleFake) MarkPendingItemsBilled(_ context.Context, ids []uuid.UUID, invoiceID uuid.UUID) error {
	if err := s.fail("MarkPendingItemsBilled"); err != nil {
		return err
	}
	for _, id := range ids {
		s.pendingBilled[id] = invoiceID
	}
	return nil
}

func (s *cycleFake) CustomerCreditBalance(_ context.Context, customerID uuid.UUID) (money.Money, error) {
	if err := s.fail("CustomerCreditBalance"); err != nil {
		return money.Money{}, err
	}
	if c, ok := s.credit[customerID]; ok {
		return c, nil
	}
	return brl(0), nil
}

func (s *cycleFake) ConsumeCustomerCredit(_ context.Context, customerID, invoiceID uuid.UUID, amount money.Money, _ time.Time) error {
	if err := s.fail("ConsumeCustomerCredit"); err != nil {
		return err
	}
	left, err := s.credit[customerID].Sub(amount)
	if err != nil {
		return err
	}
	s.credit[customerID] = left
	s.creditUses = append(s.creditUses, creditUse{customerID, invoiceID, amount})
	return nil
}

func (s *cycleFake) CycleInvoiceExists(_ context.Context, subID uuid.UUID, kind Kind, start time.Time) (bool, error) {
	if err := s.fail("CycleInvoiceExists"); err != nil {
		return false, err
	}
	k := invoiceKey(subID, kind, start)
	return s.existing[k] || s.invoiceKeys[k], nil
}

func (s *cycleFake) NextInvoiceNumber(context.Context) (int64, error) {
	if err := s.fail("NextInvoiceNumber"); err != nil {
		return 0, err
	}
	s.seq++
	return s.seq, nil
}

func (s *cycleFake) InsertInvoice(_ context.Context, inv *Invoice) error {
	if err := s.fail("InsertInvoice"); err != nil {
		return err
	}
	k := invoiceKey(*inv.SubscriptionID, inv.Kind, inv.BillingPeriod.Start)
	if s.invoiceKeys[k] {
		return errors.New("violação da fatura única por assinatura e período")
	}
	s.invoiceKeys[k] = true
	cp := *inv
	s.invoices = append(s.invoices, &cp)
	return nil
}

func (s *cycleFake) InsertStatusHistory(_ context.Context, id uuid.UUID, actor audit.Actor, changes []StatusChange) error {
	if err := s.fail("InsertStatusHistory"); err != nil {
		return err
	}
	s.statusHistory[id] = append(slices.Clone(s.statusHistory[id]), changes...)
	s.historyActors = append(s.historyActors, actor)
	return nil
}

func (s *cycleFake) PostLedger(_ context.Context, t ledger.Transaction) error {
	if err := s.fail("PostLedger"); err != nil {
		return err
	}
	if err := t.Validate(); err != nil {
		return err
	}
	s.posted = append(s.posted, t)
	return nil
}

func (s *cycleFake) Audit(_ context.Context, e audit.Entry) error {
	if err := s.fail("Audit"); err != nil {
		return err
	}
	s.audits = append(s.audits, e)
	return nil
}

func (s *cycleFake) Emit(_ context.Context, e outbox.Event) error {
	if err := s.fail("Emit"); err != nil {
		return err
	}
	s.events = append(s.events, e)
	return nil
}

func (s *cycleFake) sub(id uuid.UUID) subscriptions.Subscription {
	for _, sub := range s.subs {
		if sub.ID == id {
			return sub
		}
	}
	return subscriptions.Subscription{}
}

// Catálogo de teste.

func fixturePlan() catalog.Plan {
	priceID := uuid.New()
	return catalog.Plan{ID: uuid.New(), Code: "pro", Name: "Plano Pro", Interval: calendar.Monthly, Currency: money.BRL,
		Status: catalog.Active, CurrentPriceVersionID: &priceID, Version: 3}
}

func fixturePrice(plan catalog.Plan, model catalog.PricingModel) catalog.PriceVersion {
	metric := uuid.New()
	pv := catalog.PriceVersion{ID: *plan.CurrentPriceVersionID, PlanID: plan.ID, Version: 2, Model: model, Currency: money.BRL}
	switch model {
	case catalog.Flat:
		f := brl(10000)
		pv.FlatAmount = &f
	case catalog.PerUnit:
		pv.UnitAmount = big.NewRat(24690, 20) // R$ 12,345 por unidade
	case catalog.Usage:
		pv.UnitAmount = big.NewRat(35, 100) // R$ 0,0035 por requisição
		pv.MetricID = &metric
	case catalog.FlatPlusOverage:
		f := brl(5000)
		pv.FlatAmount = &f
		pv.UnitAmount = big.NewRat(2, 1)
		pv.MetricID = &metric
		pv.IncludedQuantity = big.NewRat(1000, 1)
	}
	return pv
}

type engineFixture struct {
	store    *cycleFake
	clock    *clock.Manual
	engine   *Engine
	tenantID uuid.UUID
	plan     catalog.Plan
	price    catalog.PriceVersion
	actor    audit.Actor
}

var engineNow = local(2026, time.November, 16, 14, 30)

func newEngineFixture(t *testing.T, model catalog.PricingModel) *engineFixture {
	t.Helper()
	f := &engineFixture{store: newCycleFake(), clock: clock.NewManual(engineNow), tenantID: uuid.New(), actor: audit.System("teste")}
	f.plan = fixturePlan()
	f.price = fixturePrice(f.plan, model)
	f.store.plans[f.plan.ID] = f.plan
	f.store.prices[f.price.ID] = f.price
	f.engine = NewEngine(f.store, f.clock)
	return f
}

func (f *engineFixture) addSub(t *testing.T, start time.Time, mutate func(*subscriptions.NewInput)) subscriptions.Subscription {
	t.Helper()
	in := subscriptions.NewInput{ID: uuid.New(), TenantID: f.tenantID, CustomerID: uuid.New(), Plan: f.plan, Price: f.price,
		Quantity: 1, StartAt: start, Location: saoPaulo, Now: f.clock.Now()}
	if mutate != nil {
		mutate(&in)
	}
	sub, err := subscriptions.New(in)
	if err != nil {
		t.Fatalf("subscriptions.New: %v", err)
	}
	sub.PopEvents()
	f.store.subs = append(f.store.subs, sub)
	return sub
}

func (f *engineFixture) run(t *testing.T) RunResult {
	t.Helper()
	res, err := f.engine.RunTenant(context.Background(), f.tenantID, f.actor)
	if err != nil {
		t.Fatalf("RunTenant: %v", err)
	}
	return res
}

func ledgerNet(t *testing.T, txs []ledger.Transaction) map[ledger.AccountCode]int64 {
	t.Helper()
	out := map[ledger.AccountCode]int64{}
	for _, tx := range txs {
		for _, e := range tx.Entries {
			if e.Direction == ledger.Debit {
				out[e.Account] += e.Amount.Amount()
			} else {
				out[e.Account] -= e.Amount.Amount()
			}
		}
	}
	return out
}

func TestEngineIssuesCycleInvoice(t *testing.T) {
	f := newEngineFixture(t, catalog.Flat)
	sub := f.addSub(t, engineNow, nil)

	res := f.run(t)
	if res.Subscriptions != 1 || res.Invoices != 1 || len(res.InvoiceIDs) != 1 {
		t.Fatalf("resultado: %+v", res)
	}
	if len(f.store.invoices) != 1 {
		t.Fatalf("faturas: %d", len(f.store.invoices))
	}
	inv := f.store.invoices[0]
	sched, _ := sub.Schedule()
	if inv.ID != res.InvoiceIDs[0] || inv.Number != "FAT-2026-000001" || inv.Status != Open || inv.Kind != KindSubscriptionCycle {
		t.Fatalf("fatura: número=%s situação=%s tipo=%s", inv.Number, inv.Status, inv.Kind)
	}
	if *inv.SubscriptionID != sub.ID || inv.CustomerID != sub.CustomerID || inv.TenantID != f.tenantID || *inv.BillingPeriod != sched.Period(0) {
		t.Fatalf("vínculos da fatura: %+v", inv)
	}
	if inv.Total.Amount() != 10000 || inv.AmountDue.Amount() != 10000 || inv.CustomerSnapshot.Name != "Padaria Central" {
		t.Fatalf("valores: total=%v saldo=%v", inv.Total, inv.AmountDue)
	}
	// O emitente é gravado na emissão, como estava a organização, com uma
	// cópia do endereço: a alteração posterior do endereço da organização não
	// muda a fatura emitida.
	if inv.Issuer == nil || inv.Issuer.Name != "Acme Serviços" || inv.Issuer.LegalName != "Acme Serviços Ltda." ||
		inv.Issuer.DocumentNumber != "11444777000161" || inv.Issuer.Address == nil || *inv.Issuer.Address != issuerAddress {
		t.Fatalf("emitente gravado na emissão: %+v", inv.Issuer)
	}
	f.store.settings.Issuer.Address.Street = "Rua Nova"
	if inv.Issuer.Address.Street != issuerAddress.Street || inv.Issuer.Address == f.store.settings.Issuer.Address {
		t.Fatalf("o endereço do emitente deveria ser uma cópia: %+v", inv.Issuer.Address)
	}
	if !inv.IssuedAt.Equal(engineNow) || !inv.DueAt.Equal(local(2026, time.November, 22, 0, 0)) {
		t.Fatalf("emissão/vencimento: %v %v", inv.IssuedAt, inv.DueAt)
	}
	if ch := f.store.statusHistory[inv.ID]; len(ch) != 1 || *ch[0].From != Draft || ch[0].To != Open {
		t.Fatalf("histórico da fatura: %+v", ch)
	}
	if f.store.historyActors[0] != f.actor {
		t.Fatal("histórico deve registrar o ator da execução")
	}

	if len(f.store.posted) != 1 {
		t.Fatalf("lançamentos: %+v", f.store.posted)
	}
	tx := f.store.posted[0]
	if tx.IdempotencyKey != "invoice.issued:"+inv.ID.String() || tx.Validate() != nil {
		t.Fatalf("lançamento de emissão: %+v", tx)
	}
	if net := ledgerNet(t, f.store.posted); net[ledger.AccountsReceivable] != 10000 || net[ledger.BilledRevenue] != -10000 {
		t.Fatalf("ledger: %+v", net)
	}

	if len(f.store.events) != 1 || f.store.events[0].Type != outbox.EventInvoiceIssued || f.store.events[0].AggregateID != inv.ID {
		t.Fatalf("evento: %+v", f.store.events)
	}
	payload := f.store.events[0].Payload.(map[string]any)
	if payload["number"] != inv.Number || payload["amount_due"] != int64(10000) || payload["due_date"] != "2026-11-21" || payload["status"] != Open {
		t.Fatalf("payload do evento: %+v", payload)
	}
	if len(f.store.audits) != 1 || f.store.audits[0].Action != "invoice.issued" || f.store.audits[0].Actor != f.actor ||
		f.store.audits[0].Details["subscription_id"] != sub.ID {
		t.Fatalf("auditoria: %+v", f.store.audits)
	}

	stored := f.store.sub(sub.ID)
	if stored.NextBillingIndex != 1 || !stored.NextBillingAt.Equal(sched.Period(1).Start) {
		t.Fatalf("assinatura deve avançar: %+v", stored)
	}
	if len(f.store.subHistory) != 1 || f.store.subHistory[0].events[0].Event != "renewed" || f.store.subHistory[0].actor != f.actor {
		t.Fatalf("histórico da assinatura: %+v", f.store.subHistory)
	}

	// Nova execução no mesmo instante não fatura de novo.
	if res := f.run(t); res.Invoices != 0 || res.Subscriptions != 0 {
		t.Fatalf("segunda execução: %+v", res)
	}
}

func TestEngineNumbersInvoicesSequentially(t *testing.T) {
	f := newEngineFixture(t, catalog.Flat)
	f.addSub(t, engineNow, nil)
	f.addSub(t, engineNow, nil)
	res := f.run(t)
	if res.Invoices != 2 || res.Subscriptions != 2 {
		t.Fatalf("resultado: %+v", res)
	}
	if f.store.invoices[0].Number != "FAT-2026-000001" || f.store.invoices[1].Number != "FAT-2026-000002" {
		t.Fatalf("numeração: %s %s", f.store.invoices[0].Number, f.store.invoices[1].Number)
	}
}

func TestEngineNumberUsesLocalYear(t *testing.T) {
	f := newEngineFixture(t, catalog.Flat)
	now := local(2026, time.December, 31, 23, 30) // já é 2027 em UTC
	f.clock.Set(now)
	f.addSub(t, now, nil)
	f.run(t)
	if got := f.store.invoices[0].Number; got != "FAT-2026-000001" {
		t.Fatalf("o ano do número segue a data local: %s", got)
	}
}

func TestEngineDoesNotDuplicateExistingCycleInvoice(t *testing.T) {
	f := newEngineFixture(t, catalog.Flat)
	sub := f.addSub(t, engineNow, nil)
	sched, _ := sub.Schedule()
	f.store.existing[invoiceKey(sub.ID, KindSubscriptionCycle, sched.Period(0).Start)] = true

	res := f.run(t)
	if res.Invoices != 0 || res.Subscriptions != 1 || len(f.store.invoices) != 0 {
		t.Fatalf("fatura existente não pode ser duplicada: %+v", res)
	}
	if f.store.seq != 0 || len(f.store.posted) != 0 || len(f.store.events) != 0 {
		t.Fatal("nenhum número, lançamento ou evento deve ser gerado")
	}
	if f.store.sub(sub.ID).NextBillingIndex != 1 {
		t.Fatal("a assinatura avança mesmo quando a fatura já existia")
	}
}

func TestEngineAppliesCustomerCredit(t *testing.T) {
	t.Run("parcial", func(t *testing.T) {
		f := newEngineFixture(t, catalog.Flat)
		sub := f.addSub(t, engineNow, nil)
		f.store.credit[sub.CustomerID] = brl(3000)
		f.run(t)
		inv := f.store.invoices[0]
		if inv.CreditApplied.Amount() != 3000 || inv.AmountDue.Amount() != 7000 || inv.Status != Open {
			t.Fatalf("crédito aplicado=%v saldo=%v", inv.CreditApplied, inv.AmountDue)
		}
		if len(f.store.creditUses) != 1 || f.store.creditUses[0] != (creditUse{sub.CustomerID, inv.ID, brl(3000)}) {
			t.Fatalf("consumo do crédito: %+v", f.store.creditUses)
		}
		net := ledgerNet(t, f.store.posted)
		if net[ledger.AccountsReceivable] != 7000 || net[ledger.CustomerCredits] != 3000 || net[ledger.BilledRevenue] != -10000 {
			t.Fatalf("ledger com crédito: %+v", net)
		}
		if f.store.events[0].Payload.(map[string]any)["amount_due"] != int64(7000) {
			t.Fatal("evento deve informar o saldo após crédito")
		}
	})
	t.Run("integral", func(t *testing.T) {
		f := newEngineFixture(t, catalog.Flat)
		sub := f.addSub(t, engineNow, nil)
		f.store.credit[sub.CustomerID] = brl(25000)
		f.run(t)
		inv := f.store.invoices[0]
		if inv.Status != Paid || inv.PaidAt == nil || !inv.AmountDue.IsZero() {
			t.Fatalf("fatura coberta por crédito nasce paga: %s", inv.Status)
		}
		if f.store.credit[sub.CustomerID].Amount() != 15000 {
			t.Fatalf("saldo de crédito restante: %v", f.store.credit[sub.CustomerID])
		}
	})
}

func TestEngineConsumesDiscountCycle(t *testing.T) {
	f := newEngineFixture(t, catalog.Flat)
	f.addSub(t, engineNow.AddDate(0, -1, 0), nil) // dois períodos vencidos
	f.store.discount = &Discount{ID: uuid.New(), CouponCode: "BEMVINDO", PercentBP: 1000}
	f.run(t)
	if len(f.store.invoices) != 2 {
		t.Fatalf("faturas: %d", len(f.store.invoices))
	}
	for _, inv := range f.store.invoices {
		if inv.DiscountTotal.Amount() != 1000 || inv.Total.Amount() != 9000 {
			t.Fatalf("desconto de 10%%: %v total=%v", inv.DiscountTotal, inv.Total)
		}
	}
	if f.store.discountUses[f.store.discount.ID] != 2 {
		t.Fatalf("cada fatura consome um ciclo do desconto: %d", f.store.discountUses[f.store.discount.ID])
	}

	zero := newEngineFixture(t, catalog.Flat)
	zero.addSub(t, engineNow, nil)
	zero.store.discount = &Discount{ID: uuid.New(), PercentBP: 0}
	zero.run(t)
	if zero.store.discountUses[zero.store.discount.ID] != 0 {
		t.Fatal("desconto sem efeito não consome ciclo")
	}
}

func TestEnginePaymentTermsPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		customer *int
		sub      *int
		wantDue  time.Time
	}{
		{"prazo do tenant", nil, nil, local(2026, time.November, 22, 0, 0)},
		{"prazo do cliente", intPtr(10), nil, local(2026, time.November, 27, 0, 0)},
		{"prazo da assinatura", intPtr(10), intPtr(15), local(2026, time.December, 2, 0, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newEngineFixture(t, catalog.Flat)
			f.store.customerTerms = c.customer
			f.addSub(t, engineNow, func(in *subscriptions.NewInput) { in.PaymentTermsDays = c.sub })
			f.run(t)
			if got := *f.store.invoices[0].DueAt; !got.Equal(c.wantDue) {
				t.Fatalf("vencimento %v, esperado %v", got, c.wantDue)
			}
		})
	}
}

func TestEngineCatchesUpOverduePeriods(t *testing.T) {
	f := newEngineFixture(t, catalog.Flat)
	start := local(2026, time.June, 16, 9, 0) // períodos 0 a 5 vencidos
	sub := f.addSub(t, start, nil)
	res := f.run(t)
	if res.Invoices != 6 || res.Subscriptions != 1 {
		t.Fatalf("recuperação: %+v", res)
	}
	sched, _ := sub.Schedule()
	for i, inv := range f.store.invoices {
		if *inv.BillingPeriod != sched.Period(i) {
			t.Fatalf("fatura %d com período %+v", i, inv.BillingPeriod)
		}
		if want := FormatNumber("FAT", 2026, int64(i+1)); inv.Number != want {
			t.Fatalf("número %s, esperado %s", inv.Number, want)
		}
	}
	if got := f.store.sub(sub.ID); got.NextBillingIndex != 6 || got.BillingDue(engineNow) {
		t.Fatalf("assinatura em dia após a recuperação: %+v", got)
	}
}

func TestEngineLimitsCatchUpPerTransaction(t *testing.T) {
	f := newEngineFixture(t, catalog.Flat)
	f.engine.MaxCatchUp = 2
	f.engine.MaxSubscriptionsPerRun = 1
	sub := f.addSub(t, local(2026, time.June, 16, 9, 0), nil)

	res := f.run(t)
	if res.Invoices != 2 || f.store.sub(sub.ID).NextBillingIndex != 2 {
		t.Fatalf("no máximo MaxCatchUp períodos por transação: %+v idx=%d", res, f.store.sub(sub.ID).NextBillingIndex)
	}
	if !f.store.sub(sub.ID).BillingDue(engineNow) {
		t.Fatal("os períodos restantes continuam pendentes")
	}

	f.engine.MaxSubscriptionsPerRun = 500
	res = f.run(t)
	// Duas reivindicações da mesma assinatura contam uma assinatura.
	if res.Invoices != 4 || res.Subscriptions != 1 || f.store.sub(sub.ID).NextBillingIndex != 6 {
		t.Fatalf("o restante é recuperado em novas reivindicações: %+v", res)
	}
}

func TestEngineBillsUsageOfPreviousPeriod(t *testing.T) {
	f := newEngineFixture(t, catalog.Usage)
	aggID := uuid.New()
	sub := f.addSub(t, engineNow.AddDate(0, -1, 0), nil)
	sched, _ := sub.Schedule()
	f.store.usage = func(_ subscriptions.Subscription, p calendar.Period) []UsageLine {
		if p != sched.Period(0) {
			t.Fatalf("consumo consultado para período inesperado: %+v", p)
		}
		return []UsageLine{{MetricID: *f.price.MetricID, MetricCode: "api_calls", MetricName: "Requisições", Unit: "requisicao",
			Aggregation: "sum", Quantity: big.NewRat(1500, 1), AggregateID: &aggID}}
	}
	res := f.run(t)
	if res.Invoices != 1 || res.Subscriptions != 1 {
		t.Fatalf("somente o período 1 tem consumo a faturar: %+v", res)
	}
	if len(f.store.usageQueries) != 1 {
		t.Fatalf("o período 0 não consulta consumo: %+v", f.store.usageQueries)
	}
	inv := f.store.invoices[0]
	if len(inv.Items) != 1 || inv.Items[0].Kind != ItemUsage || inv.Items[0].Amount.Amount() != 525 {
		t.Fatalf("item de consumo: %+v", inv.Items)
	}
	if *inv.BillingPeriod != sched.Period(1) {
		t.Fatalf("a fatura é do ciclo 1: %+v", inv.BillingPeriod)
	}
	if f.store.usageBilled[aggID] != inv.Items[0].ID {
		t.Fatal("agregado de consumo deve ser vinculado ao item faturado")
	}
	if f.store.sub(sub.ID).NextBillingIndex != 2 {
		t.Fatal("os dois períodos devem ser processados")
	}
}

func TestEngineWithoutUsageIssuesNothing(t *testing.T) {
	f := newEngineFixture(t, catalog.Usage)
	sub := f.addSub(t, engineNow.AddDate(0, -1, 0), nil)
	f.store.usage = func(subscriptions.Subscription, calendar.Period) []UsageLine {
		return []UsageLine{{MetricName: "Requisições", Unit: "requisicao", Quantity: new(big.Rat)}}
	}
	res := f.run(t)
	if res.Invoices != 0 || len(f.store.invoices) != 0 || f.store.seq != 0 {
		t.Fatalf("sem consumo não há fatura: %+v", res)
	}
	if f.store.sub(sub.ID).NextBillingIndex != 2 {
		t.Fatal("a assinatura avança mesmo sem fatura")
	}
}

func TestEngineBillsPendingItemsOnce(t *testing.T) {
	f := newEngineFixture(t, catalog.Flat)
	sub := f.addSub(t, engineNow.AddDate(0, -1, 0), nil)
	pending := PendingItem{ID: uuid.New(), Kind: ItemProration, Description: "Prorrata de upgrade", Amount: brl(1500)}
	f.store.pending[sub.ID] = []PendingItem{pending}
	f.run(t)
	if len(f.store.invoices) != 2 {
		t.Fatalf("faturas: %d", len(f.store.invoices))
	}
	first, second := f.store.invoices[0], f.store.invoices[1]
	if len(first.Items) != 2 || first.Total.Amount() != 11500 || f.store.pendingBilled[pending.ID] != first.ID {
		t.Fatalf("item pendente na primeira fatura: %+v", first.Items)
	}
	if len(second.Items) != 1 || second.Total.Amount() != 10000 {
		t.Fatalf("item pendente não pode ser cobrado de novo: %+v", second.Items)
	}
}

func TestEngineEndsSubscriptionAtPeriodEnd(t *testing.T) {
	f := newEngineFixture(t, catalog.FlatPlusOverage)
	sub := f.addSub(t, engineNow.AddDate(0, -1, 0), nil)
	sched, _ := sub.Schedule()
	// Período 0 já faturado; cancelamento agendado para o fim dele.
	stored := f.store.subs[0]
	if err := stored.ApplyStep(subscriptions.BillingStep{Kind: subscriptions.StepBill, Index: 0, Period: sched.Period(0)}, sched.Period(0).Start); err != nil {
		t.Fatal(err)
	}
	if err := stored.Cancel(subscriptions.CancelAtPeriodEnd, "", true, false, sched.Period(0).Start); err != nil {
		t.Fatal(err)
	}
	stored.PopEvents()
	f.store.subs[0] = stored
	f.store.discount = &Discount{ID: uuid.New(), PercentBP: 5000}
	f.store.usage = func(subscriptions.Subscription, calendar.Period) []UsageLine {
		return []UsageLine{{MetricName: "Requisições", Unit: "requisicao", Quantity: big.NewRat(1600, 1)}}
	}

	res := f.run(t)
	if res.Invoices != 1 {
		t.Fatalf("encerramento fatura o consumo final: %+v", res)
	}
	inv := f.store.invoices[0]
	if inv.Kind != KindFinalUsage || *inv.BillingPeriod != sched.Period(0) {
		t.Fatalf("fatura de consumo final do período encerrado: %s %+v", inv.Kind, inv.BillingPeriod)
	}
	if len(inv.Items) != 1 || inv.Items[0].Kind != ItemOverage || inv.Items[0].Amount.Amount() != 1200 {
		t.Fatalf("somente o excedente, sem mensalidade: %+v", inv.Items)
	}
	if !inv.DiscountTotal.IsZero() || f.store.pendingQueries != 1 {
		t.Fatal("encerramento não aplica descontos e consulta os itens pendentes")
	}
	got := f.store.sub(sub.ID)
	if got.Status != subscriptions.Canceled || !got.EndedAt.Equal(sched.Period(1).Start) || got.NextBillingAt != nil {
		t.Fatalf("assinatura encerrada: %+v", got)
	}
}

func TestEngineEndsTrialWithScheduledCancelWithoutInvoice(t *testing.T) {
	f := newEngineFixture(t, catalog.Usage)
	start := engineNow.AddDate(0, 0, -7)
	sub := f.addSub(t, start, func(in *subscriptions.NewInput) { in.TrialDays = 7 })
	stored := f.store.subs[0]
	if err := stored.Cancel(subscriptions.CancelAtPeriodEnd, "não converteu", true, false, start); err != nil {
		t.Fatal(err)
	}
	stored.PopEvents()
	f.store.subs[0] = stored

	res := f.run(t)
	if res.Invoices != 0 || len(f.store.invoices) != 0 || len(f.store.usageQueries) != 0 {
		t.Fatalf("trial cancelado não gera fatura nem consulta consumo: %+v", res)
	}
	got := f.store.sub(sub.ID)
	if got.Status != subscriptions.Canceled || !got.EndedAt.Equal(*got.TrialEndsAt) {
		t.Fatalf("encerrada no fim do trial: %s %v", got.Status, got.EndedAt)
	}
}

func TestEngineBillsFinalUsageAfterImmediateCancel(t *testing.T) {
	f := newEngineFixture(t, catalog.Usage)
	start := engineNow.AddDate(0, 0, -10)
	sub := f.addSub(t, start, nil)
	sched, _ := sub.Schedule()
	stored := f.store.subs[0]
	if err := stored.ApplyStep(subscriptions.BillingStep{Kind: subscriptions.StepBill, Index: 0, Period: sched.Period(0)}, start); err != nil {
		t.Fatal(err)
	}
	if err := stored.Cancel(subscriptions.CancelImmediately, "", true, false, engineNow); err != nil {
		t.Fatal(err)
	}
	stored.PopEvents()
	f.store.subs[0] = stored
	f.store.usage = func(_ subscriptions.Subscription, p calendar.Period) []UsageLine {
		return []UsageLine{{MetricName: "Requisições", Unit: "requisicao", Quantity: big.NewRat(1000, 1)}}
	}
	res := f.run(t)
	if res.Invoices != 1 {
		t.Fatalf("consumo final: %+v", res)
	}
	inv := f.store.invoices[0]
	want := calendar.Period{Start: sched.Period(0).Start, End: engineNow}
	if inv.Kind != KindFinalUsage || *inv.BillingPeriod != want || inv.Total.Amount() != 350 {
		t.Fatalf("fatura final: %s %+v %v", inv.Kind, inv.BillingPeriod, inv.Total)
	}
	if f.store.sub(sub.ID).NextBillingAt != nil {
		t.Fatal("após o consumo final não há mais faturamento")
	}
}

func TestEngineRollsBackOnFailure(t *testing.T) {
	methods := []string{"ClaimDueSubscription", "TenantSettings", "Plan", "PriceVersion", "CycleInvoiceExists", "CustomerForInvoice",
		"PendingItems", "ActiveDiscount", "CustomerCreditBalance", "NextInvoiceNumber", "InsertInvoice", "InsertStatusHistory",
		"MarkUsageBilled", "MarkPendingItemsBilled", "ConsumeDiscountCycle", "ConsumeCustomerCredit", "PostLedger", "Emit", "Audit",
		"UpdateSubscription", "InsertSubscriptionHistory", "UsageFor"}
	for _, m := range methods {
		t.Run(m, func(t *testing.T) {
			f := newEngineFixture(t, catalog.FlatPlusOverage)
			sub := f.addSub(t, engineNow.AddDate(0, -1, 0), nil)
			aggID := uuid.New()
			f.store.usage = func(subscriptions.Subscription, calendar.Period) []UsageLine {
				return []UsageLine{{MetricName: "Requisições", Unit: "requisicao", Quantity: big.NewRat(2000, 1), AggregateID: &aggID}}
			}
			f.store.pending[sub.ID] = []PendingItem{{ID: uuid.New(), Kind: ItemAdjustment, Description: "Ajuste", Amount: brl(100)}}
			f.store.discount = &Discount{ID: uuid.New(), PercentBP: 500}
			f.store.credit[sub.CustomerID] = brl(100)
			boom := errors.New("falha em " + m)
			f.store.errs[m] = boom

			_, err := f.engine.RunTenant(context.Background(), f.tenantID, f.actor)
			if !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			if len(f.store.invoices) != 0 || len(f.store.posted) != 0 || len(f.store.events) != 0 || f.store.seq != 0 ||
				f.store.sub(sub.ID).NextBillingIndex != 0 || len(f.store.creditUses) != 0 {
				t.Fatalf("falha deve desfazer toda a transação: faturas=%d lançamentos=%d", len(f.store.invoices), len(f.store.posted))
			}
		})
	}
}

func TestEngineReportsInvalidSubscriptionTimezone(t *testing.T) {
	f := newEngineFixture(t, catalog.Flat)
	f.addSub(t, engineNow, nil)
	f.store.subs[0].Timezone = "Marte/Olympus_Mons"
	if _, err := f.engine.RunTenant(context.Background(), f.tenantID, f.actor); err == nil {
		t.Fatal("fuso inválido deve interromper o processamento")
	}
	if len(f.store.invoices) != 0 {
		t.Fatal("nenhuma fatura deve ser emitida")
	}
}

func TestEngineRunTenantHonorsCancellation(t *testing.T) {
	f := newEngineFixture(t, catalog.Flat)
	f.addSub(t, engineNow, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.engine.RunTenant(ctx, f.tenantID, f.actor); !errors.Is(err, context.Canceled) {
		t.Fatalf("esperava cancelamento: %v", err)
	}
	if f.store.txCount != 0 {
		t.Fatal("nenhuma transação após cancelamento")
	}
}

func TestEngineRunTenantStopsAtMaxSubscriptions(t *testing.T) {
	f := newEngineFixture(t, catalog.Flat)
	f.engine.MaxSubscriptionsPerRun = 2
	for range 3 {
		f.addSub(t, engineNow, nil)
	}
	if res := f.run(t); res.Subscriptions != 2 || res.Invoices != 2 {
		t.Fatalf("limite por execução: %+v", res)
	}
	if res := f.run(t); res.Subscriptions != 1 {
		t.Fatalf("a assinatura restante fica para a próxima execução: %+v", res)
	}
}

func TestEngineRunAll(t *testing.T) {
	f := newEngineFixture(t, catalog.Flat)
	other := uuid.New()
	f.addSub(t, engineNow, nil)
	f.addSub(t, engineNow, func(in *subscriptions.NewInput) { in.TenantID = other })
	f.store.tenants = []uuid.UUID{f.tenantID, other}

	res, err := f.engine.RunAll(context.Background())
	if err != nil || res.Invoices != 2 || res.Subscriptions != 2 || len(res.InvoiceIDs) != 2 {
		t.Fatalf("RunAll: %+v %v", res, err)
	}
	if f.store.tenantsLimit != 1000 {
		t.Fatalf("limite de tenants por execução: %d", f.store.tenantsLimit)
	}
	for _, a := range f.store.audits {
		if a.Actor != audit.System("billing.engine") {
			t.Fatalf("execução agendada usa o ator do motor: %+v", a.Actor)
		}
	}
	if f.store.invoices[0].TenantID != f.tenantID || f.store.invoices[1].TenantID != other {
		t.Fatal("cada fatura pertence ao tenant da assinatura")
	}
}

func TestEngineRunAllStopsAtFailingTenant(t *testing.T) {
	f := newEngineFixture(t, catalog.Flat)
	failing, last := uuid.New(), uuid.New()
	f.addSub(t, engineNow, nil)
	f.addSub(t, engineNow, func(in *subscriptions.NewInput) { in.TenantID = last })
	boom := errors.New("banco indisponível")
	f.store.failTenant[failing] = boom
	f.store.tenants = []uuid.UUID{f.tenantID, failing, last}

	res, err := f.engine.RunAll(context.Background())
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), failing.String()) {
		t.Fatalf("erro deve identificar o tenant: %v", err)
	}
	if res.Invoices != 1 || len(res.InvoiceIDs) != 1 {
		t.Fatalf("resultado parcial do primeiro tenant: %+v", res)
	}
}

func TestEngineRunAllErrors(t *testing.T) {
	t.Run("falha ao listar tenants", func(t *testing.T) {
		f := newEngineFixture(t, catalog.Flat)
		boom := errors.New("consulta falhou")
		f.store.tenantsErr = boom
		if _, err := f.engine.RunAll(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("esperava %v, obtive %v", boom, err)
		}
	})
	t.Run("cancelamento", func(t *testing.T) {
		f := newEngineFixture(t, catalog.Flat)
		f.store.tenants = []uuid.UUID{f.tenantID}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := f.engine.RunAll(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("esperava cancelamento: %v", err)
		}
	})
}

func intPtr(v int) *int { return &v }
