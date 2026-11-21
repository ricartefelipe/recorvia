//go:build integration

package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways/simulator"
	"github.com/ricartefelipe/recorvia/internal/paysim"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// metricValue lê o valor da série com o rótulo informado no registro.
func metricValue(t *testing.T, m *telemetry.Metrics, name, label, value string) (float64, bool) {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, s := range f.GetMetric() {
			matched := false
			for _, l := range s.GetLabel() {
				matched = matched || (l.GetName() == label && l.GetValue() == value)
			}
			if !matched {
				continue
			}
			switch {
			case s.GetGauge() != nil:
				return s.GetGauge().GetValue(), true
			case s.GetCounter() != nil:
				return s.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

func (w *Worker) job(t *testing.T, name string) func(context.Context) error {
	t.Helper()
	for _, j := range w.Jobs() {
		if j.Name == name {
			return j.Run
		}
	}
	t.Fatalf("tarefa %s não registrada", name)
	return nil
}

// O worker concilia o tenant elegível pelo job agendado, retoma o reembolso
// adiado pela recuperação de operações e publica as métricas de divergências
// e reembolsos a partir dos totais do banco, sem rótulos de tenant.
func TestWorkerReceivablesJobsAndMetrics(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	env := fixture.New(t, 8, time.Now().UTC())
	tn := env.NewTenant(t, "worker")
	if _, err := env.Platform.ChangePlan(ctx, tn.ID, tn.Actor, "escala"); err != nil {
		t.Fatal(err)
	}
	st, err := paysim.Load("")
	if err != nil {
		t.Fatal(err)
	}
	sim := paysim.New(st, "http://checkout.test", func() time.Time { return env.Clock.Now() }, log)
	simSrv := httptest.NewServer(sim.Handler())
	t.Cleanup(simSrv.Close)
	resolver := gateways.NewResolver(2*time.Second, gateways.Factory{Provider: "simulator", Verifier: simulator.Verifier{},
		Gateway: func(a payments.ProviderAccount, hc *http.Client) (payments.Gateway, error) {
			return simulator.New(simSrv.URL, a.APIKey, hc), nil
		}})
	pay := env.Payments(resolver, payments.Config{WebhookBaseURL: "http://webhooks.test/webhooks/providers", CallTimeout: 2 * time.Second,
		Lease: 10 * time.Second})
	apiKey := "sim_worker_" + uuid.NewString()[:8]
	if _, err := pay.ConfigureAccount(ctx, tn.ID, tn.Actor, payments.AccountInput{Provider: "simulator", Environment: "local", APIKey: apiKey,
		WebhookSecret: strings.Repeat("s", 40), MakeDefault: true}); err != nil {
		t.Fatal(err)
	}
	metrics := telemetry.NewMetrics("worker")
	w := &Worker{Core: &Core{Store: env.Store, Payments: pay, Clock: env.Clock, Log: log}, Metrics: metrics, Conn: rabbit.NewConn("amqp://indisponivel", log)}
	collect := func() {
		t.Helper()
		if err := w.job(t, "metrics.collect")(ctx); err != nil {
			t.Fatal(err)
		}
	}
	gauge := func(name, label, value string) float64 {
		t.Helper()
		v, ok := metricValue(t, metrics, name, label, value)
		if !ok {
			t.Fatalf("série %s{%s=%q} ausente", name, label, value)
		}
		return v
	}

	// Cobrança paga no provedor sem webhook: a conciliação agendada aponta
	// o pagamento não refletido.
	plan := env.NewFlatPlan(t, tn, 15000)
	cust := env.NewCustomer(t, tn)
	sub := env.NewSubscription(t, tn, cust.ID, plan.ID, nil)
	if _, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	invs, err := env.Billing.List(ctx, tn.ID, billing.ListFilter{SubscriptionID: &sub.ID, Limit: 10})
	if err != nil || len(invs.Items) != 1 {
		t.Fatalf("fatura: %v", err)
	}
	charge, err := pay.RequestCharge(ctx, tn.ID, invs.Items[0].ID, tn.Actor)
	if err != nil || charge == nil {
		t.Fatalf("cobrança: %v", err)
	}
	if _, err := sim.Apply(charge.ProviderChargeID, paysim.Action{Name: "pay_silently"}); err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(time.Minute)
	if err := w.job(t, "payments.reconcile")(ctx); err != nil {
		t.Fatal(err)
	}
	collect()
	if v := gauge("recorvia_reconciliation_open_discrepancies", "kind", string(payments.DiscrepancyPaymentNotReflected)); v != 1 {
		t.Fatalf("divergências abertas de pagamento = %v", v)
	}
	for _, k := range payments.DiscrepancyKinds {
		gauge("recorvia_reconciliation_open_discrepancies", "kind", string(k))
	}
	if v := gauge("recorvia_refunds_total", "outcome", "succeeded"); v != 0 {
		t.Fatalf("reembolsos concluídos = %v", v)
	}
	// O tenant já conciliado no intervalo não é conciliado de novo.
	if err := w.job(t, "payments.reconcile")(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _, err := env.Store.ListReconciliationRuns(ctx, tn.ID, 10, "")
	if err != nil || len(runs) != 1 || runs[0].Trigger != payments.TriggerScheduled || runs[0].Status != payments.RunCompleted {
		t.Fatalf("execuções agendadas: %+v %v", runs, err)
	}

	discrepancies, _, err := env.Store.ListDiscrepancies(ctx, tn.ID, "open", "", 10, "")
	if err != nil || len(discrepancies) != 1 {
		t.Fatalf("divergências: %v %v", discrepancies, err)
	}
	if _, err := pay.ResolveDiscrepancy(ctx, tn.ID, tn.Actor, &tn.Owner.ID, payments.ResolveInput{ID: discrepancies[0].ID,
		Resolution: payments.ResolutionAcceptProvider, Justification: "Pagamento confirmado no painel do provedor.",
		ExpectedVersion: discrepancies[0].Version}); err != nil {
		t.Fatal(err)
	}

	// Reembolso recusado sem processamento (503): fica solicitado e é
	// concluído pela recuperação de operações do worker.
	if err := sim.EnqueueScenario(apiKey, "refund_unavailable"); err != nil {
		t.Fatal(err)
	}
	refund, err := pay.RequestRefund(ctx, tn.ID, tn.Actor, &tn.Owner.ID, payments.RefundInput{ChargeID: charge.ID, Amount: money.New(5000, money.BRL),
		Reason: "Cancelamento parcial do contrato", RequestKey: "worker-1"})
	if err != nil || refund.Status != payments.RefundRequested {
		t.Fatalf("reembolso: %+v %v", refund, err)
	}
	collect()
	if v := gauge("recorvia_refunds_in_progress", "status", "requested"); v != 1 {
		t.Fatalf("reembolsos solicitados = %v", v)
	}
	if v := gauge("recorvia_reconciliation_open_discrepancies", "kind", string(payments.DiscrepancyPaymentNotReflected)); v != 0 {
		t.Fatalf("divergência resolvida ainda aberta = %v", v)
	}
	env.Clock.Advance(10 * time.Minute)
	if err := w.job(t, "payments.recover_operations")(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := pay.Refund(ctx, tn.ID, refund.ID)
	if err != nil || got.Status != payments.RefundSucceeded {
		t.Fatalf("reembolso após a recuperação: %+v %v", got, err)
	}
	collect()
	if v := gauge("recorvia_refunds_total", "outcome", "succeeded"); v != 1 {
		t.Fatalf("reembolsos concluídos = %v", v)
	}
	if v := gauge("recorvia_refunds_in_progress", "status", "requested"); v != 0 {
		t.Fatalf("reembolsos solicitados = %v", v)
	}
	if v := gauge("recorvia_dependency_up", "dependency", "rabbitmq"); v != 0 {
		t.Fatalf("RabbitMQ sem conexão = %v", v)
	}
	// Nenhuma série identifica o tenant.
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, s := range f.GetMetric() {
			for _, l := range s.GetLabel() {
				if l.GetValue() == tn.ID.String() || strings.Contains(l.GetName(), "tenant") {
					t.Fatalf("métrica %s com rótulo de tenant", f.GetName())
				}
			}
		}
	}
	var ops int
	err = env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_operations WHERE tenant_id = $1 AND kind = 'refund' AND status = 'succeeded'`,
			tn.ID).Scan(&ops)
	})
	if err != nil || ops != 1 {
		t.Fatalf("operação de reembolso concluída: %d %v", ops, err)
	}
}

// listHook intercepta a listagem de cobranças do provedor, a primeira chamada
// da coleta de uma conciliação.
type listHook struct {
	payments.Gateway
	onList func()
}

func (g listHook) ListCharges(ctx context.Context, req payments.ListRequest) (payments.ListResult, error) {
	g.onList()
	if err := ctx.Err(); err != nil {
		return payments.ListResult{}, err
	}
	return g.Gateway.ListCharges(ctx, req)
}

// O executor do worker assume a conciliação manual registrada pela API e a
// conclui; ao encerrar com uma execução em andamento, o worker a devolve, e o
// worker reiniciado a retoma sem duplicar divergências. As métricas das
// execuções em andamento vêm de contagens agregadas, sem rótulo de tenant.
func TestWorkerExecutesRegisteredReconciliations(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	env := fixture.New(t, 8, time.Now().UTC())
	tn := env.NewTenant(t, "worker-conc")
	st, err := paysim.Load("")
	if err != nil {
		t.Fatal(err)
	}
	sim := paysim.New(st, "http://checkout.test", func() time.Time { return env.Clock.Now() }, log)
	simSrv := httptest.NewServer(sim.Handler())
	t.Cleanup(simSrv.Close)
	var mu sync.Mutex
	onList := func() {}
	setOnList := func(fn func()) {
		mu.Lock()
		defer mu.Unlock()
		onList = fn
	}
	resolver := gateways.NewResolver(2*time.Second, gateways.Factory{Provider: "simulator", Verifier: simulator.Verifier{},
		Gateway: func(a payments.ProviderAccount, hc *http.Client) (payments.Gateway, error) {
			return listHook{Gateway: simulator.New(simSrv.URL, a.APIKey, hc), onList: func() {
				mu.Lock()
				fn := onList
				mu.Unlock()
				fn()
			}}, nil
		}})
	pay := env.Payments(resolver, payments.Config{WebhookBaseURL: "http://webhooks.test/webhooks/providers", CallTimeout: 2 * time.Second,
		Lease: 10 * time.Second})
	if _, err := pay.ConfigureAccount(ctx, tn.ID, tn.Actor, payments.AccountInput{Provider: "simulator", Environment: "local",
		APIKey: "sim_conc_" + uuid.NewString()[:8], WebhookSecret: strings.Repeat("s", 40), MakeDefault: true}); err != nil {
		t.Fatal(err)
	}
	metrics := telemetry.NewMetrics("worker")
	w := &Worker{Core: &Core{Store: env.Store, Payments: pay, Clock: env.Clock, Log: log}, Metrics: metrics, Conn: rabbit.NewConn("amqp://indisponivel", log)}
	gauge := func(name, label, value string) float64 {
		t.Helper()
		if err := w.job(t, "metrics.collect")(ctx); err != nil {
			t.Fatal(err)
		}
		v, ok := metricValue(t, metrics, name, label, value)
		if !ok {
			t.Fatalf("série %s{%s=%q} ausente", name, label, value)
		}
		return v
	}
	// startWorker inicia o laço do executor; tick só retorna depois que a
	// passada anterior terminou, e stop espera o laço encerrar.
	startWorker := func() (tick func(), stop func(), cancel context.CancelFunc, done <-chan struct{}) {
		wctx, cancelWorker := context.WithCancel(ctx)
		ticks, finished := make(chan time.Time), make(chan struct{})
		go func() {
			defer close(finished)
			w.runReconciliations(wctx, ticks)
		}()
		return func() { ticks <- time.Time{} }, func() { cancelWorker(); <-finished }, cancelWorker, finished
	}
	stored := func(id uuid.UUID) payments.ReconciliationRun {
		t.Helper()
		run, err := pay.ReconciliationRun(ctx, tn.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}

	// Cobrança paga no provedor sem webhook: divergência a detectar.
	plan := env.NewFlatPlan(t, tn, 15000)
	cust := env.NewCustomer(t, tn)
	sub := env.NewSubscription(t, tn, cust.ID, plan.ID, nil)
	if _, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	invs, err := env.Billing.List(ctx, tn.ID, billing.ListFilter{SubscriptionID: &sub.ID, Limit: 10})
	if err != nil || len(invs.Items) != 1 {
		t.Fatalf("fatura: %v", err)
	}
	charge, err := pay.RequestCharge(ctx, tn.ID, invs.Items[0].ID, tn.Actor)
	if err != nil || charge == nil {
		t.Fatalf("cobrança: %v", err)
	}
	if _, err := sim.Apply(charge.ProviderChargeID, paysim.Action{Name: "pay_silently"}); err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(time.Minute)

	// 1. Execução registrada pela API: fica aguardando até o worker partir.
	first, err := pay.StartReconciliation(ctx, tn.ID, tn.Actor, &tn.Owner.ID, payments.RunInput{RequestKey: "worker-conc-1"})
	if err != nil || first.Status != payments.RunRunning || first.LeaseUntil != nil {
		t.Fatalf("execução registrada: %+v %v", first, err)
	}
	if v := gauge("recorvia_reconciliation_runs_in_progress", "state", "queued"); v < 1 {
		t.Fatalf("execuções aguardando um worker = %v", v)
	}
	for _, state := range []string{"executing", "stalled"} {
		gauge("recorvia_reconciliation_runs_in_progress", "state", state)
	}
	// Sem nenhum worker em execução, a API (que registrou o pedido) exporta a
	// execução que aguarda, lida do banco a cada coleta: a espera cresce
	// além do limite do alerta RecorviaReconciliationRunsWaiting sem depender
	// da tarefa metrics.collect do worker.
	apiMetrics := telemetry.NewMetrics("api")
	apiMetrics.ObserveReconciliationRuns(5*time.Second, ReconciliationRunStats(env.Store, log))
	apiGauge := func(name, label, value string) float64 {
		t.Helper()
		v, ok := metricValue(t, apiMetrics, name, label, value)
		if !ok {
			t.Fatalf("série %s{%s=%q} ausente nas métricas da API", name, label, value)
		}
		return v
	}
	env.Clock.Advance(6 * time.Minute)
	queuedBefore := apiGauge("recorvia_reconciliation_runs_in_progress", "state", "queued")
	if waiting := apiGauge("recorvia_reconciliation_oldest_waiting_seconds", "service", "api"); queuedBefore < 1 || waiting < 360 {
		t.Fatalf("execução sem worker nas métricas da API: aguardando=%v espera=%vs", queuedBefore, waiting)
	}
	tick, stop, _, _ := startWorker()
	tick()
	if got := stored(first.ID); got.Status != payments.RunCompleted || got.Attempts != 1 || got.DiscrepancyCount != 1 {
		t.Fatalf("execução concluída pelo worker: %+v", got)
	}
	stop()
	if v := apiGauge("recorvia_reconciliation_runs_in_progress", "state", "queued"); v != queuedBefore-1 {
		t.Fatalf("execuções aguardando nas métricas da API depois do worker = %v, antes %v", v, queuedBefore)
	}

	// 2. O worker encerra durante a coleta: a execução é devolvida.
	env.Clock.Advance(time.Minute)
	second, err := pay.StartReconciliation(ctx, tn.ID, tn.Actor, &tn.Owner.ID, payments.RunInput{RequestKey: "worker-conc-2"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, shutdown, done := startWorker()
	setOnList(shutdown)
	<-done
	setOnList(func() {})
	if got := stored(second.ID); got.Status != payments.RunRunning || got.LeaseUntil != nil || got.Attempts != 1 || got.Error != "" {
		t.Fatalf("execução devolvida no encerramento do worker: %+v", got)
	}

	// 3. O worker reiniciado retoma a mesma execução e a conclui.
	tick, stop, _, _ = startWorker()
	tick()
	stop()
	if got := stored(second.ID); got.Status != payments.RunCompleted || got.Attempts != 2 || got.DiscrepancyCount != 1 || got.Error != "" {
		t.Fatalf("execução retomada pelo worker reiniciado: %+v", got)
	}
	// A mesma divergência foi constatada pelas duas execuções: um registro,
	// duas ocorrências (a tentativa interrompida não gravou nada).
	var rows, occurrences int
	err = env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), COALESCE(sum(occurrences), 0) FROM reconciliation_discrepancies WHERE tenant_id = $1`,
			tn.ID).Scan(&rows, &occurrences)
	})
	if err != nil || rows != 1 || occurrences != 2 {
		t.Fatalf("divergências: %d registro(s), %d ocorrência(s) %v", rows, occurrences, err)
	}
	if v := gauge("recorvia_reconciliation_open_discrepancies", "kind", string(payments.DiscrepancyPaymentNotReflected)); v < 1 {
		t.Fatalf("divergência aberta nas métricas = %v", v)
	}
	if _, ok := metricValue(t, metrics, "recorvia_reconciliation_oldest_waiting_seconds", "service", "worker"); !ok {
		t.Fatal("série recorvia_reconciliation_oldest_waiting_seconds ausente")
	}
	// A divergência é encerrada para não alterar as contagens agregadas dos
	// demais testes do pacote, que compartilham o banco.
	open, _, err := env.Store.ListDiscrepancies(ctx, tn.ID, "open", "", 10, "")
	if err != nil || len(open) != 1 {
		t.Fatalf("divergência aberta: %v %v", open, err)
	}
	if _, err := pay.ResolveDiscrepancy(ctx, tn.ID, tn.Actor, &tn.Owner.ID, payments.ResolveInput{ID: open[0].ID,
		Resolution: payments.ResolutionDismiss, Justification: "Encerrada ao fim do roteiro do executor.", ExpectedVersion: open[0].Version}); err != nil {
		t.Fatal(err)
	}
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, s := range f.GetMetric() {
			for _, l := range s.GetLabel() {
				if l.GetValue() == tn.ID.String() || strings.Contains(l.GetName(), "tenant") {
					t.Fatalf("métrica %s com rótulo de tenant", f.GetName())
				}
			}
		}
	}
}
