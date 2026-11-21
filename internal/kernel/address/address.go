// Package address modela o endereço postal brasileiro usado no endereço de
// cobrança do cliente e no endereço da organização: o mesmo formato, as
// mesmas regras de normalização e validação e a mesma lista de campos
// alterados na auditoria.
package address

import (
	"regexp"
	"strings"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

// Address é um endereço postal.
type Address struct {
	Street     string `json:"street"`
	Number     string `json:"number"`
	Complement string `json:"complement,omitempty"`
	District   string `json:"district"`
	City       string `json:"city"`
	State      string `json:"state"`
	PostalCode string `json:"postal_code"`
	Country    string `json:"country"`
}

var (
	stateRe  = regexp.MustCompile(`^[A-Z]{2}$`)
	postalRe = regexp.MustCompile(`^[0-9]{8}$`)
)

// Normalize remove espaços das pontas, põe UF e país em maiúsculas, tira a
// pontuação do CEP e assume o Brasil quando o país não é informado. Devolve o
// endereço normalizado e os campos inválidos, pelos nomes do JSON.
func Normalize(a Address) (Address, []apperr.FieldError) {
	var f []apperr.FieldError
	trim := strings.TrimSpace
	a.Street, a.Number, a.Complement = trim(a.Street), trim(a.Number), trim(a.Complement)
	a.District, a.City = trim(a.District), trim(a.City)
	a.State = strings.ToUpper(trim(a.State))
	a.PostalCode = strings.NewReplacer("-", "", ".", "", " ", "").Replace(a.PostalCode)
	a.Country = strings.ToUpper(trim(a.Country))
	if a.Country == "" {
		a.Country = "BR"
	}
	req := func(field, v string, maxLen int) {
		if n := len([]rune(v)); n < 1 || n > maxLen {
			f = append(f, apperr.FieldError{Field: field, Message: "Campo obrigatório."})
		}
	}
	req("street", a.Street, 200)
	req("number", a.Number, 20)
	req("district", a.District, 100)
	req("city", a.City, 100)
	if len([]rune(a.Complement)) > 100 {
		f = append(f, apperr.FieldError{Field: "complement", Message: "Complemento deve ter no máximo 100 caracteres."})
	}
	if !stateRe.MatchString(a.State) {
		f = append(f, apperr.FieldError{Field: "state", Message: "Informe a UF com duas letras."})
	}
	if !postalRe.MatchString(a.PostalCode) {
		f = append(f, apperr.FieldError{Field: "postal_code", Message: "CEP deve ter 8 dígitos."})
	}
	if !stateRe.MatchString(a.Country) {
		f = append(f, apperr.FieldError{Field: "country", Message: "País deve ter duas letras (ISO 3166-1)."})
	}
	if len(f) > 0 {
		return Address{}, f
	}
	return a, nil
}

// ChangedFields devolve os nomes dos campos do endereço que diferem de prev
// (todos os preenchidos, quando não havia endereço). Somente nomes, nunca
// valores (ADR-061).
func (a Address) ChangedFields(prev *Address) []string {
	var old Address
	if prev != nil {
		old = *prev
	}
	var changed []string
	for _, f := range []struct {
		name     string
		old, new string
	}{{"street", old.Street, a.Street}, {"number", old.Number, a.Number}, {"complement", old.Complement, a.Complement},
		{"district", old.District, a.District}, {"city", old.City, a.City}, {"state", old.State, a.State},
		{"postal_code", old.PostalCode, a.PostalCode}, {"country", old.Country, a.Country}} {
		if f.old != f.new {
			changed = append(changed, f.name)
		}
	}
	return changed
}
