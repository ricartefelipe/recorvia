package pgstore

import "testing"

// Detalhes gravados como JSON null (mapa nil serializado) são lidos como
// objeto vazio: o contrato declara details como objeto, nunca null.
func TestJSONMapNeverReturnsNil(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", `{"a":1}`} {
		m, err := jsonMap([]byte(raw))
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if m == nil {
			t.Fatalf("%q: mapa nil", raw)
		}
	}
	if _, err := jsonMap([]byte("[1]")); err == nil {
		t.Fatal("JSON que não é objeto deveria falhar")
	}
}
