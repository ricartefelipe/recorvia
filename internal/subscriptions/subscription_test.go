package subscriptions

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

var saoPaulo = func() *time.Location {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		panic(err)
	}
	return loc
}()

var allStatuses = []Status{Trialing, Active, PastDue, Suspended, Canceled}

// local cria um instante no fuso de São Paulo.
func local(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, saoPaulo)
}

func localDate(t time.Time) string { return calendar.DateOf(t, saoPaulo).String() }

func fixturePlan(interval calendar.Interval) catalog.Plan {
	priceID := uuid.New()
	return catalog.Plan{ID: uuid.New(), Code: "pro", Name: "Pro", Interval: interval, Currency: money.BRL,
		Status: catalog.Active, CurrentPriceVersionID: &priceID, Version: 1}
}

func fixturePrice(plan catalog.Plan, model catalog.PricingModel) catalog.PriceVersion {
	pv := catalog.PriceVersion{ID: *plan.CurrentPriceVersionID, PlanID: plan.ID, Version: 1, Model: model, Currency: money.BRL}
	flat := money.New(9900, money.BRL)
	switch model {
	case catalog.Flat, catalog.FlatPlusOverage:
		pv.FlatAmount = &flat
	}
	return pv
}

func newInput(start time.Time, trialDays int, now time.Time) NewInput {
	plan := fixturePlan(calendar.Monthly)
	return NewInput{ID: uuid.New(), TenantID: uuid.New(), CustomerID: uuid.New(), Plan: plan, Price: fixturePrice(plan, catalog.Flat),
		Quantity: 1, StartAt: start, TrialDays: trialDays, Location: saoPaulo, Now: now}
}

func mustNew(t *testing.T, in NewInput) Subscription {
	t.Helper()
	s, err := New(in)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// billOnce aplica o passo vencido em now e falha se não houver um.
func billOnce(t *testing.T, s *Subscription, now time.Time) BillingStep {
	t.Helper()
	step, ok, err := s.NextStep(now)
	if err != nil || !ok {
		t.Fatalf("NextStep(%v): ok=%v err=%v", now, ok, err)
	}
	if err := s.ApplyStep(step, now); err != nil {
		t.Fatalf("ApplyStep: %v", err)
	}
	return step
}

func schedule(t *testing.T, s Subscription) calendar.Schedule {
	t.Helper()
	sched, err := s.Schedule()
	if err != nil {
		t.Fatal(err)
	}
	return sched
}

func wantKind(t *testing.T, err error, kind apperr.Kind, code string) {
	t.Helper()
	if !apperr.IsKind(err, kind) {
		t.Fatalf("esperava erro %v, obtive %v", kind, err)
	}
	if e, _ := apperr.As(err); e.Code != code {
		t.Fatalf("esperava código %q, obtive %q", code, e.Code)
	}
}

func eventNames(ev []HistoryEvent) []string {
	out := make([]string, len(ev))
	for i, e := range ev {
		out[i] = e.Event
	}
	return out
}

func TestCanTransitionMatrix(t *testing.T) {
	allowed := map[Status][]Status{
		Trialing:  {Active, Suspended, Canceled},
		Active:    {PastDue, Suspended, Canceled},
		PastDue:   {Active, Suspended, Canceled},
		Suspended: {Active, Trialing, Canceled},
		Canceled:  {},
	}
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			want := false
			for _, a := range allowed[from] {
				if a == to {
					want = true
				}
			}
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s)=%v, esperado %v", from, to, got, want)
			}
		}
	}
	if CanTransition("desconhecido", Active) || CanTransition(Active, "desconhecido") {
		t.Fatal("situação desconhecida não pode transitar")
	}
}

func TestNewWithTrialAnchorsAtTrialEnd(t *testing.T) {
	start := local(2026, time.March, 10, 10, 0)
	s := mustNew(t, newInput(start, 14, start))

	trialEnd := local(2026, time.March, 24, 0, 0)
	if s.Status != Trialing {
		t.Fatalf("situação %s", s.Status)
	}
	if s.TrialEndsAt == nil || !s.TrialEndsAt.Equal(trialEnd) {
		t.Fatalf("fim do trial %v, esperado %v", s.TrialEndsAt, trialEnd)
	}
	if s.Anchor != (calendar.Date{Year: 2026, Month: time.March, Day: 24}) {
		t.Fatalf("âncora deve ser o fim do trial: %v", s.Anchor)
	}
	if !s.CurrentPeriod.Start.Equal(local(2026, time.March, 10, 0, 0)) || !s.CurrentPeriod.End.Equal(trialEnd) {
		t.Fatalf("período corrente deve ser o trial: %+v", s.CurrentPeriod)
	}
	if s.NextBillingAt == nil || !s.NextBillingAt.Equal(trialEnd) || s.NextBillingIndex != 0 {
		t.Fatalf("primeiro faturamento no fim do trial: %v idx=%d", s.NextBillingAt, s.NextBillingIndex)
	}
	if s.Version != 1 || s.Timezone != "America/Sao_Paulo" || s.StartedAt.Location() != time.UTC {
		t.Fatalf("metadados: versão=%d fuso=%s início=%v", s.Version, s.Timezone, s.StartedAt)
	}
	ev := s.PopEvents()
	if len(ev) != 1 || ev[0].Event != "created" || ev[0].From != nil || ev[0].To != Trialing || ev[0].Details["trial_days"] != 14 {
		t.Fatalf("evento de criação: %+v", ev)
	}
	if len(s.PopEvents()) != 0 {
		t.Fatal("PopEvents deve limpar os eventos pendentes")
	}
	if s.BillingDue(trialEnd.Add(-time.Nanosecond)) || !s.BillingDue(trialEnd) {
		t.Fatal("cobrança vence exatamente no fim do trial")
	}
}

func TestNewWithoutTrialStartsAtLocalMidnight(t *testing.T) {
	// 01:30 UTC de 11/03 ainda é 10/03 em São Paulo.
	start := time.Date(2026, time.March, 11, 1, 30, 0, 0, time.UTC)
	s := mustNew(t, newInput(start, 0, start))

	if s.Status != Active || s.TrialEndsAt != nil {
		t.Fatalf("sem trial a assinatura nasce ativa: %s %v", s.Status, s.TrialEndsAt)
	}
	if s.Anchor != (calendar.Date{Year: 2026, Month: time.March, Day: 10}) {
		t.Fatalf("âncora deve ser a data local de início: %v", s.Anchor)
	}
	wantStart, wantEnd := local(2026, time.March, 10, 0, 0), local(2026, time.April, 10, 0, 0)
	if !s.CurrentPeriod.Start.Equal(wantStart) || !s.CurrentPeriod.End.Equal(wantEnd) {
		t.Fatalf("período 0: %+v", s.CurrentPeriod)
	}
	if s.NextBillingAt == nil || !s.NextBillingAt.Equal(wantStart) {
		t.Fatalf("período 0 é cobrado já no início: %v", s.NextBillingAt)
	}
	if !s.BillingDue(start) {
		t.Fatal("o primeiro período deve estar vencido na criação")
	}
	if ev := s.PopEvents(); len(ev) != 1 || ev[0].To != Active {
		t.Fatalf("evento de criação: %+v", ev)
	}
}

func TestNewValidation(t *testing.T) {
	now := local(2026, time.November, 16, 12, 0)
	cases := []struct {
		name   string
		mutate func(*NewInput)
		field  string
	}{
		{"quantidade zero", func(in *NewInput) { in.Quantity = 0 }, "quantity"},
		{"quantidade acima do máximo", func(in *NewInput) {
			in.Price = fixturePrice(in.Plan, catalog.PerUnit)
			in.Quantity = MaxQuantity + 1
		}, "quantity"},
		{"quantidade diferente de 1 em plano fixo", func(in *NewInput) { in.Quantity = 2 }, "quantity"},
		{"quantidade diferente de 1 em plano por consumo", func(in *NewInput) {
			in.Price = fixturePrice(in.Plan, catalog.Usage)
			in.Quantity = 3
		}, "quantity"},
		{"trial negativo", func(in *NewInput) { in.TrialDays = -1 }, "trial_days"},
		{"trial acima de 365 dias", func(in *NewInput) { in.TrialDays = 366 }, "trial_days"},
		{"início no futuro", func(in *NewInput) { in.StartAt = now.Add(time.Nanosecond) }, "start_date"},
		{"retroativo acima de 366 dias", func(in *NewInput) { in.StartAt = now.Add(-MaxBackdate - time.Second) }, "start_date"},
		{"plano arquivado", func(in *NewInput) { in.Plan.Status = catalog.Archived }, "plan_id"},
		{"preço de outro plano", func(in *NewInput) { in.Price.PlanID = uuid.New() }, "plan_id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := newInput(now, 0, now)
			c.mutate(&in)
			_, err := New(in)
			wantKind(t, err, apperr.KindInvalid, "invalid_subscription")
			e, _ := apperr.As(err)
			found := false
			for _, f := range e.Fields {
				if f.Field == c.field {
					found = true
				}
			}
			if !found {
				t.Fatalf("campo %q ausente em %+v", c.field, e.Fields)
			}
		})
	}
}

func TestNewAcceptsBoundaries(t *testing.T) {
	now := local(2026, time.November, 16, 12, 0)
	t.Run("quantidade em plano por unidade", func(t *testing.T) {
		in := newInput(now, 0, now)
		in.Price = fixturePrice(in.Plan, catalog.PerUnit)
		in.Quantity = MaxQuantity
		if s := mustNew(t, in); s.Quantity != MaxQuantity {
			t.Fatalf("quantidade %d", s.Quantity)
		}
	})
	t.Run("retroativo de exatamente 366 dias", func(t *testing.T) {
		in := newInput(now.Add(-MaxBackdate), 0, now)
		mustNew(t, in)
	})
	t.Run("trial de 365 dias", func(t *testing.T) {
		in := newInput(now, 365, now)
		s := mustNew(t, in)
		if got := localDate(*s.TrialEndsAt); got != "2027-11-16" {
			t.Fatalf("fim do trial %s", got)
		}
	})
}

func TestNewRejectsUnknownInterval(t *testing.T) {
	now := local(2026, time.November, 16, 12, 0)
	in := newInput(now, 0, now)
	in.Plan.Interval = "week"
	if _, err := New(in); err == nil {
		t.Fatal("periodicidade desconhecida deve ser rejeitada")
	}
}

func TestStateTransitionsThroughMethods(t *testing.T) {
	at := local(2026, time.November, 16, 12, 0)
	type op struct {
		name string
		run  func(*Subscription) error
		ok   map[Status]Status // situação inicial -> situação final esperada
	}
	ops := []op{
		{"suspender", func(s *Subscription) error { return s.Suspend("inadimplência", at) },
			map[Status]Status{Trialing: Suspended, Active: Suspended, PastDue: Suspended}},
		{"cancelar imediatamente", func(s *Subscription) error { return s.Cancel(CancelImmediately, "", false, false, at) },
			map[Status]Status{Trialing: Canceled, Active: Canceled, PastDue: Canceled, Suspended: Canceled}},
		{"marcar inadimplente", func(s *Subscription) error { return s.MarkPastDue(at) },
			map[Status]Status{Active: PastDue, PastDue: PastDue}},
		{"reativar", func(s *Subscription) error { return s.Reactivate(at) },
			map[Status]Status{Suspended: Active}},
	}
	for _, o := range ops {
		for _, from := range allStatuses {
			t.Run(o.name+" a partir de "+string(from), func(t *testing.T) {
				s := mustNew(t, newInput(local(2026, time.October, 1, 9, 0), 0, at))
				s.Status = from
				s.PopEvents()
				err := o.run(&s)
				want, allowed := o.ok[from]
				if !allowed {
					if !apperr.IsKind(err, apperr.KindConflict) {
						t.Fatalf("transição inválida deve ser conflito, obtive %v", err)
					}
					if s.Status != from {
						t.Fatalf("situação alterada após transição inválida: %s", s.Status)
					}
					return
				}
				if err != nil {
					t.Fatalf("transição válida falhou: %v", err)
				}
				if s.Status != want {
					t.Fatalf("situação %s, esperada %s", s.Status, want)
				}
			})
		}
	}
}

func TestMarkPastDueAndCurrent(t *testing.T) {
	at := local(2026, time.November, 16, 12, 0)
	s := mustNew(t, newInput(at, 0, at))
	s.PopEvents()

	if err := s.MarkPastDue(at); err != nil || s.Status != PastDue {
		t.Fatalf("MarkPastDue: %v %s", err, s.Status)
	}
	if err := s.MarkPastDue(at); err != nil {
		t.Fatalf("MarkPastDue repetido deve ser idempotente: %v", err)
	}
	ev := s.PopEvents()
	if len(ev) != 1 || ev[0].Event != "past_due" || *ev[0].From != Active || ev[0].To != PastDue {
		t.Fatalf("somente uma transição registrada: %+v", ev)
	}
	if err := s.MarkCurrent(at); err != nil || s.Status != Active {
		t.Fatalf("MarkCurrent: %v %s", err, s.Status)
	}
	if ev := s.PopEvents(); len(ev) != 1 || ev[0].Event != "payment_recovered" {
		t.Fatalf("evento de recuperação: %+v", ev)
	}
	for _, st := range []Status{Active, Trialing, Suspended, Canceled} {
		s.Status = st
		if err := s.MarkCurrent(at); err != nil || s.Status != st {
			t.Fatalf("MarkCurrent em %s deve ser neutro: %v %s", st, err, s.Status)
		}
	}
	if len(s.PopEvents()) != 0 {
		t.Fatal("MarkCurrent neutro não registra eventos")
	}
}

func TestFirstBillingWithoutTrialChargesPeriodZero(t *testing.T) {
	start := local(2026, time.November, 16, 12, 0)
	s := mustNew(t, newInput(start, 0, start))
	sched := schedule(t, s)
	s.PopEvents()

	step := billOnce(t, &s, start)
	if step.Kind != StepBill || step.Index != 0 || step.Period != sched.Period(0) {
		t.Fatalf("passo inicial: %+v", step)
	}
	if step.UsagePeriod != nil {
		t.Fatal("o período 0 não tem consumo anterior a cobrar")
	}
	if s.NextBillingIndex != 1 || !s.NextBillingAt.Equal(sched.Period(1).Start) || s.CurrentPeriod != sched.Period(0) {
		t.Fatalf("após faturar: idx=%d next=%v atual=%+v", s.NextBillingIndex, s.NextBillingAt, s.CurrentPeriod)
	}
	if _, ok, _ := s.NextStep(start.Add(24 * time.Hour)); ok {
		t.Fatal("nada mais a faturar antes do período 1")
	}
	ev := s.PopEvents()
	if len(ev) != 1 || ev[0].Event != "renewed" || ev[0].Details["period_index"] != 0 {
		t.Fatalf("evento de renovação: %+v", ev)
	}

	// Período 1: fixo antecipado do período 1 e consumo do período 0.
	step = billOnce(t, &s, sched.Period(1).Start)
	if step.Index != 1 || step.Period != sched.Period(1) || step.UsagePeriod == nil || *step.UsagePeriod != sched.Period(0) {
		t.Fatalf("passo 1: %+v", step)
	}
}

func TestTrialBecomesActiveOnFirstBilling(t *testing.T) {
	start := local(2026, time.March, 10, 10, 0)
	s := mustNew(t, newInput(start, 14, start))
	s.PopEvents()
	trialEnd := *s.TrialEndsAt

	if _, ok, _ := s.NextStep(trialEnd.Add(-time.Second)); ok {
		t.Fatal("nada a faturar durante o trial")
	}
	step := billOnce(t, &s, trialEnd)
	if step.Kind != StepBill || step.Index != 0 || !step.Period.Start.Equal(trialEnd) || step.UsagePeriod != nil {
		t.Fatalf("primeiro passo após o trial: %+v", step)
	}
	if got := localDate(step.Period.End); got != "2026-04-24" {
		t.Fatalf("primeiro período após o trial termina em %s", got)
	}
	if s.Status != Active {
		t.Fatalf("trial deve virar ativa no primeiro faturamento: %s", s.Status)
	}
	ev := s.PopEvents()
	if got := eventNames(ev); len(got) != 2 || got[0] != "trial_ended" || got[1] != "renewed" {
		t.Fatalf("eventos: %v", got)
	}
	if *ev[0].From != Trialing || ev[0].To != Active {
		t.Fatalf("transição do trial: %+v", ev[0])
	}
}

func TestAnchorOn31stFollowsShortMonthsWithoutDrift(t *testing.T) {
	start := local(2026, time.January, 31, 10, 0)
	s := mustNew(t, newInput(start, 0, start))
	var got []string
	now := start
	for range 4 {
		step := billOnce(t, &s, now)
		got = append(got, localDate(step.Period.Start))
		now = *s.NextBillingAt
	}
	want := []string{"2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("datas de faturamento %v, esperadas %v", got, want)
		}
	}
	if s.Anchor.Day != 31 {
		t.Fatalf("a âncora não pode se deslocar: %v", s.Anchor)
	}
	if got := localDate(*s.NextBillingAt); got != "2026-05-31" {
		t.Fatalf("o ciclo volta ao dia 31 em maio: %s", got)
	}
}

func TestCatchUpProducesExactlyTheOverduePeriods(t *testing.T) {
	start := local(2026, time.January, 15, 9, 0)
	s := mustNew(t, newInput(start, 0, start))
	billOnce(t, &s, start)

	now := start.AddDate(0, 3, 0) // relógio três meses adiante
	var indexes []int
	for range 100 {
		step, ok, err := s.NextStep(now)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		indexes = append(indexes, step.Index)
		if err := s.ApplyStep(step, now); err != nil {
			t.Fatal(err)
		}
	}
	if len(indexes) != 3 || indexes[0] != 1 || indexes[1] != 2 || indexes[2] != 3 {
		t.Fatalf("esperava exatamente os períodos 1, 2 e 3, obtive %v", indexes)
	}
	if s.NextBillingIndex != 4 || localDate(*s.NextBillingAt) != "2026-05-15" {
		t.Fatalf("próximo faturamento: idx=%d em %s", s.NextBillingIndex, localDate(*s.NextBillingAt))
	}
	if localDate(s.CurrentPeriod.Start) != "2026-04-15" {
		t.Fatalf("período corrente: %+v", s.CurrentPeriod)
	}
}

func TestCancelAtPeriodEndEndsAtNextPeriodStart(t *testing.T) {
	start := local(2026, time.November, 16, 12, 0)
	s := mustNew(t, newInput(start, 0, start))
	sched := schedule(t, s)
	billOnce(t, &s, start)
	s.PopEvents()

	at := start.Add(72 * time.Hour)
	if err := s.Cancel(CancelAtPeriodEnd, "troca de fornecedor", false, false, at); err != nil {
		t.Fatal(err)
	}
	if !s.CancelAtPeriodEnd || s.CanceledAt == nil || !s.CanceledAt.Equal(at) || s.Status != Active || s.CancelReason != "troca de fornecedor" {
		t.Fatalf("agendamento do cancelamento: %+v", s)
	}
	ev := s.PopEvents()
	if len(ev) != 1 || ev[0].Event != "cancel_scheduled" || ev[0].Details["effective_at"] != sched.Period(0).End {
		t.Fatalf("evento de agendamento: %+v", ev)
	}
	wantKind(t, s.Cancel(CancelAtPeriodEnd, "", false, false, at), apperr.KindConflict, "already_scheduled")

	end := sched.Period(1).Start
	step, ok, err := s.NextStep(end)
	if err != nil || !ok {
		t.Fatalf("NextStep: ok=%v err=%v", ok, err)
	}
	if step.Kind != StepEnd || step.Index != 1 || step.UsagePeriod == nil || *step.UsagePeriod != sched.Period(0) {
		t.Fatalf("passo de encerramento: %+v", step)
	}
	if err := s.ApplyStep(step, end); err != nil {
		t.Fatal(err)
	}
	if s.Status != Canceled || s.EndedAt == nil || !s.EndedAt.Equal(end) || s.NextBillingAt != nil {
		t.Fatalf("encerramento: status=%s ended=%v next=%v", s.Status, s.EndedAt, s.NextBillingAt)
	}
	if s.CurrentPeriod != sched.Period(0) || s.NextBillingIndex != 1 {
		t.Fatalf("encerramento não abre novo período: %+v idx=%d", s.CurrentPeriod, s.NextBillingIndex)
	}
	if got := eventNames(s.PopEvents()); len(got) != 1 || got[0] != "ended_at_period_end" {
		t.Fatalf("eventos: %v", got)
	}
	if _, ok, _ := s.NextStep(end.AddDate(1, 0, 0)); ok {
		t.Fatal("assinatura encerrada não fatura mais")
	}
	wantKind(t, s.Cancel(CancelAtPeriodEnd, "", false, false, end), apperr.KindConflict, "invalid_transition")
}

func TestCancelAtTrialEndEndsWithoutBilling(t *testing.T) {
	start := local(2026, time.March, 10, 10, 0)
	s := mustNew(t, newInput(start, 14, start))
	if err := s.Cancel(CancelAtPeriodEnd, "", false, false, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	step, ok, err := s.NextStep(*s.TrialEndsAt)
	if err != nil || !ok || step.Kind != StepEnd || step.UsagePeriod != nil {
		t.Fatalf("fim do trial com cancelamento agendado: %+v ok=%v err=%v", step, ok, err)
	}
	if err := s.ApplyStep(step, *s.TrialEndsAt); err != nil {
		t.Fatal(err)
	}
	if s.Status != Canceled || !s.EndedAt.Equal(*s.TrialEndsAt) {
		t.Fatalf("encerramento no fim do trial: %s %v", s.Status, s.EndedAt)
	}
}

func TestResumeRenewal(t *testing.T) {
	at := local(2026, time.November, 16, 12, 0)
	s := mustNew(t, newInput(at, 0, at))
	wantKind(t, s.ResumeRenewal(at), apperr.KindConflict, "no_scheduled_cancel")

	if err := s.Cancel(CancelAtPeriodEnd, "orçamento", false, false, at); err != nil {
		t.Fatal(err)
	}
	s.PopEvents()
	if err := s.ResumeRenewal(at); err != nil {
		t.Fatal(err)
	}
	if s.CancelAtPeriodEnd || s.CanceledAt != nil || s.CancelReason != "" {
		t.Fatalf("cancelamento deveria ser desfeito: %+v", s)
	}
	if got := eventNames(s.PopEvents()); len(got) != 1 || got[0] != "cancel_unscheduled" {
		t.Fatalf("eventos: %v", got)
	}
	step, ok, _ := s.NextStep(*s.NextBillingAt)
	if !ok || step.Kind != StepBill {
		t.Fatalf("após retomar, o próximo passo volta a ser cobrança: %+v", step)
	}

	s.Status, s.CancelAtPeriodEnd = Canceled, true
	wantKind(t, s.ResumeRenewal(at), apperr.KindConflict, "no_scheduled_cancel")
}

func TestCancelImmediatelyWithUsageBillsFinalUsage(t *testing.T) {
	start := local(2026, time.November, 16, 12, 0)
	s := mustNew(t, newInput(start, 0, start))
	sched := schedule(t, s)
	billOnce(t, &s, start)
	if err := s.Cancel(CancelAtPeriodEnd, "", false, false, start); err != nil {
		t.Fatal(err)
	}
	s.PopEvents()

	at := start.Add(10 * 24 * time.Hour)
	if err := s.Cancel(CancelImmediately, "encerramento do contrato", true, false, at); err != nil {
		t.Fatal(err)
	}
	if s.Status != Canceled || !s.CanceledAt.Equal(at) || !s.EndedAt.Equal(at) || s.CancelAtPeriodEnd {
		t.Fatalf("cancelamento imediato: %+v", s)
	}
	if s.NextBillingAt == nil || !s.NextBillingAt.Equal(at) {
		t.Fatalf("o consumo final deve ser faturado no cancelamento: %v", s.NextBillingAt)
	}
	ev := s.PopEvents()
	if len(ev) != 1 || ev[0].Event != "canceled" || ev[0].Details["mode"] != CancelImmediately {
		t.Fatalf("evento: %+v", ev)
	}

	step, ok, err := s.NextStep(at)
	if err != nil || !ok || step.Kind != StepFinalUsage {
		t.Fatalf("passo final: %+v ok=%v err=%v", step, ok, err)
	}
	want := calendar.Period{Start: sched.Period(0).Start, End: at}
	if step.Period != want || step.UsagePeriod == nil || *step.UsagePeriod != want || step.Index != 1 {
		t.Fatalf("consumo final deve cobrir [início do período, cancelamento): %+v", step)
	}
	if err := s.ApplyStep(step, at); err != nil {
		t.Fatal(err)
	}
	if s.NextBillingAt != nil {
		t.Fatal("após o consumo final não há mais faturamento")
	}
	if got := eventNames(s.PopEvents()); len(got) != 1 || got[0] != "final_usage_billed" {
		t.Fatalf("eventos: %v", got)
	}
	if _, ok, _ := s.NextStep(at.AddDate(0, 2, 0)); ok {
		t.Fatal("nada mais a faturar")
	}
}

func TestCancelImmediatelyWithoutFinalUsage(t *testing.T) {
	start := local(2026, time.November, 16, 12, 0)
	cases := []struct {
		name     string
		hasUsage bool
		bill     bool
		at       func(Subscription) time.Time
	}{
		{"plano sem consumo", false, true, func(s Subscription) time.Time { return s.CurrentPeriod.Start.Add(5 * 24 * time.Hour) }},
		{"cancelado no primeiro instante do período", true, true, func(s Subscription) time.Time { return s.CurrentPeriod.Start }},
		{"nenhum período faturado ainda", true, false, func(s Subscription) time.Time { return start.Add(time.Hour) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustNew(t, newInput(start, 0, start))
			if c.bill {
				billOnce(t, &s, start)
			}
			at := c.at(s)
			if err := s.Cancel(CancelImmediately, "", c.hasUsage, false, at); err != nil {
				t.Fatal(err)
			}
			if s.NextBillingAt != nil {
				t.Fatalf("não deve haver fechamento final: %v", s.NextBillingAt)
			}
			if _, ok, _ := s.NextStep(at.AddDate(0, 1, 0)); ok {
				t.Fatal("nenhum passo esperado")
			}
		})
	}
}

func TestCancelImmediatelyWithPendingItemsSchedulesFinalStep(t *testing.T) {
	start := local(2026, time.November, 16, 12, 0)
	cases := []struct {
		name      string
		bill      bool
		at        func(Subscription) time.Time
		wantUsage bool
	}{
		{"plano sem consumo com período faturado", true, func(s Subscription) time.Time { return s.CurrentPeriod.Start.Add(5 * 24 * time.Hour) }, true},
		{"cancelado no primeiro instante do período", true, func(s Subscription) time.Time { return s.CurrentPeriod.Start }, false},
		{"nenhum período faturado ainda", false, func(Subscription) time.Time { return start.Add(time.Hour) }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustNew(t, newInput(start, 0, start))
			if c.bill {
				billOnce(t, &s, start)
			}
			at := c.at(s)
			if err := s.Cancel(CancelImmediately, "", false, true, at); err != nil {
				t.Fatal(err)
			}
			if s.NextBillingAt == nil || !s.NextBillingAt.Equal(at) {
				t.Fatalf("itens pendentes agendam o fechamento no encerramento: %v", s.NextBillingAt)
			}
			step, ok, err := s.NextStep(at)
			if err != nil || !ok || step.Kind != StepFinalUsage || (step.UsagePeriod != nil) != c.wantUsage {
				t.Fatalf("passo final: %+v ok=%v err=%v", step, ok, err)
			}
		})
	}
}

func TestNextStepForCanceledEdgeCases(t *testing.T) {
	at := local(2026, time.November, 16, 12, 0)
	t.Run("cancelada sem data de encerramento", func(t *testing.T) {
		s := mustNew(t, newInput(at, 0, at))
		s.Status = Canceled
		if _, ok, err := s.NextStep(at); ok || err != nil {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
	})
	t.Run("encerrada no início do período", func(t *testing.T) {
		s := mustNew(t, newInput(at, 0, at))
		s.Status = Canceled
		ended := s.CurrentPeriod.Start
		s.EndedAt = &ended
		step, ok, err := s.NextStep(at)
		if err != nil || !ok || step.Kind != StepFinalUsage || step.UsagePeriod != nil {
			t.Fatalf("período vazio não gera consumo a faturar: %+v ok=%v err=%v", step, ok, err)
		}
	})
}

func TestCancelValidation(t *testing.T) {
	at := local(2026, time.November, 16, 12, 0)
	s := mustNew(t, newInput(at, 0, at))
	wantKind(t, s.Cancel("amanha", "", false, false, at), apperr.KindInvalid, "invalid_cancel_mode")
	wantKind(t, s.Cancel(CancelImmediately, strings.Repeat("ç", 501), false, false, at), apperr.KindInvalid, "invalid_reason")
	if s.Status != Active {
		t.Fatal("validação não altera a assinatura")
	}
	// O limite é em caracteres, não em bytes.
	if err := s.Cancel(CancelImmediately, strings.Repeat("ç", 500), false, false, at); err != nil {
		t.Fatalf("500 caracteres devem ser aceitos: %v", err)
	}
	wantKind(t, s.Cancel(CancelImmediately, "", false, false, at), apperr.KindConflict, "invalid_transition")
}

func TestSuspend(t *testing.T) {
	at := local(2026, time.November, 16, 12, 0)
	s := mustNew(t, newInput(at, 0, at))
	wantKind(t, s.Suspend(strings.Repeat("a", 501), at), apperr.KindInvalid, "invalid_reason")
	s.PopEvents()

	if err := s.Suspend("", at); err != nil {
		t.Fatal(err)
	}
	if s.Status != Suspended || !s.SuspendedAt.Equal(at) || s.NextBillingAt != nil {
		t.Fatalf("suspensão: %+v", s)
	}
	if s.BillingDue(at.AddDate(1, 0, 0)) {
		t.Fatal("assinatura suspensa não fatura")
	}
	ev := s.PopEvents()
	if len(ev) != 1 || ev[0].Details["reason_code"] != "manual" {
		t.Fatalf("motivo vazio é suspensão manual: %+v", ev)
	}

	s2 := mustNew(t, newInput(at, 0, at))
	s2.PopEvents()
	if err := s2.Suspend("fatura vencida há 30 dias", at); err != nil {
		t.Fatal(err)
	}
	if ev := s2.PopEvents(); ev[0].Details["reason_code"] != "informed" || s2.SuspensionReason != "fatura vencida há 30 dias" {
		t.Fatalf("motivo informado: %+v", ev)
	}
}

func TestBillingDue(t *testing.T) {
	at := local(2026, time.November, 16, 12, 0)
	s := mustNew(t, newInput(at, 0, at))
	next := *s.NextBillingAt
	if !s.BillingDue(next) || s.BillingDue(next.Add(-time.Nanosecond)) {
		t.Fatal("vence exatamente em NextBillingAt")
	}
	s.NextBillingAt = nil
	if s.BillingDue(at) {
		t.Fatal("sem próximo faturamento nada vence")
	}
}

func TestReactivateSkipsWholeSuspendedPeriods(t *testing.T) {
	start := local(2026, time.January, 10, 9, 0)
	s := mustNew(t, newInput(start, 0, start))
	sched := schedule(t, s)
	billOnce(t, &s, start)
	billOnce(t, &s, sched.Period(1).Start) // períodos 0 e 1 faturados
	if err := s.Suspend("inadimplência", sched.Period(1).Start.Add(5*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.PopEvents()

	at := sched.Period(4).Start.Add(3 * 24 * time.Hour) // meados do período 4
	if err := s.Reactivate(at); err != nil {
		t.Fatal(err)
	}
	if s.Status != Active || s.SuspendedAt != nil || s.SuspensionReason != "" {
		t.Fatalf("reativação: %+v", s)
	}
	if s.NextBillingIndex != 4 || !s.NextBillingAt.Equal(sched.Period(4).Start) || s.CurrentPeriod != sched.Period(4) {
		t.Fatalf("deve retomar no período corrente: idx=%d next=%v atual=%+v", s.NextBillingIndex, s.NextBillingAt, s.CurrentPeriod)
	}
	ev := s.PopEvents()
	if len(ev) != 1 || ev[0].Event != "reactivated" || ev[0].Details["skipped_periods"] != 2 {
		t.Fatalf("períodos 2 e 3 devem ser pulados: %+v", ev)
	}
	step := billOnce(t, &s, at)
	if step.Index != 4 {
		t.Fatalf("o período corrente é faturado na reativação: %+v", step)
	}
	if _, ok, _ := s.NextStep(at); ok {
		t.Fatal("períodos suspensos não são faturados retroativamente")
	}
}

func TestReactivateWithinAlreadyBilledPeriod(t *testing.T) {
	start := local(2026, time.January, 10, 9, 0)
	s := mustNew(t, newInput(start, 0, start))
	sched := schedule(t, s)
	billOnce(t, &s, start)
	billOnce(t, &s, sched.Period(1).Start)
	if err := s.Suspend("", sched.Period(1).Start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.PopEvents()
	if err := s.Reactivate(sched.Period(1).Start.Add(48 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s.NextBillingIndex != 2 || !s.NextBillingAt.Equal(sched.Period(2).Start) || s.CurrentPeriod != sched.Period(1) {
		t.Fatalf("período já faturado não é cobrado de novo: idx=%d next=%v", s.NextBillingIndex, s.NextBillingAt)
	}
	if ev := s.PopEvents(); ev[0].Details["skipped_periods"] != 0 {
		t.Fatalf("nenhum período pulado: %+v", ev)
	}
}

func TestReactivateDuringTrialKeepsTrialEnd(t *testing.T) {
	start := local(2026, time.March, 10, 10, 0)
	s := mustNew(t, newInput(start, 14, start))
	trialEnd := *s.TrialEndsAt
	trialStart := s.CurrentPeriod.Start
	if err := s.Suspend("", start.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.PopEvents()
	if err := s.Reactivate(start.Add(5 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !s.NextBillingAt.Equal(trialEnd) || s.NextBillingIndex != 0 {
		t.Fatalf("primeiro faturamento continua no fim do trial: %v idx=%d", s.NextBillingAt, s.NextBillingIndex)
	}
	if !s.CurrentPeriod.Start.Equal(trialStart) || !s.CurrentPeriod.End.Equal(trialEnd) {
		t.Fatalf("período corrente continua sendo o trial: %+v", s.CurrentPeriod)
	}
	if s.Status != Trialing {
		t.Fatalf("reativação durante a avaliação volta a trialing, obtido %s", s.Status)
	}
	if ev := s.PopEvents(); ev[0].Details["skipped_periods"] != 0 {
		t.Fatalf("nada pulado: %+v", ev)
	}
	// O primeiro faturamento encerra a avaliação normalmente.
	step, ok, err := s.NextStep(trialEnd)
	if err != nil || !ok {
		t.Fatalf("passo no fim do trial: %v %v", ok, err)
	}
	if err := s.ApplyStep(step, trialEnd); err != nil {
		t.Fatal(err)
	}
	if ev := eventNames(s.PopEvents()); s.Status != Active || ev[0] != "trial_ended" {
		t.Fatalf("esperado trial_ended e active, obtido %s %v", s.Status, ev)
	}
}

func TestReactivateAfterTrialEndedDuringSuspension(t *testing.T) {
	start := local(2026, time.March, 10, 10, 0)
	s := mustNew(t, newInput(start, 14, start))
	sched := schedule(t, s)
	if err := s.Suspend("", start.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	at := sched.Period(2).Start.Add(24 * time.Hour)
	if err := s.Reactivate(at); err != nil {
		t.Fatal(err)
	}
	if s.NextBillingIndex != 2 || !s.NextBillingAt.Equal(sched.Period(2).Start) || s.CurrentPeriod != sched.Period(2) {
		t.Fatalf("retoma no período corrente após o trial: idx=%d next=%v", s.NextBillingIndex, s.NextBillingAt)
	}
}

func TestInvalidTimezoneIsReported(t *testing.T) {
	at := local(2026, time.November, 16, 12, 0)
	s := mustNew(t, newInput(at, 0, at))
	s.Timezone = "Marte/Olympus_Mons"
	if _, err := s.Location(); err == nil {
		t.Fatal("fuso inválido deve falhar")
	}
	if _, _, err := s.NextStep(at); err == nil {
		t.Fatal("NextStep deve propagar o fuso inválido")
	}
	if err := s.ApplyStep(BillingStep{Kind: StepBill}, at); err == nil {
		t.Fatal("ApplyStep deve propagar o fuso inválido")
	}
	s.Status = Suspended
	if err := s.Reactivate(at); err == nil || s.Status != Suspended {
		t.Fatalf("Reactivate deve propagar o fuso inválido: %v", err)
	}
}

// FuzzBillingStepsCoverEachPeriodOnce: para qualquer início, trial e
// atraso do relógio, o laço NextStep/ApplyStep fatura cada período vencido
// exatamente uma vez, em ordem, com períodos contíguos e a âncora preservada.
func FuzzBillingStepsCoverEachPeriodOnce(f *testing.F) {
	f.Add(uint16(0), uint8(0), uint8(0), uint8(3), false)
	f.Add(uint16(15), uint8(9), uint8(14), uint8(12), false)
	f.Add(uint16(30), uint8(23), uint8(0), uint8(24), true)
	f.Add(uint16(365), uint8(0), uint8(30), uint8(1), true)
	f.Fuzz(func(t *testing.T, dayOffset uint16, hour, trial, monthsAhead uint8, yearly bool) {
		start := local(2026, time.January, 31, 0, 0).AddDate(0, 0, int(dayOffset%366)).Add(time.Duration(hour%24) * time.Hour)
		in := newInput(start, int(trial%31), start)
		if yearly {
			in.Plan.Interval = calendar.Yearly
		}
		s := mustNew(t, in)
		sched := schedule(t, s)
		anchorDay := s.Anchor.Day
		now := start.AddDate(0, int(monthsAhead%37), 0)

		expected := sched.IndexOf(now) + 1 // períodos com início <= now
		var got []int
		for range 100 {
			step, ok, err := s.NextStep(now)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			if step.Kind != StepBill || step.Period != sched.Period(step.Index) {
				t.Fatalf("passo inesperado: %+v", step)
			}
			if step.Period.Start.After(now) {
				t.Fatalf("período futuro faturado: %v > %v", step.Period.Start, now)
			}
			if step.Index > 0 && (step.UsagePeriod == nil || step.UsagePeriod.End != step.Period.Start) {
				t.Fatalf("consumo deve ser do período imediatamente anterior: %+v", step)
			}
			d := calendar.DateOf(step.Period.Start, saoPaulo)
			if d.Day != min(anchorDay, calendar.DaysIn(d.Year, d.Month)) {
				t.Fatalf("início %v deslocado da âncora %d", d, anchorDay)
			}
			if err := s.ApplyStep(step, now); err != nil {
				t.Fatal(err)
			}
			got = append(got, step.Index)
		}
		if len(got) != expected {
			t.Fatalf("faturados %d períodos, esperados %d (%v)", len(got), expected, got)
		}
		for i, idx := range got {
			if idx != i {
				t.Fatalf("períodos fora de ordem ou repetidos: %v", got)
			}
		}
		if s.Anchor.Day != anchorDay {
			t.Fatal("âncora alterada")
		}
		if expected > 0 && (s.Status != Active || !s.NextBillingAt.After(now)) {
			t.Fatalf("após recuperar, a assinatura fica ativa e com próximo faturamento futuro: %s %v", s.Status, s.NextBillingAt)
		}
	})
}
