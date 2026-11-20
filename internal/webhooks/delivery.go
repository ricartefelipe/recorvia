package webhooks

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

// DeliveryStatus é a situação de uma entrega.
type DeliveryStatus string

const (
	// Pending aguarda a primeira tentativa (ou um reenvio manual).
	Pending DeliveryStatus = "pending"
	// Delivering foi reivindicada por um worker (lease).
	Delivering DeliveryStatus = "delivering"
	// Succeeded recebeu resposta 2xx.
	Succeeded DeliveryStatus = "succeeded"
	// Retrying aguarda nova tentativa após falha.
	Retrying DeliveryStatus = "retrying"
	// Exhausted esgotou as tentativas ou não pode mais ser entregue.
	Exhausted DeliveryStatus = "exhausted"
)

// DefaultMaxAttempts limita as tentativas automáticas de uma entrega.
const DefaultMaxAttempts = 10

// MaxTotalAttempts limita as tentativas de uma entrega, inclusive as
// concedidas por reenvios manuais (o mesmo limite da coluna max_attempts).
const MaxTotalAttempts = 100

// Backoff exponencial: 30 s, 1 min, 2 min... limitado a 6 h, com jitter.
const (
	backoffBase = 30 * time.Second
	backoffCap  = 6 * time.Hour
)

// Backoff calcula a espera antes da próxima tentativa após attempt falhas.
// jitter deve estar em [0, 1); o fator aplicado fica entre 0,8 e 1,2.
func Backoff(attempt int, jitter float64) time.Duration {
	d := backoffBase
	for i := 1; i < attempt && d < backoffCap; i++ {
		d *= 2
	}
	d = min(d, backoffCap)
	jitter = min(max(jitter, 0), 1)
	return time.Duration(float64(d) * (0.8 + 0.4*jitter))
}

// Motivos de encerramento sem entrega.
const (
	ReasonEndpointDisabled   = "endpoint desativado"
	ReasonFeatureUnavailable = "recurso de webhooks indisponível no plano da plataforma"
	ReasonNoSecret           = "endpoint sem segredo de assinatura válido"
	ReasonSecretUnavailable  = "nenhum segredo de assinatura válido pôde ser decifrado; gere um novo segredo"
)

// Delivery é a entrega de um evento a um endpoint.
type Delivery struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	EndpointID     uuid.UUID
	EventID        uuid.UUID
	EventType      string
	Status         DeliveryStatus
	Attempts       int
	MaxAttempts    int
	NextAttemptAt  *time.Time
	LeaseUntil     *time.Time
	LastStatusCode *int
	LastError      string
	DeliveredAt    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewDelivery cria a entrega pendente de um evento.
func NewDelivery(id, endpointID, eventID uuid.UUID, eventType string, now time.Time) Delivery {
	return Delivery{ID: id, EndpointID: endpointID, EventID: eventID, EventType: eventType, Status: Pending,
		MaxAttempts: DefaultMaxAttempts, NextAttemptAt: &now, CreatedAt: now, UpdatedAt: now}
}

// Due informa se a entrega deve ser tentada: pendente ou em retentativa
// vencida, ou em andamento com lease expirado.
func (d Delivery) Due(now time.Time) bool {
	switch d.Status {
	case Pending, Retrying:
		return d.NextAttemptAt != nil && !d.NextAttemptAt.After(now)
	case Delivering:
		return d.LeaseUntil != nil && d.LeaseUntil.Before(now)
	}
	return false
}

// Claim reivindica a entrega para uma tentativa.
func (d *Delivery) Claim(now, leaseUntil time.Time) error {
	if !d.Due(now) {
		return apperr.Conflict("delivery_not_due", "A entrega não está pronta para nova tentativa.")
	}
	d.Status, d.LeaseUntil, d.NextAttemptAt, d.UpdatedAt = Delivering, &leaseUntil, nil, now
	d.Attempts++
	return nil
}

// Release devolve à fila uma entrega reivindicada e ainda não tentada, sem
// consumir a tentativa: volta a pendente (ou a retentativa, se já houve
// tentativas), com a próxima tentativa em next.
func (d *Delivery) Release(attemptsBefore int, now, next time.Time) error {
	if d.Status != Delivering {
		return apperr.Conflict("delivery_not_in_progress", "A entrega não está em andamento.")
	}
	d.Attempts, d.LeaseUntil, d.NextAttemptAt, d.UpdatedAt = attemptsBefore, nil, &next, now
	d.Status = Pending
	if d.Attempts > 0 {
		d.Status = Retrying
	}
	return nil
}

// Outcome é o resultado de uma tentativa.
type Outcome struct {
	StatusCode int
	Err        string
	// Permanent indica falha que não deve ser repetida (destino bloqueado).
	Permanent bool
}

// Success informa se o destino confirmou o recebimento (2xx).
func (o Outcome) Success() bool { return o.Err == "" && o.StatusCode >= 200 && o.StatusCode < 300 }

// Record aplica o resultado da tentativa corrente.
func (d *Delivery) Record(now time.Time, o Outcome, jitter float64) error {
	if d.Status != Delivering {
		return apperr.Conflict("delivery_not_in_progress", "A entrega não está em andamento.")
	}
	d.LeaseUntil, d.UpdatedAt = nil, now
	if o.StatusCode > 0 {
		code := o.StatusCode
		d.LastStatusCode = &code
	} else {
		d.LastStatusCode = nil
	}
	if o.Success() {
		d.Status, d.DeliveredAt, d.LastError = Succeeded, &now, ""
		return nil
	}
	d.LastError = describe(o)
	if o.Permanent || d.Attempts >= d.MaxAttempts {
		d.Status = Exhausted
		return nil
	}
	next := now.Add(Backoff(d.Attempts, jitter))
	d.Status, d.NextAttemptAt = Retrying, &next
	return nil
}

func describe(o Outcome) string {
	msg := strings.Join(strings.Fields(o.Err), " ")
	switch {
	case msg != "":
	case o.StatusCode >= 300 && o.StatusCode < 400:
		msg = "redirecionamento não seguido"
	default:
		msg = "resposta sem sucesso do destino"
	}
	return truncate(msg, 1000)
}

// Exhaust encerra a entrega sem nova tentativa.
func (d *Delivery) Exhaust(now time.Time, reason string) {
	d.Status, d.LeaseUntil, d.NextAttemptAt, d.LastError, d.UpdatedAt = Exhausted, nil, nil, reason, now
}

// ScheduleManualRetry agenda um reenvio imediato. Entregas concluídas
// (inclusive com sucesso) ganham uma tentativa adicional; em retentativa, a
// próxima tentativa é antecipada.
func (d *Delivery) ScheduleManualRetry(now time.Time) error {
	switch d.Status {
	case Pending, Delivering:
		return apperr.Conflict("delivery_in_progress", "A entrega já está agendada ou em andamento.")
	case Succeeded, Exhausted:
		if d.Attempts >= MaxTotalAttempts {
			return apperr.Conflict("delivery_attempts_limit", fmt.Sprintf("A entrega atingiu o limite de %d tentativas.", MaxTotalAttempts))
		}
		d.MaxAttempts = d.Attempts + 1
		d.DeliveredAt = nil
	}
	d.Status, d.NextAttemptAt, d.UpdatedAt = Pending, &now, now
	return nil
}

// Attempt é o registro de uma tentativa de entrega.
type Attempt struct {
	ID             uuid.UUID
	DeliveryID     uuid.UUID
	Number         int
	StartedAt      time.Time
	Duration       time.Duration
	StatusCode     *int
	Error          string
	SignatureCount int
}

// Event é o evento de domínio entregue aos endpoints.
type Event struct {
	ID         uuid.UUID
	Type       string
	Payload    json.RawMessage
	OccurredAt time.Time
}

// Body monta o corpo JSON da entrega (idêntico em todas as tentativas).
func (e Event) Body() ([]byte, error) {
	data := e.Payload
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	return json.Marshal(struct {
		ID        string          `json:"id"`
		Type      string          `json:"type"`
		CreatedAt time.Time       `json:"created_at"`
		Data      json.RawMessage `json:"data"`
	}{ID: e.ID.String(), Type: e.Type, CreatedAt: e.OccurredAt.UTC(), Data: data})
}
