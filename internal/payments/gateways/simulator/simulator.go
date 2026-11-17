// Package simulator é o adaptador do provedor simulado local (cmd/paysim).
// Webhooks são autenticados por HMAC-SHA256 com timestamp
// (header Paysim-Signature: t=<unix>,v1=<hex>), com tolerância de 5 minutos.
package simulator

import (
	"context"
	"crypto/hmac"
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
	"github.com/ricartefelipe/recorvia/internal/paysim"
)

// Gateway implementa payments.Gateway para o simulador.
type Gateway struct{ c *gateways.Client }

// New cria o adaptador para a chave de API da conta.
func New(baseURL, apiKey string, httpc *http.Client) *Gateway {
	return &Gateway{c: &gateways.Client{HTTP: httpc, BaseURL: strings.TrimRight(baseURL, "/"), Headers: func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+apiKey)
		r.Header.Set("User-Agent", "Recorvia")
	}}}
}

type chargeDTO struct {
	ID                string     `json:"id"`
	CustomerID        string     `json:"customer_id"`
	ExternalReference string     `json:"external_reference"`
	Amount            int64      `json:"amount"`
	Currency          string     `json:"currency"`
	Status            string     `json:"status"`
	PaidAmount        int64      `json:"paid_amount"`
	Fee               int64      `json:"fee"`
	NetAmount         int64      `json:"net_amount"`
	RefundedAmount    int64      `json:"refunded_amount"`
	ChargebackStatus  string     `json:"chargeback_status"`
	CheckoutURL       string     `json:"checkout_url"`
	PaidAt            *time.Time `json:"paid_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// MapStatus converte a situação do simulador.
func MapStatus(status, chargeback string) payments.ProviderStatus {
	switch status {
	case "pending":
		return payments.ProviderPending
	case "paid":
		if chargeback == "won" {
			return payments.ProviderChargebackReversed
		}
		return payments.ProviderReceived
	case "failed":
		return payments.ProviderFailed
	case "canceled":
		return payments.ProviderCanceled
	case "refunded":
		return payments.ProviderRefunded
	case "partially_refunded":
		return payments.ProviderPartiallyRefunded
	case "chargeback_requested":
		return payments.ProviderChargebackRequested
	case "chargeback_lost":
		return payments.ProviderChargebackLost
	}
	return payments.ProviderUnknownStatus
}

func (d chargeDTO) toProvider() payments.ProviderCharge {
	gross := d.Amount
	if d.PaidAmount > 0 {
		gross = d.PaidAmount
	}
	pc := payments.ProviderCharge{ProviderChargeID: d.ID, ExternalReference: d.ExternalReference, ProviderCustomerID: d.CustomerID,
		Status: MapStatus(d.Status, d.ChargebackStatus), RawStatus: d.Status, Amount: money.New(gross, money.BRL),
		RefundedAmount: money.New(d.RefundedAmount, money.BRL), CheckoutURL: d.CheckoutURL, PaidAt: d.PaidAt}
	if d.PaidAmount > 0 {
		fee, net := money.New(d.Fee, money.BRL), money.New(d.NetAmount, money.BRL)
		pc.Fee, pc.NetAmount = &fee, &net
	}
	u := d.UpdatedAt
	pc.UpdatedAt = &u
	return pc
}

// EnsureCustomer cria o pagador (idempotente por referência externa).
func (g *Gateway) EnsureCustomer(ctx context.Context, c payments.CustomerData) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := g.c.Do(ctx, http.MethodPost, "/v1/customers", map[string]string{"name": c.Name, "document": c.DocumentNumber,
		"email": c.Email, "external_reference": c.ID.String()}, &out)
	return out.ID, err
}

// CreateCharge cria a cobrança avulsa.
func (g *Gateway) CreateCharge(ctx context.Context, req payments.CreateChargeRequest) (payments.ProviderCharge, error) {
	var out chargeDTO
	err := g.c.Do(ctx, http.MethodPost, "/v1/charges", map[string]any{"customer_id": req.Customer.ProviderCustomerID,
		"amount": req.Amount.Amount(), "currency": req.Amount.Currency().Code(), "due_date": req.DueDate.String(),
		"description": req.Description, "external_reference": req.ExternalReference}, &out)
	if err != nil {
		return payments.ProviderCharge{}, err
	}
	return out.toProvider(), nil
}

// GetCharge consulta a cobrança.
func (g *Gateway) GetCharge(ctx context.Context, id string) (payments.ProviderCharge, error) {
	var out chargeDTO
	if err := g.c.Do(ctx, http.MethodGet, "/v1/charges/"+url.PathEscape(id), nil, &out); err != nil {
		return payments.ProviderCharge{}, err
	}
	return out.toProvider(), nil
}

// FindByExternalReference procura cobrança pela referência local.
func (g *Gateway) FindByExternalReference(ctx context.Context, ref string) (*payments.ProviderCharge, error) {
	var out struct {
		Data []chargeDTO `json:"data"`
	}
	if err := g.c.Do(ctx, http.MethodGet, "/v1/charges?external_reference="+url.QueryEscape(ref), nil, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, nil
	}
	pc := out.Data[0].toProvider()
	return &pc, nil
}

// Refund solicita reembolso (o simulador deduplica pela referência externa).
func (g *Gateway) Refund(ctx context.Context, req payments.RefundRequest) (payments.ProviderRefund, error) {
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Amount int64  `json:"amount"`
	}
	err := g.c.Do(ctx, http.MethodPost, "/v1/charges/"+url.PathEscape(req.ProviderChargeID)+"/refunds", map[string]any{
		"amount": req.Amount.Amount(), "description": req.Description, "external_reference": req.ExternalReference}, &out)
	if err != nil {
		return payments.ProviderRefund{}, err
	}
	return payments.ProviderRefund{ProviderRefundID: out.ID, Status: out.Status, Amount: money.New(out.Amount, money.BRL)}, nil
}

// ListCharges lista cobranças atualizadas na janela.
func (g *Gateway) ListCharges(ctx context.Context, req payments.ListRequest) (payments.ListResult, error) {
	q := url.Values{}
	if !req.UpdatedFrom.IsZero() {
		q.Set("updated_from", req.UpdatedFrom.UTC().Format(time.RFC3339))
	}
	if !req.UpdatedTo.IsZero() {
		q.Set("updated_to", req.UpdatedTo.UTC().Format(time.RFC3339))
	}
	q.Set("offset", strconv.Itoa(req.Offset))
	q.Set("limit", strconv.Itoa(max(req.Limit, 1)))
	var out struct {
		Data    []chargeDTO `json:"data"`
		HasMore bool        `json:"has_more"`
	}
	if err := g.c.Do(ctx, http.MethodGet, "/v1/charges?"+q.Encode(), nil, &out); err != nil {
		return payments.ListResult{}, err
	}
	res := payments.ListResult{HasMore: out.HasMore}
	for _, d := range out.Data {
		res.Charges = append(res.Charges, d.toProvider())
	}
	return res, nil
}

// ConfigureWebhook registra destino e segredo dos webhooks.
func (g *Gateway) ConfigureWebhook(ctx context.Context, webhookURL, secret string) error {
	return g.c.Do(ctx, http.MethodPut, "/v1/webhook", map[string]string{"url": webhookURL, "secret": secret}, nil)
}

// Verifier autentica e interpreta webhooks do simulador.
type Verifier struct {
	// Tolerance limita a diferença entre o timestamp assinado e o relógio.
	Tolerance time.Duration
}

// ErrSignature indica assinatura ausente, inválida ou expirada.
var ErrSignature = errors.New("simulator: assinatura inválida")

// Verify confere o HMAC em tempo constante e a janela de tempo.
func (v Verifier) Verify(h http.Header, body []byte, secret string, now time.Time) error {
	tol := v.Tolerance
	if tol <= 0 {
		tol = 5 * time.Minute
	}
	var ts int64
	var sigs []string
	for _, part := range strings.Split(h.Get(paysim.SignatureHeader), ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return ErrSignature
			}
			ts = n
		case "v1":
			sigs = append(sigs, val)
		}
	}
	if ts == 0 || len(sigs) == 0 {
		return ErrSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > tol || d < -tol {
		return fmt.Errorf("%w: fora da janela de tempo", ErrSignature)
	}
	expected := paysim.Sign(secret, ts, body)
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(expected)) {
			return nil
		}
	}
	return ErrSignature
}

// Parse interpreta o evento.
func (Verifier) Parse(body []byte) (payments.WebhookEvent, error) {
	var ev struct {
		ID        string    `json:"id"`
		Type      string    `json:"type"`
		CreatedAt time.Time `json:"created_at"`
		Data      struct {
			Charge chargeDTO `json:"charge"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return payments.WebhookEvent{}, err
	}
	if ev.ID == "" || ev.Type == "" || ev.Data.Charge.ID == "" {
		return payments.WebhookEvent{}, errors.New("simulator: evento incompleto")
	}
	pc := ev.Data.Charge.toProvider()
	at := ev.CreatedAt
	return payments.WebhookEvent{EventID: ev.ID, Type: ev.Type, ProviderChargeID: pc.ProviderChargeID, Status: pc.Status, OccurredAt: &at, Charge: &pc}, nil
}
