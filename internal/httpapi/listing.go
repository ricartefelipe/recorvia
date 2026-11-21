package httpapi

import (
	"net/http"
	"slices"
	"strings"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

// listRoutes são as listagens do contrato, com a allowlist de ordenação de
// cada uma (ADR-083). Uma listagem sem allowlist não ordena por parâmetro:
// sort informado é recusado com 422 invalid_sort, em vez de ignorado, assim
// como um valor fora da allowlist. A ordem de cada listagem é determinística,
// com desempate pelo identificador. O teste de contrato exige que toda
// listagem do contrato esteja aqui, que a allowlist coincida com o enum do
// parâmetro sort do contrato e que a listagem declare a resposta 422.
var listRoutes = map[string][]string{
	"GET /api/v1/roles":                                nil,
	"GET /api/v1/members":                              nil,
	"GET /api/v1/invitations":                          nil,
	"GET /api/v1/platform/plans":                       nil,
	"GET /api/v1/platform/addons":                      nil,
	"GET /api/v1/platform/addon-orders":                nil,
	"GET /api/v1/platform/statements":                  nil,
	"GET /api/v1/admin/tenants":                        nil,
	"GET /api/v1/admin/addon-orders":                   nil,
	"GET /api/v1/customers":                            {"name", "-name", "created_at", "-created_at"},
	"GET /api/v1/customers/{id}/history":               nil,
	"GET /api/v1/customers/{id}/credits":               nil,
	"GET /api/v1/customers/{id}/portal-links":          nil,
	"GET /api/v1/products":                             nil,
	"GET /api/v1/metrics":                              nil,
	"GET /api/v1/plans":                                nil,
	"GET /api/v1/coupons":                              nil,
	"GET /api/v1/subscriptions":                        nil,
	"GET /api/v1/subscriptions/{id}/history":           nil,
	"GET /api/v1/subscriptions/{id}/price-adjustments": nil,
	"GET /api/v1/usage-events":                         nil,
	"GET /api/v1/invoices":                             nil,
	"GET /api/v1/charges":                              nil,
	"GET /api/v1/refunds":                              nil,
	"GET /api/v1/payments":                             nil,
	"GET /api/v1/disputes":                             nil,
	"GET /api/v1/disputes/{id}/recovery-settlements":   nil,
	"GET /api/v1/payment-operations":                   nil,
	"GET /api/v1/payment-providers":                    nil,
	"GET /api/v1/ledger/accounts":                      nil,
	"GET /api/v1/ledger/transactions":                  nil,
	"GET /api/v1/reconciliation/runs":                  nil,
	"GET /api/v1/reconciliation/discrepancies":         nil,
	"GET /api/v1/collection-policies":                  nil,
	"GET /api/v1/notification-templates":               nil,
	"GET /api/v1/notifications":                        nil,
	"GET /api/v1/notifications/{id}/events":            nil,
	"GET /api/v1/webhook-event-types":                  nil,
	"GET /api/v1/webhook-endpoints":                    nil,
	"GET /api/v1/webhook-endpoints/{id}/deliveries":    nil,
	"GET /api/v1/webhook-deliveries/{id}/attempts":     nil,
	"GET /api/v1/api-credentials":                      nil,
	"GET /api/v1/dead-letters":                         nil,
	"GET /api/v1/audit-events":                         nil,
	"GET /api/portal/v1/invoices":                      nil,
}

// requireAllowedSort recusa a listagem pedida com ordenação fora da allowlist
// (ou com mais de uma ordenação) com 422 invalid_sort, antes do handler.
func (a *API) requireAllowedSort(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values, present := r.URL.Query()["sort"]
		if present && (len(values) != 1 || !slices.Contains(allowed, values[0])) {
			a.writeError(w, r, invalidSort(allowed))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func invalidSort(allowed []string) error {
	msg := "Esta listagem não aceita ordenação."
	if len(allowed) > 0 {
		msg = "Use " + strings.Join(allowed, ", ") + "."
	}
	return apperr.Invalid("invalid_sort", "Ordenação não permitida.", apperr.FieldError{Field: "sort", Message: msg})
}

// pageParams lê o limite (1 a 100, padrão 25) e o cursor opaco da página.
func pageParams(r *http.Request) (int, string, error) {
	limit, err := queryLimit(r)
	if err != nil {
		return 0, "", err
	}
	return limit, r.URL.Query().Get("cursor"), nil
}
