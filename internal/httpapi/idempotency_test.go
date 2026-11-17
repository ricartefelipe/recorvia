package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestBusinessKeyHasFixedLength(t *testing.T) {
	const (
		client    = "user:0b6f9c1e-4a52-4f7e-9d3c-2f8a7e5b1c90"
		operation = "POST /api/v1/invoices/{id}/credit-notes"
	)
	longest := strings.Repeat("k", 255)
	shortest := "chave-08"
	for _, key := range []string{shortest, "nota-" + strings.Repeat("x", 213), longest} {
		if !validIdempotencyKey(key) {
			t.Fatalf("chave de teste deveria ser aceita pelo middleware: %d caracteres", len(key))
		}
		got := businessKey(client, operation, key)
		sum := sha256.Sum256([]byte(client + "|" + operation + "|" + key))
		if got != hex.EncodeToString(sum[:]) || len(got) != 64 {
			t.Fatalf("chave de %d caracteres: %q (%d)", len(key), got, len(got))
		}
		if businessKey(client, operation, key) != got {
			t.Fatal("a derivação é determinística: a repetição precisa encontrar o mesmo registro")
		}
	}

	base := businessKey(client, operation, longest)
	for name, other := range map[string]string{
		"outro cliente":  businessKey("user:7d0c1b2a-9e8f-4a3b-8c7d-6e5f4a3b2c1d", operation, longest),
		"outra operação": businessKey(client, "POST /api/v1/invoices/{id}/payments", longest),
		"outra chave":    businessKey(client, operation, strings.Repeat("k", 254)+"j"),
	} {
		if other == base {
			t.Errorf("%s não pode produzir a mesma chave de negócio", name)
		}
	}
}
