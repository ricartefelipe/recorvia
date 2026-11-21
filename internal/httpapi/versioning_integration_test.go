//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
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

// A troca do plano da plataforma exige If-Match com a versão da assinatura da
// plataforma (subscription.version da visão geral): sem ele, 428; com versão
// divergente, 412, sem troca nem auditoria, inclusive para o plano atual.
func TestPlatformPlanChangeOverHTTP(t *testing.T) {
	a := newAPI(t)
	tn := apiPlanTenant(t, a, "plano-versao")
	owner := a.login(tn.Owner, &tn.ID)
	ov, h := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/platform/overview", nil)
	version := ov["subscription"].(map[string]any)["version"].(float64)
	if h.Get("ETag") != fmt.Sprintf(`"%v"`, version) {
		t.Fatalf("visão geral com ETag %q, versão %v", h.Get("ETag"), version)
	}
	audits := func() int {
		t.Helper()
		var n int
		if err := pgtest.OwnerPool(t).QueryRow(context.Background(), `SELECT count(*)::int FROM audit_log WHERE tenant_id = $1
			AND action = 'platform.plan_changed'`, tn.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := audits()
	body := map[string]any{"plan_code": "escala"}
	owner.expectProblem(http.StatusPreconditionRequired, "if_match_required", http.MethodPut, "/api/v1/platform/plan", body)
	owner.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPut, "/api/v1/platform/plan", body, matchVersion(version+1)...)
	owner.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPut, "/api/v1/platform/plan",
		map[string]any{"plan_code": "profissional"}, matchVersion(version-1)...)
	got, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/platform/overview", nil)
	if sub := got["subscription"].(map[string]any); sub["plan_code"] != "profissional" || sub["version"] != version || audits() != before {
		t.Fatalf("troca recusada alterou a assinatura: %v (auditorias %d, antes %d)", sub, audits(), before)
	}
	changed, h := owner.expect(http.StatusOK, http.MethodPut, "/api/v1/platform/plan", body, matchVersion(version)...)
	sub := changed["subscription"].(map[string]any)
	if sub["plan_code"] != "escala" || sub["version"] != version+1 || h.Get("ETag") != fmt.Sprintf(`"%v"`, version+1) || audits() != before+1 {
		t.Fatalf("plano trocado: %v (ETag %q)", sub, h.Get("ETag"))
	}
}

// O catálogo (produtos, métricas, planos e cupons), os pedidos de serviço
// adicional e os demonstrativos da organização e os reajustes de uma
// assinatura seguem o next_cursor (ADR-085): de um em um, as páginas trazem
// os mesmos itens e na mesma ordem da listagem numa única página.
func TestCatalogAndPlatformListsOverHTTP(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := apiPlanTenant(t, a, "listas")
	ctx := context.Background()
	// Demonstrativos de períodos encerrados, antes da sessão (o relógio
	// avança meses).
	ov, err := a.env.Platform.Overview(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.env.Clock.Set(ov.Subscription.PeriodEnd.AddDate(0, 2, 1))
	// O fechamento é de todos os tenants com período encerrado; os deste
	// tenant são contados no armazenamento.
	if _, err := a.env.Platform.CloseDueStatements(ctx); err != nil {
		t.Fatal(err)
	}
	closed, _, err := a.env.Store.Platform().Statements(ctx, tn.ID, 100, "")
	if err != nil || len(closed) < 3 {
		t.Fatalf("demonstrativos fechados: %d %v", len(closed), err)
	}
	owner := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano reajustado", flatPrice(10000))
	sub := newSubscription(t, a.env, tn, plan.ID, 1, nil)
	adjustments := "/api/v1/subscriptions/" + sub.ID.String() + "/price-adjustments"
	today := tenantToday(t, a.env)
	productID := ""
	for i := range 3 {
		a.env.Clock.Advance(time.Second)
		prod, _ := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/products", map[string]any{"name": fmt.Sprintf("Produto %d", i)})
		productID = prod["id"].(string)
		owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/plans", map[string]any{"product_id": productID, "code": fmt.Sprintf("plano-%d", i),
			"name": fmt.Sprintf("Plano %d", i), "billing_interval": "month", "price": map[string]any{"pricing_model": "flat", "flat_amount": 1000}})
		owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/metrics", map[string]any{"code": fmt.Sprintf("chamadas.%d", 2-i),
			"name": "Chamadas", "unit": "chamada", "aggregation": "sum", "max_quantity_per_event": "1000"})
		owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/coupons", map[string]any{"code": fmt.Sprintf("CUPOM%d", i), "name": "Cupom",
			"discount_type": "percent", "percent_off_bp": 1000, "duration": "once"})
		owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/platform/addon-orders", map[string]any{"addon_code": "implantacao_assistida"},
			"Idempotency-Key", idemKey())
		owner.expect(http.StatusCreated, http.MethodPost, adjustments, map[string]any{"effective_date": today.AddDays(90 - 30*i).String(),
			"kind": "percent", "percent_bp": 100 * (i + 1), "reason": "Reajuste programado"}, "Idempotency-Key", idemKey())
	}

	field := func(items []map[string]any, name string) []any {
		out := make([]any, 0, len(items))
		for _, it := range items {
			out = append(out, it[name])
		}
		return out
	}
	for _, tc := range []struct {
		path, field string
		want        []any
	}{
		// Produtos e planos: dos criados mais recentemente para os mais antigos.
		{"/api/v1/products", "name", []any{"Produto 2", "Produto 1", "Produto 0", "Produto Plano reajustado"}},
		{"/api/v1/products?status=active", "name", []any{"Produto 2", "Produto 1", "Produto 0", "Produto Plano reajustado"}},
		{"/api/v1/plans", "name", []any{"Plano 2", "Plano 1", "Plano 0", "Plano reajustado"}},
		{"/api/v1/plans?product_id=" + productID, "name", []any{"Plano 2"}},
		// Métricas pelo código.
		{"/api/v1/metrics", "code", []any{"chamadas.0", "chamadas.1", "chamadas.2"}},
		{"/api/v1/coupons", "code", []any{"CUPOM2", "CUPOM1", "CUPOM0"}},
		// Reajustes pela vigência.
		{adjustments, "percent_bp", []any{float64(300), float64(200), float64(100)}},
	} {
		if got := field(pageAll(t, owner, tc.path, pageData(t)), tc.field); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, esperado %v", tc.path, got, tc.want)
		}
	}
	if got := pageAll(t, owner, "/api/v1/platform/addon-orders", pageData(t)); len(got) != 3 {
		t.Errorf("pedidos de serviço adicional: %d", len(got))
	}
	statements := pageAll(t, owner, "/api/v1/platform/statements", pageData(t))
	starts := make([]time.Time, 0, len(statements))
	for _, st := range statements {
		at, err := time.Parse(time.RFC3339, st["period_start"].(string))
		if err != nil {
			t.Fatal(err)
		}
		starts = append(starts, at)
	}
	if len(statements) != len(closed) || !slices.IsSortedFunc(starts, func(x, y time.Time) int { return y.Compare(x) }) {
		t.Errorf("demonstrativos, do período mais recente: %v", field(statements, "period_start"))
	}

	for _, path := range []string{"/api/v1/products", "/api/v1/plans", "/api/v1/metrics", "/api/v1/coupons", "/api/v1/platform/addon-orders",
		"/api/v1/platform/statements", adjustments} {
		owner.expectProblem(http.StatusUnprocessableEntity, "invalid_sort", http.MethodGet, path+"?sort=name", nil)
		owner.expectProblem(http.StatusUnprocessableEntity, "invalid_limit", http.MethodGet, path+"?limit=0", nil)
		owner.expectProblem(http.StatusUnprocessableEntity, "invalid_limit", http.MethodGet, path+"?limit=101", nil)
		owner.expectProblem(http.StatusUnprocessableEntity, "invalid_cursor", http.MethodGet, path+"?cursor=abc", nil)
	}
	// O padrão é 25 itens por página.
	for i := range 23 {
		owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/products", map[string]any{"name": fmt.Sprintf("Produto extra %d", i)})
	}
	first, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/products", nil)
	if n := len(objects(t, first["data"])); n != 25 || first["next_cursor"] == nil {
		t.Fatalf("primeira página padrão: %d itens, next_cursor %v", n, first["next_cursor"])
	}
	rest, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/products?cursor="+url.QueryEscape(first["next_cursor"].(string)), nil)
	if n := len(objects(t, rest["data"])); n != 2 || rest["next_cursor"] != nil {
		t.Fatalf("segunda página: %d itens, next_cursor %v", n, rest["next_cursor"])
	}
}
