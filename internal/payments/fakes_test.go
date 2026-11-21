package payments

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

var saoPaulo = func() *time.Location {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		panic(err)
	}
	return loc
}()

type creditGrant struct {
	customerID uuid.UUID
	amount     money.Money
	kind       string
	invoiceID  *uuid.UUID
	reason     string
}

type invoiceHistoryWrite struct {
	invoiceID uuid.UUID
	actor     audit.Actor
	changes   []billing.StatusChange
}

type savedAccount struct {
	account   ProviderAccount
	tokenHash []byte
}

type endpoint struct {
	provider string
	tenantID uuid.UUID
	account  uuid.UUID
}

// payData é o estado mutável do armazenamento falso, copiado no início de
// cada transação e restaurado se ela falhar.
type payData struct {
	accounts          map[uuid.UUID]ProviderAccount
	saved             []savedAccount
	invoices          map[uuid.UUID]billing.Invoice
	invoiceHistory    []invoiceHistoryWrite
	standing          []uuid.UUID
	providerCustomers map[string]string
	charges           map[uuid.UUID]Charge
	chargeOrder       []uuid.UUID
	operations        map[uuid.UUID]Operation
	payments          []Payment
	credits           []creditGrant
	webhooks          map[uuid.UUID]StoredWebhook
	inbox             map[string]bool
	posted            []ledger.Transaction
	// ledger guarda todos os lançamentos gravados, como o banco: os testes
	// limpam posted para observar só os lançamentos seguintes, sem apagar o
	// histórico usado pela idempotência e pelo saldo a receber do pagamento.
	ledger []ledger.Transaction
	audits []audit.Entry
	events []outbox.Event
	receivables
}

func (d payData) clone() payData {
	c := d
	c.accounts = maps.Clone(d.accounts)
	c.saved = slices.Clone(d.saved)
	c.invoices = maps.Clone(d.invoices)
	c.invoiceHistory = slices.Clone(d.invoiceHistory)
	c.standing = slices.Clone(d.standing)
	c.providerCustomers = maps.Clone(d.providerCustomers)
	c.charges = maps.Clone(d.charges)
	c.chargeOrder = slices.Clone(d.chargeOrder)
	c.operations = maps.Clone(d.operations)
	c.payments = slices.Clone(d.payments)
	c.credits = slices.Clone(d.credits)
	c.webhooks = maps.Clone(d.webhooks)
	c.inbox = maps.Clone(d.inbox)
	c.posted = slices.Clone(d.posted)
	c.ledger = slices.Clone(d.ledger)
	c.audits = slices.Clone(d.audits)
	c.events = slices.Clone(d.events)
	c.receivables = d.receivables.clone()
	return c
}

// payStore implementa Store e Tx em memória.
type payStore struct {
	payData
	loc          *time.Location
	plan         platformbilling.Plan
	customers    map[uuid.UUID]CustomerData
	endpoints    map[string]endpoint
	errs         map[string]error
	tenantDueErr map[uuid.UUID]error
	tenantsErr   error
	current      uuid.UUID
	txCount      int
	dueLimit     int
	extraOpen    []Charge
	// locks registra, em ordem, as operações que bloqueiam a assinatura
	// ("subscription") ou o cliente ("customer") no armazenamento real.
	locks []string
}

func newPayStore() *payStore {
	return &payStore{
		payData: payData{accounts: map[uuid.UUID]ProviderAccount{}, invoices: map[uuid.UUID]billing.Invoice{}, providerCustomers: map[string]string{},
			charges: map[uuid.UUID]Charge{}, operations: map[uuid.UUID]Operation{}, webhooks: map[uuid.UUID]StoredWebhook{}, inbox: map[string]bool{},
			receivables: newReceivables()},
		loc: saoPaulo, customers: map[uuid.UUID]CustomerData{}, endpoints: map[string]endpoint{}, errs: map[string]error{},
		tenantDueErr: map[uuid.UUID]error{},
	}
}

func (s *payStore) InTenant(_ context.Context, tenantID uuid.UUID, fn func(Tx) error) error {
	s.txCount++
	s.current = tenantID
	backup := s.clone()
	if err := fn(s); err != nil {
		s.payData = backup
		return err
	}
	return nil
}

func (s *payStore) ResolveWebhookEndpoint(_ context.Context, provider string, tokenHash []byte) (uuid.UUID, uuid.UUID, error) {
	if err := s.fail("ResolveWebhookEndpoint"); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	e, ok := s.endpoints[hex.EncodeToString(tokenHash)]
	if !ok || e.provider != provider {
		return uuid.Nil, uuid.Nil, apperr.NotFound("endpoint_not_found", "Endpoint não encontrado.")
	}
	return e.tenantID, e.account, nil
}

func (s *payStore) TenantsWithPendingOperations(_ context.Context, now time.Time) ([]uuid.UUID, error) {
	if s.tenantsErr != nil {
		return nil, s.tenantsErr
	}
	// Como a função do banco, não filtra tentativas nem consulta a
	// cobrança: inclui os tenants com operações esgotadas.
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for _, op := range s.sortedOperations() {
		if (op.Claimable(now) || op.Exhausted(now, ChargePending)) && !seen[op.TenantID] {
			seen[op.TenantID] = true
			out = append(out, op.TenantID)
		}
	}
	return out, nil
}

func (s *payStore) ExhaustedOperations(_ context.Context, now time.Time) (map[OperationStatus]int, error) {
	if err := s.fail("ExhaustedOperations"); err != nil {
		return nil, err
	}
	out := map[OperationStatus]int{}
	for _, op := range s.operations {
		if op.TenantID == s.current && op.Exhausted(now, s.charges[op.ChargeID].Status) {
			out[op.Status]++
		}
	}
	return out, nil
}

func (s *payStore) sortedOperations() []Operation {
	ops := slices.Collect(maps.Values(s.operations))
	slices.SortFunc(ops, func(a, b Operation) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	return ops
}

func (s *payStore) fail(m string) error { return s.errs[m] }

func inboxKey(consumer string, id uuid.UUID) string { return consumer + "|" + id.String() }

func (s *payStore) MarkInbox(_ context.Context, consumer string, id uuid.UUID) (bool, error) {
	if err := s.fail("MarkInbox"); err != nil {
		return false, err
	}
	k := inboxKey(consumer, id)
	if s.inbox[k] {
		return false, nil
	}
	s.inbox[k] = true
	return true, nil
}

func (s *payStore) Location(context.Context) (*time.Location, error) {
	return s.loc, s.fail("Location")
}

func (s *payStore) PlatformPlan(context.Context) (platformbilling.Plan, error) {
	return s.plan, s.fail("PlatformPlan")
}

func (s *payStore) DefaultAccount(context.Context) (ProviderAccount, error) {
	if err := s.fail("DefaultAccount"); err != nil {
		return ProviderAccount{}, err
	}
	for _, a := range s.accounts {
		if a.IsDefault && a.TenantID == s.current {
			return a, nil
		}
	}
	return ProviderAccount{}, apperr.NotFound("provider_account_not_found", "Nenhum provedor padrão.")
}

func (s *payStore) Account(_ context.Context, id uuid.UUID) (ProviderAccount, error) {
	if err := s.fail("Account"); err != nil {
		return ProviderAccount{}, err
	}
	a, ok := s.accounts[id]
	if !ok {
		return ProviderAccount{}, apperr.NotFound("provider_account_not_found", "Conta não encontrada.")
	}
	return a, nil
}

func (s *payStore) AccountByProvider(_ context.Context, provider string) (ProviderAccount, error) {
	if err := s.fail("AccountByProvider"); err != nil {
		return ProviderAccount{}, err
	}
	for _, a := range s.accounts {
		if a.Provider == provider && a.TenantID == s.current {
			return a, nil
		}
	}
	return ProviderAccount{}, apperr.NotFound("provider_account_not_found", "Conta não encontrada.")
}

// SaveAccount reproduz a gravação condicionada à versão: 0 cria a conta do
// provedor; acima de 0, substitui a configuração com essa versão.
func (s *payStore) SaveAccount(_ context.Context, a ProviderAccount, hash []byte, expectedVersion int) error {
	if err := s.fail("SaveAccount"); err != nil {
		return err
	}
	current := 0
	for _, x := range s.accounts {
		if x.Provider == a.Provider && x.TenantID == a.TenantID {
			current = x.Version
		}
	}
	if current != expectedVersion {
		return apperr.PreconditionFailed("version_mismatch", "A configuração do provedor foi alterada por outra pessoa.")
	}
	a.Version = expectedVersion + 1
	s.accounts[a.ID] = a
	s.saved = append(s.saved, savedAccount{a, hash})
	return nil
}

func (s *payStore) Accounts(context.Context) ([]ProviderAccount, error) {
	if err := s.fail("Accounts"); err != nil {
		return nil, err
	}
	out := slices.Collect(maps.Values(s.accounts))
	slices.SortFunc(out, func(a, b ProviderAccount) int { return strings.Compare(a.Provider, b.Provider) })
	return out, nil
}

func (s *payStore) Invoice(_ context.Context, id uuid.UUID, _ bool) (billing.Invoice, error) {
	if err := s.fail("Invoice"); err != nil {
		return billing.Invoice{}, err
	}
	inv, ok := s.invoices[id]
	if !ok {
		return billing.Invoice{}, apperr.NotFound("invoice_not_found", "Fatura não encontrada.")
	}
	return inv, nil
}

func (s *payStore) UpdateInvoiceProgress(_ context.Context, inv *billing.Invoice) error {
	if err := s.fail("UpdateInvoiceProgress"); err != nil {
		return err
	}
	cp := *inv
	cp.PopChanges()
	cp.Version++
	s.invoices[inv.ID] = cp
	return nil
}

func (s *payStore) InsertInvoiceHistory(_ context.Context, id uuid.UUID, actor audit.Actor, changes []billing.StatusChange) error {
	if err := s.fail("InsertInvoiceHistory"); err != nil {
		return err
	}
	s.invoiceHistory = append(s.invoiceHistory, invoiceHistoryWrite{id, actor, changes})
	return nil
}

func (s *payStore) RefreshSubscriptionStanding(_ context.Context, id uuid.UUID, _ audit.Actor, _ time.Time) error {
	if err := s.fail("RefreshSubscriptionStanding"); err != nil {
		return err
	}
	s.locks = append(s.locks, "subscription")
	s.standing = append(s.standing, id)
	return nil
}

func (s *payStore) CustomerData(_ context.Context, id uuid.UUID, provider string) (CustomerData, error) {
	if err := s.fail("CustomerData"); err != nil {
		return CustomerData{}, err
	}
	c := s.customers[id]
	c.ProviderCustomerID = s.providerCustomers[id.String()+"|"+provider]
	return c, nil
}

func (s *payStore) SaveProviderCustomer(_ context.Context, id uuid.UUID, provider, providerID string) error {
	if err := s.fail("SaveProviderCustomer"); err != nil {
		return err
	}
	s.locks = append(s.locks, "customer") // a chave estrangeira bloqueia a linha do cliente
	s.providerCustomers[id.String()+"|"+provider] = providerID
	return nil
}

func (s *payStore) LatestCharge(_ context.Context, invoiceID uuid.UUID) (*Charge, error) {
	if err := s.fail("LatestCharge"); err != nil {
		return nil, err
	}
	for i := len(s.chargeOrder) - 1; i >= 0; i-- {
		if c := s.charges[s.chargeOrder[i]]; c.InvoiceID == invoiceID {
			return &c, nil
		}
	}
	return nil, nil
}

func (s *payStore) InsertCharge(_ context.Context, c Charge) error {
	if err := s.fail("InsertCharge"); err != nil {
		return err
	}
	s.charges[c.ID] = c
	s.chargeOrder = append(s.chargeOrder, c.ID)
	return nil
}

func (s *payStore) Charge(_ context.Context, id uuid.UUID, _ bool) (Charge, error) {
	if err := s.fail("Charge"); err != nil {
		return Charge{}, err
	}
	c, ok := s.charges[id]
	if !ok {
		return Charge{}, apperr.NotFound("charge_not_found", "Cobrança não encontrada.")
	}
	return c, nil
}

func (s *payStore) ChargeByProviderID(_ context.Context, provider, providerID string, _ bool) (Charge, error) {
	if err := s.fail("ChargeByProviderID"); err != nil {
		return Charge{}, err
	}
	for _, id := range s.chargeOrder {
		if c := s.charges[id]; c.Provider == provider && c.ProviderChargeID == providerID {
			return c, nil
		}
	}
	return Charge{}, apperr.NotFound("charge_not_found", "Cobrança não encontrada.")
}

func (s *payStore) UpdateCharge(_ context.Context, c *Charge) error {
	if err := s.fail("UpdateCharge"); err != nil {
		return err
	}
	cp := *c
	cp.Version++
	s.charges[c.ID] = cp
	return nil
}

func (s *payStore) OpenChargesForInvoice(_ context.Context, invoiceID uuid.UUID) ([]Charge, error) {
	if err := s.fail("OpenChargesForInvoice"); err != nil {
		return nil, err
	}
	var out []Charge
	for _, id := range s.chargeOrder {
		c := s.charges[id]
		if c.InvoiceID == invoiceID && (c.Status == ChargePending || c.Status == ChargeAwaitingPayment) {
			out = append(out, c)
		}
	}
	return append(out, s.extraOpen...), nil
}

func (s *payStore) InsertOperation(_ context.Context, op Operation) error {
	if err := s.fail("InsertOperation"); err != nil {
		return err
	}
	s.operations[op.ID] = op
	return nil
}

func (s *payStore) Operation(_ context.Context, id uuid.UUID, _ bool) (Operation, error) {
	if err := s.fail("Operation"); err != nil {
		return Operation{}, err
	}
	op, ok := s.operations[id]
	if !ok {
		return Operation{}, apperr.NotFound("operation_not_found", "Operação não encontrada.")
	}
	return op, nil
}

func (s *payStore) UpdateOperation(_ context.Context, op Operation) error {
	if err := s.fail("UpdateOperation"); err != nil {
		return err
	}
	s.operations[op.ID] = op
	return nil
}

func (s *payStore) DueOperationIDs(_ context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	s.dueLimit = limit
	if err := s.tenantDueErr[s.current]; err != nil {
		return nil, err
	}
	var out []uuid.UUID
	for _, op := range s.sortedOperations() {
		if op.TenantID == s.current && op.Claimable(now) {
			out = append(out, op.ID)
		}
	}
	return out, nil
}

func (s *payStore) InsertPayment(_ context.Context, p Payment) (bool, error) {
	if err := s.fail("InsertPayment"); err != nil {
		return false, err
	}
	for _, e := range s.payments {
		if (p.ChargeID != nil && e.ChargeID != nil && *e.ChargeID == *p.ChargeID) || (p.RequestKey != "" && e.RequestKey == p.RequestKey) {
			return false, nil
		}
	}
	s.payments = append(s.payments, p)
	return true, nil
}

func (s *payStore) PaymentByRequestKey(_ context.Context, key string) (Payment, error) {
	if err := s.fail("PaymentByRequestKey"); err != nil {
		return Payment{}, err
	}
	for _, p := range s.payments {
		if p.RequestKey == key {
			return p, nil
		}
	}
	return Payment{}, apperr.NotFound("payment_not_found", "Pagamento não encontrado.")
}

func (s *payStore) GrantCustomerCredit(_ context.Context, customerID uuid.UUID, amount money.Money, kind string, invoiceID *uuid.UUID, reason string, _ time.Time) error {
	if err := s.fail("GrantCustomerCredit"); err != nil {
		return err
	}
	s.locks = append(s.locks, "customer")
	// Como no banco: o consumo nunca deixa o saldo negativo.
	balance, ok := s.creditBalance[customerID]
	if !ok {
		balance = brl(0)
	}
	next, err := balance.Add(amount)
	if err != nil {
		return err
	}
	if amount.IsNegative() && next.IsNegative() {
		return apperr.Conflict("insufficient_credit", "Saldo de crédito insuficiente.")
	}
	s.creditBalance[customerID] = next
	s.credits = append(s.credits, creditGrant{customerID, amount, kind, invoiceID, reason})
	return nil
}

func (s *payStore) InsertWebhook(_ context.Context, w StoredWebhook) (bool, error) {
	if err := s.fail("InsertWebhook"); err != nil {
		return false, err
	}
	for _, e := range s.webhooks {
		if e.Provider == w.Provider && e.EventID == w.EventID {
			return false, nil
		}
	}
	s.webhooks[w.ID] = w
	return true, nil
}

func (s *payStore) Webhook(_ context.Context, id uuid.UUID, _ bool) (StoredWebhook, error) {
	if err := s.fail("Webhook"); err != nil {
		return StoredWebhook{}, err
	}
	w, ok := s.webhooks[id]
	if !ok {
		return StoredWebhook{}, apperr.NotFound("webhook_not_found", "Webhook não encontrado.")
	}
	return w, nil
}

func (s *payStore) UpdateWebhookStatus(_ context.Context, id uuid.UUID, status, errText string, _ time.Time) error {
	if err := s.fail("UpdateWebhookStatus"); err != nil {
		return err
	}
	w := s.webhooks[id]
	w.Status, w.Error = status, errText
	s.webhooks[id] = w
	return nil
}

func (s *payStore) PostLedger(_ context.Context, t ledger.Transaction) error {
	if err := s.fail("PostLedger"); err != nil {
		return err
	}
	if err := t.Validate(); err != nil {
		return err
	}
	// Como o banco, a chave de idempotência descarta o lançamento repetido.
	if slices.ContainsFunc(s.ledger, func(p ledger.Transaction) bool { return p.IdempotencyKey == t.IdempotencyKey }) {
		return nil
	}
	s.posted = append(s.posted, t)
	s.ledger = append(s.ledger, t)
	return nil
}

func (s *payStore) PendingReceipts(ctx context.Context, chargeIDs []uuid.UUID) (map[uuid.UUID]money.Money, error) {
	if err := s.fail("PendingReceipts"); err != nil {
		return nil, err
	}
	out := map[uuid.UUID]money.Money{}
	for _, id := range chargeIDs {
		_, pending, ok, err := s.PaymentPendingReceipt(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok && !pending.IsZero() {
			out[id] = pending
		}
	}
	return out, nil
}

func (s *payStore) PaymentPendingReceipt(_ context.Context, chargeID uuid.UUID) (Payment, money.Money, bool, error) {
	if err := s.fail("PaymentPendingReceipt"); err != nil {
		return Payment{}, money.Money{}, false, err
	}
	for _, p := range s.payments {
		if p.ChargeID == nil || *p.ChargeID != chargeID {
			continue
		}
		pending := money.Zero(p.Amount.Currency())
		for _, t := range s.ledger {
			if t.ReferenceType != "payment" || t.ReferenceID != p.ID {
				continue
			}
			for _, e := range t.Entries {
				if e.Account != ledger.ProviderConfirmed {
					continue
				}
				var err error
				if e.Direction == ledger.Debit {
					pending, err = pending.Add(e.Amount)
				} else {
					pending, err = pending.Sub(e.Amount)
				}
				if err != nil {
					return Payment{}, money.Money{}, false, err
				}
			}
		}
		return p, pending, true, nil
	}
	return Payment{}, money.Money{}, false, nil
}

func (s *payStore) Audit(_ context.Context, e audit.Entry) error {
	if err := s.fail("Audit"); err != nil {
		return err
	}
	s.audits = append(s.audits, e)
	return nil
}

func (s *payStore) Emit(_ context.Context, e outbox.Event) error {
	if err := s.fail("Emit"); err != nil {
		return err
	}
	s.events = append(s.events, e)
	return nil
}

func (s *payStore) eventTypes() []string {
	out := make([]string, len(s.events))
	for i, e := range s.events {
		out[i] = e.Type
	}
	return out
}

func (s *payStore) ledgerKinds() []string {
	out := make([]string, len(s.posted))
	for i, t := range s.posted {
		out[i] = t.Kind
	}
	return out
}

// Gateway falso controlável.

type gwResult struct {
	pc  ProviderCharge
	err error
}

type configureCall struct {
	url, secret string
	deadline    bool
}

type fakeGateway struct {
	createCalls    []CreateChargeRequest
	createResults  []gwResult
	createDeadline []time.Duration
	onCreate       func()
	findCalls      []string
	findResult     *ProviderCharge
	findErr        error
	ensureCalls    []CustomerData
	ensureID       string
	ensureErr      error
	getCalls       []string
	getResult      ProviderCharge
	getErr         error
	configureCalls []configureCall
	configureErr   error
	onConfigure    func()
	getByID        map[string]gwResult
	refundCalls    []RefundRequest
	refundResults  []refundRes
	onRefund       func(RefundRequest)
	cancelCalls    []string
	cancelErrs     []error
	listCalls      []ListRequest
	listPages      []ListResult
	listErr        error
	onList         func()
	onGet          func()
}

type refundRes struct {
	pr  ProviderRefund
	err error
}

func (g *fakeGateway) EnsureCustomer(_ context.Context, c CustomerData) (string, error) {
	g.ensureCalls = append(g.ensureCalls, c)
	if g.ensureErr != nil {
		return "", g.ensureErr
	}
	return g.ensureID, nil
}

func (g *fakeGateway) CreateCharge(ctx context.Context, req CreateChargeRequest) (ProviderCharge, error) {
	g.createCalls = append(g.createCalls, req)
	if dl, ok := ctx.Deadline(); ok {
		g.createDeadline = append(g.createDeadline, time.Until(dl))
	}
	if g.onCreate != nil {
		g.onCreate()
	}
	if len(g.createResults) > 0 {
		r := g.createResults[0]
		g.createResults = g.createResults[1:]
		if r.err != nil {
			return ProviderCharge{}, r.err
		}
		return r.pc, nil
	}
	n := len(g.createCalls)
	return ProviderCharge{ProviderChargeID: fmt.Sprintf("pay_%d", n), ExternalReference: req.ExternalReference,
		ProviderCustomerID: req.Customer.ProviderCustomerID, Status: ProviderPending, RawStatus: "PENDING", Amount: req.Amount,
		CheckoutURL: fmt.Sprintf("https://sandbox.exemplo/c/pay_%d", n)}, nil
}

func (g *fakeGateway) GetCharge(_ context.Context, id string) (ProviderCharge, error) {
	g.getCalls = append(g.getCalls, id)
	if g.onGet != nil {
		g.onGet()
	}
	if r, ok := g.getByID[id]; ok {
		return r.pc, r.err
	}
	return g.getResult, g.getErr
}

func (g *fakeGateway) FindByExternalReference(_ context.Context, ref string) (*ProviderCharge, error) {
	g.findCalls = append(g.findCalls, ref)
	return g.findResult, g.findErr
}

func (g *fakeGateway) Refund(_ context.Context, req RefundRequest) (ProviderRefund, error) {
	g.refundCalls = append(g.refundCalls, req)
	if g.onRefund != nil {
		g.onRefund(req)
	}
	if len(g.refundResults) > 0 {
		r := g.refundResults[0]
		g.refundResults = g.refundResults[1:]
		return r.pr, r.err
	}
	return ProviderRefund{ProviderRefundID: fmt.Sprintf("rf_%d", len(g.refundCalls)), ExternalReference: req.ExternalReference,
		Status: "succeeded", State: RefundStateDone, Amount: req.Amount}, nil
}

func (g *fakeGateway) CancelCharge(_ context.Context, id string) error {
	g.cancelCalls = append(g.cancelCalls, id)
	if len(g.cancelErrs) > 0 {
		err := g.cancelErrs[0]
		g.cancelErrs = g.cancelErrs[1:]
		return err
	}
	return nil
}

func (g *fakeGateway) ListCharges(_ context.Context, req ListRequest) (ListResult, error) {
	g.listCalls = append(g.listCalls, req)
	if g.onList != nil {
		g.onList()
	}
	if g.listErr != nil {
		return ListResult{}, g.listErr
	}
	page := req.Offset / max(req.Limit, 1)
	if page < len(g.listPages) {
		return g.listPages[page], nil
	}
	return ListResult{}, nil
}

func (g *fakeGateway) ConfigureWebhook(ctx context.Context, url, secret string) error {
	_, ok := ctx.Deadline()
	g.configureCalls = append(g.configureCalls, configureCall{url, secret, ok})
	if g.onConfigure != nil {
		g.onConfigure()
	}
	return g.configureErr
}

type verifyCall struct {
	secret string
	now    time.Time
}

// fakeVerifier interpreta corpos previamente registrados em events.
type fakeVerifier struct {
	events      map[string]WebhookEvent
	verifyErr   error
	parseErr    error
	verifyCalls []verifyCall
}

func (v *fakeVerifier) Verify(_ http.Header, _ []byte, secret string, now time.Time) error {
	v.verifyCalls = append(v.verifyCalls, verifyCall{secret, now})
	return v.verifyErr
}

func (v *fakeVerifier) Parse(body []byte) (WebhookEvent, error) {
	if v.parseErr != nil {
		return WebhookEvent{}, v.parseErr
	}
	ev, ok := v.events[string(body)]
	if !ok {
		return WebhookEvent{}, fmt.Errorf("corpo desconhecido: %q", body)
	}
	return ev, nil
}

type fakeResolver struct {
	gw *fakeGateway
	// override, quando definido, substitui gw (gateways com comportamento
	// próprio construídos sobre o falso).
	override    Gateway
	gwErr       error
	verifier    *fakeVerifier
	verifierErr error
	unsupported map[string]bool
	accounts    []ProviderAccount
}

func (r *fakeResolver) Gateway(a ProviderAccount) (Gateway, error) {
	r.accounts = append(r.accounts, a)
	if r.gwErr != nil {
		return nil, r.gwErr
	}
	if r.override != nil {
		return r.override, nil
	}
	return r.gw, nil
}

func (r *fakeResolver) Verifier(string) (WebhookVerifier, error) {
	if r.verifierErr != nil {
		return nil, r.verifierErr
	}
	return r.verifier, nil
}

func (r *fakeResolver) Supports(p string) bool {
	return (p == "simulator" || p == "asaas") && !r.unsupported[p]
}

// Cenário de teste.

var payNow = time.Date(2026, time.November, 16, 17, 30, 0, 0, time.UTC) // 14:30 em São Paulo

const endpointToken = "tok_0123456789abcdefghijklmnopqrstuv"

type payFixture struct {
	store    *payStore
	gw       *fakeGateway
	verifier *fakeVerifier
	resolver *fakeResolver
	clock    *clock.Manual
	svc      *Service
	logs     *bytes.Buffer
	tenantID uuid.UUID
	account  ProviderAccount
	customer uuid.UUID
	actor    audit.Actor
	userID   uuid.UUID
}

func newPayFixture(t *testing.T) *payFixture {
	t.Helper()
	f := &payFixture{store: newPayStore(), clock: clock.NewManual(payNow), tenantID: uuid.New(), customer: uuid.New(), userID: uuid.New(),
		logs: &bytes.Buffer{}}
	f.actor = audit.User(f.userID, "Ana Souza")
	f.gw = &fakeGateway{ensureID: "cus_1"}
	f.verifier = &fakeVerifier{events: map[string]WebhookEvent{}}
	f.resolver = &fakeResolver{gw: f.gw, verifier: f.verifier, unsupported: map[string]bool{}}
	f.account = ProviderAccount{ID: uuid.New(), TenantID: f.tenantID, Provider: "simulator", Environment: "local", Status: "active",
		IsDefault: true, APIKey: "sim_key_123456", Secret: "segredo-do-webhook-com-32-caracteres!", Version: 1}
	f.store.accounts[f.account.ID] = f.account
	f.store.customers[f.customer] = CustomerData{ID: f.customer, Name: "Padaria Central", DocumentNumber: "11222333000181", Email: "financeiro@padaria.exemplo"}
	f.store.endpoints[hex.EncodeToString(HashEndpointToken(endpointToken))] = endpoint{"simulator", f.tenantID, f.account.ID}
	f.svc = NewService(f.store, f.resolver, f.clock, Config{WebhookBaseURL: "https://api.recorvia.test/webhooks/providers/",
		Log: slog.New(slog.NewTextHandler(f.logs, nil))})
	return f
}

// addInvoice grava uma fatura emitida agora, com vencimento em 21/11/2026.
func (f *payFixture) addInvoice(t *testing.T, total int64) billing.Invoice {
	t.Helper()
	sub := uuid.New()
	inv := billing.NewDraft(uuid.New(), f.tenantID, f.customer, &sub, billing.KindSubscriptionCycle, money.BRL, nil, customers.Snapshot{Name: "Padaria Central"}, payNow)
	if err := inv.AddItem(billing.Item{ID: uuid.New(), Kind: billing.ItemSubscriptionFee, Description: "Mensalidade", Amount: brl(total)}); err != nil {
		t.Fatal(err)
	}
	if err := inv.Issue("FAT-2026-000123", payNow, billing.DueAt(payNow, 5, saoPaulo)); err != nil {
		t.Fatal(err)
	}
	inv.PopChanges()
	f.store.invoices[inv.ID] = *inv
	return *inv
}

func (f *payFixture) invoice(id uuid.UUID) billing.Invoice { return f.store.invoices[id] }

// requestCharge cria a cobrança da fatura e devolve a situação final.
func (f *payFixture) requestCharge(t *testing.T, invoiceID uuid.UUID) Charge {
	t.Helper()
	c, err := f.svc.RequestCharge(context.Background(), f.tenantID, invoiceID, f.actor)
	if err != nil {
		t.Fatalf("RequestCharge: %v", err)
	}
	if c == nil {
		t.Fatal("RequestCharge não criou cobrança")
	}
	return *c
}

func (f *payFixture) operationFor(t *testing.T, chargeID uuid.UUID) Operation {
	t.Helper()
	for _, op := range f.store.operations {
		if op.ChargeID == chargeID {
			return op
		}
	}
	t.Fatalf("operação da cobrança %s não encontrada", chargeID)
	return Operation{}
}

// storeWebhook registra um webhook recebido com o evento informado.
func (f *payFixture) storeWebhook(t *testing.T, ev WebhookEvent) uuid.UUID {
	t.Helper()
	body := "evento:" + ev.EventID
	f.verifier.events[body] = ev
	id := uuid.New()
	f.store.webhooks[id] = StoredWebhook{ID: id, TenantID: f.tenantID, ProviderAccountID: f.account.ID, Provider: "simulator",
		EventID: ev.EventID, EventType: ev.Type, ProviderChargeID: ev.ProviderChargeID, ReceivedAt: f.clock.Now(), Payload: []byte(body), Status: "received"}
	return id
}

func (f *payFixture) process(t *testing.T, webhookID uuid.UUID) uuid.UUID {
	t.Helper()
	msg := uuid.New()
	if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, webhookID, msg); err != nil {
		t.Fatalf("ProcessWebhook: %v", err)
	}
	return msg
}

func providerSnapshot(status ProviderStatus, raw string, amount, fee int64, paidAt *time.Time) *ProviderCharge {
	f := brl(fee)
	return &ProviderCharge{ProviderChargeID: "pay_1", Status: status, RawStatus: raw, Amount: brl(amount), Fee: &f, PaidAt: paidAt}
}

func ledgerNet(txs []ledger.Transaction) map[ledger.AccountCode]int64 {
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
