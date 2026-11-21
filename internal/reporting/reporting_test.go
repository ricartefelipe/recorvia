package reporting

import (
	"context"
	"errors"
	"math"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

func saoPaulo(t testing.TB) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

func TestComputeMRR(t *testing.T) {
	cases := []struct {
		name    string
		mrr     []MRRComponent
		wantMRR int64
		wantARR int64
	}{
		{
			name:    "sem assinaturas",
			wantMRR: 0, wantARR: 0,
		},
		{
			name:    "valor fixo mensal",
			mrr:     []MRRComponent{{Interval: calendar.Monthly, Model: "flat", FlatTotal: 9990}},
			wantMRR: 9990, wantARR: 119880,
		},
		{
			name:    "fixo com excedente conta apenas o fixo",
			mrr:     []MRRComponent{{Interval: calendar.Monthly, Model: "flat_plus_overage", FlatTotal: 5000}},
			wantMRR: 5000, wantARR: 60000,
		},
		{
			name: "por unidade usa o total racional",
			mrr: []MRRComponent{{Interval: calendar.Monthly, Model: "per_unit",
				PerUnitTotal: big.NewRat(29943, 2)}}, // 14971,5 centavos
			wantMRR: 14972, wantARR: 179658,
		},
		{
			name:    "por unidade sem total é zero",
			mrr:     []MRRComponent{{Interval: calendar.Monthly, Model: "per_unit"}},
			wantMRR: 0, wantARR: 0,
		},
		{
			name:    "anual entra dividido por 12",
			mrr:     []MRRComponent{{Interval: calendar.Yearly, Model: "flat", FlatTotal: 120000}},
			wantMRR: 10000, wantARR: 120000,
		},
		{
			name: "consumo medido fica fora do MRR",
			mrr: []MRRComponent{
				{Interval: calendar.Monthly, Model: "usage", FlatTotal: 100000, PerUnitTotal: big.NewRat(500, 1)},
				{Interval: calendar.Monthly, Model: "flat", FlatTotal: 1000},
			},
			wantMRR: 1000, wantARR: 12000,
		},
		{
			// 1001/12 = 83,4166...: o MRR arredonda para 83, mas o ARR é
			// calculado sobre o valor exato (1001), não sobre 83 × 12 = 996.
			name:    "ARR arredonda uma única vez",
			mrr:     []MRRComponent{{Interval: calendar.Yearly, Model: "flat", FlatTotal: 1001}},
			wantMRR: 83, wantARR: 1001,
		},
		{
			// Três anuais de 7 centavos: cada um vale 7/12 (0,58...);
			// arredondar por componente daria 3, mas a soma exata é 1,75 → 2.
			name: "soma exata antes do arredondamento",
			mrr: []MRRComponent{
				{Interval: calendar.Yearly, Model: "flat", FlatTotal: 7},
				{Interval: calendar.Yearly, Model: "flat", FlatTotal: 7},
				{Interval: calendar.Yearly, Model: "per_unit", PerUnitTotal: big.NewRat(7, 1)},
			},
			wantMRR: 2, wantARR: 21, // 21/12 = 1,75 → 2
		},
		{
			name: "combinação de periodicidades e modelos",
			mrr: []MRRComponent{
				{Interval: calendar.Monthly, Model: "flat", FlatTotal: 10000},
				{Interval: calendar.Monthly, Model: "per_unit", PerUnitTotal: big.NewRat(4500, 1)},
				{Interval: calendar.Yearly, Model: "per_unit", PerUnitTotal: big.NewRat(24000, 1)},
				{Interval: calendar.Monthly, Model: "usage", FlatTotal: 99999},
			},
			wantMRR: 16500, wantARR: 198000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Compute("2026-11", Raw{MRR: tc.mrr})
			if err != nil {
				t.Fatalf("Compute: %v", err)
			}
			if d.MRR != tc.wantMRR || d.ARR != tc.wantARR {
				t.Fatalf("MRR=%d ARR=%d, esperado MRR=%d ARR=%d", d.MRR, d.ARR, tc.wantMRR, tc.wantARR)
			}
		})
	}
}

func TestComputeRatiosInBasisPoints(t *testing.T) {
	cases := []struct {
		name            string
		open, base      int64
		ended, atStart  int
		wantDelinquency int64
		wantChurn       int64
	}{
		{name: "denominador zero resulta em zero", open: 500, base: 0, ended: 3, atStart: 0},
		{name: "numerador zero resulta em zero", open: 0, base: 1000, ended: 0, atStart: 10},
		{name: "valores negativos resultam em zero", open: -10, base: 1000, ended: 1, atStart: -5},
		{name: "proporção exata", open: 250, base: 1000, ended: 1, atStart: 8, wantDelinquency: 2500, wantChurn: 1250},
		{name: "dízima arredondada", open: 1, base: 3, ended: 2, atStart: 3, wantDelinquency: 3333, wantChurn: 6667},
		{name: "totalidade", open: 1000, base: 1000, ended: 4, atStart: 4, wantDelinquency: 10000, wantChurn: 10000},
		// 1/20000 = 0,5 bp → par (0); 3/20000 = 1,5 bp → par (2).
		{name: "empate vai para o par", open: 1, base: 20000, ended: 3, atStart: 20000, wantDelinquency: 0, wantChurn: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Compute("2026-11", Raw{
				DelinquencyOpen: tc.open, DelinquencyBase: tc.base,
				EndedInPeriod: tc.ended, ActiveAtStart: tc.atStart,
			})
			if err != nil {
				t.Fatalf("Compute: %v", err)
			}
			if d.DelinquencyRateBP != tc.wantDelinquency || d.ChurnRateBP != tc.wantChurn {
				t.Fatalf("inadimplência=%d churn=%d, esperado %d e %d",
					d.DelinquencyRateBP, d.ChurnRateBP, tc.wantDelinquency, tc.wantChurn)
			}
		})
	}
}

func TestComputeCopiesAggregates(t *testing.T) {
	raw := Raw{
		Billed: 100, BilledRecurring: 70, BilledUsage: 30, Received: 90, OpenReceivables: 40,
		OverdueReceivables: 15, ActiveNow: 12, CreatedInPeriod: 3, EndedInPeriod: 2, ActiveAtStart: 11,
		Refunds: 5, Chargebacks: 1, OpenRecoverables: 7, PendingProviderReceipt: 8, UnknownOperations: 2, OpenDiscrepancies: 4,
	}
	d, err := Compute("2026-10", raw)
	if err != nil {
		t.Fatal(err)
	}
	want := Dashboard{
		Month: "2026-10", Currency: "BRL", Billed: 100, BilledRecurring: 70, BilledUsage: 30, Received: 90,
		OpenReceivables: 40, OverdueReceivables: 15, ChurnRateBP: 1818, ActiveSubscriptions: 12,
		NewSubscriptions: 3, CanceledSubscriptions: 2, Refunds: 5, Chargebacks: 1, OpenRecoverables: 7, PendingProviderReceipt: 8,
		OpenDiscrepancies: 4, UnknownOperations: 2,
	}
	d.Definitions = nil
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("dashboard = %+v\nesperado   %+v", d, want)
	}
}

func TestComputeExposesDefinitions(t *testing.T) {
	d, err := Compute("2026-11", Raw{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"mrr", "arr", "billed", "received", "open", "overdue", "delinquency", "churn", "refunds", "chargebacks", "recoverables", "pending_receipt"} {
		if d.Definitions[k] == "" {
			t.Errorf("definição %q ausente", k)
		}
	}
}

func TestComputeOverflow(t *testing.T) {
	t.Run("MRR acima de int64", func(t *testing.T) {
		_, err := Compute("2026-11", Raw{MRR: []MRRComponent{
			{Interval: calendar.Monthly, Model: "flat", FlatTotal: math.MaxInt64},
			{Interval: calendar.Monthly, Model: "flat", FlatTotal: math.MaxInt64},
		}})
		if err == nil {
			t.Fatal("esperado erro de overflow")
		}
	})
	t.Run("ARR acima de int64", func(t *testing.T) {
		_, err := Compute("2026-11", Raw{MRR: []MRRComponent{
			{Interval: calendar.Monthly, Model: "flat", FlatTotal: math.MaxInt64 / 2},
		}})
		if err == nil {
			t.Fatal("esperado erro de overflow no ARR")
		}
	})
}

func TestMonthRange(t *testing.T) {
	sp := saoPaulo(t)
	now := time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		month     string
		now       time.Time
		wantMonth string
		wantStart time.Time
		wantEnd   time.Time
	}{
		{
			name: "mês informado", month: "2026-03", now: now, wantMonth: "2026-03",
			wantStart: time.Date(2026, time.March, 1, 3, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, time.April, 1, 3, 0, 0, 0, time.UTC),
		},
		{
			name: "dezembro termina em janeiro do ano seguinte", month: "2026-12", now: now, wantMonth: "2026-12",
			wantStart: time.Date(2026, time.December, 1, 3, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2027, time.January, 1, 3, 0, 0, 0, time.UTC),
		},
		{
			name: "mês corrente no fuso do tenant", now: now, wantMonth: "2026-11",
			wantStart: time.Date(2026, time.November, 1, 3, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, time.December, 1, 3, 0, 0, 0, time.UTC),
		},
		{
			// 01/12 às 02h UTC ainda é 30/11 em São Paulo.
			name: "virada do mês em UTC ainda é o mês anterior no fuso", now: time.Date(2026, time.December, 1, 2, 0, 0, 0, time.UTC),
			wantMonth: "2026-11",
			wantStart: time.Date(2026, time.November, 1, 3, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, time.December, 1, 3, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, start, end, err := MonthRange(tc.month, sp, tc.now)
			if err != nil {
				t.Fatalf("MonthRange: %v", err)
			}
			if m != tc.wantMonth || !start.Equal(tc.wantStart) || !end.Equal(tc.wantEnd) {
				t.Fatalf("obtido %s [%v, %v), esperado %s [%v, %v)", m, start, end, tc.wantMonth, tc.wantStart, tc.wantEnd)
			}
		})
	}
}

func TestMonthRangeInvalid(t *testing.T) {
	sp := saoPaulo(t)
	for _, month := range []string{"2026-13", "2026-00", "2026-1", "26-11", "2026/11", "novembro", "2026-11-01"} {
		t.Run(month, func(t *testing.T) {
			_, _, _, err := MonthRange(month, sp, time.Now())
			e, ok := apperr.As(err)
			if !ok || e.Kind != apperr.KindInvalid || e.Code != "invalid_month" {
				t.Fatalf("esperado invalid_month, obtido %v", err)
			}
			if len(e.Fields) != 1 || e.Fields[0].Field != "month" {
				t.Errorf("campo do erro inesperado: %+v", e.Fields)
			}
		})
	}
}

func TestMonthRangeWithoutLocation(t *testing.T) {
	_, _, _, err := MonthRange("2026-11", nil, time.Now())
	if !errors.Is(err, calendar.ErrNilLocation) {
		t.Fatalf("esperado ErrNilLocation, obtido %v", err)
	}
}

type fakeSource struct {
	loc      *time.Location
	locErr   error
	raw      Raw
	rawErr   error
	calls    int
	tenantID uuid.UUID
	from, to time.Time
	now      time.Time
}

func (f *fakeSource) Location(_ context.Context, tenantID uuid.UUID) (*time.Location, error) {
	f.tenantID = tenantID
	return f.loc, f.locErr
}

func (f *fakeSource) Raw(_ context.Context, tenantID uuid.UUID, from, to, now time.Time) (Raw, error) {
	f.calls++
	f.tenantID, f.from, f.to, f.now = tenantID, from, to, now
	return f.raw, f.rawErr
}

func TestServiceDashboard(t *testing.T) {
	sp := saoPaulo(t)
	now := time.Date(2026, time.November, 16, 15, 0, 0, 0, time.UTC)
	tenantID := uuid.New()

	t.Run("mês corrente com fórmulas aplicadas", func(t *testing.T) {
		src := &fakeSource{loc: sp, raw: Raw{
			MRR:             []MRRComponent{{Interval: calendar.Monthly, Model: "flat", FlatTotal: 5000}},
			DelinquencyOpen: 1, DelinquencyBase: 4,
		}}
		svc := NewService(src, clock.NewManual(now))
		d, err := svc.Dashboard(context.Background(), tenantID, "")
		if err != nil {
			t.Fatal(err)
		}
		if d.Month != "2026-11" || d.MRR != 5000 || d.ARR != 60000 || d.DelinquencyRateBP != 2500 {
			t.Fatalf("dashboard inesperado: %+v", d)
		}
		if src.tenantID != tenantID || !src.now.Equal(now) ||
			!src.from.Equal(time.Date(2026, time.November, 1, 3, 0, 0, 0, time.UTC)) ||
			!src.to.Equal(time.Date(2026, time.December, 1, 3, 0, 0, 0, time.UTC)) {
			t.Fatalf("consulta com parâmetros inesperados: %+v", src)
		}
	})

	t.Run("falha ao obter o fuso", func(t *testing.T) {
		boom := errors.New("banco indisponível")
		src := &fakeSource{locErr: boom}
		_, err := NewService(src, clock.NewManual(now)).Dashboard(context.Background(), tenantID, "2026-11")
		if !errors.Is(err, boom) || src.calls != 0 {
			t.Fatalf("esperado erro do fuso sem consultar agregações, err=%v calls=%d", err, src.calls)
		}
	})

	t.Run("mês inválido não consulta agregações", func(t *testing.T) {
		src := &fakeSource{loc: sp}
		_, err := NewService(src, clock.NewManual(now)).Dashboard(context.Background(), tenantID, "2026-99")
		if !apperr.IsKind(err, apperr.KindInvalid) || src.calls != 0 {
			t.Fatalf("esperado erro de validação, err=%v calls=%d", err, src.calls)
		}
	})

	t.Run("falha ao ler agregações", func(t *testing.T) {
		boom := errors.New("timeout")
		src := &fakeSource{loc: sp, rawErr: boom}
		_, err := NewService(src, clock.NewManual(now)).Dashboard(context.Background(), tenantID, "2026-10")
		if !errors.Is(err, boom) {
			t.Fatalf("esperado erro das agregações, obtido %v", err)
		}
	})
}
