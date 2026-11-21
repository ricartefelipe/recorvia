package portal

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

var errBoom = errors.New("falha simulada")

// tenantData é o estado de um tenant no armazenamento em memória.
type tenantData struct {
	tenant    *tenancy.Tenant
	customers map[uuid.UUID]customers.Customer
	contacts  map[uuid.UUID][]customers.Contact
	links     map[uuid.UUID]Link
	// invoices guarda as faturas; a listagem as ordena pela emissão e pelo
	// identificador, decrescentes, como o armazenamento real.
	invoices []billing.Invoice
	// creators guarda o nome de quem gerou cada link (resolvido por junção
	// no armazenamento real).
	creators  map[uuid.UUID]string
	checkouts map[uuid.UUID]string
	audits    []audit.Entry
}

func newTenantData() *tenantData {
	return &tenantData{customers: map[uuid.UUID]customers.Customer{}, contacts: map[uuid.UUID][]customers.Contact{}, links: map[uuid.UUID]Link{},
		checkouts: map[uuid.UUID]string{}, creators: map[uuid.UUID]string{}}
}

func (d *tenantData) clone() *tenantData {
	c := *d
	if d.tenant != nil {
		t := *d.tenant
		c.tenant = &t
	}
	c.customers = maps.Clone(d.customers)
	c.contacts = maps.Clone(d.contacts)
	c.links = maps.Clone(d.links)
	c.invoices = slices.Clone(d.invoices)
	c.checkouts = maps.Clone(d.checkouts)
	c.creators = maps.Clone(d.creators)
	c.contacts = make(map[uuid.UUID][]customers.Contact, len(d.contacts))
	for k, v := range d.contacts {
		c.contacts[k] = slices.Clone(v)
	}
	c.audits = slices.Clone(d.audits)
	return &c
}

// fakeStore implementa Store e Tx em memória. Cada transação trabalha sobre
// uma cópia do estado do tenant e só a confirma quando fn retorna nil;
// commitErr simula falha no commit depois de fn.
type fakeStore struct {
	tenants   map[uuid.UUID]*tenantData
	fail      map[string]error
	calls     map[string]int
	commitErr error
	// leaky desliga o filtro de cliente e situação das faturas e o filtro de
	// cliente dos links (que passam a trazer o hash do token), simulando um
	// armazenamento que devolve dados além do permitido.
	leaky bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{tenants: map[uuid.UUID]*tenantData{}, fail: map[string]error{}, calls: map[string]int{}}
}

func (s *fakeStore) hit(op string) error {
	s.calls[op]++
	return s.fail[op]
}

func (s *fakeStore) InTenant(_ context.Context, tenantID uuid.UUID, fn func(Tx) error) error {
	if err := s.hit("InTenant"); err != nil {
		return err
	}
	d, ok := s.tenants[tenantID]
	if !ok {
		d = newTenantData()
	}
	work := d.clone()
	if err := fn(&fakeTx{store: s, tenantID: tenantID, d: work}); err != nil {
		return err
	}
	if s.commitErr != nil {
		return s.commitErr
	}
	s.tenants[tenantID] = work
	return nil
}

func (s *fakeStore) FindLink(_ context.Context, tokenHash []byte) (uuid.UUID, uuid.UUID, error) {
	if err := s.hit("FindLink"); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	for tenantID, d := range s.tenants {
		for _, l := range d.links {
			if bytes.Equal(l.TokenHash, tokenHash) {
				return tenantID, l.ID, nil
			}
		}
	}
	return uuid.Nil, uuid.Nil, apperr.NotFound("portal_link_not_found", "Link não encontrado.")
}

type fakeTx struct {
	store    *fakeStore
	tenantID uuid.UUID
	d        *tenantData
}

func (tx *fakeTx) Tenant(context.Context) (tenancy.Tenant, error) {
	if err := tx.store.hit("Tenant"); err != nil {
		return tenancy.Tenant{}, err
	}
	if tx.d.tenant == nil {
		return tenancy.Tenant{}, apperr.NotFound("organization_not_found", "Organização não encontrada.")
	}
	return *tx.d.tenant, nil
}

func (tx *fakeTx) Customer(_ context.Context, id uuid.UUID, lock bool) (customers.Customer, error) {
	op := "Customer"
	if lock {
		op = "LockCustomer"
	}
	if err := tx.store.hit(op); err != nil {
		return customers.Customer{}, err
	}
	c, ok := tx.d.customers[id]
	if !ok {
		return customers.Customer{}, apperr.NotFound("customer_not_found", "Cliente não encontrado.")
	}
	return c, nil
}

func (tx *fakeTx) Contacts(_ context.Context, customerID uuid.UUID) ([]customers.Contact, error) {
	return slices.Clone(tx.d.contacts[customerID]), tx.store.hit("Contacts")
}

func (tx *fakeTx) InsertLink(_ context.Context, l Link) error {
	if err := tx.store.hit("InsertLink"); err != nil {
		return err
	}
	if l.TenantID != tx.tenantID {
		return errors.New("link de outro tenant")
	}
	for _, other := range tx.d.links {
		if other.CustomerID == l.CustomerID && other.RevokedAt == nil {
			return errors.New("violação de unicidade: link ativo existente")
		}
	}
	tx.d.links[l.ID] = l
	return nil
}

func (tx *fakeTx) RevokeActiveLinks(_ context.Context, customerID uuid.UUID, at time.Time, reason string) ([]uuid.UUID, error) {
	if err := tx.store.hit("RevokeActiveLinks"); err != nil {
		return nil, err
	}
	var out []uuid.UUID
	for id, l := range tx.d.links {
		if l.CustomerID == customerID && l.RevokedAt == nil {
			l.RevokedAt, l.RevokeReason = &at, reason
			tx.d.links[id] = l
			out = append(out, id)
		}
	}
	return out, nil
}

func (tx *fakeTx) RevokeLink(_ context.Context, customerID, id uuid.UUID, at time.Time, reason string) error {
	if err := tx.store.hit("RevokeLink"); err != nil {
		return err
	}
	l, ok := tx.d.links[id]
	if !ok || l.CustomerID != customerID || l.RevokedAt != nil {
		return apperr.Conflict("portal_link_unavailable", "O link já foi revogado ou não pertence a este cliente.")
	}
	l.RevokedAt, l.RevokeReason = &at, reason
	tx.d.links[id] = l
	return nil
}

// before informa se (at, id) vem depois de (otherAt, otherID) na ordem
// decrescente das listagens.
func before(at time.Time, id uuid.UUID, otherAt time.Time, otherID uuid.UUID) bool {
	if !at.Equal(otherAt) {
		return at.Before(otherAt)
	}
	return bytes.Compare(id[:], otherID[:]) < 0
}

// Links reproduz a listagem do armazenamento (cliente, ordem e posição),
// sem o hash do token; com leaky, devolve os links de todos os clientes com
// o hash.
func (tx *fakeTx) Links(_ context.Context, customerID uuid.UUID, after *Position, limit int) ([]LinkSummary, error) {
	if err := tx.store.hit("Links"); err != nil {
		return nil, err
	}
	var out []LinkSummary
	for _, l := range tx.d.links {
		if !tx.store.leaky && l.CustomerID != customerID {
			continue
		}
		if after != nil && !before(l.CreatedAt, l.ID, after.At, after.ID) {
			continue
		}
		if !tx.store.leaky {
			l.TokenHash = nil
		}
		out = append(out, LinkSummary{Link: l, CreatorName: tx.d.creators[l.ID]})
	}
	slices.SortFunc(out, func(a, b LinkSummary) int {
		if before(b.CreatedAt, b.ID, a.CreatedAt, a.ID) {
			return -1
		}
		return 1
	})
	return out[:min(len(out), limit)], nil
}

func (tx *fakeTx) Link(_ context.Context, id uuid.UUID, _ bool) (Link, error) {
	if err := tx.store.hit("Link"); err != nil {
		return Link{}, err
	}
	l, ok := tx.d.links[id]
	if !ok {
		return Link{}, apperr.NotFound("portal_link_not_found", "Link não encontrado.")
	}
	return l, nil
}

func (tx *fakeTx) MarkLinkUsed(_ context.Context, id uuid.UUID, at time.Time) error {
	if err := tx.store.hit("MarkLinkUsed"); err != nil {
		return err
	}
	l := tx.d.links[id]
	if l.UsedAt != nil || l.RevokedAt != nil || !at.Before(l.ExpiresAt) {
		return apperr.Conflict("portal_link_unavailable", "O link já foi utilizado, revogado ou expirou.")
	}
	l.UsedAt = &at
	tx.d.links[id] = l
	return nil
}

// issuedAt devolve a emissão da fatura; rascunhos (sem emissão) ficam no fim.
func issuedAt(inv billing.Invoice) time.Time {
	if inv.IssuedAt == nil {
		return time.Time{}
	}
	return *inv.IssuedAt
}

// Invoices reproduz a listagem do armazenamento (cliente, situação, ordem e
// posição), exceto com leaky, que ignora cliente e situação para exercitar a
// segunda barreira do serviço.
func (tx *fakeTx) Invoices(_ context.Context, customerID uuid.UUID, after *Position, limit int) ([]billing.Invoice, error) {
	if err := tx.store.hit("Invoices"); err != nil {
		return nil, err
	}
	var out []billing.Invoice
	for _, inv := range tx.d.invoices {
		if !tx.store.leaky && (inv.CustomerID != customerID || inv.Status == billing.Draft) {
			continue
		}
		if after != nil && !before(issuedAt(inv), inv.ID, after.At, after.ID) {
			continue
		}
		out = append(out, inv)
	}
	slices.SortStableFunc(out, func(a, b billing.Invoice) int {
		if before(issuedAt(b), b.ID, issuedAt(a), a.ID) {
			return -1
		}
		return 1
	})
	return out[:min(len(out), limit)], nil
}

func (tx *fakeTx) Invoice(_ context.Context, customerID, id uuid.UUID) (billing.Invoice, error) {
	if err := tx.store.hit("Invoice"); err != nil {
		return billing.Invoice{}, err
	}
	for _, inv := range tx.d.invoices {
		if inv.ID == id && (tx.store.leaky || (inv.CustomerID == customerID && inv.Status != billing.Draft)) {
			return inv, nil
		}
	}
	return billing.Invoice{}, apperr.NotFound("invoice_not_found", "Fatura não encontrada.")
}

func (tx *fakeTx) CheckoutURLs(_ context.Context, invoiceIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	if err := tx.store.hit("CheckoutURLs"); err != nil {
		return nil, err
	}
	out := map[uuid.UUID]string{}
	for _, id := range invoiceIDs {
		if u, ok := tx.d.checkouts[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (tx *fakeTx) SetContactPreference(_ context.Context, customerID, contactID uuid.UUID, receives bool) error {
	if err := tx.store.hit("SetContactPreference"); err != nil {
		return err
	}
	list := tx.d.contacts[customerID]
	for i := range list {
		if list[i].ID == contactID {
			list[i].ReceivesBillingEmail = receives
			return nil
		}
	}
	return apperr.NotFound("contact_not_found", "Contato não encontrado.")
}

func (tx *fakeTx) Audit(_ context.Context, e audit.Entry) error {
	if err := tx.store.hit("Audit"); err != nil {
		return err
	}
	tx.d.audits = append(tx.d.audits, e)
	return nil
}

type savedSession struct {
	s   Session
	ttl time.Duration
}

// fakeSessions implementa SessionStore em memória.
type fakeSessions struct {
	items     map[string]savedSession
	saveErr   error
	getErr    error
	deleteErr error
	saves     int
	deletes   int
	// byLink registra os links cujas sessões foram removidas.
	byLink    []uuid.UUID
	byLinkErr error
}

func newFakeSessions() *fakeSessions { return &fakeSessions{items: map[string]savedSession{}} }

func (f *fakeSessions) Save(_ context.Context, s Session, ttl time.Duration) error {
	f.saves++
	if f.saveErr != nil {
		return f.saveErr
	}
	f.items[s.ID] = savedSession{s: s, ttl: ttl}
	return nil
}

func (f *fakeSessions) Get(_ context.Context, id string) (Session, error) {
	if f.getErr != nil {
		return Session{}, f.getErr
	}
	v, ok := f.items[id]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return v.s, nil
}

func (f *fakeSessions) Delete(_ context.Context, id string) error {
	f.deletes++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.items, id)
	return nil
}

func (f *fakeSessions) DeleteByLink(_ context.Context, linkID uuid.UUID) error {
	f.byLink = append(f.byLink, linkID)
	if f.byLinkErr != nil {
		return f.byLinkErr
	}
	for id, v := range f.items {
		if v.s.LinkID == linkID {
			delete(f.items, id)
		}
	}
	return nil
}

// fakeCounter implementa FailureCounter em memória.
type fakeCounter struct {
	counts  map[string]int
	windows map[string]time.Duration
	readErr error
	recErr  error
}

func newFakeCounter() *fakeCounter {
	return &fakeCounter{counts: map[string]int{}, windows: map[string]time.Duration{}}
}

func (f *fakeCounter) Failures(_ context.Context, key string) (int, error) {
	return f.counts[key], f.readErr
}

func (f *fakeCounter) RecordFailure(_ context.Context, key string, window time.Duration) error {
	if f.recErr != nil {
		return f.recErr
	}
	f.counts[key]++
	f.windows[key] = window
	return nil
}
