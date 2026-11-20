package asaas

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/payments"
)

// Testes de contrato com servidor HTTP local seguindo os exemplos e campos
// da documentação oficial do Asaas (API v3). Não substituem a validação no
// sandbox, que exige chave de API e URL pública.

const testKey = "aact_chave_de_teste_123"

type recorded struct {
	method, path, query, token string
	body                       map[string]any
}

func server(t *testing.T, status int, response string) (*Gateway, *[]recorded) {
	t.Helper()
	var calls []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, token: r.Header.Get("access_token")}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			if err := json.Unmarshal(b, &rec.body); err != nil {
				t.Errorf("corpo não é JSON: %s", b)
			}
		}
		calls = append(calls, rec)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/v3", testKey, "financeiro@recorvia.test", srv.Client()), &calls
}

const refundedPayment = `{
  "object": "payment", "id": "pay_080225913252", "customer": "cus_000005219613", "value": 100.00, "netValue": 97.51,
  "billingType": "PIX", "status": "RECEIVED", "dueDate": "2026-11-21", "paymentDate": "2026-11-16",
  "invoiceUrl": "https://sandbox.asaas.com/i/080225913252", "externalReference": "0193a1b2-0000-7000-8000-000000000001", "deleted": false,
  "refunds": [
    {"dateCreated": "2026-11-17 10:00:00", "status": "DONE", "value": 25.50, "description": "Cancelamento parcial"},
    {"dateCreated": "2026-11-17 11:00:00", "status": "PENDING", "value": 10.00, "description": "Em processamento"},
    {"dateCreated": "2026-11-17 12:00:00", "status": "CANCELLED", "value": 5.00, "description": "Recusado"}
  ]
}`

func TestRefundFollowsDocumentedContract(t *testing.T) {
	gw, calls := server(t, http.StatusOK, refundedPayment)
	pr, err := gw.Refund(context.Background(), payments.RefundRequest{ProviderChargeID: "pay_080225913252", Amount: money.New(2550, money.BRL),
		Description: "Cancelamento parcial", ExternalReference: "ref-local"})
	if err != nil {
		t.Fatal(err)
	}
	c := (*calls)[0]
	if c.method != http.MethodPost || c.path != "/v3/payments/pay_080225913252/refund" || c.token != testKey {
		t.Fatalf("requisição: %+v", c)
	}
	if v, ok := c.body["value"].(float64); !ok || v != 25.5 || c.body["description"] != "Cancelamento parcial" || len(c.body) != 2 {
		t.Fatalf("corpo do estorno: %+v", c.body)
	}
	// Sem identificador do estorno: o reembolso fica pendente e o retrato da
	// cobrança acompanha a resposta.
	if pr.ProviderRefundID != "" || pr.State != payments.RefundStatePending || pr.Status != "RECEIVED" || pr.Charge == nil {
		t.Fatalf("reembolso: %+v", pr)
	}
	pc := pr.Charge
	if pc.RefundedAmount.Amount() != 2550 || pc.Status != payments.ProviderPartiallyRefunded || len(pc.Refunds) != 3 {
		t.Fatalf("retrato: %+v", pc)
	}
	want := []payments.RefundState{payments.RefundStateDone, payments.RefundStatePending, payments.RefundStateFailed}
	for i, r := range pc.Refunds {
		if r.State != want[i] || r.ProviderRefundID != "" || r.ExternalReference != "" {
			t.Fatalf("estorno %d: %+v", i, r)
		}
	}
	if pc.Refunds[1].Amount.Amount() != 1000 {
		t.Fatalf("valor decimal: %v", pc.Refunds[1].Amount)
	}
}

func TestRefundErrorsAreClassified(t *testing.T) {
	cases := []struct {
		status int
		kind   payments.ErrorKind
	}{
		{http.StatusBadRequest, payments.ErrKindPermanent},
		{http.StatusNotFound, payments.ErrKindNotFound},
		{http.StatusTooManyRequests, payments.ErrKindRetryable},
		{http.StatusInternalServerError, payments.ErrKindAmbiguous},
	}
	for _, tc := range cases {
		gw, _ := server(t, tc.status, `{"errors":[{"code":"invalid_action","description":"Não é possível estornar."}]}`)
		_, err := gw.Refund(context.Background(), payments.RefundRequest{ProviderChargeID: "pay_1", Amount: money.New(100, money.BRL)})
		if payments.GatewayErrorKind(err) != tc.kind {
			t.Fatalf("status %d: %v", tc.status, err)
		}
	}
	gw, _ := server(t, http.StatusOK, `{"id":"pay_1","value":"abc"}`)
	if _, err := gw.Refund(context.Background(), payments.RefundRequest{ProviderChargeID: "pay_1", Amount: money.New(100, money.BRL)}); payments.GatewayErrorKind(err) != payments.ErrKindAmbiguous {
		t.Fatalf("resposta ilegível: %v", err)
	}
}

func TestCancelChargeFollowsDocumentedContract(t *testing.T) {
	gw, calls := server(t, http.StatusOK, `{"deleted": true, "id": "pay_pCczZjBBr6RL"}`)
	if err := gw.CancelCharge(context.Background(), "pay_pCczZjBBr6RL"); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.method != http.MethodDelete || c.path != "/v3/payments/pay_pCczZjBBr6RL" || c.body != nil || c.token != testKey {
		t.Fatalf("requisição: %+v", c)
	}
	gw, _ = server(t, http.StatusOK, `{"deleted": false, "id": "pay_1"}`)
	if err := gw.CancelCharge(context.Background(), "pay_1"); payments.GatewayErrorKind(err) != payments.ErrKindAmbiguous {
		t.Fatalf("exclusão não confirmada: %v", err)
	}
	gw, _ = server(t, http.StatusNotFound, ``)
	if err := gw.CancelCharge(context.Background(), "pay_1"); payments.GatewayErrorKind(err) != payments.ErrKindNotFound {
		t.Fatalf("inexistente: %v", err)
	}
	gw, _ = server(t, http.StatusBadRequest, `{"errors":[{"code":"invalid_action","description":"Cobrança já recebida."}]}`)
	if err := gw.CancelCharge(context.Background(), "pay_1"); payments.GatewayErrorKind(err) != payments.ErrKindPermanent {
		t.Fatalf("recusa: %v", err)
	}
}

func TestListChargesFollowsDocumentedContract(t *testing.T) {
	gw, calls := server(t, http.StatusOK, `{
  "object": "list", "hasMore": true, "totalCount": 3, "limit": 100, "offset": 0,
  "data": [
    {"object": "payment", "id": "pay_1", "customer": "cus_1", "value": 99.90, "netValue": 96.91, "status": "CONFIRMED",
     "confirmedDate": "2026-11-10", "externalReference": "ref-1", "invoiceUrl": "https://sandbox.asaas.com/i/1"},
    {"object": "payment", "id": "pay_2", "customer": "cus_1", "value": 50.00, "status": "PENDING", "deleted": true},
    {"object": "payment", "id": "pay_3", "customer": "cus_2", "value": 10.00, "status": "CHARGEBACK_DISPUTE"}
  ]
}`)
	from := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	res, err := gw.ListCharges(context.Background(), payments.ListRequest{UpdatedFrom: from, UpdatedTo: to, Offset: 200, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	c := (*calls)[0]
	if c.method != http.MethodGet || c.path != "/v3/payments" ||
		c.query != "dateCreated%5Bge%5D=2026-11-01&dateCreated%5Ble%5D=2026-11-17&limit=100&offset=200" {
		t.Fatalf("requisição: %+v", c)
	}
	if !res.HasMore || len(res.Charges) != 3 {
		t.Fatalf("página: %+v", res)
	}
	p1 := res.Charges[0]
	if p1.Amount.Amount() != 9990 || p1.FeeOrDerived().Amount() != 299 || p1.Status != payments.ProviderConfirmed || p1.PaidAt == nil ||
		p1.ExternalReference != "ref-1" {
		t.Fatalf("cobrança paga: %+v", p1)
	}
	if res.Charges[1].Status != payments.ProviderCanceled || res.Charges[2].Status != payments.ProviderChargebackDispute {
		t.Fatalf("situações: %s %s", res.Charges[1].Status, res.Charges[2].Status)
	}
	gw, _ = server(t, http.StatusOK, `{"object":"list","hasMore":false,"data":[{"id":"pay_9","value":"x"}]}`)
	if _, err := gw.ListCharges(context.Background(), payments.ListRequest{}); err == nil {
		t.Fatal("valor inválido na listagem")
	}
	gw, _ = server(t, http.StatusServiceUnavailable, `{}`)
	if _, err := gw.ListCharges(context.Background(), payments.ListRequest{}); payments.GatewayErrorKind(err) != payments.ErrKindRetryable {
		t.Fatalf("indisponível: %v", err)
	}
}

func TestGetChargeWithRefunds(t *testing.T) {
	gw, calls := server(t, http.StatusOK, `{"id":"pay_1","value":100,"status":"REFUNDED","refunds":[{"status":"DONE","value":100}]}`)
	pc, err := gw.GetCharge(context.Background(), "pay_1")
	if err != nil || pc.Status != payments.ProviderRefunded || pc.RefundedAmount.Amount() != 10000 || len(pc.Refunds) != 1 {
		t.Fatalf("cobrança reembolsada: %+v %v", pc, err)
	}
	if (*calls)[0].path != "/v3/payments/pay_1" {
		t.Fatalf("requisição: %+v", (*calls)[0])
	}
	gw, _ = server(t, http.StatusOK, `{"id":"pay_1","value":100,"status":"RECEIVED","refunds":[{"status":"DONE","value":"x"}]}`)
	if _, err := gw.GetCharge(context.Background(), "pay_1"); err == nil {
		t.Fatal("estorno com valor inválido")
	}
}

func TestRefundStateMapping(t *testing.T) {
	for status, want := range map[string]payments.RefundState{"DONE": payments.RefundStateDone, "PENDING": payments.RefundStatePending,
		"AWAITING_CRITICAL_ACTION_AUTHORIZATION": payments.RefundStatePending, "AWAITING_CUSTOMER_EXTERNAL_AUTHORIZATION": payments.RefundStatePending,
		"CANCELLED": payments.RefundStateFailed} {
		if got := RefundState(status); got != want {
			t.Fatalf("%s: %s", status, got)
		}
	}
}

func TestWebhookRefundEventsCarryRefunds(t *testing.T) {
	body := []byte(`{"id":"evt_05b708f961d739ea7eba7e4db318f621&368604920","event":"PAYMENT_PARTIALLY_REFUNDED","dateCreated":"2026-11-17 10:00:00",
	  "payment":{"object":"payment","id":"pay_1","value":100.00,"status":"RECEIVED","refunds":[{"status":"DONE","value":30.00}]}}`)
	ev, err := Verifier{}.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != payments.ProviderPartiallyRefunded || ev.Charge == nil || ev.Charge.RefundedAmount.Amount() != 3000 || len(ev.Charge.Refunds) != 1 ||
		ev.OccurredAt == nil {
		t.Fatalf("evento: %+v", ev)
	}
	ev, err = Verifier{}.Parse([]byte(`{"id":"evt_2","event":"PAYMENT_DELETED","payment":{"id":"pay_1","value":10,"status":"PENDING","deleted":true}}`))
	if err != nil || ev.Status != payments.ProviderCanceled {
		t.Fatalf("exclusão: %+v %v", ev, err)
	}
}
