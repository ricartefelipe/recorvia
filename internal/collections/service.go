package collections

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/tenantscan"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// ConsumerInvoiceResolved identifica o consumidor que interrompe a régua e
// reavalia a suspensão quando uma fatura deixa de estar em aberto.
const ConsumerInvoiceResolved = "collections.invoice_resolved"

// Situação de uma suspensão aplicada pela política.
const (
	SuspensionActive      = "active"
	SuspensionReactivated = "reactivated"
	SuspensionReleased    = "released"
)

// Motivos de encerramento de uma suspensão.
const (
	EndPaymentRegularized   = "payment_regularized"
	EndManualReactivation   = "manual_reactivation"
	EndSubscriptionCanceled = "subscription_canceled"
)

// InvoiceCursor posiciona a paginação das faturas candidatas a lembrete.
type InvoiceCursor struct {
	DueAt time.Time
	ID    uuid.UUID
}

// maxReminderPages limita as páginas de faturas avaliadas por execução.
const maxReminderPages = 100

// Suspension é uma suspensão de assinatura aplicada pela política.
type Suspension struct {
	ID             uuid.UUID
	SubscriptionID uuid.UUID
	PolicyID       uuid.UUID
	InvoiceID      uuid.UUID
	DaysOverdue    int
	Status         string
	SuspendedAt    time.Time
	EndedAt        *time.Time
	EndReason      string
}

// Tx são as operações dentro da transação do tenant.
type Tx interface {
	notifications.TemplateSource
	NotificationExists(ctx context.Context, businessKey string) (bool, error)
	InsertNotification(ctx context.Context, n *notifications.Notification) (bool, error)
	CancelReminders(ctx context.Context, invoiceID uuid.UUID, reason string, now time.Time) (int, error)
	Organization(ctx context.Context) (notifications.Organization, error)
	CustomerRecipients(ctx context.Context, customerID uuid.UUID) ([]string, error)
	PlatformPlan(ctx context.Context) (platformbilling.Plan, error)

	// LockPolicies serializa, por tenant, as gravações que leem ou trocam a
	// política padrão (bloqueio liberado no fim da transação).
	LockPolicies(ctx context.Context) error
	Policies(ctx context.Context) ([]Policy, error)
	Policy(ctx context.Context, id uuid.UUID, forUpdate bool) (Policy, error)
	DefaultPolicy(ctx context.Context) (*Policy, error)
	InsertPolicy(ctx context.Context, p Policy, createdBy *uuid.UUID) error
	UpdatePolicy(ctx context.Context, p Policy, expectedVersion int) error
	ClearDefaultPolicy(ctx context.Context, keepID uuid.UUID) error

	// ReminderCandidates lista faturas em aberto com vencimento até horizon,
	// em ordem de (vencimento, ID) e depois do cursor after, se informado.
	ReminderCandidates(ctx context.Context, horizon time.Time, after *InvoiceCursor, limit int) ([]notifications.InvoiceInfo, error)
	Invoice(ctx context.Context, id uuid.UUID) (notifications.InvoiceInfo, error)
	// LockInvoice bloqueia a fatura em modo compartilhado (primeira na ordem).
	LockInvoice(ctx context.Context, id uuid.UUID) (notifications.InvoiceInfo, error)
	// LockOverdueInvoices bloqueia (compartilhado, em ordem de ID) as faturas
	// vencidas e em aberto da assinatura.
	LockOverdueInvoices(ctx context.Context, subscriptionID uuid.UUID, now time.Time) ([]notifications.InvoiceInfo, error)
	SubscriptionsToReview(ctx context.Context, now time.Time, suspendBefore *time.Time, limit int) ([]uuid.UUID, error)
	UnmarkedOverdueInvoices(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error)
	SuspendedByPolicy(ctx context.Context, limit int) ([]uuid.UUID, error)
	MarkDelinquent(ctx context.Context, inv notifications.InvoiceInfo, now time.Time) (bool, error)
	ResolveDelinquencies(ctx context.Context, now time.Time) (int, error)

	Subscription(ctx context.Context, id uuid.UUID, forUpdate bool) (subscriptions.Subscription, error)
	UpdateSubscription(ctx context.Context, s subscriptions.Subscription) error
	InsertSubscriptionHistory(ctx context.Context, subscriptionID uuid.UUID, actor audit.Actor, ev []subscriptions.HistoryEvent) error
	ActiveSuspension(ctx context.Context, subscriptionID uuid.UUID) (*Suspension, error)
	// ReleasedSuspensionFor informa se uma suspensão pela fatura foi
	// encerrada por reativação manual.
	ReleasedSuspensionFor(ctx context.Context, subscriptionID, invoiceID uuid.UUID) (bool, error)
	InsertSuspension(ctx context.Context, s Suspension) error
	EndSuspension(ctx context.Context, s Suspension) error

	MarkInbox(ctx context.Context, consumer string, messageID uuid.UUID) (bool, error)
	Audit(ctx context.Context, e audit.Entry) error
	Emit(ctx context.Context, e outbox.Event) error
}

// Store abre transações e descobre tenants com trabalho de cobrança.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
	// TenantsForCollections devolve até limit tenants com trabalho de
	// cobrança e ID maior que after (nil: desde o início), em ordem de ID.
	TenantsForCollections(ctx context.Context, horizon time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error)
}

// Service implementa as políticas, o job da régua e a inadimplência.
type Service struct {
	store Store
	clock clock.Clock
	log   *slog.Logger
	// BatchSize limita faturas e assinaturas avaliadas por tenant e execução.
	BatchSize int
	// TenantPageSize é o tamanho das páginas de descoberta de tenants.
	TenantPageSize int
	rotation       tenantscan.Rotation
}

var systemActor = audit.System("collections")

// NewService cria o serviço.
func NewService(store Store, c clock.Clock, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{store: store, clock: c, log: log, BatchSize: 500, TenantPageSize: tenantscan.PageSize}
}

// --- políticas ---

// Policies lista as políticas do tenant.
func (s *Service) Policies(ctx context.Context, tenantID uuid.UUID) ([]Policy, error) {
	var out []Policy
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		out, err = tx.Policies(ctx)
		return err
	})
	return out, err
}

// Policy devolve uma política do tenant.
func (s *Service) Policy(ctx context.Context, tenantID, id uuid.UUID) (Policy, error) {
	var out Policy
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		out, err = tx.Policy(ctx, id, false)
		return err
	})
	return out, err
}

// CreatePolicy cria uma política. A primeira política do tenant (sem padrão
// definido) passa a ser a padrão; uma política marcada como padrão substitui
// a anterior.
func (s *Service) CreatePolicy(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, userID *uuid.UUID, in PolicyInput) (Policy, error) {
	now := s.clock.Now()
	p, err := NewPolicy(ids.New(), tenantID, in, now)
	if err != nil {
		return Policy{}, err
	}
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		plan, err := tx.PlatformPlan(ctx)
		if err != nil {
			return err
		}
		if err := platformbilling.RequireFeature(plan, platformbilling.FeatureEmailDunning); err != nil {
			return err
		}
		// Sem o bloqueio, duas criações simultâneas sem padrão definido
		// gravariam duas políticas padrão (violação do índice único).
		if err := tx.LockPolicies(ctx); err != nil {
			return err
		}
		current, err := tx.DefaultPolicy(ctx)
		if err != nil {
			return err
		}
		if current == nil {
			p.IsDefault = true
		}
		if p.IsDefault && current != nil {
			if err := tx.ClearDefaultPolicy(ctx, p.ID); err != nil {
				return err
			}
		}
		if err := tx.InsertPolicy(ctx, p, userID); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "collection_policy.created", "collection_policy", p.ID.String(), policyDetails(p)))
	})
	if err != nil {
		return Policy{}, err
	}
	return p, nil
}

// UpdatePolicy altera a política com concorrência otimista.
func (s *Service) UpdatePolicy(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, in PolicyInput, expectedVersion int) (Policy, error) {
	now := s.clock.Now()
	var p Policy
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if err := tx.LockPolicies(ctx); err != nil {
			return err
		}
		var err error
		if p, err = tx.Policy(ctx, id, true); err != nil {
			return err
		}
		if expectedVersion != 0 && p.Version != expectedVersion {
			return apperr.PreconditionFailed("version_mismatch", "A política foi alterada por outra pessoa. Recarregue e tente novamente.")
		}
		wasDefault := p.IsDefault
		if err := p.Apply(in, now); err != nil {
			return err
		}
		if p.IsDefault && !wasDefault {
			if err := tx.ClearDefaultPolicy(ctx, p.ID); err != nil {
				return err
			}
		}
		if err := tx.UpdatePolicy(ctx, p, p.Version); err != nil {
			return err
		}
		p.Version++
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "collection_policy.updated", "collection_policy", p.ID.String(), policyDetails(p)))
	})
	if err != nil {
		return Policy{}, err
	}
	return p, nil
}

func policyDetails(p Policy) map[string]any {
	d := map[string]any{"is_default": p.IsDefault, "steps": p.OffsetsDays, "window_start": p.Window.StartHour, "window_end": p.Window.EndHour,
		"max_messages_per_day": p.MaxMessagesPerDay, "reactivate_on_payment": p.ReactivateOnPayment}
	if p.SuspendAfterDays != nil {
		d["suspend_after_days"] = *p.SuspendAfterDays
	}
	return d
}

// --- job da régua ---

// RunResult resume uma execução do job.
type RunResult struct {
	Tenants     int
	Reminders   int
	Overdue     int
	Suspended   int
	Reactivated int
}

func (r *RunResult) add(o RunResult) {
	r.Reminders += o.Reminders
	r.Overdue += o.Overdue
	r.Suspended += o.Suspended
	r.Reactivated += o.Reactivated
}

// Run executa a régua em todos os tenants com trabalho de cobrança: agenda o
// lembrete do passo alcançado, marca faturas vencidas, suspende assinaturas
// pela política e reativa as regularizadas. A descoberta é paginada e começa
// depois do último tenant da execução anterior (rodízio). Falhas de um tenant
// não impedem os demais.
func (s *Service) Run(ctx context.Context) (RunResult, error) {
	horizon := s.clock.Now().AddDate(0, 0, ReminderHorizonDays)
	var res RunResult
	var errs []error
	page := func(ctx context.Context, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
		return s.store.TenantsForCollections(ctx, horizon, after, limit)
	}
	err := s.rotation.Each(ctx, s.TenantPageSize, page, nil, func(t uuid.UUID) {
		res.Tenants++
		r, err := s.RunTenant(ctx, t)
		res.add(r)
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", t, err))
		}
	})
	if err != nil {
		errs = append(errs, err)
	}
	return res, errors.Join(errs...)
}

// RunTenant executa a régua de um tenant.
func (s *Service) RunTenant(ctx context.Context, tenantID uuid.UUID) (RunResult, error) {
	now := s.clock.Now()
	var (
		res      RunResult
		review   []uuid.UUID
		unmarked []uuid.UUID
		policy   *Policy
	)
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if _, err := tx.ResolveDelinquencies(ctx, now); err != nil {
			return err
		}
		org, err := tx.Organization(ctx)
		if err != nil {
			return err
		}
		plan, err := tx.PlatformPlan(ctx)
		if err != nil {
			return err
		}
		if policy, err = tx.DefaultPolicy(ctx); err != nil {
			return err
		}
		if !plan.HasFeature(platformbilling.FeatureEmailDunning) {
			policy = nil
		}
		var suspendBefore *time.Time
		if policy != nil {
			suspendBefore = policy.SuspendBefore(now, org.Location)
		}
		if review, err = tx.SubscriptionsToReview(ctx, now, suspendBefore, s.BatchSize); err != nil {
			return err
		}
		suspended, err := tx.SuspendedByPolicy(ctx, s.BatchSize)
		if err != nil {
			return err
		}
		review = appendUnique(review, suspended)
		unmarked, err = tx.UnmarkedOverdueInvoices(ctx, now, s.BatchSize)
		return err
	})
	if err != nil {
		return res, err
	}
	var errs []error
	if policy != nil {
		n, err := s.scheduleReminders(ctx, tenantID, *policy, now)
		res.Reminders += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, id := range unmarked {
		marked, err := s.markInvoiceOverdue(ctx, tenantID, id, now)
		if err != nil {
			errs = append(errs, err)
		} else if marked {
			res.Overdue++
		}
	}
	for _, id := range review {
		var r RunResult
		err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
			var err error
			r, err = s.reviewSubscription(ctx, tx, tenantID, id, now)
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("assinatura %s: %w", id, err))
			continue
		}
		res.add(r)
	}
	return res, errors.Join(errs...)
}

// scheduleReminders percorre, em páginas, as faturas em aberto com
// vencimento no horizonte e agenda o lembrete do passo alcançado.
func (s *Service) scheduleReminders(ctx context.Context, tenantID uuid.UUID, p Policy, now time.Time) (int, error) {
	if len(p.OffsetsDays) == 0 {
		return 0, nil
	}
	var after *InvoiceCursor
	scheduled := 0
	var errs []error
	for range maxReminderPages {
		var page, pending []notifications.InvoiceInfo
		err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
			org, err := tx.Organization(ctx)
			if err != nil {
				return err
			}
			if page, err = tx.ReminderCandidates(ctx, now.AddDate(0, 0, ReminderHorizonDays), after, s.BatchSize); err != nil {
				return err
			}
			pending, err = s.pendingReminders(ctx, tx, p, org, page, now)
			return err
		})
		if err != nil {
			return scheduled, errors.Join(append(errs, err)...)
		}
		for _, inv := range pending {
			ok, err := s.scheduleReminder(ctx, tenantID, p, inv.ID, now)
			if err != nil {
				errs = append(errs, err)
			} else if ok {
				scheduled++
			}
		}
		if len(page) < s.BatchSize {
			break
		}
		last := page[len(page)-1]
		after = &InvoiceCursor{DueAt: *last.DueAt, ID: last.ID}
	}
	return scheduled, errors.Join(errs...)
}

func appendUnique(a, b []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(a))
	for _, id := range a {
		seen[id] = true
	}
	for _, id := range b {
		if !seen[id] {
			seen[id] = true
			a = append(a, id)
		}
	}
	return a
}

// pendingReminders seleciona as faturas cujo passo alcançado ainda não tem
// lembrete registrado.
func (s *Service) pendingReminders(ctx context.Context, tx Tx, p Policy, org notifications.Organization, invs []notifications.InvoiceInfo,
	now time.Time) ([]notifications.InvoiceInfo, error) {
	today := calendar.DateOf(now, org.Location)
	var out []notifications.InvoiceInfo
	for _, inv := range invs {
		if !inv.Open() {
			continue
		}
		step, ok := p.DueStep(calendar.DateOf(*inv.IssuedAt, org.Location), inv.DueDate(org.Location), today)
		if !ok {
			continue
		}
		exists, err := tx.NotificationExists(ctx, notifications.ReminderKey(inv.ID, step))
		if err != nil {
			return nil, err
		}
		if !exists {
			out = append(out, inv)
		}
	}
	return out, nil
}

// scheduleReminder agenda, em transação própria, o lembrete do passo
// alcançado de uma fatura. A fatura é bloqueada em modo compartilhado antes
// da gravação (ordem fatura, assinatura e cliente) e reavaliada sob o
// bloqueio.
func (s *Service) scheduleReminder(ctx context.Context, tenantID uuid.UUID, p Policy, invoiceID uuid.UUID, now time.Time) (bool, error) {
	inserted := false
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		inv, err := tx.LockInvoice(ctx, invoiceID)
		if err != nil || !inv.Open() {
			return err
		}
		org, err := tx.Organization(ctx)
		if err != nil {
			return err
		}
		loc := org.Location
		due, today := inv.DueDate(loc), calendar.DateOf(now, loc)
		step, ok := p.DueStep(calendar.DateOf(*inv.IssuedAt, loc), due, today)
		if !ok {
			return nil
		}
		recipients, err := tx.CustomerRecipients(ctx, inv.CustomerID)
		if err != nil {
			return err
		}
		vars := notifications.InvoiceVars(org, inv)
		if step <= 0 {
			vars[notifications.VarDaysUntilDue] = fmt.Sprint(max(calendar.DaysBetween(today, due), 0))
		} else {
			vars[notifications.VarDaysOverdue] = fmt.Sprint(DaysOverdue(due, today))
		}
		n, err := notifications.Compose(ctx, tx, notifications.Request{BusinessKey: notifications.ReminderKey(inv.ID, step),
			Key: TemplateForOffset(step), Recipients: recipients, Vars: vars, CustomerID: &inv.CustomerID, InvoiceID: &inv.ID,
			SubscriptionID: inv.SubscriptionID, PolicyID: &p.ID, StepOffsetDays: &step, SendAfter: p.Window.Next(now, loc)}, now)
		if err != nil {
			return err
		}
		inserted, err = tx.InsertNotification(ctx, &n)
		return err
	})
	return inserted, err
}

// markInvoiceOverdue registra a inadimplência de uma fatura que não passa
// pela revisão de assinaturas (avulsa ou de assinatura fora de active e
// past_due).
func (s *Service) markInvoiceOverdue(ctx context.Context, tenantID, invoiceID uuid.UUID, now time.Time) (bool, error) {
	marked := false
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		inv, err := tx.LockInvoice(ctx, invoiceID)
		if err != nil || !inv.Overdue(now) {
			return err
		}
		marked, err = s.markDelinquent(ctx, tx, tenantID, inv, now)
		return err
	})
	return marked, err
}

func (s *Service) markDelinquent(ctx context.Context, tx Tx, tenantID uuid.UUID, inv notifications.InvoiceInfo, now time.Time) (bool, error) {
	first, err := tx.MarkDelinquent(ctx, inv, now)
	if err != nil || !first {
		return false, err
	}
	payload := map[string]any{"invoice_id": inv.ID, "number": inv.Number, "customer_id": inv.CustomerID, "due_at": *inv.DueAt,
		"amount_due": inv.AmountDue}
	if inv.SubscriptionID != nil {
		payload["subscription_id"] = *inv.SubscriptionID
	}
	return true, tx.Emit(ctx, outbox.New(tenantID, outbox.EventInvoiceOverdue, "invoice", inv.ID, payload, now))
}

// reviewSubscription atualiza a situação de uma assinatura conforme as
// faturas vencidas: marca a inadimplência das faturas, passa a assinatura a
// past_due (e de volta a active quando não restam faturas vencidas em
// aberto, por exemplo após cancelamento ou baixa), suspende pela política
// após N dias e reativa (ou libera) a suspensão da política. Bloqueia as
// faturas vencidas antes da assinatura.
func (s *Service) reviewSubscription(ctx context.Context, tx Tx, tenantID, subID uuid.UUID, now time.Time) (RunResult, error) {
	var res RunResult
	overdue, err := tx.LockOverdueInvoices(ctx, subID, now)
	if err != nil {
		return res, err
	}
	sub, err := tx.Subscription(ctx, subID, true)
	if err != nil {
		return res, err
	}
	org, err := tx.Organization(ctx)
	if err != nil {
		return res, err
	}
	for _, inv := range overdue {
		marked, err := s.markDelinquent(ctx, tx, tenantID, inv, now)
		if err != nil {
			return res, err
		}
		if marked {
			res.Overdue++
		}
	}
	var events []string
	switch {
	case len(overdue) > 0 && sub.Status == subscriptions.Active:
		if err := sub.MarkPastDue(now); err != nil {
			return res, err
		}
		events = append(events, outbox.EventSubscriptionUpdated)
	case len(overdue) == 0 && sub.Status == subscriptions.PastDue:
		if err := sub.MarkCurrent(now); err != nil {
			return res, err
		}
		events = append(events, outbox.EventSubscriptionUpdated)
	}
	active, err := tx.ActiveSuspension(ctx, subID)
	if err != nil {
		return res, err
	}
	if active == nil {
		suspended, err := s.maybeSuspend(ctx, tx, tenantID, &sub, overdue, org, now)
		if err != nil {
			return res, err
		}
		if suspended {
			res.Suspended++
			events = append(events, outbox.EventSubscriptionSuspended)
		}
	} else {
		reactivated, err := s.settleSuspension(ctx, tx, tenantID, &sub, *active, overdue, now)
		if err != nil {
			return res, err
		}
		if reactivated {
			res.Reactivated++
			events = append(events, outbox.EventSubscriptionReactivate)
		}
	}
	if len(events) == 0 {
		return res, nil
	}
	if err := tx.UpdateSubscription(ctx, sub); err != nil {
		return res, err
	}
	sub.Version++
	if err := tx.InsertSubscriptionHistory(ctx, sub.ID, systemActor, sub.PopEvents()); err != nil {
		return res, err
	}
	for _, et := range events {
		if err := tx.Emit(ctx, outbox.New(tenantID, et, "subscription", sub.ID,
			map[string]any{"subscription_id": sub.ID, "status": sub.Status, "cancel_at_period_end": sub.CancelAtPeriodEnd}, now)); err != nil {
			return res, err
		}
	}
	return res, nil
}

// maybeSuspend suspende a assinatura quando a fatura vencida mais antiga
// atinge os dias de atraso da política padrão.
func (s *Service) maybeSuspend(ctx context.Context, tx Tx, tenantID uuid.UUID, sub *subscriptions.Subscription,
	overdue []notifications.InvoiceInfo, org notifications.Organization, now time.Time) (bool, error) {
	if len(overdue) == 0 || (sub.Status != subscriptions.Active && sub.Status != subscriptions.PastDue) {
		return false, nil
	}
	plan, err := tx.PlatformPlan(ctx)
	if err != nil || !plan.HasFeature(platformbilling.FeatureEmailDunning) {
		return false, err
	}
	p, err := tx.DefaultPolicy(ctx)
	if err != nil || p == nil || p.SuspendAfterDays == nil {
		return false, err
	}
	oldest := overdue[0]
	for _, inv := range overdue[1:] {
		if inv.DueAt.Before(*oldest.DueAt) {
			oldest = inv
		}
	}
	today := calendar.DateOf(now, org.Location)
	days := DaysOverdue(oldest.DueDate(org.Location), today)
	if days < *p.SuspendAfterDays {
		return false, nil
	}
	// Uma reativação manual com a mesma fatura ainda vencida é decisão do
	// tenant (por exemplo, acordo de pagamento): a régua não suspende de novo
	// pela mesma fatura.
	released, err := tx.ReleasedSuspensionFor(ctx, sub.ID, oldest.ID)
	if err != nil || released {
		return false, err
	}
	reason := fmt.Sprintf("Inadimplência: fatura %s vencida há %d dia(s) (política %s).", oldest.Number, days, p.Name)
	if err := sub.Suspend(reason, now); err != nil {
		return false, err
	}
	sub.AnnotateLastEvent(map[string]any{"origin": "collection_policy", "policy_id": p.ID, "invoice_id": oldest.ID, "days_overdue": days})
	susp := Suspension{ID: ids.New(), SubscriptionID: sub.ID, PolicyID: p.ID, InvoiceID: oldest.ID, DaysOverdue: days,
		Status: SuspensionActive, SuspendedAt: now}
	if err := tx.InsertSuspension(ctx, susp); err != nil {
		return false, err
	}
	recipients, err := tx.CustomerRecipients(ctx, sub.CustomerID)
	if err != nil {
		return false, err
	}
	vars := notifications.InvoiceVars(org, oldest)
	vars[notifications.VarDaysOverdue] = fmt.Sprint(days)
	n, err := notifications.Compose(ctx, tx, notifications.Request{BusinessKey: notifications.SuspensionKey(susp.ID),
		Key: notifications.KeySubscriptionSuspended, Recipients: recipients, Vars: vars, CustomerID: &sub.CustomerID,
		InvoiceID: &oldest.ID, SubscriptionID: &sub.ID, SendAfter: now}, now)
	if err != nil {
		return false, err
	}
	if _, err := tx.InsertNotification(ctx, &n); err != nil {
		return false, err
	}
	return true, tx.Audit(ctx, audit.New(tenantID, now, systemActor, "subscription.suspended", "subscription", sub.ID.String(),
		map[string]any{"origin": "collection_policy", "policy_id": p.ID, "invoice_id": oldest.ID, "days_overdue": days, "suspension_id": susp.ID}))
}

// settleSuspension encerra a suspensão da política: libera quando a
// assinatura foi reativada manualmente (inclusive se depois foi suspensa de
// novo manualmente) ou cancelada; reativa quando não há mais faturas
// vencidas e a política que suspendeu reativa no pagamento.
func (s *Service) settleSuspension(ctx context.Context, tx Tx, tenantID uuid.UUID, sub *subscriptions.Subscription, susp Suspension,
	overdue []notifications.InvoiceInfo, now time.Time) (bool, error) {
	switch {
	case sub.Status == subscriptions.Canceled:
		return false, s.endSuspension(ctx, tx, susp, SuspensionReleased, EndSubscriptionCanceled, now)
	case sub.Status != subscriptions.Suspended || !suspendedByPolicy(*sub, susp):
		// Reativada manualmente, ou suspensa de novo manualmente depois da
		// reativação: a régua nunca desfaz uma suspensão manual.
		return false, s.endSuspension(ctx, tx, susp, SuspensionReleased, EndManualReactivation, now)
	case len(overdue) > 0:
		return false, nil
	}
	p, err := tx.Policy(ctx, susp.PolicyID, false)
	if err != nil || !p.ReactivateOnPayment {
		return false, err
	}
	if err := sub.Reactivate(now); err != nil {
		return false, err
	}
	sub.AnnotateLastEvent(map[string]any{"origin": "collection_policy", "policy_id": p.ID, "suspension_id": susp.ID})
	if err := s.endSuspension(ctx, tx, susp, SuspensionReactivated, EndPaymentRegularized, now); err != nil {
		return false, err
	}
	return true, tx.Audit(ctx, audit.New(tenantID, now, systemActor, "subscription.reactivated", "subscription", sub.ID.String(),
		map[string]any{"origin": "collection_policy", "policy_id": p.ID, "suspension_id": susp.ID}))
}

// suspendedByPolicy informa se a suspensão vigente da assinatura é a
// registrada pela política: a suspensão pela régua grava o mesmo instante na
// assinatura e no registro; uma reativação seguida de nova suspensão manual
// muda o instante da assinatura.
func suspendedByPolicy(sub subscriptions.Subscription, susp Suspension) bool {
	return sub.SuspendedAt != nil && sub.SuspendedAt.Equal(susp.SuspendedAt)
}

func (s *Service) endSuspension(ctx context.Context, tx Tx, susp Suspension, status, reason string, now time.Time) error {
	susp.Status, susp.EndedAt, susp.EndReason = status, &now, reason
	return tx.EndSuspension(ctx, susp)
}

// HandleInvoiceResolved interrompe os lembretes de uma fatura paga,
// cancelada, baixada ou quitada por nota de crédito, registra a resolução da
// inadimplência e reavalia a assinatura (reativação pela política).
func (s *Service) HandleInvoiceResolved(ctx context.Context, tenantID, invoiceID, messageID uuid.UUID) error {
	now := s.clock.Now()
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		first, err := tx.MarkInbox(ctx, ConsumerInvoiceResolved, messageID)
		if err != nil || !first {
			return err
		}
		inv, err := tx.Invoice(ctx, invoiceID)
		if err != nil {
			return err
		}
		reason := inv.ResolutionReason()
		if reason == "" {
			return nil // ainda há saldo a cobrar (por exemplo, nota de crédito parcial)
		}
		if _, err := tx.CancelReminders(ctx, inv.ID, reason, now); err != nil {
			return err
		}
		if _, err := tx.ResolveDelinquencies(ctx, now); err != nil {
			return err
		}
		if inv.SubscriptionID == nil {
			return nil
		}
		_, err = s.reviewSubscription(ctx, tx, tenantID, *inv.SubscriptionID, now)
		return err
	})
}
