// Package ids gera identificadores UUIDv7 (ordenáveis no tempo), o que
// melhora a localidade de índices e permite paginação estável por id.
package ids

import "github.com/google/uuid"

// New retorna um UUIDv7. A geração depende de crypto/rand, que a partir do
// Go 1.24 não retorna erro (falhas de entropia encerram o processo), por isso
// uuid.Must não representa um caminho de erro esperado.
func New() uuid.UUID { return uuid.Must(uuid.NewV7()) }

// Parse interpreta um UUID textual.
func Parse(s string) (uuid.UUID, error) { return uuid.Parse(s) }
