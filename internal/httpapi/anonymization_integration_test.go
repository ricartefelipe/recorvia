//go:build integration

package httpapi_test

import (
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

const anonymizationReason = "Pedido de eliminação do titular, protocolo 2026/0042"

// personalCustomer cria pela API um cliente com todos os dados pessoais
// preenchidos, um contato e endereço de cobrança.
func personalCustomer(t *testing.T, c *client, name, cpf, email string) string {
	t.Helper()
	body, _ := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/customers", map[string]any{"kind": "individual", "name": name,
		"document_number": cpf, "email": email, "phone": "+5511988887777", "external_id": "crm-" + uuid.NewString()[:8]})
	id := body["id"].(string)
	c.expect(http.StatusCreated, http.MethodPost, "/api/v1/customers/"+id+"/contacts", map[string]any{"name": "Contato de " + name,
		"email": "contato." + email, "receives_billing_email": true})
	c.expect(http.StatusOK, http.MethodPut, "/api/v1/customers/"+id+"/billing-address", map[string]any{"street": "Rua das Acácias", "number": "120",
		"district": "Centro", "city": "São Paulo", "state": "SP", "postal_code": "01001-000"})
	return id
}

// seedCustomerTraces grava, como dono das tabelas, o que meses de operação
// deixam fora do cadastro: notificações com histórico, o identificador do
// cliente no provedor, uma cobrança e o webhook recebido do provedor.
func seedCustomerTraces(t *testing.T, owner *pgxpool.Pool, tenantID uuid.UUID, customerID, invoiceID string, account uuid.UUID, email string) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	for i, status := range []string{"sent", "failed"} {
		id := ids.New()
		var sentAt any
		if status == "sent" {
			sentAt = "now"
		}
		exec(`INSERT INTO notifications (id, tenant_id, business_key, template_key, recipients, subject, content_ciphertext, content_key_id,
				customer_id, invoice_id, status, send_after, attempts, max_attempts, last_error, sent_at, created_at, updated_at)
			VALUES ($1, $2, $3, 'invoice_issued', $4, $5, '\x0102', 't1', $6, $7, $8, now(), 1, 5, $9, $10::timestamptz, now(), now())`,
			id, tenantID, "anon:"+id.String(), []string{email}, "Fatura para "+email, customerID, invoiceID, status,
			"Destinatários recusados: "+email, sentAt)
		exec(`INSERT INTO notification_events (id, tenant_id, notification_id, status, reason, attempt, detail, occurred_at)
			VALUES ($1, $2, $3, $4, 'registro', $5, $6, now())`, ids.New(), tenantID, id, status, i+1, "Destinatários recusados: "+email)
	}
	suffix := customerID[24:]
	exec(`INSERT INTO customer_provider_refs (tenant_id, customer_id, provider, provider_customer_id) VALUES ($1, $2, 'simulator', $3)`,
		tenantID, customerID, "cus_"+suffix)
	exec(`INSERT INTO charges (id, tenant_id, invoice_id, customer_id, provider_account_id, provider, provider_charge_id, status, amount, due_date)
		VALUES ($1, $2, $3, $4, $5, 'simulator', $6, 'canceled', 1000, current_date)`, ids.New(), tenantID, invoiceID, customerID, account, "ch_"+suffix)
	exec(`INSERT INTO provider_webhook_events (id, tenant_id, provider_account_id, provider, provider_event_id, event_type, provider_charge_id,
			received_at, payload, status, processed_at)
		VALUES ($1, $2, $3, 'simulator', $4, 'charge.canceled', $5, now(), jsonb_build_object('payer', jsonb_build_object('email', $6::text)), 'processed', now())`,
		ids.New(), tenantID, account, "evt_"+suffix, "ch_"+suffix, email)
}

func providerAccount(t *testing.T, owner *pgxpool.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	hash := make([]byte, 32)
	if _, err := rand.Read(hash); err != nil {
		t.Fatal(err)
	}
	id := ids.New()
	if _, err := owner.Exec(context.Background(), `INSERT INTO payment_provider_accounts (id, tenant_id, provider, environment, webhook_endpoint_token_hash)
		VALUES ($1, $2, 'simulator', 'local', $3)`, id, tenantID, hash); err != nil {
		t.Fatal(err)
	}
	return id
}

// subscribeAndInvoice cria assinatura e a fatura do período corrente.
func subscribeAndInvoice(t *testing.T, env *fixture.Env, tn fixture.Tenant, customerID string, planID uuid.UUID) (subscriptions.Subscription, billing.Invoice) {
	t.Helper()
	ctx := context.Background()
	sub := env.NewSubscription(t, tn, uuid.MustParse(customerID), planID, nil)
	if _, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	invs, err := env.Billing.List(ctx, tn.ID, billing.ListFilter{SubscriptionID: &sub.ID, Limit: 10})
	if err != nil || len(invs.Items) != 1 {
		t.Fatalf("fatura da assinatura: %v", err)
	}
	return sub, invs.Items[0]
}

// Anonimização pela API: exige permissão própria, sessão de usuário,
// Idempotency-Key e justificativa; recusa cliente com obrigações em aberto;
// substitui o cadastro, remove o que está fora dele, encerra o portal e
// mantém os registros financeiros; é idempotente e irreversível.
func TestCustomerAnonymizationOverHTTP(t *testing.T) {
	ctx := context.Background()
	a := newAPI(t)
	count := ownerCounter(t)
	ownerPool := pgtest.OwnerPool(t)
	tn := apiPlanTenant(t, a, "anonimizacao")
	owner := a.login(tn.Owner, &tn.ID)
	operator := a.login(member(t, a.env, tn, tenancy.RoleOperator), &tn.ID)
	admin := a.login(member(t, a.env, tn, tenancy.RoleAdmin), &tn.ID)

	const name, cpf, email = "Marina Albuquerque Teles", "52998224725", "marina.teles@titular.test"
	custID := personalCustomer(t, owner, name, "529.982.247-25", email)
	otherID := personalCustomer(t, owner, "Rafael Outro Titular", "111.444.777-35", "rafael@titular.test")
	path := "/api/v1/customers/" + custID + "/anonymize"
	plan := a.env.NewFlatPlan(t, tn, 12000)
	sub, inv := subscribeAndInvoice(t, a.env, tn, custID, plan.ID)
	_, otherInv := subscribeAndInvoice(t, a.env, tn, otherID, plan.ID)
	account := providerAccount(t, ownerPool, tn.ID)
	seedCustomerTraces(t, ownerPool, tn.ID, custID, inv.ID.String(), account, email)
	seedCustomerTraces(t, ownerPool, tn.ID, otherID, otherInv.ID.String(), account, "rafael@titular.test")
	portalCookie, _ := openPortal(t, a, createPortalLink(t, owner, uuid.MustParse(custID)))
	key := func() string { return "anon-" + uuid.NewString() }
	body := map[string]any{"reason": anonymizationReason}

	// Permissão própria: o operador altera clientes, mas não os anonimiza.
	operator.expectProblem(http.StatusForbidden, "forbidden", http.MethodPost, path, body, "Idempotency-Key", key())
	// Não é delegável a credenciais, e a rota recusa a credencial mesmo com outros escopos.
	if st, b, _ := owner.do(http.MethodPost, "/api/v1/api-credentials", map[string]any{"name": "Privacidade", "scopes": []string{"customers.anonymize"},
		"expires_in_days": 30}); st != http.StatusUnprocessableEntity {
		t.Fatalf("credencial com escopo de anonimização: %d %v", st, b)
	}
	_, _, secret := createCredential(t, owner, "customers.read", "customers.write")
	if r := a.raw(http.MethodPost, path, body, bearer(secret, "Idempotency-Key", key())); r.status != http.StatusForbidden ||
		r.body["code"] != "user_session_required" {
		t.Fatalf("anonimização por credencial: %d %v", r.status, r.body)
	}
	// Idempotency-Key e justificativa obrigatórias; a justificativa não leva dados do titular.
	owner.expectProblem(http.StatusUnprocessableEntity, "idempotency_key_required", http.MethodPost, path, body)
	for name, reason := range map[string]any{"ausente": nil, "curta": "pedido", "com o e-mail do titular": "Pedido enviado por " + email,
		"com o nome do titular": "Pedido de " + strings.ToUpper(name) + ", protocolo 9", "com o CPF": "Titular do CPF 529.982.247-25, protocolo 9"} {
		in := map[string]any{}
		if reason != nil {
			in["reason"] = reason
		}
		if st, b, _ := owner.do(http.MethodPost, path, in, "Idempotency-Key", key()); st != http.StatusUnprocessableEntity || b["code"] != "invalid_anonymization" {
			t.Fatalf("justificativa %s: %d %v", name, st, b)
		}
	}
	// Assinatura ativa e fatura em aberto impedem.
	owner.expectProblem(http.StatusConflict, "customer_has_obligations", http.MethodPost, path, body, "Idempotency-Key", key())
	// Cliente de outra organização não é encontrado.
	foreign := a.env.NewTenant(t, "anonimizacao-fora")
	a.login(foreign.Owner, &foreign.ID).expectProblem(http.StatusNotFound, "customer_not_found", http.MethodPost, path, body, "Idempotency-Key", key())
	if n := count(t, `SELECT count(*) FROM customers WHERE id = $1 AND anonymized_at IS NULL AND status = 'active' AND name = $2`, custID, name); n != 1 {
		t.Fatal("o cliente foi alterado por um pedido recusado")
	}
	if n := count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'customer.anonymized'`, tn.ID); n != 0 {
		t.Fatalf("auditoria de pedidos recusados: %d", n)
	}

	// Encerradas a assinatura e a fatura, a anonimização é aceita.
	if _, err := a.env.Subscriptions.Cancel(ctx, tn.ID, tn.Actor, sub.ID, subscriptions.CancelImmediately, "encerramento do contrato", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.env.Billing.Void(ctx, tn.ID, tn.Actor, inv.ID, "emitida por engano", 0); err != nil {
		t.Fatal(err)
	}
	financial := func() map[string]int {
		out := map[string]int{}
		for _, table := range []string{"invoices", "invoice_items", "invoice_status_history", "ledger_transactions", "ledger_entries", "charges",
			"subscriptions", "subscription_history", "customer_credit_entries", "payments"} {
			out[table] = count(t, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, tn.ID)
		}
		return out
	}
	before := financial()
	auditBefore := count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, tn.ID)
	previous, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/customers/"+custID, nil)
	firstKey := key()
	// O administrador também tem a permissão.
	res, hdr := admin.expect(http.StatusOK, http.MethodPost, path, map[string]any{"reason": "  " + anonymizationReason + " "}, "Idempotency-Key", firstKey)
	cust := res["customer"].(map[string]any)
	if cust["name"] != "Cliente anonimizado" || cust["document_number"] != "00000000000" || cust["email"] != "" || cust["phone"] != "" ||
		cust["external_id"] != "" || cust["legal_name"] != "" || cust["status"] != "archived" || cust["anonymized_at"] == nil ||
		len(cust["contacts"].([]any)) != 0 || cust["billing_address"] != nil || cust["kind"] != "individual" {
		t.Fatalf("cliente devolvido: %v", cust)
	}
	if res["already_anonymized"] != false || hdr.Get("ETag") == "" {
		t.Fatalf("resposta: %v %v", res, hdr)
	}
	refs := objects(t, res["provider_customers"])
	if len(refs) != 1 || refs[0]["provider"] != "simulator" || refs[0]["provider_customer_id"] != "cus_"+custID[24:] {
		t.Fatalf("identificadores no provedor devolvidos: %v", refs)
	}
	removed := res["removed"].(map[string]any)
	if removed["contacts"] != float64(1) || removed["billing_address"] != true || removed["portal_links_revoked"] != float64(1) ||
		removed["notifications"] != float64(2) || removed["provider_webhook_payloads"] != float64(1) {
		t.Fatalf("resumo do que foi removido: %v", removed)
	}

	// Nenhum dado pessoal do titular resta no cadastro nem fora dele.
	for table, q := range map[string]string{
		"customers":                  `SELECT count(*) FROM customers WHERE id = $1 AND (name <> 'Cliente anonimizado' OR email IS NOT NULL OR phone IS NOT NULL OR external_id IS NOT NULL OR legal_name IS NOT NULL OR document_number <> '00000000000' OR status <> 'archived' OR anonymized_at IS NULL)`,
		"customer_contacts":          `SELECT count(*) FROM customer_contacts WHERE customer_id = $1`,
		"customer_billing_addresses": `SELECT count(*) FROM customer_billing_addresses WHERE customer_id = $1`,
		"customer_provider_refs":     `SELECT count(*) FROM customer_provider_refs WHERE customer_id = $1`,
		"notifications":              `SELECT count(*) FROM notifications WHERE customer_id = $1`,
		"customer_portal_links":      `SELECT count(*) FROM customer_portal_links WHERE customer_id = $1 AND revoked_at IS NULL`,
	} {
		if n := count(t, q, custID); n != 0 {
			t.Errorf("%s: %d linhas com dados do cliente anonimizado", table, n)
		}
	}
	if n := count(t, `SELECT count(*) FROM notification_events WHERE tenant_id = $1 AND detail LIKE '%' || $2 || '%'`, tn.ID, email); n != 0 {
		t.Errorf("histórico de notificações com o e-mail do titular: %d", n)
	}
	if n := count(t, `SELECT count(*) FROM provider_webhook_events WHERE tenant_id = $1 AND provider_charge_id = $2 AND payload = '{}'::jsonb
		AND payload_purged_at IS NOT NULL`, tn.ID, "ch_"+custID[24:]); n != 1 {
		t.Errorf("corpo do webhook recebido da cobrança do cliente não foi esvaziado")
	}
	// O outro cliente não foi tocado.
	for table, q := range map[string]string{
		"customers":               `SELECT count(*) FROM customers WHERE id = $1 AND name = 'Rafael Outro Titular' AND anonymized_at IS NULL`,
		"customer_contacts":       `SELECT count(*) FROM customer_contacts WHERE customer_id = $1`,
		"customer_provider_refs":  `SELECT count(*) FROM customer_provider_refs WHERE customer_id = $1`,
		"provider_webhook_events": `SELECT count(*) FROM provider_webhook_events w JOIN charges c ON c.tenant_id = w.tenant_id AND c.provider_charge_id = w.provider_charge_id WHERE c.customer_id = $1 AND w.payload->'payer'->>'email' = 'rafael@titular.test'`,
	} {
		if n := count(t, q, otherID); n != 1 {
			t.Errorf("%s do outro cliente: %d", table, n)
		}
	}
	if n := count(t, `SELECT count(*) FROM notifications WHERE customer_id = $1`, otherID); n != 2 {
		t.Errorf("notificações do outro cliente: %d", n)
	}

	// Registros financeiros intactos: a fatura emitida mantém a cópia do
	// cadastro do momento da emissão, por decisão documentada (ADR-061).
	if after := financial(); len(after) != len(before) {
		t.Fatal("tabelas financeiras")
	} else {
		for table, n := range before {
			if after[table] != n {
				t.Errorf("%s: %d linhas antes e %d depois da anonimização", table, n, after[table])
			}
		}
	}
	if n := count(t, `SELECT count(*) FROM invoices WHERE id = $1 AND customer_id = $2 AND customer_snapshot->>'name' = $3
		AND customer_snapshot->>'document_number' = $4`, inv.ID, custID, name, cpf); n != 1 {
		t.Error("o snapshot da fatura emitida deveria permanecer como foi emitido")
	}
	if st, b, _ := owner.do(http.MethodGet, "/api/v1/invoices/"+inv.ID.String(), nil); st != http.StatusOK ||
		b["customer_snapshot"].(map[string]any)["name"] != name {
		t.Fatalf("fatura emitida depois da anonimização: %d %v", st, b)
	}

	// Auditoria: uma entrada, com a justificativa e o identificador do cliente
	// no provedor (que já não está no cadastro), sem dados pessoais; o evento
	// para as integrações não leva nem dados pessoais nem o identificador.
	if n := count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, tn.ID); n != auditBefore+1 {
		t.Fatalf("entradas de auditoria: %d, antes %d", n, auditBefore)
	}
	if n := count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'customer.anonymized' AND resource_id = $2 AND actor_type = 'user'
		AND details->>'reason' = $3 AND (details->>'contacts_removed')::int = 1 AND (details->>'notifications_removed')::int = 2
		AND details->'providers' = '["simulator"]'::jsonb AND details->>'previous_status' = 'active'
		AND details->'provider_customers' = jsonb_build_array(jsonb_build_object('provider', 'simulator', 'provider_customer_id', $4::text))`,
		tn.ID, custID, anonymizationReason, "cus_"+custID[24:]); n != 1 {
		t.Fatal("entrada de auditoria da anonimização ausente ou incompleta")
	}
	for _, leaked := range []string{name, cpf, email, "988887777"} {
		if n := count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'customer.anonymized' AND details::text ILIKE '%' || $2 || '%'`,
			tn.ID, leaked); n != 0 {
			t.Errorf("auditoria da anonimização contém %q", leaked)
		}
	}
	for _, leaked := range []string{name, cpf, email, "988887777", "cus_" + custID[24:]} {
		if n := count(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2 AND payload::text ILIKE '%' || $3 || '%'`,
			tn.ID, custID, leaked); n != 0 {
			t.Errorf("evento da outbox contém %q", leaked)
		}
	}
	if n := count(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = 'customer.updated'
		AND payload->>'anonymized' = 'true'`, tn.ID, custID); n != 1 {
		t.Fatal("evento customer.updated da anonimização ausente")
	}
	// O histórico do cliente mostra a anonimização com o identificador no
	// provedor: é por onde ele é recuperado se a resposta não for guardada.
	hist, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/customers/"+custID+"/history", nil)
	entry := byField(t, objects(t, hist["data"]), "action")["customer.anonymized"]
	if entry == nil {
		t.Fatal("histórico do cliente sem a anonimização")
	}
	if recorded := objects(t, entry["details"].(map[string]any)["provider_customers"]); len(recorded) != 1 ||
		recorded[0]["provider_customer_id"] != "cus_"+custID[24:] {
		t.Fatalf("identificadores no provedor registrados no histórico: %v", recorded)
	}

	// A leitura e a busca não devolvem mais os dados anteriores.
	got, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/customers/"+custID, nil)
	if got["name"] != "Cliente anonimizado" || got["anonymized_at"] == nil || got["billing_address"] != nil {
		t.Fatalf("leitura do cliente: %v", got)
	}
	// A resposta da anonimização traz o cliente como ficou gravado: o mesmo
	// instante de atualização da leitura seguinte, não o anterior à alteração.
	if cust["updated_at"] != got["updated_at"] || cust["updated_at"] == previous["updated_at"] || cust["version"] != got["version"] {
		t.Fatalf("updated_at: resposta %v, leitura seguinte %v, leitura anterior %v", cust["updated_at"], got["updated_at"], previous["updated_at"])
	}
	for _, q := range []string{"Marina", cpf} {
		list, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/customers?q="+q, nil)
		if len(objects(t, list["data"])) != 0 {
			t.Errorf("busca por %q ainda encontra o cliente", q)
		}
	}

	// O portal é encerrado: sessão recusada na requisição seguinte e nenhum link novo.
	if r := a.raw(http.MethodGet, "/api/portal/v1/invoices", nil, nil, portalCookie); r.status != http.StatusUnauthorized {
		t.Fatalf("portal de cliente anonimizado: %d %v", r.status, r.body)
	}
	owner.expectProblem(http.StatusConflict, "customer_archived", http.MethodPost, "/api/v1/customers/"+custID+"/portal-links", nil)

	// Idempotente: a mesma chave devolve a resposta registrada; uma chave nova
	// devolve o estado atual, com os identificadores registrados na
	// anonimização e sem nova auditoria.
	st, replay, rh := admin.do(http.MethodPost, path, map[string]any{"reason": "  " + anonymizationReason + " "}, "Idempotency-Key", firstKey)
	if st != http.StatusOK || rh.Get("Idempotent-Replayed") != "true" || len(objects(t, replay["provider_customers"])) != 1 {
		t.Fatalf("repetição com a mesma chave: %d %v %v", st, replay, rh)
	}
	again, _ := owner.expect(http.StatusOK, http.MethodPost, path, map[string]any{"reason": "Segundo pedido do mesmo titular"}, "Idempotency-Key", key())
	againRefs := objects(t, again["provider_customers"])
	if again["already_anonymized"] != true || len(againRefs) != 1 || againRefs[0]["provider"] != "simulator" ||
		againRefs[0]["provider_customer_id"] != "cus_"+custID[24:] || again["customer"].(map[string]any)["version"] != cust["version"] ||
		again["customer"].(map[string]any)["updated_at"] != got["updated_at"] {
		t.Fatalf("repetição com outra chave: %v", again)
	}
	if r := again["removed"].(map[string]any); r["contacts"] != float64(0) || r["billing_address"] != false || r["notifications"] != float64(0) {
		t.Fatalf("a repetição não remove nada: %v", r)
	}
	if n := count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'customer.anonymized'`, tn.ID); n != 1 {
		t.Fatalf("auditoria depois das repetições: %d", n)
	}

	// Irreversível: nenhuma operação regrava dados pessoais no cliente.
	version := `"` + strings.TrimSuffix(strings.TrimPrefix(hdr.Get("ETag"), `"`), `"`) + `"`
	owner.expectProblem(http.StatusConflict, "customer_archived", http.MethodPatch, "/api/v1/customers/"+custID, map[string]any{"kind": "individual",
		"name": name, "document_number": cpf}, "If-Match", version)
	owner.expectProblem(http.StatusConflict, "customer_anonymized", http.MethodPost, "/api/v1/customers/"+custID+"/contacts",
		map[string]any{"name": name, "email": email})
	owner.expectProblem(http.StatusConflict, "customer_anonymized", http.MethodPut, "/api/v1/customers/"+custID+"/billing-address",
		map[string]any{"street": "Rua A", "number": "1", "district": "Centro", "city": "Recife", "state": "PE", "postal_code": "50000000"})
	owner.expectProblem(http.StatusConflict, "customer_archived", http.MethodPost, "/api/v1/subscriptions",
		map[string]any{"customer_id": custID, "plan_id": plan.ID})
	// No banco, nem o dono das tabelas altera o cliente anonimizado, e uma
	// assinatura gravada diretamente é recusada.
	if _, err := ownerPool.Exec(ctx, `UPDATE customers SET name = $2, anonymized_at = NULL WHERE id = $1`, custID, name); !postgres.IsRaisedException(err) {
		t.Fatalf("alteração direta do cliente anonimizado: %v", err)
	}
	_, err := ownerPool.Exec(ctx, `INSERT INTO subscriptions SELECT (jsonb_populate_record(s, jsonb_build_object('id', $2::uuid, 'customer_id', $3::uuid))).*
		FROM subscriptions s WHERE s.id = $1`, sub.ID, ids.New(), custID)
	if !postgres.IsRaisedException(err) {
		t.Fatalf("assinatura para cliente anonimizado gravada no banco: %v", err)
	}

	// Eventos posteriores não recriam notificações para o cliente anonimizado.
	notif := notifications.NewService(a.env.Store.Notifications(), nil, a.env.Keyring, a.env.Clock, notifications.Config{})
	if err := notif.HandleInvoiceIssued(ctx, tn.ID, inv.ID, ids.New()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, `SELECT count(*) FROM notifications WHERE customer_id = $1`, custID); n != 0 {
		t.Fatalf("notificação recriada para cliente anonimizado: %d", n)
	}
}

// Cliente sem assinaturas, faturas, contatos nem endereço: a anonimização
// cobre o cadastro e não falha por falta do que remover; cliente já arquivado
// também é aceito.
func TestCustomerAnonymizationOfArchivedCustomerWithoutTraces(t *testing.T) {
	a := newAPI(t)
	count := ownerCounter(t)
	tn := a.env.NewTenant(t, "anonimizacao-simples")
	owner := a.login(tn.Owner, &tn.ID)
	cust := a.env.NewCustomer(t, tn)
	id := cust.ID.String()
	owner.expect(http.StatusOK, http.MethodPost, "/api/v1/customers/"+id+"/archive", nil)
	res, _ := owner.expect(http.StatusOK, http.MethodPost, "/api/v1/customers/"+id+"/anonymize", map[string]any{"reason": anonymizationReason},
		"Idempotency-Key", "anon-"+uuid.NewString())
	c := res["customer"].(map[string]any)
	if c["document_number"] != "00000000000000" || c["document_type"] != "cnpj" || c["name"] != "Cliente anonimizado" || c["anonymized_at"] == nil {
		t.Fatalf("pessoa jurídica anonimizada: %v", c)
	}
	removed := res["removed"].(map[string]any)
	if removed["contacts"] != float64(0) || removed["billing_address"] != false || len(objects(t, res["provider_customers"])) != 0 {
		t.Fatalf("nada havia para remover: %v", res)
	}
	if n := count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'customer.anonymized' AND details->>'previous_status' = 'archived'`,
		tn.ID); n != 1 {
		t.Fatal("auditoria da anonimização de cliente arquivado")
	}
	// O documento neutro não bloqueia novos cadastros nem outras anonimizações.
	second := a.env.NewCustomer(t, tn)
	owner.expect(http.StatusOK, http.MethodPost, "/api/v1/customers/"+second.ID.String()+"/anonymize", map[string]any{"reason": anonymizationReason},
		"Idempotency-Key", "anon-"+uuid.NewString())
	if n := count(t, `SELECT count(*) FROM customers WHERE tenant_id = $1 AND document_number = '00000000000000' AND anonymized_at IS NOT NULL`, tn.ID); n != 2 {
		t.Fatalf("clientes anonimizados com o documento neutro: %d", n)
	}
}

// A API cai depois de confirmar a anonimização e antes de registrar a
// resposta na chave de idempotência: os identificadores do cliente no
// provedor, já excluídos do cadastro, não se perdem. A repetição com a mesma
// chave assume o pedido interrompido e devolve os mesmos identificadores, que
// também ficam na auditoria.
func TestCustomerAnonymizationKeepsProviderIdentifiersWhenResponseIsLost(t *testing.T) {
	ctx := context.Background()
	a := newAPI(t)
	count := ownerCounter(t)
	ownerPool := pgtest.OwnerPool(t)
	tn := a.env.NewTenant(t, "anonimizacao-resposta-perdida")
	owner := a.login(tn.Owner, &tn.ID)
	id := a.env.NewCustomer(t, tn).ID.String()
	for provider, ref := range map[string]string{"simulator": "cus_sim_" + id[24:], "asaas": "cus_asaas_" + id[24:]} {
		if _, err := ownerPool.Exec(ctx, `INSERT INTO customer_provider_refs (tenant_id, customer_id, provider, provider_customer_id)
			VALUES ($1, $2, $3, $4)`, tn.ID, id, provider, ref); err != nil {
			t.Fatal(err)
		}
	}
	path, key, body := "/api/v1/customers/"+id+"/anonymize", "anon-"+uuid.NewString(), map[string]any{"reason": anonymizationReason}
	first, _ := owner.expect(http.StatusOK, http.MethodPost, path, body, "Idempotency-Key", key)
	want := []map[string]any{{"provider": "asaas", "provider_customer_id": "cus_asaas_" + id[24:]},
		{"provider": "simulator", "provider_customer_id": "cus_sim_" + id[24:]}}
	sameRefs := func(got any) bool {
		refs := objects(t, got)
		if len(refs) != len(want) {
			return false
		}
		for i := range want {
			if refs[i]["provider"] != want[i]["provider"] || refs[i]["provider_customer_id"] != want[i]["provider_customer_id"] {
				return false
			}
		}
		return true
	}
	if first["already_anonymized"] != false || !sameRefs(first["provider_customers"]) {
		t.Fatalf("primeira resposta: %v", first)
	}
	if n := count(t, `SELECT count(*) FROM customer_provider_refs WHERE customer_id = $1`, id); n != 0 {
		t.Fatalf("identificadores ainda no cadastro: %d", n)
	}

	// Estado deixado pela queda: a chave continua em processamento, sem
	// resposta registrada, com o prazo de processamento vencido.
	tag, err := ownerPool.Exec(ctx, `UPDATE idempotency_keys SET status = 'in_progress', response_status = NULL, response_body = NULL,
		response_content_type = NULL, completed_at = NULL, locked_until = created_at - interval '1 hour'
		WHERE tenant_id = $1 AND idempotency_key = $2 AND status = 'completed'`, tn.ID, key)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("chave de idempotência da anonimização: %v %v", tag, err)
	}
	st, retry, hdr := owner.do(http.MethodPost, path, body, "Idempotency-Key", key)
	if st != http.StatusOK || hdr.Get("Idempotent-Replayed") != "" {
		t.Fatalf("a repetição deveria reexecutar o pedido interrompido: %d %v %v", st, retry, hdr)
	}
	if retry["already_anonymized"] != true || !sameRefs(retry["provider_customers"]) {
		t.Fatalf("a repetição perdeu os identificadores do provedor: %v", retry)
	}
	if retry["customer"].(map[string]any)["updated_at"] != first["customer"].(map[string]any)["updated_at"] {
		t.Fatalf("cliente devolvido na repetição: %v, na primeira resposta: %v", retry["customer"], first["customer"])
	}
	// A resposta reexecutada fica registrada e passa a ser repetida pela chave.
	if _, _, rh := owner.do(http.MethodPost, path, body, "Idempotency-Key", key); rh.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("a chave deveria repetir a resposta registrada: %v", rh)
	}
	// Uma única anonimização auditada, com os identificadores.
	if n := count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'customer.anonymized' AND resource_id = $2
		AND jsonb_array_length(details->'provider_customers') = 2`, tn.ID, id); n != 1 {
		t.Fatalf("auditoria da anonimização com os identificadores: %d", n)
	}
	audit, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/audit-events?action=customer.anonymized", nil)
	events := objects(t, audit["data"])
	if len(events) != 1 || !sameRefs(events[0]["details"].(map[string]any)["provider_customers"]) {
		t.Fatalf("consulta da auditoria: %v", events)
	}
}
