package ids

import (
	"testing"

	"github.com/google/uuid"
)

func TestNewIsVersion7AndOrdered(t *testing.T) {
	prev := New()
	if prev.Version() != 7 || prev.Variant() != uuid.RFC4122 {
		t.Fatalf("esperado UUIDv7 RFC 4122, obtido versão %d variante %v", prev.Version(), prev.Variant())
	}
	for range 1000 {
		next := New()
		if next.String() <= prev.String() {
			t.Fatalf("identificadores devem crescer: %s depois de %s", next, prev)
		}
		prev = next
	}
}
