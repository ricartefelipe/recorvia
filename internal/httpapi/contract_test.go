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
var pendingOperations = map[string]bool{}

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
		problems = append(problems, userSessionContract(key, info, op)...)
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

// userSessionContract confere o contrato da autenticação da rota: as rotas
// restritas à sessão de usuário declaram apenas o esquema session e a resposta
// 403 user_session_required; as rotas de tenant que aceitam credenciais
// herdam a segurança global (sessão ou credencial de API).
func userSessionContract(key string, info RouteInfo, op *openapi3.Operation) []string {
	var problems []string
	sessionOnly := op.Security != nil && len(*op.Security) == 1 && len((*op.Security)[0]) == 1
	if sessionOnly {
		_, sessionOnly = (*op.Security)[0]["session"]
	}
	switch {
	case info.UserSession:
		if !sessionOnly {
			problems = append(problems, "rota restrita à sessão de usuário sem security session exclusivo no contrato: "+key)
		}
		if r := op.Responses.Value("403"); r == nil || r.Ref != "#/components/responses/UserSessionRequired" {
			problems = append(problems, "rota restrita à sessão de usuário sem a resposta 403 UserSessionRequired no contrato: "+key)
		}
	case info.Access == TenantScoped && op.Security != nil:
		problems = append(problems, "rota de tenant que aceita credencial de API com security próprio no contrato: "+key)
	}
	return problems
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
