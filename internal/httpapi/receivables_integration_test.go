//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/api"
	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/httpapi"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways/simulator"
	"github.com/ricartefelipe/recorvia/internal/paysim"
	"github.com/ricartefelipe/recorvia/internal/reporting"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/contract"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// recvAPI é a API com o simulador de pagamentos real (HTTP): cobranças,
// reembolsos e cancelamentos chegam ao simulador, e os webhooks dele chegam
// à rota pública de webhooks da própria API.
type recvAPI struct {
	*apiEnv
	sim     *paysim.Sim
	simSrv  *httptest.Server
	pay     *payments.Service
	schemas openapi3.Schemas
}

func newReceivablesAPI(t *testing.T) *recvAPI {
	t.Helper()
	a := newAPI(t)
	env := a.env
	st, err := paysim.Load("")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sim := paysim.New(st, "http://checkout.test", func() time.Time { return env.Clock.Now() }, log)
	simSrv := httptest.NewServer(sim.Handler())
	t.Cleanup(simSrv.Close)
	// O endereço da API entra na configuração do serviço de pagamentos
	// (destino dos webhooks), criado antes do roteador.
	var router http.Handler
	ready := make(chan struct{})
	srv := httptest.NewServer(contract.Wrap(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-ready
		router.ServeHTTP(w, r)
	})))
	t.Cleanup(srv.Close)
	resolver := gateways.NewResolver(2*time.Second, gateways.Factory{Provider: "simulator", Verifier: simulator.Verifier{},
		Gateway: func(acc payments.ProviderAccount, hc *http.Client) (payments.Gateway, error) {
			return simulator.New(simSrv.URL, acc.APIKey, hc), nil
		}})
	pay := env.Payments(resolver, payments.Config{WebhookBaseURL: srv.URL + "/webhooks/providers", CallTimeout: 2 * time.Second,
		Lease: 10 * time.Second})
	idsvc := identity.NewService(stubIDP{}, a.sessions, env.Store.Users(), env.Clock, identity.DefaultPolicy())
	cfg := &config.Config{Env: config.Test, CookieName: "recorvia_session", CookieSecure: false, RateLimitPerMinute: 10000,
		APICredentialRateLimitPerMinute: 10000}
	credentials := apicredentials.NewService(env.Store.APICredentials(), a.sessions, env.Clock, apicredentials.DefaultPolicy())
	_, router = httpapi.NewRouter(httpapi.Deps{Log: log, Config: cfg, Clock: env.Clock, Store: env.Store, Sessions: a.sessions,
		Identity: idsvc, Tenancy: env.Tenancy, Platform: env.Platform, Customers: env.Customers, Catalog: env.Catalog,
		Subscriptions: env.Subscriptions, Billing: env.Billing, Payments: pay, Providers: []string{"simulator"},
		Reporting: reporting.NewService(env.Store.Reporting(), env.Clock), Credentials: credentials})
	close(ready)
	a.srv = srv
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	return &recvAPI{apiEnv: a, sim: sim, simSrv: simSrv, pay: pay, schemas: doc.Components.Schemas}
}

// conforms valida um objeto da resposta contra o schema do contrato.
func (r *recvAPI) conforms(t *testing.T, schema string, v any) {
	t.Helper()
	s, ok := r.schemas[schema]
	if !ok {
		t.Fatalf("schema %s ausente do contrato", schema)
	}
	if err := s.Value.VisitJSON(v); err != nil {
		t.Fatalf("resposta fora do schema %s: %v\n%v", schema, err, v)
	}
}

// newTenant cria a organização com a conta padrão do simulador (chave de
// API própria, isolada das demais organizações no simulador) no plano da
// plataforma com limite de usuários para os papéis usados nos testes.
func (r *recvAPI) newTenant(t *testing.T, slug string) fixture.Tenant {
	t.Helper()
	tn := r.env.NewTenant(t, slug)
	if _, err := r.env.Platform.ChangePlan(context.Background(), tn.ID, tn.Actor, "escala"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pay.ConfigureAccount(context.Background(), tn.ID, tn.Actor, payments.AccountInput{Provider: "simulator", Environment: "local",
		APIKey: "sim_" + slug + "_" + uuid.NewString()[:8], WebhookSecret: strings.Repeat("s", 40), MakeDefault: true}); err != nil {
		t.Fatal(err)
	}
	return tn
}

// paidCharge emite uma fatura, cria a cobrança no simulador e confirma o
// pagamento pelo webhook recebido na API.
func (r *recvAPI) paidCharge(t *testing.T, tn fixture.Tenant, amount int64) payments.Charge {
	t.Helper()
	inv := r.issueInvoice(t, tn, amount)
	c, err := r.pay.RequestCharge(context.Background(), tn.ID, inv.ID, tn.Actor)
	if err != nil || c == nil {
		t.Fatalf("cobrança: %v", err)
	}
	r.simAction(t, c.ProviderChargeID, paysim.Action{Name: "pay"})
	r.deliver(t, tn)
	got := r.latestCharge(t, tn, inv.ID)
	if got.Status != payments.ChargePaid {
		t.Fatalf("cobrança não paga: %s", got.Status)
	}
	return got
}

func (r *recvAPI) issueInvoice(t *testing.T, tn fixture.Tenant, amount int64) billing.Invoice {
	t.Helper()
	ctx := context.Background()
	plan := r.env.NewFlatPlan(t, tn, amount)
	cust := r.env.NewCustomer(t, tn)
	sub := r.env.NewSubscription(t, tn, cust.ID, plan.ID, nil)
	if _, err := r.env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	pg, err := r.env.Billing.List(ctx, tn.ID, billing.ListFilter{SubscriptionID: &sub.ID, Limit: 10})
	if err != nil || len(pg.Items) != 1 {
		t.Fatalf("fatura não emitida: %v %d", err, len(pg.Items))
	}
	return pg.Items[0]
}

func (r *recvAPI) simAction(t *testing.T, providerChargeID string, a paysim.Action) {
	t.Helper()
	if _, err := r.sim.Apply(providerChargeID, a); err != nil {
		t.Fatalf("ação %s no simulador: %v", a.Name, err)
	}
}

func (r *recvAPI) latestCharge(t *testing.T, tn fixture.Tenant, invoiceID uuid.UUID) payments.Charge {
	t.Helper()
	cs, err := r.env.Store.InvoiceCharges(context.Background(), tn.ID, invoiceID)
	if err != nil || len(cs) == 0 {
		t.Fatalf("cobranças: %v %d", err, len(cs))
	}
	return cs[len(cs)-1]
}

// deliver entrega os webhooks pendentes do simulador à API e os processa
// como o consumidor do worker (o evento da outbox é a mensagem do inbox;
// eventos já processados são ignorados pelo inbox).
func (r *recvAPI) deliver(t *testing.T, tn fixture.Tenant) {
	t.Helper()
	ctx := context.Background()
	r.env.Clock.Advance(time.Second)
	r.sim.DispatchDue(ctx)
	type event struct {
		id      uuid.UUID
		payload []byte
	}
	var evs []event
	err := r.env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, payload FROM outbox_events WHERE tenant_id = $1 AND event_type = 'provider_webhook.received'
		                             ORDER BY occurred_at, id`, tn.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e event
			if err := rows.Scan(&e.id, &e.payload); err != nil {
				return err
			}
			evs = append(evs, e)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		var p struct {
			ID uuid.UUID `json:"webhook_event_id"`
		}
		if err := json.Unmarshal(e.payload, &p); err != nil {
			t.Fatal(err)
		}
		if err := r.pay.ProcessWebhook(ctx, tn.ID, p.ID, e.id); err != nil {
			t.Fatalf("processamento do webhook: %v", err)
		}
	}
}

// recover executa a recuperação de operações até não haver mais trabalho.
func (r *recvAPI) recover(t *testing.T) {
	t.Helper()
	for range 5 {
		r.env.Clock.Advance(time.Minute)
		if _, err := r.pay.RecoverOperations(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func (r *recvAPI) count(t *testing.T, tn fixture.Tenant, query string, args ...any) int64 {
	t.Helper()
	var n int64
	err := r.env.DB.InTenantTx(context.Background(), tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), query, append([]any{tn.ID}, args...)...).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// ledger devolve débitos e créditos por conta pela API e exige partidas
// dobradas: a soma dos débitos é igual à dos créditos.
func ledgerByAccount(t *testing.T, c *client) map[string]map[string]any {
	t.Helper()
	body, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/ledger/accounts", nil)
	accounts := objects(t, body["data"])
	var debits, credits float64
	for _, acc := range accounts {
		debits += acc["debits"].(float64)
		credits += acc["credits"].(float64)
	}
	if debits != credits {
		t.Fatalf("ledger desbalanceado: débitos %.0f, créditos %.0f", debits, credits)
	}
	return byField(t, accounts, "code")
}

// send executa a requisição sem interromper o teste (uso em goroutines).
func (c *client) send(method, path string, body any, extra ...string) (int, map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(method, c.a.srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "recorvia_session", Value: c.cookie})
	req.Header.Set("X-CSRF-Token", c.csrf)
	req.Header.Set("X-Recorvia-Tenant", c.tenant)
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]any{}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, err
}

const refundOperation = "POST /api/v1/charges/{id}/refunds"

func TestReceivablesAPIRefunds(t *testing.T) {
	r := newReceivablesAPI(t)
	tn := r.newTenant(t, "reembolso")
	owner := r.login(tn.Owner, &tn.ID)
	financeUser := member(t, r.env, tn, tenancy.RoleFinance)
	finance := r.login(financeUser, &tn.ID)
	auditor := r.login(member(t, r.env, tn, tenancy.RoleAuditor), &tn.ID)
	operator := r.login(member(t, r.env, tn, tenancy.RoleOperator), &tn.ID)
	c := r.paidCharge(t, tn, 10000)
	path := "/api/v1/charges/" + c.ID.String() + "/refunds"
	partial := map[string]any{"amount": 3000, "reason": "Cancelamento parcial do contrato"}

	auditor.expectProblem(http.StatusForbidden, "forbidden", http.MethodPost, path, partial, "Idempotency-Key", "auditor-"+uuid.NewString())
	operator.expectProblem(http.StatusForbidden, "forbidden", http.MethodPost, path, partial, "Idempotency-Key", "operador-"+uuid.NewString())
	owner.expectProblem(http.StatusUnprocessableEntity, "idempotency_key_required", http.MethodPost, path, partial)
	for _, tc := range []struct {
		name   string
		path   string
		body   map[string]any
		status int
		code   string
	}{
		{"valor zero", path, map[string]any{"amount": 0, "reason": "Sem valor"}, 422, "invalid_refund"},
		{"motivo curto", path, map[string]any{"amount": 100, "reason": "ok"}, 422, "invalid_refund"},
		{"campo desconhecido", path, map[string]any{"amount": 100, "reason": "Campo extra", "currency": "BRL"}, 422, "invalid_json"},
		{"acima do pago", path, map[string]any{"amount": 10001, "reason": "Excede o pago"}, 409, "refund_exceeds_balance"},
		{"cobrança inexistente", "/api/v1/charges/" + uuid.NewString() + "/refunds", partial, 404, "charge_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finance.expectProblem(tc.status, tc.code, http.MethodPost, tc.path, tc.body, "Idempotency-Key", "invalido-"+uuid.NewString())
		})
	}

	// Reembolso parcial: efetivado no provedor e idempotente pela chave.
	key := "reembolso-" + uuid.NewString()
	first, h1 := finance.expect(http.StatusCreated, http.MethodPost, path, partial, "Idempotency-Key", key)
	r.conforms(t, "Refund", first)
	if first["status"] != "succeeded" || first["amount"] != float64(3000) || first["charge_id"] != c.ID.String() ||
		first["provider_refund_id"] == nil || h1.Get("Idempotent-Replayed") != "" {
		t.Fatalf("reembolso parcial: %v", first)
	}
	again, h2 := finance.expect(http.StatusCreated, http.MethodPost, path, partial, "Idempotency-Key", key)
	if h2.Get("Idempotent-Replayed") != "true" || again["id"] != first["id"] {
		t.Fatalf("repetição: %v", again)
	}
	finance.expectProblem(http.StatusConflict, "idempotency_key_reused", http.MethodPost, path,
		map[string]any{"amount": 2000, "reason": "Outro conteúdo"}, "Idempotency-Key", key)
	// Depois da expurgação do registro HTTP, a chave de negócio ainda
	// impede o segundo reembolso.
	forgetIdempotencyKey(t, r.env, tn.ID, refundOperation, key)
	late, h3 := finance.expect(http.StatusCreated, http.MethodPost, path, partial, "Idempotency-Key", key)
	if late["id"] != first["id"] || h3.Get("Idempotent-Replayed") != "" {
		t.Fatalf("pedido repetido após a expurgação: %v", late)
	}
	if keys := storedRequestKeys(t, r.env, tn.ID, "refunds"); len(keys) != 1 || keys[0] != requestKey(financeUser.ID, refundOperation, key) {
		t.Fatalf("chave de negócio: %v", keys)
	}
	charge, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/charges/"+c.ID.String(), nil)
	if charge["status"] != "partially_refunded" || charge["amount_refunded"] != float64(3000) || charge["refundable_amount"] != float64(7000) {
		t.Fatalf("cobrança após o reembolso parcial: %v", charge)
	}

	// Reembolso do saldo: a cobrança fica reembolsada e não aceita outro.
	total, _ := owner.expect(http.StatusCreated, http.MethodPost, path, map[string]any{"amount": 7000, "reason": "Encerramento do contrato"},
		"Idempotency-Key", "reembolso-"+uuid.NewString())
	if total["status"] != "succeeded" {
		t.Fatalf("reembolso do saldo: %v", total)
	}
	charge, _ = owner.expect(http.StatusOK, http.MethodGet, "/api/v1/charges/"+c.ID.String(), nil)
	if charge["status"] != "refunded" || charge["amount_refunded"] != float64(10000) || charge["refundable_amount"] != float64(0) {
		t.Fatalf("cobrança reembolsada: %v", charge)
	}
	owner.expectProblem(http.StatusConflict, "charge_not_refundable", http.MethodPost, path, map[string]any{"amount": 1, "reason": "Sem saldo"},
		"Idempotency-Key", "reembolso-"+uuid.NewString())

	// Webhooks dos reembolsos e o reenvio de todos os eventos não duplicam efeitos.
	r.deliver(t, tn)
	r.simAction(t, c.ProviderChargeID, paysim.Action{Name: "replay"})
	r.deliver(t, tn)
	if n := r.count(t, tn, `SELECT count(*) FROM refunds WHERE tenant_id = $1`); n != 2 {
		t.Fatalf("reembolsos = %d", n)
	}
	accounts := ledgerByAccount(t, owner)
	if accounts["refunds"]["debits"] != float64(10000) || accounts["refunds"]["credits"] != float64(0) {
		t.Fatalf("conta de reembolsos: %v", accounts["refunds"])
	}

	// A fatura continua paga e registra as devoluções no histórico.
	inv, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/invoices/"+c.InvoiceID.String(), nil)
	reasons := 0
	for _, e := range objects(t, inv["history"]) {
		if e["reason"] == "refund" {
			reasons++
		}
	}
	if inv["status"] != "paid" || reasons != 2 {
		t.Fatalf("fatura após os reembolsos: %v %d", inv["status"], reasons)
	}

	// Listagem paginada por cursor, mais recentes primeiro.
	page1, _ := auditor.expect(http.StatusOK, http.MethodGet, "/api/v1/refunds?limit=1", nil)
	items1 := objects(t, page1["data"])
	next, _ := page1["next_cursor"].(string)
	if len(items1) != 1 || next == "" || items1[0]["id"] != total["id"] {
		t.Fatalf("primeira página: %v", page1)
	}
	page2, _ := auditor.expect(http.StatusOK, http.MethodGet, "/api/v1/refunds?limit=1&cursor="+next, nil)
	items2 := objects(t, page2["data"])
	if len(items2) != 1 || items2[0]["id"] != first["id"] || page2["next_cursor"] != nil {
		t.Fatalf("segunda página: %v", page2)
	}
	for _, it := range append(items1, items2...) {
		r.conforms(t, "Refund", it)
	}
	auditor.expectProblem(http.StatusUnprocessableEntity, "invalid_cursor", http.MethodGet, "/api/v1/refunds?cursor=invalido", nil)

	// Outra organização não vê nem reembolsa a cobrança.
	other := r.newTenant(t, "reembolso-b")
	outsider := r.login(other.Owner, &other.ID)
	list, _ := outsider.expect(http.StatusOK, http.MethodGet, "/api/v1/refunds", nil)
	if len(jsonArray(t, list["data"])) != 0 {
		t.Fatalf("reembolsos de outra organização: %v", list)
	}
	outsider.expectProblem(http.StatusNotFound, "charge_not_found", http.MethodPost, path, partial, "Idempotency-Key", "fora-"+uuid.NewString())
	outsider.expectProblem(http.StatusNotFound, "charge_not_found", http.MethodGet, "/api/v1/charges/"+c.ID.String(), nil)
}

// Pedidos concorrentes pela API nunca somam mais que o pago; a mesma chave
// concorrente cria um único reembolso.
func TestReceivablesAPIConcurrentRefunds(t *testing.T) {
	r := newReceivablesAPI(t)
	tn := r.newTenant(t, "concorrencia")
	owner := r.login(tn.Owner, &tn.ID)
	c := r.paidCharge(t, tn, 10000)
	path := "/api/v1/charges/" + c.ID.String() + "/refunds"

	type result struct {
		status int
		body   map[string]any
		err    error
	}
	run := func(n int, body map[string]any, key func(int) string) []result {
		out := make([]result, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Go(func() {
				<-start
				out[i].status, out[i].body, out[i].err = owner.send(http.MethodPost, path, body, "Idempotency-Key", key(i))
			})
		}
		close(start)
		wg.Wait()
		return out
	}
	results := run(8, map[string]any{"amount": 3000, "reason": "Reembolso concorrente"}, func(i int) string { return fmt.Sprintf("concorrente-%d", i) })
	accepted := 0
	for _, res := range results {
		switch {
		case res.err != nil:
			t.Fatal(res.err)
		case res.status == http.StatusCreated:
			accepted++
		case res.status != http.StatusConflict || res.body["code"] != "refund_exceeds_balance":
			t.Fatalf("resposta inesperada: %d %v", res.status, res.body)
		}
	}
	if accepted != 3 {
		t.Fatalf("reembolsos aceitos = %d, esperado 3", accepted)
	}
	// Reembolsos adiados pela execução serializada são concluídos pela recuperação.
	r.recover(t)
	charge, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/charges/"+c.ID.String(), nil)
	if charge["amount_refunded"] != float64(9000) || charge["refundable_amount"] != float64(1000) {
		t.Fatalf("cobrança após os pedidos concorrentes: %v", charge)
	}
	view, err := r.sim.View("", c.ProviderChargeID)
	if err != nil || view.RefundedAmount != 9000 || len(view.Refunds) != 3 {
		t.Fatalf("provedor: %+v %v", view, err)
	}

	results = run(5, map[string]any{"amount": 500, "reason": "Mesma chave concorrente"}, func(int) string { return "mesma-chave-concorrente" })
	for _, res := range results {
		if res.err != nil {
			t.Fatal(res.err)
		}
		if res.status != http.StatusCreated && (res.status != http.StatusConflict || res.body["code"] != "idempotency_in_progress") {
			t.Fatalf("mesma chave: %d %v", res.status, res.body)
		}
	}
	r.recover(t)
	if n := r.count(t, tn, `SELECT count(*) FROM refunds WHERE tenant_id = $1 AND amount = 500`); n != 1 {
		t.Fatalf("reembolsos com a mesma chave = %d", n)
	}
	if n := r.count(t, tn, `SELECT count(*) FROM refunds WHERE tenant_id = $1 AND status = 'succeeded'`); n != 4 {
		t.Fatalf("reembolsos efetivados = %d", n)
	}
	accounts := ledgerByAccount(t, owner)
	if accounts["refunds"]["debits"] != float64(9500) {
		t.Fatalf("conta de reembolsos: %v", accounts["refunds"])
	}
}

func TestReceivablesAPIDisputes(t *testing.T) {
	r := newReceivablesAPI(t)
	tn := r.newTenant(t, "disputa")
	owner := r.login(tn.Owner, &tn.ID)
	operator := r.login(member(t, r.env, tn, tenancy.RoleOperator), &tn.ID)
	won, lost := r.paidCharge(t, tn, 10000), r.paidCharge(t, tn, 6000)

	for _, c := range []payments.Charge{won, lost} {
		r.simAction(t, c.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: "requested"})
	}
	r.deliver(t, tn)
	body, _ := operator.expect(http.StatusOK, http.MethodGet, "/api/v1/disputes", nil)
	disputes := objects(t, body["data"])
	if len(disputes) != 2 {
		t.Fatalf("disputas abertas: %v", body)
	}
	// Disputas e operações seguem o next_cursor, sem repetir nem pular itens.
	if paged := pageAll(t, operator, "/api/v1/disputes", pageData(t)); len(paged) != 2 {
		t.Fatalf("disputas paginadas: %v", paged)
	}
	if ops := pageAll(t, owner, "/api/v1/payment-operations", pageData(t)); len(ops) < 2 {
		t.Fatalf("operações paginadas: %v", ops)
	}
	operator.expectProblem(http.StatusUnprocessableEntity, "invalid_sort", http.MethodGet, "/api/v1/disputes?sort=opened_at", nil)
	owner.expectProblem(http.StatusUnprocessableEntity, "invalid_cursor", http.MethodGet, "/api/v1/payment-operations?cursor=abc", nil)
	for _, d := range disputes {
		r.conforms(t, "Dispute", d)
		if d["status"] != "opened" || d["closed_at"] != nil {
			t.Fatalf("disputa aberta: %v", d)
		}
	}
	charge, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/charges/"+won.ID.String(), nil)
	if charge["status"] != "disputed" || charge["refundable_amount"] != float64(10000) {
		t.Fatalf("cobrança em disputa: %v", charge)
	}
	owner.expectProblem(http.StatusConflict, "charge_not_refundable", http.MethodPost, "/api/v1/charges/"+won.ID.String()+"/refunds",
		map[string]any{"amount": 100, "reason": "Durante a disputa"}, "Idempotency-Key", "disputa-"+uuid.NewString())
	// O valor contestado fica retido enquanto a disputa está aberta.
	if acc := ledgerByAccount(t, owner)["chargebacks"]; acc["debits"] != float64(16000) || acc["credits"] != float64(0) {
		t.Fatalf("retenção no ledger: %v", acc)
	}

	r.simAction(t, won.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: "won"})
	r.simAction(t, lost.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: "lost"})
	r.deliver(t, tn)
	body, _ = owner.expect(http.StatusOK, http.MethodGet, "/api/v1/disputes", nil)
	byCharge := byField(t, objects(t, body["data"]), "charge_id")
	if d := byCharge[won.ID.String()]; d["status"] != "won" || d["closed_at"] == nil {
		t.Fatalf("disputa ganha: %v", d)
	}
	if d := byCharge[lost.ID.String()]; d["status"] != "lost" || d["closed_at"] == nil || d["amount"] != float64(6000) {
		t.Fatalf("disputa perdida: %v", d)
	}
	wonCharge, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/charges/"+won.ID.String(), nil)
	lostCharge, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/charges/"+lost.ID.String(), nil)
	if wonCharge["status"] != "paid" || lostCharge["status"] != "charged_back" || lostCharge["amount_charged_back"] != float64(6000) ||
		lostCharge["refundable_amount"] != float64(0) {
		t.Fatalf("cobranças após a decisão: %v / %v", wonCharge, lostCharge)
	}
	// A disputa ganha estorna a retenção; a perdida permanece como chargeback.
	acc := ledgerByAccount(t, owner)["chargebacks"]
	if acc["debits"].(float64)-acc["credits"].(float64) != 6000 {
		t.Fatalf("chargebacks no ledger: %v", acc)
	}
	// O painel do mês mostra o valor líquido: a disputa ganha não conta.
	dash, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/dashboard", nil)
	r.conforms(t, "Dashboard", dash)
	if dash["chargebacks"] != float64(6000) || dash["refunds"] != float64(0) {
		t.Fatalf("painel: chargebacks=%v reembolsos=%v", dash["chargebacks"], dash["refunds"])
	}
	// Mês futuro não tem fluxo nem posição: 422 com o campo do erro.
	if p, _ := owner.expect(http.StatusUnprocessableEntity, http.MethodGet, "/api/v1/dashboard?month=2099-01", nil); p["code"] != "future_month" {
		t.Fatalf("painel de mês futuro: %v", p)
	}

	// Reenvio dos eventos não reabre nem duplica disputas.
	r.simAction(t, won.ProviderChargeID, paysim.Action{Name: "replay"})
	r.simAction(t, lost.ProviderChargeID, paysim.Action{Name: "replay"})
	r.deliver(t, tn)
	if n := r.count(t, tn, `SELECT count(*) FROM disputes WHERE tenant_id = $1`); n != 2 {
		t.Fatalf("disputas após o reenvio = %d", n)
	}

	other := r.newTenant(t, "disputa-b")
	body, _ = r.login(other.Owner, &other.ID).expect(http.StatusOK, http.MethodGet, "/api/v1/disputes", nil)
	if len(jsonArray(t, body["data"])) != 0 {
		t.Fatalf("disputas de outra organização: %v", body)
	}
}

// work faz o papel do worker: assume e executa a conciliação registrada.
func (r *recvAPI) work(t *testing.T, tenantID uuid.UUID, run map[string]any) {
	t.Helper()
	id, err := uuid.Parse(run["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.pay.ExecuteReconciliationRun(context.Background(), tenantID, id); err != nil {
		t.Fatalf("execução da conciliação registrada: %v", err)
	}
}

// reconcile pede a conciliação pela API (202, execução em andamento), executa
// como o worker e devolve a execução encerrada consultada pela API.
func (r *recvAPI) reconcile(t *testing.T, c *client, tenantID uuid.UUID, key string) map[string]any {
	t.Helper()
	run, h := c.expect(http.StatusAccepted, http.MethodPost, "/api/v1/reconciliation/runs", nil, "Idempotency-Key", key)
	r.conforms(t, "ReconciliationRun", run)
	path := "/api/v1/reconciliation/runs/" + run["id"].(string)
	if run["status"] != "running" || run["finished_at"] != nil || run["attempts"] != float64(0) || h.Get("Location") != path {
		t.Fatalf("execução registrada: %v (Location %q)", run, h.Get("Location"))
	}
	r.work(t, tenantID, run)
	done, _ := c.expect(http.StatusOK, http.MethodGet, path, nil)
	r.conforms(t, "ReconciliationRun", done)
	return done
}

func TestReceivablesAPIReconciliation(t *testing.T) {
	ctx := context.Background()
	r := newReceivablesAPI(t)
	tn := r.newTenant(t, "conciliacao")
	owner := r.login(tn.Owner, &tn.ID)
	financeUser := member(t, r.env, tn, tenancy.RoleFinance)
	finance := r.login(financeUser, &tn.ID)
	auditor := r.login(member(t, r.env, tn, tenancy.RoleAuditor), &tn.ID)
	operator := r.login(member(t, r.env, tn, tenancy.RoleOperator), &tn.ID)

	paid := r.paidCharge(t, tn, 10000)
	silentInv := r.issueInvoice(t, tn, 4000)
	silent, err := r.pay.RequestCharge(ctx, tn.ID, silentInv.ID, tn.Actor)
	if err != nil {
		t.Fatal(err)
	}
	fee := int64(500)
	r.simAction(t, paid.ProviderChargeID, paysim.Action{Name: "diverge", Fee: &fee})
	r.simAction(t, silent.ProviderChargeID, paysim.Action{Name: "pay_silently"})
	r.env.Clock.Advance(time.Minute)

	operator.expectProblem(http.StatusForbidden, "forbidden", http.MethodGet, "/api/v1/reconciliation/runs", nil)
	operator.expectProblem(http.StatusForbidden, "forbidden", http.MethodGet, "/api/v1/reconciliation/discrepancies", nil)
	auditor.expect(http.StatusOK, http.MethodGet, "/api/v1/reconciliation/runs", nil)
	auditor.expectProblem(http.StatusForbidden, "forbidden", http.MethodPost, "/api/v1/reconciliation/runs", nil, "Idempotency-Key", "auditor-"+uuid.NewString())
	owner.expectProblem(http.StatusUnprocessableEntity, "idempotency_key_required", http.MethodPost, "/api/v1/reconciliation/runs", nil)

	// O pedido registra a execução e responde de imediato, sem consultar o
	// provedor; a repetição com a mesma chave devolve a mesma execução em
	// andamento, e outro pedido da conta é recusado até ela terminar.
	key := "conciliacao-" + uuid.NewString()
	path := "/api/v1/reconciliation/runs"
	queued, h := finance.expect(http.StatusAccepted, http.MethodPost, path, nil, "Idempotency-Key", key)
	r.conforms(t, "ReconciliationRun", queued)
	runPath := path + "/" + queued["id"].(string)
	if queued["status"] != "running" || queued["trigger"] != "manual" || queued["attempts"] != float64(0) || queued["finished_at"] != nil ||
		queued["discrepancy_count"] != float64(0) || h.Get("Location") != runPath || h.Get("Idempotent-Replayed") != "" {
		t.Fatalf("execução registrada: %v %v", queued, h)
	}
	if again, h := finance.expect(http.StatusAccepted, http.MethodPost, path, nil, "Idempotency-Key", key); again["id"] != queued["id"] ||
		again["status"] != "running" || h.Get("Idempotent-Replayed") != "" {
		t.Fatalf("repetição com a execução em andamento: %v %v", again, h)
	}
	st, busy, h := owner.do(http.MethodPost, path, nil, "Idempotency-Key", "outra-"+uuid.NewString())
	if st != http.StatusConflict || busy["code"] != "reconciliation_running" || h.Get("Retry-After") == "" {
		t.Fatalf("segunda execução da conta: %d %v", st, busy)
	}
	pending, _ := auditor.expect(http.StatusOK, http.MethodGet, runPath, nil)
	if pending["status"] != "running" || len(objects(t, mustList(t, auditor, "/api/v1/reconciliation/discrepancies"))) != 0 {
		t.Fatalf("antes do worker: %v", pending)
	}
	operator.expectProblem(http.StatusForbidden, "forbidden", http.MethodGet, runPath, nil)
	auditor.expectProblem(http.StatusNotFound, "reconciliation_run_not_found", http.MethodGet, path+"/"+uuid.NewString(), nil)
	auditor.expectProblem(http.StatusNotFound, "not_found", http.MethodGet, path+"/abc", nil)

	// O worker assume e conclui a execução; quem pediu acompanha pela consulta.
	r.work(t, tn.ID, queued)
	run, _ := auditor.expect(http.StatusOK, http.MethodGet, runPath, nil)
	r.conforms(t, "ReconciliationRun", run)
	if run["status"] != "completed" || run["discrepancy_count"] != float64(2) || run["provider"] != "simulator" || run["finished_at"] == nil ||
		run["attempts"] != float64(1) || run["error"] != "" {
		t.Fatalf("execução: %v", run)
	}
	// Depois de encerrada, a mesma chave devolve o resultado (200), que
	// passa a ficar registrado na chave.
	replay, h := finance.expect(http.StatusOK, http.MethodPost, path, nil, "Idempotency-Key", key)
	if replay["id"] != run["id"] || replay["status"] != "completed" || h.Get("Idempotent-Replayed") != "" || h.Get("Location") != runPath {
		t.Fatalf("resultado pela chave: %v %v", replay, h)
	}
	// A repetição do resultado registrado reproduz também o header Location
	// do contrato, além de status e corpo.
	for range 2 {
		again, h := finance.expect(http.StatusOK, http.MethodPost, path, nil, "Idempotency-Key", key)
		if !reflect.DeepEqual(again, replay) || h.Get("Idempotent-Replayed") != "true" || h.Get("Location") != runPath {
			t.Fatalf("repetição do resultado: %v %v", again, h)
		}
	}
	runs, _ := auditor.expect(http.StatusOK, http.MethodGet, "/api/v1/reconciliation/runs", nil)
	if list := objects(t, runs["data"]); len(list) != 1 || list[0]["id"] != run["id"] {
		t.Fatalf("execuções: %v", runs)
	}

	all, _ := auditor.expect(http.StatusOK, http.MethodGet, "/api/v1/reconciliation/discrepancies", nil)
	discrepancies := byField(t, objects(t, all["data"]), "kind")
	if len(discrepancies) != 2 || discrepancies["fee_mismatch"] == nil || discrepancies["payment_not_reflected"] == nil {
		t.Fatalf("divergências: %v", all)
	}
	for _, d := range discrepancies {
		r.conforms(t, "Discrepancy", d)
		if d["status"] != "open" || d["run_id"] != run["id"] {
			t.Fatalf("divergência aberta: %v", d)
		}
	}
	filtered, _ := auditor.expect(http.StatusOK, http.MethodGet, "/api/v1/reconciliation/discrepancies?status=open&kind=fee_mismatch", nil)
	if list := objects(t, filtered["data"]); len(list) != 1 || list[0]["charge_id"] != paid.ID.String() {
		t.Fatalf("filtro por tipo: %v", filtered)
	}
	auditor.expectProblem(http.StatusUnprocessableEntity, "invalid_field", http.MethodGet, "/api/v1/reconciliation/discrepancies?kind=duplicada", nil)
	auditor.expectProblem(http.StatusUnprocessableEntity, "invalid_field", http.MethodGet, "/api/v1/reconciliation/discrepancies?status=auto_resolved", nil)

	feeID := discrepancies["fee_mismatch"]["id"].(string)
	payID := discrepancies["payment_not_reflected"]["id"].(string)
	detail, _ := auditor.expect(http.StatusOK, http.MethodGet, "/api/v1/reconciliation/discrepancies/"+feeID, nil)
	r.conforms(t, "Discrepancy", detail)
	local, provider := detail["local_value"].(map[string]any), detail["provider_value"].(map[string]any)
	if local["fee"] != float64(paysim.Fee(10000)) || provider["fee"] != float64(500) || detail["provider_charge_id"] != paid.ProviderChargeID {
		t.Fatalf("retratos da divergência: %v / %v", local, provider)
	}
	if !hasAction(t, detail, "reconciliation.discrepancy_detected") {
		t.Fatalf("histórico da divergência: %v", detail["history"])
	}

	resolvePath := "/api/v1/reconciliation/discrepancies/" + feeID + "/resolve"
	justification := "Taxa conferida no extrato do simulador de pagamentos."
	feeVersion := matchVersion(detail["version"])
	payVersion := matchVersion(finance.versionOf("/api/v1/reconciliation/discrepancies/" + payID))
	auditor.expectProblem(http.StatusForbidden, "forbidden", http.MethodPost, resolvePath,
		map[string]any{"resolution": "adjust_ledger", "justification": justification}, feeVersion...)
	finance.expectProblem(http.StatusUnprocessableEntity, "invalid_resolution", http.MethodPost, resolvePath,
		map[string]any{"resolution": "adjust_ledger", "justification": "curta"}, feeVersion...)
	finance.expectProblem(http.StatusUnprocessableEntity, "invalid_resolution", http.MethodPost, resolvePath,
		map[string]any{"resolution": "auto_resolved", "justification": justification}, feeVersion...)
	finance.expectProblem(http.StatusConflict, "resolution_not_applicable", http.MethodPost, "/api/v1/reconciliation/discrepancies/"+payID+"/resolve",
		map[string]any{"resolution": "adjust_ledger", "justification": justification}, payVersion...)

	finance.expectProblem(http.StatusPreconditionRequired, "if_match_required", http.MethodPost, resolvePath,
		map[string]any{"resolution": "adjust_ledger", "justification": justification})
	finance.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPost, resolvePath,
		map[string]any{"resolution": "adjust_ledger", "justification": justification}, matchVersion(detail["version"].(float64)+1)...)
	resolved, h := finance.expect(http.StatusOK, http.MethodPost, resolvePath, map[string]any{"resolution": "adjust_ledger", "justification": justification},
		feeVersion...)
	r.conforms(t, "Discrepancy", resolved)
	if h.Get("ETag") != fmt.Sprintf(`"%v"`, resolved["version"]) {
		t.Fatalf("ETag da resolução: %q, versão %v", h.Get("ETag"), resolved["version"])
	}
	if resolved["status"] != "resolved" || resolved["resolution"] != "adjust_ledger" || resolved["justification"] != justification ||
		resolved["resolved_by"] != financeUser.ID.String() || resolved["resolved_at"] == nil || !hasAction(t, resolved, "reconciliation.discrepancy_resolved") {
		t.Fatalf("divergência resolvida: %v", resolved)
	}
	finance.expectProblem(http.StatusConflict, "discrepancy_not_open", http.MethodPost, resolvePath,
		map[string]any{"resolution": "dismiss", "justification": justification}, matchVersion(resolved["version"])...)
	// O ajuste lança a diferença de taxa (provedor menos local) e mantém o ledger balanceado.
	adj := ledgerByAccount(t, owner)["reconciliation_adjustments"]
	if adj["debits"].(float64)-adj["credits"].(float64) != float64(500-paysim.Fee(10000)) {
		t.Fatalf("ajuste de conciliação: %v", adj)
	}
	// O ajuste e a sua liquidação no disponível referenciam a divergência.
	txs, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/ledger/transactions?reference_type=reconciliation_discrepancy&reference_id="+feeID, nil)
	kinds := map[string]bool{}
	for _, tx := range objects(t, txs["data"]) {
		kinds[tx["kind"].(string)] = true
	}
	if len(kinds) != 2 || !kinds["reconciliation.adjustment"] || !kinds["settlement.debited"] {
		t.Fatalf("lançamentos do ajuste: %v", txs)
	}

	// Pagamento não refletido: aceitar o provedor aplica o pagamento.
	finance.expect(http.StatusOK, http.MethodPost, "/api/v1/reconciliation/discrepancies/"+payID+"/resolve",
		map[string]any{"resolution": "accept_provider", "justification": "Pagamento confirmado no painel do provedor."}, payVersion...)
	inv, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/invoices/"+silentInv.ID.String(), nil)
	if inv["status"] != "paid" {
		t.Fatalf("fatura do pagamento não refletido: %v", inv["status"])
	}
	ledgerByAccount(t, owner)

	// Nova execução: nenhuma divergência aberta; a resolvida não é reaberta.
	r.env.Clock.Advance(time.Minute)
	again := r.reconcile(t, owner, tn.ID, "conciliacao-"+uuid.NewString())
	if again["status"] != "completed" || again["discrepancy_count"] != float64(0) {
		t.Fatalf("execução após as resoluções: %v", again)
	}
	// Execuções e divergências seguem o next_cursor, sem repetir nem pular itens.
	if runs := pageAll(t, owner, "/api/v1/reconciliation/runs", pageData(t)); len(runs) < 2 || runs[0]["id"] != again["id"] {
		t.Fatalf("execuções paginadas, da mais recente: %v", runs)
	}
	if ds := pageAll(t, owner, "/api/v1/reconciliation/discrepancies", pageData(t)); len(ds) != 2 {
		t.Fatalf("divergências paginadas: %v", ds)
	}
	owner.expectProblem(http.StatusUnprocessableEntity, "invalid_sort", http.MethodGet, "/api/v1/reconciliation/runs?sort=started_at", nil)
	open, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/reconciliation/discrepancies?status=open", nil)
	if len(jsonArray(t, open["data"])) != 0 {
		t.Fatalf("divergências abertas: %v", open)
	}

	// Outra organização não vê nem resolve as divergências.
	other := r.newTenant(t, "conciliacao-b")
	outsider := r.login(other.Owner, &other.ID)
	outsider.expectProblem(http.StatusNotFound, "discrepancy_not_found", http.MethodGet, "/api/v1/reconciliation/discrepancies/"+feeID, nil)
	outsider.expectProblem(http.StatusNotFound, "discrepancy_not_found", http.MethodPost, "/api/v1/reconciliation/discrepancies/"+payID+"/resolve",
		map[string]any{"resolution": "dismiss", "justification": justification}, matchVersion(1)...)
	list, _ := outsider.expect(http.StatusOK, http.MethodGet, "/api/v1/reconciliation/discrepancies", nil)
	if len(jsonArray(t, list["data"])) != 0 {
		t.Fatalf("divergências de outra organização: %v", list)
	}
	outsider.expectProblem(http.StatusNotFound, "reconciliation_run_not_found", http.MethodGet, runPath, nil)
	otherRun := r.reconcile(t, outsider, other.ID, "outra-"+uuid.NewString())
	if otherRun["status"] != "completed" || otherRun["checked_count"] != float64(0) || otherRun["discrepancy_count"] != float64(0) {
		t.Fatalf("execução de outra organização: %v", otherRun)
	}

	// Provedor indisponível: o pedido é aceito e a execução termina como
	// falha, com a causa.
	r.simSrv.Close()
	failed := r.reconcile(t, owner, tn.ID, "conciliacao-"+uuid.NewString())
	if failed["status"] != "failed" || failed["error"] == "" || failed["finished_at"] == nil {
		t.Fatalf("execução com o provedor indisponível: %v", failed)
	}
}

// mustList devolve o campo data de uma listagem.
func mustList(t *testing.T, c *client, path string) any {
	t.Helper()
	body, _ := c.expect(http.StatusOK, http.MethodGet, path, nil)
	return body["data"]
}

func hasAction(t *testing.T, discrepancy map[string]any, action string) bool {
	t.Helper()
	for _, e := range objects(t, discrepancy["history"]) {
		if e["action"] == action {
			return true
		}
	}
	return false
}

// Organização sem provedor configurado não executa a conciliação.
func TestReceivablesAPIReconciliationRequiresProvider(t *testing.T) {
	r := newReceivablesAPI(t)
	tn := r.env.NewTenant(t, "sem-provedor")
	owner := r.login(tn.Owner, &tn.ID)
	owner.expectProblem(http.StatusConflict, "provider_not_configured", http.MethodPost, "/api/v1/reconciliation/runs", nil,
		"Idempotency-Key", "sem-provedor-"+uuid.NewString())
	runs, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/reconciliation/runs", nil)
	if len(jsonArray(t, runs["data"])) != 0 {
		t.Fatalf("execuções: %v", runs)
	}
	owner.expectProblem(http.StatusNotFound, "discrepancy_not_found", http.MethodGet, "/api/v1/reconciliation/discrepancies/"+uuid.NewString(), nil)
}

// Credencial de API com os escopos de reembolso e conciliação: a autoria das
// ações fica na auditoria (ator api_credential) e as colunas de autoria que
// referenciam usuários ficam nulas, sem violar as chaves estrangeiras.
func TestReceivablesAPIWithAPICredential(t *testing.T) {
	r := newReceivablesAPI(t)
	tn := r.newTenant(t, "credencial-recebiveis")
	owner := r.login(tn.Owner, &tn.ID)
	_, _, secret := createCredential(t, owner, "payments.refund", "reconciliation.read", "reconciliation.run", "reconciliation.resolve")
	c := r.paidCharge(t, tn, 10000)
	fee := int64(500)
	r.simAction(t, c.ProviderChargeID, paysim.Action{Name: "diverge", Fee: &fee})
	r.env.Clock.Advance(time.Minute)

	path := "/api/v1/charges/" + c.ID.String() + "/refunds"
	body := map[string]any{"amount": 3000, "reason": "Cancelamento pelo ERP"}
	key := "erp-reembolso-" + uuid.NewString()
	refund := r.raw(http.MethodPost, path, body, bearer(secret, "Idempotency-Key", key))
	if refund.status != http.StatusCreated || refund.body["status"] != "succeeded" {
		t.Fatalf("reembolso por credencial: %d %v", refund.status, refund.body)
	}
	if again := r.raw(http.MethodPost, path, body, bearer(secret, "Idempotency-Key", key)); again.status != http.StatusCreated ||
		again.body["id"] != refund.body["id"] || again.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("repetição por credencial: %d %v", again.status, again.body)
	}

	queued := r.raw(http.MethodPost, "/api/v1/reconciliation/runs", nil, bearer(secret, "Idempotency-Key", "erp-conciliacao-"+uuid.NewString()))
	if queued.status != http.StatusAccepted || queued.body["status"] != "running" {
		t.Fatalf("conciliação por credencial: %d %v", queued.status, queued.body)
	}
	r.work(t, tn.ID, queued.body)
	run := r.raw(http.MethodGet, queued.header.Get("Location"), nil, bearer(secret))
	if run.status != http.StatusOK || run.body["status"] != "completed" || run.body["discrepancy_count"] != float64(1) {
		t.Fatalf("execução acompanhada pela credencial: %d %v", run.status, run.body)
	}
	open := r.raw(http.MethodGet, "/api/v1/reconciliation/discrepancies?status=open&kind=fee_mismatch", nil, bearer(secret))
	items := objects(t, open.body["data"])
	if open.status != http.StatusOK || len(items) != 1 {
		t.Fatalf("divergências: %d %v", open.status, open.body)
	}
	resolved := r.raw(http.MethodPost, "/api/v1/reconciliation/discrepancies/"+items[0]["id"].(string)+"/resolve",
		map[string]any{"resolution": "keep_local", "justification": "Taxa conferida pelo ERP no contrato."}, bearer(secret, matchVersion(items[0]["version"])...))
	if resolved.status != http.StatusOK || resolved.body["status"] != "resolved" || resolved.body["resolved_by"] != nil {
		t.Fatalf("resolução por credencial: %d %v", resolved.status, resolved.body)
	}
	r.conforms(t, "Discrepancy", resolved.body)

	for table, column := range map[string]string{"refunds": "requested_by", "reconciliation_runs": "requested_by",
		"reconciliation_discrepancies": "resolved_by"} {
		if n := r.count(t, tn, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1 AND `+column+` IS NOT NULL`); n != 0 {
			t.Fatalf("%s.%s preenchido em %d linha(s)", table, column, n)
		}
	}
	for _, action := range []string{"refund.requested", "reconciliation.run_started", "reconciliation.discrepancy_resolved"} {
		if n := r.count(t, tn, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND actor_type = 'api_credential'`,
			action); n != 1 {
			t.Fatalf("auditoria de %s pela credencial: %d", action, n)
		}
	}
	// Com a sessão do usuário, a autoria continua gravada.
	byUser, _ := owner.expect(http.StatusCreated, http.MethodPost, path, map[string]any{"amount": 1000, "reason": "Ajuste pelo painel"},
		"Idempotency-Key", "painel-"+uuid.NewString())
	if n := r.count(t, tn, `SELECT count(*) FROM refunds WHERE tenant_id = $1 AND id = $2 AND requested_by = $3`, byUser["id"],
		tn.Owner.ID); n != 1 {
		t.Fatalf("autoria do reembolso pelo painel: %d", n)
	}
}

// A sessão do portal do cliente não alcança as rotas de reembolso e
// conciliação do painel.
func TestReceivablesRoutesRejectPortalSession(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "portal-recebiveis")
	cust := a.env.NewCustomer(t, tn)
	owner := a.login(tn.Owner, &tn.ID)
	cookie, csrf := openPortal(t, a, createPortalLink(t, owner, cust.ID))
	headers := map[string]string{"X-CSRF-Token": csrf, "X-Recorvia-Tenant": tn.ID.String(), "Idempotency-Key": "portal-" + uuid.NewString()}
	for _, tc := range []struct {
		path string
		body any
	}{
		{"/api/v1/charges/" + uuid.NewString() + "/refunds", map[string]any{"amount": 100, "reason": "Pedido do cliente"}},
		{"/api/v1/reconciliation/runs", nil},
		{"/api/v1/reconciliation/discrepancies/" + uuid.NewString() + "/resolve", map[string]any{"resolution": "dismiss",
			"justification": "Pedido feito pelo portal."}},
	} {
		if res := a.raw(http.MethodPost, tc.path, tc.body, headers, cookie); res.status != http.StatusUnauthorized {
			t.Fatalf("%s com a sessão do portal: %d %v", tc.path, res.status, res.body)
		}
	}
}

// O conflito com outra conciliação em andamento é transitório: a resposta
// informa Retry-After e não fica registrada na Idempotency-Key, de modo que a
// repetição com a mesma chave executa a conciliação depois que a outra
// execução termina.
func TestReceivablesAPIReconciliationRunningIsNotStoredInIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	r := newReceivablesAPI(t)
	tn := r.newTenant(t, "conciliacao-ocupada")
	owner := r.login(tn.Owner, &tn.ID)
	other, err := r.pay.StartReconciliation(ctx, tn.ID, tn.Actor, nil, payments.RunInput{Trigger: payments.TriggerManual})
	if err != nil {
		t.Fatal(err)
	}
	key := "ocupada-" + uuid.NewString()
	st, body, h := owner.do(http.MethodPost, "/api/v1/reconciliation/runs", nil, "Idempotency-Key", key)
	if st != http.StatusConflict || body["code"] != "reconciliation_running" || h.Get("Retry-After") == "" {
		t.Fatalf("execução em andamento: %d %v %v", st, body, h)
	}
	if _, outcome, err := r.pay.ExecuteReconciliationRun(ctx, tn.ID, other.ID); err != nil || outcome != payments.RunOutcomeCompleted {
		t.Fatalf("outra execução: %s %v", outcome, err)
	}
	// A mesma chave registra a execução depois que a outra termina; enquanto
	// ela está em andamento, a resposta 202 também não prende a chave.
	run, h := owner.expect(http.StatusAccepted, http.MethodPost, "/api/v1/reconciliation/runs", nil, "Idempotency-Key", key)
	if run["status"] != "running" || run["id"] == other.ID.String() || h.Get("Idempotent-Replayed") != "" {
		t.Fatalf("repetição depois da outra execução: %v %v", run, h)
	}
	if n := r.count(t, tn, `SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1 AND idempotency_key = $2`, key); n != 0 {
		t.Fatalf("chave presa pela execução em andamento: %d registro(s)", n)
	}
	r.work(t, tn.ID, run)
	// O resultado da execução encerrada fica registrado na chave.
	done, h := owner.expect(http.StatusOK, http.MethodPost, "/api/v1/reconciliation/runs", nil, "Idempotency-Key", key)
	location := h.Get("Location")
	if done["id"] != run["id"] || done["status"] != "completed" || h.Get("Idempotent-Replayed") != "" ||
		location != "/api/v1/reconciliation/runs/"+run["id"].(string) {
		t.Fatalf("resultado da execução: %v %v", done, h)
	}
	again, h := owner.expect(http.StatusOK, http.MethodPost, "/api/v1/reconciliation/runs", nil, "Idempotency-Key", key)
	if again["id"] != run["id"] || h.Get("Idempotent-Replayed") != "true" || h.Get("Location") != location {
		t.Fatalf("resultado registrado: %v %v", again, h)
	}
	if n := r.count(t, tn, `SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`); n != 2 {
		t.Fatalf("execuções registradas = %d", n)
	}
}
