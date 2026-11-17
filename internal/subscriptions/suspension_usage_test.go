package subscriptions

import (
	"slices"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
)

// Assinatura mensal iniciada em 10/01/2026 às 09h (São Paulo): o período n
// começa no dia 10 do mês n+1 (período 0 em janeiro).
var suspensionStart = local(2026, time.January, 10, 9, 0)

// billedThrough cria a assinatura e fatura os passos 0..last (fixo de cada
// período no seu início e consumo do anterior).
func billedThrough(t *testing.T, last int) (Subscription, calendar.Schedule) {
	t.Helper()
	s := mustNew(t, newInput(suspensionStart, 0, suspensionStart))
	sched := schedule(t, s)
	for i := 0; i <= last; i++ {
		billOnce(t, &s, sched.Period(i).Start.Add(time.Minute))
	}
	s.PopEvents()
	return s, sched
}

func suspendAndReactivate(t *testing.T, s *Subscription, suspendAt, reactivateAt time.Time) {
	t.Helper()
	if err := s.Suspend("inadimplência", suspendAt); err != nil {
		t.Fatal(err)
	}
	if err := s.Reactivate(reactivateAt); err != nil {
		t.Fatal(err)
	}
}

func periodsOf(sched calendar.Schedule, idx ...int) []calendar.Period {
	out := make([]calendar.Period, len(idx))
	for i, n := range idx {
		out[i] = sched.Period(n)
	}
	return out
}

func startsOf(sched calendar.Schedule, idx ...int) []time.Time {
	out := make([]time.Time, len(idx))
	for i, n := range idx {
		out[i] = sched.Period(n).Start
	}
	return out
}

func sameStarts(a, b []time.Time) bool {
	return slices.EqualFunc(a, b, func(x, y time.Time) bool { return x.Equal(y) })
}

// O consumo anterior à suspensão no período em que ela começou é fechado pelo
// primeiro passo após a reativação, no lugar do período anterior regular
// (inteiramente suspenso); a lista é limpa e o passo seguinte volta ao
// período anterior regular.
func TestReactivateCarriesUsageOfSuspensionPeriod(t *testing.T) {
	s, sched := billedThrough(t, 1)
	suspendAndReactivate(t, &s, sched.Period(1).Start.Add(5*24*time.Hour), sched.Period(3).Start.Add(3*24*time.Hour))
	if !sameStarts(s.PendingUsageStarts, startsOf(sched, 1)) {
		t.Fatalf("consumo pendente = %v, esperava o início do período 1", s.PendingUsageStarts)
	}
	ev := s.PopEvents()
	if d := ev[len(ev)-1].Details; d["skipped_periods"] != 1 || d["pending_usage_periods"] != 1 {
		t.Fatalf("detalhes da reativação: %+v", d)
	}

	now := sched.Period(3).Start.Add(3 * 24 * time.Hour)
	step, ok, err := s.NextStep(now)
	if err != nil || !ok {
		t.Fatalf("NextStep: ok=%v err=%v", ok, err)
	}
	if step.Kind != StepBill || step.Index != 3 || step.UsagePeriod == nil || *step.UsagePeriod != sched.Period(1) || len(step.CarriedUsage) != 0 {
		t.Fatalf("o passo da reativação fecha o consumo do período 1: %+v", step)
	}
	if !slices.Equal(step.UsagePeriods(), periodsOf(sched, 1)) || *step.UsageSpan() != sched.Period(1) {
		t.Fatalf("períodos de consumo do passo: %v", step.UsagePeriods())
	}
	if !s.UsageOpen(1, sched.Period(1).Start) || s.UsageOpen(2, sched.Period(2).Start) || !s.UsageOpen(3, sched.Period(3).Start) {
		t.Fatal("antes do passo, ficam abertos o período pendente e o da reativação; o suspenso não")
	}
	if err := s.ApplyStep(step, now); err != nil {
		t.Fatal(err)
	}
	if s.PendingUsageStarts != nil || s.UsageOpen(1, sched.Period(1).Start) {
		t.Fatalf("o passo limpa o consumo pendente: %v", s.PendingUsageStarts)
	}
	next := billOnce(t, &s, sched.Period(4).Start)
	if *next.UsagePeriod != sched.Period(3) {
		t.Fatalf("o passo seguinte fecha o período da reativação: %+v", next)
	}
}

func TestReactivateCarriedUsageCases(t *testing.T) {
	cases := []struct {
		name string
		// make devolve a assinatura já reativada e o instante do próximo
		// faturamento.
		make        func(t *testing.T) (Subscription, calendar.Schedule, time.Time)
		wantPending []int
		wantUsage   []int
		wantIndex   int
	}{
		{"reativação no período seguinte não pula passos", func(t *testing.T) (Subscription, calendar.Schedule, time.Time) {
			s, sched := billedThrough(t, 1)
			suspendAndReactivate(t, &s, sched.Period(1).Start.Add(5*24*time.Hour), sched.Period(2).Start.Add(24*time.Hour))
			return s, sched, sched.Period(2).Start.Add(24 * time.Hour)
		}, nil, []int{1}, 2},
		{"faturamento atrasado na suspensão", func(t *testing.T) (Subscription, calendar.Schedule, time.Time) {
			// Somente o passo 0 foi executado; a suspensão acontece no
			// período 2 e a reativação no 5: os passos 1 a 4 são pulados e o
			// consumo dos períodos 0, 1 e 2 fica pendente.
			s, sched := billedThrough(t, 0)
			suspendAndReactivate(t, &s, sched.Period(2).Start.Add(5*24*time.Hour), sched.Period(5).Start.Add(24*time.Hour))
			return s, sched, sched.Period(5).Start.Add(24 * time.Hour)
		}, []int{0, 1, 2}, []int{0, 1, 2}, 5},
		{"nova suspensão antes do passo da reativação", func(t *testing.T) (Subscription, calendar.Schedule, time.Time) {
			s, sched := billedThrough(t, 1)
			suspendAndReactivate(t, &s, sched.Period(1).Start.Add(5*24*time.Hour), sched.Period(3).Start.Add(24*time.Hour))
			suspendAndReactivate(t, &s, sched.Period(3).Start.Add(2*24*time.Hour), sched.Period(6).Start.Add(24*time.Hour))
			return s, sched, sched.Period(6).Start.Add(24 * time.Hour)
		}, []int{1, 3}, []int{1, 3}, 6},
		{"nova suspensão reativada no mesmo período mantém a lista", func(t *testing.T) (Subscription, calendar.Schedule, time.Time) {
			s, sched := billedThrough(t, 1)
			suspendAndReactivate(t, &s, sched.Period(1).Start.Add(5*24*time.Hour), sched.Period(3).Start.Add(24*time.Hour))
			suspendAndReactivate(t, &s, sched.Period(3).Start.Add(2*24*time.Hour), sched.Period(3).Start.Add(4*24*time.Hour))
			return s, sched, sched.Period(3).Start.Add(4 * 24 * time.Hour)
		}, []int{1}, []int{1}, 3},
		{"suspensão no instante do início, antes de qualquer faturamento", func(t *testing.T) (Subscription, calendar.Schedule, time.Time) {
			s := mustNew(t, newInput(suspensionStart, 0, suspensionStart))
			sched := schedule(t, s)
			suspendAndReactivate(t, &s, suspensionStart, sched.Period(3).Start.Add(24*time.Hour))
			return s, sched, sched.Period(3).Start.Add(24 * time.Hour)
		}, nil, []int{2}, 3},
		{"suspensão no primeiro período ainda não faturado", func(t *testing.T) (Subscription, calendar.Schedule, time.Time) {
			s := mustNew(t, newInput(suspensionStart, 0, suspensionStart))
			sched := schedule(t, s)
			suspendAndReactivate(t, &s, suspensionStart.Add(10*24*time.Hour), sched.Period(3).Start.Add(24*time.Hour))
			return s, sched, sched.Period(3).Start.Add(24 * time.Hour)
		}, []int{0}, []int{0}, 3},
		{"suspensão durante a avaliação", func(t *testing.T) (Subscription, calendar.Schedule, time.Time) {
			s := mustNew(t, newInput(suspensionStart, 14, suspensionStart))
			sched := schedule(t, s)
			suspendAndReactivate(t, &s, suspensionStart.Add(24*time.Hour), sched.Period(2).Start.Add(24*time.Hour))
			return s, sched, sched.Period(2).Start.Add(24 * time.Hour)
		}, nil, []int{1}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, sched, now := tc.make(t)
			if !sameStarts(s.PendingUsageStarts, startsOf(sched, tc.wantPending...)) {
				t.Fatalf("consumo pendente = %v, esperava os períodos %v", s.PendingUsageStarts, tc.wantPending)
			}
			step, ok, err := s.NextStep(now)
			if err != nil || !ok {
				t.Fatalf("NextStep: ok=%v err=%v", ok, err)
			}
			if step.Index != tc.wantIndex || !slices.Equal(step.UsagePeriods(), periodsOf(sched, tc.wantUsage...)) {
				t.Fatalf("passo %d com consumo %v, esperava passo %d com os períodos %v", step.Index, step.UsagePeriods(), tc.wantIndex, tc.wantUsage)
			}
			if span := step.UsageSpan(); span.Start != sched.Period(tc.wantUsage[0]).Start || span.End != sched.Period(tc.wantUsage[len(tc.wantUsage)-1]).End {
				t.Fatalf("intervalo de consumo do passo: %+v", span)
			}
			if err := s.ApplyStep(step, now); err != nil {
				t.Fatal(err)
			}
			if s.PendingUsageStarts != nil {
				t.Fatalf("o passo limpa o consumo pendente: %v", s.PendingUsageStarts)
			}
		})
	}
}

func TestUsageOpen(t *testing.T) {
	s, sched := billedThrough(t, 1) // próximo passo 2: fecha o período 1
	cases := []struct {
		name    string
		pending []int
		idx     int
		want    bool
	}{
		{"período fechado pelo passo executado", nil, 0, false},
		{"período anterior ao próximo passo", nil, 1, true},
		{"período corrente", nil, 2, true},
		{"período pendente", []int{0}, 0, true},
		{"anterior regular substituído pela lista", []int{0}, 1, false},
		{"período do próximo passo com lista", []int{0}, 2, true},
	}
	for _, tc := range cases {
		s.PendingUsageStarts = startsOf(sched, tc.pending...)
		if got := s.UsageOpen(tc.idx, sched.Period(tc.idx).Start); got != tc.want {
			t.Errorf("%s: UsageOpen(%d) = %v, esperava %v", tc.name, tc.idx, got, tc.want)
		}
	}
}

// Cancelamento imediato depois da reativação, antes do motor: o fechamento
// final fatura o consumo pendente e o do período interrompido.
func TestCancelAfterReactivationBillsPendingUsage(t *testing.T) {
	cases := []struct {
		name string
		// reactivate e cancel são os instantes de reativação e de
		// cancelamento, a partir do início do período 3.
		reactivate, cancel time.Duration
		hasUsage           bool
		wantRegular        bool
	}{
		{"com consumo no período interrompido", 24 * time.Hour, 25 * time.Hour, true, true},
		{"no início do período corrente", 0, 0, true, false},
		{"plano sem consumo", 24 * time.Hour, 25 * time.Hour, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, sched := billedThrough(t, 1)
			suspendAndReactivate(t, &s, sched.Period(1).Start.Add(5*24*time.Hour), sched.Period(3).Start.Add(tc.reactivate))
			at := sched.Period(3).Start.Add(tc.cancel)
			if err := s.Cancel(CancelImmediately, "", tc.hasUsage, false, at); err != nil {
				t.Fatal(err)
			}
			if !tc.hasUsage {
				if s.NextBillingAt != nil || s.PendingUsageStarts != nil {
					t.Fatalf("sem consumo não há fechamento: %v %v", s.NextBillingAt, s.PendingUsageStarts)
				}
				return
			}
			step, ok, err := s.NextStep(at)
			if err != nil || !ok {
				t.Fatalf("NextStep: ok=%v err=%v", ok, err)
			}
			want := periodsOf(sched, 1)
			if tc.wantRegular {
				want = append(want, calendar.Period{Start: sched.Period(3).Start, End: at})
			}
			if step.Kind != StepFinalUsage || !slices.Equal(step.UsagePeriods(), want) || step.Period != want[len(want)-1] {
				t.Fatalf("fechamento final %+v, esperava os períodos %v", step, want)
			}
			if err := s.ApplyStep(step, at); err != nil {
				t.Fatal(err)
			}
			if s.NextBillingAt != nil || s.PendingUsageStarts != nil {
				t.Fatalf("o fechamento final encerra o faturamento: %v %v", s.NextBillingAt, s.PendingUsageStarts)
			}
		})
	}
}

// Encerramento agendado durante a suspensão: o passo de encerramento após a
// reativação fecha o consumo pendente.
func TestEndAtPeriodEndAfterReactivationBillsPendingUsage(t *testing.T) {
	s, sched := billedThrough(t, 1)
	suspendedAt := sched.Period(1).Start.Add(5 * 24 * time.Hour)
	if err := s.Suspend("", suspendedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(CancelAtPeriodEnd, "", true, false, suspendedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	at := sched.Period(3).Start.Add(24 * time.Hour)
	if err := s.Reactivate(at); err != nil {
		t.Fatal(err)
	}
	step := billOnce(t, &s, at)
	if step.Kind != StepEnd || !slices.Equal(step.UsagePeriods(), periodsOf(sched, 1)) {
		t.Fatalf("encerramento fecha o consumo pendente: %+v", step)
	}
	if s.Status != Canceled || s.PendingUsageStarts != nil {
		t.Fatalf("assinatura encerrada sem pendências: %s %v", s.Status, s.PendingUsageStarts)
	}
}

func TestPendingUsageEdgeCases(t *testing.T) {
	t.Run("suspensão sem instante registrado", func(t *testing.T) {
		s, sched := billedThrough(t, 1)
		s.Status = Suspended
		if err := s.Reactivate(sched.Period(3).Start.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if s.PendingUsageStarts != nil || s.NextBillingIndex != 3 {
			t.Fatalf("sem instante de suspensão nada fica pendente: %v idx=%d", s.PendingUsageStarts, s.NextBillingIndex)
		}
	})
	t.Run("passo sem consumo", func(t *testing.T) {
		var step BillingStep
		step.withUsage(nil)
		if step.UsagePeriods() != nil || step.UsageSpan() != nil {
			t.Fatalf("passo sem consumo: %+v", step)
		}
	})
	t.Run("fuso inválido no fechamento final com consumo pendente", func(t *testing.T) {
		s, sched := billedThrough(t, 1)
		suspendAndReactivate(t, &s, sched.Period(1).Start.Add(5*24*time.Hour), sched.Period(3).Start.Add(time.Hour))
		at := sched.Period(3).Start.Add(2 * time.Hour)
		if err := s.Cancel(CancelImmediately, "", true, false, at); err != nil {
			t.Fatal(err)
		}
		s.Timezone = "Marte/Olympus_Mons"
		if _, _, err := s.NextStep(at); err == nil {
			t.Fatal("NextStep deve propagar o fuso inválido")
		}
	})
}

// FuzzSuspensionClosesUsageOnce: com suspensões, reativações e
// indisponibilidade do motor em qualquer ordem, todo período com tempo
// faturável fora das suspensões tem o consumo fechado exatamente uma vez, e
// nenhum período é fechado duas vezes.
func FuzzSuspensionClosesUsageOnce(f *testing.F) {
	f.Add(uint16(0), uint8(0), uint8(20), uint8(60), false, uint8(0), uint8(0), uint8(0), uint8(0))
	f.Add(uint16(15), uint8(0), uint8(45), uint8(70), false, uint8(0), uint8(0), uint8(20), uint8(40))
	f.Add(uint16(40), uint8(14), uint8(5), uint8(90), true, uint8(2), uint8(80), uint8(0), uint8(0))
	f.Add(uint16(200), uint8(0), uint8(100), uint8(1), true, uint8(0), uint8(1), uint8(90), uint8(60))
	f.Fuzz(func(t *testing.T, dayOffset uint16, trial, suspendDays, suspendedDays uint8, again bool, gapDays, againDays,
		downFrom, downDays uint8) {
		start := local(2026, time.January, 31, 9, 0).AddDate(0, 0, int(dayOffset%366))
		s := mustNew(t, newInput(start, int(trial%31), start))
		sched := schedule(t, s)
		day := 24 * time.Hour
		type window struct{ from, to time.Time }
		windows := []window{{start.Add(time.Duration(suspendDays%120)*day + time.Hour), time.Time{}}}
		windows[0].to = windows[0].from.Add(time.Duration(1+int(suspendedDays%150)) * day)
		if again {
			from := windows[0].to.Add(time.Duration(gapDays%40)*day + time.Hour)
			windows = append(windows, window{from, from.Add(time.Duration(1+int(againDays%100)) * day)})
		}
		down := window{start.Add(time.Duration(downFrom) * day), start.Add(time.Duration(downFrom)*day + time.Duration(downDays%90)*day)}
		end := windows[len(windows)-1].to.Add(40 * day)
		if down.to.After(end) {
			end = down.to.Add(40 * day)
		}

		// Linha do tempo: suspensões e reativações e execuções do motor no
		// início de cada período, logo após cada reativação, no fim da
		// indisponibilidade e no fim; o motor não roda durante a
		// indisponibilidade.
		type action struct {
			at   time.Time
			kind int // 0 suspender, 1 reativar, 2 motor
		}
		var actions []action
		for _, w := range windows {
			actions = append(actions, action{w.from, 0}, action{w.to, 1}, action{w.to, 2})
		}
		for i := 0; !sched.Period(i).Start.After(end); i++ {
			actions = append(actions, action{sched.Period(i).Start, 2})
		}
		actions = append(actions, action{down.to, 2}, action{end, 2})
		slices.SortStableFunc(actions, func(a, b action) int {
			if c := a.at.Compare(b.at); c != 0 {
				return c
			}
			return a.kind - b.kind
		})
		closed := map[int]int{}
		for _, a := range actions {
			switch a.kind {
			case 0:
				if err := s.Suspend("", a.at); err != nil {
					t.Fatal(err)
				}
			case 1:
				if err := s.Reactivate(a.at); err != nil {
					t.Fatal(err)
				}
			case 2:
				if !a.at.Before(down.from) && a.at.Before(down.to) {
					continue
				}
				for range 400 {
					step, ok, err := s.NextStep(a.at)
					if err != nil {
						t.Fatal(err)
					}
					if !ok {
						break
					}
					for _, p := range step.UsagePeriods() {
						idx := sched.IndexOf(p.Start)
						if p != sched.Period(idx) {
							t.Fatalf("período de consumo fora do calendário: %+v", p)
						}
						closed[idx]++
					}
					if err := s.ApplyStep(step, a.at); err != nil {
						t.Fatal(err)
					}
				}
			}
		}

		active := func(p calendar.Period) bool {
			from := p.Start
			if s.StartedAt.After(from) {
				from = s.StartedAt
			}
			// Tempo faturável: [from, fim) menos as janelas de suspensão.
			for cursor := from; cursor.Before(p.End); {
				inside := false
				for _, w := range windows {
					if !cursor.Before(w.from) && cursor.Before(w.to) {
						cursor, inside = w.to, true
					}
				}
				if !inside {
					return true
				}
			}
			return false
		}
		last := sched.IndexOf(end)
		for idx, n := range closed {
			if n != 1 {
				t.Fatalf("período %d fechado %d vezes", idx, n)
			}
		}
		for idx := 0; idx < last; idx++ {
			if active(sched.Period(idx)) && closed[idx] == 0 {
				t.Fatalf("período %d com tempo faturável não foi fechado (fechados: %v, suspensões: %+v, motor indisponível: %+v)",
					idx, closed, windows, down)
			}
		}
	})
}
