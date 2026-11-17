package money

import (
	"errors"
	"math/big"
	"testing"
)

func TestRoundRat(t *testing.T) {
	cases := []struct {
		num, den int64
		mode     RoundingMode
		want     int64
	}{
		// metade exata: half-even escolhe o par
		{5, 2, HalfEven, 2}, {7, 2, HalfEven, 4}, {-5, 2, HalfEven, -2}, {-7, 2, HalfEven, -4},
		// metade exata: half-up afasta de zero
		{5, 2, HalfUp, 3}, {-5, 2, HalfUp, -3},
		// acima e abaixo da metade
		{26, 10, HalfEven, 3}, {24, 10, HalfEven, 2}, {-26, 10, HalfEven, -3}, {-24, 10, HalfUp, -2},
		// truncamento
		{29, 10, TowardZero, 2}, {-29, 10, TowardZero, -2},
		// inteiros exatos
		{10, 1, HalfEven, 10}, {-10, 5, HalfUp, -2},
	}
	for _, tc := range cases {
		got := RoundRat(big.NewRat(tc.num, tc.den), tc.mode)
		if got.Int64() != tc.want {
			t.Errorf("RoundRat(%d/%d,%v)=%s quero %d", tc.num, tc.den, tc.mode, got, tc.want)
		}
	}
}

func TestRoundingModeString(t *testing.T) {
	for mode, want := range map[RoundingMode]string{HalfEven: "half_even", HalfUp: "half_up", TowardZero: "toward_zero", 9: "rounding(9)"} {
		if mode.String() != want {
			t.Errorf("%d.String()=%q", mode, mode.String())
		}
	}
}

func TestMulRatProrationExample(t *testing.T) {
	// R$ 100,00 por 17 de 31 dias = 54,838709... -> 54,84
	m, err := New(10000, BRL).MulRat(big.NewRat(17, 31), DefaultRounding)
	if err != nil || m.Amount() != 5484 {
		t.Fatalf("MulRat=%v,%v", m, err)
	}
	// 10% de R$ 0,25 = 0,025 -> half-even 0,02; half-up 0,03
	he, _ := New(25, BRL).MulRat(Percent(1000), HalfEven)
	hu, _ := New(25, BRL).MulRat(Percent(1000), HalfUp)
	if he.Amount() != 2 || hu.Amount() != 3 {
		t.Fatalf("half-even=%d half-up=%d", he.Amount(), hu.Amount())
	}
}

func TestParseDecimal(t *testing.T) {
	valid := map[string]string{
		"0": "0", "12": "12", "0.0035": "0.0035", "-1.5": "-1.5", "1.0000000001": "1.0000000001",
	}
	for in, want := range valid {
		r, err := ParseDecimal(in)
		if err != nil {
			t.Fatalf("ParseDecimal(%q): %v", in, err)
		}
		if got := RatString(r); got != want {
			t.Errorf("RatString(%q)=%q quero %q", in, got, want)
		}
	}
	for _, in := range []string{"", "-", "1.", ".1", "1e2", "1,5", "abc", "1.00000000001", "--1", " 1", "1.2.3", "1234567890123456789012345678901234"} {
		if _, err := ParseDecimal(in); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("ParseDecimal(%q) deveria falhar, err=%v", in, err)
		}
	}
}

func TestFormatRatAndTrim(t *testing.T) {
	if got := FormatRat(big.NewRat(1, 3), 4); got != "0.3333" {
		t.Errorf("FormatRat 1/3 = %q", got)
	}
	if got := FormatRat(big.NewRat(-1, 8), 2); got != "-0.12" {
		t.Errorf("FormatRat -1/8 = %q", got)
	}
	if got := FormatRat(big.NewRat(7, 1), 0); got != "7" {
		t.Errorf("FormatRat 7 scale 0 = %q", got)
	}
	if got := TrimDecimal("10"); got != "10" {
		t.Errorf("TrimDecimal sem ponto = %q", got)
	}
	if got := TrimDecimal("1.5000"); got != "1.5" {
		t.Errorf("TrimDecimal = %q", got)
	}
	if got := TrimDecimal("2.000"); got != "2" {
		t.Errorf("TrimDecimal inteiro = %q", got)
	}
}

func TestFromRatOverflow(t *testing.T) {
	huge := new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 70))
	if _, err := FromRat(huge, BRL, HalfEven); !errors.Is(err, ErrOverflow) {
		t.Fatalf("esperava overflow, err=%v", err)
	}
}
