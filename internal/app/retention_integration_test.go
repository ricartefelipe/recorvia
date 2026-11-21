//go:build integration

package app

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/retention"
	"github.com/ricartefelipe/recorvia/internal/scheduler"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// A tarefa retention.purge do worker é reivindicada com lease em
// scheduled_jobs (uma instância por vez), remove os registros vencidos pelos
// prazos configurados, alimenta as métricas por tipo e fica agendada para a
// hora seguinte.
func TestWorkerRetentionJobRunsUnderLeaseAndFeedsMetrics(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Now().UTC().Truncate(time.Microsecond)
	env := fixture.New(t, 8, start)
	owner := pgtest.OwnerPool(t)
	tn := env.NewTenant(t, "worker-retencao")

	cfg := workerConfig(t, map[string]string{"RECORVIA_RETENTION_INBOX_DAYS": "30", "RECORVIA_RETENTION_BATCH_SIZE": "10"})
	if err := cfg.ValidateWorker(); err != nil {
		t.Fatal(err)
	}
	purge, err := retention.NewService(env.Store.Retention(), env.Clock, RetentionPolicy(cfg), log)
	if err != nil {
		t.Fatal(err)
	}
	// 25 registros vencidos pelo prazo configurado (30 dias) e 3 recentes.
	insert := func(n int, processedAt time.Time) {
		t.Helper()
		for range n {
			if _, err := owner.Exec(ctx, `INSERT INTO inbox_messages (tenant_id, consumer, message_id, processed_at) VALUES ($1, 'retencao.worker', $2, $3)`,
				tn.ID, ids.New(), processedAt); err != nil {
				t.Fatal(err)
			}
		}
	}
	insert(25, start.AddDate(0, 0, -31))
	insert(3, start.AddDate(0, 0, -29))
	remaining := func() int {
		t.Helper()
		var n int
		err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE tenant_id = $1 AND consumer = 'retencao.worker'`, tn.ID).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	metrics := telemetry.NewMetrics("worker")
	w := &Worker{Core: &Core{Store: env.Store, Clock: env.Clock, Log: log, Retention: purge}, Metrics: metrics}
	var job scheduler.Job
	for _, j := range w.Jobs() {
		if j.Name == JobRetention {
			job = j
		}
	}
	if job.Run == nil {
		t.Fatal("tarefa retention.purge não registrada")
	}
	// A execução é contada e alongada até as duas instâncias terem disputado
	// a tarefa, sem esperas por tempo.
	var runs atomic.Int32
	claimed := make(chan struct{})
	release := make(chan struct{})
	inner := job.Run
	job.Run = func(ctx context.Context) error {
		runs.Add(1)
		close(claimed)
		<-release
		return inner(ctx)
	}
	w.initMetrics([]scheduler.Job{job})
	newScheduler := func(id string) *scheduler.Scheduler {
		s, err := scheduler.New(schedulerStore{env.Store}, id, time.Second, env.Clock, log, metrics, job)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if err := env.Store.EnsureJob(ctx, JobRetention, job.Interval); err != nil {
		t.Fatal(err)
	}
	// O agendamento pode existir de outro teste do pacote: vence agora, sem lease.
	if _, err := owner.Exec(ctx, `UPDATE scheduled_jobs SET next_run_at = $2, locked_by = NULL, locked_until = NULL, run_count = 0 WHERE name = $1`,
		JobRetention, start); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	first := make(chan bool, 1)
	wg.Go(func() {
		ran, err := newScheduler("worker-a").RunOnce(ctx)
		if err != nil {
			t.Errorf("primeira instância: %v", err)
		}
		first <- ran
	})
	<-claimed
	// Com a tarefa em execução na primeira instância, a segunda não a reivindica.
	if ran, err := newScheduler("worker-b").RunOnce(ctx); err != nil || ran {
		t.Fatalf("segunda instância executou a tarefa em andamento: %v %v", ran, err)
	}
	close(release)
	wg.Wait()
	if !<-first || runs.Load() != 1 {
		t.Fatalf("execuções da tarefa: %d", runs.Load())
	}

	if n := remaining(); n != 3 {
		t.Fatalf("registros restantes do inbox: %d, esperado 3 (os recentes)", n)
	}
	if v, ok := metricValue(t, metrics, "recorvia_retention_purged_total", "kind", "inbox_messages"); !ok || v < 25 {
		t.Fatalf("recorvia_retention_purged_total{kind=inbox_messages} = %v", v)
	}
	if v, ok := metricValue(t, metrics, "recorvia_retention_purge_failures_total", "kind", "inbox_messages"); !ok || v != 0 {
		t.Fatalf("falhas do expurgo: %v", v)
	}
	if v := testutil.ToFloat64(metrics.JobRuns.WithLabelValues(JobRetention, "success")); v != 1 {
		t.Fatalf("execuções com sucesso registradas: %v", v)
	}
	if v := testutil.ToFloat64(metrics.JobRuns.WithLabelValues(JobRetention, "failure")); v != 0 {
		t.Fatalf("execuções com falha registradas: %v", v)
	}
	if _, ok := metricValue(t, metrics, "recorvia_job_last_success_timestamp_seconds", "task", JobRetention); !ok {
		t.Fatal("último sucesso da tarefa não registrado")
	}
	// Lease liberado e próxima execução uma hora depois: nada a reivindicar antes disso.
	var lockedBy *string
	var nextRun time.Time
	var runCount int
	var lastError *string
	if err := owner.QueryRow(ctx, `SELECT locked_by, next_run_at, run_count, last_error FROM scheduled_jobs WHERE name = $1`, JobRetention).
		Scan(&lockedBy, &nextRun, &runCount, &lastError); err != nil {
		t.Fatal(err)
	}
	if lockedBy != nil || !nextRun.Equal(start.Add(time.Hour)) || runCount != 1 || lastError != nil {
		t.Fatalf("agendamento depois da execução: lease=%v próxima=%s execuções=%d erro=%v", lockedBy, nextRun, runCount, lastError)
	}
	if ran, err := newScheduler("worker-b").RunOnce(ctx); err != nil || ran {
		t.Fatalf("tarefa executada antes do intervalo: %v %v", ran, err)
	}
	// Na hora seguinte a tarefa volta a ser executada (e não há mais nada vencido).
	env.Clock.Advance(time.Hour)
	job.Run = inner
	if ran, err := newScheduler("worker-b").RunOnce(ctx); err != nil || !ran {
		t.Fatalf("execução na hora seguinte: %v %v", ran, err)
	}
	if n := remaining(); n != 3 {
		t.Fatalf("a segunda execução removeu registros dentro do prazo: %d", n)
	}
}
