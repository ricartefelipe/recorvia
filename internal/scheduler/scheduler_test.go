package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

var baseTime = time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)

type completion struct {
	worker string
	name   string
	next   time.Time
	runErr error
	ctxErr error
}

type claim struct {
	worker string
	lease  time.Duration
}

// fakeStore simula a tabela scheduled_jobs: devolve, em ordem, os nomes
// enfileirados em due e registra reivindicações e conclusões.
type fakeStore struct {
	mu          sync.Mutex
	due         []string
	claimErrs   []error
	ensured     map[string]time.Duration
	ensureErr   error
	claims      []claim
	completions []completion
	completeErr error
	// claimed recebe um sinal a cada reivindicação (opcional).
	claimed chan struct{}
}

func newFakeStore(due ...string) *fakeStore {
	return &fakeStore{due: due, ensured: map[string]time.Duration{}}
}

func (s *fakeStore) EnsureJob(_ context.Context, name string, interval time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ensureErr != nil {
		return s.ensureErr
	}
	s.ensured[name] = interval
	return nil
}

func (s *fakeStore) ClaimJob(_ context.Context, worker string, lease time.Duration) (string, bool, error) {
	s.mu.Lock()
	s.claims = append(s.claims, claim{worker, lease})
	var (
		name string
		ok   bool
		err  error
	)
	switch {
	case len(s.claimErrs) > 0:
		err, s.claimErrs = s.claimErrs[0], s.claimErrs[1:]
	case len(s.due) > 0:
		name, s.due, ok = s.due[0], s.due[1:], true
	}
	ch := s.claimed
	s.mu.Unlock()
	if ch != nil {
		ch <- struct{}{}
	}
	return name, ok, err
}

func (s *fakeStore) CompleteJob(ctx context.Context, worker, name string, next time.Time, runErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completions = append(s.completions, completion{worker, name, next, runErr, ctx.Err()})
	return s.completeErr
}

func (s *fakeStore) snapshot() ([]claim, []completion) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]claim(nil), s.claims...), append([]completion(nil), s.completions...)
}

type finished struct {
	name string
	err  error
}

type fakeObserver struct {
	mu   sync.Mutex
	runs []finished
}

func (o *fakeObserver) JobFinished(name string, _ time.Duration, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.runs = append(o.runs, finished{name, err})
}

func (o *fakeObserver) snapshot() []finished {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]finished(nil), o.runs...)
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func newScheduler(t *testing.T, store Store, c Clock, obs Observer, jobs ...Job) *Scheduler {
	t.Helper()
	s, err := New(store, "worker-1", time.Second, c, discardLogger(), obs, jobs...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func noop(context.Context) error { return nil }

func TestNewRejectsInvalidJob(t *testing.T) {
	cases := []struct {
		name string
		job  Job
	}{
		{"sem nome", Job{Interval: time.Hour, Run: noop}},
		{"intervalo zero", Job{Name: "faturamento", Run: noop}},
		{"intervalo negativo", Job{Name: "faturamento", Interval: -time.Minute, Run: noop}},
		{"sem função", Job{Name: "faturamento", Interval: time.Hour}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := New(newFakeStore(), "w", time.Second, clock.NewManual(baseTime), discardLogger(), &fakeObserver{},
				Job{Name: "valida", Interval: time.Hour, Run: noop}, c.job)
			if err == nil || s != nil {
				t.Fatalf("tarefa inválida deveria ser rejeitada: s=%v err=%v", s, err)
			}
			if !strings.Contains(err.Error(), "tarefa inválida") {
				t.Fatalf("mensagem inesperada: %v", err)
			}
		})
	}
}

func TestRunOnceExecutesDueJobAndSchedulesNext(t *testing.T) {
	store := newFakeStore("faturamento")
	obs := &fakeObserver{}
	clk := clock.NewManual(baseTime)
	runs := 0
	s := newScheduler(t, store, clk, obs, Job{Name: "faturamento", Interval: 15 * time.Minute, Run: func(ctx context.Context) error {
		runs++
		return nil
	}})

	ran, err := s.RunOnce(context.Background())
	if err != nil || !ran {
		t.Fatalf("RunOnce: ran=%v err=%v", ran, err)
	}
	if runs != 1 {
		t.Fatalf("tarefa executada %d vezes", runs)
	}
	claims, completions := store.snapshot()
	if len(claims) != 1 || claims[0].worker != "worker-1" {
		t.Fatalf("reivindicação inesperada: %+v", claims)
	}
	if len(completions) != 1 {
		t.Fatalf("o lease deve ser liberado uma vez, obtive %d conclusões", len(completions))
	}
	c := completions[0]
	if c.name != "faturamento" || c.worker != "worker-1" || c.runErr != nil {
		t.Fatalf("conclusão inesperada: %+v", c)
	}
	if want := baseTime.Add(15 * time.Minute); !c.next.Equal(want) {
		t.Fatalf("próxima execução %v, esperada %v", c.next, want)
	}
	if got := obs.snapshot(); len(got) != 1 || got[0].name != "faturamento" || got[0].err != nil {
		t.Fatalf("observador: %+v", got)
	}
}

func TestRunOnceWithoutDueJob(t *testing.T) {
	store := newFakeStore()
	obs := &fakeObserver{}
	s := newScheduler(t, store, clock.NewManual(baseTime), obs, Job{Name: "faturamento", Interval: time.Hour, Run: func(context.Context) error {
		t.Fatal("nenhuma tarefa vencida deveria ser executada")
		return nil
	}})
	ran, err := s.RunOnce(context.Background())
	if err != nil || ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if _, completions := store.snapshot(); len(completions) != 0 {
		t.Fatalf("nada a liberar: %+v", completions)
	}
	if len(obs.snapshot()) != 0 {
		t.Fatal("observador não deveria ser chamado")
	}
}

func TestRunOnceClaimError(t *testing.T) {
	store := newFakeStore("faturamento")
	boom := errors.New("banco indisponível")
	store.claimErrs = []error{boom}
	s := newScheduler(t, store, clock.NewManual(baseTime), &fakeObserver{}, Job{Name: "faturamento", Interval: time.Hour, Run: noop})
	ran, err := s.RunOnce(context.Background())
	if ran || !errors.Is(err, boom) {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
}

func TestRunOnceLeaseCoversLongestTimeout(t *testing.T) {
	store := newFakeStore()
	s := newScheduler(t, store, clock.NewManual(baseTime), &fakeObserver{},
		Job{Name: "curta", Interval: time.Hour, Timeout: 2 * time.Minute, Run: noop},
		Job{Name: "longa", Interval: time.Hour, Timeout: 10 * time.Minute, Run: noop},
		Job{Name: "padrao", Interval: time.Hour, Run: noop}, // timeout padrão de 5 minutos
	)
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	claims, _ := store.snapshot()
	if len(claims) != 1 || claims[0].lease != 11*time.Minute {
		t.Fatalf("lease deve cobrir o maior timeout com folga de 1 minuto: %+v", claims)
	}
}

func TestRunOnceDefaultTimeoutIsFiveMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore("padrao")
		var deadline time.Time
		var start time.Time
		s := newScheduler(t, store, clock.NewManual(baseTime), &fakeObserver{}, Job{Name: "padrao", Interval: time.Hour, Run: func(ctx context.Context) error {
			deadline, _ = ctx.Deadline()
			return nil
		}})
		start = time.Now()
		if _, err := s.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := deadline.Sub(start); got != 5*time.Minute {
			t.Fatalf("timeout padrão %v, esperado 5m", got)
		}
		claims, _ := store.snapshot()
		if claims[0].lease != 6*time.Minute {
			t.Fatalf("lease %v, esperado 6m", claims[0].lease)
		}
	})
}

func TestRunOnceJobErrorIsRecordedAndRescheduled(t *testing.T) {
	store := newFakeStore("conciliacao")
	obs := &fakeObserver{}
	boom := errors.New("provedor fora do ar")
	s := newScheduler(t, store, clock.NewManual(baseTime), obs, Job{Name: "conciliacao", Interval: time.Hour, Run: func(context.Context) error {
		return boom
	}})
	ran, err := s.RunOnce(context.Background())
	if err != nil || !ran {
		t.Fatalf("falha da tarefa não é falha do agendador: ran=%v err=%v", ran, err)
	}
	_, completions := store.snapshot()
	if len(completions) != 1 || !errors.Is(completions[0].runErr, boom) {
		t.Fatalf("erro da tarefa deve ser registrado na conclusão: %+v", completions)
	}
	if !completions[0].next.Equal(baseTime.Add(time.Hour)) {
		t.Fatalf("a tarefa com erro deve voltar ao intervalo normal: %v", completions[0].next)
	}
	if got := obs.snapshot(); len(got) != 1 || !errors.Is(got[0].err, boom) {
		t.Fatalf("observador deve receber o erro: %+v", got)
	}
}

func TestRunOncePanicBecomesErrorAndReleasesLease(t *testing.T) {
	store := newFakeStore("instavel")
	obs := &fakeObserver{}
	s := newScheduler(t, store, clock.NewManual(baseTime), obs, Job{Name: "instavel", Interval: 10 * time.Minute, Run: func(context.Context) error {
		panic("índice fora do intervalo")
	}})
	ran, err := s.RunOnce(context.Background())
	if err != nil || !ran {
		t.Fatalf("pânico não deve escapar do agendador: ran=%v err=%v", ran, err)
	}
	_, completions := store.snapshot()
	if len(completions) != 1 {
		t.Fatalf("lease deve ser liberado após pânico: %+v", completions)
	}
	c := completions[0]
	if c.runErr == nil || !strings.Contains(c.runErr.Error(), "pânico") || !strings.Contains(c.runErr.Error(), "instavel") {
		t.Fatalf("pânico deve virar erro identificado: %v", c.runErr)
	}
	if !c.next.Equal(baseTime.Add(10 * time.Minute)) {
		t.Fatalf("tarefa deve ser reagendada: %v", c.next)
	}
	if got := obs.snapshot(); len(got) != 1 || got[0].err == nil {
		t.Fatalf("observador deve registrar a falha: %+v", got)
	}
}

func TestRunOnceUnknownJobIsReturnedWithoutRunning(t *testing.T) {
	store := newFakeStore("tarefa_antiga")
	obs := &fakeObserver{}
	clk := clock.NewManual(baseTime)
	s := newScheduler(t, store, clk, obs, Job{Name: "faturamento", Interval: time.Hour, Run: func(context.Context) error {
		t.Fatal("tarefa conhecida não deveria rodar no lugar da desconhecida")
		return nil
	}})
	ran, err := s.RunOnce(context.Background())
	if err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	_, completions := store.snapshot()
	if len(completions) != 1 {
		t.Fatalf("tarefa desconhecida deve ser devolvida: %+v", completions)
	}
	c := completions[0]
	if c.name != "tarefa_antiga" || c.runErr == nil || !strings.Contains(c.runErr.Error(), "desconhecida") {
		t.Fatalf("devolução inesperada: %+v", c)
	}
	if !c.next.Equal(baseTime.Add(time.Minute)) {
		t.Fatalf("tarefa desconhecida volta em 1 minuto: %v", c.next)
	}
	if len(obs.snapshot()) != 0 {
		t.Fatal("tarefa não executada não gera métrica")
	}
}

func TestRunOnceAppliesTimeoutThroughContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore("lenta")
		obs := &fakeObserver{}
		var remaining time.Duration
		s := newScheduler(t, store, clock.NewManual(baseTime), obs, Job{Name: "lenta", Interval: time.Hour, Timeout: 30 * time.Second,
			Run: func(ctx context.Context) error {
				dl, ok := ctx.Deadline()
				if !ok {
					t.Error("a tarefa deve receber contexto com prazo")
				}
				remaining = time.Until(dl)
				<-ctx.Done() // trabalho que só termina pelo prazo
				return ctx.Err()
			}})
		start := time.Now()
		ran, err := s.RunOnce(context.Background())
		if err != nil || !ran {
			t.Fatalf("ran=%v err=%v", ran, err)
		}
		if remaining != 30*time.Second {
			t.Fatalf("prazo da tarefa %v, esperado 30s", remaining)
		}
		if elapsed := time.Since(start); elapsed != 30*time.Second {
			t.Fatalf("a tarefa deveria ser interrompida em 30s, durou %v", elapsed)
		}
		_, completions := store.snapshot()
		if len(completions) != 1 || !errors.Is(completions[0].runErr, context.DeadlineExceeded) {
			t.Fatalf("estouro de prazo deve ser registrado: %+v", completions)
		}
		if completions[0].ctxErr != nil {
			t.Fatalf("a conclusão não pode usar o contexto expirado da tarefa: %v", completions[0].ctxErr)
		}
	})
}

func TestRunOnceReleasesLeaseEvenWhenCallerContextIsCanceled(t *testing.T) {
	store := newFakeStore("faturamento")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newScheduler(t, store, clock.NewManual(baseTime), &fakeObserver{}, Job{Name: "faturamento", Interval: time.Hour, Run: func(jctx context.Context) error {
		cancel() // desligamento durante a execução
		<-jctx.Done()
		return jctx.Err()
	}})
	if _, err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	_, completions := store.snapshot()
	if len(completions) != 1 {
		t.Fatalf("conclusão ausente: %+v", completions)
	}
	if completions[0].ctxErr != nil {
		t.Fatalf("CompleteJob deve receber contexto sem cancelamento: %v", completions[0].ctxErr)
	}
	if !errors.Is(completions[0].runErr, context.Canceled) {
		t.Fatalf("erro da tarefa: %v", completions[0].runErr)
	}
}

func TestRunOnceReturnsCompleteError(t *testing.T) {
	store := newFakeStore("faturamento")
	boom := errors.New("falha ao gravar próxima execução")
	store.completeErr = boom
	s := newScheduler(t, store, clock.NewManual(baseTime), &fakeObserver{}, Job{Name: "faturamento", Interval: time.Hour, Run: noop})
	ran, err := s.RunOnce(context.Background())
	if !ran || !errors.Is(err, boom) {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
}

func TestRunRegistersJobsAndDrainsDueWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore("a", "b")
		store.claimed = make(chan struct{})
		obs := &fakeObserver{}
		var mu sync.Mutex
		executed := []string{}
		job := func(name string) Job {
			return Job{Name: name, Interval: time.Hour, Run: func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				executed = append(executed, name)
				return nil
			}}
		}
		s := newScheduler(t, store, clock.NewManual(baseTime), obs, job("a"), job("b"))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()

		// Primeiro ciclo: duas tarefas vencidas e uma reivindicação vazia, sem
		// esperar o tick entre elas.
		for range 3 {
			<-store.claimed
		}
		synctest.Wait()
		mu.Lock()
		if len(executed) != 2 {
			t.Fatalf("tarefas vencidas devem ser drenadas no mesmo ciclo: %v", executed)
		}
		mu.Unlock()
		store.mu.Lock()
		if len(store.ensured) != 2 || store.ensured["a"] != time.Hour {
			t.Fatalf("tarefas devem ser registradas antes do laço: %+v", store.ensured)
		}
		store.mu.Unlock()

		// Sem trabalho, a próxima reivindicação ocorre somente no tick.
		start := time.Now()
		<-store.claimed
		if waited := time.Since(start); waited != time.Second {
			t.Fatalf("nova reivindicação deveria ocorrer no tick de 1s, ocorreu após %v", waited)
		}

		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run deve terminar sem erro no cancelamento: %v", err)
		}
	})
}

func TestRunContinuesAfterClaimError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore("a")
		store.claimErrs = []error{errors.New("conexão perdida")}
		store.claimed = make(chan struct{})
		ran := make(chan struct{}, 1)
		s := newScheduler(t, store, clock.NewManual(baseTime), &fakeObserver{}, Job{Name: "a", Interval: time.Hour, Run: func(context.Context) error {
			ran <- struct{}{}
			return nil
		}})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()

		<-store.claimed // falha: aguarda o próximo tick
		<-store.claimed // tick seguinte: tarefa vencida
		<-ran
		<-store.claimed // sem mais trabalho
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestRunReturnsEnsureError(t *testing.T) {
	store := newFakeStore()
	boom := errors.New("sem permissão")
	store.ensureErr = boom
	s := newScheduler(t, store, clock.NewManual(baseTime), &fakeObserver{}, Job{Name: "a", Interval: time.Hour, Run: noop})
	if err := s.Run(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("esperava erro de registro, obtive %v", err)
	}
	if claims, _ := store.snapshot(); len(claims) != 0 {
		t.Fatal("não deve reivindicar sem registrar as tarefas")
	}
}
