//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/httpapi"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/redisstore"
	"github.com/ricartefelipe/recorvia/internal/infra/secrets"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways"
	"github.com/ricartefelipe/recorvia/internal/portal"
	"github.com/ricartefelipe/recorvia/internal/reporting"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

type stubIDP struct{ identity.IdentityProvider }

func (stubIDP) NewPKCEVerifier() string           { return "verificador" }
func (stubIDP) AuthCodeURL(_, _, _ string) string { return "https://idp.test/auth" }

type apiEnv struct {
	t        *testing.T
	env      *fixture.Env
	sessions *redisstore.Store
	srv      *httptest.Server
}

// apiSetup permite ajustar configuração e políticas antes de montar a API.
type apiSetup struct {
	cfg              *config.Config
	portalPolicy     portal.Policy
	credentialPolicy apicredentials.Policy
}

func newAPI(t *testing.T, opts ...func(*apiSetup)) *apiEnv {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcredis.Run(ctx, "redis:8.8.4-alpine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	url, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rdb, err := redisstore.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	env := fixture.New(t, 8, time.Now().UTC())
	kr, err := secrets.NewKeyring(map[string][]byte{"k": bytes.Repeat([]byte{9}, 32)}, "k")
	if err != nil {
		t.Fatal(err)
	}
	sessions := redisstore.New(rdb, kr, "web")
	idsvc := identity.NewService(stubIDP{}, sessions, env.Store.Users(), env.Clock, identity.DefaultPolicy())
	setup := &apiSetup{cfg: &config.Config{Env: config.Test, CookieName: "recorvia_session", PortalCookieName: "recorvia_portal",
		CookieSecure: false, RateLimitPerMinute: 10000, APICredentialRateLimitPerMinute: 10000},
		portalPolicy: portal.DefaultPolicy(), credentialPolicy: apicredentials.DefaultPolicy()}
	for _, o := range opts {
		o(setup)
	}
	pay := env.Payments(gateways.NewResolver(time.Second), payments.Config{WebhookBaseURL: "http://api.test/webhooks/providers"})
	portalSvc := portal.NewService(env.Store.Portal(), redisstore.NewPortalSessions(redisstore.New(rdb, kr, "portal")), sessions, env.Clock,
		setup.portalPolicy)
	credentials := apicredentials.NewService(env.Store.APICredentials(), sessions, env.Clock, setup.credentialPolicy)
	_, router := httpapi.NewRouter(httpapi.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Config: setup.cfg, Clock: env.Clock,
		Store: env.Store, Sessions: sessions, Identity: idsvc, Tenancy: env.Tenancy, Platform: env.Platform, Customers: env.Customers,
		Catalog: env.Catalog, Subscriptions: env.Subscriptions, Billing: env.Billing, Payments: pay,
		Metering: metering.NewService(env.Store.Metering(), env.Clock), Reporting: reporting.NewService(env.Store.Reporting(), env.Clock),
		Portal: portalSvc, Credentials: credentials})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &apiEnv{t: t, env: env, sessions: sessions, srv: srv}
}

type client struct {
	a      *apiEnv
	cookie string
	csrf   string
	tenant string
}

func (a *apiEnv) login(user identity.User, tenantID *uuid.UUID) *client {
	a.t.Helper()
	now := a.env.Clock.Now()
	s := identity.Session{ID: identity.RandomToken(32), UserID: user.ID, ActiveTenantID: tenantID, CSRFToken: identity.RandomToken(16),
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := a.sessions.Save(context.Background(), s, time.Hour); err != nil {
		a.t.Fatal(err)
	}
	c := &client{a: a, cookie: s.ID, csrf: s.CSRFToken}
	if tenantID != nil {
		c.tenant = tenantID.String()
	}
	return c
}

func (c *client) do(method, path string, body any, extra ...string) (int, map[string]any, http.Header) {
	c.a.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			c.a.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.a.srv.URL+path, rd)
	if err != nil {
		c.a.t.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "recorvia_session", Value: c.cookie})
	}
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	if c.tenant != "" {
		req.Header.Set("X-Recorvia-Tenant", c.tenant)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.a.t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		c.a.t.Fatal(err)
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out) // respostas não JSON (CSV, HTML) ficam vazias
	return resp.StatusCode, out, resp.Header
}

func member(t *testing.T, env *fixture.Env, tn fixture.Tenant, role tenancy.Role) identity.User {
	t.Helper()
	ctx := context.Background()
	uid := ids.New()
	u, err := env.Store.Users().UpsertFromClaims(ctx, uid, identity.Claims{Issuer: "https://idp.test", Subject: uid.String(),
		Email: uid.String()[24:] + "@membro.test", EmailVerified: true, Name: "Membro"}, env.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := env.Tenancy.Invite(ctx, tn.ID, tenancy.Access{TenantID: tn.ID, UserID: tn.Owner.ID, Role: tenancy.RoleOwner},
		tenancy.UserRef{ID: tn.Owner.ID, Email: tn.Owner.Email, DisplayName: tn.Owner.DisplayName}, u.Email, role)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Tenancy.AcceptInvitation(ctx, tenancy.UserRef{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName}, token); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAPIAuthorizationAndTenantIsolation(t *testing.T) {
	a := newAPI(t)
	tnA, tnB := a.env.NewTenant(t, "api-a"), a.env.NewTenant(t, "api-b")
	owner := a.login(tnA.Owner, &tnA.ID)
	ownerB := a.login(tnB.Owner, &tnB.ID)

	anon := &client{a: a}
	if st, body, h := anon.do(http.MethodGet, "/api/v1/session", nil); st != 401 || body["code"] != "session_required" || h.Get("X-Request-ID") == "" {
		t.Fatalf("sem sessão: %d %v", st, body)
	}
	st, cust, _ := owner.do(http.MethodPost, "/api/v1/customers", map[string]any{"kind": "individual", "name": "Maria Teste", "document_number": "529.982.247-25"})
	if st != 201 {
		t.Fatalf("criação de cliente: %d %v", st, cust)
	}
	id := cust["id"].(string)
	if st, body, _ := ownerB.do(http.MethodGet, "/api/v1/customers/"+id, nil); st != 404 {
		t.Fatalf("tenant B obteve cliente do tenant A: %d %v", st, body)
	}
	forged := a.login(tnB.Owner, &tnB.ID)
	forged.tenant = tnA.ID.String()
	if st, body, _ := forged.do(http.MethodGet, "/api/v1/customers", nil); st != 409 || body["code"] != "tenant_mismatch" {
		t.Fatalf("header de tenant forjado: %d %v", st, body)
	}
	noCSRF := a.login(tnA.Owner, &tnA.ID)
	noCSRF.csrf = ""
	if st, body, _ := noCSRF.do(http.MethodPost, "/api/v1/products", map[string]any{"name": "Sem CSRF"}); st != 403 || body["code"] != "csrf_invalid" {
		t.Fatalf("sem CSRF: %d %v", st, body)
	}
	auditor := a.login(member(t, a.env, tnA, tenancy.RoleAuditor), &tnA.ID)
	if st, _, _ := auditor.do(http.MethodPost, "/api/v1/products", map[string]any{"name": "Auditor"}); st != 403 {
		t.Fatalf("auditor escreveu: %d", st)
	}
	if st, _, _ := auditor.do(http.MethodGet, "/api/v1/audit-events", nil); st != 200 {
		t.Fatalf("auditor deveria ler a auditoria: %d", st)
	}
	operator := a.login(member(t, a.env, tnA, tenancy.RoleOperator), &tnA.ID)
	if st, _, _ := operator.do(http.MethodGet, "/api/v1/ledger/accounts", nil); st != 403 {
		t.Fatalf("operador leu o ledger: %d", st)
	}
	// Troca para organização sem associação é negada.
	if st, body, _ := owner.do(http.MethodPut, "/api/v1/session/tenant", map[string]any{"tenant_id": tnB.ID}); st != 403 {
		t.Fatalf("troca indevida: %d %v", st, body)
	}
}

func TestAPIValidationConcurrencyAndIdempotency(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "api-idem")
	c := a.login(tn.Owner, &tn.ID)
	st, body, _ := c.do(http.MethodPost, "/api/v1/customers", map[string]any{"kind": "company", "name": "X", "document_number": "11222333000180"})
	if st != 422 || body["code"] != "invalid_customer" || body["errors"] == nil {
		t.Fatalf("validação: %d %v", st, body)
	}
	if st, body, _ := c.do(http.MethodPost, "/api/v1/customers", map[string]any{"campo_inexistente": 1}); st != 422 || body["code"] != "invalid_json" {
		t.Fatalf("campo desconhecido: %d %v", st, body)
	}
	if st, _, _ := c.do(http.MethodGet, "/api/v1/customers?limit=101", nil); st != 422 {
		t.Fatalf("limite acima do máximo aceito: %d", st)
	}
	if st, _, _ := c.do(http.MethodGet, "/api/v1/customers?sort=document_number", nil); st != 422 {
		t.Fatalf("ordenação fora da allowlist aceita: %d", st)
	}
	big := []byte(`{"name":"` + strings.Repeat("a", 2<<20) + `"}`)
	if st, body, _ := c.do(http.MethodPost, "/api/v1/products", big); st != 413 {
		t.Fatalf("corpo acima do limite: %d %v", st, body)
	}
	st, cust, _ := c.do(http.MethodPost, "/api/v1/customers", map[string]any{"kind": "company", "name": "Empresa Teste", "document_number": "11222333000181"})
	if st != 201 {
		t.Fatalf("cliente: %d %v", st, cust)
	}
	id := cust["id"].(string)
	upd := map[string]any{"kind": "company", "name": "Empresa Renomeada", "document_number": "11222333000181"}
	if st, body, _ := c.do(http.MethodPatch, "/api/v1/customers/"+id, upd); st != 428 {
		t.Fatalf("sem If-Match: %d %v", st, body)
	}
	if st, _, _ := c.do(http.MethodPatch, "/api/v1/customers/"+id, upd, "If-Match", `"9"`); st != 412 {
		t.Fatalf("versão divergente: %d", st)
	}
	if st, body, h := c.do(http.MethodPatch, "/api/v1/customers/"+id, upd, "If-Match", `"1"`); st != 200 || h.Get("ETag") != `"2"` {
		t.Fatalf("atualização: %d %v %s", st, body, h.Get("ETag"))
	}
	// Idempotency-Key: obrigatória, repetição devolve o mesmo resultado,
	// conteúdo diferente com a mesma chave gera conflito.
	credit := map[string]any{"amount": 1500, "reason": "Cortesia de teste"}
	if st, _, _ := c.do(http.MethodPost, "/api/v1/customers/"+id+"/credits", credit); st != 422 {
		t.Fatalf("sem Idempotency-Key: %d", st)
	}
	key := "credito-" + uuid.NewString()
	st1, b1, _ := c.do(http.MethodPost, "/api/v1/customers/"+id+"/credits", credit, "Idempotency-Key", key)
	st2, b2, h2 := c.do(http.MethodPost, "/api/v1/customers/"+id+"/credits", credit, "Idempotency-Key", key)
	if st1 != 201 || st2 != 201 || h2.Get("Idempotent-Replayed") != "true" || b1["balance"] != b2["balance"] || b1["balance"] != float64(1500) {
		t.Fatalf("repetição idempotente: %d %v / %d %v", st1, b1, st2, b2)
	}
	if st, body, _ := c.do(http.MethodPost, "/api/v1/customers/"+id+"/credits", map[string]any{"amount": 9999, "reason": "Outro"}, "Idempotency-Key", key); st != 409 || body["code"] != "idempotency_key_reused" {
		t.Fatalf("mesma chave com outro conteúdo: %d %v", st, body)
	}
	_, bal, _ := c.do(http.MethodGet, "/api/v1/customers/"+id+"/credits", nil)
	if bal["balance"] != float64(1500) {
		t.Fatalf("crédito concedido mais de uma vez: %v", bal["balance"])
	}
	// Webhook para endpoint desconhecido.
	anon := &client{a: a}
	if st, _, _ := anon.do(http.MethodPost, "/webhooks/providers/simulator/"+strings.Repeat("z", 43), map[string]any{"id": "x"}); st != 404 {
		t.Fatalf("webhook desconhecido: %d", st)
	}
	// Exportação CSV auditada.
	if st, _, h := c.do(http.MethodGet, "/api/v1/invoices/export", nil); st != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/csv") {
		t.Fatalf("exportação: %d %s", st, h.Get("Content-Type"))
	}
}
