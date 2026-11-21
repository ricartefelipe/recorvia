package address

import (
	"slices"
	"strings"
	"testing"
)

func valid() Address {
	return Address{Street: "Av. Paulista", Number: "1000", District: "Bela Vista", City: "São Paulo", State: "SP", PostalCode: "01310100"}
}

func TestNormalize(t *testing.T) {
	got, fields := Normalize(Address{Street: " Av. Paulista ", Number: " 1000 ", Complement: " 10º andar ", District: " Bela Vista ",
		City: " São Paulo ", State: " sp ", PostalCode: "01.310-100", Country: ""})
	want := Address{Street: "Av. Paulista", Number: "1000", Complement: "10º andar", District: "Bela Vista", City: "São Paulo",
		State: "SP", PostalCode: "01310100", Country: "BR"}
	if fields != nil || got != want {
		t.Fatalf("endereço = %+v (%v)\nesperado %+v", got, fields, want)
	}
	pt := valid()
	pt.Country = " pt "
	if got, fields := Normalize(pt); fields != nil || got.Country != "PT" {
		t.Fatalf("país em minúsculas: %+v (%v)", got, fields)
	}
}

func TestNormalizeRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Address)
		fields []string
	}{
		{"logradouro longo", func(a *Address) { a.Street = strings.Repeat("r", 201) }, []string{"street"}},
		{"número ausente", func(a *Address) { a.Number = " " }, []string{"number"}},
		{"bairro ausente", func(a *Address) { a.District = "" }, []string{"district"}},
		{"cidade ausente", func(a *Address) { a.City = "" }, []string{"city"}},
		{"complemento longo", func(a *Address) { a.Complement = strings.Repeat("c", 101) }, []string{"complement"}},
		{"UF inválida", func(a *Address) { a.State = "S1" }, []string{"state"}},
		{"CEP curto", func(a *Address) { a.PostalCode = "1234567" }, []string{"postal_code"}},
		{"país com três letras", func(a *Address) { a.Country = "BRA" }, []string{"country"}},
		{"vazio", func(a *Address) { *a = Address{} }, []string{"street", "number", "district", "city", "state", "postal_code"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := valid()
			tc.mutate(&a)
			got, fields := Normalize(a)
			names := make([]string, 0, len(fields))
			for _, f := range fields {
				names = append(names, f.Field)
			}
			if got != (Address{}) || !slices.Equal(names, tc.fields) {
				t.Fatalf("endereço %+v, campos %v; esperado vazio e %v", got, names, tc.fields)
			}
		})
	}
}

func TestChangedFields(t *testing.T) {
	base := valid()
	base.Country = "BR"
	if got := base.ChangedFields(nil); !slices.Equal(got, []string{"street", "number", "district", "city", "state", "postal_code", "country"}) {
		t.Fatalf("sem endereço anterior: %v", got)
	}
	if got := base.ChangedFields(&base); got != nil {
		t.Fatalf("igual: %v", got)
	}
	next := base
	next.Complement, next.PostalCode = "Sala 2", "01311000"
	if got := next.ChangedFields(&base); !slices.Equal(got, []string{"complement", "postal_code"}) {
		t.Fatalf("complemento e CEP: %v", got)
	}
}
