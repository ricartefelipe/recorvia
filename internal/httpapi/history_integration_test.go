//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// failingSender recusa todos os envios com erro temporário.
type failingSender struct{}

func (failingSender) Send(context.Context, notifications.Email) (notifications.Receipt, error) {
	return notifications.Receipt{}, errors.New("421 serviço temporariamente indisponível")
}

// Histórico de envio (RF-13): a listagem traz o motivo da situação atual e a
// rota de eventos expõe a linha do tempo gravada, com a mesma permissão de
// leitura da listagem e isolamento entre tenants.
func TestCollectionsAPINotificationReasonsAndEvents(t *testing.T) {
	a := newDunningAPI(t)
	ctx := context.Background()
	tn := a.env.NewTenant(t, "hist-eventos")
	owner := a.login(tn.Owner, &tn.ID)
	plan := a.env.NewFlatPlan(t, tn, 45000)
	inv := billedInvoice(t, a.env, tn, plan.ID, "eventos@cliente.test")
	if err := a.notif.HandleInvoiceIssued(ctx, tn.ID, inv, ids.New()); err != nil {
		t.Fatal(err)
	}
	out, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/notifications?invoice_id="+inv.String(), nil)
	items := objects(t, out["data"])
	if len(items) != 1 || items[0]["status"] != "scheduled" || items[0]["status_reason"] != "" {
		t.Fatalf("agendada sem motivo: %v", items)
	}
	id := items[0]["id"].(string)

	sender := notifications.NewService(a.env.Store.Notifications(), failingSender{}, a.env.Keyring, a.env.Clock,
		notifications.Config{Lease: time.Minute, Jitter: func() float64 { return 0.5 }})
	// O banco é compartilhado com outros testes: o envio vale para todos os
	// tenants, e o resultado é conferido na notificação deste.
	if res, err := sender.Dispatch(ctx); err != nil || res.Retried == 0 {
		t.Fatalf("tentativa com falha: %+v %v", res, err)
	}
	out, _ = owner.expect(http.StatusOK, http.MethodGet, "/api/v1/notifications?invoice_id="+inv.String(), nil)
	if n := objects(t, out["data"])[0]; n["status"] != "scheduled" || n["status_reason"] != notifications.ReasonRetryScheduled || n["attempts"] != float64(1) {
		t.Fatalf("nova tentativa agendada: %v", n)
	}

	events, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/notifications/"+id+"/events", nil)
	evs := objects(t, events["data"])
	want := []struct {
		status, reason string
		attempt        float64
	}{{"scheduled", "scheduled", 0}, {"sending", "attempt", 1}, {"scheduled", "retry_scheduled", 1}}
	if len(evs) != len(want) {
		t.Fatalf("linha do tempo: %v", evs)
	}
	for i, w := range want {
		if evs[i]["status"] != w.status || evs[i]["reason"] != w.reason || evs[i]["attempt"] != w.attempt || evs[i]["occurred_at"] == nil {
			t.Fatalf("evento %d: %v", i, evs[i])
		}
	}
	if evs[0]["detail"] != "" || evs[2]["detail"] != "421 serviço temporariamente indisponível" {
		t.Fatalf("detalhes: %v", evs)
	}

	owner.expectProblem(404, "notification_not_found", http.MethodGet, "/api/v1/notifications/"+uuid.NewString()+"/events", nil)
	owner.expectProblem(404, "not_found", http.MethodGet, "/api/v1/notifications/abc/events", nil)
	for _, role := range []tenancy.Role{tenancy.RoleAuditor, tenancy.RoleOperator} {
		c := a.login(member(t, a.env, tn, role), &tn.ID)
		if out, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/notifications/"+id+"/events", nil); len(objects(t, out["data"])) != 3 {
			t.Fatalf("%s lê a linha do tempo: %v", role, out)
		}
	}
	other := a.env.NewTenant(t, "hist-eventos-b")
	a.login(other.Owner, &other.ID).expectProblem(404, "notification_not_found", http.MethodGet, "/api/v1/notifications/"+id+"/events", nil)
}

// Tentativas individuais das entregas de webhooks enviados (RF-11): cada
// tentativa com código de resposta, duração e assinaturas; leitura com
// webhooks.read e isolamento entre tenants.
func TestWebhooksAPIDeliveryAttempts(t *testing.T) {
	a := newDunningAPI(t)
	ctx := context.Background()
	tn := proTenant(t, a.env, "wh-tentativas")
	owner := a.login(tn.Owner, &tn.ID)
	created, _ := owner.expect(http.StatusCreated, http.MethodPost, "/api/v1/webhook-endpoints",
		map[string]any{"url": "https://hooks.cliente.test/tentativas", "event_types": []string{outbox.EventInvoicePaid}})
	endpointID := created["endpoint"].(map[string]any)["id"].(string)
	payload, _ := json.Marshal(map[string]any{"invoice_id": ids.New()})
	if err := a.webhooks.HandleEvent(ctx, tn.ID, ids.New(), outbox.EventInvoicePaid, a.env.Clock.Now(), payload); err != nil {
		t.Fatal(err)
	}
	out, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/webhook-endpoints/"+endpointID+"/deliveries", nil)
	deliveryID := objects(t, out["data"])[0]["id"].(string)
	path := "/api/v1/webhook-deliveries/" + deliveryID + "/attempts"
	if out, _ := owner.expect(http.StatusOK, http.MethodGet, path, nil); len(objects(t, out["data"])) != 0 {
		t.Fatalf("entrega sem tentativas: %v", out)
	}

	// Duas tentativas com resposta 503 (o transporte do teste sempre responde 503).
	for range 2 {
		if res, err := a.webhooks.DeliverTenant(ctx, tn.ID); err != nil || res.Retrying != 1 {
			t.Fatalf("tentativa: %+v %v", res, err)
		}
		d, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/webhook-endpoints/"+endpointID+"/deliveries", nil)
		a.env.Clock.Set(instant(t, objects(t, d["data"])[0]["next_attempt_at"]))
	}
	out, _ = owner.expect(http.StatusOK, http.MethodGet, path, nil)
	attempts := objects(t, out["data"])
	if len(attempts) != 2 {
		t.Fatalf("tentativas: %v", attempts)
	}
	for i, at := range attempts {
		if at["attempt"] != float64(i+1) || at["status_code"] != float64(503) || at["signature_count"] != float64(1) ||
			at["duration_ms"] == nil || at["started_at"] == nil || at["error"] == "" {
			t.Fatalf("tentativa %d: %v", i+1, at)
		}
	}
	if !instant(t, attempts[0]["started_at"]).Before(instant(t, attempts[1]["started_at"])) {
		t.Fatalf("ordem das tentativas: %v", attempts)
	}

	a.login(member(t, a.env, tn, tenancy.RoleAuditor), &tn.ID).expect(http.StatusOK, http.MethodGet, path, nil)
	a.login(member(t, a.env, tn, tenancy.RoleOperator), &tn.ID).expectProblem(403, "forbidden", http.MethodGet, path, nil)
	owner.expectProblem(404, "webhook_delivery_not_found", http.MethodGet, "/api/v1/webhook-deliveries/"+uuid.NewString()+"/attempts", nil)
	other := proTenant(t, a.env, "wh-tentativas-b")
	a.login(other.Owner, &other.ID).expectProblem(404, "webhook_delivery_not_found", http.MethodGet, path, nil)

	// Endpoint removido: as entregas deixam de ser consultáveis.
	owner.expect(http.StatusNoContent, http.MethodDelete, "/api/v1/webhook-endpoints/"+endpointID, nil)
	owner.expectProblem(404, "webhook_delivery_not_found", http.MethodGet, path, nil)
}
