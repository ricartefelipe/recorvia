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
	return tenancy.Membership{TenantID: s.tenantID, UserID: userID, Role: tenancy.RoleOwner, Status: tenancy.MembershipActive}, nil
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
