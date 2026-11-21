// Package outbox implementa o Transactional Outbox: eventos de domínio são
// gravados na mesma transação da alteração de negócio e publicados depois
// por um relay com lease. A entrega é pelo menos uma vez; consumidores
// deduplicam pelo identificador do evento (inbox).
package outbox

import (
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
)

// Tipos de eventos publicados. O nome é também a routing key no broker.
const (
	EventInvoiceIssued          = "invoice.issued"
	EventInvoicePaid            = "invoice.paid"
	EventInvoicePartiallyPaid   = "invoice.partially_paid"
	EventInvoiceVoided          = "invoice.voided"
	EventInvoiceCredited        = "invoice.credited"
	EventInvoiceOverdue         = "invoice.overdue"
	EventInvoiceUncollectible   = "invoice.uncollectible"
	EventChargeCreated          = "charge.created"
	EventChargeFailed           = "charge.failed"
	EventPaymentConfirmed       = "payment.confirmed"
	EventRefundSucceeded        = "refund.succeeded"
	EventRefundFailed           = "refund.failed"
	EventDisputeOpened          = "dispute.opened"
	EventDisputeClosed          = "dispute.closed"
	EventDisputeRecoverySettled = "dispute.recovery_settled"
	EventSubscriptionCreated    = "subscription.created"
	EventSubscriptionUpdated    = "subscription.updated"
	EventSubscriptionCanceled   = "subscription.canceled"
	EventSubscriptionSuspended  = "subscription.suspended"
	EventSubscriptionReactivate = "subscription.reactivated"
	EventCustomerCreated        = "customer.created"
	EventCustomerUpdated        = "customer.updated"
	EventProviderWebhook        = "provider_webhook.received"
	EventDiscrepancyDetected    = "reconciliation.discrepancy_detected"
	EventMemberInvited          = "member.invited"
	EventDeadLetterReplay       = "operations.dead_letter_replay"
)

// Event é um evento de domínio a publicar.
type Event struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	Type          string
	AggregateType string
	AggregateID   uuid.UUID
	Payload       any
	OccurredAt    time.Time
	// AvailableAt permite atrasar a publicação (zero = imediata).
	AvailableAt time.Time
}

// New cria um evento com identificador próprio.
func New(tenantID uuid.UUID, eventType, aggregateType string, aggregateID uuid.UUID, payload any, at time.Time) Event {
	return Event{
		ID: ids.New(), TenantID: tenantID, Type: eventType, AggregateType: aggregateType,
		AggregateID: aggregateID, Payload: payload, OccurredAt: at.UTC(),
	}
}
