package notifications

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
)

// suspensionNotice grava o aviso de suspensão de uma assinatura suspensa
// pela suspensão informada.
func (e *env) suspensionNotice(t *testing.T, inv InvoiceInfo, suspensionID uuid.UUID) Notification {
	t.Helper()
	sub := ids.New()
	e.store.state.suspended[sub] = true
	n, err := Compose(context.Background(), nil, Request{BusinessKey: SuspensionKey(suspensionID), Key: KeySubscriptionSuspended,
		Recipients: e.store.state.recipients[inv.CustomerID], Vars: InvoiceVars(e.store.state.org, inv), CustomerID: &inv.CustomerID,
		InvoiceID: &inv.ID, SubscriptionID: &sub, SendAfter: e.clock.Now()}, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	n.PopEvents()
	e.store.put(n)
	return n
}

// O aviso de suspensão respeita a janela de horário da política que suspendeu,
// no fuso do tenant, como os lembretes (ADR-081); o limite diário de
// frequência continua valendo só para os lembretes.
func TestSuspensionNoticeRespectsSendWindow(t *testing.T) {
	t.Run("fora da janela é adiado até a abertura", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-10*24*time.Hour))
		susp := ids.New()
		e.store.state.windows[susp] = Window{StartHour: 13, EndHour: 18}
		n := e.suspensionNotice(t, inv, susp)
		res := dispatch(t, e) // 12:00 em São Paulo
		got := e.store.notification(n.ID)
		open := time.Date(2026, 3, 10, 13, 0, 0, 0, e.loc)
		if res.Deferred != 1 || res.Sent != 0 || len(e.sender.sent) != 0 || got.Status != Scheduled || !got.SendAfter.Equal(open) ||
			got.StatusReason != ReasonOutsideWindow {
			t.Fatalf("aviso fora da janela deve ser adiado: %+v %+v", res, got)
		}
		if ev := e.store.history(n.ID); len(ev) != 1 || ev[0].Reason != ReasonOutsideWindow || ev[0].Detail != open.UTC().Format(time.RFC3339) {
			t.Fatalf("histórico do adiamento: %+v", ev)
		}
		e.clock.Set(open)
		if res := dispatch(t, e); res.Sent != 1 {
			t.Fatalf("na abertura da janela o aviso deve sair: %+v", res)
		}
		if got := e.store.notification(n.ID); got.Status != Sent || got.StatusReason != "" {
			t.Fatalf("aviso enviado: %+v", got)
		}
	})
	t.Run("depois do fim da janela vai para o dia seguinte", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-10*24*time.Hour))
		susp := ids.New()
		e.store.state.windows[susp] = Window{StartHour: 8, EndHour: 11}
		n := e.suspensionNotice(t, inv, susp)
		dispatch(t, e)
		if got := e.store.notification(n.ID); !got.SendAfter.Equal(time.Date(2026, 3, 11, 8, 0, 0, 0, e.loc)) {
			t.Fatalf("próxima abertura: %v", got.SendAfter)
		}
	})
	t.Run("dentro da janela sai mesmo com o limite diário atingido", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-10*24*time.Hour))
		reminder := e.reminder(t, inv, 1, t0)
		e.store.state.rules[*reminder.PolicyID] = ReminderRules{Window: Window{StartHour: 0, EndHour: 24}, MaxPerDay: 1}
		if res := dispatch(t, e); res.Sent != 1 {
			t.Fatalf("lembrete: %+v", res)
		}
		susp := ids.New()
		e.store.state.windows[susp] = Window{StartHour: 8, EndHour: 18}
		n := e.suspensionNotice(t, inv, susp)
		if res := dispatch(t, e); res.Sent != 1 || res.Deferred != 0 {
			t.Fatalf("o aviso de suspensão não conta no limite dos lembretes: %+v", res)
		}
		if got := e.store.notification(n.ID); got.Status != Sent {
			t.Fatalf("aviso: %+v", got)
		}
	})
	t.Run("suspensão sem política conhecida não tem janela", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-10*24*time.Hour))
		n := e.suspensionNotice(t, inv, ids.New())
		if res := dispatch(t, e); res.Sent != 1 || e.store.notification(n.ID).Status != Sent {
			t.Fatalf("sem janela registrada o aviso sai: %+v", res)
		}
	})
	t.Run("falha ao ler a janela interrompe o envio", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-10*24*time.Hour))
		e.suspensionNotice(t, inv, ids.New())
		e.store.fail["SuspensionWindow"] = errInjected
		if _, err := e.svc.Dispatch(context.Background()); !errors.Is(err, errInjected) {
			t.Fatalf("erro deve propagar: %v", err)
		}
		if len(e.sender.sent) != 0 {
			t.Fatal("nada deve ser enviado")
		}
	})
}

func TestSuspensionIDFromKey(t *testing.T) {
	id := ids.New()
	cases := []struct {
		key  string
		want uuid.UUID
		ok   bool
	}{
		{SuspensionKey(id), id, true},
		{"subscription_suspended:x", uuid.Nil, false},
		{ReminderKey(id, 1), uuid.Nil, false},
	}
	for _, c := range cases {
		if got, ok := suspensionIDFromKey(c.key); got != c.want || ok != c.ok {
			t.Errorf("%q: %v %v", c.key, got, ok)
		}
	}
}

// O motivo da situação atual acompanha a notificação: adiamento pela janela
// ou nova tentativa agendada; some no envio.
func TestStatusReasonFollowsCurrentState(t *testing.T) {
	e := newEnv(t)
	inv := e.invoice(billing.Open, t0.Add(-48*time.Hour))
	n := e.reminder(t, inv, 1, t0)
	e.store.state.rules[*n.PolicyID] = ReminderRules{Window: Window{StartHour: 13, EndHour: 18}, MaxPerDay: 5}
	dispatch(t, e)
	if got := e.store.notification(n.ID); got.Status != Scheduled || got.StatusReason != ReasonOutsideWindow {
		t.Fatalf("adiada pela janela: %+v", got)
	}
	e.clock.Set(time.Date(2026, 3, 10, 13, 0, 0, 0, e.loc))
	e.sender.errs = []error{errors.New("421 serviço indisponível")}
	dispatch(t, e)
	got := e.store.notification(n.ID)
	if got.Status != Scheduled || got.StatusReason != ReasonRetryScheduled {
		t.Fatalf("nova tentativa: %+v", got)
	}
	e.clock.Set(got.SendAfter)
	dispatch(t, e)
	if got := e.store.notification(n.ID); got.Status != Sent || got.StatusReason != "" {
		t.Fatalf("enviada: %+v", got)
	}
}

// A linha do tempo de uma notificação é devolvida em ordem cronológica.
func TestEventsReturnsTimeline(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	inv := e.invoice(billing.Open, t0.Add(48*time.Hour))
	if err := e.svc.HandleInvoiceIssued(ctx, e.tenant, inv.ID, ids.New()); err != nil {
		t.Fatal(err)
	}
	e.sender.errs = []error{errors.New("conexão recusada")}
	dispatch(t, e)
	n := e.store.all()[0]
	e.clock.Set(e.store.notification(n.ID).SendAfter)
	dispatch(t, e)
	events, err := e.svc.Events(ctx, e.tenant, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, ev := range events {
		reasons = append(reasons, ev.Reason)
	}
	if strings.Join(reasons, ",") != "scheduled,attempt,retry_scheduled,attempt,sent" {
		t.Fatalf("linha do tempo: %v", reasons)
	}
	if events[2].Detail != "conexão recusada" || events[3].Attempt != 2 || events[4].Status != Sent {
		t.Fatalf("detalhes: %+v", events)
	}
	if _, err := e.svc.Events(ctx, e.tenant, ids.New()); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("notificação inexistente: %v", err)
	}
	e.store.fail["NotificationEvents"] = errInjected
	if _, err := e.svc.Events(ctx, e.tenant, n.ID); !errors.Is(err, errInjected) {
		t.Fatalf("erro deve propagar: %v", err)
	}
}

// O aviso adiado de uma suspensão já encerrada não sai quando a assinatura é
// suspensa de novo: a nova suspensão tem o próprio aviso, e o antigo é
// cancelado (subscription_not_suspended).
func TestSuspensionNoticeOfEndedSuspensionIsCanceled(t *testing.T) {
	e := newEnv(t)
	inv := e.invoice(billing.Open, t0.Add(-10*24*time.Hour))
	s1, s2 := ids.New(), ids.New()
	e.store.state.windows[s1] = Window{StartHour: 13, EndHour: 18}
	e.store.state.windows[s2] = Window{StartHour: 13, EndHour: 18}
	e.store.state.suspensions[s1] = true
	old := e.suspensionNotice(t, inv, s1)
	if res := dispatch(t, e); res.Deferred != 1 { // 12:00: fora da janela
		t.Fatalf("aviso da primeira suspensão adiado: %+v", res)
	}
	// S1 encerrada (pagamento regularizado) e a assinatura suspensa de novo
	// por S2: a assinatura do aviso antigo está suspensa outra vez.
	e.store.state.suspensions[s1], e.store.state.suspensions[s2] = false, true
	e.store.state.suspended[*old.SubscriptionID] = true
	fresh := e.suspensionNotice(t, inv, s2)
	e.store.state.suspended[*fresh.SubscriptionID] = true
	e.clock.Set(time.Date(2026, 3, 10, 13, 0, 0, 0, e.loc))
	res := dispatch(t, e)
	if got := e.store.notification(old.ID); got.Status != Canceled || got.StatusReason != ReasonSubscriptionNotActive {
		t.Fatalf("aviso da suspensão encerrada: %s %s", got.Status, got.StatusReason)
	}
	if got := e.store.notification(fresh.ID); got.Status != Sent || res.Sent != 1 || len(e.sender.sent) != 1 {
		t.Fatalf("aviso da suspensão ativa: %s %+v", got.Status, res)
	}

	t.Run("erro ao ler a suspensão", func(t *testing.T) {
		e := newEnv(t)
		inv := e.invoice(billing.Open, t0.Add(-10*24*time.Hour))
		n := e.suspensionNotice(t, inv, ids.New())
		e.store.fail["SuspensionActive"] = errInjected
		if _, err := e.svc.Dispatch(context.Background()); !errors.Is(err, errInjected) {
			t.Fatalf("a falha na leitura da suspensão deve ser propagada: %v", err)
		}
		if got := e.store.notification(n.ID); got.Status != Scheduled || len(e.sender.sent) != 0 {
			t.Fatalf("aviso continua agendado: %s", got.Status)
		}
	})
}
