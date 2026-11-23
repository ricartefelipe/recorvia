package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestStallGuard(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	g := &stallGuard[int]{limit: 10 * time.Minute, now: func() time.Time { return now }}
	steps := []struct {
		advance time.Duration
		value   int
		stalled bool
	}{
		{0, 1, false},
		{9 * time.Minute, 1, false},
		{2 * time.Minute, 2, false}, // mudou: o prazo recomeça
		{10 * time.Minute, 2, false},
		{time.Second, 2, true},
	}
	for i, s := range steps {
		now = now.Add(s.advance)
		err := g.check(s.value)
		if (err != nil) != s.stalled {
			t.Fatalf("passo %d: erro %v, esperado parado=%v", i, err, s.stalled)
		}
	}
}

func TestParallelRunsAllAndReturnsFirstError(t *testing.T) {
	boom := errors.New("falha")
	var calls atomic.Int64
	err := parallel(context.Background(), nil, "teste", 4, 50, func(_ context.Context, i int) error {
		calls.Add(1)
		if i == 7 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("erro %v, esperado %v", err, boom)
	}
	if calls.Load() != 50 {
		t.Fatalf("%d chamadas, esperadas 50", calls.Load())
	}
}

func TestParallelStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := parallel(ctx, nil, "teste", 2, 10, func(context.Context, int) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("erro %v, esperado context.Canceled", err)
	}
}
