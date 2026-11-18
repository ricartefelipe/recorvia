// Package asaas é o adaptador do Asaas (API v3), implementado conforme a
// documentação oficial (docs.asaas.com):
//
//   - Autenticação: header access_token; User-Agent identificando a aplicação.
//   - Ambientes: https://api-sandbox.asaas.com/v3 e https://api.asaas.com/v3.
//   - Cobrança avulsa: POST /payments com billingType UNDEFINED (o pagador
//     escolhe o meio na fatura hospedada invoiceUrl); o Recorvia não recebe
//     dados de cartão.
//   - Consulta: GET /payments/{id} e GET /payments?externalReference=.
//   - Reembolso: POST /payments/{id}/refund (total ou parcial); a resposta é
//     a cobrança com a lista refunds (sem identificador por reembolso, com
//     situação PENDING, AWAITING_CRITICAL_ACTION_AUTHORIZATION,
//     AWAITING_CUSTOMER_EXTERNAL_AUTHORIZATION, CANCELLED ou DONE).
//   - Cancelamento: DELETE /payments/{id}, resposta {deleted, id}; dispara o
//     evento PAYMENT_DELETED.
//   - Listagem para conciliação: GET /payments com dateCreated[ge],
//     dateCreated[le], offset e limit (máximo 100) e hasMore na resposta.
//   - Webhook: POST /webhooks com authToken; o Asaas envia o token no header
//     asaas-access-token. A documentação não descreve assinatura HMAC nem
//     chave de idempotência; por isso a proteção contra duplicidade usa
//     externalReference + consulta antes de repetir.
//
// Valores monetários do Asaas são decimais em reais; a conversão usa texto
// decimal exato (nunca float).
package asaas

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways"
)

// URLs base oficiais.
const (
	SandboxURL    = "https://api-sandbox.asaas.com/v3"
	ProductionURL = "https://api.asaas.com/v3"
)

// AuthHeader é o header com o token configurado no webhook.
const AuthHeader = "asaas-access-token"

// Gateway implementa payments.Gateway para o Asaas.
type Gateway struct {
	c            *gateways.Client
	webhookEmail string
}

// New cria o adaptador. baseURL permite apontar para um servidor de testes.
func New(baseURL, apiKey, webhookEmail string, httpc *http.Client) *Gateway {
	return &Gateway{webhookEmail: webhookEmail, c: &gateways.Client{HTTP: httpc, BaseURL: strings.TrimRight(baseURL, "/"), Headers: func(r *http.Request) {
		r.Header.Set("access_token", apiKey)
		r.Header.Set("User-Agent", "Recorvia")
	}}}
}

// BaseURL retorna a URL do ambiente.
func BaseURL(environment string) (string, error) {
	switch environment {
	case "sandbox":
		return SandboxURL, nil
	case "production":
		return ProductionURL, nil
	}
	return "", fmt.Errorf("asaas: ambiente desconhecido %q", environment)
}

// decimalToMoney converte um número JSON (reais) em centavos sem float.
func decimalToMoney(n json.Number) (money.Money, error) {
	s := n.String()
	if s == "" {
		return money.Zero(money.BRL), nil
	}
	r, err := money.ParseDecimal(s)
	if err != nil {
		return money.Money{}, err
	}
	return money.FromRat(r.Mul(r, money.New(100, money.BRL).Rat()), money.BRL, money.HalfEven)
}

// moneyToDecimal formata centavos como número JSON em reais ("99.90").
func moneyToDecimal(m money.Money) json.Number { return json.Number(m.DecimalString()) }

type paymentDTO struct {
	ID                string      `json:"id"`
	Customer          string      `json:"customer"`
	Status            string      `json:"status"`
	Value             json.Number `json:"value"`
	NetValue          json.Number `json:"netValue"`
	InvoiceURL        string      `json:"invoiceUrl"`
	ExternalReference string      `json:"externalReference"`
	PaymentDate       string      `json:"paymentDate"`
	ConfirmedDate     string      `json:"confirmedDate"`
	ClientPaymentDate string      `json:"clientPaymentDate"`
	DateCreated       string      `json:"dateCreated"`
	Deleted           bool        `json:"deleted"`
	Refunds           []refundDTO `json:"refunds"`
}

type refundDTO struct {
	Status      string      `json:"status"`
	Value       json.Number `json:"value"`
	DateCreated string      `json:"dateCreated"`
	Description string      `json:"description"`
}

// RefundState converte a situação documentada de um estorno do Asaas.
func RefundState(status string) payments.RefundState {
	switch status {
	case "DONE":
		return payments.RefundStateDone
	case "CANCELLED":
		return payments.RefundStateFailed
	}
	return payments.RefundStatePending
}

// MapStatus converte a situação documentada do Asaas.
func MapStatus(s string, deleted bool) payments.ProviderStatus {
	if deleted {
		return payments.ProviderCanceled
	}
	switch s {
	case "PENDING", "AWAITING_RISK_ANALYSIS", "DUNNING_REQUESTED":
		return payments.ProviderPending
	case "OVERDUE":
		return payments.ProviderOverdue
	case "CONFIRMED":
		return payments.ProviderConfirmed
	case "RECEIVED", "RECEIVED_IN_CASH", "DUNNING_RECEIVED":
		return payments.ProviderReceived
	case "REFUND_REQUESTED", "REFUND_IN_PROGRESS":
		return payments.ProviderRefundInProgress
	case "REFUNDED":
		return payments.ProviderRefunded
	case "CHARGEBACK_REQUESTED":
		return payments.ProviderChargebackRequested
	case "CHARGEBACK_DISPUTE":
		return payments.ProviderChargebackDispute
	case "AWAITING_CHARGEBACK_REVERSAL":
		return payments.ProviderChargebackReversed
	}
	return payments.ProviderUnknownStatus
}

func parseDate(s string) *time.Time {
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.DateOnly, "2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return &t
		}
	}
	return nil
}

func (d paymentDTO) toProvider() (payments.ProviderCharge, error) {
	gross, err := decimalToMoney(d.Value)
	if err != nil {
		return payments.ProviderCharge{}, err
	}
	pc := payments.ProviderCharge{ProviderChargeID: d.ID, ExternalReference: d.ExternalReference, ProviderCustomerID: d.Customer,
		Status: MapStatus(d.Status, d.Deleted), RawStatus: d.Status, Amount: gross, RefundedAmount: money.Zero(money.BRL), CheckoutURL: d.InvoiceURL}
	if d.NetValue.String() != "" {
		net, err := decimalToMoney(d.NetValue)
		if err != nil {
			return payments.ProviderCharge{}, err
		}
		pc.NetAmount = &net
	}
	for _, r := range d.Refunds {
		v, err := decimalToMoney(r.Value)
		if err != nil {
			return payments.ProviderCharge{}, err
		}
		// O Asaas não informa identificador por estorno: a associação com os
		// reembolsos locais é feita por valor (ver payments.planRefundSync).
		pc.Refunds = append(pc.Refunds, payments.ProviderRefund{Status: r.Status, State: RefundState(r.Status), Amount: v})
		if r.Status != "DONE" {
			continue
		}
		if pc.RefundedAmount, err = pc.RefundedAmount.Add(v); err != nil {
			return payments.ProviderCharge{}, err
		}
	}
	// Estorno efetivado em cobrança recebida indica reembolso parcial. As
	// demais situações (chargeback, estorno em andamento, reembolsada) são
	// preservadas: decidem disputas e a conciliação, e o valor reembolsado
	// continua em RefundedAmount e Refunds.
	if pc.RefundedAmount.IsPositive() && (pc.Status == payments.ProviderReceived || pc.Status == payments.ProviderConfirmed) {
		pc.Status = payments.ProviderPartiallyRefunded
	}
	for _, s := range []string{d.ConfirmedDate, d.PaymentDate, d.ClientPaymentDate} {
		if t := parseDate(s); t != nil {
			pc.PaidAt = t
			break
		}
	}
	return pc, nil
}

// EnsureCustomer procura o cliente pela referência externa e o cria se necessário.
func (g *Gateway) EnsureCustomer(ctx context.Context, c payments.CustomerData) (string, error) {
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := g.c.Do(ctx, http.MethodGet, "/customers?externalReference="+url.QueryEscape(c.ID.String()), nil, &list); err != nil {
		return "", err
	}
	if len(list.Data) > 0 {
		return list.Data[0].ID, nil
	}
	body := map[string]string{"name": c.Name, "cpfCnpj": c.DocumentNumber, "externalReference": c.ID.String()}
	if c.Email != "" {
		body["email"] = c.Email
	}
	var out struct {
		ID string `json:"id"`
	}
	err := g.c.Do(ctx, http.MethodPost, "/customers", body, &out)
	return out.ID, err
}

// CreateCharge cria a cobrança avulsa com fatura hospedada.
func (g *Gateway) CreateCharge(ctx context.Context, req payments.CreateChargeRequest) (payments.ProviderCharge, error) {
	var out paymentDTO
	err := g.c.Do(ctx, http.MethodPost, "/payments", map[string]any{
		"customer": req.Customer.ProviderCustomerID, "billingType": "UNDEFINED", "value": moneyToDecimal(req.Amount),
		"dueDate": req.DueDate.String(), "description": req.Description, "externalReference": req.ExternalReference,
	}, &out)
	if err != nil {
		return payments.ProviderCharge{}, err
	}
	return out.toProvider()
}

// GetCharge consulta a cobrança.
func (g *Gateway) GetCharge(ctx context.Context, id string) (payments.ProviderCharge, error) {
	var out paymentDTO
	if err := g.c.Do(ctx, http.MethodGet, "/payments/"+url.PathEscape(id), nil, &out); err != nil {
		return payments.ProviderCharge{}, err
	}
	return out.toProvider()
}

// FindByExternalReference procura a cobrança pela referência local.
func (g *Gateway) FindByExternalReference(ctx context.Context, ref string) (*payments.ProviderCharge, error) {
	var out struct {
		Data []paymentDTO `json:"data"`
	}
	if err := g.c.Do(ctx, http.MethodGet, "/payments?externalReference="+url.QueryEscape(ref), nil, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, nil
	}
	pc, err := out.Data[0].toProvider()
	return &pc, err
}

// Refund solicita estorno (total ou parcial). A documentação não descreve
// chave de idempotência nem identificador do estorno: a resposta é a
// cobrança atualizada, devolvida em Charge para que o resultado seja
// associado ao reembolso local pela lista refunds. Por isso o reembolso fica
// pendente até a confirmação na própria resposta, por webhook ou consulta.
func (g *Gateway) Refund(ctx context.Context, req payments.RefundRequest) (payments.ProviderRefund, error) {
	var out paymentDTO
	err := g.c.Do(ctx, http.MethodPost, "/payments/"+url.PathEscape(req.ProviderChargeID)+"/refund",
		map[string]any{"value": moneyToDecimal(req.Amount), "description": req.Description}, &out)
	if err != nil {
		return payments.ProviderRefund{}, err
	}
	pc, err := out.toProvider()
	if err != nil {
		return payments.ProviderRefund{}, &payments.GatewayError{Kind: payments.ErrKindAmbiguous, Message: "resposta do estorno ilegível", Err: err}
	}
	return payments.ProviderRefund{Status: out.Status, State: payments.RefundStatePending, Amount: req.Amount, Charge: &pc}, nil
}

// CancelCharge exclui a cobrança ainda não recebida (DELETE /payments/{id}).
// Resposta sem confirmação da exclusão é tratada como resultado ambíguo.
func (g *Gateway) CancelCharge(ctx context.Context, id string) error {
	var out struct {
		Deleted bool   `json:"deleted"`
		ID      string `json:"id"`
	}
	if err := g.c.Do(ctx, http.MethodDelete, "/payments/"+url.PathEscape(id), nil, &out); err != nil {
		return err
	}
	if !out.Deleted {
		return &payments.GatewayError{Kind: payments.ErrKindAmbiguous, StatusCode: http.StatusOK, Message: "exclusão não confirmada pelo provedor"}
	}
	return nil
}

// ListCharges lista cobranças criadas na janela (paginação offset/limit, máx. 100).
func (g *Gateway) ListCharges(ctx context.Context, req payments.ListRequest) (payments.ListResult, error) {
	q := url.Values{}
	if !req.UpdatedFrom.IsZero() {
		q.Set("dateCreated[ge]", req.UpdatedFrom.UTC().Format(time.DateOnly))
	}
	if !req.UpdatedTo.IsZero() {
		q.Set("dateCreated[le]", req.UpdatedTo.UTC().Format(time.DateOnly))
	}
	q.Set("offset", strconv.Itoa(req.Offset))
	q.Set("limit", strconv.Itoa(min(max(req.Limit, 1), 100)))
	var out struct {
		Data    []paymentDTO `json:"data"`
		HasMore bool         `json:"hasMore"`
	}
	if err := g.c.Do(ctx, http.MethodGet, "/payments?"+q.Encode(), nil, &out); err != nil {
		return payments.ListResult{}, err
	}
	res := payments.ListResult{HasMore: out.HasMore}
	for _, d := range out.Data {
		pc, err := d.toProvider()
		if err != nil {
			return payments.ListResult{}, err
		}
		res.Charges = append(res.Charges, pc)
	}
	return res, nil
}

// Events assinados no webhook criado pelo Recorvia.
var Events = []string{"PAYMENT_CREATED", "PAYMENT_UPDATED", "PAYMENT_CONFIRMED", "PAYMENT_RECEIVED", "PAYMENT_OVERDUE", "PAYMENT_DELETED",
	"PAYMENT_RESTORED", "PAYMENT_REFUNDED", "PAYMENT_PARTIALLY_REFUNDED", "PAYMENT_REFUND_IN_PROGRESS", "PAYMENT_REFUND_DENIED",
	"PAYMENT_CHARGEBACK_REQUESTED", "PAYMENT_CHARGEBACK_DISPUTE", "PAYMENT_AWAITING_CHARGEBACK_REVERSAL",
	"PAYMENT_REPROVED_BY_RISK_ANALYSIS", "PAYMENT_CREDIT_CARD_CAPTURE_REFUSED"}

// ConfigureWebhook cria o webhook com authToken próprio e envio sequencial.
func (g *Gateway) ConfigureWebhook(ctx context.Context, webhookURL, secret string) error {
	if g.webhookEmail == "" {
		return errors.New("asaas: e-mail de notificação do webhook não configurado")
	}
	return g.c.Do(ctx, http.MethodPost, "/webhooks", map[string]any{
		"name": "Recorvia", "url": webhookURL, "email": g.webhookEmail, "enabled": true, "interrupted": false,
		"apiVersion": 3, "authToken": secret, "sendType": "SEQUENTIALLY", "events": Events,
	}, nil)
}

// Verifier autentica webhooks pelo token configurado.
type Verifier struct{}

// ErrToken indica token ausente ou divergente.
var ErrToken = errors.New("asaas: token do webhook inválido")

// Verify compara o header asaas-access-token em tempo constante.
func (Verifier) Verify(h http.Header, _ []byte, secret string, _ time.Time) error {
	got := h.Get(AuthHeader)
	if got == "" || secret == "" || subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		return ErrToken
	}
	return nil
}

// eventStatus mapeia o tipo de evento para a situação normalizada.
func eventStatus(event string) payments.ProviderStatus {
	switch event {
	case "PAYMENT_CREATED", "PAYMENT_UPDATED", "PAYMENT_RESTORED", "PAYMENT_AWAITING_RISK_ANALYSIS", "PAYMENT_APPROVED_BY_RISK_ANALYSIS":
		return payments.ProviderPending
	case "PAYMENT_CONFIRMED":
		return payments.ProviderConfirmed
	case "PAYMENT_RECEIVED":
		return payments.ProviderReceived
	case "PAYMENT_OVERDUE":
		return payments.ProviderOverdue
	case "PAYMENT_DELETED":
		return payments.ProviderCanceled
	case "PAYMENT_REFUNDED":
		return payments.ProviderRefunded
	case "PAYMENT_PARTIALLY_REFUNDED":
		return payments.ProviderPartiallyRefunded
	case "PAYMENT_REFUND_IN_PROGRESS":
		return payments.ProviderRefundInProgress
	case "PAYMENT_CHARGEBACK_REQUESTED":
		return payments.ProviderChargebackRequested
	case "PAYMENT_CHARGEBACK_DISPUTE":
		return payments.ProviderChargebackDispute
	case "PAYMENT_AWAITING_CHARGEBACK_REVERSAL":
		return payments.ProviderChargebackReversed
	case "PAYMENT_REPROVED_BY_RISK_ANALYSIS", "PAYMENT_CREDIT_CARD_CAPTURE_REFUSED":
		return payments.ProviderFailed
	}
	return payments.ProviderUnknownStatus
}

// Parse interpreta o evento (id, event, dateCreated, payment).
func (Verifier) Parse(body []byte) (payments.WebhookEvent, error) {
	var ev struct {
		ID          string      `json:"id"`
		Event       string      `json:"event"`
		DateCreated string      `json:"dateCreated"`
		Payment     *paymentDTO `json:"payment"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&ev); err != nil {
		return payments.WebhookEvent{}, err
	}
	if ev.ID == "" || ev.Event == "" {
		return payments.WebhookEvent{}, errors.New("asaas: evento incompleto")
	}
	out := payments.WebhookEvent{EventID: ev.ID, Type: ev.Event, Status: eventStatus(ev.Event), OccurredAt: parseDate(ev.DateCreated)}
	if ev.Payment != nil && ev.Payment.ID != "" {
		out.ProviderChargeID = ev.Payment.ID
		pc, err := ev.Payment.toProvider()
		if err != nil {
			return payments.WebhookEvent{}, err
		}
		// A situação do evento prevalece sobre a do objeto, que pode estar
		// defasada; divergências levam à consulta autoritativa.
		if out.Status != payments.ProviderUnknownStatus {
			pc.Status = out.Status
		}
		out.Charge = &pc
	}
	return out, nil
}
