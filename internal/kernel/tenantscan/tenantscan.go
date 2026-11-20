// Package tenantscan percorre, em páginas ordenadas por ID, os tenants com
// trabalho pendente descobertos pelos jobs do worker.
//
// A descoberta é paginada (cursor pelo último ID devolvido), de modo que
// nenhum limite fixo deixa tenants de fora. Cada execução começa depois do
// último tenant visitado na execução anterior e dá a volta até ele
// (rodízio): quando uma execução é interrompida pelo tempo limite ou pelo
// orçamento, a seguinte continua de onde ela parou, e nenhum tenant fica
// sempre no fim da fila.
package tenantscan

import (
	"bytes"
	"context"
	"sync"

	"github.com/google/uuid"
)

// PageSize é o tamanho padrão das páginas de descoberta.
const PageSize = 200

// Page devolve até limit IDs de tenant maiores que after (nil: desde o
// início), em ordem crescente na ordenação do tipo uuid do PostgreSQL (a
// mesma da comparação de bytes).
type Page func(ctx context.Context, after *uuid.UUID, limit int) ([]uuid.UUID, error)

// Rotation guarda a posição do rodízio entre execuções. O valor zero começa
// pelo primeiro tenant. É seguro para uso concorrente.
type Rotation struct {
	mu   sync.Mutex
	last *uuid.UUID
}

// Position devolve o último tenant visitado (nil antes da primeira visita).
func (r *Rotation) Position() *uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		return nil
	}
	id := *r.last
	return &id
}

func (r *Rotation) advance(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = &id
}

type pass struct{ after, until *uuid.UUID }

// Each visita cada tenant descoberto uma única vez: primeiro os posteriores
// à posição do rodízio e depois, desde o início, os demais até ela
// (inclusive). Antes de cada tenant verifica o contexto, cujo erro encerra a
// execução, e o orçamento: more (opcional) devolve false para encerrar sem
// erro. pageSize <= 0 usa PageSize. A posição avança a cada tenant visitado.
func (r *Rotation) Each(ctx context.Context, pageSize int, page Page, more func() bool, visit func(uuid.UUID)) error {
	if pageSize <= 0 {
		pageSize = PageSize
	}
	start := r.Position()
	passes := []pass{{after: start}}
	if start != nil {
		passes = append(passes, pass{until: start})
	}
	for _, p := range passes {
		after := p.after
		for {
			ids, err := page(ctx, after, pageSize)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if p.until != nil && bytes.Compare(id[:], p.until[:]) > 0 {
					return nil
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if more != nil && !more() {
					return nil
				}
				visit(id)
				r.advance(id)
			}
			if len(ids) < pageSize {
				break
			}
			after = &ids[len(ids)-1]
		}
	}
	return nil
}
