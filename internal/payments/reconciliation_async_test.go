package payments

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

func (f *payFixture) start(t *testing.T, key string) ReconciliationRun {
	t.Helper()
	r, err := f.svc.StartReconciliation(context.Background(), f.tenantID, f.actor, &f.userID, RunInput{RequestKey: key})
	if err != nil {
		t.Fatalf("StartReconciliation: %v", err)
	}
	return r
}

func (f *payFixture) pending(t *testing.T) PendingRunsResult {
	t.Helper()
	res, err := f.svc.RunPendingReconciliations(context.Background())
	if err != nil {
		t.Fatalf("RunPendingReconciliations: %v", err)
	}
	return res
}

// occurrences conta as ocorrências somadas das divergências: uma retomada
// que duplicasse o registro apareceria como divergência nova ou repetida.
func occurrences(ds []Discrepancy) int {
	n := 0
	for _, d := range ds {
		n += d.Occurrences
	}
	return n
}

// O pedido manual só registra a execução (sem chamar o provedor); o worker a
// assume e executa. A chave do pedido devolve sempre a mesma execução, na
// situação atual, e há uma execução em andamento por conta.
func TestStartReconciliationIsExecutedByWorker(t *testing.T) {
	f := newReconFixture(t)
	run := f.start(t, "rec-async")
	if run.Status != RunRunning || run.LeaseUntil != nil || run.Attempts != 0 || run.FinishedAt != nil || run.Trigger != TriggerManual ||
		*run.RequestedBy != f.userID {
		t.Fatalf("execução registrada: %+v", run)
	}
	if len(f.gw.listCalls) != 0 || len(f.store.discrepancies) != 0 {
		t.Fatal("o pedido não consulta o provedor nem registra divergências")
	}
	if a := f.store.audits[0]; a.Action != "reconciliation.run_started" || a.Actor != f.actor || a.Details["queued"] != true {
		t.Fatalf("auditoria do pedido: %+v", a)
	}
	if again := f.start(t, "rec-async"); again.ID != run.ID || len(f.store.runs) != 1 {
		t.Fatalf("mesma chave devolve a mesma execução: %+v", again)
	}
	_, err := f.svc.StartReconciliation(context.Background(), f.tenantID, f.actor, nil, RunInput{RequestKey: "outra"})
	wantAppErr(t, err, apperr.KindConflict, "reconciliation_running")

	if res := f.pending(t); res != (PendingRunsResult{Completed: 1}) {
		t.Fatalf("passada do executor: %+v", res)
	}
	done := f.store.runs[run.ID]
	if done.Status != RunCompleted || done.Attempts != 1 || done.LeaseUntil != nil || done.CheckedCount != 4 || done.DiscrepancyCount != 4 ||
		done.FinishedAt == nil || done.Error != "" {
		t.Fatalf("execução concluída: %+v", done)
	}
	if slices.Contains(f.auditActions(), "reconciliation.run_resumed") {
		t.Fatal("a primeira tentativa não é uma retomada")
	}
	// Depois de encerrada: a chave devolve o resultado e nada é reexecutado.
	if again := f.start(t, "rec-async"); again.ID != run.ID || again.Status != RunCompleted || again.DiscrepancyCount != 4 {
		t.Fatalf("resultado pela chave: %+v", again)
	}
	if res := f.pending(t); res != (PendingRunsResult{}) || len(f.gw.listCalls) != 1 {
		t.Fatalf("nada a executar: %+v", res)
	}
	if _, outcome, err := f.svc.ExecuteReconciliationRun(context.Background(), f.tenantID, run.ID); err != nil || outcome != RunOutcomeSkipped {
		t.Fatalf("execução encerrada não é assumida: %s %v", outcome, err)
	}
	// A conta fica livre para a próxima execução.
	if next := f.start(t, "rec-async-2"); next.ID == run.ID || next.Status != RunRunning {
		t.Fatalf("nova execução: %+v", next)
	}
}

// Falha do provedor e execução parcial encerram a execução registrada com a
// causa, sem erro para o executor.
func TestPendingReconciliationEndsFailedOrPartialWithCause(t *testing.T) {
	t.Run("falha do provedor", func(t *testing.T) {
		f := newReconFixture(t)
		f.gw.listErr = errAmbiguous
		run := f.start(t, "")
		if res := f.pending(t); res != (PendingRunsResult{Failed: 1}) {
			t.Fatalf("passada: %+v", res)
		}
		got := f.store.runs[run.ID]
		if got.Status != RunFailed || !strings.Contains(got.Error, "listagem do provedor") || got.FinishedAt == nil || got.LeaseUntil != nil ||
			len(f.store.discrepancies) != 0 {
			t.Fatalf("execução com falha: %+v", got)
		}
	})
	t.Run("parcial pelo limite de páginas", func(t *testing.T) {
		f := newReconFixture(t)
		page := f.gw.listPages[0]
		page.HasMore = true
		f.gw.listPages = slices.Repeat([]ListResult{page}, maxProviderPages+1)
		run := f.start(t, "")
		if res := f.pending(t); res != (PendingRunsResult{Completed: 1}) {
			t.Fatalf("passada: %+v", res)
		}
		if got := f.store.runs[run.ID]; got.Status != RunCompleted || !strings.Contains(got.Error, partialProviderPages) {
			t.Fatalf("execução parcial: %+v", got)
		}
	})
}

// A execução interrompida (worker reiniciado) é retomada por outra instância
// depois do lease, do início, sem duplicar divergências.
func TestInterruptedReconciliationIsResumedWithoutDuplicates(t *testing.T) {
	t.Run("worker encerrado sem devolver a execução", func(t *testing.T) {
		f := newReconFixture(t)
		run := f.start(t, "rec-crash")
		// O processo morre durante a coleta: a execução fica assumida.
		f.gw.onList = func() { panic("worker encerrado") }
		func() {
			defer func() { _ = recover() }()
			_, _ = f.svc.RunPendingReconciliations(context.Background())
		}()
		f.gw.onList = nil
		held := f.store.runs[run.ID]
		if held.Status != RunRunning || held.Attempts != 1 || held.LeaseUntil == nil || len(f.store.discrepancies) != 0 {
			t.Fatalf("execução assumida e interrompida: %+v", held)
		}
		// Dentro do lease, ninguém a assume.
		if res := f.pending(t); res != (PendingRunsResult{}) {
			t.Fatalf("lease vigente: %+v", res)
		}
		f.clock.Advance(reconciliationLease + time.Second)
		if res := f.pending(t); res != (PendingRunsResult{Completed: 1}) {
			t.Fatalf("retomada: %+v", res)
		}
		done := f.store.runs[run.ID]
		ds := f.store.discrepancyList()
		if done.Status != RunCompleted || done.Attempts != 2 || done.DiscrepancyCount != 4 || len(ds) != 4 || occurrences(ds) != 4 {
			t.Fatalf("execução retomada: %+v, %d divergências, %d ocorrências", done, len(ds), occurrences(ds))
		}
		for _, d := range ds {
			if d.RunID != run.ID {
				t.Fatalf("divergência de outra execução: %+v", d)
			}
		}
		if !slices.Contains(f.auditActions(), "reconciliation.run_resumed") {
			t.Fatalf("auditoria da retomada: %v", f.auditActions())
		}
	})
	t.Run("instância com lease expirado não conclui a execução retomada por outra", func(t *testing.T) {
		f := newReconFixture(t)
		run := f.start(t, "")
		// Enquanto a primeira instância coleta, o lease expira e outra
		// instância retoma e conclui a execução.
		var second PendingRunsResult
		f.gw.onList = func() {
			f.gw.onList = nil
			f.clock.Advance(reconciliationLease + time.Second)
			second = f.pending(t)
		}
		_, outcome, err := f.svc.ExecuteReconciliationRun(context.Background(), f.tenantID, run.ID)
		wantAppErr(t, err, apperr.KindConflict, "reconciliation_run_interrupted")
		if outcome != RunOutcomeSkipped || second != (PendingRunsResult{Completed: 1}) {
			t.Fatalf("primeira instância: %s; segunda: %+v", outcome, second)
		}
		done := f.store.runs[run.ID]
		ds := f.store.discrepancyList()
		if done.Status != RunCompleted || done.Attempts != 2 || len(ds) != 4 || occurrences(ds) != 4 {
			t.Fatalf("uma só conclusão: %+v, %d divergências, %d ocorrências", done, len(ds), occurrences(ds))
		}
		if n := strings.Count(strings.Join(f.auditActions(), ","), "reconciliation.run_completed"); n != 1 {
			t.Fatalf("conclusões registradas: %d", n)
		}
	})
	t.Run("falha da instância com lease expirado não encerra a execução retomada", func(t *testing.T) {
		f := newReconFixture(t)
		run := f.start(t, "")
		f.gw.listErr = errAmbiguous
		f.gw.onList = func() {
			// Outra instância assumiu: mais uma tentativa e novo lease.
			r := f.store.runs[run.ID]
			lease := f.clock.Now().Add(reconciliationLease)
			r.Attempts, r.LeaseUntil = r.Attempts+1, &lease
			f.store.runs[run.ID] = r
		}
		got, outcome, err := f.svc.ExecuteReconciliationRun(context.Background(), f.tenantID, run.ID)
		if err != nil || outcome != RunOutcomeSkipped || got.Status != RunRunning || f.store.runs[run.ID].Status != RunRunning {
			t.Fatalf("a falha da tentativa antiga é descartada: %s %+v %v", outcome, got, err)
		}
	})
}

// O worker que encerra durante a coleta devolve a execução (sem lease), e a
// próxima instância a retoma sem esperar a expiração.
func TestReconciliationReleasedOnShutdownAndResumed(t *testing.T) {
	f := newReconFixture(t)
	run := f.start(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.gw.listPages[0].HasMore = true
	f.resolver.override = &partialListGateway{fakeGateway: f.gw}
	f.gw.onList = cancel
	res, err := f.svc.RunPendingReconciliations(ctx)
	if !errors.Is(err, context.Canceled) || res != (PendingRunsResult{Released: 1}) {
		t.Fatalf("passada interrompida: %+v %v", res, err)
	}
	released := f.store.runs[run.ID]
	if released.Status != RunRunning || released.LeaseUntil != nil || released.Attempts != 1 || released.Error != "" ||
		len(f.store.discrepancies) != 0 || !slices.Contains(f.auditActions(), "reconciliation.run_released") {
		t.Fatalf("execução devolvida: %+v %v", released, f.auditActions())
	}
	f.gw.onList, f.resolver.override = nil, nil
	f.gw.listPages[0].HasMore = false
	if res := f.pending(t); res != (PendingRunsResult{Completed: 1}) {
		t.Fatalf("retomada: %+v", res)
	}
	done := f.store.runs[run.ID]
	if ds := f.store.discrepancyList(); done.Status != RunCompleted || done.Attempts != 2 || len(ds) != 4 || occurrences(ds) != 4 {
		t.Fatalf("execução retomada: %+v", done)
	}

	t.Run("execução agendada interrompida também é devolvida", func(t *testing.T) {
		f := newReconFixture(t)
		f.store.plan = platformbilling.Plan{Features: []platformbilling.Feature{platformbilling.FeatureReconciliationRun}}
		f.gw.listPages[0].HasMore = true
		f.resolver.override = &partialListGateway{fakeGateway: f.gw}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.gw.onList = cancel
		run, err := f.svc.RunReconciliation(ctx, f.tenantID, f.actor, nil, RunInput{Trigger: TriggerScheduled})
		if !errors.Is(err, context.Canceled) || run.Status != RunRunning || run.LeaseUntil != nil || run.Attempts != 1 {
			t.Fatalf("execução agendada interrompida: %+v %v", run, err)
		}
		f.gw.onList, f.resolver.override = nil, nil
		f.gw.listPages[0].HasMore = false
		if res := f.pending(t); res != (PendingRunsResult{Completed: 1}) || f.store.runs[run.ID].Trigger != TriggerScheduled {
			t.Fatalf("retomada da agendada: %+v", res)
		}
	})
	t.Run("falha ao devolver mantém o lease até expirar", func(t *testing.T) {
		f := newReconFixture(t)
		run := f.start(t, "")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		boom := errors.New("banco indisponível")
		f.gw.listErr = errAmbiguous
		f.gw.onList = func() {
			cancel()
			f.store.errs["UpdateReconciliationRunLease"] = boom
		}
		_, outcome, err := f.svc.ExecuteReconciliationRun(ctx, f.tenantID, run.ID)
		if !errors.Is(err, boom) || !errors.Is(err, context.Canceled) || outcome != RunOutcomeSkipped {
			t.Fatalf("falha ao devolver: %s %v", outcome, err)
		}
		if got := f.store.runs[run.ID]; got.Status != RunRunning || got.LeaseUntil == nil {
			t.Fatalf("lease mantido: %+v", got)
		}
	})
	t.Run("falha ao ler a execução para devolvê-la", func(t *testing.T) {
		f := newReconFixture(t)
		run := f.start(t, "")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		boom := errors.New("banco indisponível")
		f.gw.listErr = errAmbiguous
		f.gw.onList = func() {
			cancel()
			f.store.errs["ReconciliationRun"] = boom
		}
		if _, _, err := f.svc.ExecuteReconciliationRun(ctx, f.tenantID, run.ID); !errors.Is(err, boom) {
			t.Fatalf("falha ao devolver: %v", err)
		}
	})
	t.Run("execução já retomada por outra instância não é devolvida", func(t *testing.T) {
		f := newReconFixture(t)
		run := f.start(t, "")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.gw.listErr = errAmbiguous
		f.gw.onList = func() {
			cancel()
			r := f.store.runs[run.ID]
			r.Attempts++
			f.store.runs[run.ID] = r
		}
		got, outcome, err := f.svc.ExecuteReconciliationRun(ctx, f.tenantID, run.ID)
		if err != nil || outcome != RunOutcomeReleased || got.LeaseUntil == nil || slices.Contains(f.auditActions(), "reconciliation.run_released") {
			t.Fatalf("execução de outra instância: %s %+v %v", outcome, got, err)
		}
	})
}

// Depois de MaxReconciliationAttempts tentativas interrompidas, a execução é
// encerrada como falha, com a causa, e a conta fica livre.
func TestReconciliationFailsAfterMaxInterruptedAttempts(t *testing.T) {
	f := newReconFixture(t)
	run := f.start(t, "rec-esgotada")
	f.gw.onList = func() { panic("worker encerrado") }
	for i := range MaxReconciliationAttempts {
		func() {
			defer func() { _ = recover() }()
			_, _ = f.svc.RunPendingReconciliations(context.Background())
		}()
		if got := f.store.runs[run.ID]; got.Attempts != i+1 || got.Status != RunRunning {
			t.Fatalf("tentativa %d: %+v", i+1, got)
		}
		f.clock.Advance(reconciliationLease + time.Second)
	}
	f.gw.onList = nil
	calls := len(f.gw.listCalls)
	if res := f.pending(t); res != (PendingRunsResult{Failed: 1}) || len(f.gw.listCalls) != calls {
		t.Fatalf("execução esgotada não é executada de novo: %+v", res)
	}
	got := f.store.runs[run.ID]
	if got.Status != RunFailed || got.FinishedAt == nil || got.LeaseUntil != nil || !strings.Contains(got.Error, "interrompida 3 vezes") ||
		len(f.store.discrepancies) != 0 {
		t.Fatalf("execução encerrada: %+v", got)
	}
	last := f.store.audits[len(f.store.audits)-1]
	if last.Action != "reconciliation.run_failed" || last.Details["attempts"] != MaxReconciliationAttempts {
		t.Fatalf("auditoria: %+v", last)
	}
	if next := f.start(t, "rec-nova"); next.ID == run.ID {
		t.Fatal("a conta aceita nova execução")
	}
}

func TestRunPendingReconciliationsFailures(t *testing.T) {
	t.Run("descoberta", func(t *testing.T) {
		f := newReconFixture(t)
		f.store.claimableErr = errors.New("descoberta")
		if _, err := f.svc.RunPendingReconciliations(context.Background()); !errors.Is(err, f.store.claimableErr) {
			t.Fatalf("erro: %v", err)
		}
	})
	t.Run("contexto cancelado", func(t *testing.T) {
		f := newReconFixture(t)
		f.start(t, "")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if res, err := f.svc.RunPendingReconciliations(ctx); !errors.Is(err, context.Canceled) || res != (PendingRunsResult{}) {
			t.Fatalf("cancelamento: %+v %v", res, err)
		}
	})
	for _, m := range []string{"ReconciliationRun", "UpdateReconciliationRunLease", "Account"} {
		t.Run("falha ao assumir: "+m, func(t *testing.T) {
			f := newReconFixture(t)
			run := f.start(t, "")
			boom := errors.New(m)
			f.store.errs[m] = boom
			res, err := f.svc.RunPendingReconciliations(context.Background())
			if !errors.Is(err, boom) || res != (PendingRunsResult{}) || !strings.Contains(err.Error(), run.ID.String()) {
				t.Fatalf("erro: %+v %v", res, err)
			}
			// Nada foi assumido: a execução continua aguardando e a próxima
			// passada a executa.
			if got := f.store.runs[run.ID]; got.Attempts != 0 || got.LeaseUntil != nil {
				t.Fatalf("execução: %+v", got)
			}
			delete(f.store.errs, m)
			if res := f.pending(t); res != (PendingRunsResult{Completed: 1}) {
				t.Fatalf("passada seguinte: %+v", res)
			}
		})
	}
	t.Run("falha ao encerrar a execução esgotada", func(t *testing.T) {
		f := newReconFixture(t)
		lease := f.clock.Now().Add(-time.Minute)
		id := uuid.New()
		f.store.runs[id] = ReconciliationRun{ID: id, TenantID: f.tenantID, ProviderAccountID: f.account.ID, Status: RunRunning, LeaseUntil: &lease,
			Attempts: MaxReconciliationAttempts}
		f.store.runOrder = append(f.store.runOrder, id)
		boom := errors.New("banco indisponível")
		f.store.errs["FinishReconciliationRun"] = boom
		if _, err := f.svc.RunPendingReconciliations(context.Background()); !errors.Is(err, boom) || f.store.runs[id].Status != RunRunning {
			t.Fatalf("erro: %v", err)
		}
	})
	t.Run("falha ao concluir encerra a execução com a causa interna", func(t *testing.T) {
		f := newReconFixture(t)
		run := f.start(t, "")
		boom := errors.New("gravação da divergência")
		f.gw.onList = func() { f.store.errs["InsertDiscrepancy"] = boom }
		_, outcome, err := f.svc.ExecuteReconciliationRun(context.Background(), f.tenantID, run.ID)
		if !errors.Is(err, boom) || outcome != RunOutcomeSkipped {
			t.Fatalf("erro: %s %v", outcome, err)
		}
		if got := f.store.runs[run.ID]; got.Status != RunFailed || !strings.Contains(got.Error, "Falha interna") || len(f.store.discrepancies) != 0 {
			t.Fatalf("execução: %+v", got)
		}
	})
	t.Run("execução inexistente", func(t *testing.T) {
		f := newReconFixture(t)
		_, _, err := f.svc.ExecuteReconciliationRun(context.Background(), f.tenantID, uuid.New())
		wantAppErr(t, err, apperr.KindNotFound, "reconciliation_run_not_found")
	})
}

func TestReconciliationRunClaimable(t *testing.T) {
	now := payNow
	past, future := now.Add(-time.Second), now.Add(time.Second)
	cases := []struct {
		name string
		run  ReconciliationRun
		want bool
	}{
		{"aguardando um worker", ReconciliationRun{Status: RunRunning}, true},
		{"lease expirado", ReconciliationRun{Status: RunRunning, LeaseUntil: &past, Attempts: 1}, true},
		{"lease vigente", ReconciliationRun{Status: RunRunning, LeaseUntil: &future, Attempts: 1}, false},
		{"lease no instante exato", ReconciliationRun{Status: RunRunning, LeaseUntil: &now, Attempts: 1}, false},
		{"concluída", ReconciliationRun{Status: RunCompleted}, false},
		{"com falha", ReconciliationRun{Status: RunFailed}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.run.Claimable(now); got != tc.want {
				t.Fatalf("Claimable = %v", got)
			}
		})
	}
	owner := ReconciliationRun{Status: RunRunning, Attempts: 2}
	if !owner.ownedBy(ReconciliationRun{Attempts: 2}) || owner.ownedBy(ReconciliationRun{Attempts: 1}) ||
		(ReconciliationRun{Status: RunFailed, Attempts: 2}).ownedBy(ReconciliationRun{Attempts: 2}) {
		t.Fatal("somente a tentativa atual de uma execução em andamento a detém")
	}
}
