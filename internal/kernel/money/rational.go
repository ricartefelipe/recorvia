package money

import (
	"fmt"
	"math/big"
	"strings"
)

// RoundingMode define como um racional é convertido em unidades mínimas.
type RoundingMode int

const (
	// HalfEven arredonda para o par mais próximo quando a parte descartada
	// é exatamente metade (critério da ABNT NBR 5891). É a política padrão
	// do Recorvia para itens de fatura, prorrata e descontos percentuais.
	HalfEven RoundingMode = iota
	// HalfUp arredonda metades para longe de zero.
	HalfUp
	// TowardZero trunca a parte fracionária.
	TowardZero
)

// DefaultRounding é a política aplicada quando o chamador não escolhe outra.
const DefaultRounding = HalfEven

func (m RoundingMode) String() string {
	switch m {
	case HalfEven:
		return "half_even"
	case HalfUp:
		return "half_up"
	case TowardZero:
		return "toward_zero"
	}
	return fmt.Sprintf("rounding(%d)", int(m))
}

// RoundRat arredonda r para um inteiro segundo mode.
func RoundRat(r *big.Rat, mode RoundingMode) *big.Int {
	num := new(big.Int).Set(r.Num())
	den := r.Denom() // sempre positivo
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if rem.Sign() == 0 || mode == TowardZero {
		return q
	}
	// |rem| * 2 comparado a den decide a direção.
	twice := new(big.Int).Abs(rem)
	twice.Lsh(twice, 1)
	cmp := twice.Cmp(den)
	awayFromZero := false
	switch {
	case cmp > 0:
		awayFromZero = true
	case cmp == 0:
		if mode == HalfUp {
			awayFromZero = true
		} else { // HalfEven
			awayFromZero = q.Bit(0) == 1
		}
	}
	if awayFromZero {
		if num.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}

// FromRat converte um racional expresso em unidades mínimas para Money.
func FromRat(r *big.Rat, c Currency, mode RoundingMode) (Money, error) {
	i := RoundRat(r, mode)
	if !i.IsInt64() {
		return Money{}, ErrOverflow
	}
	return Money{amount: i.Int64(), currency: c}, nil
}

// MaxDecimalScale limita casas decimais aceitas em quantidades e preços
// unitários fracionários (por exemplo, R$ 0,0035 por requisição).
const MaxDecimalScale = 10

// maxDecimalDigits limita o tamanho total de textos decimais aceitos.
const maxDecimalDigits = 30

// ParseDecimal interpreta um decimal estrito: dígitos, ponto opcional e até
// MaxDecimalScale casas. Aceita sinal "-" inicial. Rejeita expoentes,
// espaços, vírgulas e valores vazios.
func ParseDecimal(s string) (*big.Rat, error) {
	if s == "" || len(s) > maxDecimalDigits+2 {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	body := strings.TrimPrefix(s, "-")
	intPart, frac, hasDot := strings.Cut(body, ".")
	if intPart == "" || (hasDot && frac == "") {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	for _, part := range []string{intPart, frac} {
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return nil, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
			}
		}
	}
	if len(frac) > MaxDecimalScale {
		return nil, fmt.Errorf("%w: mais de %d casas decimais", ErrInvalidAmount, MaxDecimalScale)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	return r, nil
}

func decimalScale(s string) int {
	_, frac, ok := strings.Cut(s, ".")
	if !ok {
		return 0
	}
	return len(frac)
}

// FormatRat formata r com exatamente scale casas, arredondando half-even.
func FormatRat(r *big.Rat, scale int) string {
	scaled := new(big.Rat).Mul(r, pow10Rat(scale))
	i := RoundRat(scaled, HalfEven)
	return FormatMinor64OrBig(i, scale)
}

// FormatMinor64OrBig formata um inteiro arbitrário com scale casas.
func FormatMinor64OrBig(i *big.Int, scale int) string {
	neg := i.Sign() < 0
	s := new(big.Int).Abs(i).String()
	if scale > 0 {
		if len(s) <= scale {
			s = strings.Repeat("0", scale-len(s)+1) + s
		}
		s = s[:len(s)-scale] + "." + s[len(s)-scale:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// TrimDecimal remove zeros à direita após o ponto ("1.5000" -> "1.5").
func TrimDecimal(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// RatString formata um racional finito em decimal mínimo, usando até
// MaxDecimalScale casas.
func RatString(r *big.Rat) string {
	return TrimDecimal(FormatRat(r, MaxDecimalScale))
}

// Percent cria o racional bp/10000 a partir de pontos-base (1% = 100 bp).
func Percent(basisPoints int64) *big.Rat {
	return big.NewRat(basisPoints, 10000)
}
