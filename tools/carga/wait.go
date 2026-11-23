package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// stallLimit é o tempo máximo sem nenhum andamento numa espera (itens
// concluídos ou contadores do banco). Padrão de 10 min; RECORVIA_LOAD_STALL
// muda (por exemplo 5m).
var stallLimit = 10 * time.Minute

func init() {
	if v := os.Getenv("RECORVIA_LOAD_STALL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			stallLimit = d
		}
	}
}

// stallGuard acusa uma espera em que o valor observado não muda há
// stallLimit.
type stallGuard[T comparable] struct {
	last    T
	changed time.Time
	started bool
	limit   time.Duration
	now     func() time.Time
}

func newStallGuard[T comparable]() *stallGuard[T] {
	return &stallGuard[T]{limit: stallLimit, now: time.Now}
}

func (g *stallGuard[T]) check(v T) error {
	now := g.now()
	if !g.started || v != g.last {
		g.last, g.changed, g.started = v, now, true
		return nil
	}
	if idle := now.Sub(g.changed); idle > g.limit {
		return fmt.Errorf("sem andamento há %s", idle.Round(time.Second))
	}
	return nil
}

// parallel executa fn para 0..n-1 com conc goroutines e devolve o primeiro
// erro (os demais itens continuam). Registra o andamento a cada 15 s e
// interrompe tudo se nenhum item terminar em stallLimit (cada chamada HTTP
// tem prazo próprio, mas novas tentativas somadas podem não ter).
func parallel(ctx context.Context, log *slog.Logger, what string, conc, n int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var (
		next     atomic.Int64
		done     atomic.Int64
		failed   atomic.Int64
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	stop := make(chan struct{})
	watch := make(chan struct{})
	go func() {
		defer close(watch)
		start := time.Now()
		stall := newStallGuard[int64]()
		for {
			select {
			case <-stop:
				return
			case <-time.After(15 * time.Second):
			}
			d := done.Load()
			if log != nil {
				log.Info(what, slog.Int64("concluidos", d), slog.Int("total", n), slog.Int64("com_erro", failed.Load()),
					slog.Duration("decorrido", time.Since(start).Round(time.Second)))
			}
			if err := stall.check(d); err != nil {
				cancel(fmt.Errorf("%s: %w (%d de %d)", what, err, d, n))
				return
			}
		}
	}()
	for range min(conc, n) {
		wg.Go(func() {
			for ctx.Err() == nil {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				err := fn(ctx, i)
				done.Add(1)
				if err != nil {
					failed.Add(1)
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	<-watch
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return firstErr
}

// watchClock compara, a cada 10 s, o relógio de parede com o monotônico. No
// Linux, o monotônico não anda com a máquina suspensa: uma diferença acima
// de 30 s indica suspensão (ou o processo parado), e os números medidos
// nesse intervalo não valem. Devolve o total acumulado.
func watchClock(ctx context.Context, log *slog.Logger) func() time.Duration {
	var lost atomic.Int64
	go func() {
		prev := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			now := time.Now()
			gap := now.Round(0).Sub(prev.Round(0)) - now.Sub(prev)
			prev = now
			if gap > 30*time.Second {
				lost.Add(int64(gap))
				log.Warn("o relógio de parede avançou além do monotônico: a máquina foi suspensa ou o processo ficou parado",
					slog.Duration("intervalo", gap.Round(time.Second)))
			}
		}
	}()
	return func() time.Duration { return time.Duration(lost.Load()) }
}
