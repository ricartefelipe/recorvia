package platformbilling

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

func ptr[T any](v T) *T { return &v }

func saoPaulo(t testing.TB) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

func utc(y int, m time.Month, d, h, minute int) time.Time {
	return time.Date(y, m, d, h, minute, 0, 0, time.UTC)
}

func assertAppErr(t testing.TB, err error, kind apperr.Kind, code string) {
	t.Helper()
	e, ok := apperr.As(err)
	if !ok {
		t.Fatalf("esperado *apperr.Error %s/%s, obtido %v", kind, code, err)
	}
	if e.Kind != kind || e.Code != code {
		t.Fatalf("erro = %s/%s, esperado %s/%s", e.Kind, e.Code, kind, code)
	}
}

func essencial() Plan {
	return Plan{
		Code: "essencial", Name: "Essencial", MonthlyPrice: money.New(19900, money.BRL),
		MaxUsers: ptr(3), MaxActiveSubscriptions: ptr(100),
		Features:  []Feature{FeatureRecurringBilling, FeatureEmailDunning},
		TrialDays: 14, IsDefault: true, Active: true,
	}
}

func escala() Plan {
	return Plan{
		Code: "escala", Name: "Escala", MonthlyPrice: money.New(129900, money.BRL),
		Features: []Feature{FeatureRecurringBilling, FeatureEmailDunning, FeatureUsageBilling, FeatureOutboundWebhooks,
			FeatureAPIAccess, FeatureAsaasIntegration, FeatureReconciliationRun},
		TrialDays: 14, Active: true,
	}
}

func TestCheckUserLimit(t *testing.T) {
	cases := []struct {
		name    string
		max     *int
		active  int
		blocked bool
	}{
		{"sem limite aceita qualquer quantidade", nil, 10_000, false},
		{"abaixo do limite", ptr(3), 1, false},
		{"último lugar disponível", ptr(3), 2, false},
		{"limite atingido", ptr(3), 3, true},
		{"acima do limite", ptr(3), 7, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckUserLimit(Plan{MaxUsers: tc.max}, Usage{ActiveUsers: tc.active})
			if !tc.blocked {
				if err != nil {
					t.Fatalf("esperado sucesso, obtido %v", err)
				}
				return
			}
			assertAppErr(t, err, apperr.KindLimitExceeded, "platform_user_limit")
		})
	}
}

func TestCheckSubscriptionLimit(t *testing.T) {
	cases := []struct {
		name    string
		max     *int
		active  int
		blocked bool
	}{
		{"sem limite", nil, 1_000_000, false},
		{"abaixo do limite", ptr(100), 98, false},
		{"último contrato disponível", ptr(100), 99, false},
		{"limite atingido", ptr(100), 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// O limite de usuários não interfere no de contratos.
			err := CheckSubscriptionLimit(Plan{MaxActiveSubscriptions: tc.max, MaxUsers: ptr(1)},
				Usage{ActiveSubscriptions: tc.active, ActiveUsers: 50})
			if !tc.blocked {
				if err != nil {
					t.Fatalf("esperado sucesso, obtido %v", err)
				}
				return
			}
			assertAppErr(t, err, apperr.KindLimitExceeded, "platform_subscription_limit")
		})
	}
}

func TestFeaturesByPlan(t *testing.T) {
	basic, full := essencial(), escala()
	cases := []struct {
		plan    Plan
		feature Feature
		want    bool
	}{
		{basic, FeatureRecurringBilling, true},
		{basic, FeatureEmailDunning, true},
		{basic, FeatureUsageBilling, false},
		{basic, FeatureAPIAccess, false},
		{basic, FeatureReconciliationRun, false},
		{full, FeatureUsageBilling, true},
		{full, FeatureReconciliationRun, true},
		{Plan{}, FeatureRecurringBilling, false},
	}
	for _, tc := range cases {
		t.Run(tc.plan.Code+"/"+string(tc.feature), func(t *testing.T) {
			if got := tc.plan.HasFeature(tc.feature); got != tc.want {
				t.Fatalf("HasFeature = %v, esperado %v", got, tc.want)
			}
			err := RequireFeature(tc.plan, tc.feature)
			if tc.want {
				if err != nil {
					t.Fatalf("RequireFeature deveria liberar: %v", err)
				}
				return
			}
			assertAppErr(t, err, apperr.KindForbidden, "platform_feature_unavailable")
		})
	}
}

func TestCanChangeTo(t *testing.T) {
	inactive := escala()
	inactive.Active = false
	cases := []struct {
		name   string
		target Plan
		usage  Usage
		kind   apperr.Kind
		code   string
	}{
		{name: "upgrade para plano ilimitado", target: escala(), usage: Usage{ActiveUsers: 50, ActiveSubscriptions: 5000}},
		{name: "downgrade dentro dos limites", target: essencial(), usage: Usage{ActiveUsers: 3, ActiveSubscriptions: 100}},
		{name: "plano inativo", target: inactive, usage: Usage{}, kind: apperr.KindInvalid, code: "platform_plan_inactive"},
		{name: "usuários acima do limite", target: essencial(), usage: Usage{ActiveUsers: 4}, kind: apperr.KindConflict, code: "platform_downgrade_users"},
		{name: "contratos acima do limite", target: essencial(), usage: Usage{ActiveUsers: 1, ActiveSubscriptions: 101},
			kind: apperr.KindConflict, code: "platform_downgrade_subscriptions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CanChangeTo(tc.target, tc.usage)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("esperado sucesso, obtido %v", err)
				}
				return
			}
			assertAppErr(t, err, tc.kind, tc.code)
		})
	}
}

func TestNewSubscription(t *testing.T) {
	sp := saoPaulo(t)
	tenantID := uuid.New()
	noTrial := essencial()
	noTrial.TrialDays = 0

	cases := []struct {
		name       string
		plan       Plan
		now        time.Time
		wantStatus Status
		wantStart  time.Time
		wantEnd    time.Time
		wantTrial  *time.Time
	}{
		{
			// O primeiro período é a própria avaliação; os ciclos seguintes
			// ficam ancorados no dia local de fim da avaliação.
			name: "com avaliação termina no início do dia local", plan: essencial(), now: utc(2026, time.November, 16, 15, 0),
			wantStatus: StatusTrialing,
			wantStart:  utc(2026, time.November, 16, 3, 0), wantEnd: utc(2026, time.November, 30, 3, 0),
			wantTrial: ptr(utc(2026, time.November, 30, 3, 0)),
		},
		{
			// 01h30 UTC do dia 16 ainda é dia 15 em São Paulo.
			name: "data local anterior à data UTC", plan: essencial(), now: utc(2026, time.November, 16, 1, 30),
			wantStatus: StatusTrialing,
			wantStart:  utc(2026, time.November, 15, 3, 0), wantEnd: utc(2026, time.November, 29, 3, 0),
			wantTrial: ptr(utc(2026, time.November, 29, 3, 0)),
		},
		{
			name: "sem avaliação inicia ativa", plan: noTrial, now: utc(2026, time.November, 16, 15, 0),
			wantStatus: StatusActive,
			wantStart:  utc(2026, time.November, 16, 3, 0), wantEnd: utc(2026, time.December, 16, 3, 0),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewSubscription(tenantID, tc.plan, tc.now, sp)
			if err != nil {
				t.Fatal(err)
			}
			if s.TenantID != tenantID || s.PlanCode != tc.plan.Code || s.Version != 1 || s.Status != tc.wantStatus {
				t.Fatalf("assinatura inesperada: %+v", s)
			}
			if !s.PeriodStart.Equal(tc.wantStart) || !s.PeriodEnd.Equal(tc.wantEnd) {
				t.Errorf("período [%v, %v), esperado [%v, %v)", s.PeriodStart, s.PeriodEnd, tc.wantStart, tc.wantEnd)
			}
			switch {
			case tc.wantTrial == nil && s.TrialEndsAt != nil:
				t.Errorf("não deveria haver avaliação: %v", *s.TrialEndsAt)
			case tc.wantTrial != nil && (s.TrialEndsAt == nil || !s.TrialEndsAt.Equal(*tc.wantTrial)):
				t.Errorf("fim da avaliação = %v, esperado %v", s.TrialEndsAt, *tc.wantTrial)
			}
		})
	}
}

func TestNewSubscriptionRequiresLocation(t *testing.T) {
	_, err := NewSubscription(uuid.New(), essencial(), time.Now(), nil)
	if !errors.Is(err, calendar.ErrNilLocation) {
		t.Fatalf("esperado ErrNilLocation, obtido %v", err)
	}
}

func TestCloseStatement(t *testing.T) {
	start, end := utc(2026, time.November, 16, 3, 0), utc(2026, time.December, 16, 3, 0)
	now := utc(2026, time.December, 16, 4, 0)
	plan := essencial()
	base := Subscription{TenantID: uuid.New(), PlanCode: plan.Code, Anchor: calendar.DateOf(start, saoPaulo(t)), PeriodStart: start, PeriodEnd: end}

	cases := []struct {
		name     string
		trialEnd *time.Time
		addons   int64
		wantSub  int64
		wantTot  int64
	}{
		{name: "sem avaliação cobra o plano", wantSub: 19900, wantTot: 19900},
		{name: "período inteiro dentro da avaliação não cobra o plano", trialEnd: ptr(end.AddDate(0, 0, 10)), wantSub: 0, wantTot: 0},
		{name: "avaliação terminando exatamente no fim do período", trialEnd: ptr(end), wantSub: 0, wantTot: 0},
		{name: "avaliação terminando antes do fim do período cobra o plano", trialEnd: ptr(end.Add(-time.Nanosecond)), wantSub: 19900, wantTot: 19900},
		{name: "adicionais são somados", addons: 250000, wantSub: 19900, wantTot: 269900},
		{name: "adicionais cobrados mesmo durante a avaliação", trialEnd: ptr(end), addons: 250000, wantSub: 0, wantTot: 250000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			s.TrialEndsAt = tc.trialEnd
			id := uuid.New()
			st, err := CloseStatement(id, s, plan, money.New(tc.addons, money.BRL), now)
			if err != nil {
				t.Fatal(err)
			}
			if st.SubscriptionAmount.Amount() != tc.wantSub || st.AddonsAmount.Amount() != tc.addons || st.Total.Amount() != tc.wantTot {
				t.Fatalf("plano=%d adicionais=%d total=%d, esperado %d/%d/%d",
					st.SubscriptionAmount.Amount(), st.AddonsAmount.Amount(), st.Total.Amount(), tc.wantSub, tc.addons, tc.wantTot)
			}
			if st.SubscriptionAmount.Currency() != money.BRL || st.Total.Currency() != money.BRL {
				t.Errorf("moeda inesperada: %v", st.Total)
			}
			if st.ID != id || st.TenantID != s.TenantID || st.PlanCode != "essencial" || st.Status != "open" ||
				!st.PeriodStart.Equal(start) || !st.PeriodEnd.Equal(end) || !st.CreatedAt.Equal(now) {
				t.Errorf("demonstrativo inesperado: %+v", st)
			}
		})
	}
}

func TestCloseStatementCurrencyMismatch(t *testing.T) {
	s := Subscription{Anchor: calendar.Date{Year: 2026, Month: time.November, Day: 1}, PeriodStart: utc(2026, time.November, 1, 3, 0), PeriodEnd: utc(2026, time.December, 1, 3, 0)}
	_, err := CloseStatement(uuid.New(), s, essencial(), money.New(100, money.Currency{}), time.Now())
	if !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Fatalf("esperado ErrCurrencyMismatch, obtido %v", err)
	}
}

func TestAdvanceMonthByMonth(t *testing.T) {
	sp := saoPaulo(t)
	s, err := NewSubscription(uuid.New(), essencial(), utc(2026, time.November, 16, 15, 0), sp)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		start, end time.Time
		status     Status
	}{
		// Avaliação de 14 dias [16/11, 30/11); ciclos ancorados no dia 30
		// (fevereiro usa o último dia do mês e março volta ao dia 30).
		{utc(2026, time.November, 30, 3, 0), utc(2026, time.December, 30, 3, 0), StatusActive},
		{utc(2026, time.December, 30, 3, 0), utc(2027, time.January, 30, 3, 0), StatusActive},
		{utc(2027, time.January, 30, 3, 0), utc(2027, time.February, 28, 3, 0), StatusActive},
		{utc(2027, time.February, 28, 3, 0), utc(2027, time.March, 30, 3, 0), StatusActive},
	}
	for i, w := range want {
		prevEnd := s.PeriodEnd
		if err := s.Advance(sp); err != nil {
			t.Fatal(err)
		}
		if !s.PeriodStart.Equal(prevEnd) {
			t.Errorf("passo %d: períodos devem ser contíguos (%v != %v)", i, s.PeriodStart, prevEnd)
		}
		if !s.PeriodStart.Equal(w.start) || !s.PeriodEnd.Equal(w.end) || s.Status != w.status {
			t.Errorf("passo %d: [%v, %v) %s, esperado [%v, %v) %s", i, s.PeriodStart, s.PeriodEnd, s.Status, w.start, w.end, w.status)
		}
	}
}

func TestAdvanceEndsTrial(t *testing.T) {
	sp := saoPaulo(t)
	start, end := utc(2026, time.November, 16, 3, 0), utc(2026, time.December, 16, 3, 0)
	cases := []struct {
		name     string
		trialEnd time.Time
		want     Status
	}{
		{"avaliação já vencida no novo período", utc(2026, time.November, 30, 3, 0), StatusActive},
		{"avaliação termina no início do novo período", end, StatusActive},
		{"avaliação continua no novo período", utc(2026, time.December, 20, 3, 0), StatusTrialing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Subscription{Status: StatusTrialing, TrialEndsAt: ptr(tc.trialEnd), Anchor: calendar.DateOf(start, sp), PeriodStart: start, PeriodEnd: end}
			if err := s.Advance(sp); err != nil {
				t.Fatal(err)
			}
			if s.Status != tc.want {
				t.Fatalf("situação = %s, esperado %s", s.Status, tc.want)
			}
		})
	}
	t.Run("situação diferente de avaliação não é alterada", func(t *testing.T) {
		s := Subscription{Status: StatusPastDue, TrialEndsAt: ptr(start), Anchor: calendar.DateOf(start, sp), PeriodStart: start, PeriodEnd: end}
		if err := s.Advance(sp); err != nil {
			t.Fatal(err)
		}
		if s.Status != StatusPastDue {
			t.Fatalf("situação = %s, esperado past_due", s.Status)
		}
	})
}

func TestAdvanceKeepsAnchorAtEndOfMonth(t *testing.T) {
	sp := saoPaulo(t)
	s, err := NewSubscription(uuid.New(), Plan{Code: "essencial"}, utc(2027, time.January, 31, 15, 0), sp)
	if err != nil {
		t.Fatal(err)
	}
	wantStarts := []time.Time{
		utc(2027, time.February, 28, 3, 0),
		utc(2027, time.March, 31, 3, 0),
		utc(2027, time.April, 30, 3, 0),
		utc(2027, time.May, 31, 3, 0),
	}
	for i, want := range wantStarts {
		prevEnd := s.PeriodEnd
		if err := s.Advance(sp); err != nil {
			t.Fatal(err)
		}
		if !s.PeriodStart.Equal(prevEnd) {
			t.Errorf("passo %d: início %v deveria ser o fim anterior %v", i, s.PeriodStart, prevEnd)
		}
		if !s.PeriodStart.Equal(want) {
			t.Errorf("passo %d: início %v, esperado %v", i, s.PeriodStart, want)
		}
	}
}

func TestAdvanceRequiresLocation(t *testing.T) {
	s := Subscription{Anchor: calendar.Date{Year: 2026, Month: time.November, Day: 16}, PeriodStart: utc(2026, time.November, 16, 3, 0), PeriodEnd: utc(2026, time.December, 16, 3, 0)}
	before := s
	if err := s.Advance(nil); !errors.Is(err, calendar.ErrNilLocation) {
		t.Fatalf("esperado ErrNilLocation, obtido %v", err)
	}
	if s != before {
		t.Fatalf("assinatura não deveria mudar em caso de erro: %+v", s)
	}
}
