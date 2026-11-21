//go:build integration

package pgstore_test

import (
	"context"
	"crypto/sha256"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/db"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/paysim"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
)

// hookedList chama hook antes de cada listagem de cobranças do provedor (a
// primeira chamada da coleta de uma conciliação).
type hookedList struct {
	payments.Gateway
	hook func(ctx context.Context) error
}

func (g hookedList) ListCharges(ctx context.Context, req payments.ListRequest) (payments.ListResult, error) {
	if err := g.hook(ctx); err != nil {
		return payments.ListResult{}, err
	}
	return g.Gateway.ListCharges(ctx, req)
}

// reconHarness é um cenário com uma divergência de taxa no simulador e um
// ponto de interceptação da coleta (onList).
type reconHarness struct {
	*payHarness
	lists  atomic.Int32
	onList atomic.Pointer[func(ctx context.Context) error]
}

func newReconHarness(t *testing.T) *reconHarness {
	t.Helper()
	r := &reconHarness{}
	r.payHarness = newPayHarnessWith(t, 2*time.Second, func(gw payments.Gateway) payments.Gateway {
		return hookedList{Gateway: gw, hook: func(ctx context.Context) error {
			r.lists.Add(1)
			if fn := r.onList.Load(); fn != nil {
				return (*fn)(ctx)
			}
			return nil
		}}
	})
	_, c := r.paidCharge(t, 10000)
	fee := int64(500)
	if _, err := r.sim.Apply(c.ProviderChargeID, paysim.Action{Name: "diverge", Fee: &fee}); err != nil {
		t.Fatal(err)
	}
	r.env.Clock.Advance(time.Minute)
	return r
}

func (r *reconHarness) setOnList(fn func(ctx context.Context) error) {
	if fn == nil {
		r.onList.Store(nil)
		return
	}
	r.onList.Store(&fn)
}

func (r *reconHarness) start(t *testing.T, key string) payments.ReconciliationRun {
	t.Helper()
	run, err := r.pay.StartReconciliation(context.Background(), r.tn.ID, r.tn.Actor, &r.tn.Owner.ID, payments.RunInput{RequestKey: key})
	if err != nil {
		t.Fatalf("StartReconciliation: %v", err)
	}
	return run
}

func (r *reconHarness) stored(t *testing.T, id uuid.UUID) payments.ReconciliationRun {
	t.Helper()
	run, err := r.pay.ReconciliationRun(context.Background(), r.tn.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// claimable informa se a execução aparece na descoberta entre tenants.
func (r *reconHarness) claimable(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	refs, err := r.env.Store.Payments().ClaimableReconciliationRuns(context.Background(), r.env.Clock.Now(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(refs, func(ref payments.RunRef) bool { return ref.RunID == id && ref.TenantID == r.tn.ID })
}

// assertSingleFeeDiscrepancy exige uma única divergência de taxa, detectada
// uma única vez, ligada à execução informada.
func (r *reconHarness) assertSingleFeeDiscrepancy(t *testing.T, runID uuid.UUID) {
	t.Helper()
	if n := r.sum(t, `SELECT count(*) FROM reconciliation_discrepancies WHERE tenant_id = $1`); n != 1 {
		t.Fatalf("divergências = %d", n)
	}
	if n := r.sum(t, `SELECT count(*) FROM reconciliation_discrepancies WHERE tenant_id = $1 AND kind = 'fee_mismatch' AND status = 'open'
	                    AND occurrences = 1 AND run_id = '`+runID.String()+`'`); n != 1 {
		t.Fatalf("divergência de taxa da execução, detectada uma vez = %d", n)
	}
	if n := r.sum(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'reconciliation.discrepancy_detected'`); n != 1 {
		t.Fatalf("eventos de divergência = %d", n)
	}
}

// O pedido manual registra a execução e devolve de imediato; o worker a
// descobre, assume com lease e conclui. A chave do pedido identifica sempre a
// mesma execução e há uma execução em andamento por conta.
func TestReconciliationRegisteredAndExecutedByWorker(t *testing.T) {
	ctx := context.Background()
	r := newReconHarness(t)
	run := r.start(t, "rec-assincrona")
	if run.Status != payments.RunRunning || run.LeaseUntil != nil || run.Attempts != 0 || r.lists.Load() != 0 {
		t.Fatalf("execução registrada sem chamar o provedor: %+v (listagens: %d)", run, r.lists.Load())
	}
	if !r.claimable(t, run.ID) {
		t.Fatal("execução registrada não aparece para o worker")
	}
	stats, err := r.env.Store.ReconciliationRunStats(ctx)
	if err != nil || stats.Queued < 1 || stats.OldestWaitingSeconds < 0 {
		t.Fatalf("métrica das execuções aguardando: %+v %v", stats, err)
	}
	if again := r.start(t, "rec-assincrona"); again.ID != run.ID {
		t.Fatalf("mesma chave: %s, esperado %s", again.ID, run.ID)
	}
	if _, err := r.pay.StartReconciliation(ctx, r.tn.ID, r.tn.Actor, nil, payments.RunInput{RequestKey: "outra-chave"}); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("segunda execução da conta: %v", err)
	}

	// O executor percorre todos os tenants do banco compartilhado; vale a
	// execução deste tenant.
	if _, err := r.pay.RunPendingReconciliations(ctx); err != nil {
		t.Logf("execuções de outros tenants: %v", err)
	}
	done := r.stored(t, run.ID)
	if done.Status != payments.RunCompleted || done.Attempts != 1 || done.LeaseUntil != nil || done.DiscrepancyCount != 1 || done.CheckedCount != 1 ||
		done.FinishedAt == nil || done.Error != "" || r.lists.Load() != 1 {
		t.Fatalf("execução concluída pelo worker: %+v (listagens: %d)", done, r.lists.Load())
	}
	r.assertSingleFeeDiscrepancy(t, run.ID)
	if r.claimable(t, run.ID) {
		t.Fatal("execução encerrada não volta para o worker")
	}
	// Repetição do pedido depois de encerrada: mesmo resultado, sem nova execução.
	if again := r.start(t, "rec-assincrona"); again.ID != run.ID || again.Status != payments.RunCompleted || r.lists.Load() != 1 {
		t.Fatalf("resultado pela chave: %+v", again)
	}
	if _, outcome, err := r.pay.ExecuteReconciliationRun(ctx, r.tn.ID, run.ID); err != nil || outcome != payments.RunOutcomeSkipped {
		t.Fatalf("execução encerrada não é assumida: %s %v", outcome, err)
	}
	for _, action := range []string{"reconciliation.run_started", "reconciliation.run_completed"} {
		if n := r.sum(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = '`+action+`'`); n != 1 {
			t.Fatalf("auditoria %s = %d", action, n)
		}
	}
}

// Vários workers disputam a mesma execução: uma única instância a assume e
// executa; as demais a ignoram.
func TestReconciliationRunClaimedByOneWorker(t *testing.T) {
	ctx := context.Background()
	r := newReconHarness(t)
	run := r.start(t, "")
	const workers = 6
	outcomes := make([]payments.RunOutcome, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() { _, outcomes[i], errs[i] = r.pay.ExecuteReconciliationRun(ctx, r.tn.ID, run.ID) })
	}
	wg.Wait()
	completed := 0
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		switch outcomes[i] {
		case payments.RunOutcomeCompleted:
			completed++
		case payments.RunOutcomeSkipped:
		default:
			t.Fatalf("worker %d: resultado %s", i, outcomes[i])
		}
	}
	if completed != 1 || r.lists.Load() != 1 {
		t.Fatalf("execuções = %d, listagens no provedor = %d", completed, r.lists.Load())
	}
	if done := r.stored(t, run.ID); done.Status != payments.RunCompleted || done.Attempts != 1 {
		t.Fatalf("execução: %+v", done)
	}
	r.assertSingleFeeDiscrepancy(t, run.ID)
}

// Worker encerrado durante a coleta (sem devolver a execução): depois do
// lease, o worker reiniciado retoma a mesma execução do início e a conclui,
// sem duplicar divergências.
func TestReconciliationResumedAfterWorkerRestart(t *testing.T) {
	ctx := context.Background()
	r := newReconHarness(t)
	run := r.start(t, "rec-reinicio")
	r.setOnList(func(context.Context) error { panic("worker encerrado durante a coleta") })
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("a coleta deveria ter sido interrompida")
			}
		}()
		_, _, _ = r.pay.ExecuteReconciliationRun(ctx, r.tn.ID, run.ID)
	}()
	r.setOnList(nil)
	held := r.stored(t, run.ID)
	if held.Status != payments.RunRunning || held.Attempts != 1 || held.LeaseUntil == nil || r.claimable(t, run.ID) {
		t.Fatalf("execução assumida pelo worker interrompido: %+v", held)
	}
	if n := r.sum(t, `SELECT count(*) FROM reconciliation_discrepancies WHERE tenant_id = $1`); n != 0 {
		t.Fatalf("execução interrompida não deixa divergência gravada: %d", n)
	}
	stats, err := r.env.Store.ReconciliationRunStats(ctx)
	if err != nil || stats.Executing < 1 {
		t.Fatalf("execução com lease vigente: %+v %v", stats, err)
	}
	// Dentro do lease, o worker reiniciado não a assume, e a conta continua ocupada.
	if _, outcome, err := r.pay.ExecuteReconciliationRun(ctx, r.tn.ID, run.ID); err != nil || outcome != payments.RunOutcomeSkipped {
		t.Fatalf("lease vigente: %s %v", outcome, err)
	}
	if _, err := r.pay.StartReconciliation(ctx, r.tn.ID, r.tn.Actor, nil, payments.RunInput{}); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("conta ocupada pela execução interrompida: %v", err)
	}

	r.env.Clock.Advance(16 * time.Minute)
	if !r.claimable(t, run.ID) {
		t.Fatal("execução com lease expirado volta para o worker")
	}
	if stats, err := r.env.Store.ReconciliationRunStats(ctx); err != nil || stats.Stalled < 1 || stats.OldestWaitingSeconds < 59 {
		t.Fatalf("execução interrompida nas métricas: %+v %v", stats, err)
	}
	if _, err := r.pay.RunPendingReconciliations(ctx); err != nil {
		t.Logf("execuções de outros tenants: %v", err)
	}
	done := r.stored(t, run.ID)
	if done.Status != payments.RunCompleted || done.Attempts != 2 || done.DiscrepancyCount != 1 || done.Error != "" {
		t.Fatalf("execução retomada: %+v", done)
	}
	r.assertSingleFeeDiscrepancy(t, run.ID)
	if n := r.sum(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.run_resumed'
	                    AND (details->>'attempt')::int = 2`); n != 1 {
		t.Fatalf("auditoria da retomada = %d", n)
	}
	if again := r.start(t, "rec-reinicio"); again.ID != run.ID || again.Status != payments.RunCompleted {
		t.Fatalf("a chave do pedido continua apontando para a execução retomada: %+v", again)
	}
}

// A instância cujo lease expirou durante a coleta não conclui a execução que
// outra instância retomou e concluiu: as divergências são gravadas uma vez.
func TestReconciliationStaleWorkerCannotCompleteResumedRun(t *testing.T) {
	ctx := context.Background()
	r := newReconHarness(t)
	run := r.start(t, "")
	var second payments.ReconciliationRun
	var secondOutcome payments.RunOutcome
	var secondErr error
	r.setOnList(func(context.Context) error {
		r.setOnList(nil)
		r.env.Clock.Advance(16 * time.Minute)
		second, secondOutcome, secondErr = r.pay.ExecuteReconciliationRun(ctx, r.tn.ID, run.ID)
		return nil
	})
	_, outcome, err := r.pay.ExecuteReconciliationRun(ctx, r.tn.ID, run.ID)
	if secondErr != nil || secondOutcome != payments.RunOutcomeCompleted || second.Attempts != 2 {
		t.Fatalf("instância que retomou: %s %+v %v", secondOutcome, second, secondErr)
	}
	if e, ok := apperr.As(err); !ok || e.Code != "reconciliation_run_interrupted" || outcome != payments.RunOutcomeSkipped {
		t.Fatalf("instância com lease expirado: %s %v", outcome, err)
	}
	if done := r.stored(t, run.ID); done.Status != payments.RunCompleted || done.Attempts != 2 || done.DiscrepancyCount != 1 {
		t.Fatalf("execução: %+v", done)
	}
	r.assertSingleFeeDiscrepancy(t, run.ID)
	if n := r.sum(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.run_completed'`); n != 1 {
		t.Fatalf("conclusões registradas = %d", n)
	}
}

// O worker que encerra com a execução em andamento a devolve (sem lease) e a
// próxima instância a retoma sem esperar a expiração; depois de três
// tentativas interrompidas, a execução é encerrada como falha, com a causa.
func TestReconciliationReleasedOnShutdownAndFailedAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	r := newReconHarness(t)
	run := r.start(t, "")
	for attempt := 1; attempt <= payments.MaxReconciliationAttempts; attempt++ {
		workerCtx, shutdown := context.WithCancel(ctx)
		r.setOnList(func(ctx context.Context) error {
			shutdown()
			return ctx.Err()
		})
		got, outcome, err := r.pay.ExecuteReconciliationRun(workerCtx, r.tn.ID, run.ID)
		shutdown()
		if err != nil || outcome != payments.RunOutcomeReleased || got.Status != payments.RunRunning || got.LeaseUntil != nil || got.Attempts != attempt {
			t.Fatalf("tentativa %d devolvida: %s %+v %v", attempt, outcome, got, err)
		}
		if !r.claimable(t, run.ID) {
			t.Fatalf("tentativa %d: execução devolvida não voltou para o worker", attempt)
		}
	}
	r.setOnList(nil)
	lists := r.lists.Load()
	failed, outcome, err := r.pay.ExecuteReconciliationRun(ctx, r.tn.ID, run.ID)
	if err != nil || outcome != payments.RunOutcomeFailed || failed.Status != payments.RunFailed || failed.FinishedAt == nil ||
		!strings.Contains(failed.Error, "interrompida 3 vezes") || r.lists.Load() != lists {
		t.Fatalf("execução esgotada: %s %+v %v", outcome, failed, err)
	}
	if n := r.sum(t, `SELECT count(*) FROM reconciliation_discrepancies WHERE tenant_id = $1`); n != 0 {
		t.Fatalf("execução sem conclusão não grava divergências: %d", n)
	}
	if n := r.sum(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.run_released'`); n != 3 {
		t.Fatalf("devoluções auditadas = %d", n)
	}
	// A conta fica livre e a nova execução detecta a divergência.
	next := r.start(t, "")
	if _, outcome, err := r.pay.ExecuteReconciliationRun(ctx, r.tn.ID, next.ID); err != nil || outcome != payments.RunOutcomeCompleted {
		t.Fatalf("nova execução: %s %v", outcome, err)
	}
	r.assertSingleFeeDiscrepancy(t, next.ID)
}

// Provedor indisponível: a execução registrada termina como falha, com a
// causa, sem erro para o executor.
func TestRegisteredReconciliationFailsWithCause(t *testing.T) {
	ctx := context.Background()
	r := newReconHarness(t)
	r.setOnList(func(context.Context) error {
		return &payments.GatewayError{Kind: payments.ErrKindRetryable, StatusCode: 503, Message: "provedor indisponível"}
	})
	run := r.start(t, "")
	failed, outcome, err := r.pay.ExecuteReconciliationRun(ctx, r.tn.ID, run.ID)
	if err != nil || outcome != payments.RunOutcomeFailed || failed.Status != payments.RunFailed || !strings.Contains(failed.Error, "listagem do provedor") ||
		failed.FinishedAt == nil {
		t.Fatalf("execução com falha: %s %+v %v", outcome, failed, err)
	}
}

// A seção Down da migration 00017 funciona com dados: a execução que
// aguardava um worker é encerrada como falha (o esquema anterior não a
// executaria nem a encerraria, e ela bloquearia a conta), as disputas perdem
// as colunas do crédito e a Up pode ser reaplicada em seguida (executado numa
// transação desfeita ao fim, como dono das tabelas).
func TestAsyncReconciliationMigrationDownWithData(t *testing.T) {
	ctx := context.Background()
	r := newReconHarness(t)
	_, c := r.overpaidCharge(t)
	r.chargeback(t, c, "requested", "lost")
	if d := r.dispute(t, c.ID); d.CreditAmount.Amount() != 2500 {
		t.Fatalf("disputa com crédito consumido: %+v", d)
	}
	queued := r.start(t, "")
	// Resultado idempotente registrado com o header Location.
	key := pgstore.IdempotencyKey{TenantID: r.tn.ID, ClientID: "user:x", Operation: "POST /api/v1/reconciliation/runs", Key: "chave-down-0001"}
	hash := sha256.Sum256([]byte("corpo"))
	if _, won, err := r.env.Store.IdempotencyBegin(ctx, key, hash[:], time.Minute, time.Hour); err != nil || !won {
		t.Fatalf("chave de idempotência: won=%v err=%v", won, err)
	}
	if err := r.env.Store.IdempotencyComplete(ctx, key, 200, []byte(`{}`), "application/json", "/api/v1/reconciliation/runs/"+queued.ID.String()); err != nil {
		t.Fatal(err)
	}
	raw, err := db.Migrations.ReadFile("migrations/00017_chargeback_credito_e_conciliacao_assincrona.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("migration sem seção Down")
	}
	tx, err := envOwner(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatalf("Down: %v", err)
	}
	var status, cause string
	var finished bool
	if err := tx.QueryRow(ctx, `SELECT status, COALESCE(error, ''), finished_at IS NOT NULL FROM reconciliation_runs WHERE id = $1`,
		queued.ID).Scan(&status, &cause, &finished); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !finished || !strings.Contains(cause, "reversão do esquema") {
		t.Fatalf("execução aguardando depois do Down: %s %q", status, cause)
	}
	var columns int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'
	      AND ((table_name = 'disputes' AND column_name IN ('credit_amount', 'recovery_amount'))
	        OR (table_name = 'reconciliation_runs' AND column_name = 'attempts')
	        OR (table_name = 'idempotency_keys' AND column_name = 'response_location'))`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("colunas depois do Down: %d %v", columns, err)
	}
	// O resultado registrado continua válido sem o endereço.
	var replayable int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1 AND idempotency_key = $2 AND status = 'completed'
	      AND response_status = 200`, r.tn.ID, key.Key).Scan(&replayable); err != nil || replayable != 1 {
		t.Fatalf("resultado idempotente depois do Down: %d %v", replayable, err)
	}
	if _, err := tx.Exec(ctx, strings.TrimPrefix(up, "-- +goose Up")); err != nil {
		t.Fatalf("Up após Down: %v", err)
	}
	var attempts int
	var credit int64
	if err := tx.QueryRow(ctx, `SELECT attempts FROM reconciliation_runs WHERE id = $1`, queued.ID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("tentativas das execuções existentes: %d %v", attempts, err)
	}
	if err := tx.QueryRow(ctx, `SELECT credit_amount + recovery_amount FROM disputes WHERE charge_id = $1`, c.ID).Scan(&credit); err != nil || credit != 0 {
		t.Fatalf("colunas recriadas com zero: %d %v", credit, err)
	}
	var withoutLocation bool
	if err := tx.QueryRow(ctx, `SELECT response_location IS NULL FROM idempotency_keys WHERE tenant_id = $1 AND idempotency_key = $2`,
		r.tn.ID, key.Key).Scan(&withoutLocation); err != nil || !withoutLocation {
		t.Fatalf("endereço recriado nulo: %v %v", withoutLocation, err)
	}
	// Desfeita a transação, o esquema e os dados voltam ao estado anterior, e
	// a execução que aguardava é concluída normalmente pelo worker.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if d := r.dispute(t, c.ID); d.CreditAmount.Amount() != 2500 {
		t.Fatalf("disputa depois da transação desfeita: %+v", d)
	}
	if rec, won, err := r.env.Store.IdempotencyBegin(ctx, key, hash[:], time.Minute, time.Hour); err != nil || won ||
		rec.ResponseLocation != "/api/v1/reconciliation/runs/"+queued.ID.String() {
		t.Fatalf("resultado idempotente depois da transação desfeita: %+v won=%v err=%v", rec, won, err)
	}
	if _, outcome, err := r.pay.ExecuteReconciliationRun(ctx, r.tn.ID, queued.ID); err != nil || outcome != payments.RunOutcomeCompleted {
		t.Fatalf("execução que aguardava: %s %v", outcome, err)
	}
}
