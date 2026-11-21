package notifications

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

// Status é a situação de uma notificação.
type Status string

const (
	// Scheduled aguarda o instante de envio (send_after).
	Scheduled Status = "scheduled"
	// Sending foi reivindicada por um worker (lease) e está em envio.
	Sending Status = "sending"
	// Sent foi aceita pelo servidor SMTP.
	Sent Status = "sent"
	// Failed falhou definitivamente (erro permanente ou tentativas esgotadas).
	Failed Status = "failed"
	// Canceled deixou de ser necessária (por exemplo, fatura paga).
	Canceled Status = "canceled"
	// Suppressed não é enviada por preferência de contato ou falta de destinatário.
	Suppressed Status = "suppressed"
)

// Motivos estáveis registrados no histórico e em status_reason.
const (
	ReasonScheduled             = "scheduled"
	ReasonNoRecipient           = "no_recipient"
	ReasonLinkUnavailable       = "invitation_link_unavailable"
	ReasonInvoicePaid           = "invoice_paid"
	ReasonInvoiceCredited       = "invoice_credited"
	ReasonInvoiceVoided         = "invoice_voided"
	ReasonInvoiceUncollectible  = "invoice_uncollectible"
	ReasonSuperseded            = "superseded"
	ReasonDueDatePassed         = "due_date_passed"
	ReasonOutsideWindow         = "outside_send_window"
	ReasonFrequencyLimit        = "frequency_limit"
	ReasonInvitationNotPending  = "invitation_not_pending"
	ReasonSubscriptionNotActive = "subscription_not_suspended"
	ReasonFeatureUnavailable    = "feature_unavailable"
	ReasonAttempt               = "attempt"
	ReasonSent                  = "sent"
	ReasonRetryScheduled        = "retry_scheduled"
	ReasonPermanentFailure      = "permanent_failure"
	ReasonAttemptsExhausted     = "attempts_exhausted"
	ReasonContentUnavailable    = "content_unavailable"
)

// DefaultMaxAttempts limita as tentativas de envio de cada notificação.
const DefaultMaxAttempts = 5

// retryDelays é a espera base antes de cada nova tentativa (após a 1ª, 2ª...).
var retryDelays = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

// RetryDelay calcula a espera antes da próxima tentativa, com jitter. jitter
// deve estar em [0, 1); o fator aplicado fica entre 0,8 e 1,2.
func RetryDelay(attempt int, jitter float64) time.Duration {
	i := min(max(attempt, 1), len(retryDelays)) - 1
	jitter = min(max(jitter, 0), 1)
	return time.Duration(float64(retryDelays[i]) * (0.8 + 0.4*jitter))
}

// Event é uma entrada do histórico da notificação.
type Event struct {
	Status  Status
	Reason  string
	Attempt int
	Detail  string
	At      time.Time
}

// Content é o corpo renderizado (texto e HTML).
type Content struct {
	Text string `json:"text"`
	HTML string `json:"html"`
}

// Notification é o agregado de notificação.
type Notification struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	BusinessKey string
	TemplateKey TemplateKey
	Channel     string
	Recipients  []string
	Subject     string
	Content     Content
	// ContentError descreve por que o conteúdo cifrado não pôde ser lido
	// (chave retirada do keyring ou dado corrompido); vazio quando legível.
	ContentError   string
	CustomerID     *uuid.UUID
	InvoiceID      *uuid.UUID
	SubscriptionID *uuid.UUID
	InvitationID   *uuid.UUID
	PolicyID       *uuid.UUID
	StepOffsetDays *int
	Status         Status
	// StatusReason é o motivo da situação atual: cancelamento, supressão,
	// falha ou adiamento (janela, limite de frequência, nova tentativa);
	// vazio em envio e depois do envio.
	StatusReason string
	SendAfter    time.Time
	Attempts     int
	MaxAttempts  int
	LeaseUntil   *time.Time
	LastError    string
	MessageID    string
	SentAt       *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time

	pending []Event
}

// PopEvents devolve e limpa as entradas de histórico ainda não gravadas.
func (n *Notification) PopEvents() []Event {
	ev := n.pending
	n.pending = nil
	return ev
}

func (n *Notification) record(reason, detail string, at time.Time) {
	n.pending = append(n.pending, Event{Status: n.Status, Reason: reason, Attempt: n.Attempts, Detail: truncateRunes(detail, 1000), At: at})
}

// IsReminder informa se a notificação é um lembrete da régua de cobrança.
func (n Notification) IsReminder() bool { return n.PolicyID != nil }

// Final informa se a notificação não muda mais de situação.
func (n Notification) Final() bool {
	switch n.Status {
	case Sent, Failed, Canceled, Suppressed:
		return true
	}
	return false
}

// Due informa se a notificação deve ser avaliada pelo envio: agendada e
// vencida, ou em envio com lease expirado (worker interrompido).
func (n Notification) Due(now time.Time) bool {
	switch n.Status {
	case Scheduled:
		return !n.SendAfter.After(now)
	case Sending:
		return n.LeaseUntil != nil && n.LeaseUntil.Before(now)
	}
	return false
}

func errFinal() error {
	return apperr.Conflict("notification_final", "A notificação já foi concluída.")
}

// Claim reivindica a notificação para uma tentativa de envio com lease.
func (n *Notification) Claim(now, leaseUntil time.Time) error {
	if !n.Due(now) {
		return apperr.Conflict("notification_not_due", "A notificação não está pronta para envio.")
	}
	if !leaseUntil.After(now) {
		return fmt.Errorf("notifications: lease deve terminar depois de %s", now)
	}
	n.Status, n.StatusReason, n.LeaseUntil, n.UpdatedAt = Sending, "", &leaseUntil, now
	n.Attempts++
	n.record(ReasonAttempt, "", now)
	return nil
}

// MarkSent registra o aceite pelo servidor SMTP. Destinatários recusados
// pelo servidor (os demais receberam a mensagem) ficam em LastError e no
// histórico.
func (n *Notification) MarkSent(now time.Time, messageID string, refused ...string) error {
	if n.Status != Sending {
		return apperr.Conflict("notification_not_sending", "A notificação não está em envio.")
	}
	n.Status, n.SentAt, n.LeaseUntil, n.UpdatedAt = Sent, &now, nil, now
	n.MessageID, n.LastError, n.StatusReason = messageID, "", ""
	if len(refused) > 0 {
		n.LastError = truncateRunes(sanitizeError("Destinatários recusados: "+strings.Join(refused, "; ")), 1000)
	}
	n.record(ReasonSent, n.LastError, now)
	return nil
}

// MarkAttemptFailed registra a falha de uma tentativa: erro permanente ou
// tentativas esgotadas encerram a notificação como failed; caso contrário,
// ela volta a scheduled para retryAt.
func (n *Notification) MarkAttemptFailed(now time.Time, cause string, permanent bool, retryAt time.Time) error {
	if n.Status != Sending {
		return apperr.Conflict("notification_not_sending", "A notificação não está em envio.")
	}
	n.LastError, n.LeaseUntil, n.UpdatedAt = truncateRunes(sanitizeError(cause), 1000), nil, now
	switch {
	case permanent:
		n.Status, n.StatusReason = Failed, ReasonPermanentFailure
		n.record(ReasonPermanentFailure, n.LastError, now)
	case n.Attempts >= n.MaxAttempts:
		n.Status, n.StatusReason = Failed, ReasonAttemptsExhausted
		n.record(ReasonAttemptsExhausted, n.LastError, now)
	default:
		n.Status, n.StatusReason, n.SendAfter = Scheduled, ReasonRetryScheduled, retryAt
		n.record(ReasonRetryScheduled, n.LastError, now)
	}
	return nil
}

// Fail encerra a notificação como failed sem tentativa de envio (por
// exemplo, conteúdo cifrado que não pode mais ser lido).
func (n *Notification) Fail(now time.Time, reason, detail string) error {
	if n.Final() {
		return errFinal()
	}
	n.Status, n.StatusReason, n.LeaseUntil, n.UpdatedAt = Failed, reason, nil, now
	n.LastError = truncateRunes(sanitizeError(detail), 1000)
	n.record(reason, n.LastError, now)
	return nil
}

// Cancel encerra a notificação sem envio, com o motivo.
func (n *Notification) Cancel(now time.Time, reason string) error {
	if n.Final() {
		return errFinal()
	}
	n.Status, n.StatusReason, n.LeaseUntil, n.UpdatedAt = Canceled, reason, nil, now
	n.record(reason, "", now)
	return nil
}

// Suppress encerra a notificação sem envio por falta de destinatário ou
// preferência de contato.
func (n *Notification) Suppress(now time.Time, reason string) error {
	if n.Final() {
		return errFinal()
	}
	n.Status, n.StatusReason, n.LeaseUntil, n.UpdatedAt = Suppressed, reason, nil, now
	n.record(reason, "", now)
	return nil
}

// Defer adia o envio (janela de horário ou limite de frequência).
func (n *Notification) Defer(now, until time.Time, reason string) error {
	if n.Final() {
		return errFinal()
	}
	n.Status, n.StatusReason, n.SendAfter, n.LeaseUntil, n.UpdatedAt = Scheduled, reason, until, nil, now
	n.record(reason, until.UTC().Format(time.RFC3339), now)
	return nil
}

// sanitizeError reduz a mensagem de erro a uma linha.
func sanitizeError(s string) string { return strings.Join(strings.Fields(cleanValue(s)), " ") }

// Window é a janela diária de envio [StartHour, EndHour) no fuso do tenant.
type Window struct {
	StartHour int
	EndHour   int
}

// Valid informa se a janela é coerente (0 <= início < fim <= 24).
func (w Window) Valid() bool { return w.StartHour >= 0 && w.StartHour < w.EndHour && w.EndHour <= 24 }

func (w Window) bounds(t time.Time, loc *time.Location) (time.Time, time.Time) {
	l := t.In(loc)
	y, m, d := l.Date()
	return time.Date(y, m, d, w.StartHour, 0, 0, 0, loc), time.Date(y, m, d, w.EndHour, 0, 0, 0, loc)
}

// Contains informa se o instante está dentro da janela do seu dia civil.
func (w Window) Contains(t time.Time, loc *time.Location) bool {
	start, end := w.bounds(t, loc)
	return !t.Before(start) && t.Before(end)
}

// Next devolve o primeiro instante igual ou posterior a t dentro da janela.
func (w Window) Next(t time.Time, loc *time.Location) time.Time {
	start, end := w.bounds(t, loc)
	switch {
	case t.Before(start):
		return start.UTC()
	case t.Before(end):
		return t.UTC()
	}
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d+1, w.StartHour, 0, 0, 0, loc).UTC()
}

// NextDayStart devolve a abertura da janela no dia civil seguinte ao de t.
func (w Window) NextDayStart(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d+1, w.StartHour, 0, 0, 0, loc).UTC()
}

// DayBounds devolve o início e o fim (exclusivo) do dia civil de t no fuso.
func DayBounds(t time.Time, loc *time.Location) (time.Time, time.Time) {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc).UTC(), time.Date(y, m, d+1, 0, 0, 0, 0, loc).UTC()
}
