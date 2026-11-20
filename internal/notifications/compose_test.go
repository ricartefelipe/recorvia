package notifications

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

func TestFormatMoney(t *testing.T) {
	cases := map[int64]string{
		0: "R$ 0,00", 5: "R$ 0,05", 99900: "R$ 999,00", 123456: "R$ 1.234,56", 100000000: "R$ 1.000.000,00",
		-123456: "-R$ 1.234,56", math.MinInt64: "-R$ 92.233.720.368.547.758,08",
	}
	for in, want := range cases {
		if got := FormatMoney(in); got != want {
			t.Errorf("FormatMoney(%d) = %q, esperado %q", in, got, want)
		}
	}
}

func TestFormatDates(t *testing.T) {
	sp, _ := time.LoadLocation("America/Sao_Paulo")
	if got := FormatDate(calendar.Date{Year: 2026, Month: time.March, Day: 5}); got != "05/03/2026" {
		t.Fatal(got)
	}
	if got := FormatDateTime(time.Date(2026, 3, 10, 2, 30, 0, 0, time.UTC), sp); got != "09/03/2026 às 23:30" {
		t.Fatal(got)
	}
	if RoleLabel(tenancy.RoleFinance) != "financeiro" || RoleLabel("outro") != "outro" {
		t.Fatal("rótulo de papel inesperado")
	}
}

func TestNormalizeRecipients(t *testing.T) {
	in := []string{" a@x.test ", "A@X.test", "", "b@x.test", "c@x.test\r\nBcc: d@x.test", "e@x.test,f@x.test"}
	got := NormalizeRecipients(in)
	if strings.Join(got, "|") != "a@x.test|b@x.test" {
		t.Fatalf("destinatários inesperados: %v", got)
	}
	many := make([]string, 30)
	for i := range many {
		many[i] = fmt.Sprintf("contato%d@x.test", i)
	}
	if len(NormalizeRecipients(many)) != MaxRecipients {
		t.Fatal("limite de destinatários não aplicado")
	}
}

func TestInvoiceInfo(t *testing.T) {
	due, issued := t0.Add(48*time.Hour), t0
	base := InvoiceInfo{Status: billing.Open, AmountDue: 100, DueAt: &due, IssuedAt: &issued}
	cases := []struct {
		name   string
		inv    InvoiceInfo
		open   bool
		reason string
	}{
		{"aberta", base, true, ""},
		{"paga", InvoiceInfo{Status: billing.Paid, AmountPaid: 100}, false, ReasonInvoicePaid},
		{"quitada por nota", InvoiceInfo{Status: billing.Paid, AmountCredited: 100}, false, ReasonInvoiceCredited},
		{"cancelada", InvoiceInfo{Status: billing.Void}, false, ReasonInvoiceVoided},
		{"baixada", InvoiceInfo{Status: billing.Uncollectible}, false, ReasonInvoiceUncollectible},
		{"saldo zero", InvoiceInfo{Status: billing.PartiallyPaid, AmountDue: 0}, false, ReasonInvoicePaid},
	}
	for _, c := range cases {
		if c.inv.Open() != c.open || c.inv.ResolutionReason() != c.reason {
			t.Errorf("%s: open=%v reason=%q", c.name, c.inv.Open(), c.inv.ResolutionReason())
		}
	}
	if base.Overdue(t0) || !base.Overdue(due) || (InvoiceInfo{Status: billing.Paid}).Overdue(due) {
		t.Fatal("vencimento inesperado")
	}
	sp, _ := time.LoadLocation("America/Sao_Paulo")
	vars := InvoiceVars(Organization{Name: "Org", Location: sp}, InvoiceInfo{Number: "F-1", CustomerName: "Cliente", Total: 1000, AmountDue: 500, DueAt: &due})
	if vars[VarDueDate] != "12/03/2026" || vars[VarAmountDue] != "R$ 5,00" || vars[VarInvoiceTotal] != "R$ 10,00" || vars[VarOrganizationName] != "Org" {
		t.Fatalf("variáveis inesperadas: %v", vars)
	}
	if _, ok := InvoiceVars(Organization{Location: sp}, InvoiceInfo{})[VarDueDate]; ok {
		t.Fatal("fatura sem vencimento não informa due_date")
	}
}

func TestBusinessKeys(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	for got, want := range map[string]string{
		ReminderKey(id, -3):     "reminder:" + id.String() + ":-3",
		InvoiceIssuedKey(id):    "invoice_issued:" + id.String(),
		PaymentConfirmedKey(id): "payment_confirmed:" + id.String(),
		SuspensionKey(id):       "subscription_suspended:" + id.String(),
		InvitationKey(id):       "member_invitation:" + id.String(),
	} {
		if got != want {
			t.Errorf("%q != %q", got, want)
		}
	}
	if !IsReminderKey(KeyReminderAfterDue) || IsReminderKey(KeyInvoiceIssued) {
		t.Fatal("IsReminderKey inesperado")
	}
}

type staticSource struct {
	t   *Template
	err error
}

func (s staticSource) CustomTemplate(context.Context, TemplateKey) (*Template, error) {
	return s.t, s.err
}

func TestCompose(t *testing.T) {
	ctx := context.Background()
	custom := &Template{Key: KeyInvoiceIssued, Subject: "Personalizado {{invoice_number}}", Body: "Corpo {{customer_name}}"}
	inv := ids.New()
	n, err := Compose(ctx, staticSource{t: custom}, Request{BusinessKey: InvoiceIssuedKey(inv), Key: KeyInvoiceIssued,
		Recipients: []string{"a@x.test", "A@x.test"}, Vars: map[string]string{VarInvoiceNumber: "F-9", VarCustomerName: "Ana"},
		InvoiceID: &inv, SendAfter: t0.Add(-time.Hour)}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if n.Subject != "Personalizado F-9" || n.Content.Text != "Corpo Ana" || n.Status != Scheduled || !n.SendAfter.Equal(t0) ||
		len(n.Recipients) != 1 || n.Channel != ChannelEmail || n.MaxAttempts != DefaultMaxAttempts {
		t.Fatalf("notificação inesperada: %+v", n)
	}
	if ev := n.PopEvents(); len(ev) != 1 || ev[0].Reason != ReasonScheduled {
		t.Fatalf("histórico inesperado: %+v", ev)
	}
	none, err := Compose(ctx, nil, Request{BusinessKey: "k-1", Key: KeyInvoiceIssued, SendAfter: t0}, t0)
	if err != nil || none.Status != Suppressed || none.StatusReason != ReasonNoRecipient {
		t.Fatalf("sem destinatário deveria ser suprimida: %+v %v", none, err)
	}
	forced, err := Compose(ctx, nil, Request{BusinessKey: "k-2", Key: KeyMemberInvitation, Recipients: []string{"a@x.test"},
		SuppressReason: ReasonLinkUnavailable}, t0)
	if err != nil || forced.Status != Suppressed || forced.StatusReason != ReasonLinkUnavailable {
		t.Fatalf("supressão forçada: %+v %v", forced, err)
	}
	off := 3
	for name, req := range map[string]Request{
		"chave curta":        {BusinessKey: "k", Key: KeyInvoiceIssued},
		"template inválido":  {BusinessKey: "k-3", Key: "inexistente"},
		"passo sem política": {BusinessKey: "k-4", Key: KeyReminderAfterDue, StepOffsetDays: &off},
	} {
		if _, err := Compose(ctx, nil, req, t0); err == nil {
			t.Errorf("%s: esperava erro", name)
		}
	}
	if _, err := Compose(ctx, staticSource{err: errInjected}, Request{BusinessKey: "k-5", Key: KeyInvoiceIssued}, t0); err == nil {
		t.Fatal("erro da fonte de templates deve propagar")
	}
	bad := &Template{Key: KeyInvoiceIssued, Subject: "{{accept_url}}", Body: "Corpo"}
	if _, err := Compose(ctx, staticSource{t: bad}, Request{BusinessKey: "k-6", Key: KeyInvoiceIssued}, t0); err == nil {
		t.Fatal("template personalizado inválido deve falhar na renderização")
	}
}
