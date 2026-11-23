//go:build integration

package httpapi_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// flowIDP simula o provedor OIDC para o fluxo completo de login: o código
// identifica as claims do usuário; o ID token e o refresh token derivam do
// código. Registra as revogações feitas pelo servidor.
type flowIDP struct {
	mu      sync.Mutex
	claims  map[string]identity.Claims
	revoked []string
	expiry  time.Time
}

func newFlowIDP(expiry time.Time) *flowIDP {
	return &flowIDP{claims: map[string]identity.Claims{}, expiry: expiry}
}

func (p *flowIDP) user(code string, c identity.Claims) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.claims[code] = c
}

func (p *flowIDP) NewPKCEVerifier() string { return "verificador-pkce" }

func (p *flowIDP) AuthCodeURL(state, nonce, _ string) string {
	return "https://idp.test/auth?state=" + url.QueryEscape(state) + "&nonce=" + url.QueryEscape(nonce)
}

func (p *flowIDP) Exchange(_ context.Context, code, verifier string) (identity.Tokens, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.claims[code]; !ok || verifier != "verificador-pkce" {
		return identity.Tokens{}, errors.New("código inválido")
	}
	return identity.Tokens{IDToken: "idt-" + code, AccessToken: "at-" + code, RefreshToken: "rt-" + code, Expiry: p.expiry}, nil
}

func (p *flowIDP) VerifyIDToken(_ context.Context, raw, _ string) (identity.Claims, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.claims[strings.TrimPrefix(raw, "idt-")]
	if !ok {
		return identity.Claims{}, errors.New("id_token inválido")
	}
	return c, nil
}

func (p *flowIDP) Refresh(context.Context, string) (identity.Tokens, error) {
	return identity.Tokens{}, errors.New("renovação não esperada")
}

func (p *flowIDP) EndSessionURL(redirect string) string {
	return "https://idp.test/logout?client_id=recorvia-web&post_logout_redirect_uri=" + url.QueryEscape(redirect)
}

func (p *flowIDP) RevokeTokens(_ context.Context, t identity.Tokens) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t.RefreshToken != "" {
		p.revoked = append(p.revoked, t.RefreshToken)
	}
	return nil
}

func (p *flowIDP) revokedTokens() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.revoked...)
}

func claimsOf(u identity.User) identity.Claims {
	return identity.Claims{Issuer: u.Issuer, Subject: u.Subject, Email: u.Email, EmailVerified: true, Name: u.DisplayName}
}

// browserResp é o que o navegador simulado observa de uma resposta.
type browserResp struct {
	StatusCode int
	Header     http.Header
}

// get faz a requisição como o navegador simulado, sem seguir
// redirecionamentos, para que o teste confira o destino e os cookies.
func (a *apiEnv) get(t *testing.T, path string, cookies ...*http.Cookie) browserResp {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, a.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return browserResp{StatusCode: resp.StatusCode, Header: resp.Header}
}

// responseCookie extrai um cookie dos headers da resposta.
func responseCookie(r browserResp, name string) *http.Cookie {
	for _, c := range (&http.Response{Header: r.Header}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// sessionCookie extrai o cookie de sessão do painel dos headers da resposta.
func sessionCookie(r browserResp) *http.Cookie { return responseCookie(r, "recorvia_session") }

// loginCookie extrai o cookie que vincula o login ao navegador.
func loginCookie(r browserResp) *http.Cookie { return responseCookie(r, "recorvia_session_login") }

// startLogin percorre /auth/login e devolve o state enviado ao provedor e o
// cookie de vínculo do navegador.
func (a *apiEnv) startLogin(t *testing.T, returnTo string, cookies ...*http.Cookie) (string, *http.Cookie) {
	t.Helper()
	resp := a.get(t, "/auth/login?return_to="+url.QueryEscape(returnTo), cookies...)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Host != "idp.test" || loc.Query().Get("state") == "" || loc.Query().Get("nonce") == "" {
		t.Fatalf("redirecionamento ao provedor inesperado: %s", resp.Header.Get("Location"))
	}
	ck := loginCookie(resp)
	if ck == nil || ck.Value == "" {
		t.Fatal("o login deve gravar o cookie de vínculo do navegador")
	}
	return loc.Query().Get("state"), ck
}

// callback chama /auth/callback com o state, o código e os cookies dados.
func (a *apiEnv) callback(t *testing.T, state, code string, cookies ...*http.Cookie) browserResp {
	t.Helper()
	return a.get(t, "/auth/callback?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), cookies...)
}

// browserLogin percorre /auth/login e /auth/callback no mesmo navegador e
// devolve a resposta do callback.
func (a *apiEnv) browserLogin(t *testing.T, code, returnTo string) browserResp {
	t.Helper()
	state, ck := a.startLogin(t, returnTo)
	return a.callback(t, state, code, &http.Cookie{Name: ck.Name, Value: ck.Value})
}

// cookieClient monta o cliente da API para o cookie recebido, com o CSRF e
// a organização ativa da sessão.
func (a *apiEnv) cookieClient(t *testing.T, cookie string) (*client, map[string]any) {
	t.Helper()
	c := &client{a: a, cookie: cookie}
	status, body, _ := c.do(http.MethodGet, "/api/v1/session", nil)
	if status != http.StatusOK {
		t.Fatalf("sessão do cookie: %d %v", status, body)
	}
	c.csrf, _ = body["csrf_token"].(string)
	if at, ok := body["active_tenant"].(map[string]any); ok {
		c.tenant, _ = at["id"].(string)
	}
	return c, body
}

// O fluxo do navegador: login com PKCE, callback com cookie HttpOnly e
// SameSite=Lax, seleção automática da única organização, logout sem tokens
// no endereço devolvido e com revogação no provedor pelo servidor.
func TestLoginCallbackAndLogoutOverHTTP(t *testing.T) {
	idp := newFlowIDP(time.Now().Add(24 * time.Hour))
	a := newAPI(t, func(s *apiSetup) { s.idp = idp })
	idp.expiry = a.env.Clock.Now().Add(24 * time.Hour)
	tn := a.env.NewTenant(t, "login")
	idp.user("dono", claimsOf(tn.Owner))

	resp := a.browserLogin(t, "dono", "/faturas?status=open")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/faturas?status=open" {
		t.Fatalf("callback: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	ck := sessionCookie(resp)
	if ck == nil || ck.Value == "" || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" || ck.Secure ||
		ck.MaxAge <= 0 || ck.MaxAge > int((12*time.Hour).Seconds()) {
		t.Fatalf("atributos do cookie do painel: %+v", ck)
	}
	if strings.Contains(ck.Value, "idt-") || strings.Contains(ck.Value, "rt-") {
		t.Fatal("o cookie é um identificador opaco, não um token do provedor")
	}
	owner, view := a.cookieClient(t, ck.Value)
	if owner.tenant != tn.ID.String() {
		t.Fatalf("com uma única organização, ela fica ativa no login: %v", view["active_tenant"])
	}
	if status, _, _ := owner.do(http.MethodGet, "/api/v1/organization", nil); status != http.StatusOK {
		t.Fatalf("organização ativa acessível: %d", status)
	}

	// Callback recusado ou repetido não cria sessão.
	if resp := a.get(t, "/auth/callback?error=access_denied&state=x"); resp.StatusCode != http.StatusUnauthorized || sessionCookie(resp) != nil {
		t.Fatalf("login recusado no provedor: %d", resp.StatusCode)
	}
	if resp := a.get(t, "/auth/callback?state=desconhecido&code=dono"); resp.StatusCode != http.StatusUnauthorized || sessionCookie(resp) != nil {
		t.Fatalf("estado desconhecido: %d", resp.StatusCode)
	}
	if resp := a.browserLogin(t, "dono", "https://malicioso.test/"); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("destino externo depois do login deve virar a raiz da aplicação: %s", resp.Header.Get("Location"))
	}

	// Logout: exige CSRF; o endereço devolvido não carrega tokens.
	noCSRF := &client{a: a, cookie: owner.cookie}
	if status, body, _ := noCSRF.do(http.MethodPost, "/auth/logout", nil); status != http.StatusForbidden || body["code"] != "csrf_invalid" {
		t.Fatalf("logout sem CSRF: %d %v", status, body)
	}
	status, body, hdr := owner.do(http.MethodPost, "/auth/logout", nil)
	if status != http.StatusOK {
		t.Fatalf("logout: %d %v", status, body)
	}
	target, _ := body["redirect_url"].(string)
	if strings.Contains(target, "id_token") || strings.Contains(target, "idt-") || strings.Contains(target, "rt-") ||
		!strings.Contains(target, "client_id=recorvia-web") || !strings.Contains(target, "post_logout_redirect_uri=") {
		t.Fatalf("endereço de logout não pode levar tokens ao navegador: %s", target)
	}
	cleared := sessionCookie(browserResp{Header: hdr})
	if cleared == nil || cleared.MaxAge >= 0 || cleared.Value != "" || !cleared.HttpOnly {
		t.Fatalf("o cookie deve ser removido: %+v", cleared)
	}
	if got := idp.revokedTokens(); len(got) != 1 || got[0] != "rt-dono" {
		t.Fatalf("o refresh token deve ser revogado pelo servidor: %v", got)
	}
	if status, _, _ := owner.do(http.MethodGet, "/api/v1/session", nil); status != http.StatusUnauthorized {
		t.Fatalf("sessão encerrada: %d", status)
	}
}

// Seleção no login: quem participa de duas organizações escolhe depois;
// quem não participa de nenhuma segue sem organização ativa; e-mail não
// verificado é recusado.
func TestCallbackOrganizationSelection(t *testing.T) {
	idp := newFlowIDP(time.Now().Add(24 * time.Hour))
	a := newAPI(t, func(s *apiSetup) { s.idp = idp })
	idp.expiry = a.env.Clock.Now().Add(24 * time.Hour)
	tnA, tnB := a.env.NewTenant(t, "sel-a"), a.env.NewTenant(t, "sel-b")
	both := member(t, a.env, tnA, tenancy.RoleFinance)
	if _, err := a.env.Tenancy.AcceptInvitation(context.Background(), tenancy.UserRef{ID: both.ID, Email: both.Email}, inviteToken(t, a.env, tnB, both.Email)); err != nil {
		t.Fatal(err)
	}
	idp.user("duas", claimsOf(both))
	uid := ids.New().String()
	idp.user("nova", identity.Claims{Issuer: "https://idp.test", Subject: uid, Email: uid[24:] + "@nova.test", EmailVerified: true, Name: "Nova"})
	idp.user("sem-verificar", identity.Claims{Issuer: "https://idp.test", Subject: ids.New().String(), Email: "x@nova.test", Name: "X"})

	resp := a.browserLogin(t, "duas", "/")
	c, view := a.cookieClient(t, sessionCookie(resp).Value)
	if c.tenant != "" || len(view["organizations"].([]any)) != 2 {
		t.Fatalf("com duas organizações, nenhuma fica ativa: %v", view)
	}
	resp = a.browserLogin(t, "nova", "/")
	if c, view := a.cookieClient(t, sessionCookie(resp).Value); c.tenant != "" || len(view["organizations"].([]any)) != 0 {
		t.Fatalf("usuário novo sem organização: %v", view)
	}
	if resp := a.browserLogin(t, "sem-verificar", "/"); resp.StatusCode != http.StatusForbidden || sessionCookie(resp) != nil {
		t.Fatalf("e-mail não verificado: %d", resp.StatusCode)
	}
}

func TestSessionCookieSecureFollowsConfiguration(t *testing.T) {
	idp := newFlowIDP(time.Now().Add(24 * time.Hour))
	a := newAPI(t, func(s *apiSetup) {
		s.idp = idp
		s.cfg.CookieSecure = true
	})
	idp.expiry = a.env.Clock.Now().Add(24 * time.Hour)
	tn := a.env.NewTenant(t, "seguro")
	idp.user("dono", claimsOf(tn.Owner))
	ck := sessionCookie(a.browserLogin(t, "dono", "/"))
	if ck == nil || !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie com Secure: %+v", ck)
	}
	if _, lc := a.startLogin(t, "/"); !lc.Secure || !lc.HttpOnly {
		t.Fatalf("cookie de vínculo do login com Secure: %+v", lc)
	}
}

// Login CSRF: quem ataca inicia um login, entra no provedor com a própria
// conta e leva outra pessoa ao callback com o state e o código dele. Sem o
// cookie de vínculo do navegador que iniciou o login, o callback é recusado,
// nenhuma sessão é criada e o state fica inutilizado.
func TestLoginCallbackRequiresTheBrowserThatStartedIt(t *testing.T) {
	idp := newFlowIDP(time.Now().Add(24 * time.Hour))
	a := newAPI(t, func(s *apiSetup) { s.idp = idp })
	idp.expiry = a.env.Clock.Now().Add(24 * time.Hour)
	attacker := a.env.NewTenant(t, "atacante")
	idp.user("atacante", claimsOf(attacker.Owner))

	state, attackerCookie := a.startLogin(t, "/")
	if !attackerCookie.HttpOnly || attackerCookie.SameSite != http.SameSiteLaxMode || attackerCookie.Path != "/" ||
		attackerCookie.MaxAge <= 0 || attackerCookie.MaxAge > int((10*time.Minute).Seconds()) {
		t.Fatalf("atributos do cookie de vínculo: %+v", attackerCookie)
	}
	// O navegador da vítima: sem cookie de vínculo, ou com o vínculo de um
	// login que ela mesma iniciou.
	_, victimCookie := a.startLogin(t, "/")
	for name, cookies := range map[string][]*http.Cookie{
		"sem vínculo":           nil,
		"vínculo da vítima":     {{Name: victimCookie.Name, Value: victimCookie.Value}},
		"vínculo malformado":    {{Name: victimCookie.Name, Value: "forjado"}},
		"só a sessão da vítima": {{Name: "recorvia_session", Value: "x"}},
	} {
		st, _ := a.startLogin(t, "/")
		resp := a.callback(t, st, "atacante", cookies...)
		if resp.StatusCode != http.StatusUnauthorized || sessionCookie(resp) != nil {
			t.Fatalf("%s: callback de outro navegador aceito: %d", name, resp.StatusCode)
		}
	}
	resp := a.callback(t, state, "atacante", &http.Cookie{Name: victimCookie.Name, Value: victimCookie.Value})
	if resp.StatusCode != http.StatusUnauthorized || sessionCookie(resp) != nil {
		t.Fatalf("state do atacante no navegador da vítima: %d", resp.StatusCode)
	}
	// O state recusado foi consumido: nem o próprio navegador que o iniciou o
	// reaproveita.
	if resp := a.callback(t, state, "atacante", &http.Cookie{Name: attackerCookie.Name, Value: attackerCookie.Value}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("state reaproveitado: %d", resp.StatusCode)
	}

	// Abas simultâneas do mesmo navegador: o vínculo é reaproveitado e os
	// dois logins concluem; o callback bem-sucedido remove o cookie.
	jar := &http.Cookie{Name: attackerCookie.Name, Value: attackerCookie.Value}
	first, again := a.startLogin(t, "/", jar)
	second, _ := a.startLogin(t, "/", jar)
	if again.Value != jar.Value {
		t.Fatal("o vínculo válido do navegador deve ser reaproveitado")
	}
	for _, st := range []string{first, second} {
		resp := a.callback(t, st, "atacante", jar)
		if resp.StatusCode != http.StatusFound || sessionCookie(resp) == nil {
			t.Fatalf("login no navegador que o iniciou: %d", resp.StatusCode)
		}
		if lc := loginCookie(resp); lc == nil || lc.MaxAge >= 0 {
			t.Fatalf("o callback concluído deve remover o cookie de vínculo: %+v", lc)
		}
	}
}

func inviteToken(t *testing.T, env *fixture.Env, tn fixture.Tenant, email string) string {
	t.Helper()
	_, token, err := env.Tenancy.Invite(context.Background(), tn.ID, tenancy.Access{TenantID: tn.ID, UserID: tn.Owner.ID, Role: tenancy.RoleOwner},
		tenancy.UserRef{ID: tn.Owner.ID, Email: tn.Owner.Email, DisplayName: tn.Owner.DisplayName}, email, tenancy.RoleAuditor)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// Troca de organização: a sessão é rotacionada (cookie e CSRF novos), a
// anterior deixa de valer e a troca exige associação ativa.
func TestSwitchTenantRotatesSessionOverHTTP(t *testing.T) {
	a := newAPI(t)
	tnA, tnB, tnC := a.env.NewTenant(t, "troca-a"), a.env.NewTenant(t, "troca-b"), a.env.NewTenant(t, "troca-c")
	u := member(t, a.env, tnA, tenancy.RoleFinance)
	if _, err := a.env.Tenancy.AcceptInvitation(context.Background(), tenancy.UserRef{ID: u.ID, Email: u.Email}, inviteToken(t, a.env, tnB, u.Email)); err != nil {
		t.Fatal(err)
	}
	c := a.login(u, &tnA.ID)
	old := *c

	status, body, hdr := c.do(http.MethodPut, "/api/v1/session/tenant", map[string]any{"tenant_id": tnB.ID})
	if status != http.StatusOK {
		t.Fatalf("troca: %d %v", status, body)
	}
	ck := sessionCookie(browserResp{Header: hdr})
	if ck == nil || ck.Value == "" || ck.Value == old.cookie || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("a troca deve emitir um cookie novo: %+v", ck)
	}
	at := body["active_tenant"].(map[string]any)
	if at["id"] != tnB.ID.String() || at["role"] != "auditor" || body["csrf_token"] == old.csrf || body["csrf_token"] == "" {
		t.Fatalf("sessão rotacionada com a nova organização e novo CSRF: %v", body)
	}
	if status, _, _ := old.do(http.MethodGet, "/api/v1/session", nil); status != http.StatusUnauthorized {
		t.Fatalf("o cookie anterior deve ser invalidado: %d", status)
	}
	next := &client{a: a, cookie: ck.Value, csrf: body["csrf_token"].(string), tenant: tnB.ID.String()}
	if status, body, _ := next.do(http.MethodGet, "/api/v1/organization", nil); status != http.StatusOK || body["id"] != tnB.ID.String() {
		t.Fatalf("organização ativa depois da troca: %d %v", status, body)
	}
	// Header de outra organização (aba antiga): 409 sem executar a operação.
	stale := *next
	stale.tenant = tnA.ID.String()
	if status, body, _ := stale.do(http.MethodGet, "/api/v1/organization", nil); status != http.StatusConflict || body["code"] != "tenant_mismatch" {
		t.Fatalf("header divergente da sessão: %d %v", status, body)
	}
	// Troca para organização sem associação (existente ou não): 403, e a
	// sessão atual continua válida.
	for _, target := range []uuid.UUID{tnC.ID, uuid.New()} {
		if status, body, _ := next.do(http.MethodPut, "/api/v1/session/tenant", map[string]any{"tenant_id": target}); status != http.StatusForbidden ||
			body["code"] != "no_membership" {
			t.Fatalf("troca sem associação: %d %v", status, body)
		}
	}
	if status, _, _ := next.do(http.MethodGet, "/api/v1/session", nil); status != http.StatusOK {
		t.Fatalf("a recusa não encerra a sessão: %d", status)
	}
}

// Onboarding pela API: a organização nasce com o usuário proprietário, o
// plano padrão em avaliação e a sessão rotacionada com ela ativa.
func TestCreateOrganizationOverHTTP(t *testing.T) {
	a := newAPI(t)
	uid := ids.New()
	u, err := a.env.Store.Users().UpsertFromClaims(context.Background(), uid, identity.Claims{Issuer: "https://idp.test", Subject: uid.String(),
		Email: uid.String()[24:] + "@onboarding.test", EmailVerified: true, Name: "Nova Dona"}, a.env.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	c := a.login(u, nil)
	slug := "nova-" + uid.String()[24:]
	in := map[string]any{"name": "Nova Org", "slug": slug, "timezone": "America/Sao_Paulo", "document_number": "11222333000181"}

	if status, body, _ := (&client{a: a, cookie: c.cookie}).do(http.MethodPost, "/api/v1/organizations", in); status != http.StatusForbidden ||
		body["code"] != "csrf_invalid" {
		t.Fatalf("onboarding sem CSRF: %d %v", status, body)
	}
	if status, body, _ := c.do(http.MethodPost, "/api/v1/organizations", map[string]any{"name": "", "slug": slug}); status != http.StatusUnprocessableEntity {
		t.Fatalf("dados inválidos: %d %v", status, body)
	}
	status, body, hdr := c.do(http.MethodPost, "/api/v1/organizations", in)
	if status != http.StatusCreated {
		t.Fatalf("onboarding: %d %v", status, body)
	}
	ck := sessionCookie(browserResp{Header: hdr})
	at, _ := body["active_tenant"].(map[string]any)
	if ck == nil || ck.Value == c.cookie || at == nil || at["role"] != "owner" || at["slug"] != slug {
		t.Fatalf("sessão rotacionada com a nova organização: %+v %v", ck, body)
	}
	owner := &client{a: a, cookie: ck.Value, csrf: body["csrf_token"].(string), tenant: at["id"].(string)}
	status, ov, _ := owner.do(http.MethodGet, "/api/v1/platform/overview", nil)
	if status != http.StatusOK || ov["subscription"].(map[string]any)["status"] != "trialing" || ov["plan"].(map[string]any)["code"] != "essencial" {
		t.Fatalf("plano inicial em avaliação: %d %v", status, ov)
	}
	if status, _, _ := c.do(http.MethodGet, "/api/v1/session", nil); status != http.StatusUnauthorized {
		t.Fatalf("o cookie anterior ao onboarding deve ser invalidado: %d", status)
	}
	// Slug em uso (por outra organização ou pela recém-criada).
	in["slug"] = slug
	if status, body, _ := owner.do(http.MethodPost, "/api/v1/organizations", in); status != http.StatusConflict || body["code"] != "slug_taken" {
		t.Fatalf("slug repetido: %d %v", status, body)
	}
}

// Aceite de convite pela API: associação criada, uso único, e-mail do
// convite e limite do plano conferido de novo no aceite.
func TestAcceptInvitationOverHTTP(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "aceite-http")
	newUser := func() identity.User {
		uid := ids.New()
		u, err := a.env.Store.Users().UpsertFromClaims(context.Background(), uid, identity.Claims{Issuer: "https://idp.test", Subject: uid.String(),
			Email: uid.String()[24:] + "@aceite.test", EmailVerified: true, Name: "Convidada"}, a.env.Clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	u, other, late := newUser(), newUser(), newUser()
	token := inviteToken(t, a.env, tn, u.Email)
	lateToken := inviteToken(t, a.env, tn, late.Email)
	c := a.login(u, nil)

	if status, body, _ := a.login(other, nil).do(http.MethodPost, "/api/v1/invitations/accept", map[string]any{"token": token}); status != http.StatusForbidden ||
		body["code"] != "invitation_email_mismatch" {
		t.Fatalf("convite de outro e-mail: %d %v", status, body)
	}
	status, body, _ := c.do(http.MethodPost, "/api/v1/invitations/accept", map[string]any{"token": token})
	if status != http.StatusOK || body["tenant_id"] != tn.ID.String() || body["role"] != "auditor" {
		t.Fatalf("aceite: %d %v", status, body)
	}
	if status, body, _ := c.do(http.MethodPost, "/api/v1/invitations/accept", map[string]any{"token": token}); status != http.StatusUnprocessableEntity ||
		body["code"] != "invitation_invalid" {
		t.Fatalf("convite já usado: %d %v", status, body)
	}
	if status, body, _ := c.do(http.MethodPut, "/api/v1/session/tenant", map[string]any{"tenant_id": tn.ID}); status != http.StatusOK {
		t.Fatalf("depois do aceite, a organização pode ser ativada: %d %v", status, body)
	}

	// O plano foi reduzido depois do convite: o aceite recusa passar do limite.
	limitUsers(t, tn, 2)
	if status, body, _ := a.login(late, nil).do(http.MethodPost, "/api/v1/invitations/accept", map[string]any{"token": lateToken}); status != http.StatusForbidden ||
		body["code"] != "platform_user_limit" {
		t.Fatalf("aceite além do limite: %d %v", status, body)
	}
}

// limitUsers atribui ao tenant um plano de teste (inativo) com o limite de
// usuários informado.
func limitUsers(t *testing.T, tn fixture.Tenant, maxUsers int) {
	t.Helper()
	ctx := context.Background()
	owner := pgtest.OwnerPool(t)
	code := "teste_" + strings.ReplaceAll(uuid.NewString()[:13], "-", "")
	if _, err := owner.Exec(ctx, `INSERT INTO platform_plans (code, name, monthly_price_amount, max_users, features, trial_days, active, sort_order)
		VALUES ($1, 'Plano de teste', 9900, $2, ARRAY['recurring_billing'], 0, false, 99)`, code, maxUsers); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE tenant_platform_subscriptions SET plan_code = $1 WHERE tenant_id = $2`, code, tn.ID); err != nil {
		t.Fatal(err)
	}
}

// RF-02: um operador não estorna (a permissão é conferida antes do caso de uso).
func TestOperatorCannotRefund(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "estorno")
	op := a.login(member(t, a.env, tn, tenancy.RoleOperator), &tn.ID)
	fin := a.login(member(t, a.env, tn, tenancy.RoleFinance), &tn.ID)
	path := "/api/v1/charges/" + uuid.NewString() + "/refunds"
	in := map[string]any{"amount": 1000, "reason": "Cobrança em duplicidade"}
	if status, body, _ := op.do(http.MethodPost, path, in, "Idempotency-Key", "estorno-operador-1"); status != http.StatusForbidden || body["code"] != "forbidden" {
		t.Fatalf("operador estornando: %d %v", status, body)
	}
	if status, body, _ := fin.do(http.MethodPost, path, in, "Idempotency-Key", "estorno-financeiro-1"); status == http.StatusForbidden {
		t.Fatalf("o financeiro tem a permissão de estorno: %d %v", status, body)
	}
}

func sessionExists(t *testing.T, a *apiEnv, id string) bool {
	t.Helper()
	_, err := a.sessions.Get(context.Background(), id)
	if err != nil && !errors.Is(err, identity.ErrSessionNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// Revogação: "encerrar todas as minhas sessões", remoção do membro (encerra
// as sessões com aquela organização ativa) e usuário desativado.
func TestSessionRevocationOverHTTP(t *testing.T) {
	idp := newFlowIDP(time.Now().Add(24 * time.Hour))
	a := newAPI(t, func(s *apiSetup) { s.idp = idp })
	idp.expiry = a.env.Clock.Now().Add(24 * time.Hour)
	tnA, tnB := a.env.NewTenant(t, "revoga-a"), a.env.NewTenant(t, "revoga-b")

	t.Run("encerrar todas as sessões", func(t *testing.T) {
		idp.user("dono-a", claimsOf(tnA.Owner))
		first, _ := a.cookieClient(t, sessionCookie(a.browserLogin(t, "dono-a", "/")).Value)
		second, _ := a.cookieClient(t, sessionCookie(a.browserLogin(t, "dono-a", "/")).Value)
		stranger := a.login(tnB.Owner, &tnB.ID)
		status, body, hdr := first.do(http.MethodPost, "/auth/logout-all", nil)
		if status != http.StatusOK || strings.Contains(body["redirect_url"].(string), "rt-") || sessionCookie(browserResp{Header: hdr}).MaxAge >= 0 {
			t.Fatalf("logout-all: %d %v", status, body)
		}
		for _, c := range []*client{first, second} {
			if status, _, _ := c.do(http.MethodGet, "/api/v1/session", nil); status != http.StatusUnauthorized {
				t.Fatalf("todas as sessões do usuário devem ser encerradas: %d", status)
			}
		}
		if got := idp.revokedTokens(); len(got) != 1 || got[0] != "rt-dono-a" {
			t.Fatalf("refresh tokens revogados: %v", got)
		}
		if status, _, _ := stranger.do(http.MethodGet, "/api/v1/session", nil); status != http.StatusOK {
			t.Fatalf("sessões de outros usuários continuam: %d", status)
		}
	})

	t.Run("membro removido perde as sessões daquela organização", func(t *testing.T) {
		m := member(t, a.env, tnA, tenancy.RoleFinance)
		if _, err := a.env.Tenancy.AcceptInvitation(context.Background(), tenancy.UserRef{ID: m.ID, Email: m.Email}, inviteToken(t, a.env, tnB, m.Email)); err != nil {
			t.Fatal(err)
		}
		inA, inB, none := a.login(m, &tnA.ID), a.login(m, &tnB.ID), a.login(m, nil)
		owner := a.login(tnA.Owner, &tnA.ID)
		if status, body, _ := owner.do(http.MethodDelete, "/api/v1/members/"+m.ID.String(), nil,
			matchVersion(owner.versionIn("/api/v1/members", "user_id", m.ID.String()))...); status != http.StatusNoContent {
			t.Fatalf("remoção: %d %v", status, body)
		}
		if sessionExists(t, a, inA.cookie) {
			t.Fatal("a sessão com a organização da remoção deve ser encerrada")
		}
		if status, _, _ := inA.do(http.MethodGet, "/api/v1/organization", nil); status != http.StatusUnauthorized {
			t.Fatalf("o membro removido não acessa a organização: %d", status)
		}
		if status, body, _ := inB.do(http.MethodGet, "/api/v1/organization", nil); status != http.StatusOK || body["id"] != tnB.ID.String() {
			t.Fatalf("a outra organização continua acessível: %d %v", status, body)
		}
		// Sem organização ativa, a sessão continua, mas não ativa a removida.
		if status, body, _ := none.do(http.MethodPut, "/api/v1/session/tenant", map[string]any{"tenant_id": tnA.ID}); status != http.StatusForbidden ||
			body["code"] != "no_membership" {
			t.Fatalf("ativar a organização da remoção: %d %v", status, body)
		}

		// Usuário desativado: a primeira requisição encerra todas as sessões dele.
		if _, err := pgtest.OwnerPool(t).Exec(context.Background(), `UPDATE users SET status = 'disabled' WHERE id = $1`, m.ID); err != nil {
			t.Fatal(err)
		}
		status, _, hdr := inB.do(http.MethodGet, "/api/v1/session", nil)
		if status != http.StatusUnauthorized || sessionCookie(browserResp{Header: hdr}) == nil {
			t.Fatalf("usuário desativado: %d", status)
		}
		if sessionExists(t, a, inB.cookie) || sessionExists(t, a, none.cookie) {
			t.Fatal("todas as sessões do usuário desativado devem ser encerradas")
		}
	})
}
