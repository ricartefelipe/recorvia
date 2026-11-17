package money

import (
	"errors"
	"math"
	"math/big"
	"testing"
)

func TestParseCurrency(t *testing.T) {
	c, err := ParseCurrency(" brl ")
	if err != nil || c != BRL {
		t.Fatalf("ParseCurrency(brl) = %v, %v", c, err)
	}
	if c.Code() != "BRL" || c.Exponent() != 2 || c.String() != "BRL" || c.IsZero() {
		t.Fatalf("propriedades inesperadas: %+v", c)
	}
	if _, err := ParseCurrency("USD"); !errors.Is(err, ErrUnsupportedCurrency) {
		t.Fatalf("USD deveria ser rejeitado, err=%v", err)
	}
	if !(Currency{}).IsZero() {
		t.Fatal("moeda vazia deveria ser zero")
	}
}

func TestArithmetic(t *testing.T) {
	a := New(1050, BRL)
	b := New(-250, BRL)
	sum, err := a.Add(b)
	if err != nil || sum.Amount() != 800 {
		t.Fatalf("Add = %v, %v", sum, err)
	}
	diff, err := a.Sub(b)
	if err != nil || diff.Amount() != 1300 {
		t.Fatalf("Sub = %v, %v", diff, err)
	}
	prod, err := a.MulInt(3)
	if err != nil || prod.Amount() != 3150 {
		t.Fatalf("MulInt = %v, %v", prod, err)
	}
	if z, _ := a.MulInt(0); !z.IsZero() {
		t.Fatal("MulInt(0) deveria ser zero")
	}
	abs, err := b.Abs()
	if err != nil || abs.Amount() != 250 {
		t.Fatalf("Abs = %v, %v", abs, err)
	}
	if p, _ := a.Abs(); p != a {
		t.Fatal("Abs de positivo deve ser identidade")
	}
	if !b.IsNegative() || b.IsPositive() || !a.IsPositive() || a.IsZero() {
		t.Fatal("predicados de sinal incorretos")
	}
	if Zero(BRL).Currency() != BRL {
		t.Fatal("Zero deve preservar moeda")
	}
}

func TestOverflowProtection(t *testing.T) {
	maxV := New(math.MaxInt64, BRL)
	minV := New(math.MinInt64, BRL)
	cases := []struct {
		name string
		fn   func() error
	}{
		{"add", func() error { _, err := maxV.Add(New(1, BRL)); return err }},
		{"sub", func() error { _, err := minV.Sub(New(1, BRL)); return err }},
		{"neg", func() error { _, err := minV.Neg(); return err }},
		{"abs", func() error { _, err := minV.Abs(); return err }},
		{"mul", func() error { _, err := maxV.MulInt(2); return err }},
		{"mulrat", func() error { _, err := maxV.MulRat(big.NewRat(3, 2), HalfEven); return err }},
		{"sum", func() error { _, err := Sum(BRL, maxV, New(1, BRL)); return err }},
		{"allocate", func() error { _, err := Allocate(minV, []int64{1}); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); !errors.Is(err, ErrOverflow) {
				t.Fatalf("esperava ErrOverflow, obtive %v", err)
			}
		})
	}
	if err := New(MaxAmount+1, BRL).CheckBusinessRange(); !errors.Is(err, ErrOverflow) {
		t.Fatal("CheckBusinessRange deveria rejeitar acima do limite")
	}
	if err := New(-MaxAmount, BRL).CheckBusinessRange(); err != nil {
		t.Fatalf("limite inferior deveria ser aceito: %v", err)
	}
}

func TestCurrencyMismatch(t *testing.T) {
	other := Money{amount: 1, currency: Currency{code: "XTS", exponent: 2}}
	a := New(1, BRL)
	if _, err := a.Add(other); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Add: %v", err)
	}
	if _, err := a.Cmp(other); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Cmp: %v", err)
	}
	if _, err := Min(a, other); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Min: %v", err)
	}
	if _, err := Max(a, other); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Max: %v", err)
	}
	if _, err := Sum(BRL, a, other); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Sum: %v", err)
	}
}

func TestCompareMinMax(t *testing.T) {
	a, b := New(10, BRL), New(20, BRL)
	for _, tc := range []struct {
		x, y Money
		want int
	}{{a, b, -1}, {b, a, 1}, {a, a, 0}} {
		got, err := tc.x.Cmp(tc.y)
		if err != nil || got != tc.want {
			t.Fatalf("Cmp(%v,%v)=%d,%v", tc.x, tc.y, got, err)
		}
	}
	if m, _ := Min(a, b); m != a {
		t.Fatal("Min incorreto")
	}
	if m, _ := Min(b, a); m != a {
		t.Fatal("Min incorreto (ordem invertida)")
	}
	if m, _ := Max(a, b); m != b {
		t.Fatal("Max incorreto")
	}
	if m, _ := Max(b, a); m != b {
		t.Fatal("Max incorreto (ordem invertida)")
	}
}

func TestFormatting(t *testing.T) {
	cases := []struct {
		minor int64
		want  string
	}{
		{0, "0.00"}, {5, "0.05"}, {99, "0.99"}, {100, "1.00"}, {123456, "1234.56"},
		{-5, "-0.05"}, {-123456, "-1234.56"}, {math.MinInt64, "-92233720368547758.08"},
	}
	for _, tc := range cases {
		if got := New(tc.minor, BRL).DecimalString(); got != tc.want {
			t.Errorf("DecimalString(%d)=%q, quero %q", tc.minor, got, tc.want)
		}
	}
	if got := New(1990, BRL).String(); got != "BRL 19.90" {
		t.Errorf("String=%q", got)
	}
	if got := FormatMinor(42, 0); got != "42" {
		t.Errorf("FormatMinor exp 0 = %q", got)
	}
}

func TestParseMajor(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr error
	}{
		{"0", 0, nil}, {"1", 100, nil}, {"1.5", 150, nil}, {"1234.56", 123456, nil},
		{"-0.01", -1, nil}, {"0.001", 0, ErrInvalidAmount}, {"1,00", 0, ErrInvalidAmount},
		{"", 0, ErrInvalidAmount}, {"1e3", 0, ErrInvalidAmount}, {"+1", 0, ErrInvalidAmount},
		{".5", 0, ErrInvalidAmount}, {"5.", 0, ErrInvalidAmount}, {" 1", 0, ErrInvalidAmount},
		{"99999999999999999999", 0, ErrOverflow},
	}
	for _, tc := range cases {
		got, err := ParseMajor(tc.in, BRL)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("ParseMajor(%q) err=%v, quero %v", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got.Amount() != tc.want {
			t.Errorf("ParseMajor(%q)=%v,%v quero %d", tc.in, got, err, tc.want)
		}
	}
}
