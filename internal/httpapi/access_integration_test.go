//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/portal"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// noRedirect não segue redirecionamentos (a troca do link responde 302).
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type rawResponse struct {
	status int
	body   map[string]any
	text   string
	header http.Header
	cookie map[string]*http.Cookie
}

// raw envia uma requisição com cabeçalhos e cookies explícitos.
func (a *apiEnv) raw(method, path string, body any, headers map[string]string, cookies ...*http.Cookie) rawResponse {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.srv.URL+path, rd)
	if err != nil {
		a.t.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		a.t.Fatal(err)
	}
	out := rawResponse{status: resp.StatusCode, body: map[string]any{}, text: string(raw), header: resp.Header, cookie: map[string]*http.Cookie{}}
	_ = json.Unmarshal(raw, &out.body) // respostas sem JSON ficam vazias
	for _, c := range resp.Cookies() {
		out.cookie[c.Name] = c
	}
	return out
}

func bearer(token string, extra ...string) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + token}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return h
}

func apiPlanTenant(t *testing.T, a *apiEnv, prefix string) fixture.Tenant {
	t.Helper()
	tn := a.env.NewTenant(t, prefix)
	if _, err := a.env.Platform.ChangePlan(context.Background(), tn.ID, tn.Actor, "profissional", a.env.PlatformVersion(t, tn.ID)); err != nil {
		t.Fatal(err)
	}
	return tn
}

func createCredential(t *testing.T, c *client, scopes ...string) (id, prefix, secret string) {
	t.Helper()
	st, body, _ := c.do(http.MethodPost, "/api/v1/api-credentials", map[string]any{"name": "ERP interno", "scopes": scopes, "expires_in_days": 30})
	if st != 201 {
		t.Fatalf("criação de credencial: %d %v", st, body)
	}
	cred := body["credential"].(map[string]any)
	return cred["id"].(string), cred["prefix"].(string), body["secret"].(string)
}

func TestAPICredentialsOverHTTP(t *testing.T) {
	a := newAPI(t)
	tnA, tnB := apiPlanTenant(t, a, "bearer-a"), apiPlanTenant(t, a, "bearer-b")
	owner := a.login(tnA.Owner, &tnA.ID)

	id, prefix, secret := createCredential(t, owner, "customers.read", "customers.write", "credits.write")
	if !strings.HasPrefix(prefix, "rcv_") || !strings.HasPrefix(secret, prefix+"_") || len(secret) != apicredentials.TokenLength {
		t.Fatalf("formato do token: %q %q", prefix, secret)
	}
	st, list, _ := owner.do(http.MethodGet, "/api/v1/api-credentials", nil)
	items := list["data"].([]any)
	if st != 200 || len(items) != 1 {
		t.Fatalf("lista: %d %v", st, list)
	}
	listed := items[0].(map[string]any)
	if _, leaked := listed["secret"]; leaked || listed["prefix"] != prefix || listed["revoked_at"] != nil {
		t.Fatalf("a listagem não expõe segredos: %v", listed)
	}

	// Leitura e escrita pelos escopos, sem CSRF nem cookie.
	if r := a.raw(http.MethodGet, "/api/v1/customers", nil, bearer(secret)); r.status != 200 {
		t.Fatalf("leitura por credencial: %d %v", r.status, r.body)
	}
	r := a.raw(http.MethodPost, "/api/v1/customers", map[string]any{"kind": "individual", "name": "Cliente via API", "document_number": "529.982.247-25"},
		bearer(secret))
	if r.status != 201 {
		t.Fatalf("escrita por credencial: %d %v", r.status, r.body)
	}
	customerID := r.body["id"].(string)
	// Escopo insuficiente.
	if r := a.raw(http.MethodGet, "/api/v1/invoices", nil, bearer(secret)); r.status != 403 || r.body["code"] != "forbidden" {
		t.Fatalf("escopo insuficiente: %d %v", r.status, r.body)
	}
	// Cabeçalho de tenant: igual ao da credencial é aceito; divergente, 409.
	if r := a.raw(http.MethodGet, "/api/v1/customers", nil, bearer(secret, "X-Recorvia-Tenant", tnA.ID.String())); r.status != 200 {
		t.Fatalf("tenant coincidente: %d", r.status)
	}
	if r := a.raw(http.MethodGet, "/api/v1/customers", nil, bearer(secret, "X-Recorvia-Tenant", tnB.ID.String())); r.status != 409 || r.body["code"] != "tenant_mismatch" {
		t.Fatalf("tenant divergente: %d %v", r.status, r.body)
	}
	// Rotas de sessão e de gestão de credenciais não aceitam credenciais.
	if r := a.raw(http.MethodGet, "/api/v1/session", nil, bearer(secret)); r.status != 403 || r.body["code"] != "user_session_required" {
		t.Fatalf("rota de sessão com credencial: %d %v", r.status, r.body)
	}
	if r := a.raw(http.MethodGet, "/api/v1/api-credentials", nil, bearer(secret)); r.status != 403 {
		t.Fatalf("credencial gerenciando credenciais: %d %v", r.status, r.body)
	}
	// Auditoria com ator api_credential.
	st, events, _ := owner.do(http.MethodGet, "/api/v1/audit-events?action=customer.created&resource_id="+customerID, nil)
	ev := events["data"].([]any)
	if st != 200 || len(ev) != 1 || ev[0].(map[string]any)["actor_type"] != "api_credential" || ev[0].(map[string]any)["actor_id"] != id {
		t.Fatalf("auditoria: %d %v", st, events)
	}
	// Idempotência no escopo da credencial: repetição devolve o resultado; a
	// mesma chave usada pelo usuário é outro cliente.
	credit := map[string]any{"amount": 700, "reason": "Crédito via integração"}
	key := "integracao-" + uuid.NewString()
	r1 := a.raw(http.MethodPost, "/api/v1/customers/"+customerID+"/credits", credit, bearer(secret, "Idempotency-Key", key))
	r2 := a.raw(http.MethodPost, "/api/v1/customers/"+customerID+"/credits", credit, bearer(secret, "Idempotency-Key", key))
	if r1.status != 201 || r2.status != 201 || r2.header.Get("Idempotent-Replayed") != "true" || r2.body["balance"] != float64(700) {
		t.Fatalf("idempotência por credencial: %d %v / %d %v", r1.status, r1.body, r2.status, r2.body)
	}
	var clientID string
	if err := pgtest.OwnerPool(t).QueryRow(context.Background(), `SELECT client_id FROM idempotency_keys WHERE tenant_id = $1 AND idempotency_key = $2`,
		tnA.ID, key).Scan(&clientID); err != nil || clientID != "api_credential:"+id {
		t.Fatalf("cliente de idempotência: %q %v", clientID, err)
	}
	if st, body, _ := owner.do(http.MethodPost, "/api/v1/customers/"+customerID+"/credits", credit, "Idempotency-Key", key); st != 201 || body["balance"] != float64(1400) {
		t.Fatalf("mesma chave por outro cliente: %d %v", st, body)
	}
	// Credencial de outro tenant não alcança o recurso (404).
	ownerB := a.login(tnB.Owner, &tnB.ID)
	_, _, secretB := createCredential(t, ownerB, "customers.read")
	if r := a.raw(http.MethodGet, "/api/v1/customers/"+customerID, nil, bearer(secretB)); r.status != 404 {
		t.Fatalf("credencial de outro tenant: %d %v", r.status, r.body)
	}
	// Tokens malformados ou desconhecidos.
	for _, h := range []map[string]string{{"Authorization": "Basic dXNlcjpzZW5oYQ=="}, {"Authorization": "Bearer "}, bearer("rcv_desconhecido"),
		bearer(prefix + "_" + strings.Repeat("a", apicredentials.SecretLength))} {
		if r := a.raw(http.MethodGet, "/api/v1/customers", nil, h); r.status != 401 || r.body["code"] != "api_credential_invalid" ||
			!strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Bearer") {
			t.Fatalf("token inválido %v: %d %v", h, r.status, r.body)
		}
	}

	// Rotação: identificador mantido; o segredo anterior vale por 24 horas.
	if st, body, _ := owner.do(http.MethodPost, "/api/v1/api-credentials/"+id+"/rotate", nil); st != 428 || body["code"] != "if_match_required" {
		t.Fatalf("rotação sem If-Match: %d %v", st, body)
	}
	if st, body, _ := owner.do(http.MethodPost, "/api/v1/api-credentials/"+id+"/rotate", nil, matchVersion(2)...); st != 412 || body["code"] != "version_mismatch" {
		t.Fatalf("rotação com versão divergente: %d %v", st, body)
	}
	st, rot, rotH := owner.do(http.MethodPost, "/api/v1/api-credentials/"+id+"/rotate", nil, matchVersion(owner.versionIn("/api/v1/api-credentials", "id", id))...)
	if st != 201 || rot["credential"].(map[string]any)["id"] != id || rot["secret"] == secret || rotH.Get("ETag") != `"2"` ||
		rot["credential"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("rotação: %d %v (ETag %q)", st, rot, rotH.Get("ETag"))
	}
	rotated := rot["secret"].(string)
	for _, token := range []string{secret, rotated} {
		if r := a.raw(http.MethodGet, "/api/v1/customers", nil, bearer(token)); r.status != 200 {
			t.Fatalf("token na janela de rotação: %d %v", r.status, r.body)
		}
	}
	a.env.Clock.Advance(apicredentials.RotationGrace)
	// As sessões de navegador expiram nesse intervalo; as credenciais não.
	owner, ownerB = a.login(tnA.Owner, &tnA.ID), a.login(tnB.Owner, &tnB.ID)
	if r := a.raw(http.MethodGet, "/api/v1/customers", nil, bearer(secret)); r.status != 401 {
		t.Fatalf("segredo anterior após a janela: %d", r.status)
	}
	if r := a.raw(http.MethodGet, "/api/v1/customers", nil, bearer(rotated)); r.status != 200 {
		t.Fatalf("segredo novo: %d", r.status)
	}
	// Revogação imediata.
	if st, body, _ := owner.do(http.MethodDelete, "/api/v1/api-credentials/"+id, nil, matchVersion(1)...); st != 412 || body["code"] != "version_mismatch" {
		t.Fatalf("revogação com a versão anterior à rotação: %d %v", st, body)
	}
	if st, body, _ := owner.do(http.MethodDelete, "/api/v1/api-credentials/"+id, nil, matchVersion(2)...); st != 204 {
		t.Fatalf("revogação: %d %v", st, body)
	}
	if r := a.raw(http.MethodGet, "/api/v1/customers", nil, bearer(rotated)); r.status != 401 {
		t.Fatalf("credencial revogada aceita: %d", r.status)
	}
	// Outro tenant não revoga nem rotaciona credencial alheia.
	if st, _, _ := ownerB.do(http.MethodDelete, "/api/v1/api-credentials/"+id, nil, matchVersion(3)...); st != 404 {
		t.Fatalf("revogação por outro tenant: %d", st)
	}
	if st, _, _ := owner.do(http.MethodPost, "/api/v1/api-credentials/"+id+"/rotate", nil, matchVersion(3)...); st != 409 {
		t.Fatalf("rotação de credencial revogada: %d", st)
	}
}

// Atrás de um proxy com HTTP Basic, o navegador repete o header Authorization
// em todas as requisições do painel: com cookie de sessão, esse header é
// ignorado e a verificação CSRF continua valendo; um token do Recorvia enviado
// junto com o cookie tem precedência e autentica a credencial.
func TestSessionCookieIgnoresForeignAuthorization(t *testing.T) {
	a := newAPI(t)
	tn := apiPlanTenant(t, a, "sessao-proxy")
	owner := a.login(tn.Owner, &tn.ID)
	const basic = "Basic dXNlcjpzZW5oYQ=="
	if st, body, _ := owner.do(http.MethodGet, "/api/v1/session", nil, "Authorization", basic); st != 200 {
		t.Fatalf("sessão atrás de proxy com HTTP Basic: %d %v", st, body)
	}
	if st, body, _ := owner.do(http.MethodGet, "/api/v1/api-credentials", nil, "Authorization", basic); st != 200 {
		t.Fatalf("rota de tenant atrás de proxy com HTTP Basic: %d %v", st, body)
	}
	customer := map[string]any{"kind": "individual", "name": "Cliente pelo painel", "document_number": "529.982.247-25"}
	if st, body, _ := owner.do(http.MethodPost, "/api/v1/customers", customer, "Authorization", basic, "X-CSRF-Token", "token-invalido"); st != 403 ||
		body["code"] != "csrf_invalid" {
		t.Fatalf("CSRF com HTTP Basic do proxy: %d %v", st, body)
	}
	if st, body, _ := owner.do(http.MethodPost, "/api/v1/customers", customer, "Authorization", basic); st != 201 {
		t.Fatalf("escrita pela sessão com HTTP Basic do proxy: %d %v", st, body)
	}
	_, _, secret := createCredential(t, owner, "customers.read")
	if st, body, _ := owner.do(http.MethodGet, "/api/v1/session", nil, "Authorization", "Bearer "+secret); st != 403 || body["code"] != "user_session_required" {
		t.Fatalf("token do Recorvia junto com o cookie: %d %v", st, body)
	}
}

func TestAPICredentialManagementRules(t *testing.T) {
	a := newAPI(t)
	basic := a.env.NewTenant(t, "bearer-basico")
	ownerBasic := a.login(basic.Owner, &basic.ID)
	if st, body, _ := ownerBasic.do(http.MethodPost, "/api/v1/api-credentials", map[string]any{"name": "ERP", "scopes": []string{"customers.read"}}); st != 403 ||
		body["code"] != "platform_feature_unavailable" {
		t.Fatalf("plano sem api_access: %d %v", st, body)
	}
	tn := apiPlanTenant(t, a, "bearer-regras")
	admin := a.login(member(t, a.env, tn, tenancy.RoleAdmin), &tn.ID)
	for _, scopes := range [][]string{{"members.manage"}, {"api_credentials.manage"}, {"platform_billing.manage"}, {"permissao.inexistente"}, {}} {
		if st, body, _ := admin.do(http.MethodPost, "/api/v1/api-credentials", map[string]any{"name": "ERP", "scopes": scopes}); st != 422 {
			t.Fatalf("escopos %v: %d %v", scopes, st, body)
		}
	}
	if st, body, _ := admin.do(http.MethodPost, "/api/v1/api-credentials", map[string]any{"name": "ERP", "scopes": []string{"customers.read"}, "expires_in_days": 731}); st != 422 {
		t.Fatalf("validade acima do máximo: %d %v", st, body)
	}
	auditor := a.login(member(t, a.env, tn, tenancy.RoleAuditor), &tn.ID)
	if st, _, _ := auditor.do(http.MethodGet, "/api/v1/api-credentials", nil); st != 403 {
		t.Fatalf("auditor listou credenciais: %d", st)
	}
	_, _, secret := createCredential(t, admin, "customers.read", "customers.portal_links")
	// A credencial gera link do portal; a autoria fica na credencial.
	cust := a.env.NewCustomer(t, tn)
	r := a.raw(http.MethodPost, "/api/v1/customers/"+cust.ID.String()+"/portal-links", nil, bearer(secret))
	if r.status != 201 || !strings.Contains(r.body["url"].(string), "/portal/access?token=") {
		t.Fatalf("link do portal por credencial: %d %v", r.status, r.body)
	}
}

func TestAPICredentialRateLimits(t *testing.T) {
	const limit = 3
	a := newAPI(t, func(s *apiSetup) {
		s.cfg.APICredentialRateLimitPerMinute = limit
		s.credentialPolicy = apicredentials.Policy{MaxFailures: 2, FailureWindow: time.Minute}
	})
	tn := apiPlanTenant(t, a, "bearer-limite")
	owner := a.login(tn.Owner, &tn.ID)
	_, prefix, secret := createCredential(t, owner, "customers.read")
	// Janela fixa por minuto: em 2·limite+1 requisições seguidas ao menos uma
	// é recusada, mesmo que a sequência atravesse a virada da janela.
	limited := false
	for range 2*limit + 1 {
		if r := a.raw(http.MethodGet, "/api/v1/customers", nil, bearer(secret)); r.status == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("limite por credencial não aplicado")
	}
	// A sessão do usuário tem limite próprio.
	if st, _, _ := owner.do(http.MethodGet, "/api/v1/customers", nil); st != 200 {
		t.Fatalf("limite da credencial afetou a sessão do usuário: %d", st)
	}
	// Tentativas inválidas bloqueiam a origem.
	bad := prefix + "_" + strings.Repeat("b", apicredentials.SecretLength)
	for range 2 {
		if r := a.raw(http.MethodGet, "/api/v1/customers", nil, bearer(bad)); r.status != 401 {
			t.Fatalf("token inválido: %d", r.status)
		}
	}
	if r := a.raw(http.MethodGet, "/api/v1/customers", nil, bearer(secret)); r.status != 429 || r.body["code"] != "authentication_attempts_exceeded" {
		t.Fatalf("origem bloqueada após falhas: %d %v", r.status, r.body)
	}
}

// --- portal do cliente ---

func createPortalLink(t *testing.T, c *client, customerID uuid.UUID) string {
	t.Helper()
	st, body, _ := c.do(http.MethodPost, "/api/v1/customers/"+customerID.String()+"/portal-links", nil)
	if st != 201 {
		t.Fatalf("geração do link: %d %v", st, body)
	}
	exp, err := time.Parse(time.RFC3339Nano, body["expires_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if want := c.a.env.Clock.Now().Add(portal.DefaultPolicy().LinkTTL); !exp.Equal(want) {
		t.Fatalf("expiração do link %v, esperado %v", exp, want)
	}
	return portalToken(t, body)
}

// portalToken extrai o token da URL devolvida na geração do link.
func portalToken(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, _ := body["url"].(string)
	u, err := url.Parse(raw)
	if err != nil || u.Path != "/portal/access" || u.Query().Get("token") == "" {
		t.Fatalf("URL do link: %v %v", body["url"], err)
	}
	return u.Query().Get("token")
}

func (a *apiEnv) portalAccess(token string) rawResponse {
	return a.raw(http.MethodGet, "/portal/access?token="+url.QueryEscape(token), nil, nil)
}

func openPortal(t *testing.T, a *apiEnv, token string) (*http.Cookie, string) {
	t.Helper()
	r := a.portalAccess(token)
	if r.status != 302 || r.header.Get("Location") != "/portal" {
		t.Fatalf("troca do link: %d %v", r.status, r.body)
	}
	c := r.cookie["recorvia_portal"]
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Value == "" {
		t.Fatalf("cookie do portal: %+v", c)
	}
	s := a.raw(http.MethodGet, "/api/portal/v1/session", nil, nil, c)
	if s.status != 200 {
		t.Fatalf("sessão do portal: %d %v", s.status, s.body)
	}
	return c, s.body["csrf_token"].(string)
}

func TestPortalAccessOverHTTP(t *testing.T) {
	a := newAPI(t)
	ctx := context.Background()
	tnA, tnB := a.env.NewTenant(t, "portal-a"), a.env.NewTenant(t, "portal-b")
	cust := a.env.NewCustomer(t, tnA)
	if _, err := a.env.Customers.AddContact(ctx, tnA.ID, tnA.Actor, cust.ID, customers.ContactInput{Name: "Financeiro", Email: "fin@cliente.test",
		ReceivesBillingEmail: true}); err != nil {
		t.Fatal(err)
	}
	operator := a.login(member(t, a.env, tnA, tenancy.RoleOperator), &tnA.ID)
	auditor := a.login(member(t, a.env, tnA, tenancy.RoleAuditor), &tnA.ID)
	if st, _, _ := auditor.do(http.MethodPost, "/api/v1/customers/"+cust.ID.String()+"/portal-links", nil); st != 403 {
		t.Fatalf("auditor gerou link: %d", st)
	}
	ownerB := a.login(tnB.Owner, &tnB.ID)
	if st, _, _ := ownerB.do(http.MethodPost, "/api/v1/customers/"+cust.ID.String()+"/portal-links", nil); st != 404 {
		t.Fatalf("link para cliente de outro tenant: %d", st)
	}

	token := createPortalLink(t, operator, cust.ID)
	cookie, csrf := openPortal(t, a, token)
	// Token usado duas vezes.
	if r := a.portalAccess(token); r.status != 401 || r.body["code"] != "portal_link_invalid" {
		t.Fatalf("segundo uso do link: %d %v", r.status, r.body)
	}
	s := a.raw(http.MethodGet, "/api/portal/v1/session", nil, nil, cookie)
	sessCustomer := s.body["customer"].(map[string]any)
	org := s.body["organization"].(map[string]any)
	contacts := s.body["contacts"].([]any)
	if sessCustomer["id"] != cust.ID.String() || sessCustomer["name"] != cust.Name || org["name"] != "Org portal-a" || len(contacts) != 1 || csrf == "" {
		t.Fatalf("sessão do portal fora do escopo do link: %v", s.body)
	}
	// O cookie do portal não abre o painel; a sessão do painel não abre o portal.
	if r := a.raw(http.MethodGet, "/api/v1/customers", nil, map[string]string{"X-Recorvia-Tenant": tnA.ID.String()}, cookie); r.status != 401 {
		t.Fatalf("cookie do portal no painel: %d", r.status)
	}
	if r := a.raw(http.MethodGet, "/api/portal/v1/session", nil, nil, &http.Cookie{Name: "recorvia_session", Value: operator.cookie}); r.status != 401 ||
		r.body["code"] != "portal_session_required" {
		t.Fatalf("sessão do painel no portal: %d %v", r.status, r.body)
	}
	// Encerramento exige CSRF.
	if r := a.raw(http.MethodPost, "/api/portal/v1/logout", nil, nil, cookie); r.status != 403 || r.body["code"] != "csrf_invalid" {
		t.Fatalf("logout sem CSRF: %d %v", r.status, r.body)
	}
	r := a.raw(http.MethodPost, "/api/portal/v1/logout", nil, map[string]string{"X-CSRF-Token": csrf}, cookie)
	if r.status != 204 || r.cookie["recorvia_portal"] == nil || r.cookie["recorvia_portal"].MaxAge >= 0 {
		t.Fatalf("logout: %d %v", r.status, r.cookie)
	}
	if r := a.raw(http.MethodGet, "/api/portal/v1/session", nil, nil, cookie); r.status != 401 || r.body["code"] != "portal_session_invalid" {
		t.Fatalf("sessão após logout: %d %v", r.status, r.body)
	}

	// Um novo link encerra a sessão aberta com o anterior.
	cookie2, _ := openPortal(t, a, createPortalLink(t, operator, cust.ID))
	pending := createPortalLink(t, operator, cust.ID)
	if r := a.raw(http.MethodGet, "/api/portal/v1/session", nil, nil, cookie2); r.status != 401 || r.cookie["recorvia_portal"] == nil {
		t.Fatalf("sessão de link substituído: %d %v", r.status, r.body)
	}
	// Link expirado.
	a.env.Clock.Advance(portal.DefaultPolicy().LinkTTL)
	if r := a.portalAccess(pending); r.status != 401 {
		t.Fatalf("link expirado: %d %v", r.status, r.body)
	}
	// Auditoria com ator customer_portal.
	owner := a.login(tnA.Owner, &tnA.ID)
	st, events, _ := owner.do(http.MethodGet, "/api/v1/audit-events?action=customer_portal.session_started", nil)
	ev := events["data"].([]any)
	if st != 200 || len(ev) != 2 || ev[0].(map[string]any)["actor_type"] != "customer_portal" || ev[0].(map[string]any)["actor_id"] != cust.ID.String() {
		t.Fatalf("auditoria do portal: %d %v", st, events)
	}
}

func TestPortalAccessAttemptLimit(t *testing.T) {
	a := newAPI(t, func(s *apiSetup) {
		s.portalPolicy.MaxFailures, s.portalPolicy.FailureWindow = 2, time.Minute
	})
	tn := a.env.NewTenant(t, "portal-limite")
	cust := a.env.NewCustomer(t, tn)
	token := createPortalLink(t, a.login(tn.Owner, &tn.ID), cust.ID)
	for _, invalid := range []string{"x", strings.Repeat("A", 43)} {
		if r := a.portalAccess(invalid); r.status != 401 {
			t.Fatalf("token inválido: %d", r.status)
		}
	}
	if r := a.portalAccess(token); r.status != 429 || r.body["code"] != "portal_access_attempts_exceeded" {
		t.Fatalf("origem bloqueada: %d %v", r.status, r.body)
	}
}
