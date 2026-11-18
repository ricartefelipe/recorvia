package app

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/scheduler"
)

// recordSpans instala um provedor de rastreamento em memória e o propagador
// W3C (configurado em produção por telemetry.SetupTracing) durante o teste.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		otel.SetTextMapPropagator(prevProp)
	})
	return rec
}

func TestInstrumentCreatesConsumerSpanUnderPropagatedContext(t *testing.T) {
	rec := recordSpans(t)
	parent := "4bf92f3577b34da6a3ce929d0e0e4736"
	boom := errors.New("provedor indisponível")
	cases := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{"sucesso", nil, false},
		{"falha", boom, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var inside trace.SpanContext
			h := instrument("payments.invoice_issued", func(ctx context.Context, _ rabbit.Message) error {
				inside = trace.SpanContextFromContext(ctx)
				return tc.err
			})
			m := rabbit.Message{ID: "msg-1", Type: "invoice.issued", Attempt: 2,
				Headers: map[string]string{"traceparent": "00-" + parent + "-00f067aa0ba902b7-01"}}
			err := h(context.Background(), m)
			if !errors.Is(err, tc.err) {
				t.Fatalf("erro do handler deve ser repassado: %v", err)
			}
			spans := rec.Ended()
			s := spans[len(spans)-1]
			if s.Name() != "consume payments.invoice_issued" || s.SpanKind() != trace.SpanKindConsumer {
				t.Fatalf("span %q (%v)", s.Name(), s.SpanKind())
			}
			if s.Parent().TraceID().String() != parent || inside.TraceID().String() != parent || !inside.IsValid() {
				t.Fatalf("o span precisa continuar o rastro do evento: pai %s, handler %s", s.Parent().TraceID(), inside.TraceID())
			}
			attrs := map[string]string{}
			for _, a := range s.Attributes() {
				attrs[string(a.Key)] = a.Value.String()
			}
			if attrs["messaging.destination.name"] != rabbit.QueueName("payments.invoice_issued") || attrs["messaging.message.id"] != "msg-1" ||
				attrs["recorvia.attempt"] != "2" {
				t.Fatalf("atributos %v", attrs)
			}
			if got := s.Status().Code == codes.Error; got != tc.wantErr {
				t.Fatalf("status de erro = %v, esperado %v", got, tc.wantErr)
			}
		})
	}
}

func TestTraceJobsSkipsMetricsCollection(t *testing.T) {
	rec := recordSpans(t)
	boom := errors.New("falha")
	ran := map[string]bool{}
	job := func(name string, err error) scheduler.Job {
		return scheduler.Job{Name: name, Run: func(ctx context.Context) error {
			ran[name] = trace.SpanContextFromContext(ctx).IsValid()
			return err
		}}
	}
	jobs := traceJobs([]scheduler.Job{job("billing.run_cycle", nil), job("payments.recover_operations", boom), job("metrics.collect", nil)})
	for _, j := range jobs {
		if err := j.Run(context.Background()); err != nil && !errors.Is(err, boom) {
			t.Fatal(err)
		}
	}
	if !ran["billing.run_cycle"] || !ran["payments.recover_operations"] || ran["metrics.collect"] {
		t.Fatalf("contexto com span por tarefa: %v", ran)
	}
	spans := rec.Ended()
	if len(spans) != 2 || spans[0].Name() != "job billing.run_cycle" || spans[1].Status().Code != codes.Error {
		t.Fatalf("spans %d", len(spans))
	}
}

// Garante que as tarefas do worker continuam observadas pelas métricas depois
// da decoração de rastreamento.
func TestTraceJobsPreservesSchedule(t *testing.T) {
	w := &Worker{Core: &Core{}, Metrics: telemetry.NewMetrics("worker")}
	orig := w.Jobs()
	traced := traceJobs(orig)
	if len(traced) != len(orig) {
		t.Fatalf("tarefas %d, esperado %d", len(traced), len(orig))
	}
	for i := range orig {
		if traced[i].Name != orig[i].Name || traced[i].Interval != orig[i].Interval || traced[i].Timeout != orig[i].Timeout {
			t.Fatalf("tarefa %s alterada", orig[i].Name)
		}
	}
}
