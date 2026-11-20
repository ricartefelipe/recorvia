package money

import (
	"errors"
	"math"
	"math/big"
	"testing"
)

func amounts(ms []Money) []int64 {
	out := make([]int64, len(ms))
	for i, m := range ms {
		out[i] = m.Amount()
	}
	return out
}

func TestAllocate(t *testing.T) {
	cases := []struct {
		name    string
		total   int64
		weights []int64
		want    []int64
	}{
		{"igualitário com sobra vai ao primeiro", 100, []int64{1, 1, 1}, []int64{34, 33, 33}},
		{"proporcional", 1000, []int64{50, 30, 20}, []int64{500, 300, 200}},
		{"maior resto", 10, []int64{3, 3, 4}, []int64{3, 3, 4}},
		{"negativo espelha positivo", -100, []int64{1, 1, 1}, []int64{-34, -33, -33}},
		{"peso zero recebe zero", 7, []int64{0, 1}, []int64{0, 7}},
		{"total zero", 0, []int64{2, 5}, []int64{0, 0}},
		{"restos desempatados por índice", 2, []int64{1, 1, 1, 1}, []int64{1, 1, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Allocate(New(tc.total, BRL), tc.weights)
			if err != nil {
				t.Fatal(err)
			}
			g := amounts(got)
			for i := range g {
				if g[i] != tc.want[i] {
					t.Fatalf("Allocate=%v quero %v", g, tc.want)
				}
			}
		})
	}
}

func TestAllocateInvalidWeights(t *testing.T) {
	for _, w := range [][]int64{nil, {}, {0, 0}, {1, -1}} {
		if _, err := Allocate(New(10, BRL), w); !errors.Is(err, ErrInvalidWeights) {
			t.Errorf("Allocate(%v) err=%v", w, err)
		}
	}
}

// FuzzAllocate verifica as propriedades: a soma é preservada, cada parcela
// tem o sinal do total e difere da proporção exata em menos de uma unidade.
func FuzzAllocate(f *testing.F) {
	f.Add(int64(100), int64(1), int64(1), int64(1))
	f.Add(int64(-999), int64(7), int64(0), int64(13))
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64), int64(1), int64(2))
	f.Fuzz(func(t *testing.T, total, w1, w2, w3 int64) {
		if total == math.MinInt64 {
			return
		}
		ws := []int64{abs64(w1), abs64(w2), abs64(w3)}
		got, err := Allocate(New(total, BRL), ws)
		if ws[0] == 0 && ws[1] == 0 && ws[2] == 0 {
			if !errors.Is(err, ErrInvalidWeights) {
				t.Fatalf("pesos zero deveriam falhar: %v", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		sum := new(big.Int)
		wsum := new(big.Int)
		for _, w := range ws {
			wsum.Add(wsum, big.NewInt(w))
		}
		for i, m := range got {
			sum.Add(sum, big.NewInt(m.Amount()))
			if (total >= 0 && m.Amount() < 0) || (total <= 0 && m.Amount() > 0) {
				t.Fatalf("sinal incorreto: total=%d parcela=%d", total, m.Amount())
			}
			exact := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(total), big.NewInt(ws[i])), wsum)
			diff := new(big.Rat).Sub(exact, new(big.Rat).SetInt64(m.Amount()))
			if diff.Abs(diff).Cmp(big.NewRat(1, 1)) >= 0 {
				t.Fatalf("parcela %d=%d distante da proporção %s", i, m.Amount(), exact.FloatString(4))
			}
		}
		if sum.Int64() != total {
			t.Fatalf("soma %s != total %d", sum, total)
		}
	})
}

// FuzzMulRat garante que o arredondamento nunca se afasta mais de meia
// unidade do valor exato e que nenhum resultado inválido é produzido.
func FuzzMulRat(f *testing.F) {
	f.Add(int64(10000), int64(17), int64(31))
	f.Add(int64(-25), int64(1), int64(10))
	f.Fuzz(func(t *testing.T, amount, num, den int64) {
		if den == 0 || den == math.MinInt64 || num == math.MinInt64 {
			return
		}
		r := big.NewRat(num, den)
		exact := new(big.Rat).Mul(new(big.Rat).SetInt64(amount), r)
		for _, mode := range []RoundingMode{HalfEven, HalfUp, TowardZero} {
			got, err := New(amount, BRL).MulRat(r, mode)
			if err != nil {
				if !errors.Is(err, ErrOverflow) {
					t.Fatalf("erro inesperado: %v", err)
				}
				continue
			}
			diff := new(big.Rat).Sub(exact, new(big.Rat).SetInt64(got.Amount()))
			diff.Abs(diff)
			limit := big.NewRat(1, 2)
			if mode == TowardZero {
				limit = big.NewRat(1, 1)
			}
			if diff.Cmp(limit) > 0 || (mode == TowardZero && diff.Cmp(limit) == 0) {
				t.Fatalf("modo %v: %s arredondado para %d", mode, exact.FloatString(6), got.Amount())
			}
		}
	})
}

// FuzzParseDecimal garante que textos aceitos fazem ida e volta sem perda.
func FuzzParseDecimal(f *testing.F) {
	for _, s := range []string{"0", "1.5", "-0.0035", "abc", "1e9", "12345678901234567890.0000000001"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := ParseDecimal(s)
		if err != nil {
			return
		}
		back, err := ParseDecimal(RatString(r))
		if err != nil || back.Cmp(r) != 0 {
			t.Fatalf("ida e volta falhou para %q: %v", s, err)
		}
	})
}

func abs64(v int64) int64 {
	if v == math.MinInt64 {
		return math.MaxInt64
	}
	if v < 0 {
		return -v
	}
	return v
}
