// Package brdoc faz a validação estrutural de CPF e CNPJ (formato e dígitos
// verificadores). A validação estrutural não confirma a existência nem a
// identidade do titular do documento.
//
// O CNPJ aceita o formato alfanumérico: os 12 primeiros caracteres podem ser
// dígitos ou letras maiúsculas e os 2 últimos são dígitos verificadores. Cada
// caractere vale seu código ASCII menos 48 ('0'..'9' = 0..9, 'A' = 17, ...),
// com os mesmos pesos do cálculo numérico tradicional.
package brdoc

import (
	"errors"
	"strings"
)

// ErrInvalid indica documento estruturalmente inválido.
var ErrInvalid = errors.New("brdoc: documento inválido")

// Kind identifica o tipo de documento.
type Kind string

const (
	// CPF é o cadastro de pessoa física.
	CPF Kind = "cpf"
	// CNPJ é o cadastro de pessoa jurídica.
	CNPJ Kind = "cnpj"
)

// Normalize remove pontuação usual (".", "-", "/", espaços) e converte letras
// para maiúsculas.
func Normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r == '.', r == '-', r == '/', r == ' ':
		default:
			b.WriteRune(r) // mantém para que a validação rejeite
		}
	}
	return b.String()
}

// Validate normaliza e valida o documento do tipo informado, retornando a
// forma normalizada.
func Validate(kind Kind, raw string) (string, error) {
	n := Normalize(raw)
	switch kind {
	case CPF:
		if !validCPF(n) {
			return "", ErrInvalid
		}
	case CNPJ:
		if !validCNPJ(n) {
			return "", ErrInvalid
		}
	default:
		return "", ErrInvalid
	}
	return n, nil
}

func allSame(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}

func validCPF(s string) bool {
	if len(s) != 11 || allSame(s) {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	for _, n := range []int{9, 10} {
		sum := 0
		for i := range n {
			sum += int(s[i]-'0') * (n + 1 - i)
		}
		d := (sum * 10) % 11
		if d == 10 {
			d = 0
		}
		if int(s[n]-'0') != d {
			return false
		}
	}
	return true
}

func cnpjValue(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9', c >= 'A' && c <= 'Z':
		return int(c) - 48, true
	}
	return 0, false
}

// CNPJCheckDigits calcula os dois dígitos verificadores para a base de 12
// caracteres.
func CNPJCheckDigits(base string) (string, error) {
	if len(base) != 12 {
		return "", ErrInvalid
	}
	vals := make([]int, 0, 14)
	for i := range len(base) {
		v, ok := cnpjValue(base[i])
		if !ok {
			return "", ErrInvalid
		}
		vals = append(vals, v)
	}
	w1 := []int{5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
	w2 := []int{6, 5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
	dv := func(weights []int) int {
		sum := 0
		for i, w := range weights {
			sum += vals[i] * w
		}
		r := sum % 11
		if r < 2 {
			return 0
		}
		return 11 - r
	}
	d1 := dv(w1)
	vals = append(vals, d1)
	d2 := dv(w2)
	return string([]byte{byte('0' + d1), byte('0' + d2)}), nil
}

func validCNPJ(s string) bool {
	if len(s) != 14 || allSame(s) {
		return false
	}
	if s[12] < '0' || s[12] > '9' || s[13] < '0' || s[13] > '9' {
		return false
	}
	dv, err := CNPJCheckDigits(s[:12])
	return err == nil && dv == s[12:]
}
