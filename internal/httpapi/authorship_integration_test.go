//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/contract"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// counter executa contagens como dono das tabelas (sem RLS), para conferir
// colunas que a API não expõe, como as de autoria.
type counter func(t *testing.T, query string, args ...any) int

func ownerCounter(t *testing.T) counter {
	t.Helper()
	pool := pgtest.OwnerPool(t)
	return func(t *testing.T, query string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
}

// recordDeadLetter grava uma mensagem não processada do tenant, como faria o
// worker ao esgotar as tentativas.
func recordDeadLetter(t *testing.T, a *dunningAPI, tenantID uuid.UUID, withPayload bool) string {
	t.Helper()
	d := pgstore.DeadLetter{ID: uuid.New(), TenantID: &tenantID, Consumer: "webhooks.dispatch", MessageID: uuid.NewString(),
		Error: "destino indisponível", Attempts: 5, FailedAt: a.env.Clock.Now()}
	if withPayload {
		d.EventType, d.Payload = outbox.EventInvoicePaid, []byte(`{"invoice_id":"`+uuid.NewString()+`"}`)
	}
	if err := a.env.Store.RecordDeadLetter(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return d.ID.String()
}

// sender envia uma requisição por um tipo de principal (credencial ou sessão).
type sender func(method, path string, body any, extra ...string) rawResponse

// Rotas de tenant que aceitam credencial de API e gravam autoria. Pela pilha
// Bearer, a resposta é a de sucesso (antes, políticas, templates e endpoints
// gravavam uuid.Nil na coluna de autoria e a FK para users respondia 500), a
// coluna fica nula e a auditoria registra o ator api_credential. Pela sessão
// do usuário, a mesma rota grava o membro como autor. No tratamento de dead
// letters, a auditoria guarda também a justificativa, que para a credencial é
// registrada apenas ali e na nota da resolução.
func TestAPICredentialAuthorshipOnTenantWriteRoutes(t *testing.T) {
	a := newDunningAPI(t)
	count := ownerCounter(t)
	tn := proTenant(t, a.env, "autoria-credencial")
	owner := a.login(tn.Owner, &tn.ID)
	credID, _, secret := createCredential(t, owner, "collections.write", "webhooks.write", "credits.write", "operations.reprocess")
	var viaCredential sender = func(method, path string, body any, extra ...string) rawResponse {
		return a.raw(method, path, body, bearer(secret, extra...))
	}
	var viaSession sender = func(method, path string, body any, extra ...string) rawResponse {
		h := map[string]string{"X-CSRF-Token": owner.csrf, "X-Recorvia-Tenant": owner.tenant}
		for i := 0; i+1 < len(extra); i += 2 {
			h[extra[i]] = extra[i+1]
		}
		return a.raw(method, path, body, h, &http.Cookie{Name: "recorvia_session", Value: owner.cookie})
	}
	template := map[string]any{"subject": "Fatura {{invoice_number}} em atraso", "body": "Olá, {{customer_name}}. A fatura {{invoice_number}} venceu."}

	cases := []struct {
		route  string
		status int
		// do executa a rota e devolve a resposta e o identificador da linha
		// (e do recurso auditado) com a coluna de autoria.
		do                      func(t *testing.T, send sender) (rawResponse, string)
		table, column, idColumn string
		action                  string
		// reason é a justificativa que a entrada de auditoria precisa
		// registrar; vazia quando a rota não a exige.
		reason string
	}{
		{"POST /api/v1/collection-policies", http.StatusCreated, func(t *testing.T, send sender) (rawResponse, string) {
			r := send(http.MethodPost, "/api/v1/collection-policies", policyBody("Régua "+uuid.NewString()[:8]))
			id, _ := r.body["id"].(string)
			return r, id
		}, "collection_policies", "created_by", "id", "collection_policy.created", ""},
		{"PUT /api/v1/notification-templates/{key}", http.StatusOK, func(t *testing.T, send sender) (rawResponse, string) {
			return send(http.MethodPut, "/api/v1/notification-templates/reminder_after_due", template,
				matchVersion(owner.versionIn("/api/v1/notification-templates", "key", "reminder_after_due"))...), "reminder_after_due"
		}, "notification_templates", "updated_by", "template_key", "notification_template.updated", ""},
		{"POST /api/v1/webhook-endpoints", http.StatusCreated, func(t *testing.T, send sender) (rawResponse, string) {
			r := send(http.MethodPost, "/api/v1/webhook-endpoints", map[string]any{"url": "https://hooks.cliente.test/" + uuid.NewString(),
				"event_types": []string{outbox.EventInvoicePaid}})
			ep, _ := r.body["endpoint"].(map[string]any)
			id, _ := ep["id"].(string)
			return r, id
		}, "webhook_endpoints", "created_by", "id", "webhook_endpoint.created", ""},
		{"POST /api/v1/customers/{id}/credits", http.StatusCreated, func(t *testing.T, send sender) (rawResponse, string) {
			cust := a.env.NewCustomer(t, tn)
			return send(http.MethodPost, "/api/v1/customers/"+cust.ID.String()+"/credits", map[string]any{"amount": 700, "reason": "Crédito da integração"},
				"Idempotency-Key", "credito-"+uuid.NewString()), cust.ID.String()
		}, "customer_credit_entries", "created_by", "customer_id", "customer.credit_granted", ""},
		{"POST /api/v1/dead-letters/{id}/replay", http.StatusOK, func(t *testing.T, send sender) (rawResponse, string) {
			id := recordDeadLetter(t, a, tn.ID, true)
			return send(http.MethodPost, "/api/v1/dead-letters/"+id+"/replay", map[string]any{"reason": "Destino corrigido"}), id
		}, "dead_letters", "resolved_by", "id", "operations.dead_letter_replayed", "Destino corrigido"},
		{"POST /api/v1/dead-letters/{id}/discard", http.StatusOK, func(t *testing.T, send sender) (rawResponse, string) {
			id := recordDeadLetter(t, a, tn.ID, false)
			return send(http.MethodPost, "/api/v1/dead-letters/"+id+"/discard", map[string]any{"reason": "Evento obsoleto"}), id
		}, "dead_letters", "resolved_by", "id", "operations.dead_letter_discarded", "Evento obsoleto"},
	}
	for _, c := range cases {
		t.Run(c.route, func(t *testing.T) {
			audited := func(id, actorType, actorID string) int {
				return count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND resource_id = $3
					AND actor_type = $4 AND actor_id = $5 AND ($6 = '' OR details->>'reason' = $6)`,
					tn.ID, c.action, id, actorType, actorID, c.reason)
			}
			r, id := c.do(t, viaCredential)
			if r.status != c.status {
				t.Fatalf("por credencial: %d %v", r.status, r.body)
			}
			if n := count(t, `SELECT count(*) FROM `+c.table+` WHERE tenant_id = $1 AND `+c.idColumn+`::text = $2 AND `+c.column+` IS NULL`,
				tn.ID, id); n != 1 {
				t.Errorf("por credencial, %s.%s deveria ficar nulo (%d linha)", c.table, c.column, n)
			}
			if n := audited(id, "api_credential", credID); n != 1 {
				t.Errorf("auditoria %s pela credencial (justificativa %q): %d", c.action, c.reason, n)
			}

			r, id = c.do(t, viaSession)
			if r.status != c.status {
				t.Fatalf("pela sessão: %d %v", r.status, r.body)
			}
			if n := count(t, `SELECT count(*) FROM `+c.table+` WHERE tenant_id = $1 AND `+c.idColumn+`::text = $2 AND `+c.column+` = $3`,
				tn.ID, id, tn.Owner.ID); n != 1 {
				t.Errorf("pela sessão, %s.%s deveria ser o membro (%d linha)", c.table, c.column, n)
			}
			if n := audited(id, "user", tn.Owner.ID.String()); n != 1 {
				t.Errorf("auditoria %s pelo membro (justificativa %q): %d", c.action, c.reason, n)
			}
		})
	}
	// O reprocessamento por credencial grava a reentrega na outbox.
	if n := count(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_type = 'dead_letter_replay' AND event_type = $2`,
		tn.ID, outbox.EventInvoicePaid); n != 2 {
		t.Fatalf("reentregas na outbox: %d", n)
	}
}

// Rotas cujo ato exige uma pessoa identificada (conta e sessão, gestão de
// membros, do plano da plataforma e de credenciais) recusam a credencial pela
// pilha Bearer com 403 user_session_required, mesmo com os escopos de leitura
// correspondentes, sem efeito nenhum. A listagem de credenciais e a sessão
// também são recusadas; as leituras de membros, papéis, convites e da
// plataforma continuam disponíveis à integração pelos escopos, como descreve
// docs/arquitetura/fluxos.md, e a sessão do usuário continua executando o ato.
func TestUserSessionRoutesRejectAPICredential(t *testing.T) {
	a := newAPI(t)
	count := ownerCounter(t)
	tn := apiPlanTenant(t, a, "ato-humano")
	owner := a.login(tn.Owner, &tn.ID)
	operator := member(t, a.env, tn, tenancy.RoleOperator)
	inv, _ := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/invitations", map[string]any{"email": "convidada@empresa.test",
		"role": "finance"})
	invID := inv["invitation"].(map[string]any)["id"].(string)
	credID, _, secret := createCredential(t, owner, "members.read", "platform_billing.read", "customers.read")

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/session", nil},
		{http.MethodPatch, "/api/v1/members/" + operator.ID.String(), map[string]any{"role": "admin"}},
		{http.MethodDelete, "/api/v1/members/" + operator.ID.String(), nil},
		{http.MethodPost, "/api/v1/invitations", map[string]any{"email": "outra@empresa.test", "role": "operator"}},
		{http.MethodDelete, "/api/v1/invitations/" + invID, nil},
		{http.MethodPut, "/api/v1/platform/plan", map[string]any{"plan_code": "escala"}},
		{http.MethodPost, "/api/v1/platform/addon-orders", map[string]any{"addon_code": "implantacao_assistida", "notes": "Pedido pelo ERP"}},
		{http.MethodGet, "/api/v1/api-credentials", nil},
		{http.MethodPost, "/api/v1/api-credentials", map[string]any{"name": "Credencial derivada", "scopes": []string{"customers.read"}}},
		{http.MethodPost, "/api/v1/api-credentials/" + credID + "/rotate", nil},
		{http.MethodDelete, "/api/v1/api-credentials/" + credID, nil},
	} {
		r := a.raw(tc.method, tc.path, tc.body, bearer(secret))
		if r.status != http.StatusForbidden || r.body["code"] != "user_session_required" {
			t.Errorf("%s %s por credencial: %d %v", tc.method, tc.path, r.status, r.body)
		}
	}

	// Nada foi alterado.
	members, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/members", nil)
	if m := byField(t, objects(t, members["data"]), "user_id")[operator.ID.String()]; m["role"] != "operator" || m["status"] != "active" {
		t.Fatalf("membro alterado por credencial: %v", m)
	}
	invs, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/invitations", nil)
	byEmail := byField(t, objects(t, invs["data"]), "email")
	if byEmail["convidada@empresa.test"]["state"] != "pending" || byEmail["outra@empresa.test"] != nil {
		t.Fatalf("convites alterados por credencial: %v", byEmail)
	}
	overview, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/platform/overview", nil)
	if overview["subscription"].(map[string]any)["plan_code"] != "profissional" {
		t.Fatalf("plano alterado por credencial: %v", overview)
	}
	if orders, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/platform/addon-orders", nil); len(objects(t, orders["data"])) != 0 {
		t.Fatalf("pedido de serviço adicional por credencial: %v", orders)
	}
	creds, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/api-credentials", nil)
	if list := objects(t, creds["data"]); len(list) != 1 || list[0]["revoked_at"] != nil {
		t.Fatalf("credenciais alteradas por credencial: %v", list)
	}
	if n := count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND actor_type = 'api_credential'`, tn.ID); n != 0 {
		t.Fatalf("auditoria de ato recusado: %d", n)
	}

	// Leituras pelos escopos continuam disponíveis; o segredo segue válido.
	for _, path := range []string{"/api/v1/members", "/api/v1/roles", "/api/v1/invitations", "/api/v1/platform/overview",
		"/api/v1/platform/plans", "/api/v1/platform/addons", "/api/v1/platform/addon-orders", "/api/v1/platform/statements"} {
		if r := a.raw(http.MethodGet, path, nil, bearer(secret)); r.status != http.StatusOK {
			t.Errorf("leitura %s por credencial: %d %v", path, r.status, r.body)
		}
	}

	// Pela sessão, o pedido de serviço adicional grava o membro como autor
	// (requested_by é obrigatório).
	order, _ := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/platform/addon-orders", map[string]any{"addon_code": "implantacao_assistida"}, "Idempotency-Key", idemKey())
	if n := count(t, `SELECT count(*) FROM platform_addon_orders WHERE tenant_id = $1 AND id = $2 AND requested_by = $3`, tn.ID,
		order["id"], tn.Owner.ID); n != 1 {
		t.Fatalf("autoria do pedido pelo painel: %d", n)
	}
}

// A auditoria é a única autoria de uma dead letter tratada por credencial
// (resolved_by fica nulo), por isso é gravada na mesma transação da
// resolução: se ela falhar, a mensagem continua aberta. Antes, a resolução
// era confirmada e a auditoria gravada depois, em outra transação.
func TestDeadLetterResolutionIsAuditedAtomically(t *testing.T) {
	a := newDunningAPI(t)
	count := ownerCounter(t)
	tn := proTenant(t, a.env, "dead-letter-atomica")
	owner := a.login(tn.Owner, &tn.ID)
	_, _, secret := createCredential(t, owner, "operations.reprocess", "operations.read")
	id := recordDeadLetter(t, a, tn.ID, false)

	// Falha simulada na gravação da auditoria, restrita a este tenant.
	ctx := context.Background()
	pool := pgtest.OwnerPool(t)
	fn := "test_falha_auditoria_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := pool.Exec(ctx, `CREATE FUNCTION `+fn+`() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.tenant_id = '`+tn.ID.String()+`'::uuid AND NEW.action LIKE 'operations.dead_letter_%' THEN
    RAISE EXCEPTION 'falha simulada na auditoria';
  END IF;
  RETURN NEW;
END $$`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`() CASCADE`) })
	if _, err := pool.Exec(ctx, `CREATE TRIGGER `+fn+` BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION `+fn+`()`); err != nil {
		t.Fatal(err)
	}

	path := "/api/v1/dead-letters/" + id + "/discard"
	// A falha é provocada de propósito no banco: o 500 internal_error não é
	// declarado no contrato e fica como exceção explícita desta requisição
	// (ADR-084).
	contract.AllowUndeclared(t, http.MethodPost, path, http.StatusInternalServerError)
	if r := a.raw(http.MethodPost, path, map[string]any{"reason": "Evento obsoleto"}, bearer(secret)); r.status != http.StatusInternalServerError {
		t.Fatalf("descarte com auditoria indisponível: %d %v", r.status, r.body)
	}
	if n := count(t, `SELECT count(*) FROM dead_letters WHERE tenant_id = $1 AND id = $2 AND status = 'open' AND resolved_at IS NULL`,
		tn.ID, id); n != 1 {
		t.Fatal("a dead letter foi resolvida sem auditoria")
	}

	if _, err := pool.Exec(ctx, `DROP TRIGGER `+fn+` ON audit_log`); err != nil {
		t.Fatal(err)
	}
	if r := a.raw(http.MethodPost, path, map[string]any{"reason": "Evento obsoleto"}, bearer(secret)); r.status != http.StatusOK ||
		r.body["status"] != "discarded" {
		t.Fatalf("descarte após restabelecer a auditoria: %d %v", r.status, r.body)
	}
	if n := count(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND resource_id = $2 AND action = 'operations.dead_letter_discarded'
		AND actor_type = 'api_credential'`, tn.ID, id); n != 1 {
		t.Fatalf("auditoria do descarte: %d", n)
	}
}
