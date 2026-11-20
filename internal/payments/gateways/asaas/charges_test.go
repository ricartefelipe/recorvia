package asaas

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/payments"
)

// Testes de contrato da criação de cliente e cobrança, da consulta por
// referência externa, do registro do webhook e da autenticação dos eventos,
// com respostas no formato da documentação oficial do Asaas (API v3).

type reply struct {
	status int
	body   string
}

// routed responde por método e caminho e grava as requisições recebidas.
func routed(t *testing.T, webhookEmail string, routes map[string]reply) (*Gateway, *[]recorded) {
	t.Helper()
	var calls []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, token: r.Header.Get("access_token")}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			if err := json.Unmarshal(b, &rec.body); err != nil {
				t.Errorf("corpo não é JSON: %s", b)
			}
		}
		if ua := r.Header.Get("User-Agent"); ua != "Recorvia" {
			t.Errorf("User-Agent %q", ua)
		}
		calls = append(calls, rec)
		res, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			t.Errorf("rota inesperada: %s %s", r.Method, r.URL.Path)
			res = reply{status: http.StatusNotFound}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.status)
		_, _ = io.WriteString(w, res.body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/v3", testKey, webhookEmail, srv.Client()), &calls
}

func TestBaseURL(t *testing.T) {
	for env, want := range map[string]string{"sandbox": SandboxURL, "production": ProductionURL} {
		if got, err := BaseURL(env); err != nil || got != want {
			t.Fatalf("%s: %q %v", env, got, err)
		}
	}
	if _, err := BaseURL("local"); err == nil {
		t.Fatal("ambiente desconhecido aceito")
	}
}

func TestEnsureCustomerFollowsDocumentedContract(t *testing.T) {
	cust := payments.CustomerData{ID: uuid.MustParse("0193a1b2-0000-7000-8000-0000000000c1"), Name: "Cliente Exemplo Ltda",
		DocumentNumber: "11222333000181", Email: "financeiro@cliente.test"}
	t.Run("cliente existente pela referência externa", func(t *testing.T) {
		gw, calls := routed(t, "", map[string]reply{
			"GET /v3/customers": {http.StatusOK, `{"object":"list","hasMore":false,"totalCount":1,"data":[{"object":"customer","id":"cus_000005219613"}]}`},
		})
		id, err := gw.EnsureCustomer(context.Background(), cust)
		if err != nil || id != "cus_000005219613" {
			t.Fatalf("cliente: %q %v", id, err)
		}
		if c := (*calls)[0]; len(*calls) != 1 || c.query != "externalReference="+cust.ID.String() || c.token != testKey {
			t.Fatalf("requisições: %+v", *calls)
		}
	})
	t.Run("cria o cliente com documento, e-mail e referência", func(t *testing.T) {
		gw, calls := routed(t, "", map[string]reply{
			"GET /v3/customers":  {http.StatusOK, `{"object":"list","hasMore":false,"totalCount":0,"data":[]}`},
			"POST /v3/customers": {http.StatusOK, `{"object":"customer","id":"cus_000005219614","name":"Cliente Exemplo Ltda"}`},
		})
		id, err := gw.EnsureCustomer(context.Background(), cust)
		if err != nil || id != "cus_000005219614" || len(*calls) != 2 {
			t.Fatalf("cliente criado: %q %v %+v", id, err, *calls)
		}
		body := (*calls)[1].body
		if body["name"] != cust.Name || body["cpfCnpj"] != cust.DocumentNumber || body["externalReference"] != cust.ID.String() ||
			body["email"] != cust.Email || len(body) != 4 {
			t.Fatalf("corpo do cliente: %+v", body)
		}
	})
	t.Run("sem e-mail o campo não é enviado", func(t *testing.T) {
		gw, calls := routed(t, "", map[string]reply{
			"GET /v3/customers":  {http.StatusOK, `{"data":[]}`},
			"POST /v3/customers": {http.StatusOK, `{"id":"cus_1"}`},
		})
		noEmail := cust
		noEmail.Email = ""
		if _, err := gw.EnsureCustomer(context.Background(), noEmail); err != nil {
			t.Fatal(err)
		}
		if _, ok := (*calls)[1].body["email"]; ok {
			t.Fatalf("e-mail vazio enviado: %+v", (*calls)[1].body)
		}
	})
	t.Run("recusa do provedor é permanente", func(t *testing.T) {
		gw, _ := routed(t, "", map[string]reply{
			"GET /v3/customers":  {http.StatusOK, `{"data":[]}`},
			"POST /v3/customers": {http.StatusBadRequest, `{"errors":[{"code":"invalid_cpfCnpj","description":"CPF/CNPJ inválido."}]}`},
		})
		if _, err := gw.EnsureCustomer(context.Background(), cust); payments.GatewayErrorKind(err) != payments.ErrKindPermanent {
			t.Fatalf("recusa: %v", err)
		}
	})
}

const createdPayment = `{
  "object": "payment", "id": "pay_080225913252", "dateCreated": "2026-11-16", "customer": "cus_000005219613",
  "value": 159.90, "netValue": 155.91, "billingType": "UNDEFINED", "status": "PENDING", "dueDate": "2026-11-26",
  "invoiceUrl": "https://sandbox.asaas.com/i/080225913252", "externalReference": "0193a1b2-0000-7000-8000-0000000000a1",
  "deleted": false
}`

func TestCreateChargeFollowsDocumentedContract(t *testing.T) {
	gw, calls := routed(t, "", map[string]reply{"POST /v3/payments": {http.StatusOK, createdPayment}})
	due, err := calendar.NewDate(2026, time.November, 26)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := gw.CreateCharge(context.Background(), payments.CreateChargeRequest{ExternalReference: "0193a1b2-0000-7000-8000-0000000000a1",
		Amount: money.New(15990, money.BRL), DueDate: due, Description: "Fatura ACME-2026-000001",
		Customer: payments.CustomerData{ProviderCustomerID: "cus_000005219613"}})
	if err != nil {
		t.Fatal(err)
	}
	body := (*calls)[0].body
	// O valor vai como número decimal em reais; billingType UNDEFINED deixa a
	// escolha do meio ao pagador na fatura hospedada.
	if body["customer"] != "cus_000005219613" || body["billingType"] != "UNDEFINED" || body["value"] != 159.9 ||
		body["dueDate"] != "2026-11-26" || body["description"] != "Fatura ACME-2026-000001" ||
		body["externalReference"] != "0193a1b2-0000-7000-8000-0000000000a1" || len(body) != 6 {
		t.Fatalf("corpo da cobrança: %+v", body)
	}
	if pc.ProviderChargeID != "pay_080225913252" || pc.Status != payments.ProviderPending || pc.RawStatus != "PENDING" ||
		pc.Amount.Amount() != 15990 || pc.CheckoutURL != "https://sandbox.asaas.com/i/080225913252" || pc.PaidAt != nil ||
		pc.ExternalReference != "0193a1b2-0000-7000-8000-0000000000a1" || pc.ProviderCustomerID != "cus_000005219613" {
		t.Fatalf("cobrança: %+v", pc)
	}
	if fee := pc.FeeOrDerived(); fee.Amount() != 399 {
		t.Fatalf("taxa derivada de netValue: %v", fee)
	}

	gw, _ = routed(t, "", map[string]reply{"POST /v3/payments": {http.StatusOK, `{"id":"pay_1","value":"cento e dez"}`}})
	if _, err := gw.CreateCharge(context.Background(), payments.CreateChargeRequest{Amount: money.New(100, money.BRL), DueDate: due}); err == nil {
		t.Fatal("valor ilegível aceito")
	}
}

func TestFindByExternalReference(t *testing.T) {
	gw, calls := routed(t, "", map[string]reply{"GET /v3/payments": {http.StatusOK, `{"object":"list","hasMore":false,"data":[` + createdPayment + `]}`}})
	pc, err := gw.FindByExternalReference(context.Background(), "0193a1b2-0000-7000-8000-0000000000a1")
	if err != nil || pc == nil || pc.ProviderChargeID != "pay_080225913252" {
		t.Fatalf("cobrança encontrada: %+v %v", pc, err)
	}
	if q := (*calls)[0].query; q != "externalReference=0193a1b2-0000-7000-8000-0000000000a1" {
		t.Fatalf("filtro: %s", q)
	}
	gw, _ = routed(t, "", map[string]reply{"GET /v3/payments": {http.StatusOK, `{"object":"list","hasMore":false,"data":[]}`}})
	if pc, err := gw.FindByExternalReference(context.Background(), "ref"); err != nil || pc != nil {
		t.Fatalf("sem cobrança: %+v %v", pc, err)
	}
	gw, _ = routed(t, "", map[string]reply{"GET /v3/payments": {http.StatusUnauthorized, `{"errors":[{"code":"invalid_access_token"}]}`}})
	if _, err := gw.FindByExternalReference(context.Background(), "ref"); err == nil {
		t.Fatal("credencial recusada sem erro")
	}
}

func TestConfigureWebhookFollowsDocumentedContract(t *testing.T) {
	gw, calls := routed(t, "financeiro@recorvia.test", map[string]reply{
		"POST /v3/webhooks": {http.StatusOK, `{"id":"wh_1","name":"Recorvia","enabled":true,"interrupted":false}`},
	})
	url := "https://recorvia.example.test/webhooks/providers/asaas/token-opaco"
	secret := "token-do-webhook-com-32-caracteres-ou-mais"
	if err := gw.ConfigureWebhook(context.Background(), url, secret); err != nil {
		t.Fatal(err)
	}
	body := (*calls)[0].body
	if body["url"] != url || body["authToken"] != secret || body["email"] != "financeiro@recorvia.test" || body["enabled"] != true ||
		body["interrupted"] != false || body["apiVersion"] != float64(3) || body["sendType"] != "SEQUENTIALLY" || body["name"] != "Recorvia" {
		t.Fatalf("corpo do webhook: %+v", body)
	}
	events, _ := body["events"].([]any)
	if len(events) != len(Events) {
		t.Fatalf("eventos assinados: %v", body["events"])
	}
	for _, required := range []string{"PAYMENT_RECEIVED", "PAYMENT_DELETED", "PAYMENT_PARTIALLY_REFUNDED", "PAYMENT_REFUNDED",
		"PAYMENT_CHARGEBACK_REQUESTED", "PAYMENT_AWAITING_CHARGEBACK_REVERSAL"} {
		found := false
		for _, e := range events {
			found = found || e == required
		}
		if !found {
			t.Fatalf("evento %s não assinado", required)
		}
	}

	noEmail, _ := routed(t, "", map[string]reply{})
	if err := noEmail.ConfigureWebhook(context.Background(), url, secret); err == nil {
		t.Fatal("webhook sem e-mail de notificação")
	}
	gw, _ = routed(t, "financeiro@recorvia.test", map[string]reply{
		"POST /v3/webhooks": {http.StatusUnauthorized, `{"errors":[{"code":"invalid_access_token","description":"Chave inválida."}]}`},
	})
	if err := gw.ConfigureWebhook(context.Background(), url, secret); err == nil {
		t.Fatal("chave recusada sem erro")
	}
}

func TestVerifierAuthenticatesByToken(t *testing.T) {
	secret := "token-do-webhook-com-32-caracteres-ou-mais"
	header := func(v string) http.Header {
		h := http.Header{}
		if v != "" {
			h.Set(AuthHeader, v)
		}
		return h
	}
	cases := []struct {
		name   string
		header http.Header
		secret string
		ok     bool
	}{
		{"token correto", header(secret), secret, true},
		{"token divergente", header(secret + "x"), secret, false},
		{"sem header", header(""), secret, false},
		{"conta sem segredo", header(secret), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verifier{}.Verify(tc.header, []byte(`{}`), tc.secret, time.Now())
			if tc.ok != (err == nil) || (err != nil && !errors.Is(err, ErrToken)) {
				t.Fatalf("verificação: %v", err)
			}
		})
	}
}

func TestStatusMapping(t *testing.T) {
	for raw, want := range map[string]payments.ProviderStatus{
		"PENDING": payments.ProviderPending, "AWAITING_RISK_ANALYSIS": payments.ProviderPending, "DUNNING_REQUESTED": payments.ProviderPending,
		"OVERDUE": payments.ProviderOverdue, "CONFIRMED": payments.ProviderConfirmed, "RECEIVED": payments.ProviderReceived,
		"RECEIVED_IN_CASH": payments.ProviderReceived, "DUNNING_RECEIVED": payments.ProviderReceived,
		"REFUND_REQUESTED": payments.ProviderRefundInProgress, "REFUND_IN_PROGRESS": payments.ProviderRefundInProgress,
		"REFUNDED": payments.ProviderRefunded, "CHARGEBACK_REQUESTED": payments.ProviderChargebackRequested,
		"CHARGEBACK_DISPUTE": payments.ProviderChargebackDispute, "AWAITING_CHARGEBACK_REVERSAL": payments.ProviderChargebackReversed,
		"NOVA_SITUACAO": payments.ProviderUnknownStatus,
	} {
		if got := MapStatus(raw, false); got != want {
			t.Fatalf("%s: %s, esperado %s", raw, got, want)
		}
	}
	if MapStatus("RECEIVED", true) != payments.ProviderCanceled {
		t.Fatal("cobrança excluída deve ser cancelada")
	}
	for event, want := range map[string]payments.ProviderStatus{
		"PAYMENT_CREATED": payments.ProviderPending, "PAYMENT_UPDATED": payments.ProviderPending, "PAYMENT_RESTORED": payments.ProviderPending,
		"PAYMENT_AWAITING_RISK_ANALYSIS": payments.ProviderPending, "PAYMENT_APPROVED_BY_RISK_ANALYSIS": payments.ProviderPending,
		"PAYMENT_CONFIRMED": payments.ProviderConfirmed, "PAYMENT_RECEIVED": payments.ProviderReceived,
		"PAYMENT_OVERDUE": payments.ProviderOverdue, "PAYMENT_DELETED": payments.ProviderCanceled,
		"PAYMENT_REFUNDED": payments.ProviderRefunded, "PAYMENT_PARTIALLY_REFUNDED": payments.ProviderPartiallyRefunded,
		"PAYMENT_REFUND_IN_PROGRESS": payments.ProviderRefundInProgress, "PAYMENT_CHARGEBACK_REQUESTED": payments.ProviderChargebackRequested,
		"PAYMENT_CHARGEBACK_DISPUTE":           payments.ProviderChargebackDispute,
		"PAYMENT_AWAITING_CHARGEBACK_REVERSAL": payments.ProviderChargebackReversed,
		"PAYMENT_REPROVED_BY_RISK_ANALYSIS":    payments.ProviderFailed, "PAYMENT_CREDIT_CARD_CAPTURE_REFUSED": payments.ProviderFailed,
		"PAYMENT_REFUND_DENIED": payments.ProviderUnknownStatus,
	} {
		if got := eventStatus(event); got != want {
			t.Fatalf("%s: %s, esperado %s", event, got, want)
		}
	}
}

func TestParseRejectsMalformedEvents(t *testing.T) {
	for name, body := range map[string]string{
		"JSON inválido":          `{`,
		"sem identificador":      `{"event":"PAYMENT_RECEIVED"}`,
		"sem tipo":               `{"id":"evt_1"}`,
		"valor ilegível":         `{"id":"evt_1","event":"PAYMENT_RECEIVED","payment":{"id":"pay_1","value":"x"}}`,
		"estorno com valor ruim": `{"id":"evt_1","event":"PAYMENT_REFUNDED","payment":{"id":"pay_1","value":10,"refunds":[{"status":"DONE","value":"x"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (Verifier{}).Parse([]byte(body)); err == nil {
				t.Fatal("evento malformado aceito")
			}
		})
	}
	// Evento sem cobrança (por exemplo, de outro recurso) não carrega retrato.
	ev, err := Verifier{}.Parse([]byte(`{"id":"evt_2","event":"PAYMENT_RECEIVED","dateCreated":"2026-11-17 10:00:00"}`))
	if err != nil || ev.Charge != nil || ev.ProviderChargeID != "" || ev.OccurredAt == nil {
		t.Fatalf("evento sem cobrança: %+v %v", ev, err)
	}
	// Situação de evento desconhecida mantém a do objeto.
	ev, err = Verifier{}.Parse([]byte(`{"id":"evt_3","event":"PAYMENT_BANK_SLIP_VIEWED","payment":{"id":"pay_1","value":10,"status":"RECEIVED",
	  "paymentDate":"2026-11-17"}}`))
	if err != nil || ev.Status != payments.ProviderUnknownStatus || ev.Charge == nil || ev.Charge.Status != payments.ProviderReceived ||
		ev.Charge.PaidAt == nil {
		t.Fatalf("evento informativo: %+v %v", ev, err)
	}
}
