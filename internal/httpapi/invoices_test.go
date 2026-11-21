package httpapi

import "testing"

// Valores que começam com caractere interpretado como fórmula por planilhas
// (=, +, -, @, tabulação e retorno de carro) recebem um apóstrofo na frente;
// os demais saem como estão.
func TestCSVSafeNeutralizesFormulas(t *testing.T) {
	cases := []struct{ in, want string }{
		{"=HYPERLINK(\"http://x.test\")", "'=HYPERLINK(\"http://x.test\")"},
		{"+5511999990000", "'+5511999990000"},
		{"-2+3", "'-2+3"},
		{"@SUM(A1:A2)", "'@SUM(A1:A2)"},
		{"\t=1+1", "'\t=1+1"},
		{"\r=1+1", "'\r=1+1"},
		{"Cliente Comum", "Cliente Comum"},
		{"Empresa = Sociedade", "Empresa = Sociedade"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := csvSafe(tc.in); got != tc.want {
			t.Errorf("csvSafe(%q) = %q, esperado %q", tc.in, got, tc.want)
		}
	}
}
