package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/portal"
)

// Rotas do portal do cliente. O portal tem cookie, sessão e CSRF próprios e
// nunca aceita a sessão do painel nem credenciais de API: a identidade é o
// cliente do link, restrita a ele e ao tenant que gerou o link.

type portalKey struct{}

func withPortalSession(ctx context.Context, s portal.Session) context.Context {
	return context.WithValue(ctx, portalKey{}, s)
}

func portalSessionFrom(ctx context.Context) (portal.Session, bool) {
	s, ok := ctx.Value(portalKey{}).(portal.Session)
	return s, ok
}

func (a *API) portalCookieName() string {
	if a.Config != nil && a.Config.PortalCookieName != "" {
		return a.Config.PortalCookieName
	}
	return "recorvia_portal"
}

// setPortalCookie grava o cookie do portal: HttpOnly, Secure conforme a
// configuração (obrigatório em produção) e SameSite=Strict, que basta porque
// o cookie só é usado pelas chamadas da própria interface do portal.
func (a *API) setPortalCookie(w http.ResponseWriter, s portal.Session) {
	secure := a.Config == nil || a.Config.CookieSecure
	maxAge := int(s.ExpiresAt.Sub(a.Clock.Now()).Seconds())
	http.SetCookie(w, &http.Cookie{Name: a.portalCookieName(), Value: s.ID, Path: "/", HttpOnly: true, Secure: secure, //nolint:gosec // Secure segue a configuração
		SameSite: http.SameSiteStrictMode, MaxAge: max(maxAge, 1)})
}

func (a *API) clearPortalCookie(w http.ResponseWriter) {
	secure := a.Config == nil || a.Config.CookieSecure
	http.SetCookie(w, &http.Cookie{Name: a.portalCookieName(), Value: "", Path: "/", HttpOnly: true, Secure: secure, //nolint:gosec // Secure segue a configuração
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func (a *API) registerPortal(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(a.rateLimitIPScoped("portal_access_ip", 30))
		a.handle(r, http.MethodGet, "/portal/access", Public, "", a.portalAccess)
	})
	r.Group(func(r chi.Router) {
		r.Use(a.authenticatePortal, a.portalCSRF, a.rateLimitPortal)
		a.handle(r, http.MethodGet, "/api/portal/v1/session", Portal, "", a.portalSession)
		a.handle(r, http.MethodGet, "/api/portal/v1/invoices", Portal, "", a.portalInvoices)
		a.handle(r, http.MethodGet, "/api/portal/v1/invoices/{id}", Portal, "", a.portalInvoice)
		a.handle(r, http.MethodGet, "/api/portal/v1/invoices/{id}/document", Portal, "", a.portalInvoiceDocument)
		a.handle(r, http.MethodPut, "/api/portal/v1/contact-preferences", Portal, "", a.portalUpdateContactPreferences)
		a.handle(r, http.MethodPost, "/api/portal/v1/logout", Portal, "", a.portalLogout)
	})
}

// authenticatePortal valida a sessão do portal a cada requisição.
func (a *API) authenticatePortal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Portal == nil {
			a.writeError(w, r, apperr.Unavailable("portal_unavailable", "Portal do cliente indisponível."))
			return
		}
		c, err := r.Cookie(a.portalCookieName())
		if err != nil || c.Value == "" {
			a.writeError(w, r, apperr.Unauthenticated("portal_session_required", "Acesse o portal pelo link enviado pela empresa."))
			return
		}
		s, err := a.Portal.Authenticate(r.Context(), c.Value)
		if err != nil {
			if apperr.IsKind(err, apperr.KindUnauthenticated) {
				a.clearPortalCookie(w)
			}
			a.writeError(w, r, err)
			return
		}
		ctx := logging.WithAttrs(r.Context(), slog.String("tenant_id", s.TenantID.String()), slog.String("principal", "customer_portal"))
		next.ServeHTTP(w, r.WithContext(withPortalSession(ctx, s)))
	})
}

// portalCSRF exige o token da sessão do portal e origem da própria
// aplicação em métodos que alteram estado.
func (a *API) portalCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := portalSessionFrom(r.Context())
		if ok && !safeMethod(r.Method) && (!a.sameOrigin(r) || !s.ValidCSRF(r.Header.Get("X-CSRF-Token"))) {
			a.writeError(w, r, apperr.Forbidden("csrf_invalid", "Requisição recusada: token CSRF ausente ou inválido. Recarregue a página."))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimitPortal limita requisições por cliente do portal.
func (a *API) rateLimitPortal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := portalSessionFrom(r.Context())
		if ok && a.Sessions != nil {
			allowed, _, err := a.Sessions.Allow(r.Context(), "portal:"+s.TenantID.String()+":"+s.CustomerID.String(), 300, time.Minute)
			if err != nil {
				a.log.WarnContext(r.Context(), "limite de taxa indisponível", slog.String("error", err.Error()))
			} else if !allowed {
				a.writeError(w, r, apperr.RateLimited("rate_limited", "Muitas requisições. Aguarde e tente novamente."))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func mustPortalSession(r *http.Request) portal.Session {
	s, _ := portalSessionFrom(r.Context())
	return s
}

// portalURL monta o link de acesso ao portal (o token vai na query e é
// trocado por sessão na primeira abertura).
func (a *API) portalURL(token string) string {
	base := "/"
	if a.Config != nil && a.Config.PublicURL != nil {
		base = strings.TrimRight(a.Config.PublicURL.String(), "/") + "/"
	}
	return base + "portal/access?token=" + url.QueryEscape(token)
}

func (a *API) createPortalLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if a.Portal == nil {
		a.writeError(w, r, apperr.Unavailable("portal_unavailable", "Portal do cliente indisponível."))
		return
	}
	p := mustPrincipal(r)
	creator := portal.Creator{Actor: p.Actor(), UserID: p.UserID()}
	if p.Credential != nil {
		cid := p.Credential.CredentialID
		creator.CredentialID = &cid
	}
	l, token, err := a.Portal.CreateLink(r.Context(), p.TenantID, creator, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, map[string]any{"id": l.ID, "url": a.portalURL(token), "expires_at": l.ExpiresAt})
}

type portalLinkCreatorDTO struct {
	Type string    `json:"type"`
	ID   uuid.UUID `json:"id"`
	Name *string   `json:"name"`
}

// portalLinkDTO é o link como a organização o vê: nunca traz o token nem o
// hash do token.
type portalLinkDTO struct {
	ID           uuid.UUID            `json:"id"`
	Status       string               `json:"status"`
	CreatedAt    time.Time            `json:"created_at"`
	ExpiresAt    time.Time            `json:"expires_at"`
	UsedAt       *time.Time           `json:"used_at"`
	RevokedAt    *time.Time           `json:"revoked_at"`
	RevokeReason *string              `json:"revoke_reason"`
	CreatedBy    portalLinkCreatorDTO `json:"created_by"`
}

func portalLinkView(l portal.LinkSummary) portalLinkDTO {
	out := portalLinkDTO{ID: l.ID, Status: string(l.Current), CreatedAt: l.CreatedAt, ExpiresAt: l.ExpiresAt, UsedAt: l.UsedAt, RevokedAt: l.RevokedAt}
	if l.RevokeReason != "" {
		reason := l.RevokeReason
		out.RevokeReason = &reason
	}
	switch {
	case l.CreatedByCredential != nil:
		out.CreatedBy = portalLinkCreatorDTO{Type: "api_credential", ID: *l.CreatedByCredential}
	case l.CreatedByUser != nil:
		out.CreatedBy = portalLinkCreatorDTO{Type: "user", ID: *l.CreatedByUser}
	}
	if l.CreatorName != "" {
		name := l.CreatorName
		out.CreatedBy.Name = &name
	}
	return out
}

// listPortalLinks lista os links do portal de um cliente, dos mais novos
// para os mais antigos.
func (a *API) listPortalLinks(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	limit, err := queryLimit(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if a.Portal == nil {
		a.writeError(w, r, apperr.Unavailable("portal_unavailable", "Portal do cliente indisponível."))
		return
	}
	page, err := a.Portal.Links(r.Context(), mustPrincipal(r).TenantID, id, portal.PageQuery{Limit: limit, Cursor: r.URL.Query().Get("cursor")})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]portalLinkDTO, 0, len(page.Items))
	for _, l := range page.Items {
		out = append(out, portalLinkView(l))
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, page.NextCursor))
}

// revokePortalLink revoga um link do cliente e encerra as sessões do portal
// abertas com ele. Repetir a chamada tem o mesmo resultado (204) e não gera
// nova auditoria. Não há coluna de autoria: quem revogou fica na auditoria,
// pelo ator, e a rota aceita credenciais de API pelo escopo (ADR-059).
func (a *API) revokePortalLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	linkID, err := pathUUID(r, "link_id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if a.Portal == nil {
		a.writeError(w, r, apperr.Unavailable("portal_unavailable", "Portal do cliente indisponível."))
		return
	}
	p := mustPrincipal(r)
	res, err := a.Portal.RevokeLink(r.Context(), p.TenantID, p.Actor(), id, linkID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if res.SessionCleanupErr != nil {
		// O link está revogado e a sessão já não é aceita; fica pendente
		// apenas a remoção no armazenamento, refeita ao repetir a chamada.
		a.log.WarnContext(r.Context(), "sessões do link revogado não removidas do armazenamento",
			slog.String("error", res.SessionCleanupErr.Error()))
	}
	w.WriteHeader(http.StatusNoContent)
}

// portalAccess troca o token do link por uma sessão do portal e redireciona
// para a interface. O link é de uso único.
func (a *API) portalAccess(w http.ResponseWriter, r *http.Request) {
	if a.Portal == nil {
		a.writeError(w, r, apperr.Unavailable("portal_unavailable", "Portal do cliente indisponível."))
		return
	}
	s, err := a.Portal.Exchange(r.Context(), r.URL.Query().Get("token"), "ip:"+clientIP(r))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.setPortalCookie(w, s)
	http.Redirect(w, r, "/portal", http.StatusFound)
}

type portalSessionDTO struct {
	Customer struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"customer"`
	Organization struct {
		Name string `json:"name"`
	} `json:"organization"`
	CSRFToken string       `json:"csrf_token"`
	Contacts  []contactDTO `json:"contacts"`
}

func portalSessionView(v portal.View) portalSessionDTO {
	var out portalSessionDTO
	out.Customer.ID, out.Customer.Name = v.Session.CustomerID.String(), v.CustomerName
	out.Organization.Name = v.OrganizationName
	out.CSRFToken = v.Session.CSRFToken
	out.Contacts = make([]contactDTO, 0, len(v.Contacts))
	for _, c := range v.Contacts {
		out.Contacts = append(out.Contacts, contDTO(c))
	}
	return out
}

func (a *API) portalSession(w http.ResponseWriter, r *http.Request) {
	v, err := a.Portal.View(r.Context(), mustPortalSession(r))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, portalSessionView(v))
}

type portalInvoiceLineDTO struct {
	Description string `json:"description"`
	Amount      int64  `json:"amount"`
}

type portalInvoiceDTO struct {
	ID          uuid.UUID              `json:"id"`
	Number      string                 `json:"number"`
	Status      string                 `json:"status"`
	Currency    string                 `json:"currency"`
	Total       int64                  `json:"total"`
	AmountDue   int64                  `json:"amount_due"`
	IssuedAt    *time.Time             `json:"issued_at"`
	DueDate     *string                `json:"due_date"`
	Overdue     bool                   `json:"overdue"`
	CheckoutURL *string                `json:"checkout_url"`
	Items       []portalInvoiceLineDTO `json:"items"`
}

// portalInvoiceView expõe ao cliente apenas o necessário: sem histórico,
// identificadores internos, cobranças nem dados de outros clientes. Os itens
// trazem o valor líquido do desconto (a soma é o total da fatura).
func portalInvoiceView(inv portal.Invoice) (portalInvoiceDTO, error) {
	lines, err := inv.Lines()
	if err != nil {
		return portalInvoiceDTO{}, err
	}
	out := portalInvoiceDTO{ID: inv.ID, Number: inv.Number, Status: string(inv.Status), Currency: inv.Currency.Code(), Total: inv.Total.Amount(),
		AmountDue: inv.AmountDue.Amount(), IssuedAt: inv.IssuedAt, Overdue: inv.Overdue, Items: make([]portalInvoiceLineDTO, 0, len(lines))}
	if inv.CheckoutURL != "" {
		u := inv.CheckoutURL
		out.CheckoutURL = &u
	}
	if inv.DueDate != nil {
		d := inv.DueDate.String()
		out.DueDate = &d
	}
	for _, l := range lines {
		out.Items = append(out.Items, portalInvoiceLineDTO{Description: l.Description, Amount: l.Amount.Amount()})
	}
	return out, nil
}

// portalInvoices lista as faturas do cliente da sessão em páginas. O cursor
// é apenas uma posição: cliente e organização vêm da sessão do portal.
func (a *API) portalInvoices(w http.ResponseWriter, r *http.Request) {
	limit, err := queryLimit(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	page, err := a.Portal.Invoices(r.Context(), mustPortalSession(r), portal.PageQuery{Limit: limit, Cursor: r.URL.Query().Get("cursor")})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]portalInvoiceDTO, 0, len(page.Items))
	for _, inv := range page.Items {
		d, err := portalInvoiceView(inv)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		out = append(out, d)
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, page.NextCursor))
}

func (a *API) portalInvoiceDetail(w http.ResponseWriter, r *http.Request) (portal.InvoiceDetail, bool) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return portal.InvoiceDetail{}, false
	}
	d, err := a.Portal.Invoice(r.Context(), mustPortalSession(r), id)
	if err != nil {
		a.writeError(w, r, err)
		return portal.InvoiceDetail{}, false
	}
	return d, true
}

func (a *API) portalInvoice(w http.ResponseWriter, r *http.Request) {
	d, ok := a.portalInvoiceDetail(w, r)
	if !ok {
		return
	}
	out, err := portalInvoiceView(d.Invoice)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, out)
}

// portalInvoiceDocument devolve o mesmo documento comercial do painel,
// restrito às faturas emitidas do cliente da sessão.
func (a *API) portalInvoiceDocument(w http.ResponseWriter, r *http.Request) {
	d, ok := a.portalInvoiceDetail(w, r)
	if !ok {
		return
	}
	a.writeInvoiceDocument(w, r, invoiceDocumentData{Org: d.OrganizationName, Invoice: d.Invoice.Invoice, Location: d.Location,
		CheckoutURL: d.CheckoutURL, CustomerFacing: true})
}

func (a *API) portalUpdateContactPreferences(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Contacts []struct {
			ID                   uuid.UUID `json:"id"`
			ReceivesBillingEmail *bool     `json:"receives_billing_email"`
		} `json:"contacts"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	if in.Contacts == nil {
		a.writeError(w, r, invalidField("contacts", "Informe a lista de contatos."))
		return
	}
	prefs := make([]portal.ContactPreference, 0, len(in.Contacts))
	for _, c := range in.Contacts {
		if c.ReceivesBillingEmail == nil {
			a.writeError(w, r, invalidField("contacts", "Informe receives_billing_email para cada contato."))
			return
		}
		prefs = append(prefs, portal.ContactPreference{ContactID: c.ID, ReceivesBillingEmail: *c.ReceivesBillingEmail})
	}
	v, err := a.Portal.UpdateContactPreferences(r.Context(), mustPortalSession(r), prefs)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, portalSessionView(v))
}

func (a *API) portalLogout(w http.ResponseWriter, r *http.Request) {
	if err := a.Portal.Logout(r.Context(), mustPortalSession(r)); err != nil {
		a.writeError(w, r, err)
		return
	}
	a.clearPortalCookie(w)
	w.WriteHeader(http.StatusNoContent)
}
