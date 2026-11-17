// Package platformbilling modela o relacionamento comercial entre o tenant e
// o Recorvia: plano contratado da plataforma, limites (usuários, contratos
// ativos), recursos habilitados, serviço adicional de implantação assistida e
// demonstrativos mensais. É independente do faturamento que o tenant faz aos
// próprios clientes (módulos subscriptions e billing).
package platformbilling

import (
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// Feature é um recurso habilitado por plano da plataforma.
type Feature string

// Recursos controlados por plano.
const (
	FeatureRecurringBilling  Feature = "recurring_billing"
	FeatureEmailDunning      Feature = "email_dunning"
	FeatureUsageBilling      Feature = "usage_billing"
	FeatureOutboundWebhooks  Feature = "outbound_webhooks"
	FeatureAPIAccess         Feature = "api_access"
	FeatureAsaasIntegration  Feature = "asaas_integration"
	FeatureReconciliationRun Feature = "reconciliation_automation"
)

// Plan é um plano comercial da plataforma.
type Plan struct {
	Code                   string
	Name                   string
	Description            string
	MonthlyPrice           money.Money
	MaxUsers               *int
	MaxActiveSubscriptions *int
	Features               []Feature
	TrialDays              int
	IsDefault              bool
	Active                 bool
}

// HasFeature informa se o plano habilita o recurso.
func (p Plan) HasFeature(f Feature) bool { return slices.Contains(p.Features, f) }

// Status da assinatura do tenant na plataforma.
type Status string

const (
	StatusTrialing Status = "trialing"
	StatusActive   Status = "active"
	StatusPastDue  Status = "past_due"
	StatusCanceled Status = "canceled"
)

// Subscription é a assinatura do tenant ao Recorvia.
type Subscription struct {
	TenantID    uuid.UUID
	PlanCode    string
	Status      Status
	TrialEndsAt *time.Time
	PeriodStart time.Time
	PeriodEnd   time.Time
	Version     int
}

// Usage é o consumo atual dos limites.
type Usage struct {
	ActiveUsers         int
	ActiveSubscriptions int
}

// NewSubscription inicia a assinatura da plataforma com o período de
// avaliação do plano, em ciclos mensais ancorados na data local de início.
func NewSubscription(tenantID uuid.UUID, plan Plan, now time.Time, loc *time.Location) (Subscription, error) {
	sched, err := calendar.NewSchedule(calendar.Monthly, now, loc)
	if err != nil {
		return Subscription{}, err
	}
	p := sched.Period(0)
	s := Subscription{TenantID: tenantID, PlanCode: plan.Code, Status: StatusActive, PeriodStart: p.Start, PeriodEnd: p.End, Version: 1}
	if plan.TrialDays > 0 {
		end := calendar.DateOf(now, loc).AddDays(plan.TrialDays).StartIn(loc)
		s.Status = StatusTrialing
		s.TrialEndsAt = &end
	}
	return s, nil
}

// CheckUserLimit falha quando adicionar mais um usuário excederia o plano.
func CheckUserLimit(p Plan, u Usage) error {
	if p.MaxUsers != nil && u.ActiveUsers+1 > *p.MaxUsers {
		return apperr.LimitExceeded("platform_user_limit",
			"O plano da plataforma atingiu o limite de usuários. Altere o plano para convidar mais membros.")
	}
	return nil
}

// CheckSubscriptionLimit falha quando criar mais um contrato ativo excederia o plano.
func CheckSubscriptionLimit(p Plan, u Usage) error {
	if p.MaxActiveSubscriptions != nil && u.ActiveSubscriptions+1 > *p.MaxActiveSubscriptions {
		return apperr.LimitExceeded("platform_subscription_limit",
			"O plano da plataforma atingiu o limite de contratos ativos. Altere o plano para criar novas assinaturas.")
	}
	return nil
}

// RequireFeature falha quando o plano não habilita o recurso.
func RequireFeature(p Plan, f Feature) error {
	if !p.HasFeature(f) {
		return apperr.Forbidden("platform_feature_unavailable",
			"Este recurso não está incluído no plano atual da plataforma.")
	}
	return nil
}

// CanChangeTo verifica se o consumo atual cabe nos limites do novo plano.
func CanChangeTo(target Plan, u Usage) error {
	if !target.Active {
		return apperr.Invalid("platform_plan_inactive", "O plano escolhido não está disponível.")
	}
	if target.MaxUsers != nil && u.ActiveUsers > *target.MaxUsers {
		return apperr.Conflict("platform_downgrade_users", "A organização tem mais usuários ativos do que o plano escolhido permite.")
	}
	if target.MaxActiveSubscriptions != nil && u.ActiveSubscriptions > *target.MaxActiveSubscriptions {
		return apperr.Conflict("platform_downgrade_subscriptions", "A organização tem mais contratos ativos do que o plano escolhido permite.")
	}
	return nil
}

// Statement é o demonstrativo mensal da plataforma para o tenant.
type Statement struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	PeriodStart        time.Time
	PeriodEnd          time.Time
	PlanCode           string
	SubscriptionAmount money.Money
	AddonsAmount       money.Money
	Total              money.Money
	Status             string
	CreatedAt          time.Time
}

// CloseStatement calcula o demonstrativo do período encerrado. Períodos
// inteiramente dentro da avaliação não geram cobrança do plano.
func CloseStatement(id uuid.UUID, s Subscription, plan Plan, addons money.Money, now time.Time) (Statement, error) {
	sub := plan.MonthlyPrice
	if s.TrialEndsAt != nil && !s.TrialEndsAt.Before(s.PeriodEnd) {
		sub = money.Zero(plan.MonthlyPrice.Currency())
	}
	total, err := sub.Add(addons)
	if err != nil {
		return Statement{}, err
	}
	return Statement{
		ID: id, TenantID: s.TenantID, PeriodStart: s.PeriodStart, PeriodEnd: s.PeriodEnd, PlanCode: plan.Code,
		SubscriptionAmount: sub, AddonsAmount: addons, Total: total, Status: "open", CreatedAt: now,
	}, nil
}

// Advance move a assinatura para o próximo período mensal, encerrando a
// avaliação quando vencida.
func (s *Subscription) Advance(loc *time.Location) error {
	sched, err := calendar.NewSchedule(calendar.Monthly, s.PeriodStart, loc)
	if err != nil {
		return err
	}
	next := sched.Period(1)
	s.PeriodStart, s.PeriodEnd = next.Start, next.End
	if s.Status == StatusTrialing && s.TrialEndsAt != nil && !s.TrialEndsAt.After(s.PeriodStart) {
		s.Status = StatusActive
	}
	return nil
}
