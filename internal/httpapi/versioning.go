package httpapi

import (
	"net/http"
	"strings"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

// versionedRoutes são as alterações de recursos com versão (concorrência
// otimista, ADR-083): sem If-Match, a requisição é recusada com 428
// if_match_required antes do handler; com ele, o handler lê a versão
// (ifMatch ou ifMatchOrDefault) e o caso de uso grava condicionado a ela, na
// mesma transação (412 version_mismatch se o recurso mudou). O teste de
// contrato exige que o contrato declare o parâmetro IfMatch exatamente nestas
// operações.
var versionedRoutes = map[string]bool{
	"PATCH /api/v1/organization":                             true,
	"PATCH /api/v1/members/{user_id}":                        true,
	"DELETE /api/v1/members/{user_id}":                       true,
	"PATCH /api/v1/customers/{id}":                           true,
	"POST /api/v1/customers/{id}/archive":                    true,
	"PUT /api/v1/customers/{id}/billing-address":             true,
	"PATCH /api/v1/products/{id}":                            true,
	"PATCH /api/v1/plans/{id}":                               true,
	"POST /api/v1/plans/{id}/prices":                         true,
	"POST /api/v1/coupons/{id}/archive":                      true,
	"POST /api/v1/subscriptions/{id}/cancel":                 true,
	"POST /api/v1/subscriptions/{id}/resume-renewal":         true,
	"POST /api/v1/subscriptions/{id}/suspend":                true,
	"POST /api/v1/subscriptions/{id}/reactivate":             true,
	"POST /api/v1/subscriptions/{id}/change-plan":            true,
	"POST /api/v1/subscriptions/{id}/change-quantity":        true,
	"POST /api/v1/invoices/{id}/void":                        true,
	"POST /api/v1/invoices/{id}/mark-uncollectible":          true,
	"PUT /api/v1/payment-providers/{provider}":               true,
	"POST /api/v1/api-credentials/{id}/rotate":               true,
	"DELETE /api/v1/api-credentials/{id}":                    true,
	"POST /api/v1/reconciliation/discrepancies/{id}/resolve": true,
	"PATCH /api/v1/collection-policies/{id}":                 true,
	"PUT /api/v1/notification-templates/{key}":               true,
	"PATCH /api/v1/webhook-endpoints/{id}":                   true,
	"DELETE /api/v1/webhook-endpoints/{id}":                  true,
	"POST /api/v1/webhook-endpoints/{id}/rotate-secret":      true,

	// Troca do plano da plataforma e atendimento dos pedidos de serviços
	// adicionais pela equipe da plataforma (ADR-085).
	"PUT /api/v1/platform/plan":                                         true,
	"POST /api/v1/admin/tenants/{tenant_id}/addon-orders/{id}/schedule": true,
	"POST /api/v1/admin/tenants/{tenant_id}/addon-orders/{id}/complete": true,
	"POST /api/v1/admin/tenants/{tenant_id}/addon-orders/{id}/cancel":   true,
}

// requireIfMatch recusa com 428 a alteração de recurso versionado enviada sem
// If-Match. O formato do valor é conferido pelo handler.
func (a *API) requireIfMatch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get("If-Match")) == "" {
			a.writeError(w, r, errIfMatchRequired)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var errIfMatchRequired = &apperr.Error{Kind: apperr.KindPreconditionFailed, Code: "if_match_required",
	Message: "Informe a versão do recurso no header If-Match."}

// ifMatchOrDefault lê a versão esperada de um recurso que pode ainda não ter
// sido gravado: "0" é a versão do template padrão ainda não personalizado e
// do provedor de pagamento ainda não configurado; os demais valores seguem
// ifMatch.
func ifMatchOrDefault(r *http.Request) (int, error) {
	if strings.Trim(strings.TrimPrefix(strings.TrimSpace(r.Header.Get("If-Match")), "W/"), `"`) == "0" {
		return 0, nil
	}
	return ifMatch(r)
}

// requestRules aplica as regras de parâmetros e pré-condições da rota antes
// do handler: a ordenação em allowlist das listagens (listRoutes) e a
// exigência de If-Match das alterações versionadas (versionedRoutes).
func (a *API) requestRules(method, pattern string, h http.Handler) http.Handler {
	key := method + " " + pattern
	if versionedRoutes[key] {
		h = a.requireIfMatch(h)
	}
	if sorts, ok := listRoutes[key]; ok {
		h = a.requireAllowedSort(sorts, h)
	}
	return h
}
