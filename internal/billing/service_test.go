package billing

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

type reversal struct {
	key, description string
	at               time.Time
}

type creditReturn struct {
	customerID, invoiceID uuid.UUID
	amount                int64
}

type grantCall struct {
	customerID  uuid.UUID
	by          *uuid.UUID
	amount      money.Money
	reason, key string
}

// same compara a chamada registrada com a esperada, inclusive a autoria
// nula das concessões feitas por credencial de API.
func (g grantCall) same(o grantCall) bool {
	if (g.by == nil) != (o.by == nil) || (g.by != nil && *g.by != *o.by) {
		return false
	}
	g.by, o.by = nil, nil
	return g == o
}

type statusWrite struct {
	invoiceID uuid.UUID
	actor     audit.Actor
	changes   []StatusChange
}

// invoiceStore implementa Store e Tx do serviço de faturas em memória, com
// transações atômicas.
type invoiceStore struct {
	settings     TenantSettings
	invoices     map[uuid.UUID]Invoice
	history      map[uuid.UUID][]HistoryEntry
	statusWrites []statusWrite
	reversals    []reversal
	posted       []ledger.Transaction
	returned     []creditReturn
	grants       map[string]uuid.UUID
	grantCalls   []grantCall
	audits       []audit.Entry
	events       []outbox.Event
	errs         map[string]error
	listFilter   ListFilter
	txCount      int
}

func newInvoiceStore() *invoiceStore {
	return &invoiceStore{settings: TenantSettings{Location: saoPaulo, InvoicePrefix: "FAT", PaymentTermsDays: 5},
		invoices: map[uuid.UUID]Invoice{}, history: map[uuid.UUID][]HistoryEntry{}, grants: map[string]uuid.UUID{}, errs: map[string]error{}}
}

func (s *invoiceStore) InTenant(_ context.Context, _ uuid.UUID, fn func(Tx) error) error {
	s.txCount++
	invoices, grants := maps.Clone(s.invoices), maps.Clone(s.grants)
	sw, rv, po, rt, au, ev := len(s.statusWrites), len(s.reversals), len(s.posted), len(s.returned), len(s.audits), len(s.events)
	if err := fn(s); err != nil {
		s.invoices, s.grants = invoices, grants
		s.statusWrites, s.reversals, s.posted, s.returned = s.statusWrites[:sw], s.reversals[:rv], s.posted[:po], s.returned[:rt]
		s.audits, s.events = s.audits[:au], s.events[:ev]
		return err
	}
	return nil
}

func (s *invoiceStore) fail(m string) error { return s.errs[m] }

func (s *invoiceStore) TenantSettings(context.Context) (TenantSettings, error) {
	return s.settings, s.fail("TenantSettings")
}

func (s *invoiceStore) Invoice(_ context.Context, id uuid.UUID, _ bool) (Invoice, error) {
	if err := s.fail("Invoice"); err != nil {
		return Invoice{}, err
	}
	inv, ok := s.invoices[id]
	if !ok {
		return Invoice{}, apperr.NotFound("invoice_not_found", "Fatura não encontrada.")
	}
	return inv, nil
}

func (s *invoiceStore) UpdateInvoiceProgress(_ context.Context, inv *Invoice) error {
	if err := s.fail("UpdateInvoiceProgress"); err != nil {
		return err
	}
	cp := *inv
	cp.Version++
	cp.PopChanges()
	s.invoices[inv.ID] = cp
	return nil
}

func (s *invoiceStore) List(_ context.Context, f ListFilter) (Page, error) {
	s.listFilter = f
	return Page{NextCursor: "proximo"}, s.fail("List")
}

func (s *invoiceStore) StatusHistory(_ context.Context, id uuid.UUID) ([]HistoryEntry, error) {
	return s.history[id], s.fail("StatusHistory")
}

func (s *invoiceStore) InsertStatusHistory(_ context.Context, id uuid.UUID, actor audit.Actor, changes []StatusChange) error {
	if err := s.fail("InsertStatusHistory"); err != nil {
		return err
	}
	s.statusWrites = append(s.statusWrites, statusWrite{id, actor, changes})
	return nil
}

func (s *invoiceStore) ReverseLedger(_ context.Context, key, desc string, at time.Time) error {
	if err := s.fail("ReverseLedger"); err != nil {
		return err
	}
	s.reversals = append(s.reversals, reversal{key, desc, at})
	return nil
}

func (s *invoiceStore) PostLedger(_ context.Context, t ledger.Transaction) error {
	if err := s.fail("PostLedger"); err != nil {
		return err
	}
	if err := t.Validate(); err != nil {
		return err
	}
	s.posted = append(s.posted, t)
	return nil
}

func (s *invoiceStore) ReturnCustomerCredit(_ context.Context, customerID, invoiceID uuid.UUID, amount int64, _ time.Time) error {
	if err := s.fail("ReturnCustomerCredit"); err != nil {
		return err
	}
	s.returned = append(s.returned, creditReturn{customerID, invoiceID, amount})
	return nil
}

func (s *invoiceStore) GrantCustomerCredit(_ context.Context, customerID uuid.UUID, amount money.Money, reason string, by *uuid.UUID, key string) (uuid.UUID, bool, error) {
	s.grantCalls = append(s.grantCalls, grantCall{customerID, by, amount, reason, key})
	if err := s.fail("GrantCustomerCredit"); err != nil {
		return uuid.Nil, false, err
	}
	if id, ok := s.grants[key]; ok && key != "" {
		return id, false, nil
	}
	id := uuid.New()
	s.grants[key] = id
	return id, true, nil
}

func (s *invoiceStore) Audit(_ context.Context, e audit.Entry) error {
	if err := s.fail("Audit"); err != nil {
		return err
	}
	s.audits = append(s.audits, e)
	return nil
}

func (s *invoiceStore) Emit(_ context.Context, e outbox.Event) error {
	if err := s.fail("Emit"); err != nil {
		return err
	}
	s.events = append(s.events, e)
	return nil
}

type invoiceFixture struct {
	store    *invoiceStore
	svc      *Service
	clock    *clock.Manual
	tenantID uuid.UUID
	actor    audit.Actor
	userID   uuid.UUID
}

func newInvoiceFixture(t *testing.T) *invoiceFixture {
	t.Helper()
	f := &invoiceFixture{store: newInvoiceStore(), clock: clock.NewManual(invoiceNow.Add(24 * time.Hour)), tenantID: uuid.New(), userID: uuid.New()}
	f.actor = audit.User(f.userID, "Ana Souza")
	f.svc = NewService(f.store, NewEngine(newCycleFake(), f.clock), f.clock)
	return f
}

// addIssued grava uma fatura emitida de 100,00 com o crédito informado.
func (f *invoiceFixture) addIssued(t *testing.T, credit int64) Invoice {
	t.Helper()
	inv := draftWith(t, 10000)
	inv.TenantID = f.tenantID
	if _, err := inv.ApplyCredit(brl(credit)); err != nil {
		t.Fatal(err)
	}
	if err := inv.Issue("FAT-2026-000123", invoiceNow, DueAt(invoiceNow, 5, saoPaulo)); err != nil {
		t.Fatal(err)
	}
	inv.PopChanges()
	f.store.invoices[inv.ID] = *inv
	return *inv
}

func (f *invoiceFixture) assertUntouched(t *testing.T, inv Invoice) {
	t.Helper()
	got := f.store.invoices[inv.ID]
	if got.Status != inv.Status || got.Version != inv.Version {
		t.Fatalf("fatura alterada: %s v%d", got.Status, got.Version)
	}
	if len(f.store.statusWrites) != 0 || len(f.store.reversals) != 0 || len(f.store.posted) != 0 || len(f.store.returned) != 0 ||
		len(f.store.audits) != 0 || len(f.store.events) != 0 {
		t.Fatal("nenhum efeito deveria ser gravado")
	}
}

func TestServiceVoidReversesLedgerAndReturnsCredit(t *testing.T) {
	f := newInvoiceFixture(t)
	inv := f.addIssued(t, 3000)

	got, err := f.svc.Void(context.Background(), f.tenantID, f.actor, inv.ID, "emitida em duplicidade", inv.Version)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Void || got.Version != inv.Version+1 || f.store.invoices[inv.ID].Status != Void {
		t.Fatalf("cancelamento: %s v%d", got.Status, got.Version)
	}
	now := f.clock.Now()
	if len(f.store.reversals) != 1 || f.store.reversals[0] != (reversal{"invoice.issued:" + inv.ID.String(), "Cancelamento da fatura FAT-2026-000123", now}) {
		t.Fatalf("estorno pela chave do lançamento de emissão: %+v", f.store.reversals)
	}
	if len(f.store.returned) != 1 || f.store.returned[0] != (creditReturn{inv.CustomerID, inv.ID, 3000}) {
		t.Fatalf("crédito aplicado deve ser devolvido: %+v", f.store.returned)
	}
	if len(f.store.events) != 1 || f.store.events[0].Type != outbox.EventInvoiceVoided || f.store.events[0].AggregateID != inv.ID {
		t.Fatalf("evento: %+v", f.store.events)
	}
	if len(f.store.audits) != 1 || f.store.audits[0].Action != "invoice.voided" || f.store.audits[0].Actor != f.actor {
		t.Fatalf("auditoria: %+v", f.store.audits)
	}
	if len(f.store.statusWrites) != 1 || f.store.statusWrites[0].changes[0].To != Void || f.store.statusWrites[0].actor != f.actor {
		t.Fatalf("histórico: %+v", f.store.statusWrites)
	}
}

func TestServiceVoidWithoutCreditDoesNotReturnAnything(t *testing.T) {
	f := newInvoiceFixture(t)
	inv := f.addIssued(t, 0)
	if _, err := f.svc.Void(context.Background(), f.tenantID, f.actor, inv.ID, "erro de cadastro", 0); err != nil {
		t.Fatal(err)
	}
	if len(f.store.returned) != 0 || len(f.store.reversals) != 1 {
		t.Fatalf("sem crédito aplicado nada é devolvido: %+v", f.store.returned)
	}
}

func TestServiceVoidRules(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*invoiceFixture, Invoice) Invoice
		run   func(*invoiceFixture, Invoice) error
		kind  apperr.Kind
		code  string
	}{
		{"motivo curto", nil, func(f *invoiceFixture, inv Invoice) error {
			_, err := f.svc.Void(context.Background(), f.tenantID, f.actor, inv.ID, "x", 0)
			return err
		}, apperr.KindInvalid, "invalid_reason"},
		{"versão divergente", nil, func(f *invoiceFixture, inv Invoice) error {
			_, err := f.svc.Void(context.Background(), f.tenantID, f.actor, inv.ID, "erro de cadastro", inv.Version+1)
			return err
		}, apperr.KindPreconditionFailed, "version_mismatch"},
		{"fatura paga", func(f *invoiceFixture, inv Invoice) Invoice {
			inv.Status = Paid
			f.store.invoices[inv.ID] = inv
			return inv
		}, func(f *invoiceFixture, inv Invoice) error {
			_, err := f.svc.Void(context.Background(), f.tenantID, f.actor, inv.ID, "erro de cadastro", 0)
			return err
		}, apperr.KindConflict, "invoice_not_voidable"},
		{"fatura inexistente", nil, func(f *invoiceFixture, _ Invoice) error {
			_, err := f.svc.Void(context.Background(), f.tenantID, f.actor, uuid.New(), "erro de cadastro", 0)
			return err
		}, apperr.KindNotFound, "invoice_not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newInvoiceFixture(t)
			inv := f.addIssued(t, 1000)
			if c.setup != nil {
				inv = c.setup(f, inv)
			}
			wantAppErr(t, c.run(f, inv), c.kind, c.code)
			f.assertUntouched(t, inv)
		})
	}
}

func TestServiceVoidRollsBackOnFailure(t *testing.T) {
	for _, m := range []string{"ReverseLedger", "ReturnCustomerCredit", "Emit", "Audit", "UpdateInvoiceProgress", "InsertStatusHistory"} {
		t.Run(m, func(t *testing.T) {
			f := newInvoiceFixture(t)
			inv := f.addIssued(t, 1000)
			boom := errors.New("falha em " + m)
			f.store.errs[m] = boom
			if _, err := f.svc.Void(context.Background(), f.tenantID, f.actor, inv.ID, "erro de cadastro", 0); !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			f.assertUntouched(t, inv)
		})
	}
}

func TestServiceMarkUncollectible(t *testing.T) {
	f := newInvoiceFixture(t)
	inv := f.addIssued(t, 2000)

	got, err := f.svc.MarkUncollectible(context.Background(), f.tenantID, f.actor, inv.ID, "cliente não localizado", inv.Version)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Uncollectible || got.AmountWrittenOff.Amount() != 8000 || !got.AmountDue.IsZero() {
		t.Fatalf("baixa: %+v", got)
	}
	if len(f.store.posted) != 1 || f.store.posted[0].Kind != "invoice.uncollectible" || f.store.posted[0].Entries[0].Amount.Amount() != 8000 {
		t.Fatalf("lançamento de perda: %+v", f.store.posted)
	}
	if len(f.store.events) != 1 || f.store.events[0].Type != outbox.EventInvoiceUncollectible ||
		f.store.events[0].Payload.(map[string]any)["written_off"] != int64(8000) {
		t.Fatalf("evento: %+v", f.store.events)
	}
	if len(f.store.audits) != 1 || f.store.audits[0].Action != "invoice.marked_uncollectible" {
		t.Fatalf("auditoria: %+v", f.store.audits)
	}
}

func TestServiceMarkUncollectibleRules(t *testing.T) {
	for _, reason := range []string{"", "ab", strings.Repeat("y", 501)} {
		f := newInvoiceFixture(t)
		inv := f.addIssued(t, 0)
		_, err := f.svc.MarkUncollectible(context.Background(), f.tenantID, f.actor, inv.ID, reason, 0)
		wantAppErr(t, err, apperr.KindInvalid, "invalid_reason")
		if f.store.txCount != 0 {
			t.Fatal("motivo inválido é rejeitado antes de abrir transação")
		}
	}

	f := newInvoiceFixture(t)
	inv := f.addIssued(t, 10000) // coberta por crédito: nasce paga
	_, err := f.svc.MarkUncollectible(context.Background(), f.tenantID, f.actor, inv.ID, "cliente não localizado", 0)
	wantAppErr(t, err, apperr.KindConflict, "invoice_not_open")
	f.assertUntouched(t, inv)

	for _, m := range []string{"PostLedger", "Emit", "Audit"} {
		f := newInvoiceFixture(t)
		inv := f.addIssued(t, 0)
		boom := errors.New("falha em " + m)
		f.store.errs[m] = boom
		if _, err := f.svc.MarkUncollectible(context.Background(), f.tenantID, f.actor, inv.ID, "cliente não localizado", 0); !errors.Is(err, boom) {
			t.Fatalf("%s: esperava %v, obtive %v", m, boom, err)
		}
		f.assertUntouched(t, inv)
	}
}

func TestServiceGrantCredit(t *testing.T) {
	f := newInvoiceFixture(t)
	customer := uuid.New()
	ctx := context.Background()

	if err := f.svc.GrantCredit(ctx, f.tenantID, f.actor, &f.userID, customer, brl(2500), "cortesia por instabilidade", "user:1|POST|chave-1"); err != nil {
		t.Fatal(err)
	}
	if len(f.store.grantCalls) != 1 || !f.store.grantCalls[0].same(grantCall{customer, &f.userID, brl(2500), "cortesia por instabilidade", "user:1|POST|chave-1"}) {
		t.Fatalf("concessão: %+v", f.store.grantCalls)
	}
	creditID := f.store.grants["user:1|POST|chave-1"]
	if len(f.store.posted) != 1 {
		t.Fatalf("lançamentos: %+v", f.store.posted)
	}
	tx := f.store.posted[0]
	if tx.Kind != "customer_credit.granted" || tx.ReferenceID != creditID || tx.Entries[0].Amount.Amount() != 2500 {
		t.Fatalf("lançamento da concessão: %+v", tx)
	}
	if len(f.store.audits) != 1 || f.store.audits[0].Action != "customer.credit_granted" || f.store.audits[0].ResourceID != customer.String() ||
		f.store.audits[0].Details["credit_id"] != creditID {
		t.Fatalf("auditoria: %+v", f.store.audits)
	}

	// Mesmo pedido repetido (mesma chave): não concede nem lança de novo.
	if err := f.svc.GrantCredit(ctx, f.tenantID, f.actor, &f.userID, customer, brl(2500), "cortesia por instabilidade", "user:1|POST|chave-1"); err != nil {
		t.Fatalf("repetição deve ser aceita sem erro: %v", err)
	}
	if len(f.store.posted) != 1 || len(f.store.audits) != 1 || len(f.store.grants) != 1 {
		t.Fatalf("repetição não pode duplicar efeitos: lançamentos=%d auditoria=%d", len(f.store.posted), len(f.store.audits))
	}

	// Outra chave é outro pedido.
	if err := f.svc.GrantCredit(ctx, f.tenantID, f.actor, &f.userID, customer, brl(100), "ajuste comercial", "user:1|POST|chave-2"); err != nil {
		t.Fatal(err)
	}
	if len(f.store.posted) != 2 {
		t.Fatal("nova chave gera nova concessão")
	}
}

// Concessão feita por credencial de API: a autoria chega nula ao
// armazenamento e o ator da credencial fica na auditoria.
func TestServiceGrantCreditByAPICredential(t *testing.T) {
	f := newInvoiceFixture(t)
	customer := uuid.New()
	actor := audit.APICredential(uuid.New(), "ERP")
	if err := f.svc.GrantCredit(context.Background(), f.tenantID, actor, nil, customer, brl(700), "crédito via integração", "k"); err != nil {
		t.Fatal(err)
	}
	if len(f.store.grantCalls) != 1 || !f.store.grantCalls[0].same(grantCall{customer, nil, brl(700), "crédito via integração", "k"}) {
		t.Fatalf("concessão: %+v", f.store.grantCalls)
	}
	if len(f.store.audits) != 1 || f.store.audits[0].Actor != actor {
		t.Fatalf("auditoria pelo ator da credencial: %+v", f.store.audits)
	}
}

func TestServiceGrantCreditValidation(t *testing.T) {
	cases := []struct {
		name   string
		amount money.Money
		reason string
		code   string
		field  string
	}{
		{"valor zero", brl(0), "cortesia", "invalid_amount", "amount"},
		{"valor negativo", brl(-100), "cortesia", "invalid_amount", "amount"},
		{"valor acima do limite", brl(money.MaxAmount + 1), "cortesia", "invalid_amount", "amount"},
		{"motivo curto", brl(100), "ok", "invalid_reason", "reason"},
		{"motivo longo", brl(100), strings.Repeat("m", 501), "invalid_reason", "reason"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newInvoiceFixture(t)
			err := f.svc.GrantCredit(context.Background(), f.tenantID, f.actor, &f.userID, uuid.New(), c.amount, c.reason, "k")
			wantAppErr(t, err, apperr.KindInvalid, c.code)
			if e, _ := apperr.As(err); len(e.Fields) != 1 || e.Fields[0].Field != c.field {
				t.Fatalf("campo: %+v", e.Fields)
			}
			if f.store.txCount != 0 {
				t.Fatal("validação ocorre antes da transação")
			}
		})
	}
}

func TestServiceGrantCreditFailures(t *testing.T) {
	for _, m := range []string{"GrantCustomerCredit", "PostLedger", "Audit"} {
		t.Run(m, func(t *testing.T) {
			f := newInvoiceFixture(t)
			boom := errors.New("falha em " + m)
			f.store.errs[m] = boom
			if err := f.svc.GrantCredit(context.Background(), f.tenantID, f.actor, &f.userID, uuid.New(), brl(100), "cortesia", "k"); !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			if len(f.store.grants) != 0 || len(f.store.posted) != 0 || len(f.store.audits) != 0 {
				t.Fatal("falha desfaz a concessão")
			}
		})
	}
}

func TestServiceGetAndLocation(t *testing.T) {
	f := newInvoiceFixture(t)
	inv := f.addIssued(t, 0)
	f.store.history[inv.ID] = []HistoryEntry{{ID: uuid.New(), ToStatus: Open, Reason: "issued"}}
	d, err := f.svc.Get(context.Background(), f.tenantID, inv.ID)
	if err != nil || d.ID != inv.ID || len(d.History) != 1 || d.Location != saoPaulo {
		t.Fatalf("Get: %+v %v", d, err)
	}
	loc, err := f.svc.Location(context.Background(), f.tenantID)
	if err != nil || loc != saoPaulo {
		t.Fatalf("Location: %v %v", loc, err)
	}
	if _, err := f.svc.Get(context.Background(), f.tenantID, uuid.New()); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("inexistente: %v", err)
	}
	for _, m := range []string{"StatusHistory", "TenantSettings"} {
		f.store.errs = map[string]error{m: errors.New("falha em " + m)}
		if _, err := f.svc.Get(context.Background(), f.tenantID, inv.ID); err == nil {
			t.Fatalf("%s: erro deve ser propagado", m)
		}
	}
	if _, err := f.svc.Location(context.Background(), f.tenantID); err == nil {
		t.Fatal("falha ao ler configurações deve ser propagada")
	}
}

func TestServiceListFilters(t *testing.T) {
	f := newInvoiceFixture(t)
	from, to := invoiceNow, invoiceNow.AddDate(0, 1, 0)
	page, err := f.svc.List(context.Background(), f.tenantID, ListFilter{Status: Open, IssuedFrom: &from, IssuedTo: &to, Overdue: true, Limit: 50})
	if err != nil || page.NextCursor != "proximo" {
		t.Fatalf("List: %v", err)
	}
	if !f.store.listFilter.Now.Equal(f.clock.Now()) || !f.store.listFilter.Overdue || f.store.listFilter.Limit != 50 {
		t.Fatalf("o serviço define o instante de referência do filtro de vencidas: %+v", f.store.listFilter)
	}

	_, err = f.svc.List(context.Background(), f.tenantID, ListFilter{Status: "atrasada"})
	wantAppErr(t, err, apperr.KindInvalid, "invalid_filter")
	_, err = f.svc.List(context.Background(), f.tenantID, ListFilter{IssuedFrom: &to, IssuedTo: &from})
	wantAppErr(t, err, apperr.KindInvalid, "invalid_filter")
	if e, _ := apperr.As(err); e.Fields[0].Field != "issued_to" {
		t.Fatalf("campo do intervalo inválido: %+v", e.Fields)
	}
	same := from
	if _, err := f.svc.List(context.Background(), f.tenantID, ListFilter{IssuedFrom: &from, IssuedTo: &same}); err != nil {
		t.Fatalf("intervalo de um instante é válido: %v", err)
	}
	for _, st := range allInvoiceStatuses {
		if _, err := f.svc.List(context.Background(), f.tenantID, ListFilter{Status: st}); err != nil {
			t.Fatalf("situação %s é filtro válido: %v", st, err)
		}
	}
}

func TestServiceRunNowUsesEngine(t *testing.T) {
	f := newInvoiceFixture(t)
	cf := newCycleFake()
	plan := fixturePlan()
	price := fixturePrice(plan, catalog.Flat)
	cf.plans[plan.ID], cf.prices[price.ID] = plan, price
	f.svc = NewService(f.store, NewEngine(cf, f.clock), f.clock)
	ef := &engineFixture{store: cf, clock: f.clock, tenantID: f.tenantID, plan: plan, price: price}
	ef.addSub(t, f.clock.Now(), nil)

	res, err := f.svc.RunNow(context.Background(), f.tenantID, f.actor)
	if err != nil || res.Invoices != 1 {
		t.Fatalf("RunNow: %+v %v", res, err)
	}
	if !slices.ContainsFunc(cf.audits, func(a audit.Entry) bool { return a.Actor == f.actor }) {
		t.Fatal("execução manual registra o usuário como ator")
	}
}
