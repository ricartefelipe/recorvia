package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// Principal é a identidade autenticada da requisição: um usuário com sessão
// do navegador ou uma credencial de API (Credential preenchida, sem usuário
// nem sessão).
type Principal struct {
	User          identity.User
	Session       identity.Session
	Credential    *apicredentials.Principal
	TenantID      uuid.UUID
	Role          tenancy.Role
	PlatformStaff bool
}

// Can informa se o papel (ou os escopos da credencial) concede a permissão.
func (p Principal) Can(perm tenancy.Permission) bool {
	if p.Credential != nil {
		return p.Credential.Can(perm)
	}
	return p.Role.Can(perm)
}

// Actor descreve o executor para a auditoria.
func (p Principal) Actor() audit.Actor {
	if p.Credential != nil {
		return p.Credential.Actor()
	}
	return audit.User(p.User.ID, p.User.DisplayName)
}

// UserID devolve o usuário executor para colunas de autoria; nil quando a
// requisição é feita por credencial de API (a autoria fica na auditoria).
func (p Principal) UserID() *uuid.UUID {
	if p.Credential != nil || p.User.ID == uuid.Nil {
		return nil
	}
	id := p.User.ID
	return &id
}

// ClientID identifica o cliente da API no escopo das chaves de idempotência.
func (p Principal) ClientID() string {
	if p.Credential != nil {
		return p.Credential.ClientID()
	}
	return "user:" + p.User.ID.String()
}

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

// loginCookieName é o cookie que vincula o fluxo de login ao navegador que o
// iniciou; herda o prefixo __Host- do cookie de sessão.
func (a *API) loginCookieName() string { return a.cookieName() + "_login" }

// setLoginCookie grava o vínculo do login. SameSite=Lax porque o callback
// chega por navegação vinda do provedor de identidade (outro site); dura o
// mesmo que o fluxo de login.
func (a *API) setLoginCookie(w http.ResponseWriter, binding string, ttl time.Duration) {
	secure := a.Config == nil || a.Config.CookieSecure
	http.SetCookie(w, &http.Cookie{Name: a.loginCookieName(), Value: binding, Path: "/", HttpOnly: true, Secure: secure, //nolint:gosec // Secure segue a configuração
		SameSite: http.SameSiteLaxMode, MaxAge: max(int(ttl.Seconds()), 1)})
}

func (a *API) clearLoginCookie(w http.ResponseWriter) {
	secure := a.Config == nil || a.Config.CookieSecure
	http.SetCookie(w, &http.Cookie{Name: a.loginCookieName(), Value: "", Path: "/", HttpOnly: true, Secure: secure, //nolint:gosec // Secure segue a configuração
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// loginBinding devolve o vínculo de login do cookie, quando bem formado.
func (a *API) loginBinding(r *http.Request) string {
	if c, err := r.Cookie(a.loginCookieName()); err == nil && identity.ValidBrowserBinding(c.Value) {
		return c.Value
	}
	return ""
}

func (a *API) clearSessionCookie(w http.ResponseWriter) {
	secure := a.Config == nil || a.Config.CookieSecure
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: "", Path: "/", HttpOnly: true, Secure: secure, //nolint:gosec // Secure segue a configuração
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// authenticate valida a credencial de API (Authorization: Bearer rcv_…) ou a
// sessão do cookie. Um token do Recorvia tem precedência sobre o cookie. Um
// Authorization em outro formato só é tratado como credencial quando não há
// cookie de sessão: com cookie, ele é ignorado, porque o navegador repete por
// conta própria credenciais de outra origem (por exemplo, HTTP Basic de um
// proxy à frente da aplicação) em todas as requisições do painel.
func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(a.cookieName())
		hasSession := err == nil && c.Value != ""
		if auth := r.Header.Get("Authorization"); auth != "" && (!hasSession || recorviaBearer(auth)) {
			a.authenticateCredential(w, r, auth, next)
			return
		}
		if !hasSession {
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

// bearerToken extrai o token do header Authorization (esquema Bearer, sem
// diferenciar maiúsculas no nome do esquema).
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// recorviaBearer informa se o header traz um token do Recorvia (esquema
// Bearer e prefixo rcv_), mesmo que malformado.
func recorviaBearer(header string) bool {
	token, ok := bearerToken(header)
	return ok && strings.HasPrefix(token, apicredentials.TokenPrefix)
}

// authenticateCredential autentica a requisição por credencial de API. O
// tenant é o da credencial (um X-Recorvia-Tenant divergente gera 409 em
// requireTenant); não há sessão, por isso a verificação CSRF não se aplica:
// navegadores não enviam Authorization: Bearer por conta própria.
func (a *API) authenticateCredential(w http.ResponseWriter, r *http.Request, header string, next http.Handler) {
	token, ok := bearerToken(header)
	if !ok || a.Credentials == nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="recorvia"`)
		a.writeError(w, r, apperr.Unauthenticated("api_credential_invalid", "Credencial de API inválida, expirada ou revogada."))
		return
	}
	cp, err := a.Credentials.Authenticate(r.Context(), token, "ip:"+clientIP(r))
	if err != nil {
		if apperr.IsKind(err, apperr.KindUnauthenticated) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="recorvia", error="invalid_token"`)
		}
		a.writeError(w, r, err)
		return
	}
	ctx := logging.WithAttrs(r.Context(), slog.String("principal", cp.ClientID()))
	next.ServeHTTP(w, r.WithContext(withPrincipal(ctx, Principal{Credential: &cp, TenantID: cp.TenantID})))
}

// requireUserSession restringe a rota a usuários com sessão do navegador
// (operações de conta e de sessão não valem para credenciais de API).
func (a *API) requireUserSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := principalFrom(r.Context()); ok && p.Credential != nil {
			a.writeError(w, r, apperr.Forbidden("user_session_required", "Esta operação exige a sessão de um usuário; credenciais de API não são aceitas."))
			return
		}
		next.ServeHTTP(w, r)
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

// authLogin inicia o login vinculado ao navegador: o cookie de vínculo é
// reaproveitado enquanto vale, para que logins em abas simultâneas do mesmo
// navegador continuem válidos.
func (a *API) authLogin(w http.ResponseWriter, r *http.Request) {
	binding := a.loginBinding(r)
	if binding == "" {
		binding = identity.NewBrowserBinding()
	}
	target, err := a.Identity.BeginLogin(r.Context(), r.URL.Query().Get("return_to"), binding)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.setLoginCookie(w, binding, a.Identity.LoginFlowTTL())
	// O destino é a URL de autorização montada pelo próprio adaptador OIDC.
	http.Redirect(w, r, target, http.StatusFound) //nolint:gosec // destino gerado pelo servidor
}

func (a *API) authCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		a.writeError(w, r, apperr.Unauthenticated("login_denied", "Login cancelado ou recusado pelo provedor de identidade."))
		return
	}
	sess, user, returnTo, err := a.Identity.CompleteLogin(r.Context(), q.Get("state"), q.Get("code"), a.loginBinding(r))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.clearLoginCookie(w)
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

func (a *API) postLogoutURL() string {
	if a.Config != nil && a.Config.PublicURL != nil {
		return a.Config.PublicURL.String()
	}
	return "/"
}

// authLogout encerra a sessão. O endereço devolvido leva o navegador ao
// encerramento da sessão no provedor com client_id e
// post_logout_redirect_uri; o refresh token é revogado pelo servidor e
// nenhum token do provedor vai para o navegador.
func (a *API) authLogout(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	res, err := a.Identity.Logout(r.Context(), p.Session, a.postLogoutURL())
	a.finishLogout(w, r, res, err)
}

// authLogoutAll encerra todas as sessões do usuário, em qualquer
// navegador, e revoga no provedor os refresh tokens de cada uma.
func (a *API) authLogoutAll(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	res, err := a.Identity.LogoutAll(r.Context(), p.Session, a.postLogoutURL())
	a.finishLogout(w, r, res, err)
}

func (a *API) finishLogout(w http.ResponseWriter, r *http.Request, res identity.LogoutResult, err error) {
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if res.ProviderErr != nil {
		// A sessão local já foi encerrada; o provedor expira o token sozinho.
		a.log.WarnContext(r.Context(), "revogação no provedor de identidade falhou", slog.String("error", res.ProviderErr.Error()))
	}
	a.clearSessionCookie(w)
	a.writeJSON(w, r, http.StatusOK, map[string]string{"redirect_url": res.RedirectURL})
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
	base := ""
	if a.Config != nil && a.Config.PublicURL != nil {
		base = a.Config.PublicURL.String()
	}
	return tenancy.AcceptURL(base, token)
}
