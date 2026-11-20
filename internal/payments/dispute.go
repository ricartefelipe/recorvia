package payments

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// DisputeStatus é a situação de uma disputa (chargeback).
type DisputeStatus string

const (
	DisputeOpened      DisputeStatus = "opened"
	DisputeUnderReview DisputeStatus = "under_review"
	DisputeWon         DisputeStatus = "won"
	DisputeLost        DisputeStatus = "lost"
)

var disputeTransitions = map[DisputeStatus][]DisputeStatus{
	DisputeOpened:      {DisputeUnderReview, DisputeWon, DisputeLost},
	DisputeUnderReview: {DisputeWon, DisputeLost},
	DisputeWon:         {},
	DisputeLost:        {},
}

// Dispute é uma contestação do pagamento junto ao provedor. Na abertura, o
// valor contestado fica retido (lançamento dispute.opened); a disputa ganha
// estorna esse lançamento e a perdida registra o chargeback na cobrança.
type Dispute struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	ChargeID       uuid.UUID
	Provider       string
	Status         DisputeStatus
	Amount         money.Money
	ProviderStatus string
	OpenedAt       time.Time
	ClosedAt       *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Version        int
}

// Open informa se a disputa ainda não foi decidida.
func (d Dispute) Open() bool { return d.Status == DisputeOpened || d.Status == DisputeUnderReview }

// DisputeEvent é uma transição registrada no histórico da disputa.
type DisputeEvent struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	DisputeID      uuid.UUID
	From           *DisputeStatus
	To             DisputeStatus
	ProviderStatus string
	OccurredAt     time.Time
	RecordedAt     time.Time
	Details        map[string]any
}

// transition aplica a mudança de situação e devolve o evento do histórico.
// occurred é o instante informado pelo provedor; o encerramento nunca fica
// antes da abertura.
func (d *Dispute) transition(to DisputeStatus, providerStatus string, occurred, now time.Time) (DisputeEvent, error) {
	if !slices.Contains(disputeTransitions[d.Status], to) {
		return DisputeEvent{}, apperr.Conflict("invalid_dispute_transition", fmt.Sprintf("Transição de disputa não permitida: %s para %s.", d.Status, to))
	}
	from := d.Status
	d.Status, d.ProviderStatus, d.UpdatedAt = to, providerStatus, now
	if !d.Open() {
		closed := occurred
		if closed.Before(d.OpenedAt) {
			closed = d.OpenedAt
		}
		d.ClosedAt = &closed
	}
	return DisputeEvent{TenantID: d.TenantID, DisputeID: d.ID, From: &from, To: to, ProviderStatus: providerStatus,
		OccurredAt: occurred, RecordedAt: now}, nil
}

// disputeTarget traduz a situação do provedor na situação da disputa.
func disputeTarget(s ProviderStatus) (DisputeStatus, bool) {
	switch s {
	case ProviderChargebackRequested:
		return DisputeOpened, true
	case ProviderChargebackDispute:
		return DisputeUnderReview, true
	case ProviderChargebackReversed:
		return DisputeWon, true
	case ProviderChargebackLost:
		return DisputeLost, true
	}
	return "", false
}
