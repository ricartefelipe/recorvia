package collections

import (
	"slices"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/notifications"
)

func ptr[T any](v T) *T { return &v }

func validInput() PolicyInput {
	return PolicyInput{Name: "Padrão", OffsetsDays: []int{7, -3, 0, 3}, WindowStartHour: 9, WindowEndHour: 18, MaxMessagesPerDay: 1,
		SuspendAfterDays: ptr(15), ReactivateOnPayment: true}
}

func TestPolicyValidation(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*PolicyInput)
		field string
	}{
		{"válida", func(*PolicyInput) {}, ""},
		{"sem passos", func(in *PolicyInput) { in.OffsetsDays = nil }, ""},
		{"nome curto", func(in *PolicyInput) { in.Name = " a " }, "name"},
		{"passos demais", func(in *PolicyInput) { in.OffsetsDays = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11} }, "reminder_offsets_days"},
		{"passo antes do limite", func(in *PolicyInput) { in.OffsetsDays = []int{-31} }, "reminder_offsets_days"},
		{"passo depois do limite", func(in *PolicyInput) { in.OffsetsDays = []int{91} }, "reminder_offsets_days"},
		{"passo repetido", func(in *PolicyInput) { in.OffsetsDays = []int{3, 3} }, "reminder_offsets_days"},
		{"início inválido", func(in *PolicyInput) { in.WindowStartHour = 24 }, "send_window_start_hour"},
		{"fim inválido", func(in *PolicyInput) { in.WindowEndHour = 25 }, "send_window_end_hour"},
		{"janela invertida", func(in *PolicyInput) { in.WindowStartHour, in.WindowEndHour = 18, 9 }, "send_window_end_hour"},
		{"mensagens por dia", func(in *PolicyInput) { in.MaxMessagesPerDay = 6 }, "max_messages_per_day"},
		{"suspensão", func(in *PolicyInput) { in.SuspendAfterDays = ptr(0) }, "suspend_after_days"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := validInput()
			c.edit(&in)
			p, err := NewPolicy(ids.New(), ids.New(), in, time.Now())
			if c.field == "" {
				if err != nil {
					t.Fatal(err)
				}
				if p.Version != 1 || !slices.IsSorted(p.OffsetsDays) || p.OffsetsDays == nil {
					t.Fatalf("política inesperada: %+v", p)
				}
				return
			}
			e, ok := apperr.As(err)
			if !ok || e.Kind != apperr.KindInvalid || len(e.Fields) == 0 || e.Fields[0].Field != c.field {
				t.Fatalf("esperava erro em %s: %v", c.field, err)
			}
		})
	}
	p, _ := NewPolicy(ids.New(), ids.New(), validInput(), time.Now())
	in := validInput()
	in.Name = ""
	if err := p.Apply(in, time.Now()); err == nil || p.Name != "Padrão" {
		t.Fatal("alteração inválida não deve mudar a política")
	}
	in.Name, in.OffsetsDays = "Outra", []int{1}
	if err := p.Apply(in, time.Now()); err != nil || p.Name != "Outra" || len(p.OffsetsDays) != 1 {
		t.Fatalf("alteração: %+v %v", p, err)
	}
}

// Input devolve uma cópia dos dados editáveis: alterá-la não muda a política.
func TestPolicyInputRoundTrip(t *testing.T) {
	p, err := NewPolicy(ids.New(), ids.New(), validInput(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	in := p.Input()
	if in.Name != p.Name || in.WindowStartHour != 9 || in.WindowEndHour != 18 || in.MaxMessagesPerDay != 1 || !in.ReactivateOnPayment ||
		in.SuspendAfterDays == nil || *in.SuspendAfterDays != 15 || !slices.Equal(in.OffsetsDays, []int{-3, 0, 3, 7}) {
		t.Fatalf("dados editáveis inesperados: %+v", in)
	}
	in.OffsetsDays[0], *in.SuspendAfterDays = 1, 99
	if p.OffsetsDays[0] != -3 || *p.SuspendAfterDays != 15 {
		t.Fatalf("a cópia compartilha memória com a política: %+v", p)
	}
	if (Policy{}).Input().SuspendAfterDays != nil {
		t.Fatal("política sem suspensão devolve suspensão nula")
	}
}

func TestTemplateForOffset(t *testing.T) {
	if TemplateForOffset(-3) != notifications.KeyReminderBeforeDue || TemplateForOffset(0) != notifications.KeyReminderBeforeDue ||
		TemplateForOffset(1) != notifications.KeyReminderAfterDue {
		t.Fatal("template por passo inesperado")
	}
}

func TestDueStep(t *testing.T) {
	p := Policy{OffsetsDays: []int{-3, 0, 3, 7}}
	d := func(day int) calendar.Date { return calendar.Date{Year: 2026, Month: time.March, Day: day} }
	cases := []struct {
		name          string
		issued, today calendar.Date
		step          int
		ok            bool
	}{
		{"antes do primeiro passo", d(1), d(6), 0, false},
		{"passo antecipado", d(1), d(7), -3, true},
		{"dia do vencimento", d(1), d(10), 0, true},
		{"vencida sem passo posterior alcançado", d(1), d(12), 0, false},
		{"após atraso", d(1), d(14), 3, true},
		{"catch-up usa o passo mais recente", d(1), d(30), 7, true},
		{"passo anterior à emissão não se aplica", d(9), d(9), 0, false},
	}
	for _, c := range cases {
		step, ok := p.DueStep(c.issued, d(10), c.today)
		if ok != c.ok || (ok && step != c.step) {
			t.Errorf("%s: (%d, %v), esperado (%d, %v)", c.name, step, ok, c.step, c.ok)
		}
	}
}

func TestSuspendBeforeAndDaysOverdue(t *testing.T) {
	sp, _ := time.LoadLocation("America/Sao_Paulo")
	now := time.Date(2026, 3, 20, 15, 0, 0, 0, time.UTC)
	if (Policy{}).SuspendBefore(now, sp) != nil {
		t.Fatal("sem suspensão não há limite")
	}
	p := Policy{SuspendAfterDays: ptr(10)}
	got := p.SuspendBefore(now, sp)
	// Vencida em 10/03 (due_at = início de 11/03) atinge 10 dias em 20/03.
	if want := (calendar.Date{Year: 2026, Month: time.March, Day: 11}).StartIn(sp); !got.Equal(want) {
		t.Fatalf("SuspendBefore = %v, esperado %v", got, want)
	}
	if DaysOverdue(calendar.Date{Year: 2026, Month: time.March, Day: 10}, calendar.Date{Year: 2026, Month: time.March, Day: 20}) != 10 ||
		DaysOverdue(calendar.Date{Year: 2026, Month: time.March, Day: 21}, calendar.Date{Year: 2026, Month: time.March, Day: 20}) != 0 {
		t.Fatal("dias de atraso inesperados")
	}
}
