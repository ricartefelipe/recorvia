package notifications

import (
	"strings"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

var t0 = time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)

func scheduled() Notification {
	return Notification{Status: Scheduled, SendAfter: t0, MaxAttempts: 2, Recipients: []string{"a@x.test"}}
}

func TestNotificationLifecycle(t *testing.T) {
	n := scheduled()
	if !n.Due(t0) || n.Due(t0.Add(-time.Second)) {
		t.Fatal("agendada deveria vencer em send_after")
	}
	if err := n.Claim(t0, t0); err == nil {
		t.Fatal("lease deve terminar depois de now")
	}
	if err := n.Claim(t0.Add(-time.Minute), t0.Add(time.Minute)); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("não vencida não pode ser reivindicada: %v", err)
	}
	if err := n.Claim(t0, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n.Status != Sending || n.Attempts != 1 || n.Due(t0.Add(30*time.Second)) || !n.Due(t0.Add(2*time.Minute)) {
		t.Fatalf("reivindicação inesperada: %+v", n)
	}
	if err := n.MarkAttemptFailed(t0, "421 tente\nmais tarde", false, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n.Status != Scheduled || n.LastError != "421 tente mais tarde" || !n.SendAfter.Equal(t0.Add(time.Minute)) {
		t.Fatalf("retentativa inesperada: %+v", n)
	}
	if err := n.Claim(t0.Add(time.Minute), t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := n.MarkAttemptFailed(t0, "timeout", false, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n.Status != Failed || n.StatusReason != ReasonAttemptsExhausted || !n.Final() {
		t.Fatalf("tentativas esgotadas deveriam falhar: %+v", n)
	}
	if err := n.Cancel(t0, ReasonInvoicePaid); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("final não pode ser cancelada: %v", err)
	}
	if err := n.MarkSent(t0, "x"); err == nil {
		t.Fatal("somente em envio pode ser enviada")
	}
	if err := n.MarkAttemptFailed(t0, "x", false, t0); err == nil {
		t.Fatal("somente em envio pode falhar")
	}
	reasons := []string{}
	for _, e := range n.PopEvents() {
		reasons = append(reasons, e.Reason)
	}
	if strings.Join(reasons, ",") != "attempt,retry_scheduled,attempt,attempts_exhausted" {
		t.Fatalf("histórico inesperado: %v", reasons)
	}
	if len(n.PopEvents()) != 0 {
		t.Fatal("PopEvents deve limpar o histórico pendente")
	}
}

func TestNotificationSentPermanentCancelSuppressDefer(t *testing.T) {
	n := scheduled()
	_ = n.Claim(t0, t0.Add(time.Minute))
	if err := n.MarkSent(t0, "<id@x>"); err != nil {
		t.Fatal(err)
	}
	if n.Status != Sent || n.SentAt == nil || n.MessageID != "<id@x>" || n.LeaseUntil != nil {
		t.Fatalf("envio inesperado: %+v", n)
	}
	p := scheduled()
	_ = p.Claim(t0, t0.Add(time.Minute))
	_ = p.MarkAttemptFailed(t0, "550 recusado", true, t0)
	if p.Status != Failed || p.StatusReason != ReasonPermanentFailure {
		t.Fatalf("erro permanente: %+v", p)
	}
	c := scheduled()
	if err := c.Cancel(t0, ReasonSuperseded); err != nil || c.Status != Canceled || c.StatusReason != ReasonSuperseded {
		t.Fatalf("cancelamento: %+v %v", c, err)
	}
	s := scheduled()
	if err := s.Suppress(t0, ReasonNoRecipient); err != nil || s.Status != Suppressed {
		t.Fatalf("supressão: %+v %v", s, err)
	}
	if err := s.Suppress(t0, ReasonNoRecipient); err == nil {
		t.Fatal("final não pode ser suprimida")
	}
	if err := s.Defer(t0, t0, ReasonOutsideWindow); err == nil {
		t.Fatal("final não pode ser adiada")
	}
	d := scheduled()
	if err := d.Defer(t0, t0.Add(time.Hour), ReasonFrequencyLimit); err != nil || !d.SendAfter.Equal(t0.Add(time.Hour)) {
		t.Fatalf("adiamento: %+v %v", d, err)
	}
	ev := d.PopEvents()
	if len(ev) != 1 || ev[0].Detail != t0.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("histórico do adiamento: %+v", ev)
	}
	if (Notification{Status: Sent}).Due(t0) {
		t.Fatal("enviada não vence")
	}
	f := scheduled()
	if err := f.Fail(t0, ReasonContentUnavailable, "chave\ndesconhecida"); err != nil || f.Status != Failed ||
		f.StatusReason != ReasonContentUnavailable || f.LastError != "chave desconhecida" || f.Attempts != 0 {
		t.Fatalf("falha sem tentativa: %+v %v", f, err)
	}
	if err := f.Fail(t0, ReasonContentUnavailable, "x"); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("final não pode falhar de novo: %v", err)
	}
	r := scheduled()
	_ = r.Claim(t0, t0.Add(time.Minute))
	if err := r.MarkSent(t0, "<id@x>", "velho@x.test (550 caixa inexistente)"); err != nil || r.Status != Sent ||
		r.LastError != "Destinatários recusados: velho@x.test (550 caixa inexistente)" {
		t.Fatalf("envio com recusas: %+v %v", r, err)
	}
}

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		attempt int
		jitter  float64
		want    time.Duration
	}{
		{0, 0.5, time.Minute}, {1, 0.5, time.Minute}, {2, 0.5, 5 * time.Minute}, {3, 0.5, 15 * time.Minute}, {4, 0.5, time.Hour},
		{9, 0.5, time.Hour}, {1, 0, 48 * time.Second}, {1, 1, 72 * time.Second}, {1, -3, 48 * time.Second}, {1, 7, 72 * time.Second},
	}
	for _, c := range cases {
		if got := RetryDelay(c.attempt, c.jitter); got != c.want {
			t.Errorf("RetryDelay(%d, %v) = %v, esperado %v", c.attempt, c.jitter, got, c.want)
		}
	}
}

func TestWindow(t *testing.T) {
	sp, _ := time.LoadLocation("America/Sao_Paulo")
	w := Window{StartHour: 9, EndHour: 18}
	at := func(h, m int) time.Time { return time.Date(2026, 3, 10, h, m, 0, 0, sp) }
	cases := []struct {
		name     string
		t        time.Time
		contains bool
		next     time.Time
	}{
		{"antes", at(7, 30), false, at(9, 0)},
		{"início", at(9, 0), true, at(9, 0)},
		{"dentro", at(14, 5), true, at(14, 5)},
		{"fim exclusivo", at(18, 0), false, time.Date(2026, 3, 11, 9, 0, 0, 0, sp)},
		{"depois", at(23, 59), false, time.Date(2026, 3, 11, 9, 0, 0, 0, sp)},
	}
	for _, c := range cases {
		if got := w.Contains(c.t, sp); got != c.contains {
			t.Errorf("%s: Contains = %v", c.name, got)
		}
		if got := w.Next(c.t, sp); !got.Equal(c.next) || got.Location() != time.UTC {
			t.Errorf("%s: Next = %v, esperado %v", c.name, got, c.next)
		}
	}
	full := Window{StartHour: 0, EndHour: 24}
	if !full.Contains(at(23, 59), sp) || !full.Valid() || !w.Valid() || (Window{StartHour: 10, EndHour: 10}).Valid() {
		t.Fatal("janela integral ou validação inesperada")
	}
	if got := w.NextDayStart(at(10, 0), sp); !got.Equal(time.Date(2026, 3, 11, 9, 0, 0, 0, sp)) {
		t.Fatalf("NextDayStart = %v", got)
	}
	from, to := DayBounds(at(1, 0), sp)
	if !from.Equal(time.Date(2026, 3, 10, 0, 0, 0, 0, sp)) || !to.Equal(time.Date(2026, 3, 11, 0, 0, 0, 0, sp)) {
		t.Fatalf("DayBounds = %v %v", from, to)
	}
}

// A janela segue o relógio civil do fuso, inclusive na mudança de horário.
func TestWindowAcrossDaylightSavingChange(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("fuso indisponível")
	}
	w := Window{StartHour: 8, EndHour: 20}
	before := time.Date(2026, 3, 7, 21, 0, 0, 0, ny) // sábado; domingo muda para EDT
	next := w.Next(before, ny)
	if want := time.Date(2026, 3, 8, 8, 0, 0, 0, ny); !next.Equal(want) || next.In(ny).Hour() != 8 {
		t.Fatalf("Next = %v, esperado %v", next.In(ny), want)
	}
}
