package collections

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// 10/03/2026 12:00 em São Paulo.
var t0 = time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)

type env struct {
	store  *fakeStore
	clock  *clock.Manual
	svc    *Service
	tenant uuid.UUID
	loc    *time.Location
	actor  audit.Actor
}

func newEnv(t *testing.T) *env {
	t.Helper()
	sp, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{store: newFakeStore(sp), clock: clock.NewManual(t0), tenant: ids.New(), loc: sp, actor: audit.System("teste")}
	e.store.tenants = []uuid.UUID{e.tenant}
	e.svc = NewService(e.store, e.clock, nil)
	return e
}

func (e *env) policy(t *testing.T, in PolicyInput) Policy {
	t.Helper()
	p, err := e.svc.CreatePolicy(context.Background(), e.tenant, e.actor, nil, in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// subscription cria uma assinatura mensal ativa ancorada em 01/02/2026.
func (e *env) subscription() subscriptions.Subscription {
	anchor := calendar.Date{Year: 2026, Month: time.February, Day: 1}
	start := anchor.StartIn(e.loc)
	sched, _ := calendar.NewScheduleFromAnchor(calendar.Monthly, anchor, e.loc)
	next := sched.Period(2).Start
	s := subscriptions.Subscription{ID: ids.New(), TenantID: e.tenant, CustomerID: ids.New(), Status: subscriptions.Active, Quantity: 1,
		Interval: calendar.Monthly, Timezone: e.loc.String(), Anchor: anchor, StartedAt: start, CurrentPeriod: sched.Period(1),
		NextBillingIndex: 2, NextBillingAt: &next, Version: 1}
	e.store.state.subs[s.ID] = s
	e.store.state.recipients[s.CustomerID] = []string{"financeiro@cliente.test"}
	return s
}

// invoice cria uma fatura em aberto com vencimento no dia civil informado.
func (e *env) invoice(sub *subscriptions.Subscription, due calendar.Date) notifications.InvoiceInfo {
	dueAt := billing.DueAt(due.AddDays(-5).StartIn(e.loc), 5, e.loc)
	issued := due.AddDays(-5).StartIn(e.loc).Add(10 * time.Hour)
	inv := notifications.InvoiceInfo{ID: ids.New(), Number: "ACME-" + ids.New().String()[30:], Status: billing.Open, CustomerName: "Cliente",
		Total: 50000, AmountDue: 50000, IssuedAt: &issued, DueAt: &dueAt}
	if sub != nil {
		inv.CustomerID, inv.SubscriptionID = sub.CustomerID, &sub.ID
	} else {
		inv.CustomerID = ids.New()
		e.store.state.recipients[inv.CustomerID] = []string{"avulso@cliente.test"}
	}
	e.store.state.invoices[inv.ID] = inv
	return inv
}

func march(day int) calendar.Date { return calendar.Date{Year: 2026, Month: time.March, Day: day} }

func run(t *testing.T, e *env) RunResult {
	t.Helper()
	res, err := e.svc.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *env) eventTypes() []string {
	var out []string
	for _, ev := range e.store.state.events {
		out = append(out, ev.Type)
	}
	return out
}

func TestCreateAndUpdatePolicies(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first := e.policy(t, validInput())
	if !first.IsDefault {
		t.Fatal("a primeira política deve ser a padrão")
	}
	in := validInput()
	in.Name = "Secundária"
	second := e.policy(t, in)
	if second.IsDefault || !e.store.state.policies[first.ID].IsDefault {
		t.Fatal("nova política sem is_default não substitui a padrão")
	}
	in.Name, in.IsDefault = "Nova padrão", true
	third := e.policy(t, in)
	if !third.IsDefault || e.store.state.policies[first.ID].IsDefault {
		t.Fatal("política marcada como padrão substitui a anterior")
	}
	upd := validInput()
	upd.Name, upd.IsDefault = "Secundária alterada", true
	got, err := e.svc.UpdatePolicy(ctx, e.tenant, e.actor, second.ID, upd, second.Version)
	if err != nil || !got.IsDefault || got.Version != 2 || e.store.state.policies[third.ID].IsDefault {
		t.Fatalf("alteração para padrão: %+v %v", got, err)
	}
	if _, err := e.svc.UpdatePolicy(ctx, e.tenant, e.actor, second.ID, upd, 1); !apperr.IsKind(err, apperr.KindPreconditionFailed) {
		t.Fatalf("versão divergente: %v", err)
	}
	if _, err := e.svc.UpdatePolicy(ctx, e.tenant, e.actor, ids.New(), upd, 0); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("política inexistente: %v", err)
	}
	upd.Name = ""
	if _, err := e.svc.UpdatePolicy(ctx, e.tenant, e.actor, second.ID, upd, 0); !apperr.IsKind(err, apperr.KindInvalid) {
		t.Fatalf("dados inválidos: %v", err)
	}
	list, err := e.svc.Policies(ctx, e.tenant)
	if err != nil || len(list) != 3 {
		t.Fatalf("listagem: %v %v", list, err)
	}
	if p, err := e.svc.Policy(ctx, e.tenant, second.ID); err != nil || p.Name != "Secundária alterada" || p.Version != 2 {
		t.Fatalf("consulta da política: %+v %v", p, err)
	}
	if _, err := e.svc.Policy(ctx, e.tenant, ids.New()); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("consulta de política inexistente: %v", err)
	}
	actions := map[string]int{}
	for _, a := range e.store.state.audits {
		actions[a.Action]++
	}
	if actions["collection_policy.created"] != 3 || actions["collection_policy.updated"] != 1 {
		t.Fatalf("auditoria: %v", actions)
	}
	e.store.state.plan = platformbilling.Plan{}
	if _, err := e.svc.CreatePolicy(ctx, e.tenant, e.actor, nil, validInput()); !apperr.IsKind(err, apperr.KindForbidden) {
		t.Fatalf("plano sem o recurso: %v", err)
	}
	if _, err := e.svc.CreatePolicy(ctx, e.tenant, e.actor, nil, PolicyInput{}); !apperr.IsKind(err, apperr.KindInvalid) {
		t.Fatalf("política inválida: %v", err)
	}
}

func TestRunSchedulesLatestDueReminderOnce(t *testing.T) {
	e := newEnv(t)
	e.policy(t, validInput()) // passos -3, 0, 3, 7; janela 9h-18h
	sub := e.subscription()
	before := e.invoice(&sub, march(12)) // passo -3 em 09/03
	late := e.invoice(&sub, march(5))    // passo 3 em 08/03
	e.invoice(&sub, march(20))           // nenhum passo alcançado
	res := run(t, e)
	if res.Reminders != 2 {
		t.Fatalf("esperava 2 lembretes: %+v", res)
	}
	n := e.store.state.notifs[notifications.ReminderKey(before.ID, -3)]
	if n.TemplateKey != notifications.KeyReminderBeforeDue || !strings.Contains(n.Content.Text, "12/03/2026") || n.PolicyID == nil ||
		!n.SendAfter.Equal(t0) || n.Recipients[0] != "financeiro@cliente.test" {
		t.Fatalf("lembrete antes do vencimento: %+v", n)
	}
	l := e.store.state.notifs[notifications.ReminderKey(late.ID, 3)]
	if l.TemplateKey != notifications.KeyReminderAfterDue || !strings.Contains(l.Content.Text, "há 5 dia(s)") {
		t.Fatalf("lembrete após o vencimento: %+v", l)
	}
	if res := run(t, e); res.Reminders != 0 || len(e.store.state.notifs) != 2 {
		t.Fatalf("lembretes não devem se repetir: %+v %d", res, len(e.store.state.notifs))
	}
}

func TestRunRespectsSendWindowAndFeature(t *testing.T) {
	e := newEnv(t)
	in := validInput()
	in.WindowStartHour, in.WindowEndHour = 14, 16
	e.policy(t, in)
	sub := e.subscription()
	inv := e.invoice(&sub, march(10))
	run(t, e)
	n := e.store.state.notifs[notifications.ReminderKey(inv.ID, 0)]
	if want := time.Date(2026, 3, 10, 14, 0, 0, 0, e.loc); !n.SendAfter.Equal(want) {
		t.Fatalf("send_after deve abrir com a janela: %v", n.SendAfter)
	}
	f := newEnv(t)
	f.policy(t, validInput())
	f.store.state.plan = platformbilling.Plan{}
	s2 := f.subscription()
	f.invoice(&s2, march(10))
	if res := run(t, f); res.Reminders != 0 {
		t.Fatalf("sem o recurso no plano não há lembretes: %+v", res)
	}
}

func TestRunMarksOverdueAndPastDueOnce(t *testing.T) {
	e := newEnv(t) // sem política: marca a inadimplência mesmo assim
	sub := e.subscription()
	inv := e.invoice(&sub, march(8))
	manual := e.invoice(nil, march(7))
	res := run(t, e)
	if res.Overdue != 2 || res.Reminders != 0 {
		t.Fatalf("inadimplência: %+v", res)
	}
	got := e.store.state.subs[sub.ID]
	if got.Status != subscriptions.PastDue || got.Version != 2 {
		t.Fatalf("assinatura deveria ficar past_due: %+v", got)
	}
	types := strings.Join(e.eventTypes(), ",")
	if strings.Count(types, outbox.EventInvoiceOverdue) != 2 || !strings.Contains(types, outbox.EventSubscriptionUpdated) {
		t.Fatalf("eventos: %s", types)
	}
	if h := e.store.state.history[sub.ID]; len(h) != 1 || h[0].Event != "past_due" {
		t.Fatalf("histórico: %+v", h)
	}
	if _, ok := e.store.state.delinquencies[inv.ID]; !ok {
		t.Fatal("inadimplência da fatura não registrada")
	}
	if _, ok := e.store.state.delinquencies[manual.ID]; !ok {
		t.Fatal("inadimplência da fatura avulsa não registrada")
	}
	if res := run(t, e); res.Overdue != 0 || len(e.store.state.events) != 3 {
		t.Fatalf("segunda execução não deve repetir eventos: %+v %v", res, e.eventTypes())
	}
}

func TestRunSuspendsAfterDaysAndReactivatesOnPayment(t *testing.T) {
	e := newEnv(t)
	p := e.policy(t, validInput()) // suspende após 15 dias
	sub := e.subscription()
	inv := e.invoice(&sub, march(1))
	if res := run(t, e); res.Suspended != 0 {
		t.Fatalf("9 dias de atraso ainda não suspendem: %+v", res)
	}
	e.clock.Set(time.Date(2026, 3, 16, 15, 0, 0, 0, time.UTC))
	res := run(t, e)
	if res.Suspended != 1 {
		t.Fatalf("esperava suspensão: %+v", res)
	}
	got := e.store.state.subs[sub.ID]
	if got.Status != subscriptions.Suspended || !strings.Contains(got.SuspensionReason, inv.Number) {
		t.Fatalf("assinatura suspensa: %+v", got)
	}
	h := e.store.state.history[sub.ID]
	last := h[len(h)-1]
	if last.Event != "suspended" || last.Details["origin"] != "collection_policy" || last.Details["days_overdue"] != 15 {
		t.Fatalf("histórico da suspensão: %+v", last)
	}
	var susp Suspension
	for _, s := range e.store.state.suspensions {
		susp = s
	}
	if susp.Status != SuspensionActive || susp.PolicyID != p.ID || susp.InvoiceID != inv.ID || susp.DaysOverdue != 15 {
		t.Fatalf("registro da suspensão: %+v", susp)
	}
	notice := e.store.state.notifs[notifications.SuspensionKey(susp.ID)]
	if notice.TemplateKey != notifications.KeySubscriptionSuspended || !strings.Contains(notice.Content.Text, "15 dia(s)") {
		t.Fatalf("aviso de suspensão: %+v", notice)
	}
	if res := run(t, e); res.Suspended != 0 || res.Reactivated != 0 {
		t.Fatalf("sem pagamento não reativa: %+v", res)
	}
	// Pagamento: o consumidor da fatura paga cancela lembretes e reativa.
	inv.Status, inv.AmountDue, inv.AmountPaid = billing.Paid, 0, inv.Total
	e.store.state.invoices[inv.ID] = inv
	reminder := e.store.state.notifs[notifications.ReminderKey(inv.ID, 7)]
	if reminder.Status != notifications.Scheduled {
		t.Fatalf("esperava lembrete agendado: %+v", reminder)
	}
	msg := ids.New()
	if err := e.svc.HandleInvoiceResolved(context.Background(), e.tenant, inv.ID, msg); err != nil {
		t.Fatal(err)
	}
	got = e.store.state.subs[sub.ID]
	if got.Status != subscriptions.Active {
		t.Fatalf("assinatura deveria ser reativada: %+v", got)
	}
	if s := e.store.state.suspensions[susp.ID]; s.Status != SuspensionReactivated || s.EndReason != EndPaymentRegularized {
		t.Fatalf("suspensão encerrada: %+v", s)
	}
	if r := e.store.state.notifs[notifications.ReminderKey(inv.ID, 7)]; r.Status != notifications.Canceled || r.StatusReason != notifications.ReasonInvoicePaid {
		t.Fatalf("lembrete cancelado após pagamento: %+v", r)
	}
	if !e.store.state.resolved[inv.ID] {
		t.Fatal("inadimplência resolvida")
	}
	types := strings.Join(e.eventTypes(), ",")
	if !strings.Contains(types, outbox.EventSubscriptionSuspended) || !strings.Contains(types, outbox.EventSubscriptionReactivate) {
		t.Fatalf("eventos: %s", types)
	}
	n := len(e.store.state.events)
	if err := e.svc.HandleInvoiceResolved(context.Background(), e.tenant, inv.ID, msg); err != nil || len(e.store.state.events) != n {
		t.Fatalf("mensagem repetida não deve ter efeito: %v", err)
	}
	// Locking: faturas antes da assinatura.
	locks := strings.Join(e.store.state.locked, ",")
	if strings.Index(locks, "subscription:") < strings.Index(locks, "invoice:") {
		t.Fatalf("ordem de bloqueio: %s", locks)
	}
}

func TestSuspensionWithoutAutomaticReactivation(t *testing.T) {
	e := newEnv(t)
	in := validInput()
	in.ReactivateOnPayment, in.SuspendAfterDays = false, ptr(3)
	e.policy(t, in)
	sub := e.subscription()
	inv := e.invoice(&sub, march(5))
	if res := run(t, e); res.Suspended != 1 {
		t.Fatalf("suspensão: %+v", res)
	}
	inv.Status, inv.AmountDue = billing.Paid, 0
	e.store.state.invoices[inv.ID] = inv
	if err := e.svc.HandleInvoiceResolved(context.Background(), e.tenant, inv.ID, ids.New()); err != nil {
		t.Fatal(err)
	}
	if e.store.state.subs[sub.ID].Status != subscriptions.Suspended {
		t.Fatal("política sem reativação automática mantém a suspensão")
	}
	// Reativação manual libera o registro na execução seguinte.
	s := e.store.state.subs[sub.ID]
	if err := s.Reactivate(e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	e.store.state.subs[sub.ID] = s
	run(t, e)
	for _, susp := range e.store.state.suspensions {
		if susp.Status != SuspensionReleased || susp.EndReason != EndManualReactivation {
			t.Fatalf("suspensão deveria ser liberada: %+v", susp)
		}
	}
}

func TestSuspensionReleasedWhenCanceled(t *testing.T) {
	e := newEnv(t)
	in := validInput()
	in.SuspendAfterDays = ptr(1)
	e.policy(t, in)
	sub := e.subscription()
	e.invoice(&sub, march(5))
	run(t, e)
	s := e.store.state.subs[sub.ID]
	end := e.clock.Now()
	s.Status, s.EndedAt = subscriptions.Canceled, &end
	e.store.state.subs[sub.ID] = s
	run(t, e)
	for _, susp := range e.store.state.suspensions {
		if susp.Status != SuspensionReleased || susp.EndReason != EndSubscriptionCanceled {
			t.Fatalf("cancelamento libera a suspensão: %+v", susp)
		}
	}
}

func TestHandleInvoiceResolvedPartialCreditIsNoop(t *testing.T) {
	e := newEnv(t)
	e.policy(t, validInput())
	sub := e.subscription()
	inv := e.invoice(&sub, march(10))
	run(t, e)
	inv.AmountCredited, inv.AmountDue = 100, inv.AmountDue-100
	e.store.state.invoices[inv.ID] = inv
	if err := e.svc.HandleInvoiceResolved(context.Background(), e.tenant, inv.ID, ids.New()); err != nil {
		t.Fatal(err)
	}
	if r := e.store.state.notifs[notifications.ReminderKey(inv.ID, 0)]; r.Status != notifications.Scheduled {
		t.Fatalf("nota de crédito parcial mantém a régua: %+v", r)
	}
	void := e.invoice(nil, march(9))
	void.Status = billing.Void
	e.store.state.invoices[void.ID] = void
	if err := e.svc.HandleInvoiceResolved(context.Background(), e.tenant, void.ID, ids.New()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.HandleInvoiceResolved(context.Background(), e.tenant, ids.New(), ids.New()); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("fatura inexistente: %v", err)
	}
}

func TestRunErrorsAreIsolatedPerTenant(t *testing.T) {
	e := newEnv(t)
	e.store.fail["TenantsForCollections"] = errInjected
	if _, err := e.svc.Run(context.Background()); !errors.Is(err, errInjected) {
		t.Fatal("erro na descoberta deve propagar")
	}
	for _, op := range []string{"ResolveDelinquencies", "Organization", "PlatformPlan", "DefaultPolicy", "ReminderCandidates", "NotificationExists",
		"SubscriptionsToReview", "SuspendedByPolicy", "UnmarkedOverdueInvoices", "LockInvoice", "LockOverdueInvoices", "Subscription",
		"CustomerRecipients", "InsertNotification", "MarkDelinquent", "Emit", "UpdateSubscription", "InsertSubscriptionHistory",
		"ActiveSuspension", "InsertSuspension", "Audit"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			in := validInput()
			in.SuspendAfterDays = ptr(1)
			e.policy(t, in)
			sub := e.subscription()
			e.invoice(&sub, march(9))
			e.invoice(nil, march(2))
			e.store.fail[op] = errInjected
			if _, err := e.svc.Run(context.Background()); !errors.Is(err, errInjected) {
				t.Fatalf("erro em %s deve propagar: %v", op, err)
			}
		})
	}
	t.Run("cancelamento", func(t *testing.T) {
		e := newEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := e.svc.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelamento: %v", err)
		}
	})
}

func TestReactivationErrors(t *testing.T) {
	for _, op := range []string{"Policy", "EndSuspension"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			in := validInput()
			in.SuspendAfterDays = ptr(1)
			e.policy(t, in)
			sub := e.subscription()
			inv := e.invoice(&sub, march(5))
			run(t, e)
			inv.Status, inv.AmountDue = billing.Paid, 0
			e.store.state.invoices[inv.ID] = inv
			e.store.fail[op] = errInjected
			if err := e.svc.HandleInvoiceResolved(context.Background(), e.tenant, inv.ID, ids.New()); !errors.Is(err, errInjected) {
				t.Fatalf("erro em %s deve propagar: %v", op, err)
			}
		})
	}
	for _, op := range []string{"MarkInbox", "Invoice", "CancelReminders", "ResolveDelinquencies"} {
		e := newEnv(t)
		paid := e.invoice(nil, march(5))
		paid.Status, paid.AmountDue = billing.Paid, 0
		e.store.state.invoices[paid.ID] = paid
		e.store.fail[op] = errInjected
		if err := e.svc.HandleInvoiceResolved(context.Background(), e.tenant, paid.ID, ids.New()); !errors.Is(err, errInjected) {
			t.Fatalf("erro em %s deve propagar: %v", op, err)
		}
	}
}

// Muitas faturas em aberto: a régua percorre todas as páginas.
func TestRunPaginatesReminderCandidates(t *testing.T) {
	e := newEnv(t)
	e.svc.BatchSize = 2
	e.policy(t, validInput())
	sub := e.subscription()
	for day := 10; day <= 14; day++ {
		e.invoice(&sub, march(day)) // passo 0 (dia 10) ou -3 (dias 11 a 13); dia 14 ainda sem passo
	}
	res := run(t, e)
	if res.Reminders != 4 {
		t.Fatalf("todas as páginas devem ser avaliadas: %+v", res)
	}
	if res := run(t, e); res.Reminders != 0 {
		t.Fatalf("segunda execução sem novos lembretes: %+v", res)
	}
	e.store.fail["ReminderCandidates"] = errInjected
	if _, err := e.svc.Run(context.Background()); !errors.Is(err, errInjected) {
		t.Fatalf("erro na página deve propagar: %v", err)
	}
}

// Reativação manual com a mesma fatura vencida não é desfeita pela régua.
func TestManualReactivationIsNotUndoneForTheSameInvoice(t *testing.T) {
	e := newEnv(t)
	in := validInput()
	in.SuspendAfterDays, in.ReactivateOnPayment = ptr(3), false
	e.policy(t, in)
	sub := e.subscription()
	e.invoice(&sub, march(5))
	if res := run(t, e); res.Suspended != 1 {
		t.Fatalf("suspensão: %+v", res)
	}
	s := e.store.state.subs[sub.ID]
	if err := s.Reactivate(e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	e.store.state.subs[sub.ID] = s
	run(t, e) // libera a suspensão
	if res := run(t, e); res.Suspended != 0 || e.store.state.subs[sub.ID].Status == subscriptions.Suspended {
		t.Fatalf("a régua não deve suspender de novo pela mesma fatura: %+v", res)
	}
	// Uma nova fatura vencida volta a permitir a suspensão.
	e.clock.Set(time.Date(2026, 4, 20, 15, 0, 0, 0, time.UTC))
	e.invoice(&sub, calendar.Date{Year: 2026, Month: time.April, Day: 1})
	if res := run(t, e); res.Suspended != 0 {
		t.Fatalf("a fatura mais antiga ainda é a da suspensão liberada: %+v", res)
	}
	e.store.fail["ReleasedSuspensionFor"] = errInjected
	if _, err := e.svc.Run(context.Background()); !errors.Is(err, errInjected) {
		t.Fatalf("erro na consulta deve propagar: %v", err)
	}
}

// Fatura vencida cancelada ou baixada: a assinatura em past_due volta a
// active, pelo consumidor da fatura resolvida ou, se ele não chegar, pela
// execução seguinte do job.
func TestPastDueReturnsToActiveWhenOverdueInvoiceIsClosed(t *testing.T) {
	cases := []struct {
		name     string
		status   billing.Status
		consumer bool
	}{
		{"cancelada, pelo consumidor", billing.Void, true},
		{"baixada, pelo consumidor", billing.Uncollectible, true},
		{"cancelada, pelo job", billing.Void, false},
		{"baixada, pelo job", billing.Uncollectible, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			sub := e.subscription()
			inv := e.invoice(&sub, march(8))
			run(t, e)
			if got := e.store.state.subs[sub.ID]; got.Status != subscriptions.PastDue {
				t.Fatalf("assinatura deveria ficar past_due: %+v", got)
			}
			inv.Status, inv.AmountDue = c.status, 0
			e.store.state.invoices[inv.ID] = inv
			events := len(e.store.state.events)
			if c.consumer {
				if err := e.svc.HandleInvoiceResolved(context.Background(), e.tenant, inv.ID, ids.New()); err != nil {
					t.Fatal(err)
				}
			} else {
				run(t, e)
			}
			got := e.store.state.subs[sub.ID]
			if got.Status != subscriptions.Active {
				t.Fatalf("sem fatura vencida a assinatura volta a active: %+v", got)
			}
			h := e.store.state.history[sub.ID]
			if last := h[len(h)-1]; last.Event != "payment_recovered" {
				t.Fatalf("histórico: %+v", last)
			}
			if added := e.eventTypes()[events:]; len(added) != 1 || added[0] != outbox.EventSubscriptionUpdated {
				t.Fatalf("eventos: %v", added)
			}
			if res := run(t, e); res != (RunResult{Tenants: 1}) || e.store.state.subs[sub.ID].Version != got.Version {
				t.Fatalf("assinatura regularizada não muda de novo: %+v", res)
			}
		})
	}
}

// Fatura vencida de assinatura cancelada, suspensa manualmente ou em
// avaliação também é marcada como inadimplente (invoice.overdue uma vez),
// sem alterar a assinatura.
func TestRunMarksOverdueInvoicesOfSubscriptionsOutsideReview(t *testing.T) {
	for _, st := range []subscriptions.Status{subscriptions.Canceled, subscriptions.Suspended, subscriptions.Trialing} {
		t.Run(string(st), func(t *testing.T) {
			e := newEnv(t)
			sub := e.subscription()
			inv := e.invoice(&sub, march(8))
			s := e.store.state.subs[sub.ID]
			s.Status = st
			e.store.state.subs[sub.ID] = s
			if res := run(t, e); res.Overdue != 1 {
				t.Fatalf("inadimplência: %+v", res)
			}
			if _, ok := e.store.state.delinquencies[inv.ID]; !ok {
				t.Fatal("inadimplência não registrada")
			}
			ev := e.store.state.events
			if len(ev) != 1 || ev[0].Type != outbox.EventInvoiceOverdue || ev[0].Payload.(map[string]any)["subscription_id"] != sub.ID {
				t.Fatalf("eventos: %+v", ev)
			}
			if got := e.store.state.subs[sub.ID]; got.Status != st || got.Version != sub.Version {
				t.Fatalf("assinatura não deve mudar: %+v", got)
			}
			if res := run(t, e); res.Overdue != 0 || len(e.store.state.events) != 1 {
				t.Fatalf("segunda execução não repete o evento: %+v", res)
			}
		})
	}
}

// Suspensão manual feita depois de uma reativação manual não é desfeita pela
// régua quando a fatura da suspensão da política é paga.
func TestManualSuspensionAfterManualReactivationIsKept(t *testing.T) {
	e := newEnv(t)
	e.policy(t, validInput()) // suspende após 15 dias e reativa no pagamento
	sub := e.subscription()
	inv := e.invoice(&sub, march(1))
	e.clock.Set(time.Date(2026, 3, 16, 15, 0, 0, 0, time.UTC))
	if res := run(t, e); res.Suspended != 1 {
		t.Fatalf("suspensão: %+v", res)
	}
	s := e.store.state.subs[sub.ID]
	if err := s.Reactivate(e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	if err := s.Suspend("fraude", e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	e.store.state.subs[sub.ID] = s
	inv.Status, inv.AmountDue, inv.AmountPaid = billing.Paid, 0, inv.Total
	e.store.state.invoices[inv.ID] = inv
	if err := e.svc.HandleInvoiceResolved(context.Background(), e.tenant, inv.ID, ids.New()); err != nil {
		t.Fatal(err)
	}
	got := e.store.state.subs[sub.ID]
	if got.Status != subscriptions.Suspended || got.SuspensionReason != "fraude" {
		t.Fatalf("a suspensão manual deve continuar: %+v", got)
	}
	for _, susp := range e.store.state.suspensions {
		if susp.Status != SuspensionReleased || susp.EndReason != EndManualReactivation {
			t.Fatalf("registro da política deve ser liberado: %+v", susp)
		}
	}
}

// A descoberta é paginada: todos os tenants são processados, cada um uma
// vez por execução, e a execução seguinte começa depois do último.
func TestRunPaginatesTenants(t *testing.T) {
	e := newEnv(t)
	e.svc.TenantPageSize = 2
	e.store.tenants = []uuid.UUID{ids.New(), ids.New(), ids.New(), ids.New(), ids.New()}
	res := run(t, e)
	if res.Tenants != 5 {
		t.Fatalf("todos os tenants devem ser processados: %+v", res)
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range e.store.visited {
		seen[id] = true
	}
	for _, id := range e.store.tenants {
		if !seen[id] {
			t.Fatalf("tenant %s não processado", id)
		}
	}
	if p := e.svc.rotation.Position(); p == nil || *p != e.store.tenants[4] {
		t.Fatalf("posição do rodízio: %v", p)
	}
}

// Criação e alteração de políticas serializam a troca da política padrão.
func TestPolicyWritesLockBeforeReadingDefault(t *testing.T) {
	e := newEnv(t)
	p := e.policy(t, validInput())
	if locks := e.store.state.locked; len(locks) != 1 || locks[0] != "policies" {
		t.Fatalf("criação sem bloqueio: %v", locks)
	}
	e.store.state.locked = nil
	in := validInput()
	in.Name = "Alterada"
	if _, err := e.svc.UpdatePolicy(context.Background(), e.tenant, e.actor, p.ID, in, p.Version); err != nil {
		t.Fatal(err)
	}
	if locks := e.store.state.locked; len(locks) != 1 || locks[0] != "policies" {
		t.Fatalf("alteração sem bloqueio: %v", locks)
	}
	e.store.fail["LockPolicies"] = errInjected
	if _, err := e.svc.CreatePolicy(context.Background(), e.tenant, e.actor, nil, validInput()); !errors.Is(err, errInjected) {
		t.Fatalf("erro do bloqueio na criação: %v", err)
	}
	if _, err := e.svc.UpdatePolicy(context.Background(), e.tenant, e.actor, p.ID, in, 0); !errors.Is(err, errInjected) {
		t.Fatalf("erro do bloqueio na alteração: %v", err)
	}
}
