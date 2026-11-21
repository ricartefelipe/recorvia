package platformbilling

import (
	"encoding/base64"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// AddonStatus é a situação do pedido de serviço adicional.
type AddonStatus string

// Situações do pedido. O tenant registra o pedido (requested); a equipe da
// plataforma agenda, conclui ou cancela. Concluído e cancelado são finais.
const (
	AddonRequested AddonStatus = "requested"
	AddonScheduled AddonStatus = "scheduled"
	AddonCompleted AddonStatus = "completed"
	AddonCanceled  AddonStatus = "canceled"
)

// ParseAddonStatus valida a situação informada em filtros.
func ParseAddonStatus(s string) (AddonStatus, error) {
	switch st := AddonStatus(s); st {
	case AddonRequested, AddonScheduled, AddonCompleted, AddonCanceled:
		return st, nil
	}
	return "", apperr.Invalid("invalid_filter", "Filtro de situação inválido.",
		apperr.FieldError{Field: "status", Message: "Use requested, scheduled, completed ou canceled."})
}

// MaxScheduleAhead limita a data de atendimento agendada.
const MaxScheduleAhead = 366 * 24 * time.Hour

// AddonOrder é a solicitação de um serviço adicional (ex.: implantação assistida).
type AddonOrder struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	AddonCode   string
	Status      AddonStatus
	Notes       string
	Price       money.Money
	RequestedBy uuid.UUID
	CreatedAt   time.Time
	// ScheduledFor é o instante combinado para o atendimento.
	ScheduledFor *time.Time
	// CompletedAt é o instante da conclusão: o valor entra no primeiro
	// demonstrativo fechado depois dele.
	CompletedAt  *time.Time
	CanceledAt   *time.Time
	CancelReason string
	// HandledBy é o integrante da equipe da plataforma da última transição.
	HandledBy *uuid.UUID
	// StatementID é o demonstrativo que cobrou o pedido (somente concluídos).
	StatementID *uuid.UUID
	Version     int
}

// Open informa se o pedido ainda pode ser agendado, concluído ou cancelado.
func (o AddonOrder) Open() bool { return o.Status == AddonRequested || o.Status == AddonScheduled }

func (o AddonOrder) requireOpen() error {
	if !o.Open() {
		return apperr.Conflict("addon_order_closed", "O pedido já foi concluído ou cancelado.")
	}
	return nil
}

// Schedule agenda (ou reagenda) o atendimento.
func (o *AddonOrder) Schedule(at, now time.Time, by uuid.UUID) error {
	if err := o.requireOpen(); err != nil {
		return err
	}
	if at.IsZero() || at.Before(now) || at.Sub(now) > MaxScheduleAhead {
		return apperr.Invalid("invalid_addon_schedule", "Data de atendimento inválida.",
			apperr.FieldError{Field: "scheduled_for", Message: "Informe um instante futuro, em até um ano."})
	}
	at = at.UTC()
	o.Status, o.ScheduledFor, o.HandledBy = AddonScheduled, &at, &by
	return nil
}

// Complete registra a conclusão do atendimento. O pedido pode ser concluído
// sem agendamento prévio (atendimento imediato).
func (o *AddonOrder) Complete(now time.Time, by uuid.UUID) error {
	if err := o.requireOpen(); err != nil {
		return err
	}
	now = now.UTC()
	o.Status, o.CompletedAt, o.HandledBy = AddonCompleted, &now, &by
	return nil
}

// Cancel cancela o pedido em aberto, com justificativa.
func (o *AddonOrder) Cancel(reason string, now time.Time, by uuid.UUID) error {
	if err := o.requireOpen(); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if n := utf8.RuneCountInString(reason); n < 3 || n > 500 {
		return apperr.Invalid("invalid_addon_cancel", "Justificativa inválida.",
			apperr.FieldError{Field: "reason", Message: "Informe a justificativa com 3 a 500 caracteres."})
	}
	now = now.UTC()
	o.Status, o.CanceledAt, o.CancelReason, o.HandledBy = AddonCanceled, &now, reason, &by
	return nil
}

// StaffAddonOrder é o pedido visto pela equipe da plataforma, com a
// identificação da organização.
type StaffAddonOrder struct {
	AddonOrder
	TenantName string
	TenantSlug string
	AddonName  string
}

// Tamanho das páginas da lista da equipe da plataforma.
const (
	DefaultStaffPageSize = 50
	MaxStaffPageSize     = 200
)

// AddonCursor é a posição do último pedido de uma página: instante de
// ordenação da lista (solicitação, agendamento ou encerramento) e ID.
type AddonCursor struct {
	At time.Time
	ID uuid.UUID
}

// StaffAddonQuery é o pedido de uma página da lista da equipe.
type StaffAddonQuery struct {
	// Status vazio lista os pedidos em aberto.
	Status AddonStatus
	After  *AddonCursor
	// Limit é o número de itens pedidos ao armazenamento.
	Limit int
}

// StaffAddonPage é uma página da lista da equipe.
type StaffAddonPage struct {
	Items      []StaffAddonOrder
	NextCursor string
}

// SortKey devolve o instante pelo qual a lista com o filtro informado é
// ordenada: solicitação (em aberto e solicitados), agendamento, conclusão
// ou cancelamento.
func (o AddonOrder) SortKey(filter AddonStatus) time.Time {
	var at *time.Time
	switch filter {
	case AddonScheduled:
		at = o.ScheduledFor
	case AddonCompleted:
		at = o.CompletedAt
	case AddonCanceled:
		at = o.CanceledAt
	}
	if at == nil {
		return o.CreatedAt
	}
	return *at
}

var errInvalidAddonCursor = apperr.Invalid("invalid_cursor", "Cursor de paginação inválido.")

// cursorKind amarra o cursor ao filtro: o cursor de uma lista não vale em
// outra (a ordenação muda).
func cursorKind(st AddonStatus) string {
	if st == "" {
		return "ao-open"
	}
	return "ao-" + string(st)
}

// EncodeAddonCursor codifica a posição de forma opaca, truncada a
// microssegundos (a precisão do armazenamento).
func EncodeAddonCursor(st AddonStatus, c AddonCursor) string {
	raw := cursorKind(st) + "|" + c.At.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano) + "|" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseAddonCursor decodifica o cursor do filtro informado; vazio é a
// primeira página. Só a forma exata gerada por EncodeAddonCursor é aceita.
func ParseAddonCursor(st AddonStatus, s string) (*AddonCursor, error) {
	if s == "" {
		return nil, nil
	}
	if len(s) > 160 {
		return nil, errInvalidAddonCursor
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, errInvalidAddonCursor
	}
	parts := strings.Split(string(b), "|")
	if len(parts) != 3 || parts[0] != cursorKind(st) {
		return nil, errInvalidAddonCursor
	}
	at, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return nil, errInvalidAddonCursor
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return nil, errInvalidAddonCursor
	}
	c := AddonCursor{At: at.UTC(), ID: id}
	if EncodeAddonCursor(st, c) != s {
		return nil, errInvalidAddonCursor
	}
	return &c, nil
}
