package tenantscan

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// tenants gera n IDs em ordem crescente de bytes.
func tenants(n int) []uuid.UUID {
	out := make([]uuid.UUID, n)
	for i := range out {
		out[i][15] = byte(i + 1)
	}
	return out
}

// source simula a consulta paginada e registra as chamadas.
type source struct {
	ids   []uuid.UUID
	calls int
	err   error
}

func (s *source) page(_ context.Context, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	var out []uuid.UUID
	for _, id := range s.ids {
		if after != nil && bytes.Compare(id[:], after[:]) <= 0 {
			continue
		}
		out = append(out, id)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func collect(t *testing.T, r *Rotation, s *source, pageSize int, more func() bool) []uuid.UUID {
	t.Helper()
	var got []uuid.UUID
	if err := r.Each(context.Background(), pageSize, s.page, more, func(id uuid.UUID) { got = append(got, id) }); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestEachVisitsEveryPageOnce(t *testing.T) {
	ids := tenants(7)
	cases := []struct {
		name     string
		pageSize int
		calls    int
	}{
		{"páginas incompletas", 3, 3},
		{"última página cheia", 7, 2},
		{"tamanho padrão", 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &source{ids: ids}
			var r Rotation
			got := collect(t, &r, s, c.pageSize, nil)
			if !slices.Equal(got, ids) || s.calls != c.calls {
				t.Fatalf("visitados %v em %d consultas", got, s.calls)
			}
			if p := r.Position(); p == nil || *p != ids[6] {
				t.Fatalf("posição: %v", p)
			}
		})
	}
}

// Uma execução interrompida pelo orçamento é retomada na seguinte, a partir
// do tenant seguinte ao último visitado, com a volta até ele.
func TestEachRotatesAfterBudget(t *testing.T) {
	ids := tenants(5)
	s := &source{ids: ids}
	var r Rotation
	if r.Position() != nil {
		t.Fatal("posição inicial deve ser nula")
	}
	budget := 2
	more := func() bool { budget--; return budget >= 0 }
	if got := collect(t, &r, s, 2, more); !slices.Equal(got, ids[:2]) {
		t.Fatalf("primeira execução: %v", got)
	}
	want := append(slices.Clone(ids[2:]), ids[:2]...)
	if got := collect(t, &r, s, 2, nil); !slices.Equal(got, want) {
		t.Fatalf("segunda execução deve continuar e dar a volta: %v", got)
	}
	// A posição é o último tenant visitado (ids[1]); um tenant novo anterior a
	// ela entra na volta, e nenhum se repete.
	s.ids = append([]uuid.UUID{uuid.Nil}, ids...)
	want = append(slices.Clone(ids[2:]), uuid.Nil, ids[0], ids[1])
	if got := collect(t, &r, s, 2, nil); !slices.Equal(got, want) {
		t.Fatalf("terceira execução: %v", got)
	}
}

func TestEachStopsOnErrors(t *testing.T) {
	s := &source{ids: tenants(3), err: errors.New("falha na descoberta")}
	var r Rotation
	if err := r.Each(context.Background(), 2, s.page, nil, func(uuid.UUID) {}); !errors.Is(err, s.err) {
		t.Fatalf("erro da descoberta: %v", err)
	}
	s.err = nil
	ctx, cancel := context.WithCancel(context.Background())
	visited := 0
	err := r.Each(ctx, 2, s.page, nil, func(uuid.UUID) {
		visited++
		cancel()
	})
	if !errors.Is(err, context.Canceled) || visited != 1 {
		t.Fatalf("cancelamento: %v (%d visitados)", err, visited)
	}
	if p := r.Position(); p == nil || *p != s.ids[0] {
		t.Fatalf("posição após o cancelamento: %v", p)
	}
}
