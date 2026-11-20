package app

import (
	"context"
	"errors"
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
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

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
