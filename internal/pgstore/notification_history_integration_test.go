//go:build integration

package pgstore_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// captureSender aceita todos os e-mails e guarda os enviados.
type captureSender struct {
	mu   sync.Mutex
	sent []notifications.Email
}

func (s *captureSender) Send(_ context.Context, e notifications.Email) (notifications.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, e)
	return notifications.Receipt{MessageID: "<" + e.ID.String() + "@teste>"}, nil
}

func (s *captureSender) subjects() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sent))
	for _, e := range s.sent {
		out = append(out, e.Subject)
	}
	return out
}

// O aviso de assinatura suspensa espera a janela de horário da política que
// suspendeu, no fuso do tenant (ADR-081): a suspensão às 12:00 de São Paulo
// com janela das 13:00 às 20:00 adia o aviso para as 13:00, com o motivo
// registrado na notificação e na linha do tempo.
func TestSuspensionNoticeWaitsForPolicyWindow(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "aviso-janela")
	coll := collections.NewService(env.Store.Collections(), env.Clock, nil)
	in := defaultPolicyInput()
	in.WindowStartHour, in.WindowEndHour = 13, 20
	if _, err := coll.CreatePolicy(ctx, tn.ID, tn.Actor, &tn.Owner.ID, in); err != nil {
		t.Fatal(err)
	}
	_, invoiceID := issuedInvoice(t, env, tn, "aviso@cliente.test") // vence em 15/03
	env.Clock.Set(time.Date(2026, 3, 26, 15, 0, 0, 0, time.UTC))    // 12:00 em São Paulo, 11 dias de atraso
	if res, err := coll.RunTenant(ctx, tn.ID); err != nil || res.Suspended != 1 {
		t.Fatalf("suspensão: %+v %v", res, err)
	}
	var noticeID uuid.UUID
	if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM notifications WHERE tenant_id = $1 AND template_key = 'subscription_suspended'`, tn.ID).Scan(&noticeID)
	}); err != nil {
		t.Fatal(err)
	}
	sender := &captureSender{}
	svc := notifications.NewService(env.Store.Notifications(), sender, env.Keyring, env.Clock, notifications.Config{Lease: time.Minute})
	notice := func() notifications.Notification {
		t.Helper()
		page, err := svc.List(ctx, tn.ID, notifications.ListFilter{InvoiceID: &invoiceID, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range page.Items {
			if n.ID == noticeID {
				return n
			}
		}
		t.Fatal("aviso de suspensão fora do histórico")
		return notifications.Notification{}
	}
	if _, err := svc.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	open := time.Date(2026, 3, 26, 16, 0, 0, 0, time.UTC) // 13:00 em São Paulo
	if n := notice(); n.Status != notifications.Scheduled || n.StatusReason != notifications.ReasonOutsideWindow || !n.SendAfter.Equal(open) {
		t.Fatalf("aviso fora da janela: %+v", n)
	}
	for _, s := range sender.subjects() {
		if strings.Contains(s, "suspensa") {
			t.Fatalf("aviso enviado fora da janela: %v", sender.subjects())
		}
	}

	env.Clock.Set(open)
	if _, err := svc.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if n := notice(); n.Status != notifications.Sent || n.StatusReason != "" {
		t.Fatalf("aviso na abertura da janela: %+v", n)
	}
	events, err := svc.Events(ctx, tn.ID, noticeID)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, e := range events {
		reasons = append(reasons, e.Reason)
	}
	if strings.Join(reasons, ",") != "scheduled,outside_send_window,attempt,sent" || events[1].Detail != open.Format(time.RFC3339) {
		t.Fatalf("linha do tempo do aviso: %+v", events)
	}
	// A janela vem da política que suspendeu; outro tenant não a enxerga.
	other := env.NewTenant(t, "aviso-janela-b")
	if _, err := svc.Events(ctx, other.ID, noticeID); err == nil {
		t.Fatal("linha do tempo de outro tenant visível")
	}
	if _, err := svc.Events(ctx, tn.ID, ids.New()); err == nil {
		t.Fatal("notificação inexistente deve falhar")
	}
}

// A situação da suspensão do aviso vem de collection_suspensions: ativa,
// encerrada ou inexistente (aviso de suspensão encerrada não sai, ADR-081).
func TestSuspensionActiveReadsSuspensionStatus(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "aviso-suspensao-encerrada")
	coll := collections.NewService(env.Store.Collections(), env.Clock, nil)
	if _, err := coll.CreatePolicy(ctx, tn.ID, tn.Actor, &tn.Owner.ID, defaultPolicyInput()); err != nil {
		t.Fatal(err)
	}
	issuedInvoice(t, env, tn, "encerrada@cliente.test") // vence em 15/03
	env.Clock.Set(time.Date(2026, 3, 26, 15, 0, 0, 0, time.UTC))
	if res, err := coll.RunTenant(ctx, tn.ID); err != nil || res.Suspended != 1 {
		t.Fatalf("suspensão: %+v %v", res, err)
	}
	var suspensionID uuid.UUID
	if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM collection_suspensions WHERE tenant_id = $1`, tn.ID).Scan(&suspensionID)
	}); err != nil {
		t.Fatal(err)
	}
	read := func(id uuid.UUID) (bool, bool) {
		t.Helper()
		var active, found bool
		err := env.Store.Notifications().InTenant(ctx, tn.ID, func(tx notifications.Tx) error {
			var err error
			active, found, err = tx.SuspensionActive(ctx, id)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return active, found
	}
	if active, found := read(suspensionID); !active || !found {
		t.Fatalf("suspensão ativa: active=%v found=%v", active, found)
	}
	if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE collection_suspensions SET status = 'released', ended_at = $2, end_reason = 'manual_reactivation'
		                         WHERE tenant_id = $1 AND id = $3`, tn.ID, env.Clock.Now(), suspensionID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if active, found := read(suspensionID); active || !found {
		t.Fatalf("suspensão encerrada: active=%v found=%v", active, found)
	}
	if active, found := read(ids.New()); active || found {
		t.Fatalf("suspensão inexistente: active=%v found=%v", active, found)
	}
	// Outro tenant não enxerga a suspensão.
	other := env.NewTenant(t, "aviso-suspensao-outro")
	err := env.Store.Notifications().InTenant(ctx, other.ID, func(tx notifications.Tx) error {
		_, found, err := tx.SuspensionActive(ctx, suspensionID)
		if found {
			t.Fatal("suspensão de outro tenant visível")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
