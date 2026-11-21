package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

func TestWorkerSchedulesReceivablesJobs(t *testing.T) {
	w := &Worker{Core: &Core{}}
	jobs := map[string]time.Duration{}
	for _, j := range w.Jobs() {
		if _, dup := jobs[j.Name]; dup {
			t.Fatalf("tarefa duplicada: %s", j.Name)
		}
		if j.Run == nil || j.Interval <= 0 || j.Timeout <= 0 {
			t.Fatalf("tarefa mal configurada: %+v", j)
		}
		jobs[j.Name] = j.Interval
	}
	for name, interval := range map[string]time.Duration{
		// Reembolsos e cancelamentos no provedor são retomados pela recuperação de operações.
		"payments.recover_operations": time.Minute,
		// Conciliação agendada: o job roda a cada hora e cada tenant elegível
		// é conciliado no máximo uma vez por intervalo.
		"payments.reconcile": time.Hour,
		"metrics.collect":    15 * time.Second,
	} {
		if jobs[name] != interval {
			t.Errorf("tarefa %s: intervalo %v, esperado %v", name, jobs[name], interval)
		}
	}
	if payments.ScheduledReconciliationEvery < jobs["payments.reconcile"] {
		t.Fatal("o intervalo por tenant não pode ser menor que o do job")
	}
}

func TestWorkerConsumersCoverPaymentEvents(t *testing.T) {
	w := &Worker{Core: &Core{}}
	handlers := w.handlers()
	// Um evento pode ter vários consumidores (o fan-out de webhooks assina todos).
	consumers := map[string]map[string]bool{}
	for _, spec := range consumerSpecs {
		if handlers[spec.Name] == nil {
			t.Fatalf("consumidor sem handler: %s", spec.Name)
		}
		for _, et := range spec.EventTypes {
			if consumers[et] == nil {
				consumers[et] = map[string]bool{}
			}
			consumers[et][spec.Name] = true
		}
	}
	// Webhooks de reembolso e chargeback chegam pelo consumidor de webhooks;
	// cancelamentos no provedor decorrem da fatura cancelada e da nota de crédito.
	for event, consumer := range map[string]string{
		outbox.EventProviderWebhook: payments.ConsumerWebhooks,
		outbox.EventInvoiceIssued:   payments.ConsumerInvoiceIssued,
		outbox.EventInvoiceVoided:   "payments.invoice_voided",
		outbox.EventInvoiceCredited: payments.ConsumerInvoiceCredited,
	} {
		if !consumers[event][consumer] {
			t.Errorf("evento %s: consumidores %v, esperado %q", event, consumers[event], consumer)
		}
	}
}

// Todo consumidor declarado tem handler, e os eventos de fatura e de
// domínio chegam aos consumidores de notificações, régua e webhooks.
func TestConsumersHaveHandlersAndSubscriptions(t *testing.T) {
	w := &Worker{Core: &Core{}}
	handlers := w.handlers()
	bound := map[string][]string{}
	for _, s := range consumerSpecs {
		if handlers[s.Name] == nil {
			t.Errorf("consumidor sem handler: %s", s.Name)
		}
		if len(s.EventTypes) == 0 {
			t.Errorf("consumidor sem eventos: %s", s.Name)
		}
		bound[s.Name] = s.EventTypes
	}
	if len(handlers) != len(consumerSpecs) {
		t.Errorf("handlers sem consumidor declarado: %d handlers, %d consumidores", len(handlers), len(consumerSpecs))
	}
	expect := map[string][]string{
		notifications.ConsumerInvoiceIssued:    {outbox.EventInvoiceIssued},
		notifications.ConsumerPaymentConfirmed: {outbox.EventPaymentConfirmed},
		notifications.ConsumerMemberInvited:    {outbox.EventMemberInvited},
		collections.ConsumerInvoiceResolved:    {outbox.EventInvoicePaid, outbox.EventInvoiceVoided, outbox.EventInvoiceUncollectible, outbox.EventInvoiceCredited},
		webhooks.ConsumerFanout:                webhooks.EventTypes(),
	}
	for consumer, events := range expect {
		got := map[string]bool{}
		for _, e := range bound[consumer] {
			got[e] = true
		}
		for _, e := range events {
			if !got[e] {
				t.Errorf("%s não assina %s", consumer, e)
			}
		}
	}
}

// Mensagens malformadas são rejeitadas como erro permanente (DLQ) antes de
// qualquer efeito.
func TestWorkerHandlersRejectMalformedMessages(t *testing.T) {
	w := &Worker{Core: &Core{}}
	valid := rabbit.Message{ID: uuid.NewString(), TenantID: uuid.NewString()}
	cases := []struct {
		name string
		msg  rabbit.Message
	}{
		{"tenant inválido", rabbit.Message{ID: uuid.NewString(), TenantID: "x"}},
		{"identificador inválido", rabbit.Message{ID: "x", TenantID: uuid.NewString()}},
		{"payload inválido", rabbit.Message{ID: valid.ID, TenantID: valid.TenantID, Body: []byte("{")}},
		{"campo ausente", rabbit.Message{ID: valid.ID, TenantID: valid.TenantID, Body: []byte(`{"outro":"x"}`)}},
	}
	for name, h := range w.handlers() {
		if name == webhooks.ConsumerFanout {
			// O fan-out repassa o payload como opaco (JSON inválido vira {}) e
			// ignora tipos não assináveis; o tenant inválido é coberto abaixo.
			continue
		}
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				var perm *rabbit.PermanentError
				if err := h(context.Background(), tc.msg); !errors.As(err, &perm) {
					t.Fatalf("erro %v, esperado permanente", err)
				}
			})
		}
	}
}

// Mensagens malformadas são erros permanentes (vão direto para a DLQ, sem
// retentativas).
func TestHandlersRejectMalformedMessagesPermanently(t *testing.T) {
	w := &Worker{Core: &Core{}}
	h := w.handlers()
	tenant, msg := uuid.NewString(), uuid.NewString()
	cases := []struct {
		name     string
		consumer string
		m        rabbit.Message
	}{
		{"tenant inválido", notifications.ConsumerInvoiceIssued, rabbit.Message{ID: msg, TenantID: "x", Body: []byte(`{}`)}},
		{"id de mensagem inválido", notifications.ConsumerPaymentConfirmed, rabbit.Message{ID: "x", TenantID: tenant, Body: []byte(`{}`)}},
		{"payload sem fatura", collections.ConsumerInvoiceResolved, rabbit.Message{ID: msg, TenantID: tenant, Body: []byte(`{"invoice_id":"x"}`)}},
		{"payload não JSON", notifications.ConsumerInvoiceIssued, rabbit.Message{ID: msg, TenantID: tenant, Body: []byte(`[`)}},
		{"convite sem identificador", notifications.ConsumerMemberInvited, rabbit.Message{ID: msg, TenantID: tenant, Body: []byte(`{"role":"finance"}`)}},
		{"convite com tenant inválido", notifications.ConsumerMemberInvited, rabbit.Message{ID: msg, TenantID: "", Body: []byte(`{}`)}},
		{"fan-out com tenant inválido", webhooks.ConsumerFanout, rabbit.Message{ID: msg, TenantID: "x", Body: []byte(`{}`)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := h[c.consumer](context.Background(), c.m)
			var perm *rabbit.PermanentError
			if !errors.As(err, &perm) {
				t.Fatalf("esperava erro permanente: %v", err)
			}
		})
	}
}

// As execuções dos jobs alimentam contadores por resultado, sem rótulos de
// tenant.
func TestJobResultsFeedMetrics(t *testing.T) {
	m := telemetry.NewMetrics("teste")
	recordDispatch(m, notifications.DispatchResult{Sent: 3, Retried: 1, Failed: 2, Deferred: 4, Skipped: 1, LostLease: 1})
	recordDispatch(m, notifications.DispatchResult{Sent: 1})
	recordDeliveries(m, webhooks.DeliverResult{Succeeded: 5, Retrying: 2, Exhausted: 1, LostLease: 1, Deferred: 3})
	recordCollections(m, collections.RunResult{Reminders: 4, Overdue: 2, Suspended: 1, Reactivated: 1})
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"enviadas", testutil.ToFloat64(m.NotificationsDispatched.WithLabelValues("sent")), 4},
		{"reagendadas", testutil.ToFloat64(m.NotificationsDispatched.WithLabelValues("retried")), 1},
		{"falhas", testutil.ToFloat64(m.NotificationsDispatched.WithLabelValues("failed")), 2},
		{"adiadas", testutil.ToFloat64(m.NotificationsDispatched.WithLabelValues("deferred")), 4},
		{"entregues", testutil.ToFloat64(m.WebhookDeliveries.WithLabelValues("succeeded")), 5},
		{"esgotadas", testutil.ToFloat64(m.WebhookDeliveries.WithLabelValues("exhausted")), 1},
		{"devolvidas à fila", testutil.ToFloat64(m.WebhookDeliveries.WithLabelValues("deferred")), 3},
		{"lembretes", testutil.ToFloat64(m.CollectionActions.WithLabelValues("reminder_scheduled")), 4},
		{"suspensões", testutil.ToFloat64(m.CollectionActions.WithLabelValues("subscription_suspended")), 1},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, esperado %v", c.name, c.got, c.want)
		}
	}
	// Resultados zerados não criam séries.
	before := testutil.CollectAndCount(m.WebhookDeliveries)
	recordDeliveries(m, webhooks.DeliverResult{})
	if after := testutil.CollectAndCount(m.WebhookDeliveries); after != before {
		t.Fatalf("séries criadas sem resultado: %d -> %d", before, after)
	}
}

func TestJobsIncludeCollectionsNotificationsAndWebhooks(t *testing.T) {
	w := &Worker{Core: &Core{}}
	byName := map[string]time.Duration{}
	for _, j := range w.Jobs() {
		if j.Run == nil || j.Interval <= 0 || j.Timeout <= 0 {
			t.Errorf("tarefa inválida: %+v", j)
		}
		byName[j.Name] = j.Interval
	}
	for _, name := range []string{JobCollections, JobNotifications, JobWebhooks} {
		if byName[name] <= 0 {
			t.Errorf("tarefa ausente: %s", name)
		}
	}
	if byName[JobNotifications] > time.Minute || byName[JobWebhooks] > time.Minute {
		t.Errorf("envios devem ser frequentes: %v", byName)
	}
}

// Toda tarefa agendada exporta o intervalo configurado: os alertas de tarefa
// sem sucesso são proporcionais a ele (as tarefas horárias e a diária não
// chegam a três falhas em 30 minutos).
func TestWorkerMetricsExportEveryJobInterval(t *testing.T) {
	m := telemetry.NewMetrics("teste")
	w := &Worker{Core: &Core{}, Metrics: m}
	jobs := w.Jobs()
	w.initMetrics(jobs)
	if got := testutil.CollectAndCount(m.JobInterval); got != len(jobs) {
		t.Fatalf("séries de intervalo: %d, tarefas: %d", got, len(jobs))
	}
	for _, j := range jobs {
		if got := testutil.ToFloat64(m.JobInterval.WithLabelValues(j.Name)); got != j.Interval.Seconds() {
			t.Errorf("%s: intervalo %v, esperado %v", j.Name, got, j.Interval.Seconds())
		}
	}
	for name, want := range map[string]time.Duration{"platform.close_statements": time.Hour, "idempotency.purge": time.Hour,
		"outbox.purge": 24 * time.Hour} {
		if got := testutil.ToFloat64(m.JobInterval.WithLabelValues(name)); got != want.Seconds() {
			t.Errorf("%s: intervalo %v", name, got)
		}
	}
	if got := testutil.CollectAndCount(m.NotificationsDispatched); got != len(telemetry.NotificationOutcomes) {
		t.Fatalf("resultados de envio criados com zero: %d", got)
	}
}

// Os rótulos gravados pelas tarefas são exatamente os declarados (e
// documentados no catálogo) em telemetry.
func TestJobResultLabelsAreDeclared(t *testing.T) {
	m := telemetry.NewMetrics("teste")
	recordDispatch(m, notifications.DispatchResult{Sent: 1, Retried: 1, Failed: 1, Deferred: 1, Skipped: 1, LostLease: 1})
	recordDeliveries(m, webhooks.DeliverResult{Succeeded: 1, Retrying: 1, Exhausted: 1, LostLease: 1, Deferred: 1})
	recordCollections(m, collections.RunResult{Reminders: 1, Overdue: 1, Suspended: 1, Reactivated: 1})
	cases := []struct {
		name string
		vec  interface {
			WithLabelValues(...string) prometheus.Counter
		}
		values []string
		total  int
	}{
		{"notificações", m.NotificationsDispatched, telemetry.NotificationOutcomes, testutil.CollectAndCount(m.NotificationsDispatched)},
		{"entregas", m.WebhookDeliveries, telemetry.WebhookDeliveryOutcomes, testutil.CollectAndCount(m.WebhookDeliveries)},
		{"régua", m.CollectionActions, telemetry.CollectionActionKinds, testutil.CollectAndCount(m.CollectionActions)},
	}
	for _, c := range cases {
		if c.total != len(c.values) {
			t.Errorf("%s: %d séries, %d valores declarados", c.name, c.total, len(c.values))
		}
		for _, v := range c.values {
			if got := testutil.ToFloat64(c.vec.WithLabelValues(v)); got != 1 {
				t.Errorf("%s: %s = %v", c.name, v, got)
			}
		}
	}
}

// A contagem de operações esgotadas só é substituída por uma execução
// completa da recuperação.
func TestRecoveryFeedsExhaustedOperations(t *testing.T) {
	m := telemetry.NewMetrics("teste")
	gauge := func(s payments.OperationStatus) float64 {
		return testutil.ToFloat64(m.PaymentOperationsExhausted.WithLabelValues(string(s)))
	}
	steps := []struct {
		name string
		res  payments.RecoveryResult
		want map[payments.OperationStatus]float64
	}{
		{"execução completa", payments.RecoveryResult{Complete: true,
			Exhausted: map[payments.OperationStatus]int{payments.OpPending: 2, payments.OpInFlight: 1}},
			map[payments.OperationStatus]float64{payments.OpPending: 2, payments.OpInFlight: 1, payments.OpUnknown: 0}},
		{"execução parcial mantém a contagem", payments.RecoveryResult{Executed: 3, Exhausted: map[payments.OperationStatus]int{}},
			map[payments.OperationStatus]float64{payments.OpPending: 2, payments.OpInFlight: 1, payments.OpUnknown: 0}},
		{"nenhuma esgotada", payments.RecoveryResult{Complete: true},
			map[payments.OperationStatus]float64{payments.OpPending: 0, payments.OpInFlight: 0, payments.OpUnknown: 0}},
	}
	for _, st := range steps {
		recordRecovery(m, st.res)
		for s, want := range st.want {
			if got := gauge(s); got != want {
				t.Errorf("%s: %s = %v, esperado %v", st.name, s, got, want)
			}
		}
	}
}

// As dead letters abertas são gravadas para todos os consumidores do worker
// (zero quando não há) e as de consumidores desconhecidos, cujo nome vem do
// banco, são reunidas em "other" sem criar séries novas. Uma leitura
// posterior sem dead letters zera os medidores.
func TestDeadLetterStatsFeedGaugesWithClosedConsumerSet(t *testing.T) {
	m := telemetry.NewMetrics("teste")
	consumers := consumerNames()
	if len(consumers) != len(consumerSpecs) || !slices.Contains(consumers, payments.ConsumerWebhooks) {
		t.Fatalf("consumidores: %v", consumers)
	}
	open := func(consumer string) float64 { return testutil.ToFloat64(m.DeadLettersOpen.WithLabelValues(consumer)) }
	oldest := func(consumer string) float64 {
		return testutil.ToFloat64(m.DeadLettersOldestOpenSeconds.WithLabelValues(consumer))
	}
	steps := []struct {
		name       string
		stats      map[string]pgstore.BacklogStat
		wantOpen   map[string]float64
		wantOldest map[string]float64
	}{
		{"consumidores conhecidos e removidos", map[string]pgstore.BacklogStat{
			payments.ConsumerWebhooks: {Total: 3, OldestSeconds: 7200},
			webhooks.ConsumerFanout:   {Total: 1, OldestSeconds: 30},
			"payments.consumidor_v0":  {Total: 2, OldestSeconds: 90000},
			"consumidor-desconhecido": {Total: 5, OldestSeconds: 600},
			"6f1c2c0e-tenant-no-nome": {Total: 1, OldestSeconds: 10},
		},
			map[string]float64{payments.ConsumerWebhooks: 3, webhooks.ConsumerFanout: 1, payments.ConsumerInvoiceIssued: 0, telemetry.OtherConsumer: 8},
			map[string]float64{payments.ConsumerWebhooks: 7200, webhooks.ConsumerFanout: 30, payments.ConsumerInvoiceIssued: 0, telemetry.OtherConsumer: 90000}},
		{"todas resolvidas", map[string]pgstore.BacklogStat{},
			map[string]float64{payments.ConsumerWebhooks: 0, webhooks.ConsumerFanout: 0, telemetry.OtherConsumer: 0},
			map[string]float64{payments.ConsumerWebhooks: 0, webhooks.ConsumerFanout: 0, telemetry.OtherConsumer: 0}},
	}
	for _, st := range steps {
		recordDeadLetters(m, consumers, st.stats)
		for c, want := range st.wantOpen {
			if got := open(c); got != want {
				t.Errorf("%s: abertas de %s = %v, esperado %v", st.name, c, got, want)
			}
		}
		for c, want := range st.wantOldest {
			if got := oldest(c); got != want {
				t.Errorf("%s: idade de %s = %v, esperado %v", st.name, c, got, want)
			}
		}
		for _, vec := range []*prometheus.GaugeVec{m.DeadLettersOpen, m.DeadLettersOldestOpenSeconds} {
			if got := testutil.CollectAndCount(vec); got != len(consumers)+1 {
				t.Fatalf("%s: %d séries; esperado uma por consumidor e \"other\" (%d)", st.name, got, len(consumers)+1)
			}
		}
	}
}

// As filas de notificações e de entregas de webhooks gravam exatamente as
// situações declaradas (e documentadas no catálogo): as ausentes da leitura
// valem zero e uma situação desconhecida não cria série.
func TestQueueStatsFeedGaugesWithDeclaredStates(t *testing.T) {
	m := telemetry.NewMetrics("teste")
	recordQueue(m.NotificationQueue(), telemetry.NotificationBacklogStates, map[string]pgstore.BacklogStat{
		pgstore.NotificationBacklogDue:    {Total: 12, OldestSeconds: 1500},
		pgstore.NotificationBacklogFailed: {Total: 4, OldestSeconds: 3600},
		"canceled":                        {Total: 99, OldestSeconds: 99},
	})
	recordQueue(m.WebhookDeliveryQueue(), telemetry.WebhookBacklogStates, map[string]pgstore.BacklogStat{
		string(webhooks.Retrying):  {Total: 7, OldestSeconds: 45},
		string(webhooks.Exhausted): {Total: 99},
	})
	cases := []struct {
		name          string
		total, oldest *prometheus.GaugeVec
		state         string
		wantTotal     float64
		wantOldest    float64
	}{
		{"notificações vencidas", m.NotificationsBacklog, m.NotificationsBacklogOldestSeconds, pgstore.NotificationBacklogDue, 12, 1500},
		{"notificações em envio", m.NotificationsBacklog, m.NotificationsBacklogOldestSeconds, pgstore.NotificationBacklogSending, 0, 0},
		{"notificações com falha", m.NotificationsBacklog, m.NotificationsBacklogOldestSeconds, pgstore.NotificationBacklogFailed, 4, 3600},
		{"entregas pendentes", m.WebhookDeliveriesBacklog, m.WebhookDeliveriesBacklogOldestDueSeconds, string(webhooks.Pending), 0, 0},
		{"entregas em retentativa", m.WebhookDeliveriesBacklog, m.WebhookDeliveriesBacklogOldestDueSeconds, string(webhooks.Retrying), 7, 45},
	}
	for _, c := range cases {
		if got := testutil.ToFloat64(c.total.WithLabelValues(c.state)); got != c.wantTotal {
			t.Errorf("%s: total %v, esperado %v", c.name, got, c.wantTotal)
		}
		if got := testutil.ToFloat64(c.oldest.WithLabelValues(c.state)); got != c.wantOldest {
			t.Errorf("%s: idade %v, esperado %v", c.name, got, c.wantOldest)
		}
	}
	for vec, want := range map[*prometheus.GaugeVec]int{
		m.NotificationsBacklog: len(telemetry.NotificationBacklogStates), m.NotificationsBacklogOldestSeconds: len(telemetry.NotificationBacklogStates),
		m.WebhookDeliveriesBacklog: len(telemetry.WebhookBacklogStates), m.WebhookDeliveriesBacklogOldestDueSeconds: len(telemetry.WebhookBacklogStates),
	} {
		if got := testutil.CollectAndCount(vec); got != want {
			t.Errorf("séries criadas: %d, situações declaradas: %d", got, want)
		}
	}
	// As situações declaradas são as do banco e do domínio.
	if !slices.Equal(telemetry.NotificationBacklogStates, []string{pgstore.NotificationBacklogDue, pgstore.NotificationBacklogSending,
		pgstore.NotificationBacklogFailed}) {
		t.Errorf("situações da fila de notificações: %v", telemetry.NotificationBacklogStates)
	}
	if !slices.Equal(telemetry.WebhookBacklogStates, []string{string(webhooks.Pending), string(webhooks.Retrying)}) {
		t.Errorf("situações da fila de entregas: %v", telemetry.WebhookBacklogStates)
	}
	if notificationFailedWindow != 24*time.Hour {
		t.Errorf("janela de falhas diferente da documentada no catálogo (24 horas): %v", notificationFailedWindow)
	}
}

// Um pânico inesperado no executor de conciliações vira erro registrado no
// log: o laço continua nas passadas seguintes e termina com o worker.
func TestReconciliationRunnerSurvivesPanicAndStopsWithWorker(t *testing.T) {
	var logs bytes.Buffer
	// Sem serviço de pagamentos, a passada entra em pânico (ponteiro nulo).
	w := &Worker{Core: &Core{Log: slog.New(slog.NewTextHandler(&logs, nil))}}
	if _, err := w.reconciliationPass(context.Background()); err == nil || !strings.Contains(err.Error(), "pânico no executor de conciliações") {
		t.Fatalf("pânico convertido em erro: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.runReconciliations(ctx, ticks)
	}()
	// Cada sinal só é recebido depois que a passada anterior terminou.
	ticks <- time.Time{}
	ticks <- time.Time{}
	cancel()
	<-done
	if n := strings.Count(logs.String(), "executor de conciliações falhou"); n < 2 {
		t.Fatalf("falhas registradas no log: %d\n%s", n, logs.String())
	}
	if reconciliationRunners < 1 || reconciliationPoll <= 0 || reconciliationPoll > 10*time.Second {
		t.Fatalf("executor mal configurado: %d instâncias a cada %v", reconciliationRunners, reconciliationPoll)
	}
}
