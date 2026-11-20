package money

import (
	"errors"
	"math"
	"math/big"
	"sort"
)

// ErrInvalidWeights indica pesos negativos ou soma zero na alocação.
var ErrInvalidWeights = errors.New("money: pesos de alocação inválidos")

// Allocate distribui total proporcionalmente aos pesos pelo método do maior
// resto. A soma das parcelas é sempre exatamente igual a total. Cada parcela
// difere da proporção exata em menos de uma unidade mínima. Empates entre
// restos são resolvidos pelo menor índice, o que torna o resultado
// determinístico (útil para distribuir descontos entre itens de fatura).
func Allocate(total Money, weights []int64) ([]Money, error) {
	if len(weights) == 0 {
		return nil, ErrInvalidWeights
	}
	sum := new(big.Int)
	for _, w := range weights {
		if w < 0 {
			return nil, ErrInvalidWeights
		}
		sum.Add(sum, big.NewInt(w))
	}
	if sum.Sign() == 0 {
		return nil, ErrInvalidWeights
	}
	if total.amount == math.MinInt64 {
		return nil, ErrOverflow
	}

	neg := total.amount < 0
	abs := new(big.Int).SetInt64(total.amount)
	abs.Abs(abs)

	type rem struct {
		idx int
		r   *big.Int
	}
	shares := make([]*big.Int, len(weights))
	rems := make([]rem, len(weights))
	allocated := new(big.Int)
	for i, w := range weights {
		p := new(big.Int).Mul(abs, big.NewInt(w))
		q, r := new(big.Int).QuoRem(p, sum, new(big.Int))
		shares[i] = q
		rems[i] = rem{idx: i, r: r}
		allocated.Add(allocated, q)
	}
	left := new(big.Int).Sub(abs, allocated).Int64() // < len(weights)
	sort.SliceStable(rems, func(a, b int) bool {
		c := rems[a].r.Cmp(rems[b].r)
		if c != 0 {
			return c > 0
		}
		return rems[a].idx < rems[b].idx
	})
	for i := int64(0); i < left; i++ {
		shares[rems[i].idx].Add(shares[rems[i].idx], big.NewInt(1))
	}

	out := make([]Money, len(weights))
	for i, s := range shares {
		v := s.Int64()
		if neg {
			v = -v
		}
		out[i] = Money{amount: v, currency: total.currency}
	}
	return out, nil
}
