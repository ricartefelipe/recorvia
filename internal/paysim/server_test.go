package paysim_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/paysim"
)

// api é o simulador servido por httptest, com cliente que não segue redirects.
type api struct {
	*env
	srv    *httptest.Server
	client *http.Client
}

// newAPI cria o servidor; configure ajusta o simulador antes de ele começar a
// atender (os campos de demora são lidos pelas goroutines do servidor).
func newAPI(t *testing.T, configure func(*paysim.Sim)) *api {
	t.Helper()
	e := newEnv(t)
	if configure != nil {
		configure(e.sim)
	}
	srv := httptest.NewServer(e.sim.Handler())
	t.Cleanup(srv.Close)
	return &api{env: e, srv: srv, client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type response struct {
	status int
	header http.Header
	body   string
}

// errorCode extrai o campo "error" do corpo JSON.
func (r response) errorCode() string {
	var v struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.body), &v); err != nil {
		return ""
	}
	return v.Error
}

func (a *api) doCtx(ctx context.Context, method, path, auth, body string) (response, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.srv.URL+path, rd)
	if err != nil {
		return response{}, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return response{}, err
	}
	return response{status: resp.StatusCode, header: resp.Header, body: string(b)}, nil
}

func (a *api) do(method, path, auth, body string) response {
	a.t.Helper()
	r, err := a.doCtx(a.t.Context(), method, path, auth, body)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	return r
}

const bearer = "Bearer " + merchantKey

func chargeBody(ref string) string {
	b, err := json.Marshal(chargeInput(ref))
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestAPIKeyAuthentication(t *testing.T) {
	a := newAPI(t, nil)
	cases := []struct {
		name   string
		auth   string
		status int
	}{
		{name: "sem header", auth: "", status: http.StatusUnauthorized},
		{name: "esquema Basic", auth: "Basic " + merchantKey, status: http.StatusUnauthorized},
		{name: "esquema em minúsculas", auth: "bearer " + merchantKey, status: http.StatusUnauthorized},
		{name: "chave vazia", auth: "Bearer ", status: http.StatusUnauthorized},
		{name: "chave com 7 caracteres", auth: "Bearer 1234567", status: http.StatusUnauthorized},
		{name: "chave com 8 caracteres", auth: "Bearer 12345678", status: http.StatusOK},
		{name: "chave do comerciante", auth: bearer, status: http.StatusOK},
	}
	for _, tc := range cases {
		r := a.do(http.MethodGet, "/v1/charges", tc.auth, "")
		if r.status != tc.status {
			t.Errorf("%s: status %d, esperado %d", tc.name, r.status, tc.status)
		}
		if tc.status == http.StatusUnauthorized && r.errorCode() != "invalid_api_key" {
			t.Errorf("%s: código %q", tc.name, r.errorCode())
		}
	}

	// Todas as rotas /v1 exigem a chave.
	routes := []struct{ method, path string }{
		{http.MethodPut, "/v1/webhook"}, {http.MethodPost, "/v1/customers"}, {http.MethodPost, "/v1/charges"},
		{http.MethodGet, "/v1/charges/ch_000001"}, {http.MethodDelete, "/v1/charges/ch_000001"},
		{http.MethodPost, "/v1/charges/ch_000001/refunds"},
	}
	for _, rt := range routes {
		if r := a.do(rt.method, rt.path, "", "{}"); r.status != http.StatusUnauthorized {
			t.Errorf("%s %s sem chave: status %d", rt.method, rt.path, r.status)
		}
	}
	if r := a.do(http.MethodGet, "/healthz", "", ""); r.status != http.StatusOK {
		t.Errorf("healthz: status %d", r.status)
	}
}

func TestMerchantIsolationOverHTTP(t *testing.T) {
	a := newAPI(t, nil)
	c := a.create("", "fatura-1")
	other := "Bearer " + otherKey
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/charges/" + c.ID, ""},
		{http.MethodDelete, "/v1/charges/" + c.ID, ""},
		{http.MethodPost, "/v1/charges/" + c.ID + "/refunds", "{}"},
	} {
		if r := a.do(rt.method, rt.path, other, rt.body); r.status != http.StatusNotFound || r.errorCode() != "not_found" {
			t.Errorf("%s %s por outro comerciante: %d %s", rt.method, rt.path, r.status, r.body)
		}
	}
	if r := a.do(http.MethodGet, "/v1/charges", other, ""); !strings.Contains(r.body, `"data":[]`) {
		t.Errorf("listagem de outro comerciante: %s", r.body)
	}
}

func TestPostChargeScenarios(t *testing.T) {
	cases := []struct {
		scenario    string
		status      int
		code        string
		wantCreated bool
	}{
		{scenario: "", status: http.StatusCreated, wantCreated: true},
		{scenario: "decline", status: http.StatusUnprocessableEntity, code: "charge_declined"},
		{scenario: "unavailable", status: http.StatusServiceUnavailable, code: "temporarily_unavailable"},
		{scenario: "error_after_success", status: http.StatusBadGateway, code: "upstream_error", wantCreated: true},
		{scenario: "duplicate_webhooks", status: http.StatusCreated, wantCreated: true},
		{scenario: "out_of_order", status: http.StatusCreated, wantCreated: true},
	}
	for _, tc := range cases {
		t.Run("cenario_"+tc.scenario, func(t *testing.T) {
			a := newAPI(t, nil)
			if tc.scenario != "" {
				body := `{"api_key":"` + merchantKey + `","scenario":"` + tc.scenario + `"}`
				if r := a.do(http.MethodPost, "/sim/scenarios", "", body); r.status != http.StatusOK {
					t.Fatalf("agendamento: %d %s", r.status, r.body)
				}
			}
			r := a.do(http.MethodPost, "/v1/charges", bearer, chargeBody("fatura-1"))
			if r.status != tc.status || r.errorCode() != tc.code {
				t.Fatalf("resposta %d %s, esperado %d %q", r.status, r.body, tc.status, tc.code)
			}
			if tc.status == http.StatusCreated {
				var c paysim.Charge
				if err := json.Unmarshal([]byte(r.body), &c); err != nil || c.ID != "ch_000001" || c.Status != "pending" ||
					c.CheckoutURL != "http://checkout.test/checkout/ch_000001" {
					t.Fatalf("cobrança %+v, %v", c, err)
				}
			}
			// Em error_after_success a cobrança existe apesar do erro: o
			// integrador precisa consultá-la pela referência externa.
			list := a.do(http.MethodGet, "/v1/charges?external_reference=fatura-1", bearer, "")
			if got := strings.Contains(list.body, `"id":"ch_000001"`); got != tc.wantCreated {
				t.Fatalf("cobrança encontrada pela referência = %v, esperado %v: %s", got, tc.wantCreated, list.body)
			}
		})
	}
}

func TestPostChargeRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{name: "json malformado", body: `{"amount":`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "campo desconhecido", body: `{"amount":100,"currency":"BRL","external_reference":"x","extra":1}`, status: http.StatusBadRequest,
			code: "invalid_request"},
		{name: "valor zero", body: `{"amount":0,"currency":"BRL","external_reference":"x"}`, status: http.StatusUnprocessableEntity,
			code: "pedido inválido"},
		{name: "corpo acima de 64 KiB", body: `{"description":"` + strings.Repeat("a", 65<<10) + `"}`, status: http.StatusBadRequest,
			code: "invalid_request"},
	}
	a := newAPI(t, nil)
	for _, tc := range cases {
		if r := a.do(http.MethodPost, "/v1/charges", bearer, tc.body); r.status != tc.status || r.errorCode() != tc.code {
			t.Errorf("%s: %d %q, esperado %d %q", tc.name, r.status, r.errorCode(), tc.status, tc.code)
		}
	}
}

// TestResponseDelayScenarios prova o efeito de slow e timeout_after_success
// sem esperas longas: com a demora curta, a resposta chega depois dela; com a
// demora longa, o cliente desiste pelo próprio prazo, a cobrança fica criada
// e o servidor encerra o handler assim que a conexão é cancelada.
func TestResponseDelayScenarios(t *testing.T) {
	cases := []struct {
		scenario string
		set      func(s *paysim.Sim, d time.Duration)
		other    func(s *paysim.Sim, d time.Duration)
	}{
		{scenario: "slow", set: func(s *paysim.Sim, d time.Duration) { s.SlowDelay = d },
			other: func(s *paysim.Sim, d time.Duration) { s.TimeoutDelay = d }},
		{scenario: "timeout_after_success", set: func(s *paysim.Sim, d time.Duration) { s.TimeoutDelay = d },
			other: func(s *paysim.Sim, d time.Duration) { s.SlowDelay = d }},
	}
	const short = 30 * time.Millisecond
	for _, tc := range cases {
		t.Run(tc.scenario+"/responde depois da demora configurada", func(t *testing.T) {
			// A demora do outro cenário é longa: se o campo errado fosse
			// usado, o prazo do cliente estouraria.
			a := newAPI(t, func(s *paysim.Sim) { tc.set(s, short); tc.other(s, time.Hour) })
			if err := a.sim.EnqueueScenario(merchantKey, tc.scenario); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			start := time.Now()
			r, err := a.doCtx(ctx, http.MethodPost, "/v1/charges", bearer, chargeBody("fatura-1"))
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("requisição: %v", err)
			}
			if r.status != http.StatusCreated || !strings.Contains(r.body, `"id":"ch_000001"`) {
				t.Fatalf("resposta %d %s", r.status, r.body)
			}
			if elapsed < short {
				t.Fatalf("resposta em %v, antes da demora de %v", elapsed, short)
			}
		})

		t.Run(tc.scenario+"/cliente desiste e a cobrança fica criada", func(t *testing.T) {
			a := newAPI(t, func(s *paysim.Sim) { tc.set(s, time.Hour) })
			if err := a.sim.EnqueueScenario(merchantKey, tc.scenario); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			_, err := a.doCtx(ctx, http.MethodPost, "/v1/charges", bearer, chargeBody("fatura-1"))
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("erro = %v, esperado prazo do cliente estourado", err)
			}
			// Resultado desconhecido para o cliente, mas a cobrança existe.
			list, _ := a.sim.ListCharges(merchantKey, "fatura-1", time.Time{}, time.Time{}, 0, 10)
			if len(list) != 1 || list[0].Status != "pending" {
				t.Fatalf("cobrança após desistência do cliente: %+v", list)
			}
			// O cenário foi consumido: a próxima criação responde sem demora
			// (com a demora de uma hora, o prazo do teste estouraria).
			next, cancelNext := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancelNext()
			if r, err := a.doCtx(next, http.MethodPost, "/v1/charges", bearer, chargeBody("fatura-2")); err != nil || r.status != http.StatusCreated {
				t.Fatalf("criação seguinte: %+v, %v", r, err)
			}
			// Close aguarda os handlers ativos: só retorna porque a espera
			// respeita o cancelamento do contexto da requisição.
			a.srv.Close()
		})
	}
}

func TestRefundOverHTTP(t *testing.T) {
	cases := []struct {
		name     string
		scenario string
		body     string
		status   int
		code     string
	}{
		{name: "sucesso", body: `{"amount":3000,"external_reference":"rf-1"}`, status: http.StatusCreated},
		{name: "refund_unavailable", scenario: "refund_unavailable", body: `{"amount":3000}`, status: http.StatusServiceUnavailable,
			code: "temporarily_unavailable"},
		{name: "refund_error_after_success", scenario: "refund_error_after_success", body: `{"amount":3000}`, status: http.StatusBadGateway,
			code: "upstream_error"},
		{name: "acima do saldo", body: `{"amount":10001}`, status: http.StatusUnprocessableEntity, code: "reembolso excede o saldo"},
		{name: "json inválido", body: `{"amount":"x"}`, status: http.StatusBadRequest, code: "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAPI(t, nil)
			c := a.create("", "fatura-1")
			a.apply(c.ID, paysim.Action{Name: "pay"})
			if tc.scenario != "" {
				if err := a.sim.EnqueueScenario(merchantKey, tc.scenario); err != nil {
					t.Fatal(err)
				}
			}
			r := a.do(http.MethodPost, "/v1/charges/"+c.ID+"/refunds", bearer, tc.body)
			if r.status != tc.status || r.errorCode() != tc.code {
				t.Fatalf("resposta %d %s, esperado %d %q", r.status, r.body, tc.status, tc.code)
			}
		})
	}
	t.Run("cobrança inexistente", func(t *testing.T) {
		a := newAPI(t, nil)
		if r := a.do(http.MethodPost, "/v1/charges/ch_999999/refunds", bearer, `{}`); r.status != http.StatusNotFound {
			t.Fatalf("status %d", r.status)
		}
	})
}

func TestDeleteChargeOverHTTP(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(a *api, id string)
		path   string
		status int
		code   string
	}{
		{name: "pendente", status: http.StatusOK},
		{name: "paga", setup: func(a *api, id string) { a.apply(id, paysim.Action{Name: "pay"}) }, status: http.StatusUnprocessableEntity,
			code: "charge_not_cancelable"},
		{name: "cancel_unavailable", setup: func(a *api, _ string) {
			if err := a.sim.EnqueueScenario(merchantKey, "cancel_unavailable"); err != nil {
				a.t.Fatal(err)
			}
		}, status: http.StatusServiceUnavailable, code: "temporarily_unavailable"},
		{name: "inexistente", path: "/v1/charges/ch_999999", status: http.StatusNotFound, code: "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAPI(t, nil)
			c := a.create("", "fatura-1")
			if tc.setup != nil {
				tc.setup(a, c.ID)
			}
			path := tc.path
			if path == "" {
				path = "/v1/charges/" + c.ID
			}
			r := a.do(http.MethodDelete, path, bearer, "")
			if r.status != tc.status || r.errorCode() != tc.code {
				t.Fatalf("resposta %d %s, esperado %d %q", r.status, r.body, tc.status, tc.code)
			}
			if tc.status == http.StatusOK && !strings.Contains(r.body, `"status":"canceled"`) {
				t.Fatalf("corpo: %s", r.body)
			}
		})
	}
}

func TestGetAndListChargesOverHTTP(t *testing.T) {
	a := newAPI(t, nil)
	c := a.create("", "fatura-1")
	a.clock.Advance(time.Minute)
	a.create("", "fatura-2")

	r := a.do(http.MethodGet, "/v1/charges/"+c.ID, bearer, "")
	if r.status != http.StatusOK || !strings.Contains(r.body, `"refunds":[]`) || strings.Contains(r.body, merchantKey) {
		t.Fatalf("consulta: %d %s", r.status, r.body)
	}
	if r := a.do(http.MethodGet, "/v1/charges/ch_999999", bearer, ""); r.status != http.StatusNotFound {
		t.Fatalf("consulta inexistente: %d", r.status)
	}

	from := rfc3339Query(a.clock.Now())
	cases := []struct {
		name     string
		query    string
		status   int
		code     string
		contains []string
		excludes []string
	}{
		{name: "por referência", query: "external_reference=fatura-1", status: http.StatusOK, contains: []string{`"id":"ch_000001"`, `"has_more":false`},
			excludes: []string{`"id":"ch_000002"`}},
		{name: "referência sem cobrança", query: "external_reference=nenhuma", status: http.StatusOK, contains: []string{`"data":[]`}},
		{name: "janela de atualização", query: "updated_from=" + from, status: http.StatusOK, contains: []string{`"id":"ch_000002"`},
			excludes: []string{`"id":"ch_000001"`}},
		{name: "paginação", query: "limit=1", status: http.StatusOK, contains: []string{`"id":"ch_000002"`, `"has_more":true`}},
		{name: "deslocamento negativo vira zero", query: "limit=1&offset=-5", status: http.StatusOK, contains: []string{`"id":"ch_000002"`}},
		{name: "limite acima do máximo usa o padrão", query: "limit=1000", status: http.StatusOK,
			contains: []string{`"id":"ch_000001"`, `"id":"ch_000002"`, `"has_more":false`}},
		{name: "updated_from inválido", query: "updated_from=ontem", status: http.StatusBadRequest, code: "invalid_updated_from"},
		{name: "updated_to inválido", query: "updated_to=amanha", status: http.StatusBadRequest, code: "invalid_updated_to"},
	}
	for _, tc := range cases {
		r := a.do(http.MethodGet, "/v1/charges?"+tc.query, bearer, "")
		if r.status != tc.status || r.errorCode() != tc.code {
			t.Errorf("%s: %d %s", tc.name, r.status, r.body)
			continue
		}
		for _, s := range tc.contains {
			if !strings.Contains(r.body, s) {
				t.Errorf("%s: corpo sem %s: %s", tc.name, s, r.body)
			}
		}
		for _, s := range tc.excludes {
			if strings.Contains(r.body, s) {
				t.Errorf("%s: corpo com %s: %s", tc.name, s, r.body)
			}
		}
	}
}

func rfc3339Query(t time.Time) string { return strings.ReplaceAll(t.Format(time.RFC3339), "+", "%2B") }

func TestWebhookAndCustomerEndpoints(t *testing.T) {
	a := newAPI(t, nil)
	cases := []struct {
		name, method, path, body string
		status                   int
		code                     string
		contains                 string
	}{
		{name: "webhook válido", method: http.MethodPut, path: "/v1/webhook", body: `{"url":"https://recorvia.test/w","secret":"` + webhookSecret + `"}`,
			status: http.StatusOK, contains: `"url":"https://recorvia.test/w"`},
		{name: "webhook com segredo curto", method: http.MethodPut, path: "/v1/webhook", body: `{"url":"https://recorvia.test/w","secret":"x"}`,
			status: http.StatusUnprocessableEntity, code: "segredo muito curto"},
		{name: "webhook com json inválido", method: http.MethodPut, path: "/v1/webhook", body: `{`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "cliente", method: http.MethodPost, path: "/v1/customers", body: `{"name":"Ana","external_reference":"cli-1"}`,
			status: http.StatusCreated, contains: `"id":"cus_000001"`},
		{name: "cliente repetido", method: http.MethodPost, path: "/v1/customers", body: `{"name":"Ana","external_reference":"cli-1"}`,
			status: http.StatusCreated, contains: `"id":"cus_000001"`},
		{name: "cliente sem nome", method: http.MethodPost, path: "/v1/customers", body: `{"external_reference":"cli-2"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
	}
	for _, tc := range cases {
		r := a.do(tc.method, tc.path, bearer, tc.body)
		if r.status != tc.status || r.errorCode() != tc.code || !strings.Contains(r.body, tc.contains) {
			t.Errorf("%s: %d %s", tc.name, r.status, r.body)
		}
	}
}

func TestSimulationControlEndpoints(t *testing.T) {
	a := newAPI(t, nil)
	c := a.create("", "fatura-1")
	cases := []struct {
		name, path, body string
		status           int
		code             string
		contains         string
	}{
		{name: "cenário sem api_key", path: "/sim/scenarios", body: `{"scenario":"decline"}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "cenário desconhecido", path: "/sim/scenarios", body: `{"api_key":"` + merchantKey + `","scenario":"x"}`,
			status: http.StatusUnprocessableEntity, code: "cenário desconhecido: x"},
		{name: "cenário agendado", path: "/sim/scenarios", body: `{"api_key":"` + merchantKey + `","scenario":"refund_unavailable"}`,
			status: http.StatusOK, contains: `"scheduled":"refund_unavailable"`},
		{name: "pagar", path: "/sim/charges/" + c.ID + "/actions", body: `{"action":"pay"}`, status: http.StatusOK, contains: `"status":"paid"`},
		{name: "pagar de novo", path: "/sim/charges/" + c.ID + "/actions", body: `{"action":"pay"}`, status: http.StatusConflict,
			code: "cobrança não está pendente nem recusada"},
		{name: "divergir a taxa", path: "/sim/charges/" + c.ID + "/actions", body: `{"action":"diverge","fee":1}`, status: http.StatusOK,
			contains: `"fee":1,"net_amount":9999`},
		{name: "cobrança inexistente", path: "/sim/charges/ch_999999/actions", body: `{"action":"pay"}`, status: http.StatusNotFound, code: "not_found"},
		{name: "ação com json inválido", path: "/sim/charges/" + c.ID + "/actions", body: `{"acao":"pay"}`, status: http.StatusBadRequest,
			code: "invalid_request"},
	}
	for _, tc := range cases {
		r := a.do(http.MethodPost, tc.path, "", tc.body)
		if r.status != tc.status || r.errorCode() != tc.code || !strings.Contains(r.body, tc.contains) {
			t.Errorf("%s: %d %s", tc.name, r.status, r.body)
		}
	}
	if r := a.do(http.MethodGet, "/sim/charges", "", ""); r.status != http.StatusOK || !strings.Contains(r.body, `"id":"ch_000001"`) {
		t.Errorf("listagem de simulação: %d %s", r.status, r.body)
	}
}

func TestCheckoutPages(t *testing.T) {
	a := newAPI(t, nil)
	c := a.create("", "fatura-1")

	page := a.do(http.MethodGet, "/checkout/"+c.ID, "", "")
	if page.status != http.StatusOK || !strings.HasPrefix(page.header.Get("Content-Type"), "text/html") ||
		!strings.Contains(page.header.Get("Content-Security-Policy"), "default-src 'none'") ||
		!strings.Contains(page.body, "R$ 100,00") || !strings.Contains(page.body, "Confirmar pagamento") {
		t.Fatalf("página de checkout: %d %v\n%s", page.status, page.header, page.body)
	}

	cases := []struct {
		name     string
		path     string
		status   int
		location string
	}{
		{name: "ação inválida", path: "/checkout/" + c.ID + "/refund", status: http.StatusNotFound},
		{name: "cobrança inexistente redireciona", path: "/checkout/ch_999999/pay", status: http.StatusSeeOther, location: "/checkout/ch_999999"},
		{name: "pagar", path: "/checkout/" + c.ID + "/pay", status: http.StatusSeeOther, location: "/checkout/" + c.ID},
		// Ação inaplicável (já paga) é ignorada e também redireciona.
		{name: "recusar depois de pago", path: "/checkout/" + c.ID + "/fail", status: http.StatusSeeOther, location: "/checkout/" + c.ID},
	}
	for _, tc := range cases {
		r := a.do(http.MethodPost, tc.path, "", "")
		if r.status != tc.status || r.header.Get("Location") != tc.location {
			t.Errorf("%s: %d, Location %q", tc.name, r.status, r.header.Get("Location"))
		}
	}
	if v := a.view(c.ID); v.Status != "paid" {
		t.Fatalf("situação após checkout = %q", v.Status)
	}
	if got := eventTypes(a.dispatch()); len(got) != 2 || got[1] != "charge.paid" {
		t.Fatalf("eventos do checkout = %v", got)
	}
	if page := a.do(http.MethodGet, "/checkout/"+c.ID, "", ""); strings.Contains(page.body, "Confirmar pagamento") {
		t.Fatalf("cobrança paga ainda oferece pagamento")
	}
	if r := a.do(http.MethodGet, "/checkout/ch_999999", "", ""); r.status != http.StatusNotFound {
		t.Fatalf("checkout inexistente: %d", r.status)
	}
	if r := a.do(http.MethodGet, "/", "", ""); r.status != http.StatusOK || !strings.Contains(r.body, "fatura-1") {
		t.Fatalf("painel: %d", r.status)
	}
}
