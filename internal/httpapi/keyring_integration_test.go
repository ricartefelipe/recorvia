//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// Com as credenciais do provedor cifradas por uma chave que a instalação não
// tem mais (como depois de uma restauração sem a chave), as operações que
// usam a conta respondem 503 provider_credentials_unreadable, com mensagem
// que aponta as chaves de cifragem, em vez de 500 internal_error; a listagem
// continua disponível e a conta pode ser configurada de novo pela API, o que
// restabelece as operações.
func TestProviderCredentialsUnreadableOverHTTP(t *testing.T) {
	r := newReceivablesAPI(t)
	tn := r.newTenant(t, "chave-perdida")
	owner := r.login(tn.Owner, &tn.ID)
	if _, err := pgtest.OwnerPool(t).Exec(context.Background(),
		`UPDATE payment_provider_accounts SET api_key_key_id = 'chave-perdida', webhook_secret_key_id = 'chave-perdida' WHERE tenant_id = $1`,
		tn.ID); err != nil {
		t.Fatal(err)
	}

	status, problem, err := owner.send(http.MethodPost, "/api/v1/reconciliation/runs", nil, "Idempotency-Key", "chave-perdida-"+uuid.NewString())
	if err != nil || status != http.StatusServiceUnavailable || problem["code"] != "provider_credentials_unreadable" {
		t.Fatalf("resposta = %d %v (%v), esperado 503 provider_credentials_unreadable", status, problem, err)
	}
	detail, _ := problem["detail"].(string)
	if !strings.Contains(detail, "chaves de cifragem") || !strings.Contains(detail, "configure o provedor novamente") {
		t.Fatalf("mensagem sem a orientação esperada: %q", detail)
	}

	listed, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/payment-providers", nil)
	accounts := jsonArray(t, listed["data"])
	if len(accounts) != 1 {
		t.Fatalf("a listagem (sem segredos) deveria continuar disponível: %v", listed)
	}
	accountID := accounts[0].(map[string]any)["id"]

	newKey := "sim_reconfigurada_" + uuid.NewString()[:8]
	configured, _ := owner.expect(http.StatusOK, http.MethodPut, "/api/v1/payment-providers/simulator", map[string]any{
		"environment": "local", "api_key": newKey, "webhook_secret": strings.Repeat("n", 40), "make_default": true},
		matchVersion(accounts[0].(map[string]any)["version"])...)
	account, _ := configured["account"].(map[string]any)
	if account["id"] != accountID || account["api_key_hint"] != newKey[len(newKey)-4:] {
		t.Fatalf("a reconfiguração deveria manter a conta com a chave nova: %v", configured)
	}

	// A conciliação é pedida pela API e executada pelo worker (ADR-071).
	run := r.reconcile(t, owner, tn.ID, "chave-nova-"+uuid.NewString())
	if run["status"] != "completed" {
		t.Fatalf("a conciliação deveria voltar a funcionar depois da reconfiguração: %v", run)
	}
}
