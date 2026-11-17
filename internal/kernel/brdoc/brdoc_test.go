package brdoc

import (
	"errors"
	"testing"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		kind Kind
		in   string
		want string
		ok   bool
	}{
		{CPF, "529.982.247-25", "52998224725", true},
		{CPF, "52998224724", "", false},
		{CPF, "111.111.111-11", "", false},
		{CPF, "5299822472", "", false},
		{CPF, "5299822472A", "", false},
		{CNPJ, "11.222.333/0001-81", "11222333000181", true},
		{CNPJ, "11.222.333/0001-80", "", false},
		{CNPJ, "00000000000000", "", false},
		// Exemplo de CNPJ alfanumérico publicado pela Receita Federal.
		{CNPJ, "12.ABC.345/01DE-35", "12ABC34501DE35", true},
		{CNPJ, "12.abc.345/01de-35", "12ABC34501DE35", true},
		{CNPJ, "12.ABC.345/01DE-36", "", false},
		{CNPJ, "12ABC34501DEX5", "", false},
		{CNPJ, "12ABC34501D#35", "", false},
		{"rg", "123", "", false},
	}
	for _, tc := range cases {
		got, err := Validate(tc.kind, tc.in)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Errorf("Validate(%s,%q)=%q,%v quero %q", tc.kind, tc.in, got, err, tc.want)
			}
		} else if !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%s,%q) deveria falhar, obtive %q", tc.kind, tc.in, got)
		}
	}
}

func TestCNPJCheckDigits(t *testing.T) {
	if dv, err := CNPJCheckDigits("112223330001"); err != nil || dv != "81" {
		t.Fatalf("dv=%q err=%v", dv, err)
	}
	if _, err := CNPJCheckDigits("123"); !errors.Is(err, ErrInvalid) {
		t.Fatal("base curta deveria falhar")
	}
	if _, err := CNPJCheckDigits("12345678901!"); !errors.Is(err, ErrInvalid) {
		t.Fatal("caractere inválido deveria falhar")
	}
}

// FuzzCNPJRoundTrip: qualquer base válida com seus dígitos calculados é
// aceita; alterar um dígito verificador sempre invalida.
func FuzzCNPJRoundTrip(f *testing.F) {
	f.Add("112223330001")
	f.Add("12ABC34501DE")
	f.Fuzz(func(t *testing.T, base string) {
		dv, err := CNPJCheckDigits(base)
		if err != nil {
			return
		}
		doc := base + dv
		if allSame(doc) {
			return
		}
		if _, err := Validate(CNPJ, doc); err != nil {
			t.Fatalf("CNPJ %s com DV calculado rejeitado", doc)
		}
		wrong := []byte(doc)
		wrong[13] = '0' + (wrong[13]-'0'+1)%10
		if _, err := Validate(CNPJ, string(wrong)); err == nil {
			t.Fatalf("CNPJ %s com DV alterado aceito", wrong)
		}
	})
}
