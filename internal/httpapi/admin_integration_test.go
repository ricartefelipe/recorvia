//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// staffUser cria um integrante da equipe da plataforma, sem organização.
func staffUser(t *testing.T, a *apiEnv) identity.User {
	t.Helper()
	ctx := context.Background()
	uid := ids.New()
	u, err := a.env.Store.Users().UpsertFromClaims(ctx, uid, identity.Claims{Issuer: "https://idp.test", Subject: uid.String(),
		Email: uid.String()[24:] + "@recorvia.test", EmailVerified: true, Name: "Equipe Recorvia"}, a.env.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgtest.OwnerPool(t).Exec(ctx, `INSERT INTO platform_staff (user_id, role) VALUES ($1, 'operations')`, u.ID); err != nil {
		t.Fatal(err)
	}
	return u
}

func findOrder(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()
	for _, it := range body["data"].([]any) {
		if o := it.(map[string]any); o["id"] == id {
			return o
		}
	}
	return nil
}

// RF-16: o pedido de implantação assistida é atendido pela equipe da
// plataforma (listar, agendar, concluir, cancelar), com auditoria na trilha
// da organização, e o valor entra no demonstrativo do mês da conclusão.
func TestAdminAddonOrdersOverHTTP(t *testing.T) {
	a := newAPI(t)
	tn, other := a.env.NewTenant(t, "admin-implantacao"), a.env.NewTenant(t, "admin-outra")
	owner := a.login(tn.Owner, &tn.ID)
	status, created, _ := owner.do(http.MethodPost, "/api/v1/platform/addon-orders", map[string]any{"addon_code": "implantacao_assistida",
		"notes": "Migrar contratos da planilha"})
	if status != http.StatusCreated || created["status"] != "requested" || created["billed"] != false {
		t.Fatalf("pedido do tenant: %d %v", status, created)
	}
	orderID := created["id"].(string)
	status, second, _ := a.login(other.Owner, &other.ID).do(http.MethodPost, "/api/v1/platform/addon-orders",
		map[string]any{"addon_code": "implantacao_assistida"})
	if status != http.StatusCreated {
		t.Fatalf("segundo pedido: %d %v", status, second)
	}

	// Somente a equipe da plataforma atende; um proprietário recebe 403.
	for _, path := range []string{"/api/v1/admin/addon-orders"} {
		if status, body, _ := owner.do(http.MethodGet, path, nil); status != http.StatusForbidden {
			t.Fatalf("proprietário na rota da equipe: %d %v", status, body)
		}
	}
	if status, _, _ := owner.do(http.MethodPost, "/api/v1/admin/tenants/"+tn.ID.String()+"/addon-orders/"+orderID+"/complete", nil); status != http.StatusForbidden {
		t.Fatalf("proprietário concluindo o próprio pedido: %d", status)
	}

	staff := a.login(staffUser(t, a), nil)
	status, list, _ := staff.do(http.MethodGet, "/api/v1/admin/addon-orders", nil)
	o := findOrder(t, list, orderID)
	if status != http.StatusOK || o == nil || o["tenant_id"] != tn.ID.String() || o["tenant_name"] != "Org admin-implantacao" ||
		o["addon_name"] != "Implantação assistida" || o["price"] != float64(250000) || o["notes"] != "Migrar contratos da planilha" {
		t.Fatalf("pedido na lista da equipe: %d %v", status, o)
	}
	if _, ok := list["next_cursor"]; !ok {
		t.Fatalf("a lista da equipe é paginada: %v", list)
	}
	if status, body, _ := staff.do(http.MethodGet, "/api/v1/admin/addon-orders?status=completed&cursor=invalido", nil); status != http.StatusUnprocessableEntity ||
		body["code"] != "invalid_cursor" {
		t.Fatalf("cursor inválido: %d %v", status, body)
	}
	if status, body, _ := staff.do(http.MethodGet, "/api/v1/admin/addon-orders?status=pago", nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("filtro inválido: %d %v", status, body)
	}

	base := "/api/v1/admin/tenants/" + tn.ID.String() + "/addon-orders/" + orderID
	noCSRF := &client{a: a, cookie: staff.cookie}
	if status, body, _ := noCSRF.do(http.MethodPost, base+"/complete", nil); status != http.StatusForbidden || body["code"] != "csrf_invalid" {
		t.Fatalf("atendimento sem CSRF: %d %v", status, body)
	}
	if status, body, _ := staff.do(http.MethodPost, base+"/schedule", map[string]any{"scheduled_for": a.env.Clock.Now().Add(-time.Hour)}); status != http.StatusUnprocessableEntity ||
		body["code"] != "invalid_addon_schedule" {
		t.Fatalf("agendamento no passado: %d %v", status, body)
	}
	at := a.env.Clock.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	status, scheduled, _ := staff.do(http.MethodPost, base+"/schedule", map[string]any{"scheduled_for": at})
	if status != http.StatusOK || scheduled["status"] != "scheduled" || scheduled["scheduled_for"] != at.Format(time.RFC3339) {
		t.Fatalf("agendamento: %d %v", status, scheduled)
	}
	// O tenant do caminho delimita o pedido.
	if status, body, _ := staff.do(http.MethodPost, "/api/v1/admin/tenants/"+other.ID.String()+"/addon-orders/"+orderID+"/complete", nil); status != http.StatusNotFound {
		t.Fatalf("pedido em outro tenant: %d %v", status, body)
	}
	a.env.Clock.Advance(10 * time.Minute)
	status, done, _ := staff.do(http.MethodPost, base+"/complete", nil)
	if status != http.StatusOK || done["status"] != "completed" || done["completed_at"] == nil || done["billed"] != false {
		t.Fatalf("conclusão: %d %v", status, done)
	}
	if status, body, _ := staff.do(http.MethodPost, base+"/cancel", map[string]any{"reason": "Tarde demais."}); status != http.StatusConflict ||
		body["code"] != "addon_order_closed" {
		t.Fatalf("cancelar pedido concluído: %d %v", status, body)
	}
	secondBase := "/api/v1/admin/tenants/" + other.ID.String() + "/addon-orders/" + second["id"].(string)
	if status, body, _ := staff.do(http.MethodPost, secondBase+"/cancel", map[string]any{"reason": "x"}); status != http.StatusUnprocessableEntity {
		t.Fatalf("cancelamento sem justificativa: %d %v", status, body)
	}
	if status, body, _ := staff.do(http.MethodPost, secondBase+"/cancel", map[string]any{"reason": "A organização desistiu."}); status != http.StatusOK ||
		body["status"] != "canceled" || body["cancel_reason"] != "A organização desistiu." {
		t.Fatalf("cancelamento: %d %v", status, body)
	}
	// Assinatura da plataforma cancelada: não há demonstrativo para cobrar, e
	// a conclusão é recusada.
	status, third, _ := a.login(other.Owner, &other.ID).do(http.MethodPost, "/api/v1/platform/addon-orders",
		map[string]any{"addon_code": "implantacao_assistida"})
	if status != http.StatusCreated {
		t.Fatalf("terceiro pedido: %d %v", status, third)
	}
	if _, err := pgtest.OwnerPool(t).Exec(context.Background(), `UPDATE tenant_platform_subscriptions SET status = 'canceled' WHERE tenant_id = $1`,
		other.ID); err != nil {
		t.Fatal(err)
	}
	thirdBase := "/api/v1/admin/tenants/" + other.ID.String() + "/addon-orders/" + third["id"].(string)
	if status, body, _ := staff.do(http.MethodPost, thirdBase+"/complete", nil); status != http.StatusConflict || body["code"] != "platform_subscription_canceled" {
		t.Fatalf("conclusão com a assinatura da plataforma cancelada: %d %v", status, body)
	}
	for filter, id := range map[string]string{"completed": orderID, "canceled": second["id"].(string)} {
		if _, body, _ := staff.do(http.MethodGet, "/api/v1/admin/addon-orders?status="+filter, nil); findOrder(t, body, id) == nil {
			t.Fatalf("filtro %s sem o pedido", filter)
		}
	}
	if _, body, _ := staff.do(http.MethodGet, "/api/v1/admin/addon-orders", nil); findOrder(t, body, orderID) != nil {
		t.Fatal("pedido concluído não está em aberto")
	}

	// O tenant vê o atendimento e a auditoria registra a equipe como autora.
	auditor := a.login(member(t, a.env, tn, tenancy.RoleAuditor), &tn.ID)
	if _, body, _ := auditor.do(http.MethodGet, "/api/v1/platform/addon-orders", nil); findOrder(t, body, orderID)["status"] != "completed" {
		t.Fatalf("pedido visto pelo tenant: %v", body)
	}
	var actions int
	if err := pgtest.OwnerPool(t).QueryRow(context.Background(), `SELECT count(*)::int FROM audit_log WHERE tenant_id = $1 AND resource_id = $2
		AND action IN ('platform.addon_scheduled', 'platform.addon_completed') AND actor_type = 'user' AND (details->>'platform_staff')::boolean`,
		tn.ID, orderID).Scan(&actions); err != nil || actions != 2 {
		t.Fatalf("auditoria do atendimento: %d %v", actions, err)
	}

	// Fechamento do mês: o valor entra no demonstrativo do período da conclusão.
	status, ov, _ := owner.do(http.MethodGet, "/api/v1/platform/overview", nil)
	if status != http.StatusOK {
		t.Fatal(status)
	}
	end, err := time.Parse(time.RFC3339Nano, ov["subscription"].(map[string]any)["current_period_end"].(string))
	if err != nil {
		t.Fatal(err)
	}
	a.env.Clock.Set(end.Add(time.Minute))
	if _, err := a.env.Platform.CloseDueStatements(context.Background()); err != nil {
		t.Fatal(err)
	}
	owner = a.login(tn.Owner, &tn.ID) // a sessão anterior expirou com o avanço do relógio
	status, sts, _ := owner.do(http.MethodGet, "/api/v1/platform/statements", nil)
	data := sts["data"].([]any)
	if status != http.StatusOK || len(data) != 1 {
		t.Fatalf("demonstrativos: %d %v", status, sts)
	}
	st := data[0].(map[string]any)
	if st["addons_amount"] != float64(250000) || st["total"] != st["subscription_amount"].(float64)+250000 {
		t.Fatalf("o serviço deve entrar no demonstrativo: %v", st)
	}
	if _, body, _ := owner.do(http.MethodGet, "/api/v1/platform/addon-orders", nil); findOrder(t, body, orderID)["billed"] != true {
		t.Fatalf("o pedido fica marcado como cobrado: %v", body)
	}
}
