package pgstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/brdoc"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// Customers implementa customers.Store.
type Customers struct{ s *Store }

// Customers retorna o adaptador de clientes.
func (s *Store) Customers() *Customers { return &Customers{s: s} }

type customersTx struct{ base }

// InTenant abre a transação do tenant.
func (c *Customers) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(customers.Tx) error) error {
	return c.s.inTenant(ctx, tenantID, func(b base) error { return fn(customersTx{b}) })
}

func customerFromRow(r dbgen.Customer) customers.Customer {
	return customers.Customer{ID: r.ID, TenantID: r.TenantID, Kind: customers.Kind(r.Kind), Name: r.Name, LegalName: deref(r.LegalName),
		DocumentType: brdoc.Kind(r.DocumentType), DocumentNumber: r.DocumentNumber, Email: deref(r.Email), Phone: deref(r.Phone),
		ExternalID: deref(r.ExternalID), Status: customers.Status(r.Status), PaymentTermsDays: intFromPtr32(r.PaymentTermsDays),
		AnonymizedAt: r.AnonymizedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Version: int(r.Version)}
}

func customerConflict(err error) error {
	switch {
	case postgres.IsUniqueViolation(err, "customers_external_id_key"):
		return apperr.Conflict("external_id_taken", "Já existe um cliente com este identificador externo.")
	case postgres.IsUniqueViolation(err, "customers_active_document_key"):
		return apperr.Conflict("document_taken", "Já existe um cliente ativo com este documento.")
	}
	return err
}

func (tx customersTx) Insert(ctx context.Context, c customers.Customer) error {
	return customerConflict(tx.q.InsertCustomer(ctx, dbgen.InsertCustomerParams{ID: c.ID, TenantID: tx.tenantID, Kind: string(c.Kind), Name: c.Name,
		LegalName: strPtr(c.LegalName), DocumentType: string(c.DocumentType), DocumentNumber: c.DocumentNumber, Email: strPtr(c.Email),
		Phone: strPtr(c.Phone), ExternalID: strPtr(c.ExternalID), Status: string(c.Status), PaymentTermsDays: intPtr32(c.PaymentTermsDays),
		CreatedAt: c.CreatedAt}))
}

func (tx customersTx) Get(ctx context.Context, id uuid.UUID, forUpdate bool) (customers.Customer, error) {
	var r dbgen.Customer
	var err error
	if forUpdate {
		r, err = tx.q.GetCustomerForUpdate(ctx, dbgen.GetCustomerForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetCustomer(ctx, dbgen.GetCustomerParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return customers.Customer{}, notFound(err, "customer_not_found", "Cliente não encontrado.")
	}
	return customerFromRow(r), nil
}

func (tx customersTx) Update(ctx context.Context, c customers.Customer) error {
	n, err := tx.q.UpdateCustomer(ctx, dbgen.UpdateCustomerParams{TenantID: tx.tenantID, ID: c.ID, Kind: string(c.Kind), Name: c.Name,
		LegalName: strPtr(c.LegalName), DocumentType: string(c.DocumentType), DocumentNumber: c.DocumentNumber, Email: strPtr(c.Email),
		Phone: strPtr(c.Phone), ExternalID: strPtr(c.ExternalID), Status: string(c.Status), PaymentTermsDays: intPtr32(c.PaymentTermsDays),
		Version: int32(c.Version)})
	if err != nil {
		return customerConflict(err)
	}
	return versionConflict(n, "O cliente foi alterado por outra pessoa.")
}

// offsetCursor codifica o deslocamento de paginação de forma opaca.
func offsetCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(offset)))
}

func parseOffsetCursor(c string) (int, error) {
	if c == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || !strings.HasPrefix(string(b), "o:") {
		return 0, apperr.Invalid("invalid_cursor", "Cursor de paginação inválido.")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(b), "o:"))
	if err != nil || n < 0 || n > 1_000_000 {
		return 0, apperr.Invalid("invalid_cursor", "Cursor de paginação inválido.")
	}
	return n, nil
}

// timeCursor codifica (instante, id) para paginação por keyset.
func timeCursor(t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func parseTimeCursor(c string) (*time.Time, *uuid.UUID, error) {
	if c == "" {
		return nil, nil, nil
	}
	invalid := apperr.Invalid("invalid_cursor", "Cursor de paginação inválido.")
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return nil, nil, invalid
	}
	ts, idStr, ok := strings.Cut(string(b), "|")
	if !ok {
		return nil, nil, invalid
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return nil, nil, invalid
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, nil, invalid
	}
	return &t, &id, nil
}

// pageLimit aplica o limite padrão (25) e máximo (100).
func pageLimit(n int) int {
	switch {
	case n <= 0:
		return 25
	case n > 100:
		return 100
	}
	return n
}

func (tx customersTx) List(ctx context.Context, f customers.ListFilter) (customers.Page, error) {
	offset, err := parseOffsetCursor(f.Cursor)
	if err != nil {
		return customers.Page{}, err
	}
	limit := pageLimit(f.Limit)
	sort := f.Sort
	if sort == "" {
		sort = "name"
	}
	var search *string
	if s := strings.TrimSpace(f.Search); s != "" {
		v := EscapeLike(strings.ToLower(s))
		search = &v
	}
	rows, err := tx.q.ListCustomers(ctx, dbgen.ListCustomersParams{TenantID: tx.tenantID, Status: strPtr(string(f.Status)), Search: search,
		Sort: sort, RowLimit: int32(limit + 1), RowOffset: int32(offset)})
	if err != nil {
		return customers.Page{}, err
	}
	p := customers.Page{}
	for i, r := range rows {
		if i == limit {
			p.NextCursor = offsetCursor(offset + limit)
			break
		}
		p.Items = append(p.Items, customerFromRow(r))
	}
	return p, nil
}

func contactFromRow(r dbgen.CustomerContact) customers.Contact {
	return customers.Contact{ID: r.ID, CustomerID: r.CustomerID, Name: r.Name, Email: r.Email, Phone: deref(r.Phone), Role: r.Role,
		ReceivesBillingEmail: r.ReceivesBillingEmail, CreatedAt: r.CreatedAt}
}

func (tx customersTx) Contacts(ctx context.Context, customerID uuid.UUID) ([]customers.Contact, error) {
	rows, err := tx.q.ListContacts(ctx, dbgen.ListContactsParams{TenantID: tx.tenantID, CustomerID: customerID})
	out := make([]customers.Contact, 0, len(rows))
	for _, r := range rows {
		out = append(out, contactFromRow(r))
	}
	return out, err
}

func (tx customersTx) InsertContact(ctx context.Context, c customers.Contact) error {
	return tx.q.InsertContact(ctx, dbgen.InsertContactParams{ID: c.ID, TenantID: tx.tenantID, CustomerID: c.CustomerID, Name: c.Name,
		Email: c.Email, Phone: strPtr(c.Phone), Role: c.Role, ReceivesBillingEmail: c.ReceivesBillingEmail, CreatedAt: c.CreatedAt})
}

func (tx customersTx) UpdateContact(ctx context.Context, c customers.Contact) error {
	n, err := tx.q.UpdateContact(ctx, dbgen.UpdateContactParams{TenantID: tx.tenantID, CustomerID: c.CustomerID, ID: c.ID, Name: c.Name,
		Email: c.Email, Phone: strPtr(c.Phone), Role: c.Role, ReceivesBillingEmail: c.ReceivesBillingEmail})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.NotFound("contact_not_found", "Contato não encontrado.")
	}
	return nil
}

func (tx customersTx) DeleteContact(ctx context.Context, customerID, contactID uuid.UUID) error {
	n, err := tx.q.DeleteContact(ctx, dbgen.DeleteContactParams{TenantID: tx.tenantID, CustomerID: customerID, ID: contactID})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.NotFound("contact_not_found", "Contato não encontrado.")
	}
	return nil
}

func (tx customersTx) Address(ctx context.Context, customerID uuid.UUID) (*customers.Address, error) {
	r, err := tx.q.GetAddress(ctx, dbgen.GetAddressParams{TenantID: tx.tenantID, CustomerID: customerID})
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &customers.Address{Street: r.Street, Number: r.Number, Complement: r.Complement, District: r.District, City: r.City,
		State: r.State, PostalCode: r.PostalCode, Country: r.Country}, nil
}

func (tx customersTx) UpsertAddress(ctx context.Context, customerID uuid.UUID, a customers.Address) error {
	return tx.q.UpsertAddress(ctx, dbgen.UpsertAddressParams{TenantID: tx.tenantID, CustomerID: customerID, Street: a.Street, Number: a.Number,
		Complement: a.Complement, District: a.District, City: a.City, State: a.State, PostalCode: a.PostalCode, Country: a.Country,
		UpdatedAt: tx.store.clock.Now()})
}

func (tx customersTx) HasOpenObligations(ctx context.Context, customerID uuid.UUID) (bool, error) {
	return tx.q.CustomerHasOpenObligations(ctx, dbgen.CustomerHasOpenObligationsParams{TenantID: tx.tenantID, CustomerID: customerID})
}

// HasPendingBilling informa se ainda há faturamento do cliente por acontecer
// (ver a consulta CustomerHasPendingBilling).
func (tx customersTx) HasPendingBilling(ctx context.Context, customerID uuid.UUID, now time.Time) (bool, error) {
	return tx.q.CustomerHasPendingBilling(ctx, dbgen.CustomerHasPendingBillingParams{TenantID: tx.tenantID, CustomerID: customerID, Now: now})
}

// Anonymize grava o cadastro anonimizado e devolve o cliente com a versão e
// o instante de atualização gravados. A linha já está bloqueada pela leitura
// do caso de uso; nenhuma linha indica anonimização concorrente.
func (tx customersTx) Anonymize(ctx context.Context, c customers.Customer) (customers.Customer, error) {
	if c.AnonymizedAt == nil {
		return customers.Customer{}, errors.New("pgstore: cliente sem instante de anonimização")
	}
	r, err := tx.q.AnonymizeCustomer(ctx, dbgen.AnonymizeCustomerParams{Name: c.Name, DocumentNumber: c.DocumentNumber,
		AnonymizedAt: c.AnonymizedAt, TenantID: tx.tenantID, ID: c.ID})
	if postgres.IsNoRows(err) {
		return customers.Customer{}, apperr.Conflict("customer_anonymized", "O cliente já foi anonimizado.")
	}
	if err != nil {
		return customers.Customer{}, err
	}
	c.Version, c.UpdatedAt = int(r.Version), r.UpdatedAt
	return c, nil
}

// AnonymizationProviderRefs relê os identificadores do cliente nos
// provedores registrados na auditoria da anonimização.
func (tx customersTx) AnonymizationProviderRefs(ctx context.Context, customerID uuid.UUID) ([]customers.ProviderRef, error) {
	resource := customerID.String()
	raw, err := tx.q.CustomerAnonymizationDetails(ctx, dbgen.CustomerAnonymizationDetailsParams{TenantID: tx.tenantID, ResourceID: &resource})
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var details map[string]json.RawMessage
	if err := json.Unmarshal(raw, &details); err != nil {
		return nil, fmt.Errorf("pgstore: detalhes da anonimização do cliente %s: %w", customerID, err)
	}
	var refs []customers.ProviderRef
	if v, ok := details[customers.AuditProviderCustomers]; ok {
		if err := json.Unmarshal(v, &refs); err != nil {
			return nil, fmt.Errorf("pgstore: identificadores nos provedores do cliente %s: %w", customerID, err)
		}
	}
	return refs, nil
}

// ErasePersonalData remove os dados pessoais guardados fora do cadastro do
// cliente (já anonimizado na mesma transação): contatos, endereço,
// identificadores nos provedores e notificações; revoga os links do portal e
// esvazia o corpo dos webhooks recebidos das cobranças do cliente.
func (tx customersTx) ErasePersonalData(ctx context.Context, customerID uuid.UUID, at time.Time) (customers.Erasure, error) {
	var out customers.Erasure
	contacts, err := tx.q.DeleteCustomerContacts(ctx, dbgen.DeleteCustomerContactsParams{TenantID: tx.tenantID, CustomerID: customerID})
	if err != nil {
		return out, err
	}
	address, err := tx.q.DeleteCustomerAddress(ctx, dbgen.DeleteCustomerAddressParams{TenantID: tx.tenantID, CustomerID: customerID})
	if err != nil {
		return out, err
	}
	refs, err := tx.q.DeleteCustomerProviderRefs(ctx, dbgen.DeleteCustomerProviderRefsParams{TenantID: tx.tenantID, CustomerID: customerID})
	if err != nil {
		return out, err
	}
	links, err := tx.q.RevokeCustomerPortalLinks(ctx, dbgen.RevokeCustomerPortalLinksParams{RevokedAt: &at, TenantID: tx.tenantID,
		CustomerID: customerID})
	if err != nil {
		return out, err
	}
	notifications, err := tx.q.DeleteCustomerNotifications(ctx, dbgen.DeleteCustomerNotificationsParams{TenantID: tx.tenantID,
		CustomerID: customerID})
	if err != nil {
		return out, err
	}
	payloads, err := tx.q.PurgeCustomerProviderWebhookPayloads(ctx, dbgen.PurgeCustomerProviderWebhookPayloadsParams{Now: &at,
		TenantID: tx.tenantID, CustomerID: customerID})
	if err != nil {
		return out, err
	}
	out = customers.Erasure{Contacts: int(contacts), Address: address > 0, PortalLinksRevoked: int(links), Notifications: int(notifications),
		ProviderWebhookPayloads: int(payloads)}
	for _, r := range refs {
		out.ProviderRefs = append(out.ProviderRefs, customers.ProviderRef{Provider: r.Provider, ProviderCustomerID: r.ProviderCustomerID})
	}
	slices.SortFunc(out.ProviderRefs, func(a, b customers.ProviderRef) int { return strings.Compare(a.Provider, b.Provider) })
	return out, nil
}

// --- créditos do cliente (usados por faturamento e pagamentos) ---

// CreditEntry é um movimento de crédito do cliente para consulta.
type CreditEntry struct {
	ID        uuid.UUID
	Amount    int64
	Kind      string
	InvoiceID *uuid.UUID
	Reason    string
	CreatedAt time.Time
}

func (b base) creditBalance(ctx context.Context, customerID uuid.UUID) (money.Money, error) {
	v, err := b.q.CustomerCreditBalance(ctx, dbgen.CustomerCreditBalanceParams{TenantID: b.tenantID, CustomerID: customerID})
	return brl(v), err
}

// addCredit grava um movimento de crédito. Movimentos negativos (consumo)
// bloqueiam o cliente e verificam que o saldo não fica negativo.
func (b base) addCredit(ctx context.Context, customerID uuid.UUID, amount int64, kind string, invoiceID *uuid.UUID, reason string, createdBy *uuid.UUID) error {
	_, _, err := b.addCreditKeyed(ctx, customerID, amount, kind, invoiceID, reason, createdBy, "")
	return err
}

// addCreditKeyed grava o movimento com chave de negócio opcional; inserted
// é falso quando a chave já foi usada (pedido repetido).
func (b base) addCreditKeyed(ctx context.Context, customerID uuid.UUID, amount int64, kind string, invoiceID *uuid.UUID, reason string,
	createdBy *uuid.UUID, requestKey string) (uuid.UUID, bool, error) {
	if _, err := b.q.LockCustomerCredits(ctx, dbgen.LockCustomerCreditsParams{TenantID: b.tenantID, CustomerID: customerID}); err != nil {
		return uuid.Nil, false, notFound(err, "customer_not_found", "Cliente não encontrado.")
	}
	if amount < 0 {
		bal, err := b.creditBalance(ctx, customerID)
		if err != nil {
			return uuid.Nil, false, err
		}
		if bal.Amount()+amount < 0 {
			return uuid.Nil, false, apperr.Conflict("insufficient_credit", "Saldo de crédito insuficiente.")
		}
	}
	id := ids.New()
	n, err := b.q.InsertCreditEntry(ctx, dbgen.InsertCreditEntryParams{ID: id, TenantID: b.tenantID, CustomerID: customerID, Amount: amount,
		Currency: money.BRL.Code(), Kind: kind, InvoiceID: invoiceID, Reason: reason, CreatedBy: createdBy, RequestKey: strPtr(requestKey),
		CreatedAt: b.store.clock.Now()})
	return id, n == 1, err
}

// Credits retorna saldo e movimentos de crédito do cliente.
func (c *Customers) Credits(ctx context.Context, tenantID, customerID uuid.UUID) (money.Money, []CreditEntry, error) {
	var bal money.Money
	var out []CreditEntry
	err := c.s.inTenant(ctx, tenantID, func(b base) error {
		if _, err := b.q.GetCustomer(ctx, dbgen.GetCustomerParams{TenantID: tenantID, ID: customerID}); err != nil {
			return notFound(err, "customer_not_found", "Cliente não encontrado.")
		}
		var err error
		if bal, err = b.creditBalance(ctx, customerID); err != nil {
			return err
		}
		rows, err := b.q.ListCreditEntries(ctx, dbgen.ListCreditEntriesParams{TenantID: tenantID, CustomerID: customerID})
		for _, r := range rows {
			out = append(out, CreditEntry{ID: r.ID, Amount: r.Amount, Kind: r.Kind, InvoiceID: r.InvoiceID, Reason: r.Reason, CreatedAt: r.CreatedAt})
		}
		return err
	})
	return bal, out, err
}
