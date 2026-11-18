package notifications

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

type env struct {
	store  *fakeStore
	sender *fakeSender
	clock  *clock.Manual
	svc    *Service
	tenant uuid.UUID
	loc    *time.Location
}

func newEnv(t *testing.T) *env {
	t.Helper()
	sp, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{store: newFakeStore(sp), sender: &fakeSender{}, clock: clock.NewManual(t0), tenant: ids.New(), loc: sp}
	e.store.tenants = []uuid.UUID{e.tenant}
	e.svc = NewService(e.store, e.sender, fakeOpener{}, e.clock, Config{Lease: time.Minute, PublicURL: "https://app.test",
		IsPermanent: func(err error) bool { return errors.As(err, new(permanentErr)) }, Jitter: func() float64 { return 0.5 }})
	return e
}

func (e *env) invoice(status billing.Status, due time.Time) InvoiceInfo {
	issued := due.Add(-10 * 24 * time.Hour)
	inv := InvoiceInfo{ID: ids.New(), Number: "ACME-2026-000001", Status: status, CustomerID: ids.New(), CustomerName: "Cliente Um",
		Total: 99000, AmountDue: 99000, IssuedAt: &issued, DueAt: &due}
	if status == billing.Paid {
		inv.AmountDue, inv.AmountPaid = 0, 99000
	}
	e.store.state.invoices[inv.ID] = inv
	e.store.state.recipients[inv.CustomerID] = []string{"financeiro@cliente.test"}
	return inv
}

// reminder grava um lembrete agendado para a fatura no passo informado.
func (e *env) reminder(t *testing.T, inv InvoiceInfo, offset int, sendAfter time.Time) Notification {
	t.Helper()
	policy := ids.New()
	e.store.state.rules[policy] = ReminderRules{Window: Window{StartHour: 0, EndHour: 24}, MaxPerDay: 2}
	n, err := Compose(context.Background(), nil, Request{BusinessKey: ReminderKey(inv.ID, offset), Key: KeyReminderAfterDue,
		Recipients: e.store.state.recipients[inv.CustomerID], Vars: InvoiceVars(e.store.state.org, inv), CustomerID: &inv.CustomerID,
		InvoiceID: &inv.ID, PolicyID: &policy, StepOffsetDays: &offset, SendAfter: sendAfter}, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	n.SendAfter = sendAfter
	n.PopEvents()
	e.store.put(n)
	return n
}

func TestTemplatesMergeDefaultsAndCustom(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	actor := audit.System("teste")
	got, err := e.svc.UpdateTemplate(ctx, e.tenant, actor, nil, "reminder_after_due", "Cobrança {{invoice_number}}",
		"Olá {{customer_name}}, a fatura venceu há {{days_overdue}} dias.")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.IsDefault {
		t.Fatalf("template gravado inesperado: %+v", got)
	}
	list, err := e.svc.Templates(ctx, e.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5 {
		t.Fatalf("esperava 5 templates, obtive %d", len(list))
	}
	for _, tpl := range list {
		if (tpl.Key == KeyReminderAfterDue) == tpl.IsDefault {
			t.Errorf("%s: is_default=%v", tpl.Key, tpl.IsDefault)
		}
	}
	if len(e.store.state.audits) != 1 || e.store.state.audits[0].Action != "notification_template.updated" {
		t.Fatalf("auditoria ausente: %+v", e.store.state.audits)
	}
	if _, err := e.svc.UpdateTemplate(ctx, e.tenant, actor, nil, "member_invitation", "Assunto", "Corpo suficiente."); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("convite não é personalizável: %v", err)
	}
	if _, err := e.svc.UpdateTemplate(ctx, e.tenant, actor, nil, "invoice_issued", "Assunto", "{{accept_url}} no corpo"); !apperr.IsKind(err, apperr.KindInvalid) {
		t.Fatalf("variável não permitida deveria falhar: %v", err)
	}
	e.store.fail["CustomTemplates"] = errInjected
	if _, err := e.svc.Templates(ctx, e.tenant); !errors.Is(err, errInjected) {
		t.Fatal("erro do store deve propagar")
	}
}

func TestListValidatesStatus(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.List(context.Background(), e.tenant, ListFilter{Status: "queued"}); !apperr.IsKind(err, apperr.KindInvalid) {
		t.Fatalf("situação fora do contrato deve ser rejeitada: %v", err)
	}
	inv := e.invoice(billing.Open, t0.Add(48*time.Hour))
	e.reminder(t, inv, 3, t0)
	p, err := e.svc.List(context.Background(), e.tenant, ListFilter{Status: Scheduled, InvoiceID: &inv.ID})
	if err != nil || len(p.Items) != 1 {
		t.Fatalf("listagem inesperada: %+v %v", p, err)
	}
}

func TestHandleInvoiceIssuedIsIdempotent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	inv := e.invoice(billing.Open, t0.Add(5*24*time.Hour))
	msg := ids.New()
	for range 2 {
		if err := e.svc.HandleInvoiceIssued(ctx, e.tenant, inv.ID, msg); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.svc.HandleInvoiceIssued(ctx, e.tenant, inv.ID, ids.New()); err != nil {
		t.Fatal(err)
	}
	all := e.store.all()
	if len(all) != 1 {
		t.Fatalf("esperava uma notificação, obtive %d", len(all))
	}
	n := all[0]
	if n.BusinessKey != InvoiceIssuedKey(inv.ID) || n.Status != Scheduled || n.Recipients[0] != "financeiro@cliente.test" ||
		!strings.Contains(n.Content.Text, "R$ 990,00") || !strings.Contains(n.Subject, inv.Number) {
		t.Fatalf("notificação inesperada: %+v", n)
	}
	void := e.invoice(billing.Void, t0)
	if err := e.svc.HandleInvoiceIssued(ctx, e.tenant, void.ID, ids.New()); err != nil || len(e.store.all()) != 1 {
		t.Fatalf("fatura cancelada não gera aviso: %v", err)
	}
	if err := e.svc.HandleInvoiceIssued(ctx, e.tenant, ids.New(), ids.New()); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("fatura inexistente: %v", err)
	}
}

func TestHandlePaymentConfirmed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	inv := e.invoice(billing.Paid, t0)
	pay := PaymentInfo{ID: ids.New(), InvoiceID: inv.ID, Amount: 99000, PaidAt: t0}
	e.store.state.payments[pay.ID] = pay
	if err := e.svc.HandlePaymentConfirmed(ctx, e.tenant, pay.ID, ids.New()); err != nil {
		t.Fatal(err)
	}
	all := e.store.all()
	if len(all) != 1 || all[0].BusinessKey != PaymentConfirmedKey(pay.ID) || !strings.Contains(all[0].Content.Text, "10/03/2026 às 12:00") {
		t.Fatalf("aviso de pagamento inesperado: %+v", all)
	}
	e.store.fail["Payment"] = errInjected
	if err := e.svc.HandlePaymentConfirmed(ctx, e.tenant, pay.ID, ids.New()); !errors.Is(err, errInjected) {
		t.Fatal("erro deve propagar")
	}
}

func TestHandleMemberInvited(t *testing.T) {
	ctx := context.Background()
	newInvite := func(e *env, pending bool) InvitationInfo {
		inv := InvitationInfo{ID: ids.New(), Email: "nova@empresa.test", Role: tenancy.RoleFinance, ExpiresAt: t0.Add(7 * 24 * time.Hour), Pending: pending}
		e.store.state.invitations[inv.ID] = inv
		return inv
	}
	token := base64.StdEncoding.EncodeToString([]byte("token-secreto-do-convite"))
	cases := []struct {
		name    string
		pending bool
		ev      func(id uuid.UUID) MemberInvited
		opener  Opener
		status  Status
		reason  string
	}{
		{"enviado", true, func(id uuid.UUID) MemberInvited { return MemberInvited{id, token, "k1"} }, fakeOpener{}, Scheduled, ""},
		{"sem token", true, func(id uuid.UUID) MemberInvited { return MemberInvited{InvitationID: id} }, fakeOpener{}, Suppressed, ReasonLinkUnavailable},
		{"token inválido", true, func(id uuid.UUID) MemberInvited { return MemberInvited{id, "%%%", "k1"} }, fakeOpener{}, Suppressed, ReasonLinkUnavailable},
		{"chave errada", true, func(id uuid.UUID) MemberInvited { return MemberInvited{id, token, "k2"} }, fakeOpener{}, Suppressed, ReasonLinkUnavailable},
		{"sem decifrador", true, func(id uuid.UUID) MemberInvited { return MemberInvited{id, token, "k1"} }, nil, Suppressed, ReasonLinkUnavailable},
		{"não pendente", false, func(id uuid.UUID) MemberInvited { return MemberInvited{id, token, "k1"} }, fakeOpener{}, Suppressed, ReasonInvitationNotPending},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.svc.opener = c.opener
			inv := newInvite(e, c.pending)
			if err := e.svc.HandleMemberInvited(ctx, e.tenant, ids.New(), c.ev(inv.ID)); err != nil {
				t.Fatal(err)
			}
			all := e.store.all()
			if len(all) != 1 || all[0].Status != c.status || all[0].StatusReason != c.reason || all[0].InvitationID == nil {
				t.Fatalf("notificação inesperada: %+v", all)
			}
			if c.status == Scheduled {
				if !strings.Contains(all[0].Content.Text, "https://app.test/convite?token=token-secreto-do-convite") ||
					!strings.Contains(all[0].Content.Text, "financeiro") || all[0].Recipients[0] != inv.Email {
					t.Fatalf("conteúdo do convite: %q", all[0].Content.Text)
				}
			} else if strings.Contains(all[0].Content.Text, "token-secreto") {
				t.Fatal("convite suprimido não deve conter o token")
			}
		})
	}
}

func dispatch(t *testing.T, e *env) DispatchResult {
	t.Helper()
	res, err := e.svc.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestDispatchSendsAndRecordsHistory(t *testing.T) {
	e := newEnv(t)
	inv := e.invoice(billing.Open, t0.Add(48*time.Hour))
	if err := e.svc.HandleInvoiceIssued(context.Background(), e.tenant, inv.ID, ids.New()); err != nil {
		t.Fatal(err)
	}
	e.store.state.recipients[inv.CustomerID] = []string{"novo@cliente.test", "outro@cliente.test"}
	res := dispatch(t, e)
	if res.Sent != 1 || len(e.sender.sent) != 1 {
		t.Fatalf("esperava um envio: %+v", res)
	}
	mail := e.sender.sent[0]
	if mail.FromName != "Acme Serviços" || len(mail.To) != 2 || mail.To[0] != "novo@cliente.test" || mail.HTML == "" {
		t.Fatalf("e-mail inesperado (destinatários devem refletir as preferências no envio): %+v", mail)
	}
	n := e.store.all()[0]
	if n.Status != Sent || n.MessageID == "" || n.SentAt == nil || n.Attempts != 1 {
		t.Fatalf("situação após envio: %+v", n)
	}
	var reasons []string
	for _, ev := range e.store.history(n.ID) {
		reasons = append(reasons, ev.Reason)
	}
	if strings.Join(reasons, ",") != "scheduled,attempt,sent" {
		t.Fatalf("histórico: %v", reasons)
	}
	if res := dispatch(t, e); res.Sent != 0 || len(e.sender.sent) != 1 {
		t.Fatalf("enviada não deve ser reenviada: %+v", res)
	}
}

func TestDispatchRetriesWithBackoffAndFailsAfterLimit(t *testing.T) {
	e := newEnv(t)
	inv := e.invoice(billing.Open, t0.Add(48*time.Hour))
	_ = e.svc.HandleInvoiceIssued(context.Background(), e.tenant, inv.ID, ids.New())
	e.sender.errs = []error{errors.New("421 serviço indisponível"), errors.New("conexão recusada"), errors.New("x"), errors.New("y"), errors.New("z")}
	for attempt := 1; attempt <= DefaultMaxAttempts; attempt++ {
		res := dispatch(t, e)
		n := e.store.all()[0]
		if attempt < DefaultMaxAttempts {
			if res.Retried != 1 || n.Status != Scheduled || !n.SendAfter.Equal(e.clock.Now().Add(RetryDelay(attempt, 0.5))) {
				t.Fatalf("tentativa %d: %+v %+v", attempt, res, n)
			}
			if res := dispatch(t, e); res.Retried+res.Sent+res.Failed != 0 {
				t.Fatalf("antes do backoff nada deve ser enviado: %+v", res)
			}
			e.clock.Set(n.SendAfter)
			continue
		}
		if res.Failed != 1 || n.Status != Failed || n.StatusReason != ReasonAttemptsExhausted || n.LastError != "z" {
			t.Fatalf("após o limite: %+v %+v", res, n)
		}
	}
}

func TestDispatchPermanentFailure(t *testing.T) {
	e := newEnv(t)
	inv := e.invoice(billing.Open, t0.Add(48*time.Hour))
	_ = e.svc.HandleInvoiceIssued(context.Background(), e.tenant, inv.ID, ids.New())
	e.sender.errs = []error{permanentErr{}}
	res := dispatch(t, e)
	n := e.store.all()[0]
	if res.Failed != 1 || n.Status != Failed || n.StatusReason != ReasonPermanentFailure || n.Attempts != 1 {
		t.Fatalf("erro permanente: %+v %+v", res, n)
	}
}

func TestDispatchStopsAfterResolution(t *testing.T) {
	cases := []struct {
		name   string
		status billing.Status
		credit bool
		reason string
	}{
		{"paga", billing.Paid, false, ReasonInvoicePaid},
		{"quitada por nota de crédito", billing.Paid, true, ReasonInvoiceCredited},
		{"cancelada", billing.Void, false, ReasonInvoiceVoided},
		{"baixada", billing.Uncollectible, false, ReasonInvoiceUncollectible},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			inv := e.invoice(billing.Open, t0.Add(-48*time.Hour))
			n := e.reminder(t, inv, 1, t0)
			inv.Status, inv.AmountDue = c.status, 0
			if c.credit {
				inv.AmountCredited = inv.Total
			}
			e.store.state.invoices[inv.ID] = inv
			dispatch(t, e)
			got := e.store.notification(n.ID)
			if got.Status != Canceled || got.StatusReason != c.reason || len(e.sender.sent) != 0 {
				t.Fatalf("lembrete deveria ser cancelado: %+v", got)
			}
		})
	}
}

func TestDispatchReminderRules(t *testing.T) {
	ctx := context.Background()
	t.Run("substituído por passo posterior", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-5*24*time.Hour))
		old := e.reminder(t, inv, 1, t0)
		e.reminder(t, inv, 3, t0.Add(time.Hour))
		dispatch(t, e)
		if got := e.store.notification(old.ID); got.Status != Canceled || got.StatusReason != ReasonSuperseded {
			t.Fatalf("passo antigo: %+v", got)
		}
	})
	t.Run("lembrete de vencimento próximo após o vencimento", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-time.Hour))
		policy := ids.New()
		e.store.state.rules[policy] = ReminderRules{Window: Window{StartHour: 0, EndHour: 24}, MaxPerDay: 2}
		off := -1
		n, _ := Compose(ctx, nil, Request{BusinessKey: ReminderKey(inv.ID, off), Key: KeyReminderBeforeDue, Recipients: []string{"a@x.test"},
			CustomerID: &inv.CustomerID, InvoiceID: &inv.ID, PolicyID: &policy, StepOffsetDays: &off}, t0)
		e.store.put(n)
		dispatch(t, e)
		if got := e.store.notification(n.ID); got.Status != Canceled || got.StatusReason != ReasonDueDatePassed {
			t.Fatalf("lembrete antes do vencimento não vale depois dele: %+v", got)
		}
	})
	t.Run("fora da janela", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-48*time.Hour))
		n := e.reminder(t, inv, 1, t0)
		e.store.state.rules[*n.PolicyID] = ReminderRules{Window: Window{StartHour: 13, EndHour: 18}, MaxPerDay: 1}
		res := dispatch(t, e) // 12:00 em São Paulo
		got := e.store.notification(n.ID)
		want := time.Date(2026, 3, 10, 13, 0, 0, 0, e.loc)
		if res.Deferred != 1 || got.Status != Scheduled || !got.SendAfter.Equal(want) {
			t.Fatalf("adiamento pela janela: %+v %+v", res, got)
		}
	})
	t.Run("limite diário por cliente", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-48*time.Hour))
		first := e.reminder(t, inv, 1, t0)
		other := InvoiceInfo{ID: ids.New(), Number: "ACME-2", Status: billing.Open, CustomerID: inv.CustomerID, CustomerName: "Cliente Um",
			Total: 10, AmountDue: 10, IssuedAt: inv.IssuedAt, DueAt: inv.DueAt}
		e.store.state.invoices[other.ID] = other
		second := e.reminder(t, other, 2, t0)
		e.store.state.rules[*first.PolicyID] = ReminderRules{Window: Window{StartHour: 0, EndHour: 24}, MaxPerDay: 1}
		e.store.state.rules[*second.PolicyID] = ReminderRules{Window: Window{StartHour: 8, EndHour: 24}, MaxPerDay: 1}
		res := dispatch(t, e)
		if res.Sent != 1 || res.Deferred != 1 {
			t.Fatalf("esperava um envio e um adiamento: %+v", res)
		}
		deferred := e.store.notification(second.ID)
		if deferred.Status != Scheduled || !deferred.SendAfter.Equal(time.Date(2026, 3, 11, 8, 0, 0, 0, e.loc)) {
			t.Fatalf("adiamento pelo limite: %+v", deferred)
		}
		ev := e.store.history(second.ID)
		if ev[len(ev)-1].Reason != ReasonFrequencyLimit {
			t.Fatalf("histórico do limite: %+v", ev)
		}
	})
	t.Run("cliente bloqueado por outra instância", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-48*time.Hour))
		n := e.reminder(t, inv, 1, t0)
		e.store.lockBusy[inv.CustomerID] = true
		res := dispatch(t, e)
		if res.Skipped != 1 || e.store.notification(n.ID).Status != Scheduled {
			t.Fatalf("deveria aguardar a próxima execução: %+v", res)
		}
	})
	t.Run("sem destinatário no envio", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-48*time.Hour))
		n := e.reminder(t, inv, 1, t0)
		e.store.state.recipients[inv.CustomerID] = nil
		dispatch(t, e)
		if got := e.store.notification(n.ID); got.Status != Suppressed || got.StatusReason != ReasonNoRecipient {
			t.Fatalf("preferência de contato: %+v", got)
		}
	})
	t.Run("recurso fora do plano", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-48*time.Hour))
		n := e.reminder(t, inv, 1, t0)
		e.store.state.plan = platformbilling.Plan{}
		dispatch(t, e)
		if got := e.store.notification(n.ID); got.Status != Canceled || got.StatusReason != ReasonFeatureUnavailable {
			t.Fatalf("plano sem cobrança por e-mail: %+v", got)
		}
	})
	_ = ctx
}

func TestDispatchOtherGuards(t *testing.T) {
	ctx := context.Background()
	t.Run("aviso de fatura cancelada", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(48*time.Hour))
		_ = e.svc.HandleInvoiceIssued(ctx, e.tenant, inv.ID, ids.New())
		inv.Status = billing.Void
		e.store.state.invoices[inv.ID] = inv
		dispatch(t, e)
		if got := e.store.all()[0]; got.Status != Canceled || got.StatusReason != ReasonInvoiceVoided {
			t.Fatalf("aviso de fatura cancelada: %+v", got)
		}
	})
	t.Run("suspensão já revertida", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-48*time.Hour))
		sub := ids.New()
		n, _ := Compose(ctx, nil, Request{BusinessKey: SuspensionKey(ids.New()), Key: KeySubscriptionSuspended, Recipients: []string{"a@x.test"},
			Vars: map[string]string{}, CustomerID: &inv.CustomerID, InvoiceID: &inv.ID, SubscriptionID: &sub}, t0)
		e.store.put(n)
		dispatch(t, e)
		if got := e.store.notification(n.ID); got.Status != Canceled || got.StatusReason != ReasonSubscriptionNotActive {
			t.Fatalf("aviso de suspensão: %+v", got)
		}
	})
	t.Run("convite não pendente", func(t *testing.T) {
		e := newEnv(t)
		invite := InvitationInfo{ID: ids.New(), Email: "a@x.test", Role: tenancy.RoleAdmin, Pending: true}
		e.store.state.invitations[invite.ID] = invite
		_ = e.svc.HandleMemberInvited(ctx, e.tenant, ids.New(), MemberInvited{invite.ID, base64.StdEncoding.EncodeToString([]byte("tok")), "k1"})
		invite.Pending = false
		e.store.state.invitations[invite.ID] = invite
		dispatch(t, e)
		if got := e.store.all()[0]; got.Status != Canceled || got.StatusReason != ReasonInvitationNotPending {
			t.Fatalf("convite revogado: %+v", got)
		}
	})
}

func TestDispatchLeaseHandling(t *testing.T) {
	t.Run("lease expirado é retomado", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(48*time.Hour))
		_ = e.svc.HandleInvoiceIssued(context.Background(), e.tenant, inv.ID, ids.New())
		n := e.store.all()[0]
		lease := t0.Add(-time.Second)
		n.Status, n.LeaseUntil, n.Attempts = Sending, &lease, 1
		e.store.put(n)
		res := dispatch(t, e)
		if got := e.store.notification(n.ID); res.Sent != 1 || got.Attempts != 2 || got.Status != Sent {
			t.Fatalf("retomada após lease: %+v %+v", res, got)
		}
	})
	t.Run("outra instância assumiu a tentativa", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(48*time.Hour))
		_ = e.svc.HandleInvoiceIssued(context.Background(), e.tenant, inv.ID, ids.New())
		e.store.bumpOnSend = true
		res := dispatch(t, e)
		if res.LostLease != 1 || res.Sent != 0 {
			t.Fatalf("resultado não deve ser gravado por quem perdeu o lease: %+v", res)
		}
	})
}

func TestDispatchErrors(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.sender = nil
	if _, err := e.svc.Dispatch(ctx); err == nil {
		t.Fatal("sem canal de envio deve falhar")
	}
	for _, op := range []string{"TenantsWithDueNotifications", "Organization", "PlatformPlan", "DueNotifications", "Invoice", "CustomerRecipients",
		"ReminderRules", "TryLockCustomer", "RemindersSent", "NewerReminderExists", "UpdateNotification"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			inv := e.invoice(billing.Open, t0.Add(-48*time.Hour))
			e.reminder(t, inv, 1, t0)
			e.store.fail[op] = errInjected
			if _, err := e.svc.Dispatch(ctx); !errors.Is(err, errInjected) {
				t.Fatalf("erro em %s deve propagar: %v", op, err)
			}
		})
	}
	t.Run("registro do resultado", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(48*time.Hour))
		_ = e.svc.HandleInvoiceIssued(ctx, e.tenant, inv.ID, ids.New())
		e.store.fail["Notification"] = errInjected
		if _, err := e.svc.Dispatch(ctx); !errors.Is(err, errInjected) {
			t.Fatalf("erro ao registrar deve propagar: %v", err)
		}
	})
	t.Run("cancelamento", func(t *testing.T) {
		e := newEnv(t)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := e.svc.Dispatch(cctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelamento deve interromper: %v", err)
		}
	})
}

func TestNewServiceDefaults(t *testing.T) {
	s := NewService(newFakeStore(time.UTC), nil, nil, clock.NewManual(t0), Config{Lease: time.Minute, SendTimeout: 2 * time.Minute})
	if s.cfg.SendTimeout != 30*time.Second || s.cfg.BatchSize != 50 || s.cfg.IsPermanent(errInjected) || s.cfg.Jitter() < 0 {
		t.Fatalf("padrões inesperados: %+v", s.cfg)
	}
	if d := NewService(newFakeStore(time.UTC), nil, nil, clock.NewManual(t0), Config{}); d.cfg.Lease != 2*time.Minute {
		t.Fatal("lease padrão")
	}
	for _, r := range []string{ReasonNoRecipient, ReasonLinkUnavailable, ReasonInvoicePaid, ReasonInvoiceCredited, ReasonInvoiceVoided,
		ReasonInvoiceUncollectible, ReasonSuperseded, ReasonDueDatePassed, ReasonInvitationNotPending, ReasonSubscriptionNotActive, ReasonFeatureUnavailable,
		ReasonContentUnavailable} {
		if ReasonDescription(r) == "" {
			t.Errorf("motivo sem descrição: %s", r)
		}
	}
	if ReasonDescription("sent") != "" {
		t.Fatal("motivo sem descrição deve ser vazio")
	}
}

// O resultado do envio é gravado mesmo quando o job é cancelado logo após o
// aceite do SMTP (desligamento ou tempo limite): sem o registro, a
// notificação seria reenviada depois do lease.
func TestDispatchRecordsResultAfterJobCancellation(t *testing.T) {
	e := newEnv(t)
	inv := e.invoice(billing.Open, t0.Add(48*time.Hour))
	if err := e.svc.HandleInvoiceIssued(context.Background(), e.tenant, inv.ID, ids.New()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.sender.afterSend = cancel
	res, err := e.svc.Dispatch(ctx)
	if err != nil || res.Sent != 1 {
		t.Fatalf("o envio aceito deve ser registrado: %+v %v", res, err)
	}
	if n := e.store.all()[0]; n.Status != Sent || n.Attempts != 1 {
		t.Fatalf("situação após o cancelamento: %+v", n)
	}
	e.sender.afterSend = nil
	e.clock.Advance(5 * time.Minute) // além do lease
	if res := dispatch(t, e); res.Sent != 0 || len(e.sender.sent) != 1 {
		t.Fatalf("a notificação não pode ser reenviada: %+v (%d envios)", res, len(e.sender.sent))
	}
}

// Um lembrete reivindicado de novo após o lease vencer (worker interrompido)
// não conta para o próprio limite diário.
func TestDispatchFrequencyLimitIgnoresTheReclaimedReminder(t *testing.T) {
	e := newEnv(t)
	inv := e.invoice(billing.Open, t0.Add(-48*time.Hour))
	n := e.reminder(t, inv, 1, t0)
	e.store.state.rules[*n.PolicyID] = ReminderRules{Window: Window{StartHour: 0, EndHour: 24}, MaxPerDay: 1}
	lease := t0.Add(-time.Second)
	n.Status, n.LeaseUntil, n.Attempts = Sending, &lease, 1
	e.store.put(n)
	res := dispatch(t, e)
	if got := e.store.notification(n.ID); res.Sent != 1 || res.Deferred != 0 || got.Status != Sent || got.Attempts != 2 {
		t.Fatalf("lembrete retomado deve ser enviado: %+v %+v", res, got)
	}
}

// Uma notificação cujo conteúdo não decifra é encerrada como failed, sem
// impedir o envio das demais do tenant.
func TestDispatchFailsOnlyTheNotificationWithUnreadableContent(t *testing.T) {
	e := newEnv(t)
	bad := e.invoice(billing.Open, t0.Add(48*time.Hour))
	good := e.invoice(billing.Open, t0.Add(72*time.Hour))
	for _, inv := range []InvoiceInfo{bad, good} {
		if err := e.svc.HandleInvoiceIssued(context.Background(), e.tenant, inv.ID, ids.New()); err != nil {
			t.Fatal(err)
		}
	}
	var badID uuid.UUID
	for _, n := range e.store.all() {
		if *n.InvoiceID == bad.ID {
			n.Content, n.ContentError = Content{}, "secrets: chave desconhecida"
			e.store.put(n)
			badID = n.ID
		}
	}
	res := dispatch(t, e)
	if res.Sent != 1 || len(e.sender.sent) != 1 || e.sender.sent[0].ID == badID {
		t.Fatalf("a notificação legível deve ser enviada: %+v", res)
	}
	got := e.store.notification(badID)
	if got.Status != Failed || got.StatusReason != ReasonContentUnavailable || got.Attempts != 0 || !strings.Contains(got.LastError, "chave desconhecida") {
		t.Fatalf("notificação ilegível: %+v", got)
	}
	if h := e.store.history(badID); h[len(h)-1].Reason != ReasonContentUnavailable {
		t.Fatalf("histórico: %+v", h)
	}
	if res := dispatch(t, e); res != (DispatchResult{}) {
		t.Fatalf("nada mais a enviar: %+v", res)
	}
}

// Destinatários recusados pelo servidor (os demais receberam) ficam
// registrados na notificação enviada.
func TestDispatchRecordsRefusedRecipients(t *testing.T) {
	e := newEnv(t)
	inv := e.invoice(billing.Open, t0.Add(48*time.Hour))
	_ = e.svc.HandleInvoiceIssued(context.Background(), e.tenant, inv.ID, ids.New())
	e.sender.refused = []string{"velho@cliente.test (550 5.1.1 caixa inexistente)"}
	if res := dispatch(t, e); res.Sent != 1 {
		t.Fatalf("envio: %+v", res)
	}
	n := e.store.all()[0]
	if n.Status != Sent || !strings.Contains(n.LastError, "velho@cliente.test (550 5.1.1 caixa inexistente)") {
		t.Fatalf("recusas registradas: %+v", n)
	}
	if h := e.store.history(n.ID); h[len(h)-1].Reason != ReasonSent || !strings.Contains(h[len(h)-1].Detail, "velho@cliente.test") {
		t.Fatalf("histórico: %+v", h)
	}
}

// A descoberta é paginada: todos os tenants são visitados, cada um uma vez.
func TestDispatchPaginatesTenants(t *testing.T) {
	e := newEnv(t)
	e.svc.cfg.TenantPageSize = 2
	e.store.tenants = []uuid.UUID{ids.New(), ids.New(), ids.New(), ids.New(), ids.New()}
	dispatch(t, e)
	seen := map[uuid.UUID]int{}
	for _, id := range e.store.visited {
		seen[id]++
	}
	for _, id := range e.store.tenants {
		if seen[id] != 1 {
			t.Fatalf("tenant %s visitado %d vez(es)", id, seen[id])
		}
	}
}
