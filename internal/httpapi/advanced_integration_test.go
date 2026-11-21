//go:build integration

package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/httpapi"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways"
	"github.com/ricartefelipe/recorvia/internal/reporting"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// tenantTimezone é o fuso das organizações criadas por fixture.NewTenant.
const tenantTimezone = "America/Sao_Paulo"

// newAdvancedAPI monta a API com os serviços de faturamento avançado
// (mudanças de plano e quantidade, notas de crédito, cupons, condições
// comerciais e consumo), reaproveitando Redis, banco, relógio e sessões de
// newAPI. O servidor de newAPI continua aberto até o fim do teste, sem uso.
func newAdvancedAPI(t *testing.T) *apiEnv {
	t.Helper()
	a := newAPI(t)
	env := a.env
	idsvc := identity.NewService(stubIDP{}, a.sessions, env.Store.Users(), env.Clock, identity.DefaultPolicy())
	cfg := &config.Config{Env: config.Test, CookieName: "recorvia_session", CookieSecure: false, RateLimitPerMinute: 10000,
		APICredentialRateLimitPerMinute: 10000}
	pay := env.Payments(gateways.NewResolver(time.Second), payments.Config{WebhookBaseURL: "http://api.test/webhooks/providers"})
	_, router := httpapi.NewRouter(httpapi.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Config: cfg, Clock: env.Clock,
		Store: env.Store, Sessions: a.sessions, Identity: idsvc, Tenancy: env.Tenancy, Platform: env.Platform, Customers: env.Customers,
		Catalog: env.Catalog, Subscriptions: env.Subscriptions, Billing: env.Billing, Payments: pay,
		Changes:     billing.NewChangeService(env.Store.Billing(), env.Clock),
		CreditNotes: billing.NewCreditNoteService(env.Store.Billing(), env.Clock),
		Coupons:     catalog.NewCouponService(env.Store.Catalog(), env.Clock),
		Terms:       subscriptions.NewTermsService(env.Store.Subscriptions(), env.Clock),
		Metering:    metering.NewService(env.Store.Metering(), env.Clock),
		Reporting:   reporting.NewService(env.Store.Reporting(), env.Clock),
		Credentials: apicredentials.NewService(env.Store.APICredentials(), a.sessions, env.Clock, apicredentials.DefaultPolicy())})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	a.srv = srv
	return a
}

// idemKey gera uma Idempotency-Key nova para um pedido de operação
// financeira de criação.
func idemKey() string { return "chave-" + uuid.NewString() }

// expect executa a requisição e interrompe o teste quando o status diverge.
func (c *client) expect(want int, method, path string, body any, extra ...string) (map[string]any, http.Header) {
	c.a.t.Helper()
	st, out, h := c.do(method, path, body, extra...)
	if st != want {
		c.a.t.Fatalf("%s %s: status %d, esperado %d: %v", method, path, st, want, out)
	}
	return out, h
}

// expectProblem verifica o status e o código estável do erro.
func (c *client) expectProblem(want int, code, method, path string, body any, extra ...string) {
	c.a.t.Helper()
	st, out, _ := c.do(method, path, body, extra...)
	if st != want || out["code"] != code {
		c.a.t.Fatalf("%s %s: status %d código %v, esperado %d %s: %v", method, path, st, out["code"], want, code, out)
	}
}

// jsonArray exige uma lista JSON (vazia é aceita, nula não).
func jsonArray(t *testing.T, v any) []any {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("esperada lista JSON, obtido %T: %v", v, v)
	}
	return raw
}

// objects converte uma lista JSON em objetos.
func objects(t *testing.T, v any) []map[string]any {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("esperada lista JSON, obtido %T: %v", v, v)
	}
	out := make([]map[string]any, len(raw))
	for i, x := range raw {
		m, ok := x.(map[string]any)
		if !ok {
			t.Fatalf("item %d não é objeto JSON: %v", i, x)
		}
		out[i] = m
	}
	return out
}

// byField indexa objetos JSON pelo valor textual de um campo.
func byField(t *testing.T, list []map[string]any, field string) map[string]map[string]any {
	t.Helper()
	out := make(map[string]map[string]any, len(list))
	for _, m := range list {
		k, ok := m[field].(string)
		if !ok {
			t.Fatalf("campo %s ausente ou não textual: %v", field, m)
		}
		out[k] = m
	}
	return out
}

// instant interpreta um instante RFC 3339 da resposta.
func instant(t *testing.T, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("instante ausente ou não textual: %v", v)
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("instante inválido %q: %v", s, err)
	}
	return ts
}

// tenantToday é a data civil corrente no fuso do tenant.
func tenantToday(t *testing.T, env *fixture.Env) calendar.Date {
	t.Helper()
	loc, err := time.LoadLocation(tenantTimezone)
	if err != nil {
		t.Fatal(err)
	}
	return calendar.DateOf(env.Clock.Now(), loc)
}

func flatPrice(amount int64) catalog.PriceInput {
	return catalog.PriceInput{Model: catalog.Flat, FlatAmount: &amount}
}

// newPlan cria produto e plano mensal com código exclusivo no tenant.
func newPlan(t *testing.T, env *fixture.Env, tn fixture.Tenant, name string, price catalog.PriceInput) catalog.PlanDetails {
	t.Helper()
	ctx := context.Background()
	prod, err := env.Catalog.CreateProduct(ctx, tn.ID, tn.Actor, catalog.ProductInput{Name: "Produto " + name})
	if err != nil {
		t.Fatal(err)
	}
	uid := tn.Owner.ID
	p, err := env.Catalog.CreatePlan(ctx, tn.ID, tn.Actor, &uid, catalog.PlanInput{ProductID: prod.ID, Code: "plano-" + ids.New().String()[24:],
		Name: name, Interval: calendar.Monthly}, price)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// enableUsageBilling troca o plano da plataforma por um que inclui consumo medido.
func enableUsageBilling(t *testing.T, env *fixture.Env, tn fixture.Tenant) {
	t.Helper()
	if _, err := env.Platform.ChangePlan(context.Background(), tn.ID, tn.Actor, "escala"); err != nil {
		t.Fatal(err)
	}
}

// usagePlan cria uma métrica de soma e um plano cobrado somente por consumo.
func usagePlan(t *testing.T, env *fixture.Env, tn fixture.Tenant, unitAmount string) (catalog.PlanDetails, catalog.Metric) {
	t.Helper()
	m, err := env.Catalog.CreateMetric(context.Background(), tn.ID, tn.Actor, catalog.MetricInput{Code: "api.requisicoes",
		Name: "Requisições de API", Unit: "request", Aggregation: catalog.AggSum, MaxQuantityPerEvent: "1000"})
	if err != nil {
		t.Fatal(err)
	}
	p := newPlan(t, env, tn, "Plano por consumo", catalog.PriceInput{Model: catalog.Usage, UnitAmount: unitAmount, MetricID: &m.ID})
	return p, m
}

// newSubscription cria cliente e assinatura ativa, sem avaliação.
func newSubscription(t *testing.T, env *fixture.Env, tn fixture.Tenant, planID uuid.UUID, qty int, start *calendar.Date) subscriptions.Subscription {
	t.Helper()
	zero := 0
	cust := env.NewCustomer(t, tn)
	s, err := env.Subscriptions.Create(context.Background(), tn.ID, tn.Actor, subscriptions.CreateInput{CustomerID: cust.ID, PlanID: planID,
		Quantity: qty, StartDate: start, TrialDays: &zero})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// runBilling executa o faturamento pela API.
func runBilling(c *client) {
	c.a.t.Helper()
	c.expect(http.StatusOK, http.MethodPost, "/api/v1/billing-runs", nil, "Idempotency-Key", "faturamento-"+uuid.NewString())
}

// subscriptionInvoices lista as faturas de uma assinatura.
func subscriptionInvoices(c *client, subID uuid.UUID) []map[string]any {
	c.a.t.Helper()
	out, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/invoices?subscription_id="+subID.String(), nil)
	return objects(c.a.t, out["data"])
}

// issuedInvoice fatura o período corrente e devolve a fatura recorrente da assinatura.
func issuedInvoice(c *client, subID uuid.UUID) map[string]any {
	c.a.t.Helper()
	runBilling(c)
	list := subscriptionInvoices(c, subID)
	if len(list) != 1 || list[0]["status"] != "open" {
		c.a.t.Fatalf("esperada uma fatura em aberto da assinatura: %v", list)
	}
	return list[0]
}

// subscriptionETag devolve a versão atual da assinatura para o If-Match.
func subscriptionETag(c *client, subID uuid.UUID) string {
	c.a.t.Helper()
	_, h := c.expect(http.StatusOK, http.MethodGet, "/api/v1/subscriptions/"+subID.String(), nil)
	return h.Get("ETag")
}

// pendingChanges filtra as mudanças agendadas ainda pendentes da assinatura.
func pendingChanges(t *testing.T, sub map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ch := range objects(t, sub["scheduled_changes"]) {
		if ch["status"] == "pending" {
			out = append(out, ch)
		}
	}
	return out
}

func TestAdvancedAPICoupons(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "cupons")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Cupom", flatPrice(10000))

	pct, _ := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/coupons", map[string]any{"code": " boas-vindas10 ", "name": "Boas-vindas",
		"discount_type": "percent", "percent_off_bp": 1000, "duration": "repeating", "duration_cycles": 3})
	if pct["code"] != "BOAS-VINDAS10" || pct["status"] != "active" || pct["percent_off_bp"] != float64(1000) || pct["duration_cycles"] != float64(3) ||
		pct["redemptions_count"] != float64(0) || pct["currency"] != "BRL" || pct["version"] != float64(1) || pct["amount_off"] != nil {
		t.Fatalf("cupom percentual: %v", pct)
	}
	if plans := jsonArray(t, pct["applies_to_plan_ids"]); len(plans) != 0 {
		t.Fatalf("cupom sem restrição deveria listar planos vazios: %v", pct["applies_to_plan_ids"])
	}
	amt, _ := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/coupons", map[string]any{"code": "DESCONTO50", "name": "Desconto fixo",
		"discount_type": "amount", "amount_off": 5000, "duration": "forever", "max_redemptions": 2, "applies_to_plan_ids": []string{plan.ID.String()}})
	if amt["amount_off"] != float64(5000) || amt["max_redemptions"] != float64(2) || amt["percent_off_bp"] != nil ||
		!reflect.DeepEqual(amt["applies_to_plan_ids"], []any{plan.ID.String()}) {
		t.Fatalf("cupom de valor fixo: %v", amt)
	}

	// O código é normalizado antes da verificação de unicidade.
	c.expectProblem(http.StatusConflict, "coupon_code_taken", http.MethodPost, "/api/v1/coupons", map[string]any{"code": "boas-vindas10",
		"name": "Outro cupom", "discount_type": "amount", "amount_off": 100, "duration": "once"})

	past := a.env.Clock.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"percentual com valor fixo", map[string]any{"code": "MISTO", "name": "Misto", "discount_type": "percent", "percent_off_bp": 500,
			"amount_off": 100, "duration": "once"}, "invalid_coupon"},
		{"código curto", map[string]any{"code": "AB", "name": "Curto", "discount_type": "percent", "percent_off_bp": 500, "duration": "once"},
			"invalid_coupon"},
		{"repetição sem ciclos", map[string]any{"code": "CICLOS", "name": "Ciclos", "discount_type": "percent", "percent_off_bp": 500,
			"duration": "repeating"}, "invalid_coupon"},
		{"percentual acima de 100%", map[string]any{"code": "ALTO", "name": "Alto", "discount_type": "percent", "percent_off_bp": 10001,
			"duration": "once"}, "invalid_coupon"},
		{"validade no passado", map[string]any{"code": "VENCIDO", "name": "Vencido", "discount_type": "percent", "percent_off_bp": 500,
			"duration": "once", "valid_until": past}, "invalid_coupon"},
		{"plano inexistente", map[string]any{"code": "SEMPLANO", "name": "Sem plano", "discount_type": "percent", "percent_off_bp": 500,
			"duration": "once", "applies_to_plan_ids": []string{uuid.NewString()}}, "invalid_coupon"},
		{"campo desconhecido", map[string]any{"code": "EXTRA", "name": "Extra", "discount_type": "percent", "percent_off_bp": 500,
			"duration": "once", "campo": true}, "invalid_json"},
		// Acima do máximo aceito: antes era truncado para int32 (4294967297 virava 1) ou violava o CHECK da coluna (500).
		{"limite de usos acima do máximo", map[string]any{"code": "LIMITE1", "name": "Limite", "discount_type": "percent", "percent_off_bp": 500,
			"duration": "once", "max_redemptions": 1000000001}, "invalid_coupon"},
		{"limite de usos que trunca para int32", map[string]any{"code": "LIMITE2", "name": "Limite", "discount_type": "percent", "percent_off_bp": 500,
			"duration": "once", "max_redemptions": int64(4294967297)}, "invalid_coupon"},
		{"limite de usos que vira negativo em int32", map[string]any{"code": "LIMITE3", "name": "Limite", "discount_type": "percent",
			"percent_off_bp": 500, "duration": "once", "max_redemptions": int64(2147483648)}, "invalid_coupon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.expectProblem(http.StatusUnprocessableEntity, tc.code, http.MethodPost, "/api/v1/coupons", tc.body)
		})
	}

	list, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/coupons", nil)
	coupons := byField(t, objects(t, list["data"]), "code")
	if len(coupons) != 2 || coupons["BOAS-VINDAS10"]["id"] != pct["id"] || coupons["DESCONTO50"]["id"] != amt["id"] {
		t.Fatalf("listagem de cupons: %v", list["data"])
	}

	archivePath := "/api/v1/coupons/" + pct["id"].(string) + "/archive"
	c.expectProblem(http.StatusPreconditionRequired, "if_match_required", http.MethodPost, archivePath, nil)
	c.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPost, archivePath, nil, matchVersion(2)...)
	archived, h := c.expect(http.StatusOK, http.MethodPost, archivePath, nil, matchVersion(pct["version"])...)
	if archived["status"] != "archived" || archived["version"] != float64(2) || archived["code"] != "BOAS-VINDAS10" || h.Get("ETag") != `"2"` {
		t.Fatalf("arquivamento: %v (ETag %q)", archived, h.Get("ETag"))
	}
	// Arquivar de novo, na versão atual, não altera o cupom; com a anterior, 412.
	again, _ := c.expect(http.StatusOK, http.MethodPost, archivePath, nil, matchVersion(2)...)
	if again["status"] != "archived" || again["version"] != float64(2) {
		t.Fatalf("segundo arquivamento alterou o cupom: %v", again)
	}
	c.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPost, archivePath, nil, matchVersion(1)...)
	c.expectProblem(http.StatusNotFound, "coupon_not_found", http.MethodPost, "/api/v1/coupons/"+uuid.NewString()+"/archive", nil, matchVersion(1)...)
	c.expectProblem(http.StatusNotFound, "not_found", http.MethodPost, "/api/v1/coupons/nao-e-uuid/archive", nil, matchVersion(1)...)

	list, _ = c.expect(http.StatusOK, http.MethodGet, "/api/v1/coupons", nil)
	coupons = byField(t, objects(t, list["data"]), "code")
	if coupons["BOAS-VINDAS10"]["status"] != "archived" || coupons["DESCONTO50"]["status"] != "active" {
		t.Fatalf("situação após arquivamento: %v", list["data"])
	}
}

func TestAdvancedAPISubscriptionDiscounts(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "descontos")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Base", flatPrice(10000))
	other := newPlan(t, a.env, tn, "Plano Outro", flatPrice(20000))
	sub := newSubscription(t, a.env, tn, plan.ID, 1, nil)
	subPath := "/api/v1/subscriptions/" + sub.ID.String()

	coupon := func(body map[string]any) map[string]any {
		t.Helper()
		out, _ := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/coupons", body)
		return out
	}
	loyalty := coupon(map[string]any{"code": "FIDELIDADE", "name": "Fidelidade", "discount_type": "percent", "percent_off_bp": 1500,
		"duration": "repeating", "duration_cycles": 3})
	single := coupon(map[string]any{"code": "UNICO", "name": "Uso único", "discount_type": "amount", "amount_off": 2000, "duration": "once",
		"max_redemptions": 1})
	coupon(map[string]any{"code": "OUTROPLANO", "name": "Outro plano", "discount_type": "percent", "percent_off_bp": 500, "duration": "forever",
		"applies_to_plan_ids": []string{other.ID.String()}})
	old := coupon(map[string]any{"code": "ARQUIVADO", "name": "Arquivado", "discount_type": "percent", "percent_off_bp": 100, "duration": "once"})
	c.expect(http.StatusOK, http.MethodPost, "/api/v1/coupons/"+old["id"].(string)+"/archive", nil, matchVersion(old["version"])...)

	d, _ := c.expect(http.StatusCreated, http.MethodPost, subPath+"/discounts", map[string]any{"coupon_code": " fidelidade "}, "Idempotency-Key", idemKey())
	if d["coupon_code"] != "FIDELIDADE" || d["coupon_id"] != loyalty["id"] || d["remaining_cycles"] != float64(3) || d["status"] != "active" {
		t.Fatalf("desconto aplicado: %v", d)
	}
	got, _ := c.expect(http.StatusOK, http.MethodGet, subPath, nil)
	disc, ok := got["discount"].(map[string]any)
	if !ok || disc["id"] != d["id"] || disc["coupon_code"] != "FIDELIDADE" {
		t.Fatalf("desconto ausente na assinatura: %v", got["discount"])
	}
	c.expectProblem(http.StatusConflict, "discount_already_active", http.MethodPost, subPath+"/discounts", map[string]any{"coupon_code": "UNICO"}, "Idempotency-Key", idemKey())

	c.expect(http.StatusNoContent, http.MethodDelete, subPath+"/discounts", nil)
	got, _ = c.expect(http.StatusOK, http.MethodGet, subPath, nil)
	if got["discount"] != nil {
		t.Fatalf("desconto deveria estar encerrado: %v", got["discount"])
	}
	// Sem desconto ativo, a remoção não faz nada.
	c.expect(http.StatusNoContent, http.MethodDelete, subPath+"/discounts", nil)

	for _, tc := range []struct{ code, want string }{
		{"ARQUIVADO", "coupon_inactive"},
		{"INEXISTENTE", "coupon_not_found"},
		{"OUTROPLANO", "coupon_not_applicable"},
	} {
		c.expectProblem(http.StatusUnprocessableEntity, tc.want, http.MethodPost, subPath+"/discounts", map[string]any{"coupon_code": tc.code}, "Idempotency-Key", idemKey())
	}

	u, _ := c.expect(http.StatusCreated, http.MethodPost, subPath+"/discounts", map[string]any{"coupon_code": "UNICO"}, "Idempotency-Key", idemKey())
	if u["coupon_id"] != single["id"] || u["remaining_cycles"] != float64(1) {
		t.Fatalf("cupom de uso único: %v", u)
	}
	c.expect(http.StatusNoContent, http.MethodDelete, subPath+"/discounts", nil)
	c.expectProblem(http.StatusConflict, "coupon_exhausted", http.MethodPost, subPath+"/discounts", map[string]any{"coupon_code": "UNICO"}, "Idempotency-Key", idemKey())

	list, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/coupons", nil)
	coupons := byField(t, objects(t, list["data"]), "code")
	if coupons["FIDELIDADE"]["redemptions_count"] != float64(1) || coupons["UNICO"]["redemptions_count"] != float64(1) ||
		coupons["OUTROPLANO"]["redemptions_count"] != float64(0) {
		t.Fatalf("contagem de usos: %v", list["data"])
	}

	hist, _ := c.expect(http.StatusOK, http.MethodGet, subPath+"/history", nil)
	events := map[string]int{}
	for _, e := range objects(t, hist["data"]) {
		events[e["event"].(string)]++
	}
	if events["discount_applied"] != 2 || events["discount_removed"] != 2 {
		t.Fatalf("histórico de descontos: %v", events)
	}
	c.expectProblem(http.StatusNotFound, "subscription_not_found", http.MethodPost, "/api/v1/subscriptions/"+uuid.NewString()+"/discounts",
		map[string]any{"coupon_code": "FIDELIDADE"}, "Idempotency-Key", idemKey())
}

func TestAdvancedAPIChangePlanRequiresIfMatch(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "troca-plano")
	c := a.login(tn.Owner, &tn.ID)
	basic := newPlan(t, a.env, tn, "Plano Básico", flatPrice(10000))
	premium := newPlan(t, a.env, tn, "Plano Premium", flatPrice(16000))
	sub := newSubscription(t, a.env, tn, basic.ID, 1, nil)
	subPath := "/api/v1/subscriptions/" + sub.ID.String()
	path := subPath + "/change-plan"
	body := map[string]any{"plan_id": premium.ID, "timing": "next_cycle"}

	c.expectProblem(http.StatusPreconditionRequired, "if_match_required", http.MethodPost, path, body)
	c.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPost, path, body, "If-Match", `"9"`)
	c.expectProblem(http.StatusUnprocessableEntity, "invalid_if_match", http.MethodPost, path, body, "If-Match", `"versao"`)

	etag := subscriptionETag(c, sub.ID)
	if etag != `"1"` {
		t.Fatalf("ETag inicial %s", etag)
	}
	out, h := c.expect(http.StatusOK, http.MethodPost, path, body, "If-Match", etag)
	if h.Get("ETag") != `"2"` || out["version"] != float64(2) || out["plan_id"] != basic.ID.String() {
		t.Fatalf("mudança agendada deveria manter o plano até o próximo ciclo: %s %v", h.Get("ETag"), out)
	}
	pending := pendingChanges(t, out)
	if len(pending) != 1 || pending[0]["kind"] != "plan_change" || pending[0]["new_plan_id"] != premium.ID.String() || pending[0]["new_quantity"] != nil {
		t.Fatalf("mudança agendada: %v", out["scheduled_changes"])
	}
	if !instant(t, pending[0]["effective_at"]).Equal(instant(t, out["current_period_end"])) {
		t.Fatalf("mudança deveria valer no fim do período: %v / %v", pending[0]["effective_at"], out["current_period_end"])
	}
	got, h := c.expect(http.StatusOK, http.MethodGet, subPath, nil)
	if p := pendingChanges(t, got); len(p) != 1 || p[0]["id"] != pending[0]["id"] || h.Get("ETag") != `"2"` {
		t.Fatalf("mudança agendada não aparece na consulta: %v", got["scheduled_changes"])
	}
	// A versão anterior deixou de valer.
	c.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPost, path, body, "If-Match", etag)

	for _, tc := range []struct {
		name   string
		body   map[string]any
		status int
		code   string
	}{
		{"sem plano", map[string]any{"timing": "next_cycle"}, http.StatusUnprocessableEntity, "invalid_change"},
		{"momento inválido", map[string]any{"plan_id": premium.ID, "timing": "amanha"}, http.StatusUnprocessableEntity, "invalid_timing"},
		{"sem momento", map[string]any{"plan_id": premium.ID}, http.StatusUnprocessableEntity, "invalid_timing"},
		{"plano atual", map[string]any{"plan_id": basic.ID, "timing": "next_cycle"}, http.StatusUnprocessableEntity, "no_change"},
		{"quantidade em plano fixo", map[string]any{"plan_id": premium.ID, "quantity": 2, "timing": "next_cycle"}, http.StatusUnprocessableEntity,
			"invalid_change"},
		{"plano inexistente", map[string]any{"plan_id": uuid.New(), "timing": "next_cycle"}, http.StatusNotFound, "plan_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.expectProblem(tc.status, tc.code, http.MethodPost, path, tc.body, "If-Match", `"2"`)
		})
	}
	if e := subscriptionETag(c, sub.ID); e != `"2"` {
		t.Fatalf("pedidos rejeitados alteraram a assinatura: %s", e)
	}
	c.expectProblem(http.StatusNotFound, "subscription_not_found", http.MethodPost, "/api/v1/subscriptions/"+uuid.NewString()+"/change-plan", body,
		"If-Match", `"1"`)
}

func TestAdvancedAPIChangeQuantityRequiresIfMatch(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "troca-qtd")
	c := a.login(tn.Owner, &tn.ID)
	seats := newPlan(t, a.env, tn, "Plano por assento", catalog.PriceInput{Model: catalog.PerUnit, UnitAmount: "5000"})
	flat := newPlan(t, a.env, tn, "Plano fixo", flatPrice(9000))
	sub := newSubscription(t, a.env, tn, seats.ID, 2, nil)
	path := "/api/v1/subscriptions/" + sub.ID.String() + "/change-quantity"

	c.expectProblem(http.StatusPreconditionRequired, "if_match_required", http.MethodPost, path, map[string]any{"quantity": 5, "timing": "next_cycle"})
	c.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPost, path, map[string]any{"quantity": 5, "timing": "next_cycle"},
		"If-Match", `"7"`)
	c.expectProblem(http.StatusUnprocessableEntity, "invalid_change", http.MethodPost, path, map[string]any{"timing": "next_cycle"}, "If-Match", `"1"`)
	c.expectProblem(http.StatusUnprocessableEntity, "invalid_change", http.MethodPost, path, map[string]any{"quantity": 0, "timing": "next_cycle"},
		"If-Match", `"1"`)
	c.expectProblem(http.StatusUnprocessableEntity, "no_change", http.MethodPost, path, map[string]any{"quantity": 2, "timing": "next_cycle"},
		"If-Match", `"1"`)

	out, h := c.expect(http.StatusOK, http.MethodPost, path, map[string]any{"quantity": 5, "timing": "next_cycle"}, "If-Match", `"1"`)
	pending := pendingChanges(t, out)
	if h.Get("ETag") != `"2"` || out["quantity"] != float64(2) || len(pending) != 1 || pending[0]["kind"] != "quantity_change" ||
		pending[0]["new_quantity"] != float64(5) || pending[0]["new_plan_id"] != nil {
		t.Fatalf("troca de quantidade agendada: %s %v", h.Get("ETag"), out)
	}
	// Uma nova mudança substitui a agendada anteriormente.
	out, _ = c.expect(http.StatusOK, http.MethodPost, path, map[string]any{"quantity": 4, "timing": "next_cycle"}, "If-Match", `"2"`)
	pending = pendingChanges(t, out)
	if len(pending) != 1 || pending[0]["new_quantity"] != float64(4) {
		t.Fatalf("mudança substituta: %v", out["scheduled_changes"])
	}
	all := objects(t, out["scheduled_changes"])
	if len(all) != 2 {
		t.Fatalf("histórico de mudanças agendadas: %v", all)
	}
	for _, ch := range all {
		if ch["new_quantity"] == float64(5) && ch["status"] != "canceled" {
			t.Fatalf("mudança substituída deveria estar cancelada: %v", ch)
		}
	}

	// Redução imediata com o período já faturado vira crédito do cliente.
	// A troca ocorre no primeiro dia do período (fração 1): crédito de
	// 2 × 50,00 − 1 × 50,00 = 50,00, sem nova fatura.
	issuedInvoice(c, sub.ID)
	etag := subscriptionETag(c, sub.ID)
	c.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPost, path, map[string]any{"quantity": 1, "timing": "immediate"},
		"If-Match", `"2"`)
	out, h = c.expect(http.StatusOK, http.MethodPost, path, map[string]any{"quantity": 1, "timing": "immediate"}, "If-Match", etag)
	if out["quantity"] != float64(1) || len(pendingChanges(t, out)) != 0 || h.Get("ETag") == etag {
		t.Fatalf("redução imediata: %s %v", h.Get("ETag"), out)
	}
	credits, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/customers/"+sub.CustomerID.String()+"/credits", nil)
	if credits["balance"] != float64(5000) {
		t.Fatalf("crédito de prorrata: %v", credits)
	}
	if invs := subscriptionInvoices(c, sub.ID); len(invs) != 1 {
		t.Fatalf("redução não deveria emitir fatura: %v", invs)
	}

	flatSub := newSubscription(t, a.env, tn, flat.ID, 1, nil)
	c.expectProblem(http.StatusUnprocessableEntity, "invalid_change", http.MethodPost, "/api/v1/subscriptions/"+flatSub.ID.String()+"/change-quantity",
		map[string]any{"quantity": 3, "timing": "next_cycle"}, "If-Match", `"1"`)
}

func TestAdvancedAPIImmediateUpgradeIssuesProrationInvoice(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "upgrade")
	c := a.login(tn.Owner, &tn.ID)
	seats := newPlan(t, a.env, tn, "Plano por assento", catalog.PriceInput{Model: catalog.PerUnit, UnitAmount: "5000"})
	sub := newSubscription(t, a.env, tn, seats.ID, 2, nil)
	issuedInvoice(c, sub.ID)

	// No primeiro dia do período a fração é 1: crédito de 2 × 50,00 e
	// cobrança de 3 × 50,00, diferença de 50,00 em fatura de prorrata.
	out, _ := c.expect(http.StatusOK, http.MethodPost, "/api/v1/subscriptions/"+sub.ID.String()+"/change-quantity",
		map[string]any{"quantity": 3, "timing": "immediate"}, "If-Match", subscriptionETag(c, sub.ID))
	if out["quantity"] != float64(3) {
		t.Fatalf("aumento imediato: %v", out)
	}
	var proration map[string]any
	for _, inv := range subscriptionInvoices(c, sub.ID) {
		if inv["kind"] == "proration" {
			proration = inv
		}
	}
	if proration == nil || proration["total"] != float64(5000) || proration["status"] != "open" {
		t.Fatalf("fatura de prorrata: %v", proration)
	}
}

func TestAdvancedAPIPriceAdjustments(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "reajustes")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Reajuste", flatPrice(10000))
	sub := newSubscription(t, a.env, tn, plan.ID, 1, nil)
	otherSub := newSubscription(t, a.env, tn, plan.ID, 1, nil)
	subPath := "/api/v1/subscriptions/" + sub.ID.String()
	path := subPath + "/price-adjustments"
	today := tenantToday(t, a.env)
	first, second := today.AddDays(30), today.AddDays(60)

	adj, _ := c.expect(http.StatusCreated, http.MethodPost, path, map[string]any{"effective_date": first.String(), "kind": "percent",
		"percent_bp": 450, "index_name": "IPCA", "reason": "Reajuste anual pelo IPCA"}, "Idempotency-Key", idemKey())
	if adj["status"] != "scheduled" || adj["effective_date"] != first.String() || adj["percent_bp"] != float64(450) || adj["index_name"] != "IPCA" ||
		adj["applied_at"] != nil || adj["previous_amount"] != nil || adj["resulting_amount"] != nil || adj["previous_amount_decimal"] != nil ||
		adj["resulting_amount_decimal"] != nil || adj["canceled_at"] != nil || adj["cancel_reason"] != nil {
		t.Fatalf("reajuste percentual: %v", adj)
	}
	fixed, _ := c.expect(http.StatusCreated, http.MethodPost, path, map[string]any{"effective_date": second.String(), "kind": "fixed_amount",
		"new_amount": 12000, "reason": "Novo valor contratual"}, "Idempotency-Key", idemKey())
	if fixed["new_amount"] != float64(12000) || fixed["percent_bp"] != nil {
		t.Fatalf("reajuste por valor: %v", fixed)
	}
	c.expectProblem(http.StatusConflict, "adjustment_date_taken", http.MethodPost, path, map[string]any{"effective_date": first.String(),
		"kind": "percent", "percent_bp": 100, "reason": "Reajuste duplicado"}, "Idempotency-Key", idemKey())

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"data no passado", map[string]any{"effective_date": today.AddDays(-1).String(), "kind": "percent", "percent_bp": 100, "reason": "Atrasado"}},
		{"data mal formatada", map[string]any{"effective_date": "17/11/2026", "kind": "percent", "percent_bp": 100, "reason": "Formato"}},
		{"além do horizonte", map[string]any{"effective_date": today.AddDays(1200).String(), "kind": "percent", "percent_bp": 100, "reason": "Distante"}},
		{"percentual ausente", map[string]any{"effective_date": first.AddDays(1).String(), "kind": "percent", "reason": "Sem percentual"}},
		{"percentual com valor", map[string]any{"effective_date": first.AddDays(1).String(), "kind": "percent", "percent_bp": 100,
			"new_amount": 100, "reason": "Misto"}},
		{"tipo desconhecido", map[string]any{"effective_date": first.AddDays(1).String(), "kind": "indice", "reason": "Tipo"}},
		{"motivo curto", map[string]any{"effective_date": first.AddDays(1).String(), "kind": "percent", "percent_bp": 100, "reason": "ok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.expectProblem(http.StatusUnprocessableEntity, "invalid_adjustment", http.MethodPost, path, tc.body, "Idempotency-Key", idemKey())
		})
	}

	list, _ := c.expect(http.StatusOK, http.MethodGet, path, nil)
	items := objects(t, list["data"])
	if len(items) != 2 || items[0]["id"] != adj["id"] || items[1]["id"] != fixed["id"] {
		t.Fatalf("listagem ordenada por data efetiva: %v", items)
	}
	if other, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/subscriptions/"+otherSub.ID.String()+"/price-adjustments", nil); len(objects(t, other["data"])) != 0 {
		t.Fatalf("reajustes de outra assinatura vazaram: %v", other["data"])
	}

	adjPath := path + "/" + adj["id"].(string)
	// O reajuste só pode ser cancelado pela própria assinatura.
	c.expectProblem(http.StatusNotFound, "adjustment_not_found", http.MethodDelete,
		"/api/v1/subscriptions/"+otherSub.ID.String()+"/price-adjustments/"+adj["id"].(string), nil)
	c.expect(http.StatusNoContent, http.MethodDelete, adjPath, nil)
	c.expectProblem(http.StatusConflict, "adjustment_not_scheduled", http.MethodDelete, adjPath, nil)
	c.expectProblem(http.StatusNotFound, "adjustment_not_found", http.MethodDelete, path+"/"+uuid.NewString(), nil)

	list, _ = c.expect(http.StatusOK, http.MethodGet, path, nil)
	status := map[string]any{}
	listed := byField(t, objects(t, list["data"]), "id")
	for id, it := range listed {
		status[id] = it["status"]
	}
	if status[adj["id"].(string)] != "canceled" || status[fixed["id"].(string)] != "scheduled" {
		t.Fatalf("situação após cancelamento: %v", status)
	}
	if canceled := listed[adj["id"].(string)]; canceled["cancel_reason"] != "manual" || canceled["canceled_at"] == nil {
		t.Fatalf("motivo do cancelamento manual: %v", canceled)
	}
	// Cancelado o reajuste, a data fica livre para outro.
	c.expect(http.StatusCreated, http.MethodPost, path, map[string]any{"effective_date": first.String(), "kind": "percent", "percent_bp": 300,
		"reason": "Reajuste renegociado"}, "Idempotency-Key", idemKey())

	hist, _ := c.expect(http.StatusOK, http.MethodGet, subPath+"/history", nil)
	events := map[string]int{}
	for _, e := range objects(t, hist["data"]) {
		events[e["event"].(string)]++
	}
	if events["price_adjustment_scheduled"] != 3 || events["price_adjustment_canceled"] != 1 {
		t.Fatalf("histórico de reajustes: %v", events)
	}
	c.expectProblem(http.StatusNotFound, "subscription_not_found", http.MethodGet, "/api/v1/subscriptions/"+uuid.NewString()+"/price-adjustments", nil)
}

// Reajuste percentual de preço por unidade com fração de centavo: a API
// devolve os valores arredondados a centavos para exibição e os valores
// exatos em texto; a fatura do ciclo seguinte arredonda o total uma vez.
func TestAdvancedAPIPerUnitAdjustmentExposesExactAmounts(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "reajuste-unidade")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano por requisição", catalog.PriceInput{Model: catalog.PerUnit, UnitAmount: "4.5"})
	sub := newSubscription(t, a.env, tn, plan.ID, 100_000, nil)
	issuedInvoice(c, sub.ID)
	path := "/api/v1/subscriptions/" + sub.ID.String() + "/price-adjustments"
	c.expect(http.StatusCreated, http.MethodPost, path, map[string]any{"effective_date": tenantToday(t, a.env).AddDays(1).String(),
		"kind": "percent", "percent_bp": 500, "reason": "Reajuste anual"}, "Idempotency-Key", idemKey())
	c.expectProblem(http.StatusUnprocessableEntity, "invalid_adjustment", http.MethodPost, path, map[string]any{
		"effective_date": tenantToday(t, a.env).AddDays(2).String(), "kind": "fixed_amount", "new_amount": 1_000_000_000_000,
		"reason": "Valor acima do limite"}, "Idempotency-Key", idemKey())

	a.env.Clock.Set(sub.CurrentPeriod.End.Add(10 * time.Hour))
	c = a.login(tn.Owner, &tn.ID)
	runBilling(c)
	var cycle map[string]any
	for _, inv := range subscriptionInvoices(c, sub.ID) {
		if inv["total"] == float64(472500) {
			cycle = inv
		}
	}
	if cycle == nil {
		t.Fatalf("fatura com 100.000 × R$ 0,04725 = R$ 4.725,00: %v", subscriptionInvoices(c, sub.ID))
	}
	list, _ := c.expect(http.StatusOK, http.MethodGet, path, nil)
	items := objects(t, list["data"])
	if len(items) != 1 {
		t.Fatalf("reajustes: %v", items)
	}
	adj := items[0]
	if adj["status"] != "applied" || adj["previous_amount"] != float64(4) || adj["resulting_amount"] != float64(5) ||
		adj["previous_amount_decimal"] != "4.5" || adj["resulting_amount_decimal"] != "4.725" || adj["cancel_reason"] != nil {
		t.Fatalf("valores do reajuste aplicado: %v", adj)
	}
}

// usageEvent monta um evento de consumo para a ingestão.
func usageEvent(externalID string, subID uuid.UUID, metricCode string, qty any, at time.Time) map[string]any {
	return map[string]any{"external_id": externalID, "subscription_id": subID, "metric_code": metricCode, "unit": "request", "quantity": qty,
		"occurred_at": at}
}

// with copia o evento substituindo os campos informados em pares chave e valor.
func with(ev map[string]any, kv ...any) map[string]any {
	out := make(map[string]any, len(ev)+len(kv)/2)
	for k, v := range ev {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// ingest envia o lote e indexa os resultados por external_id, conferindo a ordem.
func ingest(c *client, events ...map[string]any) map[string]map[string]any {
	c.a.t.Helper()
	out, _ := c.expect(http.StatusOK, http.MethodPost, "/api/v1/usage-events", map[string]any{"events": events})
	results := objects(c.a.t, out["results"])
	if len(results) != len(events) {
		c.a.t.Fatalf("resultados = %d, esperado %d: %v", len(results), len(events), results)
	}
	for i, r := range results {
		if r["external_id"] != events[i]["external_id"] {
			c.a.t.Fatalf("resultado %d fora de ordem: %v", i, r)
		}
	}
	return byField(c.a.t, results, "external_id")
}

func TestAdvancedAPIUsageIngestionAndListing(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "consumo")
	enableUsageBilling(t, a.env, tn)
	c := a.login(tn.Owner, &tn.ID)
	plan, metric := usagePlan(t, a.env, tn, "15")
	start := tenantToday(t, a.env)
	sub := newSubscription(t, a.env, tn, plan.ID, 1, &start)
	// Precisão de microssegundos, a mesma do banco.
	at := a.env.Clock.Now().Truncate(time.Microsecond)
	ev := func(id string, qty any) map[string]any { return usageEvent(id, sub.ID, metric.Code, qty, at) }

	res := ingest(c,
		ev("evt-1", "2.5"),
		ev("evt-2", 2),
		with(ev("evt-3", "1"), "properties", map[string]any{"rota": "/v1/pedidos", "status": 200}),
		with(ev("evt-4", "1"), "metric_code", "metrica.inexistente"),
		with(ev("evt-5", "1"), "unit", "minute"),
		ev("evt-6", "-1"),
		with(ev("evt-7", "1"), "occurred_at", at.Add(time.Hour)),
		ev("evt-8", "0.0000001"),
		ev("evt-9", "5000"),
		with(ev("evt-10", "1"), "subscription_id", uuid.New()),
		ev("evt-11", "doze"),
		with(ev("evt-12", "1"), "occurred_at", sub.StartedAt.Add(-time.Hour)),
	)
	accepted := map[string]string{}
	for _, id := range []string{"evt-1", "evt-2", "evt-3"} {
		r := res[id]
		eid, ok := r["event_id"].(string)
		if r["outcome"] != "accepted" || !ok || r["error"] != nil {
			t.Fatalf("%s deveria ser aceito: %v", id, r)
		}
		accepted[eid] = id
	}
	for _, id := range []string{"evt-4", "evt-5", "evt-6", "evt-7", "evt-8", "evt-9", "evt-10", "evt-11", "evt-12"} {
		r := res[id]
		if msg, _ := r["error"].(string); r["outcome"] != "rejected" || r["event_id"] != nil || msg == "" {
			t.Fatalf("%s deveria ser rejeitado com motivo: %v", id, r)
		}
	}
	if !strings.Contains(res["evt-10"]["error"].(string), "Assinatura não encontrada") {
		t.Fatalf("motivo da rejeição por assinatura: %v", res["evt-10"])
	}

	// Reenvio: mesmo conteúdo é duplicado (mesmo evento); conteúdo diferente é rejeitado.
	again := ingest(c,
		ev("evt-1", "2.50"),
		with(ev("evt-3", "1"), "properties", map[string]any{"status": 200, "rota": "/v1/pedidos"}),
		ev("evt-2", 3),
	)
	if again["evt-1"]["outcome"] != "duplicate" || accepted[again["evt-1"]["event_id"].(string)] != "evt-1" {
		t.Fatalf("reenvio idêntico: %v", again["evt-1"])
	}
	if again["evt-3"]["outcome"] != "duplicate" || accepted[again["evt-3"]["event_id"].(string)] != "evt-3" {
		t.Fatalf("reenvio com propriedades em outra ordem: %v", again["evt-3"])
	}
	if msg, _ := again["evt-2"]["error"].(string); again["evt-2"]["outcome"] != "rejected" || !strings.Contains(msg, "conteúdo diferente") {
		t.Fatalf("reenvio com conteúdo diferente: %v", again["evt-2"])
	}

	c.expectProblem(http.StatusUnprocessableEntity, "invalid_usage_batch", http.MethodPost, "/api/v1/usage-events", map[string]any{"events": []any{}})
	c.expectProblem(http.StatusUnprocessableEntity, "invalid_usage_batch", http.MethodPost, "/api/v1/usage-events", map[string]any{})
	c.expectProblem(http.StatusUnprocessableEntity, "invalid_json", http.MethodPost, "/api/v1/usage-events",
		map[string]any{"events": []map[string]any{with(ev("evt-13", "1"), "origem", "teste")}})

	// Paginação: duas páginas, sem repetição nem lacuna.
	seen := map[string]map[string]any{}
	q := url.Values{"subscription_id": {sub.ID.String()}, "limit": {"2"}}
	pages := 0
	for {
		page, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/usage-events?"+q.Encode(), nil)
		pages++
		for _, e := range objects(t, page["data"]) {
			id := e["id"].(string)
			if _, dup := seen[id]; dup {
				t.Fatalf("evento %s repetido entre páginas", id)
			}
			seen[id] = e
		}
		next, ok := page["next_cursor"].(string)
		if !ok {
			if page["next_cursor"] != nil {
				t.Fatalf("next_cursor inválido: %v", page["next_cursor"])
			}
			break
		}
		if pages > 3 {
			t.Fatal("paginação não terminou")
		}
		q.Set("cursor", next)
	}
	if pages != 2 || len(seen) != 3 {
		t.Fatalf("paginação: %d páginas, %d eventos", pages, len(seen))
	}
	wantQty := map[string]string{"evt-1": "2.5", "evt-2": "2", "evt-3": "1"}
	for id, e := range seen {
		ext, ok := accepted[id]
		if !ok || e["external_id"] != ext || e["quantity"] != wantQty[ext] || e["status"] != "accepted" || e["metric_id"] != metric.ID.String() ||
			e["subscription_id"] != sub.ID.String() || e["invoice_item_id"] != nil {
			t.Fatalf("evento listado: %v", e)
		}
		if !instant(t, e["period_start"]).Equal(sub.CurrentPeriod.Start) || !instant(t, e["occurred_at"]).Equal(at) {
			t.Fatalf("período ou ocorrência do evento: %v", e)
		}
	}
	byMetric, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/usage-events?metric_id="+metric.ID.String(), nil)
	if n := len(objects(t, byMetric["data"])); n != 3 || byMetric["next_cursor"] != nil {
		t.Fatalf("filtro por métrica: %d eventos, cursor %v", n, byMetric["next_cursor"])
	}
	c.expectProblem(http.StatusUnprocessableEntity, "invalid_limit", http.MethodGet, "/api/v1/usage-events?limit=0", nil)
	c.expectProblem(http.StatusUnprocessableEntity, "invalid_cursor", http.MethodGet, "/api/v1/usage-events?cursor=invalido", nil)
	c.expectProblem(http.StatusUnprocessableEntity, "invalid_filter", http.MethodGet, "/api/v1/usage-events?subscription_id=123", nil)

	// Consumo do período: 2,5 + 2 + 1 = 5,5 unidades × R$ 0,15 = 82,5
	// centavos, arredondado uma vez por half-even para 82.
	usage, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/subscriptions/"+sub.ID.String()+"/usage", nil)
	if !instant(t, usage["period_start"]).Equal(sub.CurrentPeriod.Start) || !instant(t, usage["period_end"]).Equal(sub.CurrentPeriod.End) {
		t.Fatalf("período do consumo: %v", usage)
	}
	metrics := objects(t, usage["metrics"])
	if len(metrics) != 1 {
		t.Fatalf("métricas do consumo: %v", usage["metrics"])
	}
	m := metrics[0]
	if m["metric_id"] != metric.ID.String() || m["code"] != metric.Code || m["unit"] != "request" || m["aggregation"] != "sum" ||
		m["quantity"] != "5.5" || m["event_count"] != float64(3) || m["estimated_amount"] != float64(82) {
		t.Fatalf("consumo agregado: %v", m)
	}
	c.expectProblem(http.StatusNotFound, "subscription_not_found", http.MethodGet, "/api/v1/subscriptions/"+uuid.NewString()+"/usage", nil)
}

// countOutbox conta eventos da outbox de um agregado do tenant.
func countOutbox(t *testing.T, env *fixture.Env, tenantID uuid.UUID, eventType string, aggregateID uuid.UUID) int {
	t.Helper()
	var n int
	err := env.DB.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2 AND aggregate_id = $3`,
			tenantID, eventType, aggregateID).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAdvancedAPICreditNotesAreIdempotent(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "notas")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Nota", flatPrice(15000))
	sub := newSubscription(t, a.env, tn, plan.ID, 1, nil)
	inv := issuedInvoice(c, sub.ID)
	invID := uuid.MustParse(inv["id"].(string))
	invPath := "/api/v1/invoices/" + invID.String()
	number, _ := inv["number"].(string)

	before, _ := c.expect(http.StatusOK, http.MethodGet, invPath, nil)
	if notes := jsonArray(t, before["credit_notes"]); len(notes) != 0 || before["amount_due"] != float64(15000) || number == "" {
		t.Fatalf("fatura antes das notas: %v", before)
	}
	notePath := invPath + "/credit-notes"
	body := map[string]any{"amount": 4000, "reason": "Desconto comercial negociado"}
	c.expectProblem(http.StatusUnprocessableEntity, "idempotency_key_required", http.MethodPost, notePath, body)

	key := "nota-" + uuid.NewString()
	first, h1 := c.expect(http.StatusCreated, http.MethodPost, notePath, body, "Idempotency-Key", key)
	notes := objects(t, first["credit_notes"])
	if h1.Get("Idempotent-Replayed") != "" || first["amount_credited"] != float64(4000) || first["amount_due"] != float64(11000) ||
		first["total"] != float64(15000) || first["status"] != "open" || len(notes) != 1 {
		t.Fatalf("primeira nota de crédito: %v", first)
	}
	if notes[0]["number"] != number+"-NC01" || notes[0]["amount"] != float64(4000) || notes[0]["reason"] != "Desconto comercial negociado" {
		t.Fatalf("nota emitida: %v", notes[0])
	}
	second, h2 := c.expect(http.StatusCreated, http.MethodPost, notePath, body, "Idempotency-Key", key)
	if h2.Get("Idempotent-Replayed") != "true" || !reflect.DeepEqual(first, second) {
		t.Fatalf("repetição deveria devolver a mesma resposta:\n%v\n%v", first, second)
	}
	c.expectProblem(http.StatusConflict, "idempotency_key_reused", http.MethodPost, notePath,
		map[string]any{"amount": 1000, "reason": "Outro conteúdo"}, "Idempotency-Key", key)

	for _, tc := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"valor zero", map[string]any{"amount": 0, "reason": "Sem valor"}, "invalid_credit_note"},
		{"valor negativo", map[string]any{"amount": -10, "reason": "Negativo"}, "invalid_credit_note"},
		{"acima do saldo", map[string]any{"amount": 11001, "reason": "Excede o saldo"}, "credit_note_exceeds_due"},
		{"motivo curto", map[string]any{"amount": 100, "reason": "ok"}, "invalid_credit_note"},
		{"campo desconhecido", map[string]any{"amount": 100, "reason": "Campo extra", "currency": "BRL"}, "invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.expectProblem(http.StatusUnprocessableEntity, tc.code, http.MethodPost, notePath, tc.body, "Idempotency-Key", "nota-"+uuid.NewString())
		})
	}

	got, h := c.expect(http.StatusOK, http.MethodGet, invPath, nil)
	notes = objects(t, got["credit_notes"])
	if len(notes) != 1 || notes[0]["id"] != objects(t, first["credit_notes"])[0]["id"] || got["amount_due"] != float64(11000) || h.Get("ETag") == "" {
		t.Fatalf("detalhe da fatura após a nota: %v", got)
	}
	// A fatura emitida não é reescrita: itens, subtotal e total permanecem.
	if !reflect.DeepEqual(got["items"], before["items"]) || got["subtotal"] != before["subtotal"] || got["total"] != before["total"] {
		t.Fatalf("itens ou totais da fatura foram alterados:\n%v\n%v", before, got)
	}

	// Nota pelo saldo restante quita a fatura.
	last, _ := c.expect(http.StatusCreated, http.MethodPost, notePath, map[string]any{"amount": 11000, "reason": "Encerramento do saldo"},
		"Idempotency-Key", "nota-"+uuid.NewString())
	notes = objects(t, last["credit_notes"])
	numbers := map[string]bool{}
	for _, n := range notes {
		numbers[n["number"].(string)] = true
	}
	if last["status"] != "paid" || last["amount_due"] != float64(0) || last["amount_credited"] != float64(15000) || len(notes) != 2 ||
		!numbers[number+"-NC01"] || !numbers[number+"-NC02"] {
		t.Fatalf("quitação por nota de crédito: %v", last)
	}
	c.expectProblem(http.StatusConflict, "invoice_not_open", http.MethodPost, notePath, map[string]any{"amount": 100, "reason": "Após a quitação"},
		"Idempotency-Key", "nota-"+uuid.NewString())
	c.expectProblem(http.StatusNotFound, "invoice_not_found", http.MethodPost, "/api/v1/invoices/"+uuid.NewString()+"/credit-notes", body,
		"Idempotency-Key", "nota-"+uuid.NewString())

	reasons := map[string]int{}
	for _, e := range objects(t, last["history"]) {
		if r, ok := e["reason"].(string); ok {
			reasons[r]++
		}
	}
	if reasons["credit_note"] != 2 {
		t.Fatalf("histórico da fatura: %v", last["history"])
	}
	if n := countOutbox(t, a.env, tn.ID, outbox.EventInvoiceCredited, invID); n != 2 {
		t.Fatalf("eventos invoice.credited = %d, esperado 2", n)
	}
	if n := countOutbox(t, a.env, tn.ID, outbox.EventInvoicePaid, invID); n != 1 {
		t.Fatalf("eventos invoice.paid = %d, esperado 1", n)
	}
}

func TestAdvancedAPIWritePermissions(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "permissoes")
	enableUsageBilling(t, a.env, tn)
	owner := a.login(tn.Owner, &tn.ID)
	auditor := a.login(member(t, a.env, tn, tenancy.RoleAuditor), &tn.ID)
	operator := a.login(member(t, a.env, tn, tenancy.RoleOperator), &tn.ID)

	flat := newPlan(t, a.env, tn, "Plano Fixo", flatPrice(10000))
	premium := newPlan(t, a.env, tn, "Plano Premium", flatPrice(15000))
	seats := newPlan(t, a.env, tn, "Plano por assento", catalog.PriceInput{Model: catalog.PerUnit, UnitAmount: "3000"})
	usage, metric := usagePlan(t, a.env, tn, "10")
	sub := newSubscription(t, a.env, tn, flat.ID, 1, nil)
	seatSub := newSubscription(t, a.env, tn, seats.ID, 2, nil)
	start := tenantToday(t, a.env)
	usageSub := newSubscription(t, a.env, tn, usage.ID, 1, &start)
	inv := issuedInvoice(owner, sub.ID)
	coupon, _ := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/coupons", map[string]any{"code": "AUDITORIA", "name": "Auditoria",
		"discount_type": "percent", "percent_off_bp": 1000, "duration": "once"})
	subPath := "/api/v1/subscriptions/" + sub.ID.String()
	adj, _ := owner.expect(http.StatusCreated, http.MethodPost, subPath+"/price-adjustments", map[string]any{
		"effective_date": tenantToday(t, a.env).AddDays(20).String(), "kind": "percent", "percent_bp": 500, "reason": "Reajuste programado"}, "Idempotency-Key", idemKey())
	etag := subscriptionETag(owner, sub.ID)
	seatETag := subscriptionETag(owner, seatSub.ID)
	at := a.env.Clock.Now().Truncate(time.Microsecond)
	invPath := "/api/v1/invoices/" + inv["id"].(string)

	writes := []struct {
		name, method, path string
		body               any
		extra              []string
	}{
		{"criar cupom", http.MethodPost, "/api/v1/coupons", map[string]any{"code": "NEGADO", "name": "Negado", "discount_type": "percent",
			"percent_off_bp": 100, "duration": "once"}, nil},
		{"arquivar cupom", http.MethodPost, "/api/v1/coupons/" + coupon["id"].(string) + "/archive", nil, nil},
		{"trocar plano", http.MethodPost, subPath + "/change-plan", map[string]any{"plan_id": premium.ID, "timing": "next_cycle"},
			[]string{"If-Match", etag}},
		{"trocar quantidade", http.MethodPost, "/api/v1/subscriptions/" + seatSub.ID.String() + "/change-quantity",
			map[string]any{"quantity": 3, "timing": "next_cycle"}, []string{"If-Match", seatETag}},
		{"programar reajuste", http.MethodPost, subPath + "/price-adjustments", map[string]any{"effective_date": tenantToday(t, a.env).AddDays(40).String(),
			"kind": "percent", "percent_bp": 100, "reason": "Reajuste negado"}, []string{"Idempotency-Key", idemKey()}},
		{"cancelar reajuste", http.MethodDelete, subPath + "/price-adjustments/" + adj["id"].(string), nil, nil},
		{"aplicar cupom", http.MethodPost, subPath + "/discounts", map[string]any{"coupon_code": "AUDITORIA"}, []string{"Idempotency-Key", idemKey()}},
		{"remover desconto", http.MethodDelete, subPath + "/discounts", nil, nil},
		{"enviar consumo", http.MethodPost, "/api/v1/usage-events", map[string]any{"events": []map[string]any{
			usageEvent("aud-1", usageSub.ID, metric.Code, "1", at)}}, nil},
		{"emitir nota de crédito", http.MethodPost, invPath + "/credit-notes", map[string]any{"amount": 100, "reason": "Nota negada"},
			[]string{"Idempotency-Key", "nota-" + uuid.NewString()}},
	}
	for _, w := range writes {
		t.Run("auditor "+w.name, func(t *testing.T) {
			auditor.expectProblem(http.StatusForbidden, "forbidden", w.method, w.path, w.body, w.extra...)
		})
	}
	for _, path := range []string{"/api/v1/coupons", subPath + "/price-adjustments", "/api/v1/usage-events",
		"/api/v1/subscriptions/" + usageSub.ID.String() + "/usage", invPath, subPath} {
		auditor.expect(http.StatusOK, http.MethodGet, path, nil)
	}

	// As escritas negadas não tiveram efeito.
	list, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/coupons", nil)
	coupons := byField(t, objects(t, list["data"]), "code")
	if len(coupons) != 1 || coupons["AUDITORIA"]["status"] != "active" || coupons["AUDITORIA"]["redemptions_count"] != float64(0) {
		t.Fatalf("cupons após escritas negadas: %v", list["data"])
	}
	adjs, _ := owner.expect(http.StatusOK, http.MethodGet, subPath+"/price-adjustments", nil)
	if items := objects(t, adjs["data"]); len(items) != 1 || items[0]["status"] != "scheduled" {
		t.Fatalf("reajustes após escritas negadas: %v", adjs["data"])
	}
	got, h := owner.expect(http.StatusOK, http.MethodGet, subPath, nil)
	if h.Get("ETag") != etag || len(objects(t, got["scheduled_changes"])) != 0 || got["discount"] != nil {
		t.Fatalf("assinatura alterada por escritas negadas: %s %v", h.Get("ETag"), got)
	}
	detail, _ := owner.expect(http.StatusOK, http.MethodGet, invPath, nil)
	if len(jsonArray(t, detail["credit_notes"])) != 0 || detail["amount_due"] != float64(10000) {
		t.Fatalf("fatura alterada por escritas negadas: %v", detail)
	}
	events, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/usage-events", nil)
	if n := len(objects(t, events["data"])); n != 0 {
		t.Fatalf("consumo gravado por escrita negada: %d eventos", n)
	}

	// Operador mantém assinaturas e consumo, mas não catálogo nem faturas.
	res := ingest(operator, usageEvent("op-1", usageSub.ID, metric.Code, "1", at))
	if res["op-1"]["outcome"] != "accepted" {
		t.Fatalf("operador deveria enviar consumo: %v", res["op-1"])
	}
	if e := subscriptionETag(owner, seatSub.ID); e != seatETag {
		t.Fatalf("assinatura por assento alterada por escrita negada: %s, esperado %s", e, seatETag)
	}
	operator.expect(http.StatusOK, http.MethodPost, "/api/v1/subscriptions/"+seatSub.ID.String()+"/change-quantity",
		map[string]any{"quantity": 3, "timing": "next_cycle"}, "If-Match", seatETag)
	operator.expect(http.StatusCreated, http.MethodPost, subPath+"/discounts", map[string]any{"coupon_code": "AUDITORIA"}, "Idempotency-Key", idemKey())
	operator.expectProblem(http.StatusForbidden, "forbidden", http.MethodPost, "/api/v1/coupons", map[string]any{"code": "OPERADOR", "name": "Operador",
		"discount_type": "percent", "percent_off_bp": 100, "duration": "once"})
	operator.expectProblem(http.StatusForbidden, "forbidden", http.MethodPost, invPath+"/credit-notes", map[string]any{"amount": 100,
		"reason": "Nota do operador"}, "Idempotency-Key", "nota-"+uuid.NewString())
}

func TestAdvancedAPITenantIsolation(t *testing.T) {
	a := newAdvancedAPI(t)
	tnA, tnB := a.env.NewTenant(t, "iso-a"), a.env.NewTenant(t, "iso-b")
	enableUsageBilling(t, a.env, tnA)
	enableUsageBilling(t, a.env, tnB)
	ownerA, ownerB := a.login(tnA.Owner, &tnA.ID), a.login(tnB.Owner, &tnB.ID)

	planA := newPlan(t, a.env, tnA, "Plano A", flatPrice(10000))
	usageA, metricA := usagePlan(t, a.env, tnA, "10")
	subA := newSubscription(t, a.env, tnA, planA.ID, 1, nil)
	start := tenantToday(t, a.env)
	usageSubA := newSubscription(t, a.env, tnA, usageA.ID, 1, &start)
	invA := issuedInvoice(ownerA, subA.ID)
	couponA, _ := ownerA.expect(http.StatusCreated, http.MethodPost, "/api/v1/coupons", map[string]any{"code": "EXCLUSIVO-A", "name": "Exclusivo A",
		"discount_type": "percent", "percent_off_bp": 1000, "duration": "once"})
	ownerA.expect(http.StatusCreated, http.MethodPost, "/api/v1/coupons", map[string]any{"code": "SOMENTE-A", "name": "Somente A",
		"discount_type": "percent", "percent_off_bp": 500, "duration": "once"})
	subAPath := "/api/v1/subscriptions/" + subA.ID.String()
	adjA, _ := ownerA.expect(http.StatusCreated, http.MethodPost, subAPath+"/price-adjustments", map[string]any{
		"effective_date": start.AddDays(15).String(), "kind": "fixed_amount", "new_amount": 11000, "reason": "Reajuste do tenant A"}, "Idempotency-Key", idemKey())
	at := a.env.Clock.Now().Truncate(time.Microsecond)
	if r := ingest(ownerA, usageEvent("iso-1", usageSubA.ID, metricA.Code, "2", at)); r["iso-1"]["outcome"] != "accepted" {
		t.Fatalf("evento do tenant A: %v", r["iso-1"])
	}
	etagA := subscriptionETag(ownerA, subA.ID)

	planB := newPlan(t, a.env, tnB, "Plano B", flatPrice(10000))
	subB := newSubscription(t, a.env, tnB, planB.ID, 1, nil)
	subBPath := "/api/v1/subscriptions/" + subB.ID.String()
	invAPath := "/api/v1/invoices/" + invA["id"].(string)

	// Recursos de outro tenant respondem como inexistentes, com o mesmo
	// código de um identificador aleatório.
	random := uuid.NewString()
	for _, tc := range []struct {
		name, method, path, randomPath string
		body                           any
		extra                          []string
		code                           string
	}{
		{"arquivar cupom", http.MethodPost, "/api/v1/coupons/" + couponA["id"].(string) + "/archive", "/api/v1/coupons/" + random + "/archive",
			nil, matchVersion(1), "coupon_not_found"},
		{"trocar plano", http.MethodPost, subAPath + "/change-plan", "/api/v1/subscriptions/" + random + "/change-plan",
			map[string]any{"plan_id": planA.ID, "timing": "next_cycle"}, []string{"If-Match", etagA}, "subscription_not_found"},
		{"trocar quantidade", http.MethodPost, subAPath + "/change-quantity", "/api/v1/subscriptions/" + random + "/change-quantity",
			map[string]any{"quantity": 2, "timing": "next_cycle"}, []string{"If-Match", etagA}, "subscription_not_found"},
		{"listar reajustes", http.MethodGet, subAPath + "/price-adjustments", "/api/v1/subscriptions/" + random + "/price-adjustments",
			nil, nil, "subscription_not_found"},
		{"programar reajuste", http.MethodPost, subAPath + "/price-adjustments", "/api/v1/subscriptions/" + random + "/price-adjustments",
			map[string]any{"effective_date": start.AddDays(20).String(), "kind": "percent", "percent_bp": 100, "reason": "Tentativa do B"},
			[]string{"Idempotency-Key", idemKey()}, "subscription_not_found"},
		{"cancelar reajuste", http.MethodDelete, subAPath + "/price-adjustments/" + adjA["id"].(string),
			"/api/v1/subscriptions/" + random + "/price-adjustments/" + random, nil, nil, "subscription_not_found"},
		{"cancelar reajuste pela própria assinatura", http.MethodDelete, subBPath + "/price-adjustments/" + adjA["id"].(string),
			subBPath + "/price-adjustments/" + random, nil, nil, "adjustment_not_found"},
		{"aplicar cupom", http.MethodPost, subAPath + "/discounts", "/api/v1/subscriptions/" + random + "/discounts",
			map[string]any{"coupon_code": "EXCLUSIVO-A"}, []string{"Idempotency-Key", idemKey()}, "subscription_not_found"},
		{"remover desconto", http.MethodDelete, subAPath + "/discounts", "/api/v1/subscriptions/" + random + "/discounts", nil, nil,
			"subscription_not_found"},
		{"consumo do período", http.MethodGet, "/api/v1/subscriptions/" + usageSubA.ID.String() + "/usage", "/api/v1/subscriptions/" + random + "/usage",
			nil, nil, "subscription_not_found"},
		{"detalhe da fatura", http.MethodGet, invAPath, "/api/v1/invoices/" + random, nil, nil, "invoice_not_found"},
		{"nota de crédito", http.MethodPost, invAPath + "/credit-notes", "/api/v1/invoices/" + random + "/credit-notes",
			map[string]any{"amount": 100, "reason": "Tentativa do B"}, []string{"Idempotency-Key", "nota-" + uuid.NewString()}, "invoice_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ownerB.expectProblem(http.StatusNotFound, tc.code, tc.method, tc.path, tc.body, tc.extra...)
			extra := tc.extra
			if len(extra) == 2 && extra[0] == "Idempotency-Key" {
				extra = []string{"Idempotency-Key", idemKey()}
			}
			ownerB.expectProblem(http.StatusNotFound, tc.code, tc.method, tc.randomPath, tc.body, extra...)
		})
	}

	// Plano de outro tenant é tão inexistente quanto um identificador aleatório.
	for _, planID := range []uuid.UUID{planA.ID, uuid.New()} {
		ownerB.expectProblem(http.StatusNotFound, "plan_not_found", http.MethodPost, subBPath+"/change-plan",
			map[string]any{"plan_id": planID, "timing": "next_cycle"}, "If-Match", `"1"`)
	}

	listB, _ := ownerB.expect(http.StatusOK, http.MethodGet, "/api/v1/coupons", nil)
	if n := len(objects(t, listB["data"])); n != 0 {
		t.Fatalf("tenant B listou cupons do tenant A: %v", listB["data"])
	}
	// O código do cupom é exclusivo por tenant: o tenant B pode reutilizá-lo.
	ownerB.expect(http.StatusCreated, http.MethodPost, "/api/v1/coupons", map[string]any{"code": "EXCLUSIVO-A", "name": "Cupom do B",
		"discount_type": "amount", "amount_off": 500, "duration": "once"})
	ownerB.expectProblem(http.StatusUnprocessableEntity, "invalid_coupon", http.MethodPost, "/api/v1/coupons", map[string]any{"code": "PLANO-A",
		"name": "Plano alheio", "discount_type": "percent", "percent_off_bp": 100, "duration": "once", "applies_to_plan_ids": []string{planA.ID.String()}})
	ownerB.expectProblem(http.StatusUnprocessableEntity, "coupon_not_found", http.MethodPost, subBPath+"/discounts", map[string]any{"coupon_code": "SOMENTE-A"}, "Idempotency-Key", idemKey())

	for _, path := range []string{"/api/v1/usage-events", "/api/v1/usage-events?subscription_id=" + usageSubA.ID.String(),
		"/api/v1/usage-events?metric_id=" + metricA.ID.String()} {
		page, _ := ownerB.expect(http.StatusOK, http.MethodGet, path, nil)
		if n := len(objects(t, page["data"])); n != 0 {
			t.Fatalf("%s: tenant B listou %d eventos do tenant A", path, n)
		}
	}
	res := ingest(ownerB, usageEvent("iso-2", usageSubA.ID, metricA.Code, "1", at), usageEvent("iso-1", usageSubA.ID, metricA.Code, "2", at))
	for _, id := range []string{"iso-1", "iso-2"} {
		if msg, _ := res[id]["error"].(string); res[id]["outcome"] != "rejected" || !strings.Contains(msg, "Assinatura não encontrada") {
			t.Fatalf("evento com assinatura de outro tenant: %v", res[id])
		}
	}

	// Nada do tenant A foi alterado.
	coupons, _ := ownerA.expect(http.StatusOK, http.MethodGet, "/api/v1/coupons", nil)
	byCode := byField(t, objects(t, coupons["data"]), "code")
	if len(byCode) != 2 || byCode["EXCLUSIVO-A"]["status"] != "active" || byCode["EXCLUSIVO-A"]["redemptions_count"] != float64(0) ||
		byCode["EXCLUSIVO-A"]["name"] != "Exclusivo A" {
		t.Fatalf("cupons do tenant A: %v", coupons["data"])
	}
	adjs, _ := ownerA.expect(http.StatusOK, http.MethodGet, subAPath+"/price-adjustments", nil)
	if items := objects(t, adjs["data"]); len(items) != 1 || items[0]["status"] != "scheduled" {
		t.Fatalf("reajustes do tenant A: %v", adjs["data"])
	}
	got, h := ownerA.expect(http.StatusOK, http.MethodGet, subAPath, nil)
	if h.Get("ETag") != etagA || len(objects(t, got["scheduled_changes"])) != 0 || got["discount"] != nil {
		t.Fatalf("assinatura do tenant A alterada: %s %v", h.Get("ETag"), got)
	}
	detail, _ := ownerA.expect(http.StatusOK, http.MethodGet, invAPath, nil)
	if len(jsonArray(t, detail["credit_notes"])) != 0 || detail["amount_due"] != float64(10000) {
		t.Fatalf("fatura do tenant A alterada: %v", detail)
	}
	usage, _ := ownerA.expect(http.StatusOK, http.MethodGet, "/api/v1/subscriptions/"+usageSubA.ID.String()+"/usage", nil)
	if m := objects(t, usage["metrics"]); len(m) != 1 || m[0]["quantity"] != "2" || m[0]["event_count"] != float64(1) {
		t.Fatalf("consumo do tenant A: %v", usage["metrics"])
	}
}

func TestAdvancedAPIUsageRetryWithNanosecondTimestampIsDuplicate(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "consumo-ns")
	enableUsageBilling(t, a.env, tn)
	c := a.login(tn.Owner, &tn.ID)
	plan, metric := usagePlan(t, a.env, tn, "10")
	start := tenantToday(t, a.env)
	sub := newSubscription(t, a.env, tn, plan.ID, 1, &start)
	// Instante com nanossegundos, como o produzido por time.Now em clientes Go.
	at := a.env.Clock.Now().Truncate(time.Second).Add(123456789 * time.Nanosecond)
	if at.After(a.env.Clock.Now()) {
		at = at.Add(-time.Second)
	}
	ev := usageEvent("ns-1", sub.ID, metric.Code, "1", at)

	first := ingest(c, ev)
	if first["ns-1"]["outcome"] != "accepted" {
		t.Fatalf("primeiro envio: %v", first["ns-1"])
	}
	retry := ingest(c, ev)
	if retry["ns-1"]["outcome"] != "duplicate" || retry["ns-1"]["event_id"] != first["ns-1"]["event_id"] {
		t.Fatalf("reenvio idêntico deveria ser duplicate: %v", retry["ns-1"])
	}
}

func TestAdvancedAPIUsageSameBatchConflictingEventIsRejected(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "consumo-lote")
	enableUsageBilling(t, a.env, tn)
	c := a.login(tn.Owner, &tn.ID)
	plan, metric := usagePlan(t, a.env, tn, "10")
	start := tenantToday(t, a.env)
	sub := newSubscription(t, a.env, tn, plan.ID, 1, &start)
	at := a.env.Clock.Now().Truncate(time.Microsecond)

	out, _ := c.expect(http.StatusOK, http.MethodPost, "/api/v1/usage-events", map[string]any{"events": []map[string]any{
		usageEvent("lote-1", sub.ID, metric.Code, "1", at),
		usageEvent("lote-1", sub.ID, metric.Code, "1", at),
		usageEvent("lote-1", sub.ID, metric.Code, "5", at),
	}})
	results := objects(t, out["results"])
	if len(results) != 3 || results[0]["outcome"] != "accepted" || results[1]["outcome"] != "duplicate" ||
		results[1]["event_id"] != results[0]["event_id"] {
		t.Fatalf("repetição idêntica no mesmo lote: %v", results)
	}
	if results[2]["outcome"] != "rejected" || results[2]["event_id"] != nil {
		t.Fatalf("external_id repetido com conteúdo diferente no mesmo lote deveria ser rejeitado: %v", results[2])
	}
}

// Caractere NUL (\u0000 no JSON) em external_id, metric_code, unique_key ou
// properties é rejeitado por evento: a resposta é 200 com os demais eventos
// gravados, sem erro interno.
func TestAdvancedAPIUsageRejectsNULWithoutFailingBatch(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "consumo-nul")
	enableUsageBilling(t, a.env, tn)
	c := a.login(tn.Owner, &tn.ID)
	plan, metric := usagePlan(t, a.env, tn, "10")
	start := tenantToday(t, a.env)
	sub := newSubscription(t, a.env, tn, plan.ID, 1, &start)
	at := a.env.Clock.Now().Truncate(time.Microsecond)
	ev := func(id string) map[string]any { return usageEvent(id, sub.ID, metric.Code, "1", at) }

	events := []map[string]any{
		ev("nul\x00id"),
		with(ev("nul-codigo"), "metric_code", metric.Code+"\x00"),
		with(ev("nul-chave"), "unique_key", "chave\x00"),
		with(ev("nul-propriedade"), "properties", map[string]any{"nota": "a\x00b"}),
		ev("valido"),
	}
	out, _ := c.expect(http.StatusOK, http.MethodPost, "/api/v1/usage-events", map[string]any{"events": events})
	results := objects(t, out["results"])
	if len(results) != len(events) {
		t.Fatalf("resultados: %v", results)
	}
	for i, r := range results[:4] {
		if msg, _ := r["error"].(string); r["outcome"] != "rejected" || r["event_id"] != nil || !strings.Contains(msg, "NUL") {
			t.Fatalf("evento %d com NUL deveria ser rejeitado com motivo: %v", i, r)
		}
	}
	if results[4]["outcome"] != "accepted" {
		t.Fatalf("evento válido do mesmo lote: %v", results[4])
	}
}

// requestKey devolve a chave de negócio gravada pelo caso de uso para a
// Idempotency-Key informada: hex(SHA-256(cliente|operação|chave)).
func requestKey(userID uuid.UUID, operation, key string) string {
	sum := sha256.Sum256([]byte("user:" + userID.String() + "|" + operation + "|" + key))
	return hex.EncodeToString(sum[:])
}

// forgetIdempotencyKey remove o registro HTTP da chave, como a expurgação
// após a validade de 24 h; a partir daí só a chave de negócio protege contra
// duplicidade.
func forgetIdempotencyKey(t *testing.T, env *fixture.Env, tenantID uuid.UUID, operation, key string) {
	t.Helper()
	err := env.DB.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(context.Background(), `DELETE FROM idempotency_keys WHERE tenant_id = $1 AND operation = $2 AND idempotency_key = $3`,
			tenantID, operation, key)
		if err == nil && tag.RowsAffected() != 1 {
			t.Errorf("registros de idempotência removidos: %d", tag.RowsAffected())
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// storedRequestKeys lista as chaves de negócio gravadas numa tabela do tenant.
func storedRequestKeys(t *testing.T, env *fixture.Env, tenantID uuid.UUID, table string) []string {
	t.Helper()
	var keys []string
	err := env.DB.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT request_key FROM `+table+` WHERE tenant_id = $1 AND request_key IS NOT NULL ORDER BY created_at`, tenantID)
		if err != nil {
			return err
		}
		keys, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestAdvancedAPICreditNoteAcceptsMaximumIdempotencyKey(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "nota-chave")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Chave", flatPrice(10000))
	sub := newSubscription(t, a.env, tn, plan.ID, 1, nil)
	inv := issuedInvoice(c, sub.ID)
	path := "/api/v1/invoices/" + inv["id"].(string) + "/credit-notes"
	const operation = "POST /api/v1/invoices/{id}/credit-notes"
	// 255 caracteres visíveis: o maior Idempotency-Key aceito pela API.
	key := strings.Repeat("k", 255)
	body := map[string]any{"amount": 1000, "reason": "Chave de tamanho máximo"}

	first, _ := c.expect(http.StatusCreated, http.MethodPost, path, body, "Idempotency-Key", key)
	second, h := c.expect(http.StatusCreated, http.MethodPost, path, body, "Idempotency-Key", key)
	if h.Get("Idempotent-Replayed") != "true" || !reflect.DeepEqual(first, second) || len(objects(t, first["credit_notes"])) != 1 {
		t.Fatalf("repetição com chave longa: %v / %v", first, second)
	}
	keys := storedRequestKeys(t, a.env, tn.ID, "credit_notes")
	if len(keys) != 1 || keys[0] != requestKey(tn.Owner.ID, operation, key) {
		t.Fatalf("chave de negócio da nota com tamanho fixo: %q", keys)
	}

	// Sem o registro HTTP, a chave de negócio devolve a nota já emitida.
	forgetIdempotencyKey(t, a.env, tn.ID, operation, key)
	third, h3 := c.expect(http.StatusCreated, http.MethodPost, path, body, "Idempotency-Key", key)
	notes := objects(t, third["credit_notes"])
	if h3.Get("Idempotent-Replayed") != "" || len(notes) != 1 || notes[0]["id"] != objects(t, first["credit_notes"])[0]["id"] ||
		third["amount_credited"] != float64(1000) || third["amount_due"] != float64(9000) {
		t.Fatalf("repetição após a expiração da chave HTTP não emite outra nota: %v", third)
	}
}

// A chave de negócio de pagamentos manuais e de créditos concedidos também
// tem tamanho fixo: Idempotency-Key de 255 caracteres não viola o limite das
// colunas request_key e a repetição continua idempotente.
func TestAdvancedAPIManualPaymentAndCreditAcceptMaximumIdempotencyKey(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "chave-longa")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Chave Longa", flatPrice(10000))
	sub := newSubscription(t, a.env, tn, plan.ID, 1, nil)
	inv := issuedInvoice(c, sub.ID)
	key := strings.Repeat("p", 255)

	payPath := "/api/v1/invoices/" + inv["id"].(string) + "/payments"
	const payOperation = "POST /api/v1/invoices/{id}/payments"
	payBody := map[string]any{"amount": 2500, "method": "pix", "note": "Transferência identificada no extrato"}
	paid, _ := c.expect(http.StatusCreated, http.MethodPost, payPath, payBody, "Idempotency-Key", key)
	forgetIdempotencyKey(t, a.env, tn.ID, payOperation, key)
	again, h := c.expect(http.StatusCreated, http.MethodPost, payPath, payBody, "Idempotency-Key", key)
	if h.Get("Idempotent-Replayed") != "" || again["id"] != paid["id"] {
		t.Fatalf("pagamento repetido com chave longa: %v / %v", paid, again)
	}
	if keys := storedRequestKeys(t, a.env, tn.ID, "payments"); len(keys) != 1 || keys[0] != requestKey(tn.Owner.ID, payOperation, key) {
		t.Fatalf("chave de negócio do pagamento: %q", keys)
	}
	got, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/invoices/"+inv["id"].(string), nil)
	if got["amount_paid"] != float64(2500) || got["amount_due"] != float64(7500) {
		t.Fatalf("pagamento aplicado uma única vez: %v", got)
	}

	customerID := got["customer_id"].(string)
	creditPath := "/api/v1/customers/" + customerID + "/credits"
	const creditOperation = "POST /api/v1/customers/{id}/credits"
	creditBody := map[string]any{"amount": 1500, "reason": "Cortesia comercial"}
	c.expect(http.StatusCreated, http.MethodPost, creditPath, creditBody, "Idempotency-Key", key)
	forgetIdempotencyKey(t, a.env, tn.ID, creditOperation, key)
	bal, _ := c.expect(http.StatusCreated, http.MethodPost, creditPath, creditBody, "Idempotency-Key", key)
	if bal["balance"] != float64(1500) {
		t.Fatalf("crédito concedido uma única vez: %v", bal)
	}
	keys := storedRequestKeys(t, a.env, tn.ID, "customer_credit_entries")
	if len(keys) != 1 || keys[0] != requestKey(tn.Owner.ID, creditOperation, key) {
		t.Fatalf("chave de negócio do crédito: %q", keys)
	}
}
