package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// Principal é a identidade autenticada da requisição.
type Principal struct {
	User          identity.User
	Session       identity.Session
	TenantID      uuid.UUID
	Role          tenancy.Role
	PlatformStaff bool
}

// Can informa se o papel concede a permissão.
func (p Principal) Can(perm tenancy.Permission) bool { return p.Role.Can(perm) }

// Actor descreve o executor para a auditoria.
func (p Principal) Actor() audit.Actor { return audit.User(p.User.ID, p.User.DisplayName) }

// UserRef descreve o usuário para os casos de uso de tenancy.
func (p Principal) UserRef() tenancy.UserRef {
	return tenancy.UserRef{ID: p.User.ID, Email: p.User.Email, DisplayName: p.User.DisplayName}
}

type principalKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func principalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

func mustPrincipal(r *http.Request) Principal {
	p, _ := principalFrom(r.Context())
	return p
}

func (a *API) cookieName() string {
	if a.Config != nil && a.Config.CookieName != "" {
		return a.Config.CookieName
	}
	return "recorvia_session"
}

func (a *API) setSessionCookie(w http.ResponseWriter, s identity.Session) {
	secure := a.Config == nil || a.Config.CookieSecure
	maxAge := int(s.ExpiresAt.Sub(a.Clock.Now()).Seconds())
	// Secure vem da configuração: obrigatório em produção (validado na
	// inicialização) e desligado apenas no ambiente local em HTTP.
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: s.ID, Path: "/", HttpOnly: true, Secure: secure, //nolint:gosec // ver comentário acima
		SameSite: http.SameSiteLaxMode, MaxAge: max(maxAge, 1)})
}

func (a *API) clearSessionCookie(w http.ResponseWriter) {
	secure := a.Config == nil || a.Config.CookieSecure
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: "", Path: "/", HttpOnly: true, Secure: secure, //nolint:gosec // Secure segue a configuração
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// authenticate valida a sessão do cookie.
func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(a.cookieName())
		if err != nil || c.Value == "" {
			a.writeError(w, r, apperr.Unauthenticated("session_required", "Entre para continuar."))
			return
		}
		sess, user, err := a.Identity.Authenticate(r.Context(), c.Value)
		if err != nil {
			if apperr.IsKind(err, apperr.KindUnauthenticated) {
				a.clearSessionCookie(w)
			}
			a.writeError(w, r, err)
			return
		}
		staff, err := a.Identity.IsPlatformStaff(r.Context(), user.ID)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		ctx := logging.WithAttrs(r.Context(), slog.String("user_id", user.ID.String()))
		next.ServeHTTP(w, r.WithContext(withPrincipal(ctx, Principal{User: user, Session: sess, PlatformStaff: staff})))
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// sameOrigin confere Origin (quando presente) contra a URL pública e as
// origens permitidas.
func (a *API) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || a.Config == nil || a.Config.PublicURL == nil {
		return true
	}
	if origin == a.Config.PublicURL.Scheme+"://"+a.Config.PublicURL.Host {
		return true
	}
	for _, o := range a.Config.AllowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// csrf exige o token da sessão em métodos que alteram estado.
func (a *API) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFrom(r.Context())
		if ok && p.Session.ID != "" && !safeMethod(r.Method) {
			if !a.sameOrigin(r) || !p.Session.ValidCSRF(r.Header.Get("X-CSRF-Token")) {
				a.writeError(w, r, apperr.Forbidden("csrf_invalid", "Requisição recusada: token CSRF ausente ou inválido. Recarregue a página."))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireTenant valida a organização ativa contra a associação do usuário
// em toda requisição e verifica a permissão da rota.
func (a *API) requireTenant(perm tenancy.Permission, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFrom(r.Context())
		if !ok {
			a.writeError(w, r, apperr.Unauthenticated("session_required", "Entre para continuar."))
			return
		}
		if p.TenantID == uuid.Nil {
			if p.Session.ActiveTenantID == nil {
				a.writeError(w, r, apperr.Forbidden("no_active_tenant", "Selecione uma organização."))
				return
			}
			access, err := a.Tenancy.ResolveAccess(r.Context(), *p.Session.ActiveTenantID, p.User.ID)
			if err != nil {
				a.writeError(w, r, err)
				return
			}
			p.TenantID, p.Role = access.TenantID, access.Role
		}
		if h := r.Header.Get("X-Recorvia-Tenant"); h != "" && h != p.TenantID.String() {
			a.writeError(w, r, apperr.Conflict("tenant_mismatch", "A organização ativa mudou em outra aba. Recarregue a página."))
			return
		}
		if perm != "" && !p.Can(perm) {
			a.writeError(w, r, apperr.Forbidden("forbidden", "Você não tem permissão para esta ação."))
			return
		}
		ctx := logging.WithAttrs(r.Context(), slog.String("tenant_id", p.TenantID.String()))
		next.ServeHTTP(w, r.WithContext(withPrincipal(ctx, p)))
	})
}

func (a *API) requireStaff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFrom(r.Context())
		if !ok || !p.PlatformStaff {
			a.writeError(w, r, apperr.Forbidden("forbidden", "Acesso restrito à equipe da plataforma."))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) authLogin(w http.ResponseWriter, r *http.Request) {
	target, err := a.Identity.BeginLogin(r.Context(), r.URL.Query().Get("return_to"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	// O destino é a URL de autorização montada pelo próprio adaptador OIDC.
	http.Redirect(w, r, target, http.StatusFound) //nolint:gosec // destino gerado pelo servidor
}

func (a *API) authCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		a.writeError(w, r, apperr.Unauthenticated("login_denied", "Login cancelado ou recusado pelo provedor de identidade."))
		return
	}
	sess, user, returnTo, err := a.Identity.CompleteLogin(r.Context(), q.Get("state"), q.Get("code"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	// Com uma única organização, ela já fica ativa (sessão rotacionada).
	orgs, err := a.Tenancy.UserOrganizations(r.Context(), user.ID)
	if err == nil && len(orgs) == 1 {
		if next, err := a.Identity.SwitchTenant(r.Context(), sess, orgs[0].TenantID, a.accessChecker); err == nil {
			sess = next
		}
	}
	a.setSessionCookie(w, sess)
	// returnTo foi saneado por identity.SafeReturnPath (somente caminho relativo).
	http.Redirect(w, r, returnTo, http.StatusFound) //nolint:gosec // caminho relativo saneado
}

func (a *API) accessChecker(ctx context.Context, tenantID, userID uuid.UUID) error {
	_, err := a.Tenancy.ResolveAccess(ctx, tenantID, userID)
	return err
}

func (a *API) authLogout(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	post := "/"
	if a.Config != nil && a.Config.PublicURL != nil {
		post = a.Config.PublicURL.String()
	}
	target, err := a.Identity.Logout(r.Context(), p.Session, post)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.clearSessionCookie(w)
	a.writeJSON(w, r, http.StatusOK, map[string]string{"redirect_url": target})
}

type orgSummaryDTO struct {
	ID     uuid.UUID    `json:"id"`
	Name   string       `json:"name"`
	Slug   string       `json:"slug"`
	Role   tenancy.Role `json:"role"`
	Status string       `json:"status"`
}

type activeTenantDTO struct {
	orgSummaryDTO
	Permissions []tenancy.Permission `json:"permissions"`
	Timezone    string               `json:"timezone"`
}

type sessionDTO struct {
	User struct {
		ID          uuid.UUID `json:"id"`
		Email       string    `json:"email"`
		DisplayName string    `json:"display_name"`
	} `json:"user"`
	CSRFToken     string           `json:"csrf_token"`
	PlatformStaff bool             `json:"platform_staff"`
	Organizations []orgSummaryDTO  `json:"organizations"`
	ActiveTenant  *activeTenantDTO `json:"active_tenant"`
}

func (a *API) sessionView(ctx context.Context, p Principal) (sessionDTO, error) {
	var out sessionDTO
	out.User.ID, out.User.Email, out.User.DisplayName = p.User.ID, p.User.Email, p.User.DisplayName
	out.CSRFToken, out.PlatformStaff = p.Session.CSRFToken, p.PlatformStaff
	orgs, err := a.Tenancy.UserOrganizations(ctx, p.User.ID)
	if err != nil {
		return out, err
	}
	out.Organizations = make([]orgSummaryDTO, 0, len(orgs))
	for _, o := range orgs {
		out.Organizations = append(out.Organizations, orgSummaryDTO{ID: o.TenantID, Name: o.TenantName, Slug: o.TenantSlug, Role: o.Role, Status: string(o.TenantStatus)})
	}
	if p.Session.ActiveTenantID != nil {
		access, err := a.Tenancy.ResolveAccess(ctx, *p.Session.ActiveTenantID, p.User.ID)
		if err == nil {
			t, err := a.Tenancy.Organization(ctx, access.TenantID)
			if err != nil {
				return out, err
			}
			out.ActiveTenant = &activeTenantDTO{orgSummaryDTO: orgSummaryDTO{ID: t.ID, Name: t.Name, Slug: t.Slug, Role: access.Role, Status: string(t.Status)},
				Permissions: access.Role.Permissions(), Timezone: t.Timezone}
		} else if !apperr.IsKind(err, apperr.KindForbidden) {
			return out, err
		}
	}
	return out, nil
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	view, err := a.sessionView(r.Context(), mustPrincipal(r))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, view)
}

func (a *API) switchTenant(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TenantID uuid.UUID `json:"tenant_id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	next, err := a.Identity.SwitchTenant(r.Context(), p.Session, in.TenantID, a.accessChecker)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.setSessionCookie(w, next)
	p.Session = next
	view, err := a.sessionView(r.Context(), p)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, view)
}

func (a *API) createOrganization(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name             string `json:"name"`
		Slug             string `json:"slug"`
		LegalName        string `json:"legal_name"`
		DocumentNumber   string `json:"document_number"`
		Timezone         string `json:"timezone"`
		InvoicePrefix    string `json:"invoice_prefix"`
		PaymentTermsDays *int   `json:"payment_terms_days"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	t, err := a.Tenancy.Onboard(r.Context(), p.UserRef(), tenancy.TenantInput{Name: in.Name, Slug: in.Slug, LegalName: in.LegalName,
		DocumentNumber: in.DocumentNumber, Timezone: in.Timezone, InvoicePrefix: in.InvoicePrefix, PaymentTermsDays: in.PaymentTermsDays})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	next, err := a.Identity.SwitchTenant(r.Context(), p.Session, t.ID, a.accessChecker)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.setSessionCookie(w, next)
	p.Session = next
	view, err := a.sessionView(r.Context(), p)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, view)
}

func (a *API) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	m, err := a.Tenancy.AcceptInvitation(r.Context(), mustPrincipal(r).UserRef(), in.Token)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, map[string]any{"tenant_id": m.TenantID, "role": m.Role})
}

func (a *API) adminTenants(w http.ResponseWriter, r *http.Request) {
	rows, err := a.Store.Tenancy().PlatformCatalog(r.Context())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, t := range rows {
		item := map[string]any{"id": t.TenantID, "name": t.Name, "slug": t.Slug, "status": t.Status, "member_count": t.MemberCount,
			"created_at": t.CreatedAt, "plan_code": nullable(t.PlanCode), "platform_status": nullable(t.PlatformStatus)}
		out = append(out, item)
	}
	a.writeJSON(w, r, http.StatusOK, map[string]any{"data": out})
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// acceptURL monta o link de aceite de convite.
func (a *API) acceptURL(token string) string {
	base := "/"
	if a.Config != nil && a.Config.PublicURL != nil {
		base = strings.TrimRight(a.Config.PublicURL.String(), "/") + "/"
	}
	return base + "convite?token=" + url.QueryEscape(token)
}
