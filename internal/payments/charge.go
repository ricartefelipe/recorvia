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
	ChargeDisputed:          {ChargePaid, ChargePartiallyRefunded, ChargeRefunded, ChargeChargedBack},
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
	// AmountRefundPending soma os reembolsos pedidos e ainda não confirmados
	// pelo provedor (reserva do saldo reembolsável).
	AmountRefundPending money.Money
	AmountChargedBack   money.Money
	CheckoutURL         string
	DueDate             calendar.Date
	PaidAt              *time.Time
	FailureReason       string
	ProviderStatus      string
	ProviderStatusAt    *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
	Version             int
}

// NewCharge cria a cobrança local ainda não enviada ao provedor.
func NewCharge(id, tenantID, invoiceID, customerID, accountID uuid.UUID, provider string, attempt int, amount money.Money, due calendar.Date, now time.Time) (Charge, error) {
	if !amount.IsPositive() {
		return Charge{}, apperr.Invalid("invalid_amount", "O valor da cobrança deve ser positivo.")
	}
	z := money.Zero(amount.Currency())
	return Charge{ID: id, TenantID: tenantID, InvoiceID: invoiceID, CustomerID: customerID, ProviderAccountID: accountID,
		Provider: provider, Attempt: attempt, Status: ChargePending, Amount: amount, AmountPaid: z, Fee: z,
		AmountRefunded: z, AmountRefundPending: z, AmountChargedBack: z, DueDate: due, CreatedAt: now, UpdatedAt: now, Version: 1}, nil
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

// Refundable é o saldo elegível a novos reembolsos: pago menos reembolsado,
// estornado por chargeback e reservado por reembolsos em andamento.
func (c *Charge) Refundable() (money.Money, error) {
	r, err := c.AmountPaid.Sub(c.AmountRefunded)
	if err != nil {
		return money.Money{}, err
	}
	if r, err = r.Sub(c.AmountChargedBack); err != nil {
		return money.Money{}, err
	}
	pending := c.AmountRefundPending
	if pending.Currency().IsZero() {
		pending = money.Zero(r.Currency())
	}
	return r.Sub(pending)
}

// AcceptsRefund informa se a cobrança aceita pedido ou envio de reembolso:
// somente cobranças pagas, inclusive parcialmente reembolsadas. Em disputa ou
// estornada por chargeback, o reembolso poderia devolver o valor ao pagador
// duas vezes.
func (c *Charge) AcceptsRefund() bool {
	return c.Status == ChargePaid || c.Status == ChargePartiallyRefunded
}

// ReserveRefund reserva o valor de um novo pedido de reembolso. Somente
// cobranças pagas (inclusive parcialmente reembolsadas) aceitam pedidos, e a
// soma das reservas com o já reembolsado e estornado nunca excede o pago.
func (c *Charge) ReserveRefund(amount money.Money, at time.Time) error {
	if !c.AcceptsRefund() {
		return apperr.Conflict("charge_not_refundable", fmt.Sprintf("A cobrança não aceita reembolso na situação %s.", c.Status))
	}
	elig, err := c.Refundable()
	if err != nil {
		return err
	}
	if cmp, err := amount.Cmp(elig); err != nil || cmp > 0 || !amount.IsPositive() {
		return apperr.Conflict("refund_exceeds_balance", "O reembolso excede o saldo elegível da cobrança.")
	}
	if c.AmountRefundPending, err = c.pendingOrZero().Add(amount); err != nil {
		return err
	}
	c.UpdatedAt = at
	return nil
}

func (c *Charge) pendingOrZero() money.Money {
	if c.AmountRefundPending.Currency().IsZero() {
		return money.Zero(c.AmountPaid.Currency())
	}
	return c.AmountRefundPending
}

// ReleaseRefund libera a reserva de um reembolso recusado ou confirmado.
func (c *Charge) ReleaseRefund(amount money.Money, at time.Time) error {
	left, err := c.pendingOrZero().Sub(amount)
	if err != nil {
		return err
	}
	if left.IsNegative() {
		return apperr.Conflict("refund_reservation_mismatch", "A reserva de reembolso da cobrança é menor que o valor liberado.")
	}
	c.AmountRefundPending, c.UpdatedAt = left, at
	return nil
}

// ConfirmRefund registra um reembolso efetivado pelo provedor. reserved
// indica que o valor foi reservado no pedido (reembolso solicitado pelo
// Recorvia); reembolsos feitos diretamente no provedor não têm reserva e
// precisam caber no saldo elegível. A cobrança paga passa a parcialmente
// reembolsada ou reembolsada; em disputa ou estornada, mantém a situação.
func (c *Charge) ConfirmRefund(amount money.Money, reserved bool, at time.Time) error {
	if !IsSettledStatus(c.Status) {
		return apperr.Conflict("refund_exceeds_balance", "O reembolso excede o saldo elegível da cobrança.")
	}
	if reserved {
		if err := c.ReleaseRefund(amount, at); err != nil {
			return err
		}
	}
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
	c.UpdatedAt = at
	if c.Status != ChargePaid && c.Status != ChargePartiallyRefunded {
		return nil
	}
	if cmp, _ := c.AmountRefunded.Cmp(c.AmountPaid); cmp == 0 {
		return c.transition(ChargeRefunded, at)
	}
	return c.transition(ChargePartiallyRefunded, at)
}

// ApplyRefund registra um reembolso efetivado sem reserva prévia.
func (c *Charge) ApplyRefund(amount money.Money, at time.Time) error {
	return c.ConfirmRefund(amount, false, at)
}

// OpenDispute registra chargeback solicitado.
func (c *Charge) OpenDispute(at time.Time) error { return c.transition(ChargeDisputed, at) }

// CloseDispute encerra a disputa: won devolve a cobrança à situação
// correspondente aos reembolsos confirmados (paga, parcialmente reembolsada
// ou reembolsada, inclusive pelos reembolsos confirmados durante a disputa);
// lost registra o valor estornado ao pagador, limitado ao saldo elegível
// (pago menos reembolsado, estornado e reservado por reembolsos em
// andamento): o valor devolvido por reembolso não é estornado de novo, e o
// reembolso pedido antes da disputa e confirmado depois dela continua
// cabendo no saldo.
func (c *Charge) CloseDispute(won bool, amount money.Money, at time.Time) error {
	if c.Status != ChargeDisputed {
		return apperr.Conflict("no_open_dispute", "A cobrança não está em disputa.")
	}
	if won {
		switch cmp, _ := c.AmountRefunded.Cmp(c.AmountPaid); {
		case c.AmountRefunded.IsPositive() && cmp >= 0:
			return c.transition(ChargeRefunded, at)
		case c.AmountRefunded.IsPositive():
			return c.transition(ChargePartiallyRefunded, at)
		}
		return c.transition(ChargePaid, at)
	}
	limit, err := c.Refundable()
	if err != nil {
		return err
	}
	if limit.IsNegative() {
		limit = money.Zero(limit.Currency())
	}
	if cmp, err := amount.Cmp(limit); err != nil {
		return err
	} else if cmp > 0 {
		amount = limit
	}
	if c.AmountChargedBack, err = c.AmountChargedBack.Add(amount); err != nil {
		return err
	}
	return c.transition(ChargeChargedBack, at)
}

// RecordProviderStatus guarda a última situação informada pelo provedor e o
// instante de origem mais recente informado por ele.
func (c *Charge) RecordProviderStatus(raw string, at *time.Time) {
	c.ProviderStatus = raw
	if at != nil && (c.ProviderStatusAt == nil || at.After(*c.ProviderStatusAt)) {
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
	// OpCancelCharge cancela no provedor uma cobrança cancelada localmente.
	OpCancelCharge OperationKind = "cancel_charge"
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

// Stalled informa se a operação ficou em execução depois do fim do lease
// (processo interrompido durante a chamada ao provedor): o resultado é
// desconhecido, como em OpUnknown, até nova consulta ao provedor.
func (o Operation) Stalled(now time.Time) bool {
	return o.Status == OpInFlight && (o.LeaseUntil == nil || o.LeaseUntil.Before(now))
}

// Running informa se a operação está em execução dentro do lease.
func (o Operation) Running(now time.Time) bool { return o.Status == OpInFlight && !o.Stalled(now) }

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

// Obsolete informa se a criação de cobrança deixou de se aplicar porque a
// cobrança não está mais pendente (por exemplo, cancelada com a fatura
// enquanto a criação aguardava nova tentativa). A execução de uma operação
// obsoleta só consulta o provedor pela referência externa: vincula a
// cobrança encontrada, sem anunciá-la como criada, ou encerra a operação
// como falha; nunca cria a cobrança no provedor.
func (o Operation) Obsolete(charge ChargeStatus) bool {
	return o.Kind == OpCreateCharge && charge != ChargePending
}

// Exhausted informa se a operação esgotou as tentativas automáticas e ficou
// parada: pendente ou com resultado desconhecido depois do horário da
// próxima tentativa, ou em execução com o lease vencido. A recuperação não a
// reivindica mais; "unknown" segue para a conciliação, enquanto "pending" e
// "in_flight" deixam a cobrança sem confirmação até a intervenção da
// operação. charge é a situação da cobrança da operação: uma criação
// "pending" obsoleta (Obsolete) não conta, porque nenhuma chamada ficou com
// resultado incerto (o provedor recusou a última de forma transitória) e a
// cobrança não será mais criada; a reexecução manual apenas a encerra. Em
// "unknown" e "in_flight", a cobrança pode existir no provedor mesmo
// cancelada localmente, e a operação continua contando.
func (o Operation) Exhausted(now time.Time, charge ChargeStatus) bool {
	if o.Attempts < MaxOperationAttempts {
		return false
	}
	switch o.Status {
	case OpPending:
		return !o.NextAttemptAt.After(now) && !o.Obsolete(charge)
	case OpUnknown:
		return !o.NextAttemptAt.After(now)
	case OpInFlight:
		return o.LeaseUntil != nil && o.LeaseUntil.Before(now)
	}
	return false
}

// CheckManualRetry valida a reexecução manual da operação agora. Ao
// contrário da recuperação agendada, ela antecipa a próxima tentativa
// (ignora next_attempt_at) e sempre consulta o provedor antes de criar. O
// limite de tentativas vale para "unknown" e "in_flight", cujo resultado
// segue para a conciliação; uma operação "pending" esgotada pode ser
// reexecutada depois de corrigida a causa, porque o provedor recusou a
// última criação de forma transitória e a cobrança não existe (cada nova
// tentativa consulta antes). O lease de uma execução em andamento é
// respeitado. A reexecução de uma operação obsoleta (Obsolete) só consulta o
// provedor e nunca cria a cobrança.
func (o Operation) CheckManualRetry(now time.Time) error {
	switch {
	case o.Status == OpSucceeded || o.Status == OpFailed:
		return apperr.Conflict("operation_completed", "A operação já foi concluída.")
	case o.Status != OpPending && o.Status != OpUnknown && o.Status != OpInFlight:
		return apperr.Conflict("operation_not_retryable", "A operação não pode ser reexecutada.")
	case o.Attempts >= MaxOperationAttempts && o.Status != OpPending:
		return apperr.Conflict("operation_attempts_exhausted",
			"A operação esgotou as tentativas automáticas; o resultado é resolvido pela conciliação.")
	case o.Status == OpInFlight && (o.LeaseUntil == nil || !o.LeaseUntil.Before(now)):
		return apperr.Conflict("operation_in_progress", "A operação está em execução; aguarde o resultado.")
	}
	return nil
}
