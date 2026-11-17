package payments

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// ProviderStatus é a situação normalizada de uma cobrança no provedor.
type ProviderStatus string

const (
	ProviderPending             ProviderStatus = "pending"
	ProviderConfirmed           ProviderStatus = "confirmed"
	ProviderReceived            ProviderStatus = "received"
	ProviderOverdue             ProviderStatus = "overdue"
	ProviderFailed              ProviderStatus = "failed"
	ProviderCanceled            ProviderStatus = "canceled"
	ProviderRefundInProgress    ProviderStatus = "refund_in_progress"
	ProviderPartiallyRefunded   ProviderStatus = "partially_refunded"
	ProviderRefunded            ProviderStatus = "refunded"
	ProviderChargebackRequested ProviderStatus = "chargeback_requested"
	ProviderChargebackDispute   ProviderStatus = "chargeback_dispute"
	ProviderChargebackReversed  ProviderStatus = "chargeback_reversed"
	ProviderChargebackLost      ProviderStatus = "chargeback_lost"
	ProviderUnknownStatus       ProviderStatus = "unknown"
)

// IsPaid informa se a situação implica pagamento confirmado.
func (s ProviderStatus) IsPaid() bool {
	switch s {
	case ProviderConfirmed, ProviderReceived, ProviderRefundInProgress, ProviderPartiallyRefunded, ProviderRefunded,
		ProviderChargebackRequested, ProviderChargebackDispute, ProviderChargebackReversed, ProviderChargebackLost:
		return true
	}
	return false
}

// CustomerData identifica o pagador para o provedor.
type CustomerData struct {
	ID                 uuid.UUID
	Name               string
	DocumentNumber     string
	Email              string
	ProviderCustomerID string
}

// CreateChargeRequest é o pedido de criação de cobrança avulsa.
type CreateChargeRequest struct {
	ExternalReference string
	Amount            money.Money
	DueDate           calendar.Date
	Description       string
	Customer          CustomerData
}

// ProviderCharge é a visão do provedor sobre uma cobrança.
type ProviderCharge struct {
	ProviderChargeID   string
	ExternalReference  string
	ProviderCustomerID string
	Status             ProviderStatus
	RawStatus          string
	Amount             money.Money
	NetAmount          *money.Money
	Fee                *money.Money
	RefundedAmount     money.Money
	CheckoutURL        string
	PaidAt             *time.Time
	UpdatedAt          *time.Time
	Raw                map[string]any
}

// FeeOrDerived retorna a taxa informada ou derivada de bruto menos líquido.
func (p ProviderCharge) FeeOrDerived() money.Money {
	if p.Fee != nil {
		return *p.Fee
	}
	if p.NetAmount != nil {
		if f, err := p.Amount.Sub(*p.NetAmount); err == nil && !f.IsNegative() {
			return f
		}
	}
	return money.Zero(p.Amount.Currency())
}

// RefundRequest é o pedido de reembolso.
type RefundRequest struct {
	ProviderChargeID  string
	Amount            money.Money
	Description       string
	ExternalReference string
}

// ProviderRefund é o resultado de um reembolso.
type ProviderRefund struct {
	ProviderRefundID string
	Status           string
	Amount           money.Money
}

// ListRequest filtra cobranças para conciliação.
type ListRequest struct {
	UpdatedFrom time.Time
	UpdatedTo   time.Time
	Offset      int
	Limit       int
}

// ListResult é uma página de cobranças do provedor.
type ListResult struct {
	Charges []ProviderCharge
	HasMore bool
}

// Gateway é a porta para um provedor de pagamento.
type Gateway interface {
	EnsureCustomer(ctx context.Context, c CustomerData) (string, error)
	CreateCharge(ctx context.Context, req CreateChargeRequest) (ProviderCharge, error)
	GetCharge(ctx context.Context, providerChargeID string) (ProviderCharge, error)
	FindByExternalReference(ctx context.Context, ref string) (*ProviderCharge, error)
	Refund(ctx context.Context, req RefundRequest) (ProviderRefund, error)
	ListCharges(ctx context.Context, req ListRequest) (ListResult, error)
	ConfigureWebhook(ctx context.Context, url, secret string) error
}

// WebhookEvent é um evento recebido, normalizado pelo adaptador.
type WebhookEvent struct {
	EventID          string
	Type             string
	ProviderChargeID string
	Status           ProviderStatus
	OccurredAt       *time.Time
	Charge           *ProviderCharge
}

// WebhookVerifier autentica e interpreta webhooks conforme o contrato do provedor.
type WebhookVerifier interface {
	Verify(header http.Header, body []byte, secret string, now time.Time) error
	Parse(body []byte) (WebhookEvent, error)
}

// ErrorKind classifica falhas de integração.
type ErrorKind int

const (
	// ErrKindPermanent: o provedor rejeitou o pedido (validação, credencial).
	ErrKindPermanent ErrorKind = iota + 1
	// ErrKindRetryable: o pedido comprovadamente não foi processado
	// (conexão recusada, limite de taxa); repetir é seguro.
	ErrKindRetryable
	// ErrKindAmbiguous: o pedido pode ter sido processado (timeout após
	// envio, resposta perdida, erro 5xx). Exige consulta antes de repetir.
	ErrKindAmbiguous
	// ErrKindNotFound: recurso inexistente no provedor.
	ErrKindNotFound
)

// GatewayError é um erro classificado de provedor.
type GatewayError struct {
	Kind       ErrorKind
	StatusCode int
	Code       string
	Message    string
	Err        error
}

func (e *GatewayError) Error() string {
	return fmt.Sprintf("gateway: kind=%d status=%d code=%s: %s", e.Kind, e.StatusCode, e.Code, e.Message)
}

// Unwrap expõe a causa.
func (e *GatewayError) Unwrap() error { return e.Err }

// GatewayErrorKind extrai a classificação; erros não classificados são
// tratados como ambíguos (postura conservadora).
func GatewayErrorKind(err error) ErrorKind {
	var ge *GatewayError
	if errors.As(err, &ge) {
		return ge.Kind
	}
	return ErrKindAmbiguous
}

// ProviderAccount é a configuração de um provedor no tenant.
type ProviderAccount struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	Provider    string
	Environment string
	Status      string
	IsDefault   bool
	APIKey      string
	APIKeyHint  string
	Secret      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Version     int
}

// GatewayResolver instancia o adaptador para uma conta.
type GatewayResolver interface {
	Gateway(account ProviderAccount) (Gateway, error)
	Verifier(provider string) (WebhookVerifier, error)
	Supports(provider string) bool
}
