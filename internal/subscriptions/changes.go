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
		// O valor fixo reajustado é inteiro em centavos e limitado a
		// money.MaxAmount pelo próprio reajuste; a conversão não arredonda.
		if m, err := money.FromRat(s.PriceOverride, p.Currency, money.DefaultRounding); err == nil {
			p.FlatAmount = &m
		}
	case catalog.PerUnit:
		p.UnitAmount = new(big.Rat).Set(s.PriceOverride)
	}
	return p
}

// FixedBase é o valor sujeito a reajuste, exato e em centavos: valor fixo
// (flat e flat_plus_overage) ou preço por unidade (per_unit, que pode ter
// fração de centavo). Preços somente por consumo não têm componente fixo.
func FixedBase(p catalog.PriceVersion) (*big.Rat, error) {
	switch p.Model {
	case catalog.Flat, catalog.FlatPlusOverage:
		return p.FlatAmount.Rat(), nil
	case catalog.PerUnit:
		return new(big.Rat).Set(p.UnitAmount), nil
	}
	return nil, apperr.Invalid("price_without_fixed_component",
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
// versão contratada); os reajustes ainda programados devem ser cancelados
// pelo chamador na mesma transação (CancelScheduledAdjustments).
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
// como dia restante. Credita-se a fração não usufruída do valor efetivamente
// faturado pelo componente fixo vigente no período (ProrationBasis.Billed) e
// cobra-se a mesma fração do novo componente fixo com o desconto percentual
// que vale para ele, cada valor arredondado uma única vez (half-even).
// Exemplo: período de 30 dias faturado por R$ 100,00, troca no dia 21 para
// R$ 160,00 → fração 10/30; crédito R$ 33,33; cobrança R$ 53,33; diferença
// R$ 20,00. Se o período foi faturado com R$ 10,00 de desconto (R$ 90,00) e
// um cupom de 10% vale para o novo plano: crédito R$ 30,00, cobrança
// R$ 48,00, diferença R$ 18,00. Sem faturamento válido do período, não há
// crédito.
type Proration struct {
	Period        calendar.Period
	Fraction      *big.Rat
	RemainingDays int
	PeriodDays    int
	// OldAmount e NewAmount são os componentes fixos anterior e novo pelo
	// preço vigente, sem desconto (informativos).
	OldAmount money.Money
	NewAmount money.Money
	// CreditBase é o valor faturado pelo componente anterior para o período
	// integral (zero sem faturamento válido); NewDiscountBP é o percentual
	// de desconto da cobrança.
	CreditBase    *big.Rat
	NewDiscountBP int64
	Credit        money.Money
	Charge        money.Money
	Net           money.Money
}

// ProrationBasis são as bases da prorrata.
type ProrationBasis struct {
	// Billed é o valor efetivamente faturado pelo componente fixo vigente
	// para o período integral, líquido de descontos e já com reajustes. Nulo
	// quando não há faturamento válido do componente no período: não há
	// crédito.
	Billed *big.Rat
	// NewBP é o percentual de desconto, em pontos-base, que vale para o
	// novo componente (zero sem desconto percentual).
	NewBP int64
}

// discountFactor é 1 − bp/10000, limitado ao intervalo [0, 1].
func discountFactor(bp int64) *big.Rat {
	bp = min(max(bp, 0), 10000)
	return big.NewRat(10000-bp, 10000)
}

// ProratesAt informa se uma mudança imediata no instante informado tem
// prorrata: o período corrente já foi faturado e contém o instante.
func (s Subscription) ProratesAt(at time.Time) (bool, error) {
	billed, err := s.CurrentPeriodBilled()
	if err != nil || !billed {
		return false, err
	}
	return s.CurrentPeriod.Contains(at), nil
}

// ProrateChange calcula a prorrata entre os componentes fixos anterior e
// novo (já com reajustes aplicados). O crédito usa o valor faturado
// informado em basis e a cobrança, o novo componente com o percentual de
// desconto de basis. ok é falso quando não há prorrata: o período corrente
// ainda não foi faturado ou a mudança ocorre fora dele.
func ProrateChange(s Subscription, oldPrice, newPrice catalog.PriceVersion, oldQty, newQty int, at time.Time,
	basis ProrationBasis) (Proration, bool, error) {
	prorates, err := s.ProratesAt(at)
	if err != nil || !prorates {
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
	base := new(big.Rat)
	if basis.Billed != nil && basis.Billed.Sign() > 0 {
		base.Set(basis.Billed)
	}
	credit, err := money.FromRat(new(big.Rat).Mul(base, frac), oldAmount.Currency(), money.DefaultRounding)
	if err != nil {
		return Proration{}, false, err
	}
	bp := min(max(basis.NewBP, 0), 10000)
	charge, err := newAmount.MulRat(new(big.Rat).Mul(frac, discountFactor(bp)), money.DefaultRounding)
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
		PeriodDays: total, OldAmount: oldAmount, NewAmount: newAmount, CreditBase: base, NewDiscountBP: bp, Credit: credit, Charge: charge,
		Net: net}, true, nil
}

// CreditUnitAmount é o valor unitário exato do item de crédito (negativo):
// valor faturado pelo componente anterior para o período integral.
// Multiplicado pela fração, resulta no crédito antes do arredondamento.
func (p Proration) CreditUnitAmount() *big.Rat {
	if p.CreditBase == nil {
		return new(big.Rat)
	}
	return new(big.Rat).Neg(p.CreditBase)
}

// ChargeUnitAmount é o valor unitário exato do item de cobrança: novo
// componente fixo com o desconto percentual que vale para ele. É também o
// valor faturado pelo novo componente para o período integral, base do
// crédito de uma nova mudança no mesmo período.
func (p Proration) ChargeUnitAmount() *big.Rat {
	return new(big.Rat).Mul(p.NewAmount.Rat(), discountFactor(p.NewDiscountBP))
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

// AdjustmentCancelReason informa por que um reajuste programado foi
// cancelado.
type AdjustmentCancelReason string

const (
	// AdjustmentCanceledManually: cancelado por um usuário.
	AdjustmentCanceledManually AdjustmentCancelReason = "manual"
	// AdjustmentCanceledSubscriptionEnded: assinatura cancelada ou encerrada.
	AdjustmentCanceledSubscriptionEnded AdjustmentCancelReason = "subscription_ended"
	// AdjustmentCanceledPriceChanged: a versão de preço da assinatura mudou;
	// o reajuste foi programado sobre o preço anterior.
	AdjustmentCanceledPriceChanged AdjustmentCancelReason = "price_version_changed"
	// AdjustmentCanceledNoFixedComponent: o preço vigente no início do ciclo
	// é somente por consumo.
	AdjustmentCanceledNoFixedComponent AdjustmentCancelReason = "price_without_fixed_component"
	// AdjustmentCanceledAmountLimit: o valor resultante excede o limite.
	AdjustmentCanceledAmountLimit AdjustmentCancelReason = "adjustment_amount_limit"
)

// MaxAdjustmentHorizon limita a antecedência de um reajuste programado.
const MaxAdjustmentHorizon = 3 * 366

// PriceAdjustment é um reajuste programado do componente fixo. Vale a partir
// do primeiro período iniciado na data efetiva ou depois; períodos já
// faturados nunca são alterados.
//
// PreviousAmount e ResultingAmount são exatos, em centavos: no valor fixo o
// resultado é arredondado a centavos uma única vez; no preço por unidade o
// percentual incide sobre o preço unitário exato e o resultado é arredondado
// (half-even) a money.MaxDecimalScale casas, deixando o arredondamento a
// centavos para o total do item (preço unitário × quantidade).
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
	PreviousAmount     *big.Rat
	ResultingAmount    *big.Rat
	AppliedPeriodStart *time.Time
	AppliedAt          *time.Time
	CanceledAt         *time.Time
	CancelReason       AdjustmentCancelReason
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
	if len(f) == 0 {
		// O limite do valor resultante é verificado já no agendamento, sobre o
		// componente fixo vigente.
		if _, _, err := a.resulting(s, price); err != nil {
			e, ok := apperr.As(err)
			if !ok || e.Code != "adjustment_amount_limit" {
				return PriceAdjustment{}, err
			}
			field := "percent_bp"
			if a.Kind == AdjustFixedAmount {
				field = "new_amount"
			}
			add(field, "O valor reajustado excede o limite permitido.")
		}
	}
	if len(f) > 0 {
		return PriceAdjustment{}, apperr.Invalid("invalid_adjustment", "Reajuste inválido.", f...)
	}
	return a, nil
}

// resulting calcula o valor anterior (componente fixo vigente da
// assinatura) e o valor resultante do reajuste, com o arredondamento único
// da política (centavos no valor fixo; escala do preço unitário no preço por
// unidade). O resultado negativo é limitado a zero; acima do limite de
// negócio (no valor ou no total do componente fixo com a quantidade atual),
// devolve adjustment_amount_limit.
func (a PriceAdjustment) resulting(s Subscription, price catalog.PriceVersion) (prev, res *big.Rat, err error) {
	eff := s.EffectivePrice(price)
	if prev, err = FixedBase(eff); err != nil {
		return nil, nil, err
	}
	switch a.Kind {
	case AdjustPercent:
		factor := new(big.Rat).Add(big.NewRat(1, 1), money.Percent(int64(*a.PercentBP)))
		scale := 0
		if eff.Model == catalog.PerUnit {
			scale = money.MaxDecimalScale
		}
		res = money.RoundToScale(new(big.Rat).Mul(prev, factor), scale, money.DefaultRounding)
	case AdjustFixedAmount:
		res = a.NewAmount.Rat()
	default:
		return nil, nil, apperr.Invalid("invalid_adjustment", "Tipo de reajuste desconhecido.")
	}
	if res.Sign() < 0 {
		res = new(big.Rat)
	}
	limit := apperr.Invalid("adjustment_amount_limit", "O valor reajustado excede o limite permitido.")
	if res.Cmp(new(big.Rat).SetInt64(money.MaxAmount)) > 0 {
		return nil, nil, limit
	}
	adjusted := Subscription{PriceOverride: res}.EffectivePrice(eff)
	if total, _, _, err := adjusted.FixedCharge(s.Quantity); err != nil || total.CheckBusinessRange() != nil {
		return nil, nil, limit
	}
	return prev, res, nil
}

// cents arredonda um valor exato a centavos (exibição e auditoria).
func cents(r *big.Rat) int64 { return money.RoundRat(r, money.DefaultRounding).Int64() }

// Apply aplica o reajuste ao componente fixo vigente da assinatura no início
// do período informado, registrando os valores anterior e resultante.
func (a *PriceAdjustment) Apply(s *Subscription, price catalog.PriceVersion, periodStart, at time.Time) error {
	if a.Status != AdjustmentScheduled {
		return apperr.Conflict("adjustment_not_scheduled", "O reajuste não está programado.")
	}
	prev, res, err := a.resulting(*s, price)
	if err != nil {
		return err
	}
	id := a.ID
	s.PriceOverride, s.PriceOverrideAdjustmentID = new(big.Rat).Set(res), &id
	a.Status, a.PreviousAmount, a.ResultingAmount, a.AppliedPeriodStart, a.AppliedAt = AdjustmentApplied, prev, res, &periodStart, &at
	details := map[string]any{"adjustment_id": a.ID, "kind": a.Kind, "previous_amount": cents(prev), "resulting_amount": cents(res),
		"previous_amount_decimal": money.RatString(prev), "resulting_amount_decimal": money.RatString(res), "period_start": periodStart}
	if a.PercentBP != nil {
		details["percent_bp"] = *a.PercentBP
	}
	if a.IndexName != "" {
		details["index_name"] = a.IndexName
	}
	s.record("price_adjusted", nil, at, details)
	return nil
}

// ApplyOrCancel aplica o reajuste no início do período ou, quando ele não
// pode valer para o preço vigente (preço somente por consumo ou valor
// resultante acima do limite), cancela-o com o motivo e registra o
// cancelamento no histórico da assinatura, sem interromper o faturamento.
// applied informa o desfecho; os demais erros são devolvidos.
func (a *PriceAdjustment) ApplyOrCancel(s *Subscription, price catalog.PriceVersion, periodStart, at time.Time) (applied bool, err error) {
	err = a.Apply(s, price, periodStart, at)
	if err == nil {
		return true, nil
	}
	reason, ok := unapplicableReason(err)
	if !ok {
		return false, err
	}
	if err := a.Cancel(reason, at); err != nil {
		return false, err
	}
	s.record("price_adjustment_canceled", nil, at, map[string]any{"adjustment_id": a.ID, "reason": reason, "period_start": periodStart})
	return false, nil
}

func unapplicableReason(err error) (AdjustmentCancelReason, bool) {
	e, ok := apperr.As(err)
	if !ok {
		return "", false
	}
	switch e.Code {
	case "price_without_fixed_component":
		return AdjustmentCanceledNoFixedComponent, true
	case "adjustment_amount_limit":
		return AdjustmentCanceledAmountLimit, true
	}
	return "", false
}

// Cancel cancela um reajuste ainda não aplicado, registrando o motivo.
func (a *PriceAdjustment) Cancel(reason AdjustmentCancelReason, at time.Time) error {
	if a.Status != AdjustmentScheduled {
		return apperr.Conflict("adjustment_not_scheduled", "Somente reajustes programados podem ser cancelados.")
	}
	a.Status, a.CanceledAt, a.CancelReason = AdjustmentCanceled, &at, reason
	return nil
}

// RecordAdjustmentsCanceled registra no histórico o cancelamento de reajustes
// programados (por exemplo, após a troca da versão de preço).
func (s *Subscription) RecordAdjustmentsCanceled(ids []uuid.UUID, reason AdjustmentCancelReason, at time.Time) {
	for _, id := range ids {
		s.record("price_adjustment_canceled", nil, at, map[string]any{"adjustment_id": id, "reason": reason})
	}
}
