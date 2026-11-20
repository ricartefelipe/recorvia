// Package collections implementa a régua de cobrança e a inadimplência:
// políticas por tenant com passos relativos ao vencimento, janela de envio no
// fuso do tenant e limite diário de mensagens por cliente; marcação de faturas
// vencidas (assinatura past_due e evento invoice.overdue); suspensão da
// assinatura após N dias de atraso e reativação quando as faturas vencidas
// são regularizadas, conforme a política, com histórico.
//
// O job da régua é idempotente e seguro com várias instâncias: cada lembrete
// tem identidade de negócio (fatura + passo) e as mudanças de assinatura
// ocorrem com a assinatura bloqueada, na ordem fatura, assinatura e cliente.
package collections

import (
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/notifications"
)

// Limites das políticas (os mesmos do contrato da API).
const (
	MinOffsetDays     = -30
	MaxOffsetDays     = 90
	MaxSteps          = 10
	MaxMessagesPerDay = 5
	MaxSuspendDays    = 180
	// ReminderHorizonDays cobre o passo mais antecipado (30 dias antes).
	ReminderHorizonDays = -MinOffsetDays + 1
)

// Policy é a política de cobrança do tenant.
type Policy struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	Name                string
	IsDefault           bool
	OffsetsDays         []int
	Window              notifications.Window
	MaxMessagesPerDay   int
	SuspendAfterDays    *int
	ReactivateOnPayment bool
	CreatedAt           time.Time
	UpdatedAt           time.Time
	Version             int
}

// PolicyInput são os dados de criação e alteração.
type PolicyInput struct {
	Name                string
	IsDefault           bool
	OffsetsDays         []int
	WindowStartHour     int
	WindowEndHour       int
	MaxMessagesPerDay   int
	SuspendAfterDays    *int
	ReactivateOnPayment bool
}

func validate(in PolicyInput) (PolicyInput, error) {
	var f []apperr.FieldError
	in.Name = strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(in.Name); n < 2 || n > 100 || strings.ContainsRune(in.Name, 0) {
		f = append(f, apperr.FieldError{Field: "name", Message: "Informe um nome com 2 a 100 caracteres."})
	}
	offsets := slices.Clone(in.OffsetsDays)
	slices.Sort(offsets)
	switch {
	case len(offsets) > MaxSteps:
		f = append(f, apperr.FieldError{Field: "reminder_offsets_days", Message: "Informe no máximo 10 passos."})
	case len(offsets) > 0 && (offsets[0] < MinOffsetDays || offsets[len(offsets)-1] > MaxOffsetDays):
		f = append(f, apperr.FieldError{Field: "reminder_offsets_days", Message: "Cada passo deve estar entre 30 dias antes e 90 dias depois do vencimento."})
	case len(slices.Compact(slices.Clone(offsets))) != len(offsets):
		f = append(f, apperr.FieldError{Field: "reminder_offsets_days", Message: "Os passos não podem se repetir."})
	}
	if in.OffsetsDays == nil {
		offsets = []int{}
	}
	in.OffsetsDays = offsets
	if in.WindowStartHour < 0 || in.WindowStartHour > 23 {
		f = append(f, apperr.FieldError{Field: "send_window_start_hour", Message: "Informe uma hora entre 0 e 23."})
	}
	if in.WindowEndHour < 1 || in.WindowEndHour > 24 {
		f = append(f, apperr.FieldError{Field: "send_window_end_hour", Message: "Informe uma hora entre 1 e 24."})
	} else if in.WindowStartHour >= in.WindowEndHour && in.WindowStartHour <= 23 && in.WindowStartHour >= 0 {
		f = append(f, apperr.FieldError{Field: "send_window_end_hour", Message: "O fim da janela deve ser posterior ao início."})
	}
	if in.MaxMessagesPerDay < 1 || in.MaxMessagesPerDay > MaxMessagesPerDay {
		f = append(f, apperr.FieldError{Field: "max_messages_per_day", Message: "Informe entre 1 e 5 mensagens por dia."})
	}
	if d := in.SuspendAfterDays; d != nil && (*d < 1 || *d > MaxSuspendDays) {
		f = append(f, apperr.FieldError{Field: "suspend_after_days", Message: "Informe entre 1 e 180 dias."})
	}
	if len(f) > 0 {
		return PolicyInput{}, apperr.Invalid("invalid_collection_policy", "Política de cobrança inválida.", f...)
	}
	return in, nil
}

// NewPolicy valida e cria uma política.
func NewPolicy(id, tenantID uuid.UUID, in PolicyInput, now time.Time) (Policy, error) {
	in, err := validate(in)
	if err != nil {
		return Policy{}, err
	}
	p := Policy{ID: id, TenantID: tenantID, CreatedAt: now, Version: 1}
	p.set(in, now)
	return p, nil
}

// Apply altera a política com os dados validados.
func (p *Policy) Apply(in PolicyInput, now time.Time) error {
	in, err := validate(in)
	if err != nil {
		return err
	}
	p.set(in, now)
	return nil
}

// Input devolve os dados editáveis da política, base das alterações
// parciais (campos omitidos mantêm o valor atual).
func (p Policy) Input() PolicyInput {
	var suspend *int
	if p.SuspendAfterDays != nil {
		d := *p.SuspendAfterDays
		suspend = &d
	}
	return PolicyInput{Name: p.Name, IsDefault: p.IsDefault, OffsetsDays: slices.Clone(p.OffsetsDays), WindowStartHour: p.Window.StartHour,
		WindowEndHour: p.Window.EndHour, MaxMessagesPerDay: p.MaxMessagesPerDay, SuspendAfterDays: suspend,
		ReactivateOnPayment: p.ReactivateOnPayment}
}

func (p *Policy) set(in PolicyInput, now time.Time) {
	p.Name, p.IsDefault, p.OffsetsDays = in.Name, in.IsDefault, in.OffsetsDays
	p.Window = notifications.Window{StartHour: in.WindowStartHour, EndHour: in.WindowEndHour}
	p.MaxMessagesPerDay, p.SuspendAfterDays, p.ReactivateOnPayment = in.MaxMessagesPerDay, in.SuspendAfterDays, in.ReactivateOnPayment
	p.UpdatedAt = now
}

// TemplateForOffset devolve o template do passo: até o dia do vencimento,
// lembrete de vencimento próximo; depois dele, lembrete de fatura vencida.
func TemplateForOffset(offsetDays int) notifications.TemplateKey {
	if offsetDays <= 0 {
		return notifications.KeyReminderBeforeDue
	}
	return notifications.KeyReminderAfterDue
}

// DueStep devolve o passo mais recente já alcançado em today para uma fatura
// emitida em issued com vencimento em due. Passos anteriores à emissão não se
// aplicam, nem passos até o vencimento depois que ele passou; passos
// alcançados e ainda não enviados são substituídos pelo mais recente (sem
// rajadas de lembretes após indisponibilidade do worker).
func (p Policy) DueStep(issued, due, today calendar.Date) (int, bool) {
	found, step := false, 0
	for _, off := range p.OffsetsDays {
		d := due.AddDays(off)
		if d.Before(issued) || today.Before(d) || (off <= 0 && due.Before(today)) {
			continue
		}
		found, step = true, off
	}
	return step, found
}

// SuspendBefore devolve o limite de vencimento (due_at) a partir do qual uma
// fatura em aberto já atingiu os dias de atraso da suspensão em now: uma
// fatura vencida no dia D atinge N dias de atraso no dia D+N, isto é, quando
// due_at (início do dia D+1) <= início do dia today-N+1.
func (p Policy) SuspendBefore(now time.Time, loc *time.Location) *time.Time {
	if p.SuspendAfterDays == nil {
		return nil
	}
	t := calendar.DateOf(now, loc).AddDays(1 - *p.SuspendAfterDays).StartIn(loc)
	return &t
}

// DaysOverdue conta os dias civis de atraso de um vencimento em today.
func DaysOverdue(due, today calendar.Date) int { return max(calendar.DaysBetween(due, today), 0) }
