//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/paysim"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// recoveryDispute leva pelo simulador e pelos webhooks da API uma cobrança de
// 100,00 paga com 125,00 (25,00 de crédito do excedente), usa 20,00 desse
// crédito em outra fatura do mesmo cliente e perde a disputa: 5,00 de
// crédito consumido e 20,00 a recuperar do cliente (ADR-070).
func (r *recvAPI) recoveryDispute(t *testing.T, tn fixture.Tenant) map[string]any {
	t.Helper()
	ctx := context.Background()
	inv := r.issueInvoice(t, tn, 10000)
	c, err := r.pay.RequestCharge(ctx, tn.ID, inv.ID, tn.Actor)
	if err != nil || c == nil {
		t.Fatalf("cobrança: %v", err)
	}
	r.simAction(t, c.ProviderChargeID, paysim.Action{Name: "pay", Amount: 12500})
	r.deliver(t, tn)
	plan := r.env.NewFlatPlan(t, tn, 2000)
	sub := r.env.NewSubscription(t, tn, inv.CustomerID, plan.ID, nil)
	if _, err := r.env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	pg, err := r.env.Billing.List(ctx, tn.ID, billing.ListFilter{SubscriptionID: &sub.ID, Limit: 10})
	if err != nil || len(pg.Items) != 1 || pg.Items[0].CreditApplied.Amount() != 2000 {
		t.Fatalf("crédito aplicado em outra fatura: %v %+v", err, pg.Items)
	}
	for _, stage := range []string{"requested", "lost"} {
		r.simAction(t, c.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: stage})
		r.deliver(t, tn)
	}
	owner := r.login(tn.Owner, &tn.ID)
	body, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/disputes", nil)
	for _, d := range objects(t, body["data"]) {
		if d["charge_id"] == c.ID.String() {
			if d["status"] != "lost" || d["recovery_amount"] != float64(2000) || d["recovery_outstanding_amount"] != float64(2000) ||
				d["recovery_received_amount"] != float64(0) || d["recovery_written_off_amount"] != float64(0) {
				t.Fatalf("disputa perdida: %v", d)
			}
			r.conforms(t, "Dispute", d)
			return d
		}
	}
	t.Fatalf("disputa da cobrança %s não listada: %v", c.ID, body)
	return nil
}

func settlementBody(outcome string, amount int64) map[string]any {
	return map[string]any{"outcome": outcome, "amount": amount, "justification": "Cliente pagou por transferência identificada no extrato."}
}

// A baixa pela API: recebida e perdida, parciais até zerar o saldo, com a
// resposta da disputa expondo quanto falta recuperar, a repetição pela
// Idempotency-Key, os conflitos de saldo, as permissões por papel e o
// isolamento entre organizações.
func TestRecoverySettlementAPI(t *testing.T) {
	r := newReceivablesAPI(t)
	tn := r.newTenant(t, "recuperar")
	owner := r.login(tn.Owner, &tn.ID)
	finance := r.login(member(t, r.env, tn, tenancy.RoleFinance), &tn.ID)
	operator := r.login(member(t, r.env, tn, tenancy.RoleOperator), &tn.ID)
	auditor := r.login(member(t, r.env, tn, tenancy.RoleAuditor), &tn.ID)
	d := r.recoveryDispute(t, tn)
	id := d["id"].(string)
	path := "/api/v1/disputes/" + id + "/recovery-settlements"

	got, _ := auditor.expect(http.StatusOK, http.MethodGet, "/api/v1/disputes/"+id, nil)
	r.conforms(t, "Dispute", got)
	if got["recovery_outstanding_amount"] != float64(2000) {
		t.Fatalf("disputa: %v", got)
	}
	empty, _ := auditor.expect(http.StatusOK, http.MethodGet, path, nil)
	if len(jsonArray(t, empty["data"])) != 0 {
		t.Fatalf("baixas antes de qualquer baixa: %v", empty)
	}
	// Permissões: operador e auditor não dão baixa; a chave é obrigatória.
	operator.expectProblem(http.StatusForbidden, "forbidden", http.MethodPost, path, settlementBody("received", 100), "Idempotency-Key", "op-"+uuid.NewString())
	auditor.expectProblem(http.StatusForbidden, "forbidden", http.MethodPost, path, settlementBody("received", 100), "Idempotency-Key", "au-"+uuid.NewString())
	finance.expectProblem(http.StatusUnprocessableEntity, "idempotency_key_required", http.MethodPost, path, settlementBody("received", 100))
	finance.expectProblem(http.StatusUnprocessableEntity, "invalid_recovery_settlement", http.MethodPost, path,
		map[string]any{"outcome": "received", "amount": 100, "justification": "curta"}, "Idempotency-Key", "inv-"+uuid.NewString())
	finance.expectProblem(http.StatusUnprocessableEntity, "invalid_recovery_settlement", http.MethodPost, path,
		map[string]any{"outcome": "received", "amount": 100, "justification": "Data efetiva no futuro.", "effective_date": "2099-01-01"},
		"Idempotency-Key", "fut-"+uuid.NewString())

	key := "baixa-recebida-" + uuid.NewString()
	received, _ := finance.expect(http.StatusCreated, http.MethodPost, path, settlementBody("received", 1500), "Idempotency-Key", key)
	r.conforms(t, "RecoverySettlement", received)
	if received["outcome"] != "received" || received["amount"] != float64(1500) || received["settled_by"] == nil || received["dispute_id"] != id {
		t.Fatalf("baixa recebida: %v", received)
	}
	again, h := finance.expect(http.StatusCreated, http.MethodPost, path, settlementBody("received", 1500), "Idempotency-Key", key)
	if again["id"] != received["id"] || h.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("repetição: %v %v", again, h)
	}
	finance.expectProblem(http.StatusConflict, "idempotency_key_reused", http.MethodPost, path, settlementBody("received", 1400), "Idempotency-Key", key)
	finance.expectProblem(http.StatusConflict, "recovery_exceeds_outstanding", http.MethodPost, path, settlementBody("written_off", 501),
		"Idempotency-Key", "acima-"+uuid.NewString())
	lost, _ := owner.expect(http.StatusCreated, http.MethodPost, path,
		map[string]any{"outcome": "written_off", "amount": 500, "justification": "Acordo comercial: restante perdoado.",
			"effective_date": tenantToday(t, r.env).String()}, "Idempotency-Key", "perda-"+uuid.NewString())
	r.conforms(t, "RecoverySettlement", lost)
	owner.expectProblem(http.StatusConflict, "recovery_already_settled", http.MethodPost, path, settlementBody("received", 1),
		"Idempotency-Key", "zerado-"+uuid.NewString())

	got, _ = auditor.expect(http.StatusOK, http.MethodGet, "/api/v1/disputes/"+id, nil)
	if got["recovery_received_amount"] != float64(1500) || got["recovery_written_off_amount"] != float64(500) ||
		got["recovery_outstanding_amount"] != float64(0) {
		t.Fatalf("disputa depois das baixas: %v", got)
	}
	list, _ := auditor.expect(http.StatusOK, http.MethodGet, path, nil)
	items := objects(t, list["data"])
	if len(items) != 2 || items[0]["id"] != received["id"] || items[1]["id"] != lost["id"] {
		t.Fatalf("baixas: %v", list)
	}
	for _, it := range items {
		r.conforms(t, "RecoverySettlement", it)
	}
	accounts := ledgerByAccount(t, owner)
	if acc := accounts["customer_recoverables"]; acc["debits"] != float64(2000) || acc["credits"] != float64(2000) {
		t.Fatalf("customer_recoverables: %v", acc)
	}
	if acc := accounts["bad_debt"]; acc["debits"] != float64(500) {
		t.Fatalf("bad_debt: %v", acc)
	}
	dash, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/dashboard", nil)
	r.conforms(t, "Dashboard", dash)
	if dash["open_recoverables"] != float64(0) || dash["chargebacks"] != float64(10000) {
		t.Fatalf("painel: %v", dash)
	}
	if n := r.count(t, tn, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'dispute.recovery_settled' AND actor_type = 'user'`); n != 2 {
		t.Fatalf("auditoria das baixas = %d", n)
	}

	// Outra organização não enxerga a disputa nem as baixas.
	other := r.newTenant(t, "recuperar-b")
	stranger := r.login(other.Owner, &other.ID)
	stranger.expectProblem(http.StatusNotFound, "dispute_not_found", http.MethodGet, "/api/v1/disputes/"+id, nil)
	stranger.expectProblem(http.StatusNotFound, "dispute_not_found", http.MethodGet, path, nil)
	stranger.expectProblem(http.StatusNotFound, "dispute_not_found", http.MethodPost, path, settlementBody("received", 1),
		"Idempotency-Key", "outro-"+uuid.NewString())
}

// Pela credencial de API com o escopo da baixa, a autoria fica nula e o ator
// api_credential vai para a auditoria; sem o escopo, a rota é recusada.
// Pedidos concorrentes nunca baixam mais que o saldo.
func TestRecoverySettlementAPIWithCredentialAndConcurrency(t *testing.T) {
	r := newReceivablesAPI(t)
	tn := r.newTenant(t, "recuperar-erp")
	owner := r.login(tn.Owner, &tn.ID)
	d := r.recoveryDispute(t, tn)
	path := "/api/v1/disputes/" + d["id"].(string) + "/recovery-settlements"
	_, _, readOnly := createCredential(t, owner, "payments.read")
	if res := r.raw(http.MethodPost, path, settlementBody("received", 100), bearer(readOnly, "Idempotency-Key", "sem-escopo-"+uuid.NewString())); res.status != http.StatusForbidden {
		t.Fatalf("credencial sem escopo: %d %v", res.status, res.body)
	}
	_, _, secret := createCredential(t, owner, "payments.read", "payments.settle_recovery")
	res := r.raw(http.MethodPost, path, settlementBody("received", 200), bearer(secret, "Idempotency-Key", "erp-"+uuid.NewString()))
	if res.status != http.StatusCreated || res.body["settled_by"] != nil {
		t.Fatalf("baixa por credencial: %d %v", res.status, res.body)
	}
	if n := r.count(t, tn, `SELECT count(*) FROM dispute_recovery_settlements WHERE tenant_id = $1 AND settled_by IS NULL`); n != 1 {
		t.Fatalf("baixa sem autoria = %d", n)
	}
	if n := r.count(t, tn, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'dispute.recovery_settled' AND actor_type = 'api_credential'`); n != 1 {
		t.Fatalf("auditoria pela credencial = %d", n)
	}

	// Saldo de 18,00: seis pedidos concorrentes de 5,00 resultam em três baixas.
	const n = 6
	type result struct {
		status int
		body   map[string]any
		err    error
	}
	out := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			out[i].status, out[i].body, out[i].err = owner.send(http.MethodPost, path, settlementBody("written_off", 500),
				"Idempotency-Key", fmt.Sprintf("concorrente-%d-%s", i, uuid.NewString()))
		})
	}
	close(start)
	wg.Wait()
	accepted := 0
	for _, res := range out {
		switch {
		case res.err != nil:
			t.Fatal(res.err)
		case res.status == http.StatusCreated:
			accepted++
		case res.status != http.StatusConflict || res.body["code"] != "recovery_exceeds_outstanding":
			t.Fatalf("resposta inesperada: %d %v", res.status, res.body)
		}
	}
	got, _ := owner.expect(http.StatusOK, http.MethodGet, "/api/v1/disputes/"+d["id"].(string), nil)
	if accepted != 3 || got["recovery_written_off_amount"] != float64(1500) || got["recovery_outstanding_amount"] != float64(300) {
		t.Fatalf("aceitas=%d disputa=%v", accepted, got)
	}
	if acc := ledgerByAccount(t, owner)["customer_recoverables"]; acc["debits"].(float64)-acc["credits"].(float64) != 300 {
		t.Fatalf("customer_recoverables: %v", acc)
	}
}
