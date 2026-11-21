package pgstore

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
)

func wantInvalidCursor(t *testing.T, raw string, err error) {
	t.Helper()
	if e, ok := apperr.As(err); !ok || e.Code != "invalid_cursor" {
		t.Errorf("cursor %q: erro %v, esperado invalid_cursor", raw, err)
	}
}

// O cursor das métricas leva o código e o identificador; um código que o
// banco recusaria como texto torna o cursor inválido (422), em vez de falhar
// na consulta (500).
func TestCodeCursor(t *testing.T) {
	id := uuid.New()
	for _, code := range []string{"chamadas.api", "gb_armazenado", "a|b"} {
		c, got, err := parseCodeCursor(codeCursor(code, id))
		if err != nil || c == nil || *c != code || got == nil || *got != id {
			t.Fatalf("%q: %v %v %v", code, c, got, err)
		}
	}
	if c, got, err := parseCodeCursor(""); c != nil || got != nil || err != nil {
		t.Fatalf("cursor vazio é a primeira página: %v %v %v", c, got, err)
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, raw := range []string{"%%%", enc("chamadas|" + id.String()), enc("k:chamadas"), enc("k:chamadas|nao-e-uuid"),
		enc("k:\xff\xfe|" + id.String()), enc("k:cha\x00madas|" + id.String()), timeCursor(time.Now(), id)} {
		_, _, err := parseCodeCursor(raw)
		wantInvalidCursor(t, raw, err)
	}
}

// O cursor dos reajustes leva a vigência, o instante da programação e o
// identificador, a posição completa da ordenação.
func TestAdjustmentCursor(t *testing.T) {
	row := dbgen.SubscriptionPriceAdjustment{ID: uuid.New(), EffectiveDate: dateToPg(calendar.Date{Year: 2027, Month: time.March, Day: 1}),
		CreatedAt: time.Date(2026, time.November, 21, 12, 30, 15, 123456000, time.UTC)}
	p, err := parseAdjustmentCursor(adjustmentCursor(row))
	if err != nil || p.CursorDate != row.EffectiveDate || p.CursorTs == nil || !p.CursorTs.Equal(row.CreatedAt) || p.CursorID == nil ||
		*p.CursorID != row.ID {
		t.Fatalf("cursor decodificado: %+v %v", p, err)
	}
	if p, err := parseAdjustmentCursor(""); err != nil || p.CursorDate != (pgtype.Date{}) || p.CursorTs != nil || p.CursorID != nil {
		t.Fatalf("cursor vazio é a primeira página: %+v %v", p, err)
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, raw := range []string{"%%%", enc("2027-03-01|2026-11-21T12:30:15Z|" + row.ID.String()), enc("a:2027-03-01|" + row.ID.String()),
		enc("a:01/03/2027|2026-11-21T12:30:15Z|" + row.ID.String()), enc("a:2027-03-01|ontem|" + row.ID.String()),
		enc("a:2027-03-01|2026-11-21T12:30:15Z|nao-e-uuid")} {
		_, err := parseAdjustmentCursor(raw)
		wantInvalidCursor(t, raw, err)
	}
}
