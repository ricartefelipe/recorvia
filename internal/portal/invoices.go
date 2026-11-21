package portal

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

const (
	// MaxContactPreferences é o máximo de contatos por alteração de
	// preferências (o mesmo limite de contatos por cliente).
	MaxContactPreferences = 20
)

// Invoice é uma fatura emitida do cliente como exibida no portal.
type Invoice struct {
	billing.Invoice
	// DueDate é a data civil de vencimento no fuso da organização.
	DueDate *calendar.Date
	// Overdue indica fatura em aberto com o vencimento ultrapassado.
	Overdue bool
	// CheckoutURL é o pagamento hospedado da cobrança em aberto; vazio
	// quando a fatura não pode ser paga pelo portal.
	CheckoutURL string
}

// Line é um item da fatura com o valor líquido do desconto: a soma das
// linhas é o total da fatura.
type Line struct {
	Description string
	Amount      money.Money
}

// Lines devolve os itens da fatura com o valor líquido do desconto.
func (i Invoice) Lines() ([]Line, error) {
	out := make([]Line, 0, len(i.Items))
	for _, it := range i.Items {
		net, err := it.Amount.Sub(it.DiscountAmount)
		if err != nil {
			return nil, err
		}
		out = append(out, Line{Description: it.Description, Amount: net})
	}
	return out, nil
}

// InvoiceDetail é a fatura com o necessário para o documento comercial.
type InvoiceDetail struct {
	Invoice
	// Location é o fuso da organização para as datas do documento.
	Location         *time.Location
	OrganizationName string
	// OrganizationLegalName e OrganizationDocument identificam o emitente
	// (razão social e CNPJ da organização) no documento.
	OrganizationLegalName string
	OrganizationDocument  string
}

// ContactPreference é a escolha do cliente para um de seus contatos.
type ContactPreference struct {
	ContactID            uuid.UUID
	ReceivesBillingEmail bool
}

var errInvoiceNotFound = apperr.NotFound("invoice_not_found", "Fatura não encontrada.")

// visible confirma que a fatura pertence ao cliente da sessão e foi emitida.
// O armazenamento já filtra por cliente e situação; a verificação aqui é uma
// segunda barreira.
func visible(inv billing.Invoice, sess Session) bool {
	return inv.TenantID == sess.TenantID && inv.CustomerID == sess.CustomerID && inv.Status != billing.Draft
}

// payable informa se a fatura ainda pode ser paga pelo portal.
func payable(inv billing.Invoice) bool {
	return (inv.Status == billing.Open || inv.Status == billing.PartiallyPaid) && inv.AmountDue.IsPositive()
}

// safeCheckoutURL aceita apenas endereços absolutos http(s): o link vem do
// provedor de pagamentos e é exibido ao cliente como destino de navegação.
func safeCheckoutURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}

func present(inv billing.Invoice, loc *time.Location, now time.Time, checkout string) Invoice {
	out := Invoice{Invoice: inv, Overdue: inv.IsOverdue(now)}
	if inv.DueAt != nil {
		d := billing.DueDate(*inv.DueAt, loc)
		out.DueDate = &d
	}
	if payable(inv) && safeCheckoutURL(checkout) {
		out.CheckoutURL = checkout
	}
	return out
}

func tenantLocation(ctx context.Context, tx Tx) (*time.Location, string, error) {
	t, err := tx.Tenant(ctx)
	if err != nil {
		return nil, "", err
	}
	loc, err := t.Location()
	if err != nil {
		return nil, "", err
	}
	return loc, t.Name, nil
}

// InvoicePage é uma página das faturas do cliente no portal.
type InvoicePage struct {
	Items      []Invoice
	NextCursor string
}

// Invoices lista as faturas emitidas do cliente da sessão, das mais recentes
// para as mais antigas, em páginas. Rascunhos nunca aparecem. A organização
// e o cliente consultados vêm somente da sessão: o cursor guarda apenas a
// posição (emissão e identificador da última fatura da página anterior), de
// modo que faturas emitidas durante a navegação não deslocam as páginas
// seguintes e um cursor obtido em outra sessão não alcança faturas alheias.
func (s *Service) Invoices(ctx context.Context, sess Session, q PageQuery) (InvoicePage, error) {
	after, err := parseCursor(cursorInvoices, q.Cursor)
	if err != nil {
		return InvoicePage{}, err
	}
	limit := pageLimit(q.Limit)
	now := s.clock.Now()
	var page InvoicePage
	err = s.store.InTenant(ctx, sess.TenantID, func(tx Tx) error {
		page = InvoicePage{Items: []Invoice{}}
		loc, _, err := tenantLocation(ctx, tx)
		if err != nil {
			return err
		}
		invs, err := tx.Invoices(ctx, sess.CustomerID, after, limit+1)
		if err != nil {
			return err
		}
		if len(invs) > limit {
			invs = invs[:limit]
			last := invs[limit-1]
			if last.IssuedAt == nil {
				return errors.New("portal: fatura listada sem data de emissão")
			}
			page.NextCursor = encodeCursor(cursorInvoices, Position{At: *last.IssuedAt, ID: last.ID})
		}
		ids := make([]uuid.UUID, 0, len(invs))
		for _, inv := range invs {
			ids = append(ids, inv.ID)
		}
		urls, err := tx.CheckoutURLs(ctx, ids)
		if err != nil {
			return err
		}
		for _, inv := range invs {
			if visible(inv, sess) {
				page.Items = append(page.Items, present(inv, loc, now, urls[inv.ID]))
			}
		}
		return nil
	})
	if err != nil {
		return InvoicePage{}, err
	}
	return page, nil
}

// Invoice lê uma fatura emitida do cliente da sessão. Fatura de outro
// cliente, de outra organização ou em rascunho responde como inexistente.
func (s *Service) Invoice(ctx context.Context, sess Session, id uuid.UUID) (InvoiceDetail, error) {
	now := s.clock.Now()
	var out InvoiceDetail
	err := s.store.InTenant(ctx, sess.TenantID, func(tx Tx) error {
		t, err := tx.Tenant(ctx)
		if err != nil {
			return err
		}
		loc, err := t.Location()
		if err != nil {
			return err
		}
		inv, err := tx.Invoice(ctx, sess.CustomerID, id)
		if err != nil {
			if apperr.IsKind(err, apperr.KindNotFound) {
				return errInvoiceNotFound
			}
			return err
		}
		if !visible(inv, sess) {
			return errInvoiceNotFound
		}
		urls, err := tx.CheckoutURLs(ctx, []uuid.UUID{inv.ID})
		if err != nil {
			return err
		}
		out = InvoiceDetail{Invoice: present(inv, loc, now, urls[inv.ID]), Location: loc, OrganizationName: t.Name,
			OrganizationLegalName: t.LegalName, OrganizationDocument: t.DocumentNumber}
		return nil
	})
	return out, err
}

func validatePreferences(prefs []ContactPreference) error {
	if len(prefs) > MaxContactPreferences {
		return apperr.Invalid("invalid_contact_preferences", "Preferências de contato inválidas.",
			apperr.FieldError{Field: "contacts", Message: "Informe no máximo 20 contatos."})
	}
	seen := make(map[uuid.UUID]bool, len(prefs))
	for _, p := range prefs {
		if p.ContactID == uuid.Nil || seen[p.ContactID] {
			return apperr.Invalid("invalid_contact_preferences", "Preferências de contato inválidas.",
				apperr.FieldError{Field: "contacts", Message: "Informe cada contato uma única vez."})
		}
		seen[p.ContactID] = true
	}
	return nil
}

// UpdateContactPreferences grava a escolha do cliente sobre quais contatos
// recebem e-mails de cobrança. Somente contatos do próprio cliente podem
// ser alterados (os demais respondem como inexistentes); contatos não
// informados ficam como estão. A alteração é auditada com o ator do portal
// e serializada pelo bloqueio do cliente.
func (s *Service) UpdateContactPreferences(ctx context.Context, sess Session, prefs []ContactPreference) (View, error) {
	if err := validatePreferences(prefs); err != nil {
		return View{}, err
	}
	now := s.clock.Now()
	v := View{Session: sess}
	err := s.store.InTenant(ctx, sess.TenantID, func(tx Tx) error {
		t, err := tx.Tenant(ctx)
		if err != nil {
			return err
		}
		c, err := tx.Customer(ctx, sess.CustomerID, true)
		if err != nil {
			return err
		}
		if c.Status != customers.Active {
			return errSessionInvalid
		}
		contacts, err := tx.Contacts(ctx, sess.CustomerID)
		if err != nil {
			return err
		}
		index := make(map[uuid.UUID]int, len(contacts))
		for i, ct := range contacts {
			index[ct.ID] = i
		}
		var changes []map[string]any
		for _, p := range prefs {
			i, ok := index[p.ContactID]
			if !ok {
				return apperr.NotFound("contact_not_found", "Contato não encontrado.")
			}
			if contacts[i].ReceivesBillingEmail == p.ReceivesBillingEmail {
				continue
			}
			if err := tx.SetContactPreference(ctx, sess.CustomerID, p.ContactID, p.ReceivesBillingEmail); err != nil {
				return err
			}
			contacts[i].ReceivesBillingEmail = p.ReceivesBillingEmail
			changes = append(changes, map[string]any{"contact_id": p.ContactID, "receives_billing_email": p.ReceivesBillingEmail})
		}
		if len(changes) > 0 {
			if err := tx.Audit(ctx, audit.New(sess.TenantID, now, sess.Actor(), "customer.contact_preferences_updated", "customer",
				sess.CustomerID.String(), map[string]any{"link_id": sess.LinkID, "changes": changes})); err != nil {
				return err
			}
		}
		v.CustomerName, v.OrganizationName, v.Contacts = c.Name, t.Name, contacts
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return v, nil
}
