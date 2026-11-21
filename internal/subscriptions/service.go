package subscriptions

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

// ListFilter define filtros permitidos.
type ListFilter struct {
	Status Status
	// InForce restringe às assinaturas em trial, ativas ou em atraso, as
	// mesmas das assinaturas ativas do painel.
	InForce    bool
	CustomerID *uuid.UUID
	Limit      int
	Cursor     string
}

// Page é uma página de assinaturas.
type Page struct {
	Items      []Subscription
	NextCursor string
}

// HistoryEntry é uma entrada persistida do histórico.
type HistoryEntry struct {
	ID         uuid.UUID
	Event      string
	FromStatus *Status
	ToStatus   Status
	OccurredAt time.Time
	ActorType  string
	ActorID    string
	Details    map[string]any
}

// Tx são as operações dentro da transação do tenant.
type Tx interface {
	TenantLocation(ctx context.Context) (*time.Location, error)
	Customer(ctx context.Context, id uuid.UUID) (customers.Customer, error)
	Plan(ctx context.Context, id uuid.UUID) (catalog.Plan, error)
	PriceVersion(ctx context.Context, id uuid.UUID) (catalog.PriceVersion, error)
	PlatformPlan(ctx context.Context) (platformbilling.Plan, error)
	PlatformUsage(ctx context.Context) (platformbilling.Usage, error)

	Insert(ctx context.Context, s Subscription) error
	Get(ctx context.Context, id uuid.UUID, forUpdate bool) (Subscription, error)
	Update(ctx context.Context, s Subscription) error
	List(ctx context.Context, f ListFilter) (Page, error)
	InsertHistory(ctx context.Context, subscriptionID uuid.UUID, actor audit.Actor, ev []HistoryEvent) error
	History(ctx context.Context, subscriptionID uuid.UUID) ([]HistoryEntry, error)
	// CancelPendingTerms cancela mudanças agendadas e reajustes programados
	// (assinatura encerrada ou com encerramento agendado).
	CancelPendingTerms(ctx context.Context, subscriptionID uuid.UUID, at time.Time) error
	// HasOpenPendingItems informa se há itens pendentes ainda não faturados.
	HasOpenPendingItems(ctx context.Context, subscriptionID uuid.UUID) (bool, error)

	Audit(ctx context.Context, e audit.Entry) error
	Emit(ctx context.Context, e outbox.Event) error
}

// Store abre transações do tenant.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
}

// Service implementa os casos de uso de assinaturas.
type Service struct {
	store Store
	clock clock.Clock
}

// NewService cria o serviço.
func NewService(store Store, c clock.Clock) *Service { return &Service{store: store, clock: c} }

// CreateInput são os dados de criação.
type CreateInput struct {
	CustomerID       uuid.UUID
	PlanID           uuid.UUID
	Quantity         int
	StartDate        *calendar.Date
	TrialDays        *int
	PaymentTermsDays *int
}

// Create cria uma assinatura com a versão de preço vigente do plano.
func (s *Service) Create(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, in CreateInput) (Subscription, error) {
	now := s.clock.Now()
	var sub Subscription
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		loc, err := tx.TenantLocation(ctx)
		if err != nil {
			return err
		}
		cust, err := tx.Customer(ctx, in.CustomerID)
		if err != nil {
			return err
		}
		if cust.Status != customers.Active {
			return apperr.Conflict("customer_archived", "O cliente está arquivado.")
		}
		plan, err := tx.Plan(ctx, in.PlanID)
		if err != nil {
			return err
		}
		if plan.CurrentPriceVersionID == nil {
			return apperr.Conflict("plan_without_price", "O plano não possui preço vigente.")
		}
		price, err := tx.PriceVersion(ctx, *plan.CurrentPriceVersionID)
		if err != nil {
			return err
		}
		pplan, err := tx.PlatformPlan(ctx)
		if err != nil {
			return err
		}
		if price.HasUsage() {
			if err := platformbilling.RequireFeature(pplan, platformbilling.FeatureUsageBilling); err != nil {
				return err
			}
		}
		usage, err := tx.PlatformUsage(ctx)
		if err != nil {
			return err
		}
		if err := platformbilling.CheckSubscriptionLimit(pplan, usage); err != nil {
			return err
		}
		start := now
		if in.StartDate != nil {
			start = in.StartDate.StartIn(loc)
			if start.After(now) {
				return apperr.Invalid("invalid_subscription", "Dados da assinatura inválidos.",
					apperr.FieldError{Field: "start_date", Message: "A data de início não pode estar no futuro."})
			}
		}
		trial := plan.TrialDays
		if in.TrialDays != nil {
			trial = *in.TrialDays
		}
		qty := in.Quantity
		if qty == 0 {
			qty = 1
		}
		sub, err = New(NewInput{ID: ids.New(), TenantID: tenantID, CustomerID: cust.ID, Plan: plan, Price: price,
			Quantity: qty, StartAt: start, TrialDays: trial, Location: loc, Now: now, PaymentTermsDays: in.PaymentTermsDays})
		if err != nil {
			return err
		}
		if err := tx.Insert(ctx, sub); err != nil {
			return err
		}
		if err := tx.InsertHistory(ctx, sub.ID, actor, sub.PopEvents()); err != nil {
			return err
		}
		if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventSubscriptionCreated, "subscription", sub.ID,
			map[string]any{"subscription_id": sub.ID, "customer_id": sub.CustomerID, "status": sub.Status}, now)); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "subscription.created", "subscription", sub.ID.String(),
			map[string]any{"plan_id": plan.ID, "price_version": price.Version, "quantity": qty}))
	})
	return sub, err
}

// Get retorna uma assinatura.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (Subscription, error) {
	var sub Subscription
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		sub, err = tx.Get(ctx, id, false)
		return err
	})
	return sub, err
}

// List lista assinaturas.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, f ListFilter) (Page, error) {
	if f.Status != "" {
		if _, ok := transitions[f.Status]; !ok {
			return Page{}, apperr.Invalid("invalid_filter", "Filtro de situação inválido.", apperr.FieldError{Field: "status", Message: "Situação desconhecida."})
		}
	}
	var p Page
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		p, err = tx.List(ctx, f)
		return err
	})
	return p, err
}

// History retorna o histórico de transições.
func (s *Service) History(ctx context.Context, tenantID, id uuid.UUID) ([]HistoryEntry, error) {
	var out []HistoryEntry
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if _, err := tx.Get(ctx, id, false); err != nil {
			return err
		}
		var err error
		out, err = tx.History(ctx, id)
		return err
	})
	return out, err
}

// mutate carrega com bloqueio, verifica a versão, aplica a mudança e grava
// histórico, evento e auditoria na mesma transação.
func (s *Service) mutate(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, expectedVersion int,
	action, eventType string, fn func(tx Tx, sub *Subscription, now time.Time) error) (Subscription, error) {
	var sub Subscription
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if sub, err = tx.Get(ctx, id, true); err != nil {
			return err
		}
		if expectedVersion != 0 && sub.Version != expectedVersion {
			return apperr.PreconditionFailed("version_mismatch", "A assinatura foi alterada por outra pessoa. Recarregue e tente novamente.")
		}
		now := s.clock.Now()
		if err := fn(tx, &sub, now); err != nil {
			return err
		}
		if err := tx.Update(ctx, sub); err != nil {
			return err
		}
		sub.Version++
		if err := tx.InsertHistory(ctx, sub.ID, actor, sub.PopEvents()); err != nil {
			return err
		}
		if err := tx.Emit(ctx, outbox.New(tenantID, eventType, "subscription", sub.ID,
			map[string]any{"subscription_id": sub.ID, "status": sub.Status, "cancel_at_period_end": sub.CancelAtPeriodEnd}, now)); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, action, "subscription", sub.ID.String(), map[string]any{"status": sub.Status}))
	})
	return sub, err
}

// Cancel cancela imediatamente ou agenda para o fim do período.
func (s *Service) Cancel(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, mode CancelMode, reason string, expectedVersion int) (Subscription, error) {
	event := outbox.EventSubscriptionCanceled
	if mode == CancelAtPeriodEnd {
		event = outbox.EventSubscriptionUpdated
	}
	return s.mutate(ctx, tenantID, actor, id, expectedVersion, "subscription.canceled", event, func(tx Tx, sub *Subscription, now time.Time) error {
		price, err := tx.PriceVersion(ctx, sub.PriceVersionID)
		if err != nil {
			return err
		}
		// No cancelamento imediato, itens pendentes (por exemplo, correções de
		// consumo) ainda precisam ser faturados no fechamento.
		pending := false
		if mode == CancelImmediately {
			if pending, err = tx.HasOpenPendingItems(ctx, sub.ID); err != nil {
				return err
			}
		}
		if err := sub.Cancel(mode, reason, price.HasUsage(), pending, now); err != nil {
			return err
		}
		// Mudanças e reajustes futuros deixam de valer: no cancelamento ao fim
		// do período não há próximo ciclo em que aplicá-los.
		return tx.CancelPendingTerms(ctx, sub.ID, now)
	})
}

// ResumeRenewal desfaz o cancelamento agendado.
func (s *Service) ResumeRenewal(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, expectedVersion int) (Subscription, error) {
	return s.mutate(ctx, tenantID, actor, id, expectedVersion, "subscription.renewal_resumed", outbox.EventSubscriptionUpdated,
		func(_ Tx, sub *Subscription, now time.Time) error { return sub.ResumeRenewal(now) })
}

// Suspend suspende a assinatura.
func (s *Service) Suspend(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, reason string, expectedVersion int) (Subscription, error) {
	return s.mutate(ctx, tenantID, actor, id, expectedVersion, "subscription.suspended", outbox.EventSubscriptionSuspended,
		func(_ Tx, sub *Subscription, now time.Time) error { return sub.Suspend(reason, now) })
}

// Reactivate reativa a assinatura suspensa.
func (s *Service) Reactivate(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, expectedVersion int) (Subscription, error) {
	return s.mutate(ctx, tenantID, actor, id, expectedVersion, "subscription.reactivated", outbox.EventSubscriptionReactivate,
		func(_ Tx, sub *Subscription, now time.Time) error { return sub.Reactivate(now) })
}
