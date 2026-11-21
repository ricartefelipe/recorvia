//go:build integration

package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

func countIn(t *testing.T, env *fixture.Env, tenantID uuid.UUID, query string, args ...any) int {
	t.Helper()
	var n int
	err := env.DB.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), query, args...).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// idemRoute descreve uma operação financeira de criação incluída pela
// auditoria do RF-09: o pedido, um pedido de conteúdo diferente e a
// contagem dos recursos criados.
type idemRoute struct {
	name, path  string
	body, other map[string]any
	count       func() int
}

func idempotencyRoutes(t *testing.T, a *apiEnv, tn fixture.Tenant) []idemRoute {
	t.Helper()
	owner := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Idempotente", flatPrice(10000))
	cust := a.env.NewCustomer(t, tn)
	sub := newSubscription(t, a.env, tn, plan.ID, 1, nil)
	subPath := "/api/v1/subscriptions/" + sub.ID.String()
	for _, code := range []string{"IDEM-A", "IDEM-B"} {
		owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/coupons", map[string]any{"code": code, "name": "Cupom " + code,
			"discount_type": "percent", "percent_off_bp": 1000, "duration": "once"})
	}
	today := tenantToday(t, a.env)
	adjustment := func(days int) map[string]any {
		return map[string]any{"effective_date": today.AddDays(days).String(), "kind": "percent", "percent_bp": 300, "reason": "Reajuste anual"}
	}
	q := func(query string, args ...any) func() int {
		return func() int { return countIn(t, a.env, tn.ID, query, append([]any{tn.ID}, args...)...) }
	}
	return []idemRoute{
		{"criar assinatura", "/api/v1/subscriptions",
			map[string]any{"customer_id": cust.ID, "plan_id": plan.ID},
			map[string]any{"customer_id": cust.ID, "plan_id": plan.ID, "quantity": 1},
			q(`SELECT count(*) FROM subscriptions WHERE tenant_id = $1 AND customer_id = $2`, cust.ID)},
		{"pedir serviço adicional", "/api/v1/platform/addon-orders",
			map[string]any{"addon_code": "implantacao_assistida"},
			map[string]any{"addon_code": "implantacao_assistida", "notes": "Outro pedido"},
			q(`SELECT count(*) FROM platform_addon_orders WHERE tenant_id = $1`)},
		{"programar reajuste", subPath + "/price-adjustments", adjustment(30), adjustment(60),
			q(`SELECT count(*) FROM subscription_price_adjustments WHERE tenant_id = $1 AND subscription_id = $2`, sub.ID)},
		{"aplicar cupom", subPath + "/discounts",
			map[string]any{"coupon_code": "IDEM-A"}, map[string]any{"coupon_code": "IDEM-B"},
			q(`SELECT count(*) FROM subscription_discounts WHERE tenant_id = $1 AND subscription_id = $2`, sub.ID)},
	}
}

// RF-09: as operações financeiras de criação que não exigiam Idempotency-Key
// passam a exigir. Sem a chave, nada é criado; a repetição devolve o
// resultado registrado sem criar outro recurso; a mesma chave com conteúdo
// diferente é recusada.
func TestFinancialCreationRoutesRequireIdempotencyKey(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "idem-rotas")
	owner := a.login(tn.Owner, &tn.ID)
	for _, r := range idempotencyRoutes(t, a, tn) {
		t.Run(r.name, func(t *testing.T) {
			owner.expectProblem(http.StatusUnprocessableEntity, "idempotency_key_required", http.MethodPost, r.path, r.body)
			owner.expectProblem(http.StatusUnprocessableEntity, "idempotency_key_required", http.MethodPost, r.path, r.body, "Idempotency-Key", "curta")
			if n := r.count(); n != 0 {
				t.Fatalf("pedido sem chave criou %d recursos", n)
			}
			key := idemKey()
			first, h1 := owner.expect(http.StatusCreated, http.MethodPost, r.path, r.body, "Idempotency-Key", key)
			second, h2 := owner.expect(http.StatusCreated, http.MethodPost, r.path, r.body, "Idempotency-Key", key)
			if h1.Get("Idempotent-Replayed") != "" || h2.Get("Idempotent-Replayed") != "true" || !reflect.DeepEqual(first, second) || first["id"] == nil {
				t.Fatalf("repetição: %v / %v", first, second)
			}
			if n := r.count(); n != 1 {
				t.Fatalf("repetição criou outro recurso: %d", n)
			}
			// A impressão do pedido são os bytes do corpo (com método e
			// caminho): um corpo de outro conteúdo, ou mesmo equivalente com
			// outros bytes, é recusado com a mesma chave.
			owner.expectProblem(http.StatusConflict, "idempotency_key_reused", http.MethodPost, r.path, r.other, "Idempotency-Key", key)
			if n := r.count(); n != 1 {
				t.Fatalf("chave reutilizada criou outro recurso: %d", n)
			}
		})
	}
}

// A mesma Idempotency-Key usada em outra operação é independente: o
// registro tem escopo de tenant, cliente da API e operação (método e rota).
func TestSameIdempotencyKeyInAnotherOperationIsIndependent(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "idem-escopo")
	owner := a.login(tn.Owner, &tn.ID)
	routes := idempotencyRoutes(t, a, tn)
	key := idemKey()
	ids := map[any]bool{}
	for _, r := range routes {
		out, h := owner.expect(http.StatusCreated, http.MethodPost, r.path, r.body, "Idempotency-Key", key)
		if h.Get("Idempotent-Replayed") != "" || ids[out["id"]] {
			t.Fatalf("%s com a chave de outra operação devolveu resultado registrado: %v", r.name, out)
		}
		ids[out["id"]] = true
		if n := r.count(); n != 1 {
			t.Fatalf("%s: %d recursos", r.name, n)
		}
	}
	// Também em operações que já exigiam a chave.
	cust := a.env.NewCustomer(t, tn)
	credit, h := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/customers/"+cust.ID.String()+"/credits",
		map[string]any{"amount": 1500, "reason": "Cortesia comercial"}, "Idempotency-Key", key)
	if h.Get("Idempotent-Replayed") != "" || credit["balance"] != float64(1500) {
		t.Fatalf("crédito com a chave de outra operação: %v", credit)
	}
	// Outro membro do mesmo tenant com a mesma chave também é independente.
	other := a.login(member(t, a.env, tn, tenancy.RoleOperator), &tn.ID)
	out, h := other.expect(http.StatusCreated, http.MethodPost, routes[0].path, routes[0].body, "Idempotency-Key", key)
	if h.Get("Idempotent-Replayed") != "" || ids[out["id"]] || routes[0].count() != 2 {
		t.Fatalf("mesma chave de outro usuário: %v", out)
	}
	// Na mesma rota, outro recurso no caminho é outro pedido: a chave já
	// usada é recusada em vez de devolver o resultado do primeiro.
	sub := newSubscription(t, a.env, tn, routes[0].body["plan_id"].(uuid.UUID), 1, nil)
	owner.expectProblem(http.StatusConflict, "idempotency_key_reused", http.MethodPost, "/api/v1/subscriptions/"+sub.ID.String()+"/discounts",
		routes[3].body, "Idempotency-Key", key)
}

// Processamento interrompido depois da gravação da assinatura e antes do
// registro da resposta (lease expirado): a nova tentativa com a mesma chave
// assume o pedido e a chave de negócio devolve a assinatura já criada, sem
// criar outra nem faturar de novo.
func TestSubscriptionCreationRetryAfterInterruptedProcessingReturnsSameSubscription(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "idem-assinatura")
	owner := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Interrompido", flatPrice(10000))
	cust := a.env.NewCustomer(t, tn)
	const operation = "POST /api/v1/subscriptions"
	key := idemKey()
	body := map[string]any{"customer_id": cust.ID, "plan_id": plan.ID}
	first, _ := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/subscriptions", body, "Idempotency-Key", key)
	if keys := storedRequestKeys(t, a.env, tn.ID, "subscriptions"); len(keys) != 1 || keys[0] != requestKey(tn.Owner.ID, operation, key) {
		t.Fatalf("chave de negócio da assinatura: %q", keys)
	}
	past := a.env.Clock.Now().Add(-time.Minute)
	err := a.env.DB.InTenantTx(context.Background(), tn.ID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(context.Background(), `UPDATE idempotency_keys SET status = 'in_progress', response_status = NULL, response_body = NULL,
			response_content_type = NULL, response_location = NULL, completed_at = NULL, locked_until = $4
			WHERE tenant_id = $1 AND operation = $2 AND idempotency_key = $3`, tn.ID, operation, key, past)
		if err == nil && tag.RowsAffected() != 1 {
			t.Errorf("registros alterados: %d", tag.RowsAffected())
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	again, h := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/subscriptions", body, "Idempotency-Key", key)
	if h.Get("Idempotent-Replayed") != "" || again["id"] != first["id"] || h.Get("ETag") == "" {
		t.Fatalf("nova tentativa após interrupção: %v / %v", first, again)
	}
	if n := countIn(t, a.env, tn.ID, `SELECT count(*) FROM subscriptions WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, cust.ID); n != 1 {
		t.Fatalf("assinaturas após a nova tentativa: %d", n)
	}
	id, err := uuid.Parse(first["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if n := countOutbox(t, a.env, tn.ID, "subscription.created", id); n != 1 {
		t.Fatalf("eventos de criação: %d", n)
	}
	// Sem o registro HTTP (expirado), a chave de negócio ainda protege; para
	// outro cliente, a mesma chave é recusada.
	forgetIdempotencyKey(t, a.env, tn.ID, operation, key)
	third, _ := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/subscriptions", body, "Idempotency-Key", key)
	if third["id"] != first["id"] {
		t.Fatalf("repetição após a expiração do registro HTTP: %v", third)
	}
	forgetIdempotencyKey(t, a.env, tn.ID, operation, key)
	other := a.env.NewCustomer(t, tn)
	owner.expectProblem(http.StatusConflict, "request_key_reused", http.MethodPost, "/api/v1/subscriptions",
		map[string]any{"customer_id": other.ID, "plan_id": plan.ID}, "Idempotency-Key", key)
	forgetIdempotencyKey(t, a.env, tn.ID, operation, key)
	// Também para o mesmo cliente e plano com outro conteúdo de negócio: a
	// impressão dos campos do pedido fica gravada com a chave de negócio.
	for _, changed := range []map[string]any{
		{"customer_id": cust.ID, "plan_id": plan.ID, "quantity": 2},
		{"customer_id": cust.ID, "plan_id": plan.ID, "trial_days": 7},
		{"customer_id": cust.ID, "plan_id": plan.ID, "payment_terms_days": 0},
	} {
		owner.expectProblem(http.StatusConflict, "request_key_reused", http.MethodPost, "/api/v1/subscriptions", changed, "Idempotency-Key", key)
		forgetIdempotencyKey(t, a.env, tn.ID, operation, key)
	}
	// O mesmo conteúdo com a quantidade 1 explícita é o mesmo pedido.
	same, _ := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/subscriptions",
		map[string]any{"customer_id": cust.ID, "plan_id": plan.ID, "quantity": 1}, "Idempotency-Key", key)
	if same["id"] != first["id"] || countIn(t, a.env, tn.ID, `SELECT count(*) FROM subscriptions WHERE tenant_id = $1 AND customer_id = $2`,
		tn.ID, cust.ID) != 1 {
		t.Fatalf("mesmo conteúdo de negócio: %v", same)
	}
}

// Com credencial de API: as rotas exigem a chave do mesmo jeito, o registro e
// a chave de negócio são da credencial (a mesma chave enviada pela sessão do
// usuário é outro pedido), e o pedido de serviço adicional, restrito à sessão
// de uma pessoa, é recusado antes de qualquer efeito.
func TestFinancialCreationRoutesWithAPICredential(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := apiPlanTenant(t, a, "idem-credencial")
	owner := a.login(tn.Owner, &tn.ID)
	routes := idempotencyRoutes(t, a, tn)
	credID, _, secret := createCredential(t, owner, "subscriptions.read", "subscriptions.write")
	send := func(r idemRoute, body map[string]any, key string) rawResponse {
		t.Helper()
		var h map[string]string
		if key != "" {
			h = bearer(secret, "Idempotency-Key", key)
		} else {
			h = bearer(secret)
		}
		return a.raw(http.MethodPost, r.path, body, h)
	}
	for _, r := range []idemRoute{routes[0], routes[2], routes[3]} {
		t.Run(r.name, func(t *testing.T) {
			if res := send(r, r.body, ""); res.status != http.StatusUnprocessableEntity || res.body["code"] != "idempotency_key_required" {
				t.Fatalf("sem a chave: %d %v", res.status, res.body)
			}
			key := idemKey()
			first, second := send(r, r.body, key), send(r, r.body, key)
			if first.status != http.StatusCreated || second.status != http.StatusCreated || second.header.Get("Idempotent-Replayed") != "true" ||
				second.body["id"] != first.body["id"] || r.count() != 1 {
				t.Fatalf("repetição pela credencial: %d %v / %d %v (%d)", first.status, first.body, second.status, second.body, r.count())
			}
			if res := send(r, r.other, key); res.status != http.StatusConflict || res.body["code"] != "idempotency_key_reused" || r.count() != 1 {
				t.Fatalf("mesma chave com outro conteúdo: %d %v", res.status, res.body)
			}
		})
	}

	// Chave de negócio da assinatura com o cliente da credencial.
	const operation = "POST /api/v1/subscriptions"
	key := idemKey()
	sub := routes[0]
	created := send(sub, sub.body, key)
	if created.status != http.StatusCreated {
		t.Fatalf("assinatura pela credencial: %d %v", created.status, created.body)
	}
	sum := sha256.Sum256([]byte("api_credential:" + credID + "|" + operation + "|" + key))
	if !slices.Contains(storedRequestKeys(t, a.env, tn.ID, "subscriptions"), hex.EncodeToString(sum[:])) {
		t.Fatal("chave de negócio derivada do cliente da credencial ausente")
	}
	forgetIdempotencyKey(t, a.env, tn.ID, operation, key)
	if again := send(sub, sub.body, key); again.status != http.StatusCreated || again.body["id"] != created.body["id"] {
		t.Fatalf("repetição pela credencial após a expiração do registro HTTP: %d %v", again.status, again.body)
	}
	byUser, h := owner.expect(http.StatusCreated, http.MethodPost, sub.path, sub.body, "Idempotency-Key", key)
	if h.Get("Idempotent-Replayed") != "" || byUser["id"] == created.body["id"] {
		t.Fatalf("a mesma chave pela sessão do usuário é outro pedido: %v", byUser)
	}

	// Pedido de serviço adicional: somente sessão de usuário, com ou sem chave.
	addon := routes[1]
	for _, k := range []string{"", idemKey()} {
		if res := send(addon, addon.body, k); res.status != http.StatusForbidden || res.body["code"] != "user_session_required" {
			t.Fatalf("pedido de serviço adicional por credencial: %d %v", res.status, res.body)
		}
	}
	if n := addon.count(); n != 0 {
		t.Fatalf("pedido criado por credencial: %d", n)
	}
}
