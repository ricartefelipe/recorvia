//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/httpapi"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/safehttp"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways"
	"github.com/ricartefelipe/recorvia/internal/reporting"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/contract"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

// staticHosts resolve os nomes dos testes para endereços fixos, sem DNS real.
type staticHosts map[string][]netip.Addr

func (s staticHosts) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := s[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// statusTransport responde a toda entrega com o status configurado (o
// destino real não é contatado nos testes da API).
type statusTransport struct{ status int }

func (s statusTransport) Post(context.Context, webhooks.Request) (int, error) { return s.status, nil }

// dunningAPI expõe os serviços de cobrança, notificações e webhooks usados
// pela API, para preparar cenários sem passar pelo worker.
type dunningAPI struct {
	*apiEnv
	notif    *notifications.Service
	coll     *collections.Service
	webhooks *webhooks.Service
}

// newDunningAPI monta a API com políticas de cobrança, templates,
// notificações, webhooks enviados e credenciais de API. O validador de
// destino resolve hooks.cliente.test para um endereço público e
// interno.cliente.test para uma rede privada.
func newDunningAPI(t *testing.T) *dunningAPI {
	t.Helper()
	a := newAPI(t)
	env := a.env
	hosts := staticHosts{"hooks.cliente.test": {netip.MustParseAddr("93.184.215.14")}, "interno.cliente.test": {netip.MustParseAddr("10.20.30.40")}}
	client := safehttp.New(safehttp.Options{Resolver: hosts})
	d := &dunningAPI{apiEnv: a,
		notif: notifications.NewService(env.Store.Notifications(), nil, env.Keyring, env.Clock, notifications.Config{}),
		coll:  collections.NewService(env.Store.Collections(), env.Clock, nil),
		webhooks: webhooks.NewService(env.Store.Webhooks(), statusTransport{status: http.StatusServiceUnavailable}, client, env.Clock,
			webhooks.Config{Jitter: func() float64 { return 0.5 }}),
	}
	idsvc := identity.NewService(stubIDP{}, a.sessions, env.Store.Users(), env.Clock, identity.DefaultPolicy())
	cfg := &config.Config{Env: config.Test, CookieName: "recorvia_session", CookieSecure: false, RateLimitPerMinute: 10000,
		APICredentialRateLimitPerMinute: 10000}
	pay := env.Payments(gateways.NewResolver(time.Second), payments.Config{WebhookBaseURL: "http://api.test/webhooks/providers"})
	credentials := apicredentials.NewService(env.Store.APICredentials(), a.sessions, env.Clock, apicredentials.DefaultPolicy())
	_, router := httpapi.NewRouter(httpapi.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Config: cfg, Clock: env.Clock,
		Store: env.Store, Sessions: a.sessions, Identity: idsvc, Tenancy: env.Tenancy, Platform: env.Platform, Customers: env.Customers,
		Catalog: env.Catalog, Subscriptions: env.Subscriptions, Billing: env.Billing, Payments: pay,
		Reporting: reporting.NewService(env.Store.Reporting(), env.Clock), Notifications: d.notif, Collections: d.coll, Webhooks: d.webhooks,
		Credentials: credentials})
	srv := httptest.NewServer(contract.Wrap(t, router))
	t.Cleanup(srv.Close)
	a.srv = srv
	return d
}

// billedInvoice cria cliente com contato de cobrança, assinatura e a fatura
// do período corrente; devolve o ID da fatura.
func billedInvoice(t *testing.T, env *fixture.Env, tn fixture.Tenant, planID uuid.UUID, contact string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	c := env.NewCustomer(t, tn)
	if _, err := env.Customers.AddContact(ctx, tn.ID, tn.Actor, c.ID, customers.ContactInput{Name: "Financeiro", Email: contact,
		ReceivesBillingEmail: true}); err != nil {
		t.Fatal(err)
	}
	env.NewSubscription(t, tn, c.ID, planID, nil)
	if _, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM invoices WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, c.ID).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// auditActions conta as ações auditadas do tenant pelo endpoint da API.
func auditActions(c *client, resourceType string) map[string]int {
	c.a.t.Helper()
	out, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/audit-events?limit=100&resource_type="+resourceType, nil)
	actions := map[string]int{}
	for _, e := range objects(c.a.t, out["data"]) {
		actions[e["action"].(string)]++
	}
	return actions
}

func policyBody(name string, extra ...any) map[string]any {
	b := map[string]any{"name": name, "reminder_offsets_days": []int{7, -3, 0, 3}, "send_window_start_hour": 8, "send_window_end_hour": 20,
		"max_messages_per_day": 2}
	for i := 0; i+1 < len(extra); i += 2 {
		b[extra[i].(string)] = extra[i+1]
	}
	return b
}

// Políticas de cobrança: criação (a primeira é a padrão), validação,
// concorrência otimista, alteração parcial que preserva os opcionais
// omitidos, troca da padrão, permissões, auditoria e isolamento.
func TestCollectionsAPIPolicies(t *testing.T) {
	a := newDunningAPI(t)
	tn := proTenant(t, a.env, "politicas")
	owner := a.login(tn.Owner, &tn.ID)

	created, h := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/collection-policies",
		policyBody("Régua padrão", "suspend_after_days", 15, "reactivate_on_payment", true))
	if created["is_default"] != true || h.Get("ETag") != `"1"` || created["version"] != float64(1) || created["suspend_after_days"] != float64(15) ||
		created["reactivate_on_payment"] != true || created["send_window_start_hour"] != float64(8) || created["id"] == nil || created["created_at"] == nil {
		t.Fatalf("política criada: %v %s", created, h.Get("ETag"))
	}
	if got := jsonArray(t, created["reminder_offsets_days"]); len(got) != 4 || got[0] != float64(-3) || got[3] != float64(7) {
		t.Fatalf("passos ordenados: %v", got)
	}
	id := created["id"].(string)

	st, body, _ := owner.do(http.MethodPost, "/api/v1/collection-policies", map[string]any{"name": "Incompleta"})
	if st != 422 || body["code"] != "invalid_collection_policy" || len(objects(t, body["errors"])) != 4 {
		t.Fatalf("campos obrigatórios ausentes: %d %v", st, body)
	}
	owner.expectProblem(422, "invalid_collection_policy", http.MethodPost, "/api/v1/collection-policies",
		policyBody("Janela invertida", "send_window_start_hour", 20, "send_window_end_hour", 8))
	owner.expectProblem(422, "invalid_collection_policy", http.MethodPost, "/api/v1/collection-policies",
		policyBody("Passos repetidos", "reminder_offsets_days", []int{3, 3}))
	owner.expectProblem(422, "invalid_json", http.MethodPost, "/api/v1/collection-policies", policyBody("Campo extra", "channel", "sms"))
	owner.expectProblem(409, "collection_policy_name_taken", http.MethodPost, "/api/v1/collection-policies", policyBody("Régua padrão"))

	// Alteração: If-Match obrigatório e conferido.
	path := "/api/v1/collection-policies/" + id
	owner.expectProblem(428, "if_match_required", http.MethodPatch, path, policyBody("Régua alterada"))
	owner.expectProblem(412, "version_mismatch", http.MethodPatch, path, policyBody("Régua alterada"), "If-Match", `"7"`)
	// Opcionais omitidos mantêm os valores atuais (padrão, suspensão e reativação).
	upd, h := owner.expect(http.StatusOK, http.MethodPatch, path, policyBody("Régua alterada", "max_messages_per_day", 1), "If-Match", `"1"`)
	if upd["name"] != "Régua alterada" || upd["is_default"] != true || upd["suspend_after_days"] != float64(15) || upd["reactivate_on_payment"] != true ||
		upd["max_messages_per_day"] != float64(1) || h.Get("ETag") != `"2"` {
		t.Fatalf("alteração parcial: %v %s", upd, h.Get("ETag"))
	}
	// Nulo explícito remove a suspensão.
	upd, _ = owner.expect(http.StatusOK, http.MethodPatch, path, policyBody("Régua alterada", "suspend_after_days", nil, "is_default", true,
		"reactivate_on_payment", false), "If-Match", `"2"`)
	if upd["suspend_after_days"] != nil || upd["reactivate_on_payment"] != false || upd["version"] != float64(3) {
		t.Fatalf("remoção da suspensão: %v", upd)
	}
	owner.expectProblem(404, "collection_policy_not_found", http.MethodPatch, "/api/v1/collection-policies/"+uuid.NewString(),
		policyBody("Inexistente"), "If-Match", `"1"`)

	// Uma nova política padrão substitui a anterior.
	second, _ := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/collection-policies", policyBody("Régua curta", "is_default", true,
		"reminder_offsets_days", []int{}))
	if second["is_default"] != true || len(jsonArray(t, second["reminder_offsets_days"])) != 0 {
		t.Fatalf("segunda política: %v", second)
	}
	out, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/collection-policies", nil)
	byName := byField(t, objects(t, out["data"]), "name")
	if len(byName) != 2 || byName["Régua alterada"]["is_default"] != false || byName["Régua curta"]["is_default"] != true {
		t.Fatalf("política padrão única: %v", byName)
	}
	if acts := auditActions(owner, "collection_policy"); acts["collection_policy.created"] != 2 || acts["collection_policy.updated"] != 2 {
		t.Fatalf("auditoria das políticas: %v", acts)
	}

	// Permissões: operador e auditor leem; financeiro altera; operador não altera.
	operator := a.login(member(t, a.env, tn, tenancy.RoleOperator), &tn.ID)
	operator.expect(http.StatusOK, http.MethodGet, "/api/v1/collection-policies", nil)
	operator.expectProblem(403, "forbidden", http.MethodPost, "/api/v1/collection-policies", policyBody("Operador"))
	auditor := a.login(member(t, a.env, tn, tenancy.RoleAuditor), &tn.ID)
	auditor.expect(http.StatusOK, http.MethodGet, "/api/v1/collection-policies", nil)
	auditor.expectProblem(403, "forbidden", http.MethodPatch, path, policyBody("Auditor"), "If-Match", `"3"`)
	finance := a.login(member(t, a.env, tn, tenancy.RoleFinance), &tn.ID)
	finance.expect(http.StatusCreated, http.MethodPost, "/api/v1/collection-policies", policyBody("Régua do financeiro"))

	// Isolamento: outro tenant não lista nem altera a política.
	other := a.env.NewTenant(t, "politicas-b")
	ownerB := a.login(other.Owner, &other.ID)
	if out, _ := ownerB.expect(http.StatusOK, http.MethodGet, "/api/v1/collection-policies", nil); len(objects(t, out["data"])) != 0 {
		t.Fatalf("políticas de outro tenant visíveis: %v", out)
	}
	ownerB.expectProblem(404, "collection_policy_not_found", http.MethodPatch, path, policyBody("Invasão"), "If-Match", `"3"`)
}

// Templates: padrões do sistema com variáveis permitidas, personalização
// validada (variável desconhecida rejeitada), auditoria e chave inexistente.
func TestCollectionsAPITemplates(t *testing.T) {
	a := newDunningAPI(t)
	tn := a.env.NewTenant(t, "templates")
	owner := a.login(tn.Owner, &tn.ID)
	out, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/notification-templates", nil)
	list := byField(t, objects(t, out["data"]), "key")
	if len(list) != 5 || list["member_invitation"] != nil {
		t.Fatalf("templates personalizáveis: %v", list)
	}
	after := list["reminder_after_due"]
	if after["is_default"] != true || after["channel"] != "email" || after["updated_at"] != nil || !strings.Contains(after["body"].(string), "{{days_overdue}}") {
		t.Fatalf("template padrão: %v", after)
	}
	vars := map[string]bool{}
	for _, v := range jsonArray(t, after["variables"]) {
		vars[v.(string)] = true
	}
	if !vars["days_overdue"] || !vars["customer_name"] || vars["accept_url"] {
		t.Fatalf("variáveis do template: %v", vars)
	}

	path := "/api/v1/notification-templates/reminder_after_due"
	// O template padrão tem a versão 0: é a do If-Match da primeira personalização.
	if v := after["version"]; v != float64(0) {
		t.Fatalf("versão do template padrão: %v", v)
	}
	st, body, _ := owner.do(http.MethodPut, path, map[string]any{"subject": "Fatura {{invoice_number}} em atraso", "body": "Olá {{senha}}, pague."},
		matchVersion(0)...)
	if st != 422 || body["code"] != "invalid_template" || !strings.Contains(objects(t, body["errors"])[0]["message"].(string), "senha") {
		t.Fatalf("variável não permitida: %d %v", st, body)
	}
	owner.expectProblem(422, "invalid_template", http.MethodPut, path, map[string]any{"subject": "ab", "body": "Corpo suficiente."}, matchVersion(0)...)
	saved, h := owner.expect(http.StatusOK, http.MethodPut, path, map[string]any{"subject": "Fatura {{invoice_number}} em atraso",
		"body": "Olá, {{customer_name}}.\n\nA fatura {{invoice_number}} está vencida há {{days_overdue}} dia(s)."}, matchVersion(0)...)
	if saved["is_default"] != false || saved["updated_at"] == nil || saved["key"] != "reminder_after_due" || saved["subject"] != "Fatura {{invoice_number}} em atraso" ||
		saved["version"] != float64(1) || h.Get("ETag") != `"1"` {
		t.Fatalf("template gravado: %v (ETag %q)", saved, h.Get("ETag"))
	}
	// Quem ainda via o template padrão não sobrescreve a personalização.
	owner.expectProblem(412, "version_mismatch", http.MethodPut, path, map[string]any{"subject": "Outro assunto", "body": "Outro corpo suficiente."},
		matchVersion(0)...)
	out, _ = owner.expect(http.StatusOK, http.MethodGet, "/api/v1/notification-templates", nil)
	if byField(t, objects(t, out["data"]), "key")["reminder_after_due"]["is_default"] != false {
		t.Fatal("template personalizado não listado")
	}
	owner.expectProblem(404, "template_not_found", http.MethodPut, "/api/v1/notification-templates/member_invitation",
		map[string]any{"subject": "Convite", "body": "Corpo do convite."}, matchVersion(0)...)
	if acts := auditActions(owner, "notification_template"); acts["notification_template.updated"] != 1 {
		t.Fatalf("auditoria do template: %v", acts)
	}
	operator := a.login(member(t, a.env, tn, tenancy.RoleOperator), &tn.ID)
	operator.expectProblem(403, "forbidden", http.MethodPut, path, map[string]any{"subject": "Assunto", "body": "Corpo suficiente."})
	// Outro tenant continua com o padrão do sistema.
	other := a.env.NewTenant(t, "templates-b")
	out, _ = a.login(other.Owner, &other.ID).expect(http.StatusOK, http.MethodGet, "/api/v1/notification-templates", nil)
	if byField(t, objects(t, out["data"]), "key")["reminder_after_due"]["is_default"] != true {
		t.Fatal("personalização vazou para outro tenant")
	}
}

// Histórico de notificações: filtros por fatura e situação, paginação por
// cursor, filtro inválido e isolamento entre tenants.
func TestCollectionsAPINotificationHistory(t *testing.T) {
	a := newDunningAPI(t)
	ctx := context.Background()
	tn := a.env.NewTenant(t, "historico")
	owner := a.login(tn.Owner, &tn.ID)
	plan := a.env.NewFlatPlan(t, tn, 45000)
	first := billedInvoice(t, a.env, tn, plan.ID, "primeiro@cliente.test")
	second := billedInvoice(t, a.env, tn, plan.ID, "segundo@cliente.test")
	for _, inv := range []uuid.UUID{first, second} {
		if err := a.notif.HandleInvoiceIssued(ctx, tn.ID, inv, ids.New()); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/notifications?invoice_id="+first.String(), nil)
	items := objects(t, out["data"])
	if len(items) != 1 || out["next_cursor"] != nil {
		t.Fatalf("filtro por fatura: %v", out)
	}
	n := items[0]
	if n["recipient"] != "primeiro@cliente.test" || n["status"] != "scheduled" || n["template_key"] != "invoice_issued" || n["channel"] != "email" ||
		n["business_key"] != notifications.InvoiceIssuedKey(first) || n["invoice_id"] != first.String() || n["attempts"] != float64(0) ||
		n["sent_at"] != nil || !strings.Contains(n["subject"].(string), "emitida") || n["customer_id"] == nil {
		t.Fatalf("notificação: %v", n)
	}
	page1, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/notifications?limit=1&status=scheduled", nil)
	cursor, ok := page1["next_cursor"].(string)
	if len(objects(t, page1["data"])) != 1 || !ok {
		t.Fatalf("primeira página: %v", page1)
	}
	page2, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/notifications?limit=1&status=scheduled&cursor="+cursor, nil)
	if p2 := objects(t, page2["data"]); len(p2) != 1 || p2[0]["id"] == objects(t, page1["data"])[0]["id"] || page2["next_cursor"] != nil {
		t.Fatalf("segunda página: %v", page2)
	}
	if out, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/notifications?status=sent", nil); len(objects(t, out["data"])) != 0 {
		t.Fatalf("filtro por situação: %v", out)
	}
	owner.expectProblem(422, "invalid_filter", http.MethodGet, "/api/v1/notifications?status=entregue", nil)
	owner.expectProblem(422, "invalid_filter", http.MethodGet, "/api/v1/notifications?invoice_id=abc", nil)
	owner.expectProblem(422, "invalid_cursor", http.MethodGet, "/api/v1/notifications?cursor=invalido", nil)

	auditor := a.login(member(t, a.env, tn, tenancy.RoleAuditor), &tn.ID)
	if out, _ := auditor.expect(http.StatusOK, http.MethodGet, "/api/v1/notifications", nil); len(objects(t, out["data"])) != 2 {
		t.Fatalf("auditor lê o histórico: %v", out)
	}
	other := a.env.NewTenant(t, "historico-b")
	if out, _ := a.login(other.Owner, &other.ID).expect(http.StatusOK, http.MethodGet, "/api/v1/notifications?invoice_id="+first.String(), nil); len(objects(t, out["data"])) != 0 {
		t.Fatalf("notificações de outro tenant visíveis: %v", out)
	}
}

// proTenant cria uma organização no plano profissional (webhooks enviados e
// limite de usuários suficiente para os papéis dos testes).
func proTenant(t *testing.T, env *fixture.Env, prefix string) fixture.Tenant {
	t.Helper()
	tn := env.NewTenant(t, prefix)
	if _, err := env.Platform.ChangePlan(context.Background(), tn.ID, tn.Actor, "profissional"); err != nil {
		t.Fatal(err)
	}
	return tn
}

// Endpoints de webhook: tipos de evento, criação com segredo exibido uma
// única vez, destinos privados e de metadados recusados, alteração com
// If-Match, rotação com janela, permissões, plano e remoção.
func TestWebhooksAPIEndpoints(t *testing.T) {
	a := newDunningAPI(t)
	tn := proTenant(t, a.env, "wh-api")
	owner := a.login(tn.Owner, &tn.ID)

	out, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/webhook-event-types", nil)
	types := jsonArray(t, out["data"])
	if len(types) != len(webhooks.EventTypes()) || types[0] != webhooks.EventTypes()[0] {
		t.Fatalf("tipos de evento: %v", types)
	}

	input := map[string]any{"url": "https://hooks.cliente.test/recorvia", "description": "ERP financeiro",
		"event_types": []string{outbox.EventInvoicePaid, outbox.EventInvoiceIssued, outbox.EventInvoicePaid}}
	created, h := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/webhook-endpoints", input)
	secret, _ := created["secret"].(string)
	ep := created["endpoint"].(map[string]any)
	if !strings.HasPrefix(secret, webhooks.SecretPrefix) || ep["status"] != "active" || ep["version"] != float64(1) || h.Get("ETag") != `"1"` ||
		len(jsonArray(t, ep["event_types"])) != 2 || len(objects(t, ep["secrets"])) != 1 || ep["description"] != "ERP financeiro" {
		t.Fatalf("endpoint criado: %v", created)
	}
	id := ep["id"].(string)
	for _, path := range []string{"/api/v1/webhook-endpoints", "/api/v1/audit-events?resource_type=webhook_endpoint"} {
		raw := rawBody(t, owner, path)
		if strings.Contains(raw, secret) || strings.Contains(raw, strings.TrimPrefix(secret, webhooks.SecretPrefix)) {
			t.Fatalf("segredo exposto em %s", path)
		}
	}

	// Destinos recusados (proteção contra SSRF) e dados inválidos.
	for _, u := range []string{"https://interno.cliente.test/hook", "http://169.254.169.254/latest/meta-data", "http://127.0.0.1:8081/hook",
		"http://[::1]/hook", "https://desconhecido.cliente.test/hook"} {
		owner.expectProblem(422, "webhook_destination_not_allowed", http.MethodPost, "/api/v1/webhook-endpoints",
			map[string]any{"url": u, "event_types": []string{outbox.EventInvoicePaid}})
	}
	for _, body := range []map[string]any{
		{"url": "ftp://hooks.cliente.test/x", "event_types": []string{outbox.EventInvoicePaid}},
		{"url": "https://usuario:senha@hooks.cliente.test/x", "event_types": []string{outbox.EventInvoicePaid}},
		{"url": "https://hooks.cliente.test/x", "event_types": []string{outbox.EventProviderWebhook}},
		{"url": "https://hooks.cliente.test/x", "event_types": []string{}},
	} {
		owner.expectProblem(422, "invalid_webhook_endpoint", http.MethodPost, "/api/v1/webhook-endpoints", body)
	}

	// Alteração: If-Match, situação obrigatória e descrição omitida preservada.
	path := "/api/v1/webhook-endpoints/" + id
	upd := map[string]any{"url": "https://hooks.cliente.test/v2", "event_types": []string{outbox.EventInvoiceOverdue}, "status": "disabled"}
	owner.expectProblem(428, "if_match_required", http.MethodPatch, path, upd)
	owner.expectProblem(412, "version_mismatch", http.MethodPatch, path, upd, "If-Match", `"5"`)
	owner.expectProblem(422, "invalid_status", http.MethodPatch, path, map[string]any{"url": "https://hooks.cliente.test/v2",
		"event_types": []string{outbox.EventInvoiceOverdue}, "description": "x"}, "If-Match", `"1"`)
	got, h := owner.expect(http.StatusOK, http.MethodPatch, path, upd, "If-Match", `"1"`)
	if got["url"] != "https://hooks.cliente.test/v2" || got["status"] != "disabled" || got["description"] != "ERP financeiro" || h.Get("ETag") != `"2"` {
		t.Fatalf("alteração: %v", got)
	}
	upd["status"], upd["url"] = "active", "https://interno.cliente.test/v3"
	owner.expectProblem(422, "webhook_destination_not_allowed", http.MethodPatch, path, upd, "If-Match", `"2"`)
	upd["url"], upd["description"] = "https://hooks.cliente.test/v3", ""
	got, _ = owner.expect(http.StatusOK, http.MethodPatch, path, upd, "If-Match", `"2"`)
	if got["status"] != "active" || got["description"] != "" || got["version"] != float64(3) {
		t.Fatalf("reativação: %v", got)
	}

	// Rotação: novo segredo uma única vez; o anterior expira na janela.
	owner.expectProblem(428, "if_match_required", http.MethodPost, path+"/rotate-secret", nil)
	owner.expectProblem(412, "version_mismatch", http.MethodPost, path+"/rotate-secret", nil, "If-Match", `"2"`)
	rotated, rh := owner.expect(http.StatusOK, http.MethodPost, path+"/rotate-secret", nil, "If-Match", `"3"`)
	if rotated["endpoint"].(map[string]any)["version"] != float64(4) || rh.Get("ETag") != `"4"` {
		t.Fatalf("a rotação gera a versão seguinte do endpoint: %v (ETag %q)", rotated["endpoint"], rh.Get("ETag"))
	}
	newSecret := rotated["secret"].(string)
	secrets := objects(t, rotated["endpoint"].(map[string]any)["secrets"])
	if newSecret == secret || !strings.HasPrefix(newSecret, webhooks.SecretPrefix) || len(secrets) != 2 {
		t.Fatalf("rotação padrão: %v", rotated)
	}
	expiring := 0
	for _, s := range secrets {
		if s["expires_at"] != nil {
			expiring++
			if d := instant(t, s["expires_at"]).Sub(a.env.Clock.Now()) - 24*time.Hour; d < -time.Second || d > time.Second {
				t.Fatalf("janela padrão de 24 h: %v", s["expires_at"])
			}
		}
	}
	if expiring != 1 {
		t.Fatalf("segredo anterior com validade: %v", secrets)
	}
	owner.expectProblem(422, "invalid_rotation_window", http.MethodPost, path+"/rotate-secret", map[string]any{"previous_secret_valid_hours": 169},
		"If-Match", `"4"`)
	rotated, _ = owner.expect(http.StatusOK, http.MethodPost, path+"/rotate-secret", map[string]any{"previous_secret_valid_hours": 0}, "If-Match", `"4"`)
	if s := objects(t, rotated["endpoint"].(map[string]any)["secrets"]); len(s) != 1 || s[0]["expires_at"] != nil {
		t.Fatalf("rotação sem janela encerra os anteriores: %v", s)
	}
	if acts := auditActions(owner, "webhook_endpoint"); acts["webhook_endpoint.created"] != 1 || acts["webhook_endpoint.updated"] != 2 ||
		acts["webhook_endpoint.secret_rotated"] != 2 {
		t.Fatalf("auditoria dos endpoints: %v", acts)
	}

	// Permissões: auditor e financeiro leem; somente quem tem webhooks.write
	// altera; operador não lê.
	auditor := a.login(member(t, a.env, tn, tenancy.RoleAuditor), &tn.ID)
	auditor.expect(http.StatusOK, http.MethodGet, "/api/v1/webhook-endpoints", nil)
	auditor.expectProblem(403, "forbidden", http.MethodPost, path+"/rotate-secret", nil, "If-Match", `"5"`)
	finance := a.login(member(t, a.env, tn, tenancy.RoleFinance), &tn.ID)
	finance.expect(http.StatusOK, http.MethodGet, "/api/v1/webhook-event-types", nil)
	finance.expectProblem(403, "forbidden", http.MethodDelete, path, nil, "If-Match", `"5"`)
	operator := a.login(member(t, a.env, tn, tenancy.RoleOperator), &tn.ID)
	operator.expectProblem(403, "forbidden", http.MethodGet, "/api/v1/webhook-endpoints", nil)

	// Isolamento e plano: outro tenant não enxerga o endpoint; plano sem o
	// recurso não cria endpoints.
	other := a.env.NewTenant(t, "wh-api-b")
	ownerB := a.login(other.Owner, &other.ID)
	ownerB.expectProblem(404, "webhook_endpoint_not_found", http.MethodPost, path+"/rotate-secret", nil, "If-Match", `"5"`)
	ownerB.expectProblem(404, "webhook_endpoint_not_found", http.MethodGet, path+"/deliveries", nil)
	ownerB.expectProblem(403, "platform_feature_unavailable", http.MethodPost, "/api/v1/webhook-endpoints", input)

	// Remoção: exige a versão atual, sai da listagem e não aceita novas operações.
	owner.expectProblem(428, "if_match_required", http.MethodDelete, path, nil)
	owner.expectProblem(412, "version_mismatch", http.MethodDelete, path, nil, "If-Match", `"4"`)
	owner.expect(http.StatusNoContent, http.MethodDelete, path, nil, "If-Match", `"5"`)
	if out, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/webhook-endpoints", nil); len(objects(t, out["data"])) != 0 {
		t.Fatalf("endpoint removido listado: %v", out)
	}
	owner.expectProblem(404, "webhook_endpoint_not_found", http.MethodDelete, path, nil, "If-Match", `"6"`)
	owner.expectProblem(404, "not_found", http.MethodDelete, "/api/v1/webhook-endpoints/nao-e-uuid", nil, "If-Match", `"1"`)
}

// rawBody devolve o corpo bruto da resposta (para conferir ausência de segredos).
func rawBody(t *testing.T, c *client, path string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.a.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "recorvia_session", Value: c.cookie})
	req.Header.Set("X-Recorvia-Tenant", c.tenant)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d %v", path, resp.StatusCode, err)
	}
	return string(raw)
}

// Entregas: listagem paginada do endpoint, reenvio manual auditado somente
// fora de andamento e isolamento.
func TestWebhooksAPIDeliveriesAndManualRetry(t *testing.T) {
	a := newDunningAPI(t)
	ctx := context.Background()
	tn := proTenant(t, a.env, "wh-entregas")
	owner := a.login(tn.Owner, &tn.ID)
	created, _ := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/webhook-endpoints",
		map[string]any{"url": "https://hooks.cliente.test/entregas", "event_types": []string{outbox.EventInvoicePaid}})
	id := created["endpoint"].(map[string]any)["id"].(string)
	events := []uuid.UUID{ids.New(), ids.New()}
	for _, ev := range events {
		payload, _ := json.Marshal(map[string]any{"invoice_id": ids.New()})
		if err := a.webhooks.HandleEvent(ctx, tn.ID, ev, outbox.EventInvoicePaid, a.env.Clock.Now(), payload); err != nil {
			t.Fatal(err)
		}
		a.env.Clock.Advance(time.Second)
	}
	// Evento não assinado pelo endpoint não gera entrega.
	if err := a.webhooks.HandleEvent(ctx, tn.ID, ids.New(), outbox.EventInvoiceIssued, a.env.Clock.Now(), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/webhook-endpoints/" + id + "/deliveries"
	page1, _ := owner.expect(http.StatusOK, http.MethodGet, path+"?limit=1", nil)
	cursor, _ := page1["next_cursor"].(string)
	first := objects(t, page1["data"])
	if len(first) != 1 || cursor == "" || first[0]["status"] != "pending" || first[0]["event_type"] != outbox.EventInvoicePaid ||
		first[0]["endpoint_id"] != id || first[0]["attempts"] != float64(0) || first[0]["next_attempt_at"] == nil {
		t.Fatalf("primeira página: %v", page1)
	}
	page2, _ := owner.expect(http.StatusOK, http.MethodGet, path+"?limit=1&cursor="+cursor, nil)
	if p := objects(t, page2["data"]); len(p) != 1 || p[0]["id"] == first[0]["id"] || page2["next_cursor"] != nil {
		t.Fatalf("segunda página: %v", page2)
	}
	deliveryID := first[0]["id"].(string)
	owner.expectProblem(409, "delivery_in_progress", http.MethodPost, "/api/v1/webhook-deliveries/"+deliveryID+"/retry", nil)

	// Uma tentativa com resposta 503 deixa as entregas em retentativa.
	if res, err := a.webhooks.DeliverTenant(ctx, tn.ID); err != nil || res.Retrying != 2 {
		t.Fatalf("tentativas: %+v %v", res, err)
	}
	out, _ := owner.expect(http.StatusOK, http.MethodGet, path, nil)
	for _, d := range objects(t, out["data"]) {
		if d["status"] != "retrying" || d["last_status_code"] != float64(503) || d["attempts"] != float64(1) || d["last_error"] == "" {
			t.Fatalf("entrega após falha: %v", d)
		}
	}
	retried, _ := owner.expect(http.StatusOK, http.MethodPost, "/api/v1/webhook-deliveries/"+deliveryID+"/retry", nil)
	if retried["status"] != "pending" || retried["id"] != deliveryID || instant(t, retried["next_attempt_at"]).After(a.env.Clock.Now()) {
		t.Fatalf("reenvio manual: %v", retried)
	}
	if acts := auditActions(owner, "webhook_delivery"); acts["webhook_delivery.retried"] != 1 {
		t.Fatalf("reenvio auditado: %v", acts)
	}
	owner.expectProblem(404, "webhook_delivery_not_found", http.MethodPost, "/api/v1/webhook-deliveries/"+uuid.NewString()+"/retry", nil)
	auditor := a.login(member(t, a.env, tn, tenancy.RoleAuditor), &tn.ID)
	auditor.expect(http.StatusOK, http.MethodGet, path, nil)
	auditor.expectProblem(403, "forbidden", http.MethodPost, "/api/v1/webhook-deliveries/"+deliveryID+"/retry", nil)
	other := proTenant(t, a.env, "wh-entregas-b")
	a.login(other.Owner, &other.ID).expectProblem(404, "webhook_delivery_not_found", http.MethodPost,
		"/api/v1/webhook-deliveries/"+deliveryID+"/retry", nil)

	// Endpoint desativado: o reenvio é recusado.
	owner.expect(http.StatusOK, http.MethodPatch, "/api/v1/webhook-endpoints/"+id, map[string]any{"url": "https://hooks.cliente.test/entregas",
		"event_types": []string{outbox.EventInvoicePaid}, "status": "disabled"}, "If-Match", `"1"`)
	second := objects(t, page2["data"])[0]["id"].(string)
	owner.expectProblem(409, "webhook_endpoint_disabled", http.MethodPost, "/api/v1/webhook-deliveries/"+second+"/retry", nil)
}
