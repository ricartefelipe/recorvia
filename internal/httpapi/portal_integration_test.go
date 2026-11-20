//go:build integration

package httpapi_test

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/api"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// contractSchemas carrega os schemas do contrato uma única vez.
var contractSchemas = sync.OnceValues(func() (openapi3.Schemas, error) {
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	if err != nil {
		return nil, err
	}
	return doc.Components.Schemas, nil
})

// assertSchema valida uma resposta JSON contra um schema do contrato.
func assertSchema(t *testing.T, name string, v any) {
	t.Helper()
	schemas, err := contractSchemas()
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := schemas[name]
	if !ok || ref.Value == nil {
		t.Fatalf("schema %s ausente do contrato", name)
	}
	if err := ref.Value.VisitJSON(v); err != nil {
		t.Fatalf("resposta fora do schema %s: %v\n%v", name, err, v)
	}
}

// issueInvoice assina um plano de valor fixo para o cliente e executa o
// faturamento, devolvendo a fatura emitida.
func issueInvoice(t *testing.T, a *apiEnv, tn fixture.Tenant, customerID uuid.UUID, amount int64) billing.Invoice {
	t.Helper()
	ctx := context.Background()
	plan := newPlan(t, a.env, tn, "Plano Profissional", flatPrice(amount))
	sub := a.env.NewSubscription(t, tn, customerID, plan.ID, nil)
	if _, err := a.env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	pg, err := a.env.Billing.List(ctx, tn.ID, billing.ListFilter{SubscriptionID: &sub.ID, Limit: 10})
	if err != nil || len(pg.Items) != 1 || pg.Items[0].Status != billing.Open {
		t.Fatalf("fatura não emitida: %v %+v", err, pg.Items)
	}
	return pg.Items[0]
}

// addCheckout registra, sem chamar o provedor, uma cobrança aguardando
// pagamento com o link do pagamento hospedado.
func addCheckout(t *testing.T, a *apiEnv, tn fixture.Tenant, inv billing.Invoice, checkoutURL string) {
	t.Helper()
	ctx := context.Background()
	err := a.env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		account := ids.New()
		hash := sha256.Sum256([]byte(account.String()))
		if _, err := tx.Exec(ctx, `INSERT INTO payment_provider_accounts (id, tenant_id, provider, environment, webhook_endpoint_token_hash)
			VALUES ($1, $2, 'simulator', 'local', $3)`, account, tn.ID, hash[:]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO charges (id, tenant_id, invoice_id, customer_id, provider_account_id, provider, status, amount,
			checkout_url, due_date) VALUES ($1, $2, $3, $4, $5, 'simulator', 'awaiting_payment', $6, $7, $8::timestamptz::date)`,
			ids.New(), tn.ID, inv.ID, inv.CustomerID, account, inv.AmountDue.Amount(), checkoutURL, *inv.DueAt)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// addDraftInvoice grava um rascunho de fatura do cliente.
func addDraftInvoice(t *testing.T, a *apiEnv, tn fixture.Tenant, customerID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := ids.New()
	err := a.env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO invoices (id, tenant_id, customer_id, status, kind, subtotal, total, amount_due, customer_snapshot)
			VALUES ($1, $2, $3, 'draft', 'manual', 500, 500, 500, '{}')`, id, tn.ID, customerID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func addContact(t *testing.T, a *apiEnv, tn fixture.Tenant, customerID uuid.UUID, name string, billingEmail bool) customers.Contact {
	t.Helper()
	c, err := a.env.Customers.AddContact(context.Background(), tn.ID, tn.Actor, customerID, customers.ContactInput{Name: name,
		Email: strings.ToLower(strings.ReplaceAll(name, " ", ".")) + "@cliente.test", ReceivesBillingEmail: billingEmail})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func portalIDs(t *testing.T, r rawResponse) map[string]map[string]any {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("listagem do portal: %d %v", r.status, r.body)
	}
	items := objects(t, r.body["data"])
	for _, it := range items {
		assertSchema(t, "PortalInvoice", it)
	}
	return byField(t, items, "id")
}

// Fluxo completo do portal: link, acesso, sessão, faturas somente do cliente,
// documento, preferências de contato e saída.
func TestPortalCustomerFlowOverHTTP(t *testing.T) {
	a := newAPI(t)
	tnA, tnB := a.env.NewTenant(t, "portal-fluxo-a"), a.env.NewTenant(t, "portal-fluxo-b")
	cust, other := a.env.NewCustomer(t, tnA), a.env.NewCustomer(t, tnA)
	fin := addContact(t, a, tnA, cust.ID, "Financeiro Cliente", true)
	ops := addContact(t, a, tnA, cust.ID, "Operacoes Cliente", false)
	foreignContact := addContact(t, a, tnA, other.ID, "Financeiro Outro", true)
	own := issueInvoice(t, a, tnA, cust.ID, 15990)
	second := issueInvoice(t, a, tnA, cust.ID, 4990)
	draft := addDraftInvoice(t, a, tnA, cust.ID)
	foreign := issueInvoice(t, a, tnA, other.ID, 7700)
	custB := a.env.NewCustomer(t, tnB)
	foreignTenant := issueInvoice(t, a, tnB, custB.ID, 9900)
	const checkout = "https://pagamentos.exemplo.test/checkout/abc"
	addCheckout(t, a, tnA, own, checkout)

	operator := a.login(member(t, a.env, tnA, tenancy.RoleOperator), &tnA.ID)
	cookie, csrf := openPortal(t, a, createPortalLink(t, operator, cust.ID))
	session := a.raw(http.MethodGet, "/api/portal/v1/session", nil, nil, cookie)
	assertSchema(t, "PortalSession", session.body)

	// Listagem: somente as faturas emitidas do cliente do link.
	listed := portalIDs(t, a.raw(http.MethodGet, "/api/portal/v1/invoices", nil, nil, cookie))
	if len(listed) != 2 || listed[own.ID.String()] == nil || listed[second.ID.String()] == nil {
		t.Fatalf("faturas fora do escopo do cliente: %v", listed)
	}
	got := listed[own.ID.String()]
	wantDue := billing.DueDate(*own.DueAt, mustLocation(t)).String()
	items := objects(t, got["items"])
	if got["number"] != own.Number || got["status"] != "open" || got["total"] != float64(15990) || got["amount_due"] != float64(15990) ||
		got["checkout_url"] != checkout || got["due_date"] != wantDue || got["overdue"] != false || len(items) != 1 || items[0]["amount"] != float64(15990) {
		t.Fatalf("fatura no portal: %v", got)
	}
	if listed[second.ID.String()]["checkout_url"] != nil {
		t.Fatalf("fatura sem cobrança não tem link de pagamento: %v", listed[second.ID.String()])
	}
	for _, leaked := range []string{"customer_id", "customer_name", "history", "charges", "version"} {
		if _, ok := got[leaked]; ok {
			t.Fatalf("o portal não expõe %s: %v", leaked, got)
		}
	}

	// Detalhe e documento: fatura do cliente; as demais respondem 404.
	detail := a.raw(http.MethodGet, "/api/portal/v1/invoices/"+own.ID.String(), nil, nil, cookie)
	if detail.status != http.StatusOK || detail.body["id"] != own.ID.String() || detail.body["checkout_url"] != checkout {
		t.Fatalf("detalhe: %d %v", detail.status, detail.body)
	}
	assertSchema(t, "PortalInvoice", detail.body)
	doc := a.raw(http.MethodGet, "/api/portal/v1/invoices/"+own.ID.String()+"/document", nil, nil, cookie)
	if doc.status != http.StatusOK || !strings.HasPrefix(doc.header.Get("Content-Type"), "text/html") ||
		!strings.Contains(doc.header.Get("Content-Security-Policy"), "default-src 'none'") || !strings.Contains(doc.text, own.Number) ||
		!strings.Contains(doc.text, "Org portal-fluxo-a") || !strings.Contains(doc.text, "Pagar esta fatura") || strings.Contains(doc.text, other.Name) {
		t.Fatalf("documento: %d %v %s", doc.status, doc.header, doc.text)
	}
	for name, id := range map[string]string{"outro cliente": foreign.ID.String(), "outro tenant": foreignTenant.ID.String(), "rascunho": draft.String(),
		"inexistente": uuid.NewString()} {
		for _, suffix := range []string{"", "/document"} {
			if r := a.raw(http.MethodGet, "/api/portal/v1/invoices/"+id+suffix, nil, nil, cookie); r.status != http.StatusNotFound ||
				r.body["code"] != "invoice_not_found" {
				t.Fatalf("%s%s: %d %v", name, suffix, r.status, r.body)
			}
		}
	}
	if r := a.raw(http.MethodGet, "/api/portal/v1/invoices/nao-e-uuid", nil, nil, cookie); r.status != http.StatusNotFound {
		t.Fatalf("identificador malformado: %d", r.status)
	}
	// O documento do painel continua disponível para a organização.
	owner := a.login(tnA.Owner, &tnA.ID)
	if st, _, h := owner.do(http.MethodGet, "/api/v1/invoices/"+own.ID.String()+"/document", nil); st != http.StatusOK ||
		!strings.HasPrefix(h.Get("Content-Type"), "text/html") {
		t.Fatalf("documento no painel: %d", st)
	}

	// Preferências de contato: exigem CSRF e alcançam apenas contatos do cliente.
	prefs := map[string]any{"contacts": []map[string]any{{"id": fin.ID, "receives_billing_email": false}, {"id": ops.ID, "receives_billing_email": true}}}
	if r := a.raw(http.MethodPut, "/api/portal/v1/contact-preferences", prefs, nil, cookie); r.status != http.StatusForbidden || r.body["code"] != "csrf_invalid" {
		t.Fatalf("preferências sem CSRF: %d %v", r.status, r.body)
	}
	withCSRF := map[string]string{"X-CSRF-Token": csrf}
	r := a.raw(http.MethodPut, "/api/portal/v1/contact-preferences", prefs, withCSRF, cookie)
	if r.status != http.StatusOK {
		t.Fatalf("preferências: %d %v", r.status, r.body)
	}
	assertSchema(t, "PortalSession", r.body)
	contacts := byField(t, objects(t, r.body["contacts"]), "id")
	if contacts[fin.ID.String()]["receives_billing_email"] != false || contacts[ops.ID.String()]["receives_billing_email"] != true ||
		r.body["csrf_token"] != csrf {
		t.Fatalf("preferências gravadas: %v", r.body)
	}
	foreignPrefs := map[string]any{"contacts": []map[string]any{{"id": fin.ID, "receives_billing_email": true},
		{"id": foreignContact.ID, "receives_billing_email": false}}}
	if r := a.raw(http.MethodPut, "/api/portal/v1/contact-preferences", foreignPrefs, withCSRF, cookie); r.status != http.StatusNotFound ||
		r.body["code"] != "contact_not_found" {
		t.Fatalf("contato de outro cliente: %d %v", r.status, r.body)
	}
	for name, body := range map[string]any{
		"sem lista":          map[string]any{},
		"sem preferência":    map[string]any{"contacts": []map[string]any{{"id": fin.ID}}},
		"contato repetido":   map[string]any{"contacts": []map[string]any{{"id": fin.ID, "receives_billing_email": true}, {"id": fin.ID, "receives_billing_email": false}}},
		"campo desconhecido": map[string]any{"contacts": []map[string]any{}, "email": "outro@cliente.test"},
	} {
		if r := a.raw(http.MethodPut, "/api/portal/v1/contact-preferences", body, withCSRF, cookie); r.status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %v", name, r.status, r.body)
		}
	}
	// O painel enxerga a alteração, sem efeito parcial da tentativa recusada,
	// e a auditoria registra o ator do portal.
	_, details, _ := owner.do(http.MethodGet, "/api/v1/customers/"+cust.ID.String(), nil)
	panel := byField(t, objects(t, details["contacts"]), "id")
	if panel[fin.ID.String()]["receives_billing_email"] != false || panel[ops.ID.String()]["receives_billing_email"] != true {
		t.Fatalf("contatos no painel: %v", details["contacts"])
	}
	_, otherDetails, _ := owner.do(http.MethodGet, "/api/v1/customers/"+other.ID.String(), nil)
	if objects(t, otherDetails["contacts"])[0]["receives_billing_email"] != true {
		t.Fatalf("contato de outro cliente alterado: %v", otherDetails["contacts"])
	}
	st, events, _ := owner.do(http.MethodGet, "/api/v1/audit-events?action=customer.contact_preferences_updated&resource_id="+cust.ID.String(), nil)
	ev := objects(t, events["data"])
	if st != http.StatusOK || len(ev) != 1 || ev[0]["actor_type"] != "customer_portal" || ev[0]["actor_id"] != cust.ID.String() {
		t.Fatalf("auditoria das preferências: %d %v", st, events)
	}

	// A sessão de um cliente de outro tenant não alcança as faturas deste.
	ownerB := a.login(tnB.Owner, &tnB.ID)
	cookieB, _ := openPortal(t, a, createPortalLink(t, ownerB, custB.ID))
	listedB := portalIDs(t, a.raw(http.MethodGet, "/api/portal/v1/invoices", nil, nil, cookieB))
	if len(listedB) != 1 || listedB[foreignTenant.ID.String()] == nil {
		t.Fatalf("faturas do cliente do outro tenant: %v", listedB)
	}
	if r := a.raw(http.MethodGet, "/api/portal/v1/invoices/"+own.ID.String(), nil, nil, cookieB); r.status != http.StatusNotFound {
		t.Fatalf("fatura de outro tenant pela sessão do portal: %d %v", r.status, r.body)
	}

	// Saída: a sessão deixa de valer para todas as rotas do portal.
	if r := a.raw(http.MethodPost, "/api/portal/v1/logout", nil, withCSRF, cookie); r.status != http.StatusNoContent {
		t.Fatalf("saída: %d %v", r.status, r.body)
	}
	for _, path := range []string{"/api/portal/v1/invoices", "/api/portal/v1/invoices/" + own.ID.String(), "/api/portal/v1/invoices/" + own.ID.String() + "/document"} {
		if r := a.raw(http.MethodGet, path, nil, nil, cookie); r.status != http.StatusUnauthorized || r.body["code"] != "portal_session_invalid" {
			t.Fatalf("%s após a saída: %d %v", path, r.status, r.body)
		}
	}
	if r := a.raw(http.MethodPut, "/api/portal/v1/contact-preferences", prefs, withCSRF, cookie); r.status != http.StatusUnauthorized {
		t.Fatalf("preferências após a saída: %d %v", r.status, r.body)
	}
}

// Arquivar o cliente encerra o acesso ao portal na requisição seguinte.
func TestPortalAccessEndsWhenCustomerIsArchived(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "portal-arquivado")
	cust := a.env.NewCustomer(t, tn)
	owner := a.login(tn.Owner, &tn.ID)
	cookie, _ := openPortal(t, a, createPortalLink(t, owner, cust.ID))
	if st, body, _ := owner.do(http.MethodPost, "/api/v1/customers/"+cust.ID.String()+"/archive", nil); st != http.StatusOK {
		t.Fatalf("arquivamento: %d %v", st, body)
	}
	if r := a.raw(http.MethodGet, "/api/portal/v1/invoices", nil, nil, cookie); r.status != http.StatusUnauthorized || r.cookie["recorvia_portal"] == nil {
		t.Fatalf("portal de cliente arquivado: %d %v", r.status, r.body)
	}
	if st, body, _ := owner.do(http.MethodPost, "/api/v1/customers/"+cust.ID.String()+"/portal-links", nil); st != http.StatusConflict ||
		body["code"] != "customer_archived" {
		t.Fatalf("link para cliente arquivado: %d %v", st, body)
	}
}

func mustLocation(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(tenantTimezone)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// Credencial com usage.write ingere consumo sem cookie nem CSRF; escopo
// insuficiente, tenant divergente, rotação e revogação seguem as regras da
// autenticação Bearer.
func TestAPICredentialIngestsUsageWithoutCSRF(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "bearer-consumo")
	enableUsageBilling(t, a.env, tn)
	other := apiPlanTenant(t, a, "bearer-consumo-b")
	owner := a.login(tn.Owner, &tn.ID)
	plan, metric := usagePlan(t, a.env, tn, "15")
	start := tenantToday(t, a.env)
	sub := newSubscription(t, a.env, tn, plan.ID, 1, &start)
	at := a.env.Clock.Now().Truncate(time.Microsecond)
	batch := func(id string) map[string]any {
		return map[string]any{"events": []map[string]any{usageEvent(id, sub.ID, metric.Code, "3", at)}}
	}
	outcome := func(r rawResponse) string {
		t.Helper()
		results := objects(t, r.body["results"])
		if len(results) != 1 {
			t.Fatalf("resultados: %v", r.body)
		}
		s, _ := results[0]["outcome"].(string)
		return s
	}
	writerID, _, writer := createCredential(t, owner, "usage.write")
	_, _, reader := createCredential(t, owner, "usage.read")

	r := a.raw(http.MethodPost, "/api/v1/usage-events", batch("erp-1"), bearer(writer))
	if r.status != http.StatusOK || outcome(r) != "accepted" {
		t.Fatalf("ingestão por credencial: %d %v", r.status, r.body)
	}
	if r := a.raw(http.MethodPost, "/api/v1/usage-events", batch("erp-1"), bearer(writer)); r.status != http.StatusOK || outcome(r) != "duplicate" {
		t.Fatalf("reenvio do mesmo evento: %d %v", r.status, r.body)
	}
	// A sessão do navegador, ao contrário, continua exigindo o token CSRF.
	session := &http.Cookie{Name: "recorvia_session", Value: owner.cookie}
	if r := a.raw(http.MethodPost, "/api/v1/usage-events", batch("erp-2"), map[string]string{"X-Recorvia-Tenant": tn.ID.String()}, session); r.status != http.StatusForbidden ||
		r.body["code"] != "csrf_invalid" {
		t.Fatalf("sessão sem CSRF: %d %v", r.status, r.body)
	}
	// Escopo de leitura não escreve, mas lista o que foi ingerido.
	if r := a.raw(http.MethodPost, "/api/v1/usage-events", batch("erp-3"), bearer(reader)); r.status != http.StatusForbidden || r.body["code"] != "forbidden" {
		t.Fatalf("escopo insuficiente: %d %v", r.status, r.body)
	}
	// Tenant divergente do da credencial.
	if r := a.raw(http.MethodPost, "/api/v1/usage-events", batch("erp-4"), bearer(writer, "X-Recorvia-Tenant", other.ID.String())); r.status != http.StatusConflict ||
		r.body["code"] != "tenant_mismatch" {
		t.Fatalf("tenant divergente: %d %v", r.status, r.body)
	}
	// Rotação: o segredo novo e o anterior (dentro da janela) ingerem.
	st, rot, _ := owner.do(http.MethodPost, "/api/v1/api-credentials/"+writerID+"/rotate", nil)
	if st != http.StatusCreated {
		t.Fatalf("rotação: %d %v", st, rot)
	}
	assertSchema(t, "ApiCredentialWithSecret", rot)
	rotated := rot["secret"].(string)
	for i, token := range []string{rotated, writer} {
		if r := a.raw(http.MethodPost, "/api/v1/usage-events", batch("erp-rotacao-"+strconv.Itoa(i)), bearer(token)); r.status != http.StatusOK || outcome(r) != "accepted" {
			t.Fatalf("ingestão após a rotação (%d): %d %v", i, r.status, r.body)
		}
	}
	// Revogação: os dois segredos deixam de valer na hora.
	if st, body, _ := owner.do(http.MethodDelete, "/api/v1/api-credentials/"+writerID, nil); st != http.StatusNoContent {
		t.Fatalf("revogação: %d %v", st, body)
	}
	for _, token := range []string{rotated, writer} {
		if r := a.raw(http.MethodPost, "/api/v1/usage-events", batch("erp-5"), bearer(token)); r.status != http.StatusUnauthorized ||
			r.body["code"] != "api_credential_invalid" || !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Bearer") {
			t.Fatalf("credencial revogada: %d %v", r.status, r.body)
		}
	}
	list := a.raw(http.MethodGet, "/api/v1/usage-events?subscription_id="+sub.ID.String(), nil, bearer(reader))
	if list.status != http.StatusOK || len(objects(t, list.body["data"])) != 3 {
		t.Fatalf("eventos ingeridos: %d %v", list.status, list.body)
	}
	// A listagem de credenciais segue o contrato e mostra a revogação.
	_, creds, _ := owner.do(http.MethodGet, "/api/v1/api-credentials", nil)
	for _, c := range objects(t, creds["data"]) {
		assertSchema(t, "ApiCredential", c)
		if c["id"] == writerID && c["revoked_at"] == nil {
			t.Fatalf("credencial revogada sem revoked_at: %v", c)
		}
	}
}
