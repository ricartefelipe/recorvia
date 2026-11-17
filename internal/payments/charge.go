// Package payments integra o Recorvia a provedores de pagamento. O
// Recorvia não custodia dinheiro nem processa cartões: cada fatura gera uma
// cobrança avulsa no provedor (checkout hospedado), e confirmações chegam por
// webhook ou consulta.
//
// Operações externas são registros persistidos (payment_operations) com
// lease e estado "unknown" quando não há confirmação segura do resultado.
// Antes de repetir uma criação de cobrança ambígua, o provedor é consultado
// pela referência externa (o ID local da cobrança); nunca se repete às cegas.
package payments

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// ChargeStatus é a situação local da cobrança.
type ChargeStatus string

const (
	ChargePending           ChargeStatus = "pending"
	ChargeAwaitingPayment   ChargeStatus = "awaiting_payment"
	ChargePaid              ChargeStatus = "paid"
	ChargeFailed            ChargeStatus = "failed"
	ChargeCanceled          ChargeStatus = "canceled"
	ChargePartiallyRefunded ChargeStatus = "partially_refunded"
	ChargeRefunded          ChargeStatus = "refunded"
	ChargeDisputed          ChargeStatus = "disputed"
	ChargeChargedBack       ChargeStatus = "charged_back"
)

// chargeTransitions lista as transições permitidas. Recusa (failed) e
// cancelamento (canceled) não encerram a cobrança no provedor: a única saída
// desses estados é a confirmação de pagamento pelo provedor (MarkPaid), para
// que o dinheiro recebido nunca seja descartado.
var chargeTransitions = map[ChargeStatus][]ChargeStatus{
	ChargePending:           {ChargeAwaitingPayment, ChargePaid, ChargeFailed, ChargeCanceled},
	ChargeAwaitingPayment:   {ChargePaid, ChargeFailed, ChargeCanceled},
	ChargePaid:              {ChargePartiallyRefunded, ChargeRefunded, ChargeDisputed},
	ChargePartiallyRefunded: {ChargePartiallyRefunded, ChargeRefunded, ChargeDisputed},
	ChargeDisputed:          {ChargePaid, ChargePartiallyRefunded, ChargeChargedBack},
	ChargeFailed:            {ChargePaid},
	ChargeCanceled:          {ChargePaid},
	ChargeRefunded:          {},
	ChargeChargedBack:       {},
}

// CanTransition informa se a transição de cobrança é permitida.
func CanTransition(from, to ChargeStatus) bool { return slices.Contains(chargeTransitions[from], to) }

// IsSettledStatus informa se a cobrança já teve pagamento confirmado.
func IsSettledStatus(s ChargeStatus) bool {
	switch s {
	case ChargePaid, ChargePartiallyRefunded, ChargeRefunded, ChargeDisputed, ChargeChargedBack:
		return true
	}
	return false
}

// Charge é uma cobrança de fatura em um provedor.
type Charge struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	InvoiceID         uuid.UUID
	CustomerID        uuid.UUID
	ProviderAccountID uuid.UUID
	Provider          string
	Attempt           int
	ProviderChargeID  string
	Status            ChargeStatus
	Amount            money.Money
	AmountPaid        money.Money
	Fee               money.Money
	AmountRefunded    money.Money
	AmountChargedBack money.Money
	CheckoutURL       string
	DueDate           calendar.Date
	PaidAt            *time.Time
	FailureReason     string
	ProviderStatus    string
	ProviderStatusAt  *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Version           int
}

// NewCharge cria a cobrança local ainda não enviada ao provedor.
func NewCharge(id, tenantID, invoiceID, customerID, accountID uuid.UUID, provider string, attempt int, amount money.Money, due calendar.Date, now time.Time) (Charge, error) {
	if !amount.IsPositive() {
		return Charge{}, apperr.Invalid("invalid_amount", "O valor da cobrança deve ser positivo.")
	}
	z := money.Zero(amount.Currency())
	return Charge{ID: id, TenantID: tenantID, InvoiceID: invoiceID, CustomerID: customerID, ProviderAccountID: accountID,
		Provider: provider, Attempt: attempt, Status: ChargePending, Amount: amount, AmountPaid: z, Fee: z,
		AmountRefunded: z, AmountChargedBack: z, DueDate: due, CreatedAt: now, UpdatedAt: now, Version: 1}, nil
}

func (c *Charge) transition(to ChargeStatus, at time.Time) error {
	if c.Status == to && to == ChargePartiallyRefunded {
		c.UpdatedAt = at
		return nil
	}
	if !CanTransition(c.Status, to) {
		return apperr.Conflict("invalid_charge_transition", fmt.Sprintf("Transição de cobrança não permitida: %s para %s.", c.Status, to))
	}
	c.Status, c.UpdatedAt = to, at
	return nil
}

// MarkCreated registra a criação confirmada no provedor.
func (c *Charge) MarkCreated(providerChargeID, checkoutURL string, at time.Time) error {
	if c.ProviderChargeID != "" && c.ProviderChargeID != providerChargeID {
		return apperr.Conflict("provider_charge_mismatch", "A cobrança já está vinculada a outra cobrança do provedor.")
	}
	c.ProviderChargeID, c.CheckoutURL = providerChargeID, checkoutURL
	if c.Status == ChargePending {
		return c.transition(ChargeAwaitingPayment, at)
	}
	return nil
}

// MarkPaid registra o pagamento confirmado pelo provedor. O valor pago e a
// taxa vêm do provedor e podem diferir do valor cobrado (juros, multa ou
// desconto do provedor); a diferença é tratada na aplicação à fatura
// (excedente vira crédito) e apontada na conciliação.
//
// A confirmação prevalece sobre recusa e cancelamento locais. O cancelamento
// no Recorvia (fatura cancelada, nota de crédito ou substituição por mudança
// de saldo) é apenas local, e a cobrança continua válida no provedor; uma
// recusa (por exemplo, do cartão) não impede o pagamento posterior por outro
// meio no mesmo checkout. Nos dois casos a cobrança passa a paga, para que o
// valor recebido seja aplicado à fatura ou vire crédito do cliente. O motivo
// da recusa deixa de descrever a situação da cobrança e é removido; quem
// confirma o pagamento o preserva na auditoria.
func (c *Charge) MarkPaid(paid, fee money.Money, paidAt, at time.Time) error {
	if IsSettledStatus(c.Status) {
		return nil // confirmação repetida: idempotente
	}
	if !paid.IsPositive() || paid.Currency() != c.Amount.Currency() {
		return apperr.Invalid("invalid_amount", "Valor pago inválido.")
	}
	if err := c.transition(ChargePaid, at); err != nil {
		return err
	}
	c.AmountPaid, c.Fee, c.PaidAt, c.FailureReason = paid, fee, &paidAt, ""
	return nil
}

// MarkFailed registra falha definitiva.
func (c *Charge) MarkFailed(reason string, at time.Time) error {
	if err := c.transition(ChargeFailed, at); err != nil {
		return err
	}
	c.FailureReason = truncate(reason, 500)
	return nil
}

// MarkCanceled cancela a cobrança (por exemplo, fatura cancelada).
func (c *Charge) MarkCanceled(at time.Time) error { return c.transition(ChargeCanceled, at) }

// Refundable é o saldo elegível a reembolso.
func (c *Charge) Refundable() (money.Money, error) {
	r, err := c.AmountPaid.Sub(c.AmountRefunded)
	if err != nil {
		return money.Money{}, err
	}
	return r.Sub(c.AmountChargedBack)
}

// ApplyRefund registra um reembolso efetivado.
func (c *Charge) ApplyRefund(amount money.Money, at time.Time) error {
	elig, err := c.Refundable()
	if err != nil {
		return err
	}
	if cmp, err := amount.Cmp(elig); err != nil || cmp > 0 || !amount.IsPositive() {
		return apperr.Conflict("refund_exceeds_balance", "O reembolso excede o saldo elegível da cobrança.")
	}
	if c.AmountRefunded, err = c.AmountRefunded.Add(amount); err != nil {
		return err
	}
	if cmp, _ := c.AmountRefunded.Cmp(c.AmountPaid); cmp == 0 {
		return c.transition(ChargeRefunded, at)
	}
	return c.transition(ChargePartiallyRefunded, at)
}

// OpenDispute registra chargeback solicitado.
func (c *Charge) OpenDispute(at time.Time) error { return c.transition(ChargeDisputed, at) }

// CloseDispute encerra a disputa: won devolve a cobrança a paga; lost
// registra o valor estornado ao pagador.
func (c *Charge) CloseDispute(won bool, amount money.Money, at time.Time) error {
	if c.Status != ChargeDisputed {
		return apperr.Conflict("no_open_dispute", "A cobrança não está em disputa.")
	}
	if won {
		if c.AmountRefunded.IsPositive() {
			return c.transition(ChargePartiallyRefunded, at)
		}
		return c.transition(ChargePaid, at)
	}
	var err error
	if c.AmountChargedBack, err = c.AmountChargedBack.Add(amount); err != nil {
		return err
	}
	return c.transition(ChargeChargedBack, at)
}

// RecordProviderStatus guarda a última situação informada pelo provedor
// junto do instante de origem informado por ele.
func (c *Charge) RecordProviderStatus(raw string, at *time.Time) {
	c.ProviderStatus = raw
	if at != nil {
		c.ProviderStatusAt = at
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// OperationKind é o tipo de operação externa.
type OperationKind string

const (
	OpCreateCharge OperationKind = "create_charge"
	OpRefund       OperationKind = "refund"
)

// OperationStatus é a situação da operação externa.
type OperationStatus string

const (
	OpPending   OperationStatus = "pending"
	OpInFlight  OperationStatus = "in_flight"
	OpSucceeded OperationStatus = "succeeded"
	OpFailed    OperationStatus = "failed"
	OpUnknown   OperationStatus = "unknown"
)

// Operation é uma operação externa persistida.
type Operation struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	ChargeID          uuid.UUID
	RefundID          *uuid.UUID
	Kind              OperationKind
	Status            OperationStatus
	ExternalReference string
	Attempts          int
	LeaseUntil        *time.Time
	NextAttemptAt     time.Time
	LastError         string
	ProviderResponse  map[string]any
	CreatedAt         time.Time
	UpdatedAt         time.Time
	CompletedAt       *time.Time
}

// MaxOperationAttempts limita retentativas automáticas; depois disso uma
// operação ambígua permanece "unknown" até resolução pela conciliação.
const MaxOperationAttempts = 6

// Backoff calcula a espera antes da próxima tentativa (exponencial, limitada).
func Backoff(attempt int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < attempt && d < 30*time.Minute; i++ {
		d *= 2
	}
	return min(d, 30*time.Minute)
}

// Claimable informa se a operação pode ser reivindicada agora.
func (o Operation) Claimable(now time.Time) bool {
	switch o.Status {
	case OpPending, OpUnknown:
		return !o.NextAttemptAt.After(now) && o.Attempts < MaxOperationAttempts
	case OpInFlight:
		return o.LeaseUntil != nil && o.LeaseUntil.Before(now) && o.Attempts < MaxOperationAttempts
	}
	return false
}
