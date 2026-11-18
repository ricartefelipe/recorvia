package httpapi

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/ricartefelipe/recorvia/api"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

// pendingOperations lista operações do contrato ainda não implementadas nesta
// versão. A lista só pode diminuir: o teste falha se uma operação
// implementada continuar aqui ou se o contrato tiver operação sem handler
// e fora desta lista.
var pendingOperations = map[string]bool{
	"POST /api/v1/charges/{id}/refunds": true, "GET /api/v1/refunds": true, "GET /api/v1/disputes": true,
	"GET /api/v1/reconciliation/runs": true, "POST /api/v1/reconciliation/runs": true,
	"GET /api/v1/reconciliation/discrepancies": true, "GET /api/v1/reconciliation/discrepancies/{id}": true,
	"POST /api/v1/reconciliation/discrepancies/{id}/resolve": true,
	"GET /api/v1/collection-policies":                        true, "POST /api/v1/collection-policies": true, "PATCH /api/v1/collection-policies/{id}": true,
	"GET /api/v1/notification-templates": true, "PUT /api/v1/notification-templates/{key}": true, "GET /api/v1/notifications": true,
	"GET /api/v1/webhook-event-types": true, "GET /api/v1/webhook-endpoints": true, "POST /api/v1/webhook-endpoints": true,
	"PATCH /api/v1/webhook-endpoints/{id}": true, "DELETE /api/v1/webhook-endpoints/{id}": true,
	"POST /api/v1/webhook-endpoints/{id}/rotate-secret": true, "GET /api/v1/webhook-endpoints/{id}/deliveries": true,
	"POST /api/v1/webhook-deliveries/{id}/retry": true,
}

// Rotas utilitárias fora do contrato de negócio.
var unlistedRoutes = map[string]bool{"GET /api/openapi.yaml": true}

func specOperations(t *testing.T) map[string]*openapi3.Operation {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ops := map[string]*openapi3.Operation{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			ops[strings.ToUpper(method)+" "+path] = op
		}
	}
	return ops
}

func TestRoutesMatchOpenAPIContract(t *testing.T) {
	a, _ := NewRouter(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.System{}})
	ops := specOperations(t)
	registered := map[string]RouteInfo{}
	for _, r := range a.Routes() {
		registered[r.Method+" "+r.Pattern] = r
	}
	registered["GET /livez"], registered["GET /readyz"] = RouteInfo{}, RouteInfo{}
	var problems []string
	for key, info := range registered {
		if unlistedRoutes[key] {
			continue
		}
		op, ok := ops[key]
		if !ok {
			problems = append(problems, "rota sem contrato: "+key)
			continue
		}
		if pendingOperations[key] {
			problems = append(problems, "rota implementada ainda marcada como pendente: "+key)
		}
		if info.Access == TenantScoped && info.Permission == "" {
			problems = append(problems, "rota de tenant sem permissão declarada: "+key)
		}
		needsIdem := false
		for _, p := range op.Parameters {
			if p.Value != nil && p.Value.Name == "Idempotency-Key" {
				needsIdem = true
			}
		}
		if needsIdem != info.Idempotent {
			problems = append(problems, "Idempotency-Key divergente entre contrato e rota: "+key)
		}
	}
	for key := range ops {
		if _, ok := registered[key]; !ok && !pendingOperations[key] {
			problems = append(problems, "operação do contrato sem handler: "+key)
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

func TestEveryRouteDeclaresAccess(t *testing.T) {
	a, _ := NewRouter(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.System{}})
	public := map[string]bool{"GET /auth/login": true, "GET /auth/callback": true, "POST /webhooks/providers/{provider}/{token}": true,
		"GET /portal/access": true}
	for _, r := range a.Routes() {
		key := r.Method + " " + r.Pattern
		if r.Access == Public && !public[key] {
			t.Errorf("rota pública não prevista: %s", key)
		}
	}
}
