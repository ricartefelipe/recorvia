// Package platformbilling modela o relacionamento comercial entre o tenant e
// o Recorvia: plano contratado da plataforma, limites (usuários, contratos
// ativos), recursos habilitados, serviço adicional de implantação assistida e
// demonstrativos mensais. É independente do faturamento que o tenant faz aos
// próprios clientes (módulos subscriptions e billing).
package platformbilling

import (
	"fmt"
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
	// Anchor é a data civil que ancora os ciclos mensais (dias 29 a 31
	// são preservados nos meses seguintes).
	Anchor      calendar.Date
	PeriodStart time.Time
	PeriodEnd   time.Time
	Version     int
}

// Usage é o consumo atual dos limites. Convites pendentes (não aceitos, não
// revogados e não expirados) reservam vagas de usuário: quem os aceita entra
// sem nova checagem de convite.
type Usage struct {
	ActiveUsers         int
	PendingInvitations  int
	ActiveSubscriptions int
}

// ReservedUsers são as vagas de usuário ocupadas: membros ativos e convites
// pendentes.
func (u Usage) ReservedUsers() int { return u.ActiveUsers + u.PendingInvitations }

// NewSubscription inicia a assinatura da plataforma. Sem avaliação, os ciclos
// mensais são ancorados na data local de início. Com avaliação, o primeiro
// período é a própria avaliação (sem cobrança do plano) e os ciclos seguintes
// são ancorados na data local de fim da avaliação.
func NewSubscription(tenantID uuid.UUID, plan Plan, now time.Time, loc *time.Location) (Subscription, error) {
	if loc == nil {
		return Subscription{}, calendar.ErrNilLocation
	}
	today := calendar.DateOf(now, loc)
	s := Subscription{TenantID: tenantID, PlanCode: plan.Code, Status: StatusActive, Anchor: today, Version: 1}
	if plan.TrialDays > 0 {
		end := today.AddDays(plan.TrialDays).StartIn(loc)
		s.Status = StatusTrialing
		s.TrialEndsAt = &end
		s.Anchor = today.AddDays(plan.TrialDays)
		s.PeriodStart, s.PeriodEnd = today.StartIn(loc), end
		return s, nil
	}
	sched, err := calendar.NewScheduleFromAnchor(calendar.Monthly, s.Anchor, loc)
	if err != nil {
		return Subscription{}, err
	}
	p := sched.Period(0)
	s.PeriodStart, s.PeriodEnd = p.Start, p.End
	return s, nil
}

// CheckUserLimit falha quando mais um convite excederia o plano: membros
// ativos e convites pendentes ocupam vagas.
func CheckUserLimit(p Plan, u Usage) error {
	if p.MaxUsers != nil && u.ReservedUsers()+1 > *p.MaxUsers {
		return apperr.LimitExceeded("platform_user_limit",
			"O plano da plataforma atingiu o limite de usuários. Altere o plano para convidar mais membros.")
	}
	return nil
}

// CheckJoinLimit falha quando a entrada de mais um membro ativo excederia o
// plano. Vale no aceite do convite: a vaga reservada pelo próprio convite
// deixa de contar como pendente, e só os membros ativos são comparados (o
// plano pode ter sido rebaixado depois do convite).
func CheckJoinLimit(p Plan, u Usage) error {
	if p.MaxUsers != nil && u.ActiveUsers+1 > *p.MaxUsers {
		return apperr.LimitExceeded("platform_user_limit",
			"O plano da plataforma atingiu o limite de usuários. Peça a um proprietário para alterar o plano antes de aceitar o convite.")
	}
	return nil
}

// CheckSubscriptionLimit falha quando mais um contrato ativo (criação ou
// reativação de assinatura suspensa) excederia o plano.
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
// Convites pendentes contam como usuários: aceitos depois da troca, levariam
// a organização além do limite.
func CanChangeTo(target Plan, u Usage) error {
	if !target.Active {
		return apperr.Invalid("platform_plan_inactive", "O plano escolhido não está disponível.")
	}
	if target.MaxUsers != nil && u.ReservedUsers() > *target.MaxUsers {
		return apperr.Conflict("platform_downgrade_users",
			"A organização tem mais usuários ativos e convites pendentes do que o plano escolhido permite. Revogue convites ou remova membros antes de trocar.")
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

// Advance move a assinatura para o período mensal que começa no fim do
// período corrente, calculado a partir da âncora (sem sobreposição e sem
// perder dias 29 a 31), encerrando a avaliação quando vencida.
func (s *Subscription) Advance(loc *time.Location) error {
	sched, err := calendar.NewScheduleFromAnchor(calendar.Monthly, s.Anchor, loc)
	if err != nil {
		return err
	}
	idx := sched.IndexOf(s.PeriodEnd)
	if idx < 0 {
		return fmt.Errorf("platformbilling: fim do período anterior à âncora")
	}
	next := sched.Period(idx)
	if !next.Start.Equal(s.PeriodEnd) {
		return fmt.Errorf("platformbilling: período corrente fora do ciclo ancorado")
	}
	s.PeriodStart, s.PeriodEnd = next.Start, next.End
	if s.Status == StatusTrialing && s.TrialEndsAt != nil && !s.TrialEndsAt.After(s.PeriodStart) {
		s.Status = StatusActive
	}
	return nil
}
