//go:build integration

package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// secretFields procura, em qualquer nível da resposta, campos que exporiam
// as credenciais do provedor.
func secretFields(v any, path string) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "api_key" || k == "webhook_secret" || k == "secret" || strings.Contains(k, "ciphertext") {
				out = append(out, path+"."+k)
			}
			out = append(out, secretFields(val, path+"."+k)...)
		}
	case []any:
		for _, val := range x {
			out = append(out, secretFields(val, path+"[]")...)
		}
	}
	return out
}

// PUT e GET de /api/v1/payment-providers nunca devolvem a chave de API nem o
// segredo do webhook: nem os campos, nem os valores em qualquer outro campo.
// Só a dica (últimos 4 caracteres da chave) é exposta.
func TestPaymentProvidersNeverReturnCredentials(t *testing.T) {
	r := newReceivablesAPI(t)
	tn := r.env.NewTenant(t, "provedor-segredos")
	owner := r.login(tn.Owner, &tn.ID)
	check := func(name string, body map[string]any, secrets ...string) {
		t.Helper()
		if f := secretFields(body, "$"); len(f) != 0 {
			t.Fatalf("%s expõe campos de credencial: %v", name, f)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range secrets {
			if strings.Contains(string(raw), s) {
				t.Fatalf("%s contém uma credencial: %s", name, raw)
			}
		}
	}

	key1, secret1 := "sim_chave_"+uuid.NewString(), "segredo-do-webhook-"+uuid.NewString()
	put := func(key, secret string) map[string]any {
		out, _ := owner.expect(http.StatusOK, http.MethodPut, "/api/v1/payment-providers/simulator", map[string]any{
			"environment": "local", "api_key": key, "webhook_secret": secret, "make_default": true})
		account, _ := out["account"].(map[string]any)
		if account == nil || account["api_key_hint"] != key[len(key)-4:] || out["webhook_endpoint_url"] == nil {
			t.Fatalf("configuração: %v", out)
		}
		return out
	}
	check("PUT", put(key1, secret1), key1, secret1)
	listed, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/payment-providers", nil)
	if n := len(jsonArray(t, listed["data"])); n != 1 {
		t.Fatalf("contas listadas: %d", n)
	}
	check("GET", listed, key1, secret1)

	// Reconfiguração com credenciais novas: nenhuma das duas aparece.
	key2, secret2 := "sim_rotacionada_"+uuid.NewString(), "segredo-novo-do-webhook-"+uuid.NewString()
	check("PUT de reconfiguração", put(key2, secret2), key1, secret1, key2, secret2)
	listed, _ = owner.expect(http.StatusOK, http.MethodGet, "/api/v1/payment-providers", nil)
	check("GET após reconfiguração", listed, key1, secret1, key2, secret2)

	// A recusa de validação também não ecoa o que foi enviado.
	st, problem, _ := owner.do(http.MethodPut, "/api/v1/payment-providers/simulator", map[string]any{
		"environment": "local", "api_key": key2, "webhook_secret": "curto", "make_default": true})
	if st < 400 {
		t.Fatalf("segredo curto aceito: %d %v", st, problem)
	}
	check("PUT recusado", problem, key2, "curto\"")
}
