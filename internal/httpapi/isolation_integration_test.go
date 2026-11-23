//go:build integration

package httpapi_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/httpapi"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/redisstore"
	"github.com/ricartefelipe/recorvia/internal/infra/safehttp"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways"
	"github.com/ricartefelipe/recorvia/internal/paysim"
	"github.com/ricartefelipe/recorvia/internal/portal"
	"github.com/ricartefelipe/recorvia/internal/reporting"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/testsupport/contract"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

// newIsolationAPI monta a API com todos os serviços que recebem
// identificadores de recursos do tenant (cadastro, catálogo, assinaturas,
// faturas, consumo, portal, credenciais, régua e webhooks enviados).
func newIsolationAPI(t *testing.T) *apiEnv {
	t.Helper()
	a := newAPI(t)
	env := a.env
	hosts := staticHosts{"hooks.cliente.test": {netip.MustParseAddr("93.184.215.14")}}
	hooks := webhooks.NewService(env.Store.Webhooks(), statusTransport{status: http.StatusServiceUnavailable},
		safehttp.New(safehttp.Options{Resolver: hosts}), env.Clock, webhooks.Config{Jitter: func() float64 { return 0.5 }})
	portalSvc := portal.NewService(env.Store.Portal(), redisstore.NewPortalSessions(redisstore.New(a.rdb, env.Keyring, "portal")), a.sessions,
		env.Clock, portal.DefaultPolicy())
	cfg := &config.Config{Env: config.Test, CookieName: "recorvia_session", PortalCookieName: "recorvia_portal", CookieSecure: false,
		RateLimitPerMinute: 10000, APICredentialRateLimitPerMinute: 10000}
	_, router := httpapi.NewRouter(httpapi.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Config: cfg, Clock: env.Clock,
		Store: env.Store, Sessions: a.sessions, Identity: identity.NewService(stubIDP{}, a.sessions, env.Store.Users(), env.Clock, identity.DefaultPolicy()),
		Tenancy: env.Tenancy, Platform: env.Platform, Customers: env.Customers, Catalog: env.Catalog, Subscriptions: env.Subscriptions,
		Billing: env.Billing, Changes: billing.NewChangeService(env.Store.Billing(), env.Clock),
		CreditNotes: billing.NewCreditNoteService(env.Store.Billing(), env.Clock), Coupons: catalog.NewCouponService(env.Store.Catalog(), env.Clock),
		Terms:    subscriptions.NewTermsService(env.Store.Subscriptions(), env.Clock),
		Payments: env.Payments(gateways.NewResolver(time.Second), payments.Config{WebhookBaseURL: "http://api.test/webhooks/providers"}),
		Metering: metering.NewService(env.Store.Metering(), env.Clock), Reporting: reporting.NewService(env.Store.Reporting(), env.Clock),
		Notifications: notifications.NewService(env.Store.Notifications(), nil, env.Keyring, env.Clock, notifications.Config{}),
		Collections:   collections.NewService(env.Store.Collections(), env.Clock, nil), Webhooks: hooks, Portal: portalSvc,
		Credentials: apicredentials.NewService(env.Store.APICredentials(), a.sessions, env.Clock, apicredentials.DefaultPolicy())})
	srv := httptest.NewServer(contract.Wrap(t, router))
	t.Cleanup(srv.Close)
	a.srv = srv
	return a
}

// crossCase é uma tentativa de alcançar, a partir do tenant B, um recurso
// do tenant A.
type crossCase struct {
	name, method, path string
	body               any
	extra              []string
}

// Acesso cruzado por referência direta: com uma sessão válida do tenant B
// (dono, com todas as permissões), os identificadores de recursos do tenant A
// nos caminhos, nos corpos e nos cursores respondem como inexistentes, nada
// do tenant A é alterado e a credencial de API de B tampouco os alcança. A
// listagem e o isolamento das rotas de assinatura avançadas, cupons,
// reajustes e notas de crédito estão em TestAdvancedAPITenantIsolation; os do
// portal em TestPortalCustomerFlowOverHTTP.
func TestCrossTenantReferencesOverHTTP(t *testing.T) {
	a := newIsolationAPI(t)
	tnA, tnB := apiPlanTenant(t, a, "cruzado-a"), apiPlanTenant(t, a, "cruzado-b")
	ownerA, ownerB := a.login(tnA.Owner, &tnA.ID), a.login(tnB.Owner, &tnB.ID)

	// Recursos do tenant A.
	custA := a.env.NewCustomer(t, tnA)
	contactA := addContact(t, a, tnA, custA.ID, "Financeiro A", true)
	invA := issueInvoice(t, a, tnA, custA.ID, 5000)
	planA := a.env.NewFlatPlan(t, tnA, 10000)
	subA := a.env.NewSubscription(t, tnA, custA.ID, planA.ID, nil)
	linkA, _ := newPortalLink(t, ownerA, custA.ID)
	credA, _, _ := createCredential(t, ownerA, "customers.read")
	eventType := webhooks.EventTypes()[0]
	endpointA, _ := ownerA.expect(http.StatusCreated, http.MethodPost, "/api/v1/webhook-endpoints",
		map[string]any{"url": "https://hooks.cliente.test/a", "event_types": []string{eventType}})
	endpointID := endpointA["endpoint"].(map[string]any)["id"].(string)
	policyA, _ := ownerA.expect(http.StatusCreated, http.MethodPost, "/api/v1/collection-policies", policyBody("Régua do A"))
	invitationA, _ := ownerA.expect(http.StatusCreated, http.MethodPost, "/api/v1/invitations",
		map[string]any{"email": "convidado-a@cruzado.test", "role": "auditor"})
	invitationID := invitationA["invitation"].(map[string]any)["id"].(string)
	for range 2 {
		a.env.NewCustomer(t, tnA)
	}

	// Recursos próprios do tenant B, usados com referências do A no corpo.
	custB := a.env.NewCustomer(t, tnB)
	planB := a.env.NewFlatPlan(t, tnB, 10000)
	subB := a.env.NewSubscription(t, tnB, custB.ID, planB.ID, nil)

	cust := "/api/v1/customers/" + custA.ID.String()
	sub := "/api/v1/subscriptions/" + subA.ID.String()
	inv := "/api/v1/invoices/" + invA.ID.String()
	plan := "/api/v1/plans/" + planA.ID.String()
	ifMatch := matchVersion(1)
	idem := func() []string { return []string{"Idempotency-Key", idemKey()} }
	address := map[string]any{"street": "Rua do B", "number": "1", "district": "Centro", "city": "São Paulo", "state": "SP", "postal_code": "01001-000"}

	// Referências no caminho: 404, como um identificador inexistente.
	for _, tc := range []crossCase{
		{"cliente", http.MethodGet, cust, nil, nil},
		{"alterar cliente", http.MethodPatch, cust, map[string]any{"kind": "individual", "name": "Tomado pelo B", "document_number": "529.982.247-25"}, ifMatch},
		{"arquivar cliente", http.MethodPost, cust + "/archive", nil, ifMatch},
		{"anonimizar cliente", http.MethodPost, cust + "/anonymize", map[string]any{"reason": "Pedido do titular pelo B"}, idem()},
		{"histórico do cliente", http.MethodGet, cust + "/history", nil, nil},
		{"incluir contato", http.MethodPost, cust + "/contacts", map[string]any{"name": "Contato do B", "email": "b@cruzado.test"}, nil},
		{"alterar contato", http.MethodPatch, cust + "/contacts/" + contactA.ID.String(), map[string]any{"name": "Contato do B"}, nil},
		{"remover contato", http.MethodDelete, cust + "/contacts/" + contactA.ID.String(), nil, nil},
		{"endereço de cobrança", http.MethodPut, cust + "/billing-address", address, ifMatch},
		{"saldo de crédito", http.MethodGet, cust + "/credits", nil, nil},
		{"conceder crédito", http.MethodPost, cust + "/credits", map[string]any{"amount": 100, "reason": "Crédito pelo B"}, idem()},
		{"listar links do portal", http.MethodGet, cust + "/portal-links", nil, nil},
		{"gerar link do portal", http.MethodPost, cust + "/portal-links", nil, nil},
		{"revogar link do portal", http.MethodDelete, cust + "/portal-links/" + linkA, nil, nil},
		{"alterar produto", http.MethodPatch, "/api/v1/products/" + planA.ProductID.String(), map[string]any{"name": "Produto do B", "status": "active"}, ifMatch},
		{"plano", http.MethodGet, plan, nil, nil},
		{"alterar plano", http.MethodPatch, plan, map[string]any{"name": "Plano do B", "trial_days": 0, "status": "active"}, ifMatch},
		{"novo preço", http.MethodPost, plan + "/prices", map[string]any{"pricing_model": "flat", "flat_amount": 1}, ifMatch},
		{"assinatura", http.MethodGet, sub, nil, nil},
		{"histórico da assinatura", http.MethodGet, sub + "/history", nil, nil},
		{"cancelar assinatura", http.MethodPost, sub + "/cancel", map[string]any{"mode": "immediate"}, ifMatch},
		{"suspender assinatura", http.MethodPost, sub + "/suspend", map[string]any{"reason": "Pelo B"}, ifMatch},
		{"reativar assinatura", http.MethodPost, sub + "/reactivate", nil, ifMatch},
		{"retomar renovação", http.MethodPost, sub + "/resume-renewal", nil, ifMatch},
		{"documento da fatura", http.MethodGet, inv + "/document", nil, nil},
		{"cancelar fatura", http.MethodPost, inv + "/void", map[string]any{"reason": "Pelo B"}, ifMatch},
		{"fatura incobrável", http.MethodPost, inv + "/mark-uncollectible", map[string]any{"reason": "Pelo B"}, ifMatch},
		{"pagamento manual", http.MethodPost, inv + "/payments", map[string]any{"amount": 5000, "method": "pix"}, idem()},
		{"cobrança no provedor", http.MethodPost, inv + "/charges", nil, idem()},
		{"alterar papel do membro", http.MethodPatch, "/api/v1/members/" + tnA.Owner.ID.String(), map[string]any{"role": "auditor"}, ifMatch},
		{"remover membro", http.MethodDelete, "/api/v1/members/" + tnA.Owner.ID.String(), nil, ifMatch},
		{"cancelar convite", http.MethodDelete, "/api/v1/invitations/" + invitationID, nil, nil},
		{"rotacionar credencial", http.MethodPost, "/api/v1/api-credentials/" + credA + "/rotate", nil, ifMatch},
		{"revogar credencial", http.MethodDelete, "/api/v1/api-credentials/" + credA, nil, ifMatch},
		{"alterar endpoint", http.MethodPatch, "/api/v1/webhook-endpoints/" + endpointID,
			map[string]any{"url": "https://hooks.cliente.test/b", "event_types": []string{eventType}, "status": "active"}, ifMatch},
		{"rotacionar segredo do endpoint", http.MethodPost, "/api/v1/webhook-endpoints/" + endpointID + "/rotate-secret", map[string]any{}, ifMatch},
		{"entregas do endpoint", http.MethodGet, "/api/v1/webhook-endpoints/" + endpointID + "/deliveries", nil, nil},
		{"remover endpoint", http.MethodDelete, "/api/v1/webhook-endpoints/" + endpointID, nil, ifMatch},
		{"alterar régua", http.MethodPatch, "/api/v1/collection-policies/" + policyA["id"].(string), policyBody("Régua do B"), ifMatch},
	} {
		if st, body, _ := ownerB.do(tc.method, tc.path, tc.body, tc.extra...); st != http.StatusNotFound {
			t.Errorf("%s (%s %s): status %d, esperado 404: %v", tc.name, tc.method, tc.path, st, body)
		}
	}

	// Referências no corpo: o recurso do A não é encontrado no tenant B.
	for _, tc := range []crossCase{
		{"assinar com cliente do A", http.MethodPost, "/api/v1/subscriptions", map[string]any{"customer_id": custA.ID, "plan_id": planB.ID}, idem()},
		{"assinar o plano do A", http.MethodPost, "/api/v1/subscriptions", map[string]any{"customer_id": custB.ID, "plan_id": planA.ID}, idem()},
		{"trocar para o plano do A", http.MethodPost, "/api/v1/subscriptions/" + subB.ID.String() + "/change-plan",
			map[string]any{"plan_id": planA.ID, "timing": "next_cycle"}, []string{"If-Match", subscriptionETag(ownerB, subB.ID)}},
	} {
		if st, body, _ := ownerB.do(tc.method, tc.path, tc.body, tc.extra...); st != http.StatusNotFound && st != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, esperado 404 ou 422: %v", tc.name, st, body)
		}
	}
	// Consumo com a assinatura do A (métrica de mesmo código nos dois
	// tenants): o evento é recusado; o mesmo evento com a assinatura do B é
	// aceito, o que mostra que a recusa vem da assinatura.
	usagePlanA, metric := usagePlan(t, a.env, tnA, "10")
	usagePlanB, _ := usagePlan(t, a.env, tnB, "10")
	start := tenantToday(t, a.env)
	usageSubA := newSubscription(t, a.env, tnA, usagePlanA.ID, 1, &start)
	usageSubB := newSubscription(t, a.env, tnB, usagePlanB.ID, 1, &start)
	at := a.env.Clock.Now().Truncate(time.Microsecond)
	got := ingest(ownerB, usageEvent("cruzado-a", usageSubA.ID, metric.Code, "1", at), usageEvent("cruzado-b", usageSubB.ID, metric.Code, "1", at))
	if got["cruzado-a"]["outcome"] == "accepted" || got["cruzado-b"]["outcome"] != "accepted" {
		t.Fatalf("consumo com a assinatura de outro tenant: %v", got)
	}

	// Cursor de uma listagem do A usado no B: só itens do B (ou recusa).
	pageA, _ := ownerA.expect(http.StatusOK, http.MethodGet, "/api/v1/customers?limit=1", nil)
	if next, _ := pageA["next_cursor"].(string); next != "" {
		st, pageB, _ := ownerB.do(http.MethodGet, "/api/v1/customers?limit=50&cursor="+next, nil)
		if st == http.StatusOK {
			allA, _ := ownerA.expect(http.StatusOK, http.MethodGet, "/api/v1/customers?limit=100", nil)
			ofA := map[any]bool{}
			for _, c := range objects(t, allA["data"]) {
				ofA[c["id"]] = true
			}
			for _, c := range objects(t, pageB["data"]) {
				if ofA[c["id"]] {
					t.Fatalf("cursor do tenant A trouxe cliente do tenant A no tenant B: %v", c)
				}
			}
		} else if st != http.StatusUnprocessableEntity {
			t.Fatalf("cursor do tenant A no tenant B: %d %v", st, pageB)
		}
	} else {
		t.Fatal("a listagem do tenant A deveria paginar")
	}

	// Credencial de API do B: não alcança o A pelo identificador nem pelo
	// cabeçalho de tenant.
	_, _, secretB := createCredential(t, ownerB, "customers.read", "customers.write")
	bearerB := &client{a: a}
	if st, body, _ := bearerB.do(http.MethodGet, cust, nil, "Authorization", "Bearer "+secretB); st != http.StatusNotFound {
		t.Fatalf("credencial do B leu cliente do A: %d %v", st, body)
	}
	if st, body, _ := bearerB.do(http.MethodGet, "/api/v1/customers", nil, "Authorization", "Bearer "+secretB,
		"X-Recorvia-Tenant", tnA.ID.String()); st != http.StatusConflict || body["code"] != "tenant_mismatch" {
		t.Fatalf("credencial do B com o tenant do A no cabeçalho: %d %v", st, body)
	}

	// Nada do tenant A mudou.
	if got, _ := ownerA.expect(http.StatusOK, http.MethodGet, cust, nil); got["name"] != custA.Name || got["status"] != "active" {
		t.Fatalf("cliente do A alterado: %v", got)
	}
	if got, _ := ownerA.expect(http.StatusOK, http.MethodGet, sub, nil); got["status"] != string(subA.Status) {
		t.Fatalf("assinatura do A alterada: %v", got)
	}
	if got, _ := ownerA.expect(http.StatusOK, http.MethodGet, inv, nil); got["status"] != "open" || got["amount_due"] != float64(5000) {
		t.Fatalf("fatura do A alterada: %v", got)
	}
	links, _ := listPortalLinks(t, ownerA, custA.ID, "")
	if len(links) != 1 || links[0]["status"] != "pending" || links[0]["revoked_at"] != nil {
		t.Fatalf("links do portal do A alterados: %v", links)
	}
	if got, _ := ownerA.expect(http.StatusOK, http.MethodGet, "/api/v1/members", nil); !strings.Contains(stringify(got), tnA.Owner.ID.String()) {
		t.Fatalf("dono do A removido: %v", got)
	}
	creds, _ := ownerA.expect(http.StatusOK, http.MethodGet, "/api/v1/api-credentials", nil)
	for _, c := range objects(t, creds["data"]) {
		if c["id"] == credA && (c["revoked_at"] != nil || c["version"] != float64(1)) {
			t.Fatalf("credencial do A alterada: %v", c)
		}
	}
	hooks, _ := ownerA.expect(http.StatusOK, http.MethodGet, "/api/v1/webhook-endpoints", nil)
	if eps := objects(t, hooks["data"]); len(eps) != 1 || eps[0]["status"] != "active" || eps[0]["url"] != "https://hooks.cliente.test/a" {
		t.Fatalf("endpoint do A alterado: %v", eps)
	}
	if got, _ := ownerA.expect(http.StatusOK, http.MethodGet, "/api/v1/invitations", nil); !strings.Contains(stringify(got), invitationID) {
		t.Fatalf("convite do A cancelado: %v", got)
	}
	if got, _ := ownerA.expect(http.StatusOK, http.MethodGet, "/api/v1/collection-policies", nil); !strings.Contains(stringify(got), "Régua do A") {
		t.Fatalf("régua do A alterada: %v", got)
	}
}

// stringify serializa a resposta para procurar identificadores.
func stringify(v any) string {
	raw, _ := json.Marshal(v) // respostas já decodificadas de JSON
	return string(raw)
}

// Acesso cruzado aos recebíveis: cobrança, reembolso, disputa, baixa do valor
// a recuperar e reexecução de operação do tenant A respondem como
// inexistentes para o tenant B, sem efeito no A.
func TestCrossTenantReceivablesOverHTTP(t *testing.T) {
	r := newReceivablesAPI(t)
	tnA, tnB := r.newTenant(t, "recebiveis-a"), r.newTenant(t, "recebiveis-b")
	ownerA, ownerB := r.login(tnA.Owner, &tnA.ID), r.login(tnB.Owner, &tnB.ID)
	charge := r.paidCharge(t, tnA, 10000)
	r.simAction(t, charge.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: "requested"})
	r.deliver(t, tnA)
	disputes, _ := ownerA.expect(http.StatusOK, http.MethodGet, "/api/v1/disputes", nil)
	ds := objects(t, disputes["data"])
	if len(ds) != 1 {
		t.Fatalf("disputa do A: %v", ds)
	}
	disputeID := ds[0]["id"].(string)
	ops, _ := ownerA.expect(http.StatusOK, http.MethodGet, "/api/v1/payment-operations", nil)
	opList := objects(t, ops["data"])
	if len(opList) == 0 {
		t.Fatal("operações do A")
	}

	chargePath := "/api/v1/charges/" + charge.ID.String()
	for _, tc := range []crossCase{
		{"cobrança", http.MethodGet, chargePath, nil, nil},
		{"reembolso", http.MethodPost, chargePath + "/refunds", map[string]any{"amount": 100, "reason": "Pelo B"}, []string{"Idempotency-Key", idemKey()}},
		{"disputa", http.MethodGet, "/api/v1/disputes/" + disputeID, nil, nil},
		{"baixas da disputa", http.MethodGet, "/api/v1/disputes/" + disputeID + "/recovery-settlements", nil, nil},
		{"baixar valor a recuperar", http.MethodPost, "/api/v1/disputes/" + disputeID + "/recovery-settlements",
			map[string]any{"outcome": "written_off", "amount": 100, "justification": "Tentativa de baixa pelo tenant B"}, []string{"Idempotency-Key", idemKey()}},
		{"reexecutar operação", http.MethodPost, "/api/v1/payment-operations/" + opList[0]["id"].(string) + "/retry", nil, nil},
	} {
		if st, body, _ := ownerB.do(tc.method, tc.path, tc.body, tc.extra...); st != http.StatusNotFound {
			t.Errorf("%s (%s %s): status %d, esperado 404: %v", tc.name, tc.method, tc.path, st, body)
		}
	}
	if got, _ := ownerA.expect(http.StatusOK, http.MethodGet, chargePath, nil); got["status"] != "disputed" || got["refundable_amount"] != float64(10000) {
		t.Fatalf("cobrança do A alterada: %v", got)
	}
	if n := r.count(t, tnA, `SELECT count(*) FROM dispute_recovery_settlements WHERE tenant_id = $1`); n != 0 {
		t.Fatalf("baixa gravada no A: %d", n)
	}
}
