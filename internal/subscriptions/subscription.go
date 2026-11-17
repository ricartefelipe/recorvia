// Package subscriptions modela as assinaturas dos clientes dos tenants:
// trial, início, renovação por ciclos ancorados, quantidade contratada,
// suspensão, reativação e cancelamento, com máquina de estados explícita.
//
// Política de cobrança: componentes fixos são cobrados antecipadamente no
// início de cada período; consumo medido é cobrado no período seguinte
// (pós-pago). O índice NextBillingIndex aponta o próximo período a faturar
// e NextBillingAt o seu início; o motor de faturamento processa todos os
// períodos vencidos, inclusive após indisponibilidade do agendador.
package subscriptions

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
)

// Status é a situação da assinatura.
type Status string

const (
	Trialing  Status = "trialing"
	Active    Status = "active"
	PastDue   Status = "past_due"
	Suspended Status = "suspended"
	Canceled  Status = "canceled"
)

// transitions define as transições válidas da máquina de estados.
var transitions = map[Status][]Status{
	Trialing:  {Active, Suspended, Canceled},
	Active:    {PastDue, Suspended, Canceled},
	PastDue:   {Active, Suspended, Canceled},
	Suspended: {Active, Canceled},
	Canceled:  {},
}

// CanTransition informa se a transição é permitida.
func CanTransition(from, to Status) bool { return slices.Contains(transitions[from], to) }

// MaxQuantity limita a quantidade contratada.
const MaxQuantity = 1_000_000

// HistoryEvent registra uma mudança relevante da assinatura.
type HistoryEvent struct {
	Event   string
	From    *Status
	To      Status
	At      time.Time
	Details map[string]any
}

// Subscription é o agregado de assinatura.
type Subscription struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	CustomerID        uuid.UUID
	PlanID            uuid.UUID
	PriceVersionID    uuid.UUID
	Status            Status
	Quantity          int
	Interval          calendar.Interval
	Timezone          string
	Anchor            calendar.Date
	TrialEndsAt       *time.Time
	StartedAt         time.Time
	CurrentPeriod     calendar.Period
	NextBillingIndex  int
	NextBillingAt     *time.Time
	CancelAtPeriodEnd bool
	CanceledAt        *time.Time
	CancelReason      string
	EndedAt           *time.Time
	SuspendedAt       *time.Time
	SuspensionReason  string
	PaymentTermsDays  *int
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Version           int

	pending []HistoryEvent
}

// Location retorna o fuso das regras de calendário do contrato.
func (s Subscription) Location() (*time.Location, error) { return time.LoadLocation(s.Timezone) }

// Schedule retorna o ciclo de cobrança ancorado.
func (s Subscription) Schedule() (calendar.Schedule, error) {
	loc, err := s.Location()
	if err != nil {
		return calendar.Schedule{}, err
	}
	return calendar.NewScheduleFromAnchor(s.Interval, s.Anchor, loc)
}

// PopEvents retorna e limpa os eventos de histórico pendentes.
func (s *Subscription) PopEvents() []HistoryEvent {
	ev := s.pending
	s.pending = nil
	return ev
}

func (s *Subscription) record(event string, from *Status, at time.Time, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	s.pending = append(s.pending, HistoryEvent{Event: event, From: from, To: s.Status, At: at, Details: details})
}

func (s *Subscription) transition(to Status, event string, at time.Time, details map[string]any) error {
	if !CanTransition(s.Status, to) {
		return apperr.Conflict("invalid_transition",
			fmt.Sprintf("Transição de assinatura não permitida: %s para %s.", s.Status, to))
	}
	from := s.Status
	s.Status = to
	s.record(event, &from, at, details)
	return nil
}

// NewInput são os parâmetros de criação.
type NewInput struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	CustomerID uuid.UUID
	Plan       catalog.Plan
	Price      catalog.PriceVersion
	Quantity   int
	StartAt    time.Time
	TrialDays  int
	Location   *time.Location
	Now        time.Time
	PaymentTermsDays *int
}

// MaxBackdate limita o início retroativo (migração de contratos existentes).
const MaxBackdate = 366 * 24 * time.Hour

// New cria a assinatura. Com trial, o ciclo é ancorado na data local de fim
// do trial; sem trial, na data local de início. O início não pode estar no
// futuro e pode ser retroativo em até MaxBackdate (períodos passados são
// faturados pelo motor de recorrência).
func New(in NewInput) (Subscription, error) {
	var f []apperr.FieldError
	if in.Quantity < 1 || in.Quantity > MaxQuantity {
		f = append(f, apperr.FieldError{Field: "quantity", Message: "A quantidade deve ficar entre 1 e 1.000.000."})
	}
	if in.Price.Model != catalog.PerUnit && in.Quantity != 1 {
		f = append(f, apperr.FieldError{Field: "quantity", Message: "Somente planos por quantidade aceitam quantidade diferente de 1."})
	}
	if in.TrialDays < 0 || in.TrialDays > 365 {
		f = append(f, apperr.FieldError{Field: "trial_days", Message: "O trial deve ficar entre 0 e 365 dias."})
	}
	if in.StartAt.After(in.Now) {
		f = append(f, apperr.FieldError{Field: "start_date", Message: "A data de início não pode estar no futuro."})
	}
	if in.Now.Sub(in.StartAt) > MaxBackdate {
		f = append(f, apperr.FieldError{Field: "start_date", Message: "O início retroativo é limitado a 366 dias."})
	}
	if in.Plan.Status != catalog.Active {
		f = append(f, apperr.FieldError{Field: "plan_id", Message: "O plano está arquivado."})
	}
	if in.Price.PlanID != in.Plan.ID {
		f = append(f, apperr.FieldError{Field: "plan_id", Message: "Versão de preço não pertence ao plano."})
	}
	if len(f) > 0 {
		return Subscription{}, apperr.Invalid("invalid_subscription", "Dados da assinatura inválidos.", f...)
	}
	s := Subscription{
		ID: in.ID, TenantID: in.TenantID, CustomerID: in.CustomerID, PlanID: in.Plan.ID, PriceVersionID: in.Price.ID,
		Quantity: in.Quantity, Interval: in.Plan.Interval, Timezone: in.Location.String(), StartedAt: in.StartAt.UTC(),
		PaymentTermsDays: in.PaymentTermsDays, CreatedAt: in.Now, UpdatedAt: in.Now, Version: 1,
	}
	startDate := calendar.DateOf(in.StartAt, in.Location)
	if in.TrialDays > 0 {
		trialEnd := startDate.AddDays(in.TrialDays).StartIn(in.Location)
		s.Status = Trialing
		s.TrialEndsAt = &trialEnd
		s.Anchor = calendar.DateOf(trialEnd, in.Location)
		s.CurrentPeriod = calendar.Period{Start: startDate.StartIn(in.Location), End: trialEnd}
		s.NextBillingAt = &trialEnd
	} else {
		s.Status = Active
		s.Anchor = startDate
		sched, err := s.Schedule()
		if err != nil {
			return Subscription{}, err
		}
		s.CurrentPeriod = sched.Period(0)
		start := s.CurrentPeriod.Start
		s.NextBillingAt = &start
	}
	s.record("created", nil, in.Now, map[string]any{"quantity": s.Quantity, "price_version_id": s.PriceVersionID, "trial_days": in.TrialDays})
	return s, nil
}

// BillingDue informa se há período (ou fechamento final) a faturar.
func (s Subscription) BillingDue(now time.Time) bool {
	return s.NextBillingAt != nil && !s.NextBillingAt.After(now) && s.Status != Suspended
}

// StepKind é o tipo de passo do motor de faturamento.
type StepKind string

const (
	// StepBill fatura o próximo período (fixo antecipado e consumo anterior).
	StepBill StepKind = "bill"
	// StepEnd encerra a assinatura no fim do período, faturando só o consumo final.
	StepEnd StepKind = "end"
	// StepFinalUsage fatura o consumo do período interrompido por cancelamento imediato.
	StepFinalUsage StepKind = "final_usage"
)

// BillingStep descreve o que o motor deve faturar.
type BillingStep struct {
	Kind        StepKind
	Index       int
	Period      calendar.Period
	UsagePeriod *calendar.Period
}

// NextStep calcula o próximo passo de faturamento.
func (s Subscription) NextStep(now time.Time) (BillingStep, bool, error) {
	if !s.BillingDue(now) {
		return BillingStep{}, false, nil
	}
	if s.Status == Canceled {
		if s.EndedAt == nil {
			return BillingStep{}, false, nil
		}
		usage := calendar.Period{Start: s.CurrentPeriod.Start, End: *s.EndedAt}
		if !usage.Start.Before(usage.End) {
			return BillingStep{Kind: StepFinalUsage, Index: s.NextBillingIndex}, true, nil
		}
		return BillingStep{Kind: StepFinalUsage, Index: s.NextBillingIndex, Period: usage, UsagePeriod: &usage}, true, nil
	}
	sched, err := s.Schedule()
	if err != nil {
		return BillingStep{}, false, err
	}
	idx := s.NextBillingIndex
	step := BillingStep{Kind: StepBill, Index: idx, Period: sched.Period(idx)}
	if idx > 0 {
		prev := sched.Period(idx - 1)
		step.UsagePeriod = &prev
	}
	if s.CancelAtPeriodEnd {
		step.Kind = StepEnd
	}
	return step, true, nil
}

// ApplyStep registra o resultado de um passo de faturamento.
func (s *Subscription) ApplyStep(step BillingStep, at time.Time) error {
	switch step.Kind {
	case StepBill:
		sched, err := s.Schedule()
		if err != nil {
			return err
		}
		if s.Status == Trialing {
			if err := s.transition(Active, "trial_ended", at, nil); err != nil {
				return err
			}
		}
		s.CurrentPeriod = step.Period
		s.NextBillingIndex = step.Index + 1
		next := sched.Period(step.Index + 1).Start
		s.NextBillingAt = &next
		s.record("renewed", nil, at, map[string]any{"period_start": step.Period.Start, "period_index": step.Index})
	case StepEnd:
		end := step.Period.Start
		s.EndedAt = &end
		s.NextBillingAt = nil
		return s.transition(Canceled, "ended_at_period_end", at, map[string]any{"ended_at": end})
	case StepFinalUsage:
		s.NextBillingAt = nil
		s.record("final_usage_billed", nil, at, nil)
	}
	return nil
}

// CancelMode define quando o cancelamento vale.
type CancelMode string

const (
	CancelImmediately CancelMode = "immediate"
	CancelAtPeriodEnd CancelMode = "period_end"
)

// Cancel cancela a assinatura. Imediato: encerra agora; se o plano tiver
// consumo, o consumo do período interrompido é faturado no fechamento
// final. Ao fim do período: mantém o acesso até o fim do período corrente.
func (s *Subscription) Cancel(mode CancelMode, reason string, hasUsage bool, at time.Time) error {
	if len([]rune(reason)) > 500 {
		return apperr.Invalid("invalid_reason", "O motivo deve ter no máximo 500 caracteres.")
	}
	switch mode {
	case CancelImmediately:
		if err := s.transition(Canceled, "canceled", at, map[string]any{"mode": mode}); err != nil {
			return err
		}
		s.CanceledAt, s.EndedAt, s.CancelReason = &at, &at, reason
		s.CancelAtPeriodEnd = false
		if hasUsage && s.NextBillingIndex > 0 && at.After(s.CurrentPeriod.Start) {
			s.NextBillingAt = &at
		} else {
			s.NextBillingAt = nil
		}
		return nil
	case CancelAtPeriodEnd:
		if s.Status == Canceled {
			return apperr.Conflict("invalid_transition", "A assinatura já está cancelada.")
		}
		if s.CancelAtPeriodEnd {
			return apperr.Conflict("already_scheduled", "O cancelamento ao fim do período já está agendado.")
		}
		s.CancelAtPeriodEnd, s.CanceledAt, s.CancelReason = true, &at, reason
		s.record("cancel_scheduled", nil, at, map[string]any{"effective_at": s.CurrentPeriod.End})
		return nil
	}
	return apperr.Invalid("invalid_cancel_mode", "Modo de cancelamento inválido.", apperr.FieldError{Field: "mode", Message: "Use immediate ou period_end."})
}

// ResumeRenewal desfaz um cancelamento agendado para o fim do período.
func (s *Subscription) ResumeRenewal(at time.Time) error {
	if s.Status == Canceled || !s.CancelAtPeriodEnd {
		return apperr.Conflict("no_scheduled_cancel", "Não há cancelamento agendado.")
	}
	s.CancelAtPeriodEnd, s.CanceledAt, s.CancelReason = false, nil, ""
	s.record("cancel_unscheduled", nil, at, nil)
	return nil
}

// Suspend interrompe o faturamento (por exemplo, por inadimplência).
func (s *Subscription) Suspend(reason string, at time.Time) error {
	if len([]rune(reason)) > 500 {
		return apperr.Invalid("invalid_reason", "O motivo deve ter no máximo 500 caracteres.")
	}
	if err := s.transition(Suspended, "suspended", at, map[string]any{"reason_code": reasonCode(reason)}); err != nil {
		return err
	}
	s.SuspendedAt, s.SuspensionReason, s.NextBillingAt = &at, reason, nil
	return nil
}

func reasonCode(r string) string {
	if r == "" {
		return "manual"
	}
	return "informed"
}

// Reactivate retoma uma assinatura suspensa. Política: períodos inteiros
// decorridos durante a suspensão não são faturados (o serviço não foi
// prestado); o faturamento retoma no período que contém o instante da
// reativação, se ainda não faturado.
func (s *Subscription) Reactivate(at time.Time) error {
	if s.Status != Suspended {
		return apperr.Conflict("invalid_transition", "Somente assinaturas suspensas podem ser reativadas.")
	}
	sched, err := s.Schedule()
	if err != nil {
		return err
	}
	idx := sched.IndexOf(at)
	if idx < 0 {
		idx = 0
	}
	skipped := 0
	if idx > s.NextBillingIndex {
		skipped = idx - s.NextBillingIndex
		s.NextBillingIndex = idx
	}
	if s.TrialEndsAt != nil && at.Before(*s.TrialEndsAt) {
		s.CurrentPeriod = calendar.Period{Start: s.CurrentPeriod.Start, End: *s.TrialEndsAt}
	} else {
		s.CurrentPeriod = sched.Period(max(idx, 0))
	}
	next := sched.Period(s.NextBillingIndex).Start
	if s.TrialEndsAt != nil && s.NextBillingIndex == 0 {
		next = *s.TrialEndsAt
	}
	s.NextBillingAt = &next
	s.SuspendedAt, s.SuspensionReason = nil, ""
	return s.transition(Active, "reactivated", at, map[string]any{"skipped_periods": skipped})
}

// MarkPastDue indica fatura vencida em aberto.
func (s *Subscription) MarkPastDue(at time.Time) error {
	if s.Status == PastDue {
		return nil
	}
	return s.transition(PastDue, "past_due", at, nil)
}

// MarkCurrent volta a ativa quando não há mais faturas vencidas.
func (s *Subscription) MarkCurrent(at time.Time) error {
	if s.Status != PastDue {
		return nil
	}
	return s.transition(Active, "payment_recovered", at, nil)
}
