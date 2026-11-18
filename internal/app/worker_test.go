package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/payments"
)

func TestWorkerSchedulesReceivablesJobs(t *testing.T) {
	w := &Worker{Core: &Core{}}
	jobs := map[string]time.Duration{}
	for _, j := range w.Jobs() {
		if _, dup := jobs[j.Name]; dup {
			t.Fatalf("tarefa duplicada: %s", j.Name)
		}
		if j.Run == nil || j.Interval <= 0 || j.Timeout <= 0 || j.Timeout > j.Interval*10 {
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
	consumers := map[string]string{}
	for _, spec := range consumerSpecs {
		if handlers[spec.Name] == nil {
			t.Fatalf("consumidor sem handler: %s", spec.Name)
		}
		for _, et := range spec.EventTypes {
			consumers[et] = spec.Name
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
		if consumers[event] != consumer {
			t.Errorf("evento %s: consumidor %q, esperado %q", event, consumers[event], consumer)
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
