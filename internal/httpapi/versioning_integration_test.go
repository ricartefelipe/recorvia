//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// matchVersion é o header If-Match com a versão informada (número ou campo
// version de uma resposta JSON).
func matchVersion(v any) []string { return []string{"If-Match", `"` + fmt.Sprint(v) + `"`} }

// versionOf lê a versão atual do recurso no GET informado.
func (c *client) versionOf(path string) any {
	c.a.t.Helper()
	out, _ := c.expect(http.StatusOK, http.MethodGet, path, nil)
	if out["version"] == nil {
		c.a.t.Fatalf("GET %s sem version: %v", path, out)
	}
	return out["version"]
}

// versionIn lê a versão atual do item da listagem com o identificador
// informado no campo field.
func (c *client) versionIn(listPath, field, id string) any {
	c.a.t.Helper()
	out, _ := c.expect(http.StatusOK, http.MethodGet, listPath, nil)
	for _, item := range jsonArray(c.a.t, out["data"]) {
		if m, ok := item.(map[string]any); ok && m[field] == id {
			return m["version"]
		}
	}
	c.a.t.Fatalf("%s sem o item %s", listPath, id)
	return nil
}

// pageAll percorre a listagem de um em um item pelo next_cursor e confere que
// as páginas trazem, sem repetição nem lacuna, os mesmos itens e na mesma
// ordem da listagem numa única página; items extrai os itens da resposta.
func pageAll(t *testing.T, c *client, path string, items func(map[string]any) []map[string]any) []map[string]any {
	t.Helper()
	sep := "?"
	if u, err := url.Parse(path); err == nil && u.RawQuery != "" {
		sep = "&"
	}
	whole, _ := c.expect(http.StatusOK, http.MethodGet, path+sep+"limit=100", nil)
	if whole["next_cursor"] != nil {
		t.Fatalf("%s: listagem completa com next_cursor: %v", path, whole["next_cursor"])
	}
	var paged []map[string]any
	cursor := ""
	for range 100 {
		target := path + sep + "limit=1"
		if cursor != "" {
			target += "&cursor=" + url.QueryEscape(cursor)
		}
		page, _ := c.expect(http.StatusOK, http.MethodGet, target, nil)
		got := items(page)
		if len(got) > 1 {
			t.Fatalf("%s: página com %d itens e limite 1", target, len(got))
		}
		paged = append(paged, got...)
		next, _ := page["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	want := items(whole)
	ids := func(list []map[string]any) []any {
		out := make([]any, 0, len(list))
		for _, m := range list {
			out = append(out, m["id"])
		}
		return out
	}
	if !slices.Equal(ids(paged), ids(want)) {
		t.Fatalf("%s: páginas %v, listagem completa %v", path, ids(paged), ids(want))
	}
	return want
}

func pageData(t *testing.T) func(map[string]any) []map[string]any {
	return func(body map[string]any) []map[string]any { return objects(t, body["data"]) }
}

// Alterações versionadas pela API: sem If-Match, 428; com versão divergente,
// 412 e nada muda; na versão atual, a resposta traz o ETag da versão gravada.
func TestVersionedAlterationsOverHTTP(t *testing.T) {
	a := newAPI(t)
	tn := apiPlanTenant(t, a, "versoes")
	owner := a.login(tn.Owner, &tn.ID)

	t.Run("membros", func(t *testing.T) {
		m := member(t, a.env, tn, tenancy.RoleFinance)
		path := "/api/v1/members/" + m.ID.String()
		owner.expectProblem(http.StatusPreconditionRequired, "if_match_required", http.MethodPatch, path, map[string]any{"role": "operator"})
		owner.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPatch, path, map[string]any{"role": "operator"},
			matchVersion(2)...)
		changed, h := owner.expect(http.StatusOK, http.MethodPatch, path, map[string]any{"role": "operator"},
			matchVersion(owner.versionIn("/api/v1/members", "user_id", m.ID.String()))...)
		if changed["role"] != "operator" || h.Get("ETag") != fmt.Sprintf(`"%v"`, changed["version"]) {
			t.Fatalf("papel alterado: %v (ETag %q)", changed, h.Get("ETag"))
		}
		owner.expectProblem(http.StatusPreconditionRequired, "if_match_required", http.MethodDelete, path, nil)
		owner.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodDelete, path, nil, matchVersion(1)...)
		if v := owner.versionIn("/api/v1/members", "user_id", m.ID.String()); v != changed["version"] {
			t.Fatalf("remoção recusada alterou o membro: versão %v", v)
		}
		owner.expect(http.StatusNoContent, http.MethodDelete, path, nil, matchVersion(changed["version"])...)
	})

	t.Run("arquivamento de cliente", func(t *testing.T) {
		cust := a.env.NewCustomer(t, tn)
		path := "/api/v1/customers/" + cust.ID.String()
		owner.expectProblem(http.StatusPreconditionRequired, "if_match_required", http.MethodPost, path+"/archive", nil)
		owner.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPost, path+"/archive", nil, matchVersion(2)...)
		if got, _ := owner.expect(http.StatusOK, http.MethodGet, path, nil); got["status"] != "active" {
			t.Fatalf("arquivamento recusado alterou o cliente: %v", got)
		}
		archived, h := owner.expect(http.StatusOK, http.MethodPost, path+"/archive", nil, matchVersion(1)...)
		if archived["status"] != "archived" || archived["version"] != float64(2) || h.Get("ETag") != `"2"` {
			t.Fatalf("cliente arquivado: %v (ETag %q)", archived, h.Get("ETag"))
		}
		owner.expect(http.StatusOK, http.MethodPost, path+"/archive", nil, matchVersion(2)...)
		owner.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPost, path+"/archive", nil, matchVersion(1)...)
	})

	t.Run("nova versão de preço", func(t *testing.T) {
		plan := newPlan(t, a.env, tn, "Plano versionado", flatPrice(10000))
		path := "/api/v1/plans/" + plan.ID.String()
		price := map[string]any{"pricing_model": "flat", "flat_amount": 12000}
		owner.expectProblem(http.StatusPreconditionRequired, "if_match_required", http.MethodPost, path+"/prices", price)
		owner.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPost, path+"/prices", price, matchVersion(7)...)
		got, _ := owner.expect(http.StatusOK, http.MethodGet, path, nil)
		if n := len(jsonArray(t, got["prices"])); n != 1 {
			t.Fatalf("versão de preço gravada com a versão divergente do plano: %d versões", n)
		}
		_, h := owner.expect(http.StatusCreated, http.MethodPost, path+"/prices", price, matchVersion(got["version"])...)
		if want := fmt.Sprintf(`"%v"`, got["version"].(float64)+1); h.Get("ETag") != want || owner.versionOf(path) != got["version"].(float64)+1 {
			t.Fatalf("ETag da nova versão de preço: %q, esperado %s", h.Get("ETag"), want)
		}
	})
}

// As listagens que crescem com a operação seguem o next_cursor: convites,
// movimentos de crédito, credenciais e mensagens não processadas, com limite
// validado e ordenação fora da allowlist recusada.
func TestPaginatedListsOverHTTP(t *testing.T) {
	a := newAPI(t)
	tn := apiPlanTenant(t, a, "paginas")
	owner := a.login(tn.Owner, &tn.ID)
	for i := range 3 {
		owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/invitations", map[string]any{"email": fmt.Sprintf("pessoa%d@paginas.test", i),
			"role": "operator"})
		a.env.Clock.Advance(1)
		createCredential(t, owner, "customers.read")
		if err := a.env.Store.RecordDeadLetter(context.Background(), pgstore.DeadLetter{ID: uuid.New(), TenantID: &tn.ID,
			Consumer: "webhooks.dispatch", MessageID: uuid.NewString(), Error: "destino indisponível", Attempts: 5, FailedAt: a.env.Clock.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	cust := a.env.NewCustomer(t, tn)
	creditsPath := "/api/v1/customers/" + cust.ID.String() + "/credits"
	for i := range 3 {
		owner.expect(http.StatusCreated, http.MethodPost, creditsPath, map[string]any{"amount": 100 * (i + 1), "reason": "Cortesia de teste"},
			"Idempotency-Key", idemKey())
		a.env.Clock.Advance(1)
	}

	for _, path := range []string{"/api/v1/invitations", "/api/v1/api-credentials", "/api/v1/dead-letters", "/api/v1/dead-letters?status=open"} {
		if got := pageAll(t, owner, path, pageData(t)); len(got) != 3 {
			t.Fatalf("%s: %d itens", path, len(got))
		}
	}
	entries := func(body map[string]any) []map[string]any {
		if body["balance"] != float64(600) {
			t.Fatalf("saldo em todas as páginas: %v", body["balance"])
		}
		return objects(t, body["entries"])
	}
	if got := pageAll(t, owner, creditsPath, entries); len(got) != 3 || got[0]["amount"] != float64(300) {
		t.Fatalf("movimentos de crédito, do mais recente: %v", got)
	}
	for _, path := range []string{"/api/v1/invitations", "/api/v1/api-credentials", "/api/v1/dead-letters", creditsPath} {
		owner.expectProblem(http.StatusUnprocessableEntity, "invalid_sort", http.MethodGet, path+"?sort=created_at", nil)
		owner.expectProblem(http.StatusUnprocessableEntity, "invalid_limit", http.MethodGet, path+"?limit=101", nil)
		owner.expectProblem(http.StatusUnprocessableEntity, "invalid_cursor", http.MethodGet, path+"?cursor=abc", nil)
	}
}
