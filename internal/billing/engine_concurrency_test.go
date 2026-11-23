package billing

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// concurrentStore registra quantos tenants estão ao mesmo tempo dentro de
// uma transação do motor. Cada transação reivindica no máximo uma assinatura
// por tenant (as do fake não existem: nada é faturado) e espera até que
// `barrier` tenants estejam dentro ou o prazo acabe.
type concurrentStore struct {
	tenants []uuid.UUID
	fail    map[uuid.UUID]error
	barrier int

	mu      sync.Mutex
	inside  int
	peak    int
	visited []uuid.UUID
	enough  chan struct{}
}

func (s *concurrentStore) TenantsWithDueBilling(context.Context, time.Time, int) ([]uuid.UUID, error) {
	return s.tenants, nil
}

func (s *concurrentStore) InCycleTx(ctx context.Context, tenantID uuid.UUID, fn func(CycleTx) error) error {
	s.mu.Lock()
	s.inside++
	s.peak = max(s.peak, s.inside)
	s.visited = append(s.visited, tenantID)
	if s.peak >= s.barrier {
		select {
		case <-s.enough:
		default:
			close(s.enough)
		}
	}
	s.mu.Unlock()
	select {
	case <-s.enough:
	case <-time.After(2 * time.Second):
	}
	defer func() {
		s.mu.Lock()
		s.inside--
		s.mu.Unlock()
	}()
	if err := s.fail[tenantID]; err != nil {
		return err
	}
	return fn(idleTx{})
}

// idleTx não tem assinaturas vencidas.
type idleTx struct{ CycleTx }

func (idleTx) ClaimDueSubscription(context.Context, time.Time, []uuid.UUID) (subscriptions.Subscription, bool, error) {
	return subscriptions.Subscription{}, false, nil
}

func newConcurrentStore(n, barrier int) *concurrentStore {
	s := &concurrentStore{fail: map[uuid.UUID]error{}, barrier: barrier, enough: make(chan struct{})}
	for range n {
		s.tenants = append(s.tenants, uuid.New())
	}
	return s
}

func TestEngineRunAllConcurrency(t *testing.T) {
	cases := []struct {
		name        string
		concurrency int
		tenants     int
		wantPeak    int
	}{
		{"em sequência", 1, 5, 1},
		{"zero equivale a um", 0, 3, 1},
		{"quatro ao mesmo tempo", 4, 10, 4},
		{"menos tenants que a concorrência", 8, 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newConcurrentStore(tc.tenants, tc.wantPeak)
			e := NewEngine(store, clock.NewManual(engineNow))
			e.Concurrency = tc.concurrency
			if _, err := e.RunAll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if store.peak != tc.wantPeak {
				t.Errorf("tenants ao mesmo tempo = %d, esperado %d", store.peak, tc.wantPeak)
			}
			if len(store.visited) != tc.tenants {
				t.Errorf("tenants processados = %d, esperado %d", len(store.visited), tc.tenants)
			}
		})
	}
}

// A falha de um tenant na execução paralela volta como erro que identifica o
// tenant, e o motor continua utilizável. Quantos tenants começam antes de a
// falha ser registrada depende do escalonamento das goroutines, então o
// teste não conta tenants iniciados (a interrupção sem paralelismo é
// conferida em TestEngineRunAllStopsAtFailingTenant).
func TestEngineRunAllConcurrentFailure(t *testing.T) {
	store := newConcurrentStore(20, 2)
	boom := errors.New("banco indisponível")
	failing := store.tenants[1]
	store.fail[failing] = boom
	e := NewEngine(store, clock.NewManual(engineNow))
	e.Concurrency = 2
	_, err := e.RunAll(context.Background())
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), failing.String()) {
		t.Fatalf("erro deve identificar o tenant: %v", err)
	}
	if !slices.Contains(store.visited, failing) {
		t.Fatal("o tenant com falha foi processado")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.RunAll(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelamento: %v", err)
	}
}
