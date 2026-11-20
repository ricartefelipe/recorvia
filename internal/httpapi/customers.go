package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
)

type customerDTO struct {
	ID               uuid.UUID `json:"id"`
	Kind             string    `json:"kind"`
	Name             string    `json:"name"`
	LegalName        string    `json:"legal_name"`
	DocumentType     string    `json:"document_type"`
	DocumentNumber   string    `json:"document_number"`
	Email            string    `json:"email"`
	Phone            string    `json:"phone"`
	ExternalID       string    `json:"external_id"`
	Status           string    `json:"status"`
	PaymentTermsDays *int      `json:"payment_terms_days"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Version          int       `json:"version"`
}

type contactDTO struct {
	ID                   uuid.UUID `json:"id"`
	Name                 string    `json:"name"`
	Email                string    `json:"email"`
	Phone                string    `json:"phone"`
	Role                 string    `json:"role"`
	ReceivesBillingEmail bool      `json:"receives_billing_email"`
	CreatedAt            time.Time `json:"created_at"`
}

type customerDetailsDTO struct {
	customerDTO
	Contacts       []contactDTO       `json:"contacts"`
	BillingAddress *customers.Address `json:"billing_address"`
}

func custDTO(c customers.Customer) customerDTO {
	return customerDTO{ID: c.ID, Kind: string(c.Kind), Name: c.Name, LegalName: c.LegalName, DocumentType: string(c.DocumentType),
		DocumentNumber: c.DocumentNumber, Email: c.Email, Phone: c.Phone, ExternalID: c.ExternalID, Status: string(c.Status),
		PaymentTermsDays: c.PaymentTermsDays, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Version: c.Version}
}

func contDTO(c customers.Contact) contactDTO {
	return contactDTO{ID: c.ID, Name: c.Name, Email: c.Email, Phone: c.Phone, Role: c.Role, ReceivesBillingEmail: c.ReceivesBillingEmail, CreatedAt: c.CreatedAt}
}

func detailsDTO(d customers.Details) customerDetailsDTO {
	out := customerDetailsDTO{customerDTO: custDTO(d.Customer), BillingAddress: d.Address, Contacts: []contactDTO{}}
	for _, c := range d.Contacts {
		out.Contacts = append(out.Contacts, contDTO(c))
	}
	return out
}

type customerInput struct {
	Kind             customers.Kind `json:"kind"`
	Name             string         `json:"name"`
	LegalName        string         `json:"legal_name"`
	DocumentNumber   string         `json:"document_number"`
	Email            string         `json:"email"`
	Phone            string         `json:"phone"`
	ExternalID       string         `json:"external_id"`
	PaymentTermsDays *int           `json:"payment_terms_days"`
}

func (in customerInput) domain() customers.Input {
	return customers.Input{Kind: in.Kind, Name: in.Name, LegalName: in.LegalName, DocumentNumber: in.DocumentNumber, Email: in.Email,
		Phone: in.Phone, ExternalID: in.ExternalID, PaymentTermsDays: in.PaymentTermsDays}
}

func (a *API) listCustomers(w http.ResponseWriter, r *http.Request) {
	limit, err := queryLimit(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	pg, err := a.Customers.List(r.Context(), mustPrincipal(r).TenantID, customers.ListFilter{Status: customers.Status(q.Get("status")),
		Search: q.Get("q"), Sort: q.Get("sort"), Limit: limit, Cursor: q.Get("cursor")})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]customerDTO, 0, len(pg.Items))
	for _, c := range pg.Items {
		out = append(out, custDTO(c))
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, pg.NextCursor))
}

func (a *API) createCustomer(w http.ResponseWriter, r *http.Request) {
	var in customerInput
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	c, err := a.Customers.Create(r.Context(), p.TenantID, p.Actor(), in.domain())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusCreated, c.Version, customerDetailsDTO{customerDTO: custDTO(c), Contacts: []contactDTO{}})
}

func (a *API) getCustomer(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	d, err := a.Customers.Get(r.Context(), mustPrincipal(r).TenantID, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusOK, d.Version, detailsDTO(d))
}

func (a *API) updateCustomer(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	version, err := ifMatch(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in customerInput
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	if _, err := a.Customers.Update(r.Context(), p.TenantID, p.Actor(), id, in.domain(), version); err != nil {
		a.writeError(w, r, err)
		return
	}
	a.getCustomer(w, r)
}

func (a *API) archiveCustomer(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	if _, err := a.Customers.Archive(r.Context(), p.TenantID, p.Actor(), id); err != nil {
		a.writeError(w, r, err)
		return
	}
	a.getCustomer(w, r)
}

func (a *API) addContact(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		Name                 string `json:"name"`
		Email                string `json:"email"`
		Phone                string `json:"phone"`
		Role                 string `json:"role"`
		ReceivesBillingEmail *bool  `json:"receives_billing_email"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	receives := true
	if in.ReceivesBillingEmail != nil {
		receives = *in.ReceivesBillingEmail
	}
	p := mustPrincipal(r)
	c, err := a.Customers.AddContact(r.Context(), p.TenantID, p.Actor(), id, customers.ContactInput{Name: in.Name, Email: in.Email, Phone: in.Phone,
		Role: in.Role, ReceivesBillingEmail: receives})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, contDTO(c))
}

func (a *API) removeContact(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	contactID, err := pathUUID(r, "contact_id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	if err := a.Customers.RemoveContact(r.Context(), p.TenantID, p.Actor(), id, contactID); err != nil {
		a.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) setAddress(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in customers.Address
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	addr, err := a.Customers.SetAddress(r.Context(), p.TenantID, p.Actor(), id, in)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, addr)
}

func (a *API) customerHistory(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	if _, err := a.Customers.Get(r.Context(), p.TenantID, id); err != nil {
		a.writeError(w, r, err)
		return
	}
	events, _, err := a.Store.AuditEvents(r.Context(), p.TenantID, pgstore.AuditFilter{ResourceType: "customer", ResourceID: id.String(), Limit: 100})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, list(auditDTOs(events)))
}

func (a *API) creditBalanceView(r *http.Request, customerID uuid.UUID) (map[string]any, error) {
	bal, entries, err := a.Store.Customers().Credits(r.Context(), mustPrincipal(r).TenantID, customerID)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{"id": e.ID, "amount": e.Amount, "kind": e.Kind, "invoice_id": e.InvoiceID, "reason": e.Reason, "created_at": e.CreatedAt})
	}
	return map[string]any{"balance": bal.Amount(), "currency": bal.Currency().Code(), "entries": out}, nil
}

func (a *API) getCredits(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	v, err := a.creditBalanceView(r, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, v)
}

func (a *API) grantCredit(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		Amount int64  `json:"amount"`
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	if err := a.Billing.GrantCredit(r.Context(), p.TenantID, p.Actor(), p.UserID(), id, money.New(in.Amount, money.BRL), in.Reason,
		idempotencyKeyFrom(r.Context())); err != nil {
		a.writeError(w, r, err)
		return
	}
	v, err := a.creditBalanceView(r, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, v)
}
