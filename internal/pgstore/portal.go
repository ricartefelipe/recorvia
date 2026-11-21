package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/portal"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// Portal implementa portal.Store.
type Portal struct{ s *Store }

// Portal retorna o adaptador do portal do cliente.
func (s *Store) Portal() *Portal { return &Portal{s: s} }

type portalTx struct{ base }

// InTenant abre a transação do tenant.
func (p *Portal) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(portal.Tx) error) error {
	return p.s.inTenant(ctx, tenantID, func(b base) error { return fn(portalTx{b}) })
}

// FindLink localiza o tenant e o link pelo hash do token.
func (p *Portal) FindLink(ctx context.Context, tokenHash []byte) (uuid.UUID, uuid.UUID, error) {
	var row dbgen.FindPortalLinkRow
	err := p.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		row, err = q.FindPortalLink(ctx, tokenHash)
		return notFound(err, "portal_link_not_found", "Link não encontrado.")
	})
	return row.TenantID, row.LinkID, err
}

func portalLinkFromRow(r dbgen.CustomerPortalLink) portal.Link {
	return portal.Link{ID: r.ID, TenantID: r.TenantID, CustomerID: r.CustomerID, TokenHash: r.TokenHash, CreatedByUser: r.CreatedByUserID,
		CreatedByCredential: r.CreatedByCredentialID, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, UsedAt: r.UsedAt, RevokedAt: r.RevokedAt,
		RevokeReason: deref(r.RevokeReason)}
}

func (tx portalTx) Tenant(ctx context.Context) (tenancy.Tenant, error) {
	r, err := tx.q.GetTenant(ctx)
	if err != nil {
		return tenancy.Tenant{}, notFound(err, "organization_not_found", "Organização não encontrada.")
	}
	return tenantFromRow(r), nil
}

func (tx portalTx) Customer(ctx context.Context, id uuid.UUID, lock bool) (customers.Customer, error) {
	var r dbgen.Customer
	var err error
	if lock {
		r, err = tx.q.LockCustomerForPortalLink(ctx, dbgen.LockCustomerForPortalLinkParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetCustomer(ctx, dbgen.GetCustomerParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return customers.Customer{}, notFound(err, "customer_not_found", "Cliente não encontrado.")
	}
	return customerFromRow(r), nil
}

func (tx portalTx) Contacts(ctx context.Context, customerID uuid.UUID) ([]customers.Contact, error) {
	return customersTx(tx).Contacts(ctx, customerID)
}

func (tx portalTx) InsertLink(ctx context.Context, l portal.Link) error {
	err := tx.q.InsertPortalLink(ctx, dbgen.InsertPortalLinkParams{ID: l.ID, TenantID: tx.tenantID, CustomerID: l.CustomerID,
		TokenHash: l.TokenHash, CreatedByUserID: l.CreatedByUser, CreatedByCredentialID: l.CreatedByCredential, CreatedAt: l.CreatedAt,
		ExpiresAt: l.ExpiresAt})
	if postgres.IsUniqueViolation(err, "customer_portal_links_active_key") {
		return apperr.Conflict("portal_link_concurrent", "Outro link foi gerado para este cliente ao mesmo tempo. Tente novamente.")
	}
	return err
}

func (tx portalTx) RevokeActiveLinks(ctx context.Context, customerID uuid.UUID, at time.Time, reason string) ([]uuid.UUID, error) {
	return tx.q.RevokeActivePortalLinks(ctx, dbgen.RevokeActivePortalLinksParams{TenantID: tx.tenantID, CustomerID: customerID,
		RevokedAt: &at, RevokeReason: &reason})
}

func (tx portalTx) RevokeLink(ctx context.Context, customerID, id uuid.UUID, at time.Time, reason string) error {
	n, err := tx.q.RevokePortalLink(ctx, dbgen.RevokePortalLinkParams{TenantID: tx.tenantID, CustomerID: customerID, ID: id,
		RevokedAt: &at, RevokeReason: &reason})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.Conflict("portal_link_unavailable", "O link já foi revogado ou não pertence a este cliente.")
	}
	return nil
}

// portalCursor converte a posição da página anterior nos parâmetros de
// keyset das consultas do portal.
func portalCursor(after *portal.Position) (*time.Time, *uuid.UUID) {
	if after == nil {
		return nil, nil
	}
	return &after.At, &after.ID
}

// portalRowLimit limita a consulta a uma página mais o item que indica a
// existência da seguinte.
func portalRowLimit(limit int) int32 {
	return int32(min(max(limit, 1), portal.MaxPageSize+1)) //nolint:gosec // limitado ao tamanho máximo de página
}

func (tx portalTx) Links(ctx context.Context, customerID uuid.UUID, after *portal.Position, limit int) ([]portal.LinkSummary, error) {
	ts, id := portalCursor(after)
	rows, err := tx.q.ListPortalLinks(ctx, dbgen.ListPortalLinksParams{TenantID: tx.tenantID, CustomerID: customerID, CursorTs: ts, CursorID: id,
		RowLimit: portalRowLimit(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]portal.LinkSummary, 0, len(rows))
	for _, r := range rows {
		l := portal.LinkSummary{Link: portal.Link{ID: r.ID, TenantID: r.TenantID, CustomerID: r.CustomerID, CreatedByUser: r.CreatedByUserID,
			CreatedByCredential: r.CreatedByCredentialID, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, UsedAt: r.UsedAt, RevokedAt: r.RevokedAt,
			RevokeReason: deref(r.RevokeReason)}, CreatorName: deref(r.CreatedByUserName)}
		if r.CreatedByCredentialID != nil {
			l.CreatorName = deref(r.CreatedByCredentialName)
		}
		out = append(out, l)
	}
	return out, nil
}

func (tx portalTx) Link(ctx context.Context, id uuid.UUID, forUpdate bool) (portal.Link, error) {
	var r dbgen.CustomerPortalLink
	var err error
	if forUpdate {
		r, err = tx.q.GetPortalLinkForUpdate(ctx, dbgen.GetPortalLinkForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetPortalLink(ctx, dbgen.GetPortalLinkParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return portal.Link{}, notFound(err, "portal_link_not_found", "Link não encontrado.")
	}
	return portalLinkFromRow(r), nil
}

func (tx portalTx) MarkLinkUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	n, err := tx.q.MarkPortalLinkUsed(ctx, dbgen.MarkPortalLinkUsedParams{TenantID: tx.tenantID, ID: id, UsedAt: &at})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.Conflict("portal_link_unavailable", "O link já foi utilizado, revogado ou expirou.")
	}
	return nil
}

func (tx portalTx) Invoices(ctx context.Context, customerID uuid.UUID, after *portal.Position, limit int) ([]billing.Invoice, error) {
	ts, id := portalCursor(after)
	rows, err := tx.q.ListPortalInvoices(ctx, dbgen.ListPortalInvoicesParams{TenantID: tx.tenantID, CustomerID: customerID, CursorTs: ts,
		CursorID: id, RowLimit: portalRowLimit(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]billing.Invoice, 0, len(rows))
	index := make(map[uuid.UUID]int, len(rows))
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		inv, err := invoiceFromRow(r)
		if err != nil {
			return nil, err
		}
		index[inv.ID] = len(out)
		ids = append(ids, inv.ID)
		out = append(out, inv)
	}
	if len(ids) == 0 {
		return out, nil
	}
	items, err := tx.q.ListPortalInvoiceItems(ctx, dbgen.ListPortalInvoiceItemsParams{TenantID: tx.tenantID, InvoiceIds: ids})
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		i, ok := index[it.InvoiceID]
		if !ok {
			continue
		}
		item, err := invoiceItemFromRow(it, out[i].Currency)
		if err != nil {
			return nil, err
		}
		out[i].Items = append(out[i].Items, item)
	}
	return out, nil
}

func (tx portalTx) Invoice(ctx context.Context, customerID, id uuid.UUID) (billing.Invoice, error) {
	r, err := tx.q.GetPortalInvoice(ctx, dbgen.GetPortalInvoiceParams{TenantID: tx.tenantID, CustomerID: customerID, ID: id})
	if err != nil {
		return billing.Invoice{}, notFound(err, "invoice_not_found", "Fatura não encontrada.")
	}
	inv, err := invoiceFromRow(r)
	if err != nil {
		return billing.Invoice{}, err
	}
	items, err := tx.q.ListPortalInvoiceItems(ctx, dbgen.ListPortalInvoiceItemsParams{TenantID: tx.tenantID, InvoiceIds: []uuid.UUID{id}})
	if err != nil {
		return billing.Invoice{}, err
	}
	for _, it := range items {
		item, err := invoiceItemFromRow(it, inv.Currency)
		if err != nil {
			return billing.Invoice{}, err
		}
		inv.Items = append(inv.Items, item)
	}
	return inv, nil
}

func (tx portalTx) CheckoutURLs(ctx context.Context, invoiceIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(invoiceIDs))
	if len(invoiceIDs) == 0 {
		return out, nil
	}
	rows, err := tx.q.ListPortalCheckoutURLs(ctx, dbgen.ListPortalCheckoutURLsParams{TenantID: tx.tenantID, InvoiceIds: invoiceIDs})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.InvoiceID] = r.CheckoutUrl
	}
	return out, nil
}

func (tx portalTx) SetContactPreference(ctx context.Context, customerID, contactID uuid.UUID, receivesBillingEmail bool) error {
	n, err := tx.q.SetContactBillingPreference(ctx, dbgen.SetContactBillingPreferenceParams{ReceivesBillingEmail: receivesBillingEmail,
		TenantID: tx.tenantID, CustomerID: customerID, ID: contactID})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.NotFound("contact_not_found", "Contato não encontrado.")
	}
	return nil
}
