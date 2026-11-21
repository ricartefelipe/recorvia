//go:build integration

package httpapi_test

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// custHistory percorre o histórico do cliente com o limite informado e
// devolve os eventos na ordem da API e o número de páginas.
func custHistory(c *client, customerID string, limit int) ([]map[string]any, int) {
	c.a.t.Helper()
	var all []map[string]any
	cursor, pages := "", 0
	for {
		path := fmt.Sprintf("/api/v1/customers/%s/history?limit=%d", customerID, limit)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		out, _ := c.expect(http.StatusOK, http.MethodGet, path, nil)
		pages++
		page := objects(c.a.t, out["data"])
		if len(page) > limit {
			c.a.t.Fatalf("página com %d eventos, limite %d", len(page), limit)
		}
		all = append(all, page...)
		next, _ := out["next_cursor"].(string)
		if next == "" {
			return all, pages
		}
		if pages > 50 {
			c.a.t.Fatal("o histórico não terminou")
		}
		cursor = next
	}
}

func custFields(t *testing.T, e map[string]any) []string {
	t.Helper()
	d, _ := e["details"].(map[string]any)
	raw, _ := d["fields"].([]any)
	out := make([]string, 0, len(raw))
	for _, f := range raw {
		out = append(out, f.(string))
	}
	return out
}

// Histórico do cliente pela API depois de alterações comuns (cadastro,
// endereço e contato), com os nomes dos campos alterados e nunca os valores,
// em páginas por cursor sem lacunas nem repetições; e edição de contato.
func TestCustomerHistoryAndContactEditOverHTTP(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "historico")
	c := a.login(tn.Owner, &tn.ID)
	cust, _ := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/customers", map[string]any{"kind": "company", "name": "Empresa Histórico",
		"document_number": "11222333000181", "email": "financeiro@historico.test"})
	id := cust["id"].(string)
	custPath := "/api/v1/customers/" + id

	// Alteração comum do cadastro.
	c.expect(http.StatusOK, http.MethodPatch, custPath, map[string]any{"kind": "company", "name": "Empresa Histórico Renomeada",
		"document_number": "11222333000181", "email": "financeiro@historico.test", "phone": "+5511999990000"}, "If-Match", `"1"`)
	events, _ := custHistory(c, id, 25)
	if len(events) != 2 || events[0]["action"] != "customer.updated" || events[1]["action"] != "customer.created" ||
		!slices.Equal(custFields(t, events[0]), []string{"name", "phone"}) || events[0]["actor_id"] != tn.Owner.ID.String() {
		t.Fatalf("histórico depois da alteração do cadastro: %v", events)
	}
	for _, e := range events {
		assertSchema(t, "AuditEvent", e)
	}

	// Endereço: cadastro com todos os campos, alteração somente com os que
	// mudaram e regravação igual sem evento.
	// O endereço faz parte da ficha: If-Match e ETag são a versão do cliente,
	// que muda a cada gravação e não muda na regravação igual.
	addr := map[string]any{"street": "Rua das Flores", "number": "10", "district": "Centro", "city": "Curitiba", "state": "PR",
		"postal_code": "80010-000"}
	v := int(c.versionOf(custPath).(float64))
	c.expectProblem(http.StatusPreconditionRequired, "if_match_required", http.MethodPut, custPath+"/billing-address", addr)
	c.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPut, custPath+"/billing-address", addr, matchVersion(v+1)...)
	// Cadastro, alteração e regravação igual.
	for i, step := range []struct{ ifMatch, etag int }{{v, v + 1}, {v + 1, v + 2}, {v + 2, v + 2}} {
		if i == 1 {
			addr["city"], addr["complement"] = "Londrina", "Sala 2"
		}
		_, h := c.expect(http.StatusOK, http.MethodPut, custPath+"/billing-address", addr, matchVersion(step.ifMatch)...)
		if h.Get("ETag") != fmt.Sprintf(`"%d"`, step.etag) {
			t.Fatalf("gravação %d do endereço: ETag %q, esperado %d", i+1, h.Get("ETag"), step.etag)
		}
	}
	if got := c.versionOf(custPath); got != float64(v+2) {
		t.Fatalf("versão do cliente depois do endereço: %v", got)
	}

	// Contato: inclusão e edição.
	ct, _ := c.expect(http.StatusCreated, http.MethodPost, custPath+"/contacts", map[string]any{"name": "Ana Lima", "email": "ana@historico.test"})
	ctPath := custPath + "/contacts/" + ct["id"].(string)
	edited, _ := c.expect(http.StatusOK, http.MethodPatch, ctPath, map[string]any{"name": "Ana Lima", "email": "Ana.Lima@Historico.test",
		"role": "Contas a pagar", "receives_billing_email": false})
	assertSchema(t, "Contact", edited)
	if edited["id"] != ct["id"] || edited["email"] != "ana.lima@historico.test" || edited["role"] != "Contas a pagar" ||
		edited["receives_billing_email"] != false ||
		!instant(t, edited["created_at"]).Equal(instant(t, ct["created_at"]).Truncate(time.Microsecond)) {
		t.Fatalf("contato editado: %v", edited)
	}
	got, _ := c.expect(http.StatusOK, http.MethodGet, custPath, nil)
	contacts := objects(t, got["contacts"])
	if len(contacts) != 1 || contacts[0]["email"] != "ana.lima@historico.test" || contacts[0]["receives_billing_email"] != false {
		t.Fatalf("ficha do cliente depois da edição do contato: %v", contacts)
	}
	// Mesmo conteúdo: nenhum evento novo.
	c.expect(http.StatusOK, http.MethodPatch, ctPath, map[string]any{"name": "Ana Lima", "email": "ana.lima@historico.test",
		"role": "Contas a pagar", "receives_billing_email": false})
	// Alteração parcial: campos omitidos (inclusive a preferência de e-mail de
	// cobrança desligada) mantêm o valor atual.
	partial, _ := c.expect(http.StatusOK, http.MethodPatch, ctPath, map[string]any{"role": "Tesouraria"})
	if partial["role"] != "Tesouraria" || partial["receives_billing_email"] != false || partial["email"] != "ana.lima@historico.test" ||
		partial["name"] != "Ana Lima" {
		t.Fatalf("alteração parcial do contato: %v", partial)
	}

	other, _ := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/customers", map[string]any{"kind": "individual", "name": "Outra Pessoa",
		"document_number": "529.982.247-25"})
	for _, tc := range []struct {
		name, path string
		body       any
		status     int
		code       string
	}{
		{"contato inexistente", custPath + "/contacts/" + uuid.NewString(), map[string]any{"name": "X", "email": "x@x.test"}, 404, "contact_not_found"},
		{"contato de outro cliente", "/api/v1/customers/" + other["id"].(string) + "/contacts/" + ct["id"].(string),
			map[string]any{"name": "X", "email": "x@x.test"}, 404, "contact_not_found"},
		{"e-mail inválido", ctPath, map[string]any{"name": "Ana", "email": "sem-arroba"}, 422, "invalid_contact"},
		{"campo desconhecido", ctPath, map[string]any{"name": "Ana", "email": "ana@historico.test", "cpf": "1"}, 422, "invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.expectProblem(tc.status, tc.code, http.MethodPatch, tc.path, tc.body)
		})
	}
	tnB := a.env.NewTenant(t, "historico-b")
	cB := a.login(tnB.Owner, &tnB.ID)
	cB.expectProblem(http.StatusNotFound, "customer_not_found", http.MethodPatch, ctPath, map[string]any{"name": "Ana", "email": "a@b.test"})
	cB.expectProblem(http.StatusNotFound, "customer_not_found", http.MethodGet, custPath+"/history", nil)
	auditor := a.login(member(t, a.env, tn, tenancy.RoleAuditor), &tn.ID)
	if st, _, _ := auditor.do(http.MethodPatch, ctPath, map[string]any{"name": "Ana", "email": "a@b.test"}); st != http.StatusForbidden {
		t.Fatalf("auditor editou contato: %d", st)
	}
	if st, _, _ := auditor.do(http.MethodGet, custPath+"/history", nil); st != http.StatusOK {
		t.Fatalf("auditor deveria ler o histórico: %d", st)
	}

	// Histórico completo em páginas de 2: criação, alteração, endereço
	// cadastrado, endereço alterado, contato incluído e contato editado duas vezes.
	all, pages := custHistory(c, id, 2)
	if pages != 4 {
		t.Fatalf("páginas do histórico: %d", pages)
	}
	actions := make([]string, 0, len(all))
	ids := map[string]bool{}
	for i, e := range all {
		actions = append(actions, e["action"].(string))
		if ids[e["id"].(string)] {
			t.Fatalf("evento repetido entre páginas: %v", e)
		}
		ids[e["id"].(string)] = true
		if i > 0 && instant(t, e["occurred_at"]).After(instant(t, all[i-1]["occurred_at"])) {
			t.Fatalf("histórico fora da ordem do mais recente para o mais antigo: %v", all)
		}
	}
	wantActions := []string{"customer.contact_updated", "customer.contact_updated", "customer.contact_added", "customer.address_updated", "customer.address_updated",
		"customer.updated", "customer.created"}
	if !slices.Equal(actions, wantActions) {
		t.Fatalf("ações do histórico: %v", actions)
	}
	whole, _ := custHistory(c, id, 100)
	if len(whole) != len(all) {
		t.Fatalf("o histórico paginado deveria ter os mesmos eventos do completo: %d e %d", len(all), len(whole))
	}
	byAction := func(i int) []string { return custFields(t, all[i]) }
	if f := byAction(0); !slices.Equal(f, []string{"role"}) {
		t.Errorf("campos da alteração parcial do contato: %v", f)
	}
	if f := byAction(1); !slices.Equal(f, []string{"email", "role", "receives_billing_email"}) {
		t.Errorf("campos da edição do contato: %v", f)
	}
	if f := byAction(3); !slices.Equal(f, []string{"complement", "city"}) || all[3]["details"].(map[string]any)["created"] != false {
		t.Errorf("campos da alteração do endereço: %v %v", f, all[3]["details"])
	}
	if f := byAction(4); !slices.Equal(f, []string{"street", "number", "district", "city", "state", "postal_code", "country"}) ||
		all[4]["details"].(map[string]any)["created"] != true {
		t.Errorf("campos do cadastro do endereço: %v %v", f, all[4]["details"])
	}
	// Nenhum valor pessoal no histórico, somente nomes de campos.
	for _, e := range all {
		s := fmt.Sprint(e["details"])
		for _, v := range []string{"Londrina", "Curitiba", "Flores", "80010", "ana.lima", "Contas a pagar", "Tesouraria", "Renomeada", "99999"} {
			if strings.Contains(s, v) {
				t.Errorf("histórico com valor pessoal %q: %v", v, e)
			}
		}
	}
	c.expectProblem(http.StatusUnprocessableEntity, "invalid_limit", http.MethodGet, custPath+"/history?limit=101", nil)
}
