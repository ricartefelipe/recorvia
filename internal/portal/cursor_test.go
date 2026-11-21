package portal

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

func rawCursor(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestCursorRoundTrip(t *testing.T) {
	id := uuid.MustParse("019a7c1e-0000-7000-8000-00000000abcd")
	loc := time.FixedZone("BRT", -3*60*60)
	cases := []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{"UTC com microssegundos", time.Date(2026, time.November, 17, 12, 0, 0, 123456000, time.UTC), time.Date(2026, time.November, 17, 12, 0, 0, 123456000, time.UTC)},
		{"nanossegundos truncados", time.Date(2026, time.November, 17, 12, 0, 0, 123456789, time.UTC), time.Date(2026, time.November, 17, 12, 0, 0, 123456000, time.UTC)},
		{"outro fuso vira UTC", time.Date(2026, time.November, 17, 9, 0, 0, 0, loc), time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, kind := range []string{cursorInvoices, cursorLinks} {
				c := encodeCursor(kind, Position{At: tc.at, ID: id})
				if len(c) > maxCursorLength || strings.ContainsAny(c, "+/=|") {
					t.Fatalf("cursor fora do formato opaco: %q", c)
				}
				p, err := parseCursor(kind, c)
				if err != nil || p == nil || !p.At.Equal(tc.want) || p.At.Location() != time.UTC || p.ID != id {
					t.Fatalf("ida e volta: %+v %v", p, err)
				}
			}
		})
	}
	if p, err := parseCursor(cursorInvoices, ""); p != nil || err != nil {
		t.Fatalf("cursor vazio é a primeira página: %+v %v", p, err)
	}
}

func TestParseCursorRejectsMalformedInput(t *testing.T) {
	id := uuid.NewString()
	const at = "2026-11-17T12:00:00Z"
	cases := map[string]string{
		"não é base64url":          "não é base64",
		"base64 com preenchimento": base64.URLEncoding.EncodeToString([]byte(cursorInvoices + "|2026-11-17T12:00:00.5Z|" + id)),
		"sem separadores":          rawCursor("pi"),
		"partes a mais":            rawCursor(cursorInvoices + "|" + at + "|" + id + "|x"),
		"tipo de outra listagem":   encodeCursor(cursorLinks, Position{At: testNow, ID: uuid.New()}),
		"tipo desconhecido":        rawCursor("zz|" + at + "|" + id),
		"instante inválido":        rawCursor(cursorInvoices + "|ontem|" + id),
		"identificador inválido":   rawCursor(cursorInvoices + "|" + at + "|123"),
		"forma não canônica":       rawCursor(cursorInvoices + "|2026-11-17T09:00:00-03:00|" + id),
		"identificador maiúsculo":  rawCursor(cursorInvoices + "|" + at + "|" + strings.ToUpper(id)),
		"longo demais":             strings.Repeat("A", maxCursorLength+1),
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := parseCursor(cursorInvoices, c)
			if p != nil {
				t.Fatalf("cursor inválido aceito: %+v", p)
			}
			assertErr(t, err, apperr.KindInvalid, "invalid_cursor")
		})
	}
}

func TestPageLimit(t *testing.T) {
	for in, want := range map[int]int{-1: DefaultPageSize, 0: DefaultPageSize, 1: 1, 25: 25, MaxPageSize: MaxPageSize, MaxPageSize + 1: MaxPageSize} {
		if got := pageLimit(in); got != want {
			t.Errorf("pageLimit(%d) = %d, esperado %d", in, got, want)
		}
	}
}

// Qualquer entrada é recusada ou é exatamente a forma canônica de uma
// posição: um cursor aceito nunca carrega nada além do instante e do
// identificador, e reaparece idêntico ao ser codificado de novo.
func FuzzParseCursor(f *testing.F) {
	valid := encodeCursor(cursorInvoices, Position{At: testNow, ID: uuid.MustParse("019a7c1e-0000-7000-8000-00000000abcd")})
	for _, s := range []string{valid, valid + "A", valid[:len(valid)-1], "", "AAAA", rawCursor("pi|2026-11-17T12:00:00Z|x"),
		rawCursor("pl|2026-11-17T12:00:00Z|019a7c1e-0000-7000-8000-00000000abcd"), rawCursor("pi||"), strings.Repeat("-", 200)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, c string) {
		for _, kind := range []string{cursorInvoices, cursorLinks} {
			p, err := parseCursor(kind, c)
			if err != nil {
				if p != nil || !apperr.IsKind(err, apperr.KindInvalid) {
					t.Fatalf("recusa fora do padrão para %q: %+v %v", c, p, err)
				}
				continue
			}
			if c == "" {
				if p != nil {
					t.Fatalf("cursor vazio com posição: %+v", p)
				}
				continue
			}
			if p == nil || len(c) > maxCursorLength || encodeCursor(kind, *p) != c {
				t.Fatalf("cursor aceito fora da forma canônica: %q %+v", c, p)
			}
			other := cursorLinks
			if kind == cursorLinks {
				other = cursorInvoices
			}
			if q, err := parseCursor(other, c); err == nil || q != nil {
				t.Fatalf("cursor de %s aceito como %s: %q", kind, other, c)
			}
		}
	})
}
