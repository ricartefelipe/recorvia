// Package scheduler executa tarefas periódicas com segurança em múltiplas
// instâncias: a próxima execução e o lease ficam no PostgreSQL
// (scheduled_jobs); cada instância reivindica tarefas vencidas com
// FOR UPDATE SKIP LOCKED. Não há cron em memória como única garantia: se o
// worker ficar indisponível, a tarefa vencida é executada na volta e as
// próprias tarefas recuperam o atraso (catch-up).
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ricartefelipe/recorvia/internal/infra/logging"
)

// Store persiste o estado das tarefas.
type Store interface {
	EnsureJob(ctx context.Context, name string, interval time.Duration) error
	ClaimJob(ctx context.Context, worker string, lease time.Duration) (name string, ok bool, err error)
	CompleteJob(ctx context.Context, worker, name string, next time.Time, runErr error) error
}

// ErrMoreWork é devolvido por uma tarefa que parou num limite próprio de
// trabalho por execução com trabalho ainda pendente (por exemplo, o motor de
// recorrência no limite de assinaturas por tenant). Não é falha: a tarefa
// volta a vencer depois de MoreWorkDelay, em vez de esperar o intervalo, e
// as outras tarefas já vencidas são reivindicadas antes dela (ordem de
// vencimento). Depois de MaxMoreWorkRuns execuções seguidas com trabalho
// pendente nesta instância, a tarefa espera o intervalo normal: uma tarefa
// que avisa trabalho pendente sem avançar não pode rodar sem parar.
var ErrMoreWork = errors.New("scheduler: há mais trabalho pendente")

const (
	// MoreWorkDelay é a espera mínima antes de repetir uma tarefa que
	// devolveu ErrMoreWork.
	MoreWorkDelay = 5 * time.Second
	// MaxMoreWorkRuns é o número máximo de repetições seguidas de uma tarefa
	// por ErrMoreWork antes de voltar ao intervalo normal.
	MaxMoreWorkRuns = 20
)

// Job é uma tarefa periódica idempotente.
type Job struct {
	Name     string
	Interval time.Duration
	Timeout  time.Duration
	Run      func(ctx context.Context) error
}

// Observer recebe sinais para métricas.
type Observer interface {
	JobFinished(name string, d time.Duration, err error)
}

// Clock é o relógio do agendador.
type Clock interface{ Now() time.Time }

// Scheduler coordena as tarefas.
type Scheduler struct {
	store  Store
	jobs   map[string]Job
	worker string
	tick   time.Duration
	log    *slog.Logger
	obs    Observer
	clock  Clock

	mu sync.Mutex
	// moreRuns conta, por tarefa, as repetições seguidas por ErrMoreWork.
	moreRuns map[string]int
}

// New cria o agendador.
func New(store Store, worker string, tick time.Duration, c Clock, log *slog.Logger, obs Observer, jobs ...Job) (*Scheduler, error) {
	m := map[string]Job{}
	for _, j := range jobs {
		if j.Name == "" || j.Interval <= 0 || j.Run == nil {
			return nil, fmt.Errorf("scheduler: tarefa inválida %q", j.Name)
		}
		if j.Timeout <= 0 {
			j.Timeout = 5 * time.Minute
		}
		m[j.Name] = j
	}
	return &Scheduler{store: store, jobs: m, worker: worker, tick: tick, log: log.With("component", "scheduler"), obs: obs, clock: c,
		moreRuns: map[string]int{}}, nil
}

// Run registra as tarefas e executa o laço de reivindicação até ctx terminar.
func (s *Scheduler) Run(ctx context.Context) error {
	for _, j := range s.jobs {
		if err := s.store.EnsureJob(ctx, j.Name, j.Interval); err != nil {
			return err
		}
	}
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		for ctx.Err() == nil {
			ran, err := s.RunOnce(ctx)
			if err != nil {
				s.log.WarnContext(ctx, "reivindicação de tarefa falhou", slog.String("error", err.Error()))
				break
			}
			if !ran {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// RunOnce reivindica e executa no máximo uma tarefa vencida.
func (s *Scheduler) RunOnce(ctx context.Context) (bool, error) {
	// O lease cobre o maior timeout configurado com folga.
	var lease time.Duration
	for _, j := range s.jobs {
		lease = max(lease, j.Timeout+time.Minute)
	}
	name, ok, err := s.store.ClaimJob(ctx, s.worker, lease)
	if err != nil || !ok {
		return false, err
	}
	job, known := s.jobs[name]
	if !known {
		// Tarefa registrada por outra versão do worker: devolve sem executar.
		return true, s.store.CompleteJob(ctx, s.worker, name, s.clock.Now().Add(time.Minute), errors.New("tarefa desconhecida nesta versão"))
	}
	start := time.Now()
	// O nome da tarefa entra no contexto: todos os logs emitidos durante a
	// execução (inclusive pelos casos de uso) levam o atributo job.
	ctx = logging.WithAttrs(ctx, slog.String("job", name))
	jctx, cancel := context.WithTimeout(ctx, job.Timeout)
	runErr := safeRun(jctx, job)
	cancel()
	next := s.clock.Now().Add(job.Interval)
	if errors.Is(runErr, ErrMoreWork) {
		runErr = nil
		if s.countMoreWork(name) {
			next = s.clock.Now().Add(min(MoreWorkDelay, job.Interval))
		} else {
			s.log.WarnContext(ctx, "tarefa com trabalho pendente repetida no limite; volta ao intervalo normal",
				slog.Int("repeticoes", MaxMoreWorkRuns))
		}
	} else {
		s.resetMoreWork(name)
	}
	s.obs.JobFinished(name, time.Since(start), runErr)
	if runErr != nil {
		s.log.ErrorContext(ctx, "tarefa falhou", slog.String("error", runErr.Error()))
	}
	return true, s.store.CompleteJob(context.WithoutCancel(ctx), s.worker, name, next, runErr)
}

// countMoreWork registra uma repetição por ErrMoreWork e informa se ainda
// cabe repetir antes do intervalo; no limite, zera a contagem.
func (s *Scheduler) countMoreWork(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moreRuns[name]++
	if s.moreRuns[name] > MaxMoreWorkRuns {
		delete(s.moreRuns, name)
		return false
	}
	return true
}

func (s *Scheduler) resetMoreWork(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.moreRuns, name)
}

// safeRun converte pânicos inesperados da tarefa em erro, para que o lease
// seja liberado e a tarefa volte a ser agendada.
func safeRun(ctx context.Context, job Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("scheduler: pânico na tarefa %s: %v", job.Name, r)
		}
	}()
	return job.Run(ctx)
}
