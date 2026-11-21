package portal

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

// Tamanho das páginas das listagens do portal e dos links.
const (
	DefaultPageSize = 25
	MaxPageSize     = 100
)

// PageQuery é o pedido de uma página: limit itens depois do cursor
// devolvido pela página anterior (vazio na primeira).
type PageQuery struct {
	Limit  int
	Cursor string
}

func pageLimit(n int) int {
	if n <= 0 {
		return DefaultPageSize
	}
	return min(n, MaxPageSize)
}

// Position é a posição do último item de uma página em uma listagem ordenada
// por (instante, identificador) decrescentes. É só uma posição: o tenant e o
// cliente consultados vêm sempre da sessão ou do principal, nunca do cursor.
type Position struct {
	At time.Time
	ID uuid.UUID
}

// Tipos de cursor: o cursor de uma listagem não é aceito em outra.
const (
	cursorInvoices = "pi"
	cursorLinks    = "pl"
)

// maxCursorLength supera com folga o maior cursor gerado e limita o que é
// decodificado de uma entrada do cliente.
const maxCursorLength = 160

var errInvalidCursor = apperr.Invalid("invalid_cursor", "Cursor de paginação inválido.")

// encodeCursor codifica a posição de forma opaca. O instante é truncado a
// microssegundos, a precisão do armazenamento.
func encodeCursor(kind string, p Position) string {
	raw := kind + "|" + p.At.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano) + "|" + p.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// parseCursor decodifica um cursor do tipo indicado; vazio significa a
// primeira página. Só é aceita a forma exata gerada por encodeCursor.
func parseCursor(kind, c string) (*Position, error) {
	if c == "" {
		return nil, nil
	}
	if len(c) > maxCursorLength {
		return nil, errInvalidCursor
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(c)
	if err != nil {
		return nil, errInvalidCursor
	}
	parts := strings.Split(string(b), "|")
	if len(parts) != 3 || parts[0] != kind {
		return nil, errInvalidCursor
	}
	at, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return nil, errInvalidCursor
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return nil, errInvalidCursor
	}
	p := Position{At: at.UTC(), ID: id}
	if encodeCursor(kind, p) != c {
		return nil, errInvalidCursor
	}
	return &p, nil
}
