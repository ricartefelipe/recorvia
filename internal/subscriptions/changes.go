package subscriptions

import (
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// ChangeTiming define quando uma mudança de plano ou quantidade vale.
type ChangeTiming string

const (
	// ChangeImmediately aplica a mudança agora, com prorrata do período corrente.
	ChangeImmediately ChangeTiming = "immediate"
	// ChangeNextCycle agenda a mudança para o início do próximo período.
	ChangeNextCycle ChangeTiming = "next_cycle"
)

// ChangeKind é o tipo de mudança agendada.
type ChangeKind string

const (
	PlanChange     ChangeKind = "plan_change"
	QuantityChange ChangeKind = "quantity_change"
)

// ChangeStatus é a situação de uma mudança agendada.
type ChangeStatus string

const (
	ChangePending  ChangeStatus = "pending"
	ChangeApplied  ChangeStatus = "applied"
	ChangeCanceled ChangeStatus = "canceled"
)

// ScheduledChange é uma mudança de plano ou quantidade agendada para o início
// de um período. A versão de preço fica fixada no agendamento.
type ScheduledChange struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	SubscriptionID    uuid.UUID
	Kind              ChangeKind
	NewPlanID         *uuid.UUID
	NewPriceVersionID *uuid.UUID
	NewQuantity       *int
	EffectiveAt       time.Time
	Status            ChangeStatus
	CreatedBy         *uuid.UUID
	CreatedAt         time.Time
	ResolvedAt        *time.Time
}

// EffectivePrice devolve a versão de preço com o reajuste da assinatura
// aplicado ao componente fixo. O componente de consumo não é reajustado.
func (s Subscription) EffectivePrice(p catalog.PriceVersion) catalog.PriceVersion {
	if s.PriceOverride == nil {
		return p
	}
	switch p.Model {
	case catalog.Flat, catalog.FlatPlusOverage:
		m := *s.PriceOverride
		p.FlatAmount = &m
	case catalog.PerUnit:
		p.UnitAmount = s.PriceOverride.Rat()
	}
	return p
}

// FixedBase é o valor sujeito a reajuste: valor fixo (flat e
// flat_plus_overage) ou preço por unidade (per_unit), arredondado a
// centavos. Preços somente por consumo não têm componente fixo.
func FixedBase(p catalog.PriceVersion) (money.Money, error) {
	switch p.Model {
	case catalog.Flat, catalog.FlatPlusOverage:
		return *p.FlatAmount, nil
	case catalog.PerUnit:
		return money.FromRat(p.UnitAmount, p.Currency, money.DefaultRounding)
	}
	return money.Money{}, apperr.Invalid("price_without_fixed_component",
		"O preço desta assinatura é somente por consumo e não tem componente fixo para reajustar.")
}

// ValidateChange verifica se a assinatura pode passar ao plano, versão de
// preço e quantidade informados. Mudanças imediatas exigem preços sem
// consumo (o período de consumo em andamento é sempre precificado por uma
// única versão); a periodicidade e a moeda não mudam.
func (s Subscription) ValidateChange(current catalog.PriceVersion, plan catalog.Plan, price catalog.PriceVersion, qty int, timing ChangeTiming) error {
	switch s.Status {
	case Canceled:
		return apperr.Conflict("invalid_transition", "A assinatura está cancelada.")
	case Suspended:
		return apperr.Conflict("subscription_suspended", "Reative a assinatura antes de alterar plano ou quantidade.")
	}
	if timing != ChangeImmediately && timing != ChangeNextCycle {
		return apperr.Invalid("invalid_timing", "Momento da alteração inválido.",
			apperr.FieldError{Field: "timing", Message: "Use immediate ou next_cycle."})
	}
	if s.CancelAtPeriodEnd {
		return apperr.Conflict("cancel_scheduled", "Desfaça o cancelamento agendado antes de alterar a assinatura.")
	}
	var f []apperr.FieldError
	if plan.ID != s.PlanID && plan.Status != catalog.Active {
		f = append(f, apperr.FieldError{Field: "plan_id", Message: "O plano está arquivado."})
	}
	if price.PlanID != plan.ID {
		f = append(f, apperr.FieldError{Field: "plan_id", Message: "Versão de preço não pertence ao plano."})
	}
	if plan.Interval != s.Interval {
		f = append(f, apperr.FieldError{Field: "plan_id", Message: "A periodicidade do plano deve ser a mesma da assinatura."})
	}
	if price.Currency != current.Currency {
		f = append(f, apperr.FieldError{Field: "plan_id", Message: "A moeda do plano deve ser a mesma da assinatura."})
	}
	if qty < 1 || qty > MaxQuantity {
		f = append(f, apperr.FieldError{Field: "quantity", Message: "A quantidade deve ficar entre 1 e 1.000.000."})
	} else if price.Model != catalog.PerUnit && qty != 1 {
		f = append(f, apperr.FieldError{Field: "quantity", Message: "Somente planos por quantidade aceitam quantidade diferente de 1."})
	}
	if len(f) > 0 {
		return apperr.Invalid("invalid_change", "Alteração de assinatura inválida.", f...)
	}
	if price.ID == s.PriceVersionID && qty == s.Quantity {
		return apperr.Invalid("no_change", "A assinatura já está neste plano e quantidade.")
	}
	if timing == ChangeImmediately && (current.HasUsage() || price.HasUsage()) {
		return apperr.Invalid("immediate_change_requires_fixed_price",
			"Planos com consumo medido só podem ser alterados a partir do próximo ciclo.",
			apperr.FieldError{Field: "timing", Message: "Use next_cycle."})
	}
	return nil
}

// ApplyChange troca plano, versão de preço e quantidade. Quando a versão de
// preço muda, o reajuste da assinatura deixa de valer (o novo preço é o da
// versão contratada).
func (s *Subscription) ApplyChange(plan catalog.Plan, price catalog.PriceVersion, qty int, event string, at time.Time, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	details["from_plan_id"], details["to_plan_id"] = s.PlanID, plan.ID
	details["from_price_version_id"], details["to_price_version_id"] = s.PriceVersionID, price.ID
	details["from_quantity"], details["to_quantity"] = s.Quantity, qty
	if price.ID != s.PriceVersionID {
		s.PriceOverride, s.PriceOverrideAdjustmentID = nil, nil
	}
	s.PlanID, s.PriceVersionID, s.Quantity = plan.ID, price.ID, qty
	s.record(event, nil, at, details)
}

// NextChangeAt é o instante em que uma mudança agendada para o próximo ciclo
// passa a valer: o fim do período corrente (ou da avaliação).
func (s Subscription) NextChangeAt() time.Time { return s.CurrentPeriod.End }

// CurrentPeriodBilled informa se o componente fixo do período corrente já foi
// faturado (cobrança antecipada). Na avaliação e logo após a reativação, o
// período ainda não foi faturado.
func (s Subscription) CurrentPeriodBilled() (bool, error) {
	if s.Status == Trialing {
		return false, nil
	}
	sched, err := s.Schedule()
	if err != nil {
		return false, err
	}
	idx := sched.IndexOf(s.CurrentPeriod.Start)
	return idx >= 0 && idx < s.NextBillingIndex && sched.Period(idx).Start.Equal(s.CurrentPeriod.Start), nil
}

// Proration é o cálculo da prorrata de uma mudança imediata.
//
// Política: por dias civis no fuso do contrato, contando o dia da mudança
// como dia restante. Credita-se a parte não usufruída do componente fixo
// anterior e cobra-se a mesma fração do novo componente fixo, cada valor
// arredondado uma única vez (half-even). Exemplo: período de 30 dias, troca
// no dia 21 de R$ 100,00 para R$ 160,00 → fração 10/30; crédito R$ 33,33;
// cobrança R$ 53,33; diferença R$ 20,00.
type Proration struct {
	Period        calendar.Period
	Fraction      *big.Rat
	RemainingDays int
	PeriodDays    int
	OldAmount     money.Money
	NewAmount     money.Money
	Credit        money.Money
	Charge        money.Money
	Net           money.Money
}

// ProrateChange calcula a prorrata entre os componentes fixos anterior e
// novo (já com reajustes aplicados). ok é falso quando não há prorrata: o
// período corrente ainda não foi faturado ou a mudança ocorre fora dele.
func ProrateChange(s Subscription, oldPrice, newPrice catalog.PriceVersion, oldQty, newQty int, at time.Time) (Proration, bool, error) {
	billed, err := s.CurrentPeriodBilled()
	if err != nil || !billed || !s.CurrentPeriod.Contains(at) {
		return Proration{}, false, err
	}
	loc, err := s.Location()
	if err != nil {
		return Proration{}, false, err
	}
	frac, err := calendar.RemainingFraction(s.CurrentPeriod, at, loc)
	if err != nil {
		return Proration{}, false, err
	}
	oldAmount, _, _, err := oldPrice.FixedCharge(oldQty)
	if err != nil {
		return Proration{}, false, err
	}
	newAmount, _, _, err := newPrice.FixedCharge(newQty)
	if err != nil {
		return Proration{}, false, err
	}
	credit, err := oldAmount.MulRat(frac, money.DefaultRounding)
	if err != nil {
		return Proration{}, false, err
	}
	charge, err := newAmount.MulRat(frac, money.DefaultRounding)
	if err != nil {
		return Proration{}, false, err
	}
	net, err := charge.Sub(credit)
	if err != nil {
		return Proration{}, false, err
	}
	total := s.CurrentPeriod.Days(loc)
	remaining := int(new(big.Rat).Mul(frac, big.NewRat(int64(total), 1)).Num().Int64())
	start := calendar.DateOf(at, loc).StartIn(loc)
	if start.Before(s.CurrentPeriod.Start) {
		start = s.CurrentPeriod.Start
	}
	return Proration{Period: calendar.Period{Start: start, End: s.CurrentPeriod.End}, Fraction: frac, RemainingDays: remaining,
		PeriodDays: total, OldAmount: oldAmount, NewAmount: newAmount, Credit: credit, Charge: charge, Net: net}, true, nil
}

// AdjustmentKind é o tipo de reajuste.
type AdjustmentKind string

const (
	// AdjustPercent aplica um percentual (pontos-base) ao valor vigente.
	AdjustPercent AdjustmentKind = "percent"
	// AdjustFixedAmount define um novo valor em centavos.
	AdjustFixedAmount AdjustmentKind = "fixed_amount"
)

// AdjustmentStatus é a situação de um reajuste.
type AdjustmentStatus string

const (
	AdjustmentScheduled AdjustmentStatus = "scheduled"
	AdjustmentApplied   AdjustmentStatus = "applied"
	AdjustmentCanceled  AdjustmentStatus = "canceled"
)

// MaxAdjustmentHorizon limita a antecedência de um reajuste programado.
const MaxAdjustmentHorizon = 3 * 366

// PriceAdjustment é um reajuste programado do componente fixo. Vale a partir
// do primeiro período iniciado na data efetiva ou depois; períodos já
// faturados nunca são alterados.
type PriceAdjustment struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	SubscriptionID     uuid.UUID
	EffectiveDate      calendar.Date
	Kind               AdjustmentKind
	PercentBP          *int
	NewAmount          *money.Money
	IndexName          string
	Reason             string
	Status             AdjustmentStatus
	PreviousAmount     *money.Money
	ResultingAmount    *money.Money
	AppliedPeriodStart *time.Time
	AppliedAt          *time.Time
	CanceledAt         *time.Time
	CreatedBy          *uuid.UUID
	CreatedAt          time.Time
}

// AdjustmentInput são os dados de um reajuste.
type AdjustmentInput struct {
	EffectiveDate calendar.Date
	Kind          AdjustmentKind
	PercentBP     *int
	NewAmount     *int64
	IndexName     string
	Reason        string
}

// NewPriceAdjustment valida e cria um reajuste programado.
func NewPriceAdjustment(id uuid.UUID, s Subscription, price catalog.PriceVersion, in AdjustmentInput, createdBy *uuid.UUID, now time.Time) (PriceAdjustment, error) {
	if s.Status == Canceled || s.CancelAtPeriodEnd {
		return PriceAdjustment{}, apperr.Conflict("subscription_ending", "Assinaturas canceladas ou com cancelamento agendado não recebem reajustes.")
	}
	if _, err := FixedBase(price); err != nil {
		return PriceAdjustment{}, err
	}
	loc, err := s.Location()
	if err != nil {
		return PriceAdjustment{}, err
	}
	var f []apperr.FieldError
	add := func(field, msg string) { f = append(f, apperr.FieldError{Field: field, Message: msg}) }
	today := calendar.DateOf(now, loc)
	if in.EffectiveDate == (calendar.Date{}) {
		add("effective_date", "Data efetiva inválida.")
	} else {
		if in.EffectiveDate.Before(today) {
			add("effective_date", "A data efetiva não pode estar no passado.")
		}
		if calendar.DaysBetween(today, in.EffectiveDate) > MaxAdjustmentHorizon {
			add("effective_date", "A data efetiva deve estar nos próximos três anos.")
		}
	}
	a := PriceAdjustment{ID: id, TenantID: s.TenantID, SubscriptionID: s.ID, EffectiveDate: in.EffectiveDate, Kind: in.Kind,
		IndexName: in.IndexName, Reason: in.Reason, Status: AdjustmentScheduled, CreatedBy: createdBy, CreatedAt: now}
	switch in.Kind {
	case AdjustPercent:
		switch {
		case in.PercentBP == nil || *in.PercentBP == 0:
			add("percent_bp", "Informe o percentual em pontos-base (diferente de zero).")
		case *in.PercentBP < -10000 || *in.PercentBP > 100000:
			add("percent_bp", "O percentual deve ficar entre -100% e 1.000%.")
		default:
			bp := *in.PercentBP
			a.PercentBP = &bp
		}
		if in.NewAmount != nil {
			add("new_amount", "Reajuste percentual não usa novo valor.")
		}
	case AdjustFixedAmount:
		if in.NewAmount == nil || *in.NewAmount < 0 || *in.NewAmount > money.MaxAmount {
			add("new_amount", "Informe o novo valor em centavos (zero ou positivo).")
		} else {
			m := money.New(*in.NewAmount, price.Currency)
			a.NewAmount = &m
		}
		if in.PercentBP != nil {
			add("percent_bp", "Reajuste por valor não usa percentual.")
		}
	default:
		add("kind", "Use percent ou fixed_amount.")
	}
	if l := len([]rune(in.Reason)); l < 3 || l > 500 {
		add("reason", "Informe o motivo (3 a 500 caracteres).")
	}
	if len([]rune(in.IndexName)) > 50 {
		add("index_name", "O índice deve ter no máximo 50 caracteres.")
	}
	if len(f) > 0 {
		return PriceAdjustment{}, apperr.Invalid("invalid_adjustment", "Reajuste inválido.", f...)
	}
	return a, nil
}

// Apply aplica o reajuste ao componente fixo vigente da assinatura no início
// do período informado, registrando os valores anterior e resultante.
func (a *PriceAdjustment) Apply(s *Subscription, price catalog.PriceVersion, periodStart, at time.Time) error {
	if a.Status != AdjustmentScheduled {
		return apperr.Conflict("adjustment_not_scheduled", "O reajuste não está programado.")
	}
	prev, err := FixedBase(s.EffectivePrice(price))
	if err != nil {
		return err
	}
	var res money.Money
	switch a.Kind {
	case AdjustPercent:
		factor := new(big.Rat).Add(big.NewRat(1, 1), money.Percent(int64(*a.PercentBP)))
		if res, err = prev.MulRat(factor, money.DefaultRounding); err != nil {
			return err
		}
	case AdjustFixedAmount:
		res = *a.NewAmount
	default:
		return apperr.Invalid("invalid_adjustment", "Tipo de reajuste desconhecido.")
	}
	if res.IsNegative() {
		res = money.Zero(prev.Currency())
	}
	if err := res.CheckBusinessRange(); err != nil {
		return apperr.Invalid("adjustment_amount_limit", "O valor reajustado excede o limite permitido.")
	}
	s.PriceOverride, s.PriceOverrideAdjustmentID = &res, &a.ID
	a.Status, a.PreviousAmount, a.ResultingAmount, a.AppliedPeriodStart, a.AppliedAt = AdjustmentApplied, &prev, &res, &periodStart, &at
	details := map[string]any{"adjustment_id": a.ID, "kind": a.Kind, "previous_amount": prev.Amount(), "resulting_amount": res.Amount(),
		"period_start": periodStart}
	if a.PercentBP != nil {
		details["percent_bp"] = *a.PercentBP
	}
	if a.IndexName != "" {
		details["index_name"] = a.IndexName
	}
	s.record("price_adjusted", nil, at, details)
	return nil
}

// Cancel cancela um reajuste ainda não aplicado.
func (a *PriceAdjustment) Cancel(at time.Time) error {
	if a.Status != AdjustmentScheduled {
		return apperr.Conflict("adjustment_not_scheduled", "Somente reajustes programados podem ser cancelados.")
	}
	a.Status, a.CanceledAt = AdjustmentCanceled, &at
	return nil
}
