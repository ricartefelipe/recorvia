package customers

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

var errBoom = errors.New("falha simulada")

// memState é o estado de um tenant no armazenamento em memória.
type memState struct {
	customers   map[uuid.UUID]Customer
	contacts    map[uuid.UUID][]Contact
	addresses   map[uuid.UUID]Address
	obligations map[uuid.UUID]bool
	// pendingBilling marca clientes com faturamento ainda por acontecer;
	// pendingAskedAt é o instante recebido na última consulta.
	pendingBilling map[uuid.UUID]bool
	pendingAskedAt time.Time
	// Dados fora do cadastro removidos pela anonimização.
	providerRefs    map[uuid.UUID][]ProviderRef
	portalLinks     map[uuid.UUID]int
	notifications   map[uuid.UUID]int
	webhookPayloads map[uuid.UUID]int
	erasedAt        time.Time
	audits          []audit.Entry
	events          []outbox.Event
	updates         int
	lastFilter      ListFilter
	page            Page
}

func newMemState() *memState {
	return &memState{customers: map[uuid.UUID]Customer{}, contacts: map[uuid.UUID][]Contact{},
		addresses: map[uuid.UUID]Address{}, obligations: map[uuid.UUID]bool{}, pendingBilling: map[uuid.UUID]bool{},
		providerRefs: map[uuid.UUID][]ProviderRef{},
		portalLinks:  map[uuid.UUID]int{}, notifications: map[uuid.UUID]int{}, webhookPayloads: map[uuid.UUID]int{}}
}

func (m *memState) clone() *memState {
	c := *m
	c.customers = maps.Clone(m.customers)
	c.contacts = map[uuid.UUID][]Contact{}
	for k, v := range m.contacts {
		c.contacts[k] = slices.Clone(v)
	}
	c.addresses = maps.Clone(m.addresses)
	c.obligations = maps.Clone(m.obligations)
	c.pendingBilling = maps.Clone(m.pendingBilling)
	c.providerRefs = maps.Clone(m.providerRefs)
	c.portalLinks = maps.Clone(m.portalLinks)
	c.notifications = maps.Clone(m.notifications)
	c.webhookPayloads = maps.Clone(m.webhookPayloads)
	c.audits = slices.Clone(m.audits)
	c.events = slices.Clone(m.events)
	return &c
}

// memStore implementa Store e Tx; a transação só é confirmada quando fn
// retorna nil.
type memStore struct {
	tenants map[uuid.UUID]*memState
	fail    map[string]error
	calls   map[string]int
}

func newMemStore() *memStore {
	return &memStore{tenants: map[uuid.UUID]*memState{}, fail: map[string]error{}, calls: map[string]int{}}
}

func (s *memStore) state(id uuid.UUID) *memState {
	st, ok := s.tenants[id]
	if !ok {
		st = newMemState()
		s.tenants[id] = st
	}
	return st
}

func (s *memStore) hit(op string) error {
	s.calls[op]++
	return s.fail[op]
}

func (s *memStore) InTenant(_ context.Context, tenantID uuid.UUID, fn func(Tx) error) error {
	if err := s.hit("InTenant"); err != nil {
		return err
	}
	work := s.state(tenantID).clone()
	if err := fn(&memTx{s: s, st: work}); err != nil {
		return err
	}
	s.tenants[tenantID] = work
	return nil
}

type memTx struct {
	s  *memStore
	st *memState
}

func notFound() error { return apperr.NotFound("customer_not_found", "Cliente não encontrado.") }

func (t *memTx) Insert(_ context.Context, c Customer) error {
	if err := t.s.hit("Insert"); err != nil {
		return err
	}
	t.st.customers[c.ID] = c
	return nil
}

func (t *memTx) Get(_ context.Context, id uuid.UUID, _ bool) (Customer, error) {
	if err := t.s.hit("Get"); err != nil {
		return Customer{}, err
	}
	c, ok := t.st.customers[id]
	if !ok {
		return Customer{}, notFound()
	}
	return c, nil
}

func (t *memTx) Update(_ context.Context, c Customer) error {
	if err := t.s.hit("Update"); err != nil {
		return err
	}
	cur, ok := t.st.customers[c.ID]
	if !ok || cur.Version != c.Version {
		return apperr.PreconditionFailed("version_mismatch", "O cliente foi alterado por outra pessoa.")
	}
	c.Version++
	t.st.customers[c.ID] = c
	t.st.updates++
	return nil
}

func (t *memTx) List(_ context.Context, f ListFilter) (Page, error) {
	if err := t.s.hit("List"); err != nil {
		return Page{}, err
	}
	t.st.lastFilter = f
	return t.st.page, nil
}

func (t *memTx) Contacts(_ context.Context, customerID uuid.UUID) ([]Contact, error) {
	if err := t.s.hit("Contacts"); err != nil {
		return nil, err
	}
	return t.st.contacts[customerID], nil
}

func (t *memTx) InsertContact(_ context.Context, c Contact) error {
	if err := t.s.hit("InsertContact"); err != nil {
		return err
	}
	t.st.contacts[c.CustomerID] = append(t.st.contacts[c.CustomerID], c)
	return nil
}

func (t *memTx) UpdateContact(_ context.Context, c Contact) error {
	if err := t.s.hit("UpdateContact"); err != nil {
		return err
	}
	list := t.st.contacts[c.CustomerID]
	i := slices.IndexFunc(list, func(x Contact) bool { return x.ID == c.ID })
	if i < 0 {
		return apperr.NotFound("contact_not_found", "Contato não encontrado.")
	}
	list[i] = c
	return nil
}

func (t *memTx) DeleteContact(_ context.Context, customerID, contactID uuid.UUID) error {
	if err := t.s.hit("DeleteContact"); err != nil {
		return err
	}
	list := t.st.contacts[customerID]
	i := slices.IndexFunc(list, func(c Contact) bool { return c.ID == contactID })
	if i < 0 {
		return apperr.NotFound("contact_not_found", "Contato não encontrado.")
	}
	t.st.contacts[customerID] = slices.Delete(list, i, i+1)
	return nil
}

func (t *memTx) Address(_ context.Context, customerID uuid.UUID) (*Address, error) {
	if err := t.s.hit("Address"); err != nil {
		return nil, err
	}
	a, ok := t.st.addresses[customerID]
	if !ok {
		return nil, nil
	}
	return &a, nil
}

func (t *memTx) UpsertAddress(_ context.Context, customerID uuid.UUID, a Address) error {
	if err := t.s.hit("UpsertAddress"); err != nil {
		return err
	}
	t.st.addresses[customerID] = a
	return nil
}

func (t *memTx) HasOpenObligations(_ context.Context, customerID uuid.UUID) (bool, error) {
	if err := t.s.hit("HasOpenObligations"); err != nil {
		return false, err
	}
	return t.st.obligations[customerID], nil
}

func (t *memTx) HasPendingBilling(_ context.Context, customerID uuid.UUID, now time.Time) (bool, error) {
	if err := t.s.hit("HasPendingBilling"); err != nil {
		return false, err
	}
	t.st.pendingAskedAt = now
	return t.st.pendingBilling[customerID], nil
}

// touchedAt é o instante que o banco grava em updated_at a cada alteração
// (gatilho customers_touch), diferente do relógio do caso de uso.
var touchedAt = testNow.Add(90 * time.Second)

func (t *memTx) Anonymize(_ context.Context, c Customer) (Customer, error) {
	if err := t.s.hit("Anonymize"); err != nil {
		return Customer{}, err
	}
	cur, ok := t.st.customers[c.ID]
	if !ok {
		return Customer{}, notFound()
	}
	if cur.Anonymized() {
		return Customer{}, apperr.Conflict("customer_anonymized", "O cliente já foi anonimizado.")
	}
	c.Version, c.UpdatedAt = cur.Version+1, touchedAt
	t.st.customers[c.ID] = c
	return c, nil
}

// AnonymizationProviderRefs relê os identificadores da auditoria, como o
// adaptador real.
func (t *memTx) AnonymizationProviderRefs(_ context.Context, customerID uuid.UUID) ([]ProviderRef, error) {
	if err := t.s.hit("AnonymizationProviderRefs"); err != nil {
		return nil, err
	}
	for _, a := range slices.Backward(t.st.audits) {
		if a.Action == "customer.anonymized" && a.ResourceID == customerID.String() {
			refs, _ := a.Details[AuditProviderCustomers].([]ProviderRef)
			return slices.Clone(refs), nil
		}
	}
	return nil, nil
}

func (t *memTx) ErasePersonalData(_ context.Context, customerID uuid.UUID, at time.Time) (Erasure, error) {
	if err := t.s.hit("ErasePersonalData"); err != nil {
		return Erasure{}, err
	}
	_, hadAddress := t.st.addresses[customerID]
	out := Erasure{Contacts: len(t.st.contacts[customerID]), Address: hadAddress, ProviderRefs: t.st.providerRefs[customerID],
		PortalLinksRevoked: t.st.portalLinks[customerID], Notifications: t.st.notifications[customerID],
		ProviderWebhookPayloads: t.st.webhookPayloads[customerID]}
	delete(t.st.contacts, customerID)
	delete(t.st.addresses, customerID)
	delete(t.st.providerRefs, customerID)
	delete(t.st.portalLinks, customerID)
	delete(t.st.notifications, customerID)
	delete(t.st.webhookPayloads, customerID)
	t.st.erasedAt = at
	return out, nil
}

func (t *memTx) Audit(_ context.Context, e audit.Entry) error {
	if err := t.s.hit("Audit"); err != nil {
		return err
	}
	t.st.audits = append(t.st.audits, e)
	return nil
}

func (t *memTx) Emit(_ context.Context, e outbox.Event) error {
	if err := t.s.hit("Emit"); err != nil {
		return err
	}
	t.st.events = append(t.st.events, e)
	return nil
}
