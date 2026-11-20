package calendar

import (
	"errors"
	"math/big"
	"testing"
	"time"
)

func mustLoc(t testing.TB, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%s): %v", name, err)
	}
	return loc
}

func TestMonthlyAnchor31ReturnsTo31(t *testing.T) {
	loc := mustLoc(t, "America/Sao_Paulo")
	s, err := NewSchedule(Monthly, Date{2025, time.January, 31}.StartIn(loc), loc)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2025-01-31", "2025-02-28", "2025-03-31", "2025-04-30", "2025-05-31", "2025-06-30", "2025-07-31"}
	for n, w := range want {
		if got := s.StartDate(n).String(); got != w {
			t.Errorf("StartDate(%d)=%s quero %s", n, got, w)
		}
	}
	// Fevereiro de ano bissexto
	s2, _ := NewSchedule(Monthly, Date{2028, time.January, 30}.StartIn(loc), loc)
	if got := s2.StartDate(1).String(); got != "2028-02-29" {
		t.Errorf("bissexto: %s", got)
	}
	if got := s2.StartDate(2).String(); got != "2028-03-30" {
		t.Errorf("retorno ao dia 30: %s", got)
	}
}

func TestYearlyAnchorFeb29(t *testing.T) {
	loc := time.UTC
	s, err := NewScheduleFromAnchor(Yearly, Date{2024, time.February, 29}, loc)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2024-02-29", "2025-02-28", "2026-02-28", "2027-02-28", "2028-02-29"}
	for n, w := range want {
		if got := s.StartDate(n).String(); got != w {
			t.Errorf("StartDate(%d)=%s quero %s", n, got, w)
		}
	}
	if s.Interval() != Yearly || s.Anchor() != (Date{2024, time.February, 29}) || s.Location() != loc {
		t.Fatal("acessores incorretos")
	}
}

func TestPeriodBoundsAreHalfOpen(t *testing.T) {
	loc := mustLoc(t, "America/Sao_Paulo")
	s, _ := NewScheduleFromAnchor(Monthly, Date{2026, time.January, 15}, loc)
	p := s.Period(0)
	if p.Start != time.Date(2026, 1, 15, 3, 0, 0, 0, time.UTC) {
		t.Fatalf("início em UTC incorreto: %s", p.Start)
	}
	if !p.Contains(p.Start) || p.Contains(p.End) || !p.Contains(p.End.Add(-time.Nanosecond)) {
		t.Fatal("período deve ser [início, fim)")
	}
	if s.Period(1).Start != p.End {
		t.Fatal("períodos devem ser contíguos")
	}
	if p.Days(loc) != 31 {
		t.Fatalf("dias=%d", p.Days(loc))
	}
}

func TestIndexOf(t *testing.T) {
	loc := mustLoc(t, "America/Sao_Paulo")
	s, _ := NewScheduleFromAnchor(Monthly, Date{2025, time.January, 31}, loc)
	cases := []struct {
		at   time.Time
		want int
	}{
		{Date{2025, time.January, 30}.StartIn(loc), -1},
		{Date{2025, time.January, 31}.StartIn(loc), 0},
		{Date{2025, time.February, 27}.StartIn(loc), 0},
		{Date{2025, time.February, 28}.StartIn(loc).Add(-time.Second), 0},
		{Date{2025, time.February, 28}.StartIn(loc), 1},
		{Date{2025, time.March, 30}.StartIn(loc), 1},
		{Date{2025, time.March, 31}.StartIn(loc), 2},
		{Date{2026, time.January, 31}.StartIn(loc), 12},
	}
	for _, tc := range cases {
		if got := s.IndexOf(tc.at); got != tc.want {
			t.Errorf("IndexOf(%s)=%d quero %d", tc.at, got, tc.want)
		}
	}
	if _, _, err := s.PeriodContaining(Date{2024, time.December, 1}.StartIn(loc)); !errors.Is(err, ErrInvalidPeriod) {
		t.Fatalf("antes da âncora deveria falhar: %v", err)
	}
	p, n, err := s.PeriodContaining(Date{2025, time.March, 5}.StartIn(loc))
	if err != nil || n != 1 || DateOf(p.Start, loc).String() != "2025-02-28" || DateOf(p.End, loc).String() != "2025-03-31" {
		t.Fatalf("PeriodContaining=%v,%d,%v", p, n, err)
	}
}

func TestRemainingFraction(t *testing.T) {
	loc := mustLoc(t, "America/Sao_Paulo")
	s, _ := NewScheduleFromAnchor(Monthly, Date{2026, time.March, 1}, loc)
	p := s.Period(0) // 01/03 a 01/04: 31 dias
	cases := []struct {
		at   time.Time
		want *big.Rat
	}{
		{p.Start, big.NewRat(1, 1)},
		{p.Start.Add(-time.Hour), big.NewRat(1, 1)},
		{Date{2026, time.March, 15}.StartIn(loc).Add(15 * time.Hour), big.NewRat(17, 31)},
		{Date{2026, time.March, 31}.StartIn(loc), big.NewRat(1, 31)},
		{p.End, new(big.Rat)},
	}
	for _, tc := range cases {
		got, err := RemainingFraction(p, tc.at, loc)
		if err != nil || got.Cmp(tc.want) != 0 {
			t.Errorf("RemainingFraction(%s)=%v,%v quero %v", tc.at, got, err, tc.want)
		}
	}
	if _, err := RemainingFraction(Period{Start: p.End, End: p.Start}, p.Start, loc); !errors.Is(err, ErrInvalidPeriod) {
		t.Fatal("período invertido deveria falhar")
	}
	if _, err := RemainingFraction(Period{Start: p.Start, End: p.Start.Add(time.Hour)}, p.Start.Add(time.Minute), loc); !errors.Is(err, ErrInvalidPeriod) {
		t.Fatal("período com zero dias civis deveria falhar")
	}
}

func TestDaylightSavingZoneCountsCivilDays(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	s, _ := NewScheduleFromAnchor(Monthly, Date{2026, time.March, 1}, loc)
	p := s.Period(0) // inclui a mudança para horário de verão (dia de 23h)
	if p.Days(loc) != 31 {
		t.Fatalf("dias=%d", p.Days(loc))
	}
	if got := p.End.Sub(p.Start); got != 31*24*time.Hour-time.Hour {
		t.Fatalf("duração em horas inesperada: %s", got)
	}
}

func TestDateHelpers(t *testing.T) {
	if _, err := NewDate(2025, time.February, 29); err == nil {
		t.Fatal("29/02/2025 não existe")
	}
	if _, err := NewDate(2025, 13, 1); err == nil {
		t.Fatal("mês 13 não existe")
	}
	d, err := ParseDate("2024-02-29")
	if err != nil || d != (Date{2024, time.February, 29}) {
		t.Fatalf("ParseDate=%v,%v", d, err)
	}
	if _, err := ParseDate("2024-13-01"); err == nil {
		t.Fatal("ParseDate deveria rejeitar")
	}
	if got := d.AddDays(1).String(); got != "2024-03-01" {
		t.Fatalf("AddDays=%s", got)
	}
	if !IsLeap(2000) || IsLeap(1900) || !IsLeap(2024) || IsLeap(2025) {
		t.Fatal("regra de bissexto incorreta")
	}
	if DaysBetween(Date{2024, 1, 1}, Date{2025, 1, 1}) != 366 {
		t.Fatal("2024 tem 366 dias")
	}
	if d.Compare(d) != 0 || !(Date{2024, 1, 1}).Before(Date{2024, 1, 2}) || (Date{2025, 1, 1}).Compare(Date{2024, 12, 31}) != 1 {
		t.Fatal("Compare incorreto")
	}
	if (Date{2024, 2, 1}).Compare(Date{2024, 3, 1}) != -1 {
		t.Fatal("Compare por mês incorreto")
	}
	if got := addMonthsAnchored(2025, time.January, -2, 31).String(); got != "2024-11-30" {
		t.Fatalf("meses negativos: %s", got)
	}
}

func TestScheduleValidation(t *testing.T) {
	if _, err := NewSchedule("week", time.Now(), time.UTC); !errors.Is(err, ErrInvalidInterval) {
		t.Fatalf("periodicidade inválida: %v", err)
	}
	if _, err := NewSchedule(Monthly, time.Now(), nil); !errors.Is(err, ErrNilLocation) {
		t.Fatalf("fuso nulo: %v", err)
	}
	if _, err := NewScheduleFromAnchor(Monthly, Date{2025, 2, 30}, time.UTC); err == nil {
		t.Fatal("âncora inválida")
	}
	if _, err := NewScheduleFromAnchor(Monthly, Date{2025, 2, 1}, nil); !errors.Is(err, ErrNilLocation) {
		t.Fatal("fuso nulo")
	}
	if _, err := NewScheduleFromAnchor("x", Date{2025, 2, 1}, time.UTC); !errors.Is(err, ErrInvalidInterval) {
		t.Fatal("periodicidade inválida")
	}
	if i, err := ParseInterval("year"); err != nil || i.Months() != 12 {
		t.Fatal("ParseInterval year")
	}
}

// FuzzScheduleInvariants verifica, para âncoras arbitrárias, que os períodos
// são contíguos, não vazios, que o dia de início respeita a âncora limitada
// ao mês e que IndexOf é coerente com Period.
func FuzzScheduleInvariants(f *testing.F) {
	f.Add(2025, 1, 31, 30, false)
	f.Add(2024, 2, 29, 9, true)
	f.Add(1999, 12, 15, 40, false)
	sp, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, year, month, day, n int, yearly bool) {
		year = 1970 + abs(year)%130
		m := time.Month(1 + abs(month)%12)
		day = 1 + abs(day)%DaysIn(year, m)
		n = abs(n) % 60
		interval := Monthly
		if yearly {
			interval = Yearly
		}
		s, err := NewScheduleFromAnchor(interval, Date{year, m, day}, sp)
		if err != nil {
			t.Fatal(err)
		}
		p, next := s.Period(n), s.Period(n+1)
		if err := p.Validate(); err != nil {
			t.Fatalf("período vazio: %v", p)
		}
		if p.End != next.Start {
			t.Fatal("períodos não contíguos")
		}
		sd := s.StartDate(n)
		if sd.Day != min(day, DaysIn(sd.Year, sd.Month)) {
			t.Fatalf("dia %d não respeita âncora %d", sd.Day, day)
		}
		if got := s.IndexOf(p.Start); got != n {
			t.Fatalf("IndexOf(início)=%d quero %d", got, n)
		}
		if got := s.IndexOf(p.End.Add(-time.Nanosecond)); got != n {
			t.Fatalf("IndexOf(fim-1ns)=%d quero %d", got, n)
		}
		frac, err := RemainingFraction(p, p.Start.Add(36*time.Hour), sp)
		if err != nil || frac.Sign() < 0 || frac.Cmp(big.NewRat(1, 1)) > 0 {
			t.Fatalf("fração fora de [0,1]: %v %v", frac, err)
		}
	})
}

func abs(v int) int {
	if v < 0 {
		if v == -v { // menor inteiro representável
			return 0
		}
		return -v
	}
	return v
}

func TestStartInMidnightGap(t *testing.T) {
	// Em 06/10/1996 o horário de verão em São Paulo começou à meia-noite:
	// o dia começa às 01:00 locais (03:00 UTC).
	loc := mustLoc(t, "America/Sao_Paulo")
	d := Date{1996, time.October, 6}
	got := d.StartIn(loc)
	if DateOf(got, loc) != d {
		t.Fatalf("StartIn caiu fora da data: %s", got.In(loc))
	}
	if got != time.Date(1996, 10, 6, 3, 0, 0, 0, time.UTC) {
		t.Fatalf("StartIn=%s", got)
	}
}
