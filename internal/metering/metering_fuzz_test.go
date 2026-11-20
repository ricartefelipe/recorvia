package metering

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// decimalPlaces conta as casas decimais significativas de um texto já aceito
// por money.ParseDecimal (zeros à direita não contam).
func decimalPlaces(s string) int {
	_, frac, _ := strings.Cut(strings.TrimPrefix(s, "-"), ".")
	return len(strings.TrimRight(frac, "0"))
}

// FuzzParseEventQuantity garante que parseEvent só aceita quantidades
// representáveis em numeric(20,6), sem alterar o valor informado, e que
// cada rejeição de quantidade tem a mensagem do primeiro limite violado.
func FuzzParseEventQuantity(f *testing.F) {
	for _, s := range []string{"0", "-0", "12.5", " 7 ", "0.000001", "0.0000001", "1.0000000", "1.0000001", "-1",
		"99999999999999.999999", "100000000000000", "1e3", "12,5", "abc", "", "00012.500"} {
		f.Add(s)
	}
	e := newEnv()
	base := e.input()
	f.Fuzz(func(t *testing.T, s string) {
		in := base
		in.Quantity = s
		ev, msg := parseEvent(e.tenantID, e.sub, e.metric, in, testNow)

		text := strings.TrimSpace(s)
		r, err := money.ParseDecimal(text)
		if err != nil {
			if msg != msgDecimal {
				t.Fatalf("quantidade %q inválida: mensagem %q, esperava %q", s, msg, msgDecimal)
			}
			return
		}
		scaleOK := decimalPlaces(text) <= MaxScale
		if withinScale(r, MaxScale) != scaleOK {
			t.Fatalf("withinScale(%q) = %v, esperava %v", text, !scaleOK, scaleOK)
		}
		want := ""
		switch {
		case r.Sign() < 0:
			want = msgNegative
		case r.Cmp(maxQuantity) >= 0:
			want = msgAboveLimit
		case !scaleOK:
			want = msgScale
		}
		if msg != want {
			t.Fatalf("quantidade %q: mensagem %q, esperava %q", s, msg, want)
		}
		if want != "" {
			return
		}
		if ev.Quantity.Cmp(r) != 0 {
			t.Fatalf("quantidade %q gravada como %s", s, ev.Quantity.RatString())
		}
		back, err := money.ParseDecimal(money.FormatRat(ev.Quantity, MaxScale))
		if err != nil || back.Cmp(ev.Quantity) != 0 {
			t.Fatalf("quantidade %q não cabe em seis casas decimais: %v", s, err)
		}
		if intPart, _, _ := strings.Cut(money.FormatRat(ev.Quantity, MaxScale), "."); len(intPart) > 14 {
			t.Fatalf("quantidade %q excede 14 dígitos inteiros", s)
		}
	})
}

// FuzzWithinScale compara withinScale com a regra aritmética: um racional
// reduzido p/q tem representação com até n casas decimais se e somente se q
// divide 10^n.
func FuzzWithinScale(f *testing.F) {
	f.Add(int64(1), int64(3), uint8(6))
	f.Add(int64(25), int64(2), uint8(1))
	f.Add(int64(-1), int64(1_000_000), uint8(6))
	f.Add(int64(1), int64(10_000_000), uint8(6))
	f.Add(int64(0), int64(7), uint8(0))
	f.Fuzz(func(t *testing.T, num, den int64, scale uint8) {
		if den == 0 {
			return
		}
		n := int(scale % 19)
		r := big.NewRat(num, den)
		pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
		want := new(big.Int).Mod(pow, r.Denom()).Sign() == 0
		if got := withinScale(r, n); got != want {
			t.Fatalf("withinScale(%s, %d) = %v, esperava %v", r.RatString(), n, got, want)
		}
	})
}
