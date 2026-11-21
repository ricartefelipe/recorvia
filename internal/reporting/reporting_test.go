package reporting

import (
	"context"
	"errors"
	"math"
	"math/big"
	"reflect"
	"strings"
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

func rat(a, b int64) *big.Rat { return big.NewRat(a, b) }

var position = time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)

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
			name:    "componente fixo mensal",
			mrr:     []MRRComponent{{Interval: calendar.Monthly, Gross: rat(9990, 1)}},
			wantMRR: 9990, wantARR: 119880,
		},
		{
			name:    "componente fracionário arredonda uma vez",
			mrr:     []MRRComponent{{Interval: calendar.Monthly, Gross: rat(29943, 2)}}, // 14971,5 centavos
			wantMRR: 14972, wantARR: 179658,
		},
		{
			name:    "componente nulo é zero",
			mrr:     []MRRComponent{{Interval: calendar.Monthly}},
			wantMRR: 0, wantARR: 0,
		},
		{
			name:    "anual entra dividido por 12",
			mrr:     []MRRComponent{{Interval: calendar.Yearly, Gross: rat(120000, 1)}},
			wantMRR: 10000, wantARR: 120000,
		},
		{
			name:    "desconto recorrente reduz o MRR",
			mrr:     []MRRComponent{{Interval: calendar.Monthly, Gross: rat(10000, 1), Discount: rat(1500, 1)}},
			wantMRR: 8500, wantARR: 102000,
		},
		{
			// 120000 − 12000 de desconto anual = 108000 / 12 = 9000.
			name:    "desconto anual também é dividido por 12",
			mrr:     []MRRComponent{{Interval: calendar.Yearly, Gross: rat(120000, 1), Discount: rat(12000, 1)}},
			wantMRR: 9000, wantARR: 108000,
		},
		{
			name:    "desconto maior que o componente não deixa o MRR negativo",
			mrr:     []MRRComponent{{Interval: calendar.Monthly, Gross: rat(1000, 1), Discount: rat(5000, 1)}},
			wantMRR: 0, wantARR: 0,
		},
		{
			// 1001/12 = 83,4166...: o MRR arredonda para 83, mas o ARR é
			// calculado sobre o valor exato (1001), não sobre 83 × 12 = 996.
			name:    "ARR arredonda uma única vez",
			mrr:     []MRRComponent{{Interval: calendar.Yearly, Gross: rat(1001, 1)}},
			wantMRR: 83, wantARR: 1001,
		},
		{
			// Desconto percentual de 33,33% sobre 100: 66,67 exatos por
			// periodicidade; a soma (133,34) arredonda uma vez para 133.
			name: "soma exata antes do arredondamento",
			mrr: []MRRComponent{
				{Interval: calendar.Monthly, Gross: rat(100, 1), Discount: rat(3333, 100)},
				{Interval: calendar.Yearly, Gross: rat(800, 1), Discount: rat(3996, 10)}, // (800 − 399,6) / 12 = 33,3666...
			},
			wantMRR: 100, wantARR: 1200, // 66,67 + 33,3666... = 100,0366... → 100; × 12 = 1200,44 → 1200
		},
		{
			name: "combinação de periodicidades",
			mrr: []MRRComponent{
				{Interval: calendar.Monthly, Gross: rat(14500, 1), Discount: rat(500, 1)},
				{Interval: calendar.Yearly, Gross: rat(24000, 1)},
			},
			wantMRR: 16000, wantARR: 192000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Compute("2026-11", position, Raw{MRR: tc.mrr})
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
		name             string
		open, base       int64
		churned, atStart int
		wantDelinquency  int64
		wantChurn        int64
	}{
		{name: "denominador zero resulta em zero", open: 500, base: 0, churned: 3, atStart: 0},
		{name: "numerador zero resulta em zero", open: 0, base: 1000, churned: 0, atStart: 10},
		{name: "valores negativos resultam em zero", open: -10, base: 1000, churned: 1, atStart: -5},
		{name: "proporção exata", open: 250, base: 1000, churned: 1, atStart: 8, wantDelinquency: 2500, wantChurn: 1250},
		{name: "dízima arredondada", open: 1, base: 3, churned: 2, atStart: 3, wantDelinquency: 3333, wantChurn: 6667},
		{name: "totalidade", open: 1000, base: 1000, churned: 4, atStart: 4, wantDelinquency: 10000, wantChurn: 10000},
		// 1/20000 = 0,5 bp → par (0); 3/20000 = 1,5 bp → par (2).
		{name: "empate vai para o par", open: 1, base: 20000, churned: 3, atStart: 20000, wantDelinquency: 0, wantChurn: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Encerradas no mês sem estar vigentes no início (trial, criadas
			// no mês) contam em canceladas, nunca no churn.
			d, err := Compute("2026-11", position, Raw{
				DelinquencyOpen: tc.open, DelinquencyBase: tc.base,
				ChurnedInPeriod: tc.churned, EndedInPeriod: tc.churned + 7, ActiveAtStart: tc.atStart,
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
		OverdueReceivables: 15, ActiveAtPosition: 12, StartedInPeriod: 3, EndedInPeriod: 4, ActiveAtStart: 11, ChurnedInPeriod: 2,
		Refunds: 5, Chargebacks: 1, OpenRecoverables: 7, PendingProviderReceipt: 8, UnknownOperations: 2, OpenDiscrepancies: 4,
	}
	d, err := Compute("2026-10", position, raw)
	if err != nil {
		t.Fatal(err)
	}
	want := Dashboard{
		Month: "2026-10", Currency: "BRL", PositionAt: position, Billed: 100, BilledRecurring: 70, BilledUsage: 30, Received: 90,
		OpenReceivables: 40, OverdueReceivables: 15, ChurnRateBP: 1818, ActiveSubscriptions: 12,
		NewSubscriptions: 3, CanceledSubscriptions: 4, Refunds: 5, Chargebacks: 1, OpenRecoverables: 7, PendingProviderReceipt: 8,
		OpenDiscrepancies: 4,
		UnknownOperations: 2,
	}
	d.Definitions, d.Bases = nil, nil
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("dashboard = %+v\nesperado   %+v", d, want)
	}
}

func TestIndicatorsHaveUniqueKeysDefinitionsAndBases(t *testing.T) {
	d, err := Compute("2026-11", position, Raw{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Definitions) != len(Indicators) || len(d.Bases) != len(Indicators) {
		t.Fatalf("chaves repetidas: %d indicadores, %d definições, %d bases", len(Indicators), len(d.Definitions), len(d.Bases))
	}
	for _, i := range Indicators {
		if d.Definitions[i.Key] == "" || d.Bases[i.Key] == "" {
			t.Errorf("indicador %q sem definição ou base", i.Key)
		}
	}
	// A base declarada aparece no texto exibido ao usuário.
	cases := map[Basis]string{BasisCurrent: "Posição atual", BasisMonthEnd: "último instante do mês"}
	for _, i := range Indicators {
		note, ok := cases[i.Basis]
		if ok && !strings.Contains(i.Definition, note) {
			t.Errorf("definição de %s não informa a base %s: %q", i.Key, i.Basis, i.Definition)
		}
		if i.Basis == BasisMonth && (strings.Contains(i.Definition, "Posição atual") || strings.Contains(i.Definition, "último instante")) {
			t.Errorf("definição de %s (fluxo do mês) fala de posição: %q", i.Key, i.Definition)
		}
	}
	for _, key := range []string{"mrr", "arr", "open_discrepancies", "unknown_operations"} {
		if d.Bases[key] != BasisCurrent {
			t.Errorf("%s deveria ser posição atual: %s", key, d.Bases[key])
		}
	}
}

func TestComputeOverflow(t *testing.T) {
	t.Run("MRR acima de int64", func(t *testing.T) {
		_, err := Compute("2026-11", position, Raw{MRR: []MRRComponent{
			{Interval: calendar.Monthly, Gross: rat(math.MaxInt64, 1)},
			{Interval: calendar.Monthly, Gross: rat(math.MaxInt64, 1)},
		}})
		if err == nil {
			t.Fatal("esperado erro de overflow")
		}
	})
	t.Run("ARR acima de int64", func(t *testing.T) {
		_, err := Compute("2026-11", position, Raw{MRR: []MRRComponent{
			{Interval: calendar.Monthly, Gross: rat(math.MaxInt64/2, 1)},
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
			name: "dezembro termina em janeiro do ano seguinte", month: "2025-12", now: now, wantMonth: "2025-12",
			wantStart: time.Date(2025, time.December, 1, 3, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, time.January, 1, 3, 0, 0, 0, time.UTC),
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

func TestPositionAt(t *testing.T) {
	to := time.Date(2026, time.December, 1, 3, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"mês em curso usa o momento da consulta", to.Add(-time.Hour), to.Add(-time.Hour)},
		{"mês encerrado no instante do fim", to, to.Add(-time.Microsecond)},
		{"mês encerrado há tempo", to.AddDate(1, 0, 0), to.Add(-time.Microsecond)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PositionAt(to, tc.now); !got.Equal(tc.want) {
				t.Fatalf("PositionAt = %v, esperado %v", got, tc.want)
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

func TestMonthRangeRejectsFutureMonth(t *testing.T) {
	sp := saoPaulo(t)
	// 01/12 às 02h UTC ainda é novembro em São Paulo: dezembro é futuro.
	now := time.Date(2026, time.December, 1, 2, 0, 0, 0, time.UTC)
	for _, month := range []string{"2026-12", "2027-01", "9999-12"} {
		_, _, _, err := MonthRange(month, sp, now)
		if e, ok := apperr.As(err); !ok || e.Kind != apperr.KindInvalid || e.Code != "future_month" || len(e.Fields) != 1 || e.Fields[0].Field != "month" {
			t.Errorf("%s: esperado future_month, obtido %v", month, err)
		}
	}
	if m, _, _, err := MonthRange("2026-11", sp, now); err != nil || m != "2026-11" {
		t.Errorf("mês corrente recusado: %v", err)
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
	at       time.Time
	current  bool
}

func (f *fakeSource) Location(_ context.Context, tenantID uuid.UUID) (*time.Location, error) {
	f.tenantID = tenantID
	return f.loc, f.locErr
}

func (f *fakeSource) Raw(_ context.Context, tenantID uuid.UUID, w Window) (Raw, error) {
	f.calls++
	f.tenantID, f.from, f.to, f.at, f.current = tenantID, w.From, w.To, w.PositionAt, w.Current
	return f.raw, f.rawErr
}

func TestServiceDashboard(t *testing.T) {
	sp := saoPaulo(t)
	now := time.Date(2026, time.November, 16, 15, 0, 0, 0, time.UTC)
	tenantID := uuid.New()

	t.Run("mês corrente com fórmulas aplicadas", func(t *testing.T) {
		src := &fakeSource{loc: sp, raw: Raw{
			MRR:             []MRRComponent{{Interval: calendar.Monthly, Gross: rat(5000, 1)}},
			DelinquencyOpen: 1, DelinquencyBase: 4,
		}}
		svc := NewService(src, clock.NewManual(now))
		d, err := svc.Dashboard(context.Background(), tenantID, "")
		if err != nil {
			t.Fatal(err)
		}
		if d.Month != "2026-11" || d.MRR != 5000 || d.ARR != 60000 || d.DelinquencyRateBP != 2500 || !d.PositionAt.Equal(now) {
			t.Fatalf("dashboard inesperado: %+v", d)
		}
		if src.tenantID != tenantID || !src.at.Equal(now) || !src.current ||
			!src.from.Equal(time.Date(2026, time.November, 1, 3, 0, 0, 0, time.UTC)) ||
			!src.to.Equal(time.Date(2026, time.December, 1, 3, 0, 0, 0, time.UTC)) {
			t.Fatalf("consulta com parâmetros inesperados: %+v", src)
		}
	})

	t.Run("mês encerrado usa a posição no último instante do mês", func(t *testing.T) {
		src := &fakeSource{loc: sp}
		d, err := NewService(src, clock.NewManual(now)).Dashboard(context.Background(), tenantID, "2025-01")
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2025, time.February, 1, 2, 59, 59, 999999000, time.UTC)
		if !src.at.Equal(want) || !d.PositionAt.Equal(want) || !src.to.Equal(want.Add(time.Microsecond)) || src.current {
			t.Fatalf("posição %v (painel %v), esperado %v", src.at, d.PositionAt, want)
		}
	})

	t.Run("mês futuro é recusado sem consultar agregações", func(t *testing.T) {
		src := &fakeSource{loc: sp}
		_, err := NewService(src, clock.NewManual(now)).Dashboard(context.Background(), tenantID, "2026-12")
		e, ok := apperr.As(err)
		if !ok || e.Kind != apperr.KindInvalid || e.Code != "future_month" || src.calls != 0 {
			t.Fatalf("esperado future_month, err=%v calls=%d", err, src.calls)
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
