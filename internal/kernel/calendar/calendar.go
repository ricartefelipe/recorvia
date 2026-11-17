// Package calendar implementa regras de calendário para ciclos de cobrança.
//
// Convenções:
//   - Instantes são armazenados em UTC; regras de calendário usam o fuso IANA
//     do tenant.
//   - Um Period é um intervalo [Start, End): início inclusivo, fim exclusivo.
//   - Cada período começa no primeiro instante de uma data civil do fuso.
//   - A âncora original do ciclo é preservada: o n-ésimo período é calculado
//     a partir da âncora, nunca a partir do período anterior. Assim, uma
//     assinatura ancorada no dia 31 cai em 28/29 de fevereiro e volta ao dia
//     31 em março, sem deslocamento permanente.
package calendar

import (
	"errors"
	"fmt"
	"math/big"
	"time"
)

var (
	// ErrInvalidInterval indica periodicidade desconhecida.
	ErrInvalidInterval = errors.New("calendar: periodicidade inválida")
	// ErrInvalidPeriod indica período vazio ou invertido.
	ErrInvalidPeriod = errors.New("calendar: período inválido")
	// ErrNilLocation indica fuso não informado.
	ErrNilLocation = errors.New("calendar: fuso horário obrigatório")
)

// Date é uma data civil sem fuso.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// DateOf retorna a data civil de t no fuso loc.
func DateOf(t time.Time, loc *time.Location) Date {
	y, m, d := t.In(loc).Date()
	return Date{Year: y, Month: m, Day: d}
}

// NewDate valida e cria uma data civil.
func NewDate(y int, m time.Month, d int) (Date, error) {
	if m < time.January || m > time.December || d < 1 || d > DaysIn(y, m) {
		return Date{}, fmt.Errorf("calendar: data inválida %04d-%02d-%02d", y, m, d)
	}
	return Date{Year: y, Month: m, Day: d}, nil
}

// ParseDate interpreta "AAAA-MM-DD".
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return Date{}, fmt.Errorf("calendar: data inválida %q", s)
	}
	return Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}, nil
}

// String formata como "AAAA-MM-DD".
func (d Date) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day) }

// StartIn retorna o primeiro instante da data no fuso loc, em UTC. Quando a
// meia-noite não existe por início de horário de verão (como ocorria em
// America/Sao_Paulo), time.Date pode escolher um instante que ainda pertence
// ao dia anterior; nesse caso o início do dia é a própria transição de fuso.
func (d Date) StartIn(loc *time.Location) time.Time {
	t := time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
	if DateOf(t, loc) != d {
		if _, end := t.ZoneBounds(); !end.IsZero() {
			t = end
		}
	}
	return t.UTC()
}

// AddDays soma n dias civis.
func (d Date) AddDays(n int) Date {
	t := time.Date(d.Year, d.Month, d.Day+n, 12, 0, 0, 0, time.UTC)
	return Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}

// Compare retorna -1, 0 ou 1.
func (d Date) Compare(o Date) int {
	switch {
	case d.Year != o.Year:
		return cmpInt(d.Year, o.Year)
	case d.Month != o.Month:
		return cmpInt(int(d.Month), int(o.Month))
	}
	return cmpInt(d.Day, o.Day)
}

// Before informa se d é anterior a o.
func (d Date) Before(o Date) bool { return d.Compare(o) < 0 }

// DaysBetween retorna o número de dias civis de a até b (b - a).
func DaysBetween(a, b Date) int {
	ta := time.Date(a.Year, a.Month, a.Day, 12, 0, 0, 0, time.UTC)
	tb := time.Date(b.Year, b.Month, b.Day, 12, 0, 0, 0, time.UTC)
	return int(tb.Sub(ta).Hours() / 24)
}

// DaysIn retorna a quantidade de dias do mês.
func DaysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 12, 0, 0, 0, time.UTC).Day()
}

// IsLeap informa se o ano é bissexto.
func IsLeap(year int) bool { return DaysIn(year, time.February) == 29 }

// addMonthsAnchored soma n meses a partir de (year, month) e posiciona no
// dia anchorDay, limitado ao último dia do mês resultante.
func addMonthsAnchored(year int, month time.Month, n, anchorDay int) Date {
	total := year*12 + int(month-1) + n
	y, m := total/12, time.Month(total%12+1)
	if total < 0 && total%12 != 0 {
		y--
		m = time.Month(total%12 + 13)
	}
	return Date{Year: y, Month: m, Day: min(anchorDay, DaysIn(y, m))}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Interval é a periodicidade de cobrança.
type Interval string

const (
	// Monthly cobra a cada mês civil a partir da âncora.
	Monthly Interval = "month"
	// Yearly cobra a cada ano a partir da âncora.
	Yearly Interval = "year"
)

// ParseInterval valida a periodicidade.
func ParseInterval(s string) (Interval, error) {
	switch Interval(s) {
	case Monthly, Yearly:
		return Interval(s), nil
	}
	return "", fmt.Errorf("%w: %q", ErrInvalidInterval, s)
}

// Months retorna quantos meses tem um ciclo.
func (i Interval) Months() int {
	if i == Yearly {
		return 12
	}
	return 1
}

// Period é um intervalo [Start, End) em UTC.
type Period struct {
	Start time.Time
	End   time.Time
}

// Validate garante Start < End.
func (p Period) Validate() error {
	if !p.Start.Before(p.End) {
		return ErrInvalidPeriod
	}
	return nil
}

// Contains informa se t pertence ao período (início inclusivo, fim exclusivo).
func (p Period) Contains(t time.Time) bool {
	return !t.Before(p.Start) && t.Before(p.End)
}

// Days retorna a quantidade de dias civis do período no fuso loc.
func (p Period) Days(loc *time.Location) int {
	return DaysBetween(DateOf(p.Start, loc), DateOf(p.End, loc))
}

// Schedule descreve um ciclo recorrente ancorado numa data civil.
type Schedule struct {
	interval Interval
	anchor   Date
	loc      *time.Location
}

// NewSchedule cria um ciclo cuja âncora é a data civil local de firstStart.
func NewSchedule(interval Interval, firstStart time.Time, loc *time.Location) (Schedule, error) {
	if loc == nil {
		return Schedule{}, ErrNilLocation
	}
	if _, err := ParseInterval(string(interval)); err != nil {
		return Schedule{}, err
	}
	return Schedule{interval: interval, anchor: DateOf(firstStart, loc), loc: loc}, nil
}

// NewScheduleFromAnchor cria um ciclo a partir de uma âncora já conhecida.
func NewScheduleFromAnchor(interval Interval, anchor Date, loc *time.Location) (Schedule, error) {
	if loc == nil {
		return Schedule{}, ErrNilLocation
	}
	if _, err := ParseInterval(string(interval)); err != nil {
		return Schedule{}, err
	}
	if _, err := NewDate(anchor.Year, anchor.Month, anchor.Day); err != nil {
		return Schedule{}, err
	}
	return Schedule{interval: interval, anchor: anchor, loc: loc}, nil
}

// Anchor retorna a data âncora.
func (s Schedule) Anchor() Date { return s.anchor }

// Interval retorna a periodicidade.
func (s Schedule) Interval() Interval { return s.interval }

// Location retorna o fuso do ciclo.
func (s Schedule) Location() *time.Location { return s.loc }

// StartDate retorna a data civil de início do n-ésimo período (n >= 0).
func (s Schedule) StartDate(n int) Date {
	return addMonthsAnchored(s.anchor.Year, s.anchor.Month, n*s.interval.Months(), s.anchor.Day)
}

// Period retorna o n-ésimo período.
func (s Schedule) Period(n int) Period {
	return Period{Start: s.StartDate(n).StartIn(s.loc), End: s.StartDate(n + 1).StartIn(s.loc)}
}

// IndexOf retorna o índice do período que contém t. Instantes anteriores à
// âncora retornam -1.
func (s Schedule) IndexOf(t time.Time) int {
	d := DateOf(t, s.loc)
	if d.Before(s.anchor) {
		return -1
	}
	months := (d.Year-s.anchor.Year)*12 + int(d.Month-s.anchor.Month)
	n := months / s.interval.Months()
	// Ajuste fino: a estimativa pode estar uma posição adiante quando o dia
	// do mês ainda não alcançou o dia de início do período.
	for n > 0 && t.Before(s.Period(n).Start) {
		n--
	}
	for !t.Before(s.Period(n).End) {
		n++
	}
	return n
}

// PeriodContaining retorna o período que contém t e seu índice.
func (s Schedule) PeriodContaining(t time.Time) (Period, int, error) {
	n := s.IndexOf(t)
	if n < 0 {
		return Period{}, -1, fmt.Errorf("%w: instante anterior à âncora", ErrInvalidPeriod)
	}
	return s.Period(n), n, nil
}

// RemainingFraction calcula a fração do período ainda não usufruída a partir
// de at, por dias civis no fuso loc. O dia da alteração conta como dia
// restante (a alteração vale para o dia inteiro em que ocorre). O resultado
// fica sempre entre 0 e 1.
func RemainingFraction(p Period, at time.Time, loc *time.Location) (*big.Rat, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	total := p.Days(loc)
	if total <= 0 {
		return nil, ErrInvalidPeriod
	}
	switch {
	case !at.After(p.Start):
		return big.NewRat(1, 1), nil
	case !at.Before(p.End):
		return new(big.Rat), nil
	}
	remaining := DaysBetween(DateOf(at, loc), DateOf(p.End, loc))
	remaining = max(0, min(remaining, total))
	return big.NewRat(int64(remaining), int64(total)), nil
}
