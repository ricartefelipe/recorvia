package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// accessTenancy resolve a associação ativa de um único usuário a um tenant.
type accessTenancy struct {
	tenancy.Store
	tenancy.Tx
	tenantID, userID uuid.UUID
	// role é o papel da associação (proprietário quando vazio).
	role tenancy.Role
}

func (s *accessTenancy) InTenant(_ context.Context, tenantID uuid.UUID, fn func(tenancy.Tx) error) error {
	if tenantID != s.tenantID {
		return fn(&accessTenancy{})
	}
	return fn(s)
}

func (s *accessTenancy) Tenant(context.Context) (tenancy.Tenant, error) {
	return tenancy.Tenant{ID: s.tenantID, Status: tenancy.TenantActive}, nil
}

func (s *accessTenancy) Membership(_ context.Context, userID uuid.UUID, _ bool) (tenancy.Membership, error) {
	role := s.role
	if role == "" {
		role = tenancy.RoleOwner
	}
	return tenancy.Membership{TenantID: s.tenantID, UserID: userID, Role: role, Status: tenancy.MembershipActive}, nil
}

// RNF-07: o log de acesso de uma requisição autenticada leva request_id,
// trace_id e span_id do span HTTP e o tenant_id e o user_id identificados
// pelas etapas internas (autenticação e validação do tenant), embora seja
// escrito pelo middleware externo.
func TestAccessLogCarriesTenantUserAndTrace(t *testing.T) {
	now := time.Date(2026, time.March, 2, 12, 0, 0, 0, time.UTC)
	clk := clock.NewManual(now)
	user := identity.User{ID: uuid.New(), DisplayName: "Ana", Status: "active"}
	tenantID := uuid.New()
	const sessionID = "sessao-log-acesso"
	sessions := &authSessions{items: map[string]identity.Session{sessionID: {ID: sessionID, UserID: user.ID, ActiveTenantID: &tenantID,
		CSRFToken: "csrf", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}}}
	ids := identity.NewService(nil, sessions, authUsers{user: user}, clk,
		identity.Policy{Absolute: 8 * time.Hour, Idle: time.Hour, TouchInterval: time.Minute})
	var logs bytes.Buffer
	log := logging.New(&logs, "info", "api")
	a := &API{Deps: Deps{Identity: ids, Tenancy: tenancy.NewService(&accessTenancy{tenantID: tenantID, userID: user.ID}, clk), Clock: clk,
		Log: log}, log: log}
	h := a.requestID(a.observe(a.authenticate(a.requireTenant(tenancy.PermCustomersRead, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))))

	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("teste").Start(t.Context(), "http")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/customers", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: "recorvia_session", Value: sessionID})
	req.Header.Set("X-Request-ID", "req-log-acesso-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	span.End()
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var access map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log não é JSON: %v (%s)", err, line)
		}
		if m["msg"] == "requisição" {
			access = m
		}
	}
	sc := span.SpanContext()
	want := map[string]any{"request_id": "req-log-acesso-01", "tenant_id": tenantID.String(), "user_id": user.ID.String(),
		"trace_id": sc.TraceID().String(), "span_id": sc.SpanID().String(), "status": float64(http.StatusNoContent)}
	for k, v := range want {
		if access[k] != v {
			t.Errorf("%s=%v, esperado %v (log %v)", k, access[k], v, access)
		}
	}
}

// accessLogLine devolve o registro do log de acesso ("requisição").
func accessLogLine(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	var access map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log não é JSON: %v (%s)", err, line)
		}
		if m["msg"] == "requisição" {
			access = m
		}
	}
	if access == nil {
		t.Fatalf("sem log de acesso: %s", logs.String())
	}
	return access
}

// A requisição recusada antes da validação do tenant (401 sem sessão, 403 sem
// a permissão da rota) leva no log de acesso o tenant pedido no header, com a
// chave requested_tenant_id: o pedido não foi validado e tenant_id continua
// reservado ao tenant validado. Header malformado não é repetido no log.
func TestAccessLogCarriesRequestedTenant(t *testing.T) {
	now := time.Date(2026, time.March, 2, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	other := uuid.New()
	cases := []struct {
		name       string
		session    bool
		role       tenancy.Role
		header     string
		wantStatus int
		// wantRequested e wantTenant vazios exigem a chave ausente.
		wantRequested, wantTenant string
	}{
		{name: "401 sem sessão", header: tenantID.String(), wantStatus: http.StatusUnauthorized, wantRequested: tenantID.String()},
		{name: "401 com o tenant em maiúsculas", header: strings.ToUpper(tenantID.String()), wantStatus: http.StatusUnauthorized,
			wantRequested: tenantID.String()},
		{name: "403 sem a permissão da rota", session: true, role: tenancy.RoleAuditor, header: tenantID.String(),
			wantStatus: http.StatusForbidden, wantRequested: tenantID.String()},
		{name: "409 com outro tenant no header", session: true, header: other.String(), wantStatus: http.StatusConflict,
			wantRequested: other.String()},
		{name: "header malformado não vai ao log", header: "nao-e-um-tenant\nnivel=erro", wantStatus: http.StatusUnauthorized},
		{name: "sem header", session: true, wantStatus: http.StatusNoContent, wantTenant: tenantID.String()},
		{name: "aceita: tenant validado e tenant pedido", session: true, header: tenantID.String(), wantStatus: http.StatusNoContent,
			wantRequested: tenantID.String(), wantTenant: tenantID.String()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := clock.NewManual(now)
			user := identity.User{ID: uuid.New(), DisplayName: "Ana", Status: "active"}
			const sessionID = "sessao-tenant-pedido"
			sessions := &authSessions{items: map[string]identity.Session{sessionID: {ID: sessionID, UserID: user.ID, ActiveTenantID: &tenantID,
				CSRFToken: "csrf", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}}}
			ids := identity.NewService(nil, sessions, authUsers{user: user}, clk,
				identity.Policy{Absolute: 8 * time.Hour, Idle: time.Hour, TouchInterval: time.Minute})
			var logs bytes.Buffer
			log := logging.New(&logs, "info", "api")
			a := &API{Deps: Deps{Identity: ids, Tenancy: tenancy.NewService(&accessTenancy{tenantID: tenantID, userID: user.ID, role: tc.role}, clk),
				Clock: clk, Log: log}, log: log}
			h := a.requestID(a.observe(a.authenticate(a.requireTenant(tenancy.PermCustomersWrite, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})))))
			req := httptest.NewRequest(http.MethodGet, "/api/v1/customers", nil)
			if tc.session {
				req.AddCookie(&http.Cookie{Name: "recorvia_session", Value: sessionID})
			}
			if tc.header != "" {
				req.Header.Set("X-Recorvia-Tenant", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, esperado %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			access := accessLogLine(t, &logs)
			for key, want := range map[string]string{"requested_tenant_id": tc.wantRequested, "tenant_id": tc.wantTenant} {
				got, present := access[key]
				if want == "" && present {
					t.Errorf("%s=%v, esperado ausente (log %v)", key, got, access)
				}
				if want != "" && got != want {
					t.Errorf("%s=%v, esperado %s (log %v)", key, got, want, access)
				}
			}
			if strings.Contains(logs.String(), "nivel=erro") {
				t.Errorf("o header malformado foi repetido no log: %s", logs.String())
			}
		})
	}
}
