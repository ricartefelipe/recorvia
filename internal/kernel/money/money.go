// Package money implementa valores monetários em unidades mínimas inteiras,
// com moeda explícita, aritmética protegida contra overflow e cálculos
// intermediários exatos com números racionais.
//
// Invariantes:
//   - Um Money nunca armazena frações da unidade mínima (centavos para BRL).
//   - Operações entre moedas diferentes falham; não há conversão cambial.
//   - Qualquer operação que excederia o intervalo de int64 retorna ErrOverflow.
//   - Cálculos com taxas, preços fracionários e prorrata usam *big.Rat e são
//     arredondados uma única vez, com RoundingMode explícito.
package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

var (
	// ErrOverflow indica que o resultado não cabe em int64 unidades mínimas.
	ErrOverflow = errors.New("money: overflow")
	// ErrCurrencyMismatch indica operação entre moedas diferentes.
	ErrCurrencyMismatch = errors.New("money: moedas diferentes")
	// ErrUnsupportedCurrency indica moeda não habilitada nesta versão.
	ErrUnsupportedCurrency = errors.New("money: moeda não suportada")
	// ErrInvalidAmount indica texto ou valor monetário malformado.
	ErrInvalidAmount = errors.New("money: valor inválido")
)

// MaxAmount limita valores aceitos em entradas de negócio (1 trilhão de
// unidades maiores). Mantém folga grande para somas e multiplicações
// intermediárias sem aproximar o limite de int64.
const MaxAmount int64 = 1_000_000_000_000_00

// Currency identifica uma moeda ISO 4217 e o número de casas da unidade mínima.
type Currency struct {
	code     string
	exponent int
}

// BRL é a única moeda habilitada na primeira versão.
var BRL = Currency{code: "BRL", exponent: 2}

var supported = map[string]Currency{"BRL": BRL}

// ParseCurrency valida o código ISO 4217 contra as moedas habilitadas.
func ParseCurrency(code string) (Currency, error) {
	c, ok := supported[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrUnsupportedCurrency, code)
	}
	return c, nil
}

// Code retorna o código ISO 4217.
func (c Currency) Code() string { return c.code }

// Exponent retorna a quantidade de casas decimais da unidade mínima.
func (c Currency) Exponent() int { return c.exponent }

// IsZero informa se a moeda não foi definida.
func (c Currency) IsZero() bool { return c.code == "" }

func (c Currency) String() string { return c.code }

// Money é um valor em unidades mínimas de uma moeda.
type Money struct {
	amount   int64
	currency Currency
}

// New cria um valor em unidades mínimas.
func New(minor int64, c Currency) Money { return Money{amount: minor, currency: c} }

// Zero retorna o valor zero na moeda.
func Zero(c Currency) Money { return Money{currency: c} }

// Amount retorna o valor em unidades mínimas.
func (m Money) Amount() int64 { return m.amount }

// Currency retorna a moeda.
func (m Money) Currency() Currency { return m.currency }

// IsZero informa se o valor é zero.
func (m Money) IsZero() bool { return m.amount == 0 }

// IsNegative informa se o valor é negativo.
func (m Money) IsNegative() bool { return m.amount < 0 }

// IsPositive informa se o valor é estritamente positivo.
func (m Money) IsPositive() bool { return m.amount > 0 }

func (m Money) sameCurrency(o Money) error {
	if m.currency != o.currency {
		return fmt.Errorf("%w: %s e %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	return nil
}

// Add soma dois valores da mesma moeda.
func (m Money) Add(o Money) (Money, error) {
	if err := m.sameCurrency(o); err != nil {
		return Money{}, err
	}
	s, err := addInt64(m.amount, o.amount)
	if err != nil {
		return Money{}, err
	}
	return Money{amount: s, currency: m.currency}, nil
}

// Sub subtrai o valor o de m.
func (m Money) Sub(o Money) (Money, error) {
	n, err := o.Neg()
	if err != nil {
		return Money{}, err
	}
	return m.Add(n)
}

// Neg retorna o valor com sinal invertido.
func (m Money) Neg() (Money, error) {
	if m.amount == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return Money{amount: -m.amount, currency: m.currency}, nil
}

// Abs retorna o valor absoluto.
func (m Money) Abs() (Money, error) {
	if m.amount < 0 {
		return m.Neg()
	}
	return m, nil
}

// MulInt multiplica por um inteiro (por exemplo, quantidade de licenças).
func (m Money) MulInt(k int64) (Money, error) {
	p, err := mulInt64(m.amount, k)
	if err != nil {
		return Money{}, err
	}
	return Money{amount: p, currency: m.currency}, nil
}

// MulRat multiplica por um racional e arredonda uma única vez.
func (m Money) MulRat(r *big.Rat, mode RoundingMode) (Money, error) {
	prod := new(big.Rat).Mul(new(big.Rat).SetInt64(m.amount), r)
	return FromRat(prod, m.currency, mode)
}

// Cmp compara valores da mesma moeda: -1, 0 ou 1.
func (m Money) Cmp(o Money) (int, error) {
	if err := m.sameCurrency(o); err != nil {
		return 0, err
	}
	switch {
	case m.amount < o.amount:
		return -1, nil
	case m.amount > o.amount:
		return 1, nil
	}
	return 0, nil
}

// Min retorna o menor entre dois valores da mesma moeda.
func Min(a, b Money) (Money, error) {
	c, err := a.Cmp(b)
	if err != nil {
		return Money{}, err
	}
	if c <= 0 {
		return a, nil
	}
	return b, nil
}

// Max retorna o maior entre dois valores da mesma moeda.
func Max(a, b Money) (Money, error) {
	c, err := a.Cmp(b)
	if err != nil {
		return Money{}, err
	}
	if c >= 0 {
		return a, nil
	}
	return b, nil
}

// Sum soma valores exigindo a mesma moeda c.
func Sum(c Currency, values ...Money) (Money, error) {
	total := Zero(c)
	for _, v := range values {
		var err error
		total, err = total.Add(v)
		if err != nil {
			return Money{}, err
		}
	}
	return total, nil
}

// Rat retorna o valor em unidades mínimas como racional exato.
func (m Money) Rat() *big.Rat { return new(big.Rat).SetInt64(m.amount) }

// DecimalString formata em unidades maiores com ponto decimal ("1234.56").
func (m Money) DecimalString() string {
	return FormatMinor(m.amount, m.currency.exponent)
}

// String retorna representação técnica, por exemplo "BRL 1234.56".
func (m Money) String() string {
	return m.currency.code + " " + m.DecimalString()
}

// FormatMinor formata unidades mínimas com exp casas decimais.
func FormatMinor(minor int64, exp int) string {
	neg := minor < 0
	u := new(big.Int).SetInt64(minor)
	u.Abs(u)
	s := u.String()
	if exp > 0 {
		if len(s) <= exp {
			s = strings.Repeat("0", exp-len(s)+1) + s
		}
		s = s[:len(s)-exp] + "." + s[len(s)-exp:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// ParseMajor interpreta um texto decimal em unidades maiores ("1234.56")
// exigindo no máximo o número de casas da moeda. Não aceita notação
// científica, separador de milhar nem sinal "+".
func ParseMajor(s string, c Currency) (Money, error) {
	r, err := ParseDecimal(s)
	if err != nil {
		return Money{}, err
	}
	if scale := decimalScale(s); scale > c.exponent {
		return Money{}, fmt.Errorf("%w: mais de %d casas decimais", ErrInvalidAmount, c.exponent)
	}
	minor := new(big.Rat).Mul(r, pow10Rat(c.exponent))
	if !minor.IsInt() {
		return Money{}, ErrInvalidAmount
	}
	n := minor.Num()
	if !n.IsInt64() {
		return Money{}, ErrOverflow
	}
	return Money{amount: n.Int64(), currency: c}, nil
}

// CheckBusinessRange garante que o valor esteja dentro de ±MaxAmount.
func (m Money) CheckBusinessRange() error {
	if m.amount > MaxAmount || m.amount < -MaxAmount {
		return fmt.Errorf("%w: valor acima do limite permitido", ErrOverflow)
	}
	return nil
}

func addInt64(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, ErrOverflow
	}
	return a + b, nil
}

func mulInt64(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	p := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	if !p.IsInt64() {
		return 0, ErrOverflow
	}
	return p.Int64(), nil
}

func pow10Rat(exp int) *big.Rat {
	return new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil))
}
