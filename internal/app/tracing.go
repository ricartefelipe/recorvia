package app

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/scheduler"
)

// tracerName identifica os spans criados pelo worker.
const tracerName = "github.com/ricartefelipe/recorvia/internal/app"

// untracedJobs não geram rastros: rodam a cada poucos segundos e não
// processam dados de negócio.
var untracedJobs = map[string]bool{"metrics.collect": true}

// traceConsumer abre um span de consumo por mensagem, filho do contexto
// propagado nos headers do evento (a requisição que o originou). Sem ele, as
// consultas ao banco do consumidor não geram spans (o otelpgx só cria spans
// com um span pai em gravação). Os atributos não incluem dados de negócio.
func traceConsumer(consumer string, h rabbit.Handler) rabbit.Handler {
	tracer := otel.Tracer(tracerName)
	return func(ctx context.Context, m rabbit.Message) error {
		ctx, span := tracer.Start(ctx, "consume "+consumer, trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(attribute.String("messaging.system", "rabbitmq"),
				attribute.String("messaging.destination.name", rabbit.QueueName(consumer)),
				attribute.String("messaging.message.id", m.ID), attribute.String("recorvia.event_type", m.Type),
				attribute.Int("recorvia.attempt", m.Attempt)))
		defer span.End()
		err := h(ctx, m)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "falha no processamento")
		}
		return err
	}
}

// traceJobs abre um span por execução de tarefa agendada (exceto as de
// coleta de métricas), para que o faturamento e a recuperação de operações
// apareçam no rastreamento com as consultas ao banco e as chamadas ao
// provedor.
func traceJobs(jobs []scheduler.Job) []scheduler.Job {
	tracer := otel.Tracer(tracerName)
	out := make([]scheduler.Job, len(jobs))
	for i, j := range jobs {
		out[i] = j
		if untracedJobs[j.Name] {
			continue
		}
		run, name := j.Run, j.Name
		out[i].Run = func(ctx context.Context) error {
			ctx, span := tracer.Start(ctx, "job "+name, trace.WithSpanKind(trace.SpanKindInternal),
				trace.WithAttributes(attribute.String("recorvia.task", name)))
			defer span.End()
			err := run(ctx)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "falha na tarefa")
			}
			return err
		}
	}
	return out
}
