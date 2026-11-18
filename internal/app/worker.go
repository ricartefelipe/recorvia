package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/scheduler"
)

// relayStore adapta o pgstore à porta do relay.
type relayStore struct{ s *pgstore.Store }

func (r relayStore) Claim(ctx context.Context, worker string, batch int, lease time.Duration) ([]outbox.Record, error) {
	recs, err := r.s.ClaimOutbox(ctx, worker, batch, lease)
	out := make([]outbox.Record, 0, len(recs))
	for _, x := range recs {
		out = append(out, outbox.Record{ID: x.ID, TenantID: x.TenantID, Type: x.EventType, Payload: x.Payload, Headers: x.Headers,
			OccurredAt: x.OccurredAt, Attempts: x.Attempts})
	}
	return out, err
}

func (r relayStore) MarkPublished(ctx context.Context, worker string, eventIDs []uuid.UUID) (int, error) {
	return r.s.MarkOutboxPublished(ctx, worker, eventIDs)
}

func (r relayStore) Release(ctx context.Context, worker string, id uuid.UUID, delay time.Duration, cause string) error {
	return r.s.ReleaseOutbox(ctx, worker, id, delay, cause)
}

// eventPublisher publica eventos da outbox no RabbitMQ com confirmação.
type eventPublisher struct{ p *rabbit.Publisher }

func (e eventPublisher) PublishEvent(ctx context.Context, r outbox.Record) error {
	headers := map[string]string{}
	for k, v := range r.Headers {
		headers[k] = v
	}
	queue := headers[pgstore.HeaderTargetQueue]
	return e.p.Publish(ctx, queue, rabbit.Message{ID: r.ID.String(), Type: r.Type, TenantID: r.TenantID.String(), Body: r.Payload,
		Headers: headers, OccurredAt: r.OccurredAt}, 0)
}

// schedulerStore adapta o pgstore à porta do agendador.
type schedulerStore struct{ s *pgstore.Store }

func (s schedulerStore) EnsureJob(ctx context.Context, name string, interval time.Duration) error {
	return s.s.EnsureJob(ctx, name, interval)
}

func (s schedulerStore) ClaimJob(ctx context.Context, worker string, lease time.Duration) (string, bool, error) {
	j, err := s.s.ClaimJob(ctx, worker, lease)
	if err != nil || j == nil {
		return "", false, err
	}
	return j.Name, true, nil
}

func (s schedulerStore) CompleteJob(ctx context.Context, worker, name string, next time.Time, runErr error) error {
	return s.s.CompleteJob(ctx, worker, name, next, runErr)
}

// Consumidores e eventos assinados.
var consumerSpecs = []rabbit.ConsumerSpec{
	{Name: payments.ConsumerInvoiceIssued, EventTypes: []string{outbox.EventInvoiceIssued}},
	{Name: payments.ConsumerWebhooks, EventTypes: []string{outbox.EventProviderWebhook}},
	{Name: "payments.invoice_voided", EventTypes: []string{outbox.EventInvoiceVoided}},
	{Name: payments.ConsumerInvoiceCredited, EventTypes: []string{outbox.EventInvoiceCredited}},
}

// Worker é o processo de eventos, agendamentos e integrações.
type Worker struct {
	Core    *Core
	Metrics *telemetry.Metrics
	Conn    *rabbit.Conn
}

func parseMessage(m rabbit.Message) (tenantID, messageID uuid.UUID, err error) {
	if tenantID, err = uuid.Parse(m.TenantID); err != nil {
		return uuid.Nil, uuid.Nil, rabbit.Permanent(fmt.Errorf("tenant inválido: %w", err))
	}
	if messageID, err = uuid.Parse(m.ID); err != nil {
		return uuid.Nil, uuid.Nil, rabbit.Permanent(fmt.Errorf("id de mensagem inválido: %w", err))
	}
	return tenantID, messageID, nil
}

func payloadID(m rabbit.Message, field string) (uuid.UUID, error) {
	var p map[string]any
	if err := json.Unmarshal(m.Body, &p); err != nil {
		return uuid.Nil, rabbit.Permanent(fmt.Errorf("payload inválido: %w", err))
	}
	s, _ := p[field].(string)
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, rabbit.Permanent(fmt.Errorf("campo %s inválido", field))
	}
	return id, nil
}

func (w *Worker) handlers() map[string]rabbit.Handler {
	pay := w.Core.Payments
	return map[string]rabbit.Handler{
		payments.ConsumerInvoiceIssued: func(ctx context.Context, m rabbit.Message) error {
			tenantID, msgID, err := parseMessage(m)
			if err != nil {
				return err
			}
			invoiceID, err := payloadID(m, "invoice_id")
			if err != nil {
				return err
			}
			return pay.HandleInvoiceIssued(ctx, tenantID, invoiceID, msgID)
		},
		payments.ConsumerWebhooks: func(ctx context.Context, m rabbit.Message) error {
			tenantID, msgID, err := parseMessage(m)
			if err != nil {
				return err
			}
			webhookID, err := payloadID(m, "webhook_event_id")
			if err != nil {
				return err
			}
			return pay.ProcessWebhook(ctx, tenantID, webhookID, msgID)
		},
		"payments.invoice_voided": func(ctx context.Context, m rabbit.Message) error {
			tenantID, msgID, err := parseMessage(m)
			if err != nil {
				return err
			}
			invoiceID, err := payloadID(m, "invoice_id")
			if err != nil {
				return err
			}
			return pay.CancelOpenCharges(ctx, tenantID, invoiceID, msgID)
		},
		payments.ConsumerInvoiceCredited: func(ctx context.Context, m rabbit.Message) error {
			tenantID, msgID, err := parseMessage(m)
			if err != nil {
				return err
			}
			invoiceID, err := payloadID(m, "invoice_id")
			if err != nil {
				return err
			}
			return pay.HandleInvoiceCredited(ctx, tenantID, invoiceID, msgID)
		},
	}
}

// instrument acrescenta correlação de logs e contexto de rastreamento (span
// de consumo filho do contexto propagado no evento).
func instrument(consumer string, h rabbit.Handler) rabbit.Handler {
	traced := traceConsumer(consumer, h)
	return func(ctx context.Context, m rabbit.Message) error {
		ctx = telemetry.ExtractContext(ctx, m.Headers)
		ctx = logging.WithAttrs(ctx, slog.String("consumer", consumer), slog.String("message_id", m.ID), slog.String("tenant_id", m.TenantID),
			slog.String("request_id", m.Headers["request_id"]))
		return traced(ctx, m)
	}
}

// dlqRecorder grava mensagens da DLQ no banco para consulta e reprocessamento.
func (w *Worker) dlqRecorder(consumer string) rabbit.Handler {
	return func(ctx context.Context, m rabbit.Message) error {
		d := pgstore.DeadLetter{ID: ids.New(), Consumer: consumer, MessageID: m.ID, EventType: m.Type, Payload: m.Body, Headers: m.Headers,
			Error: m.Headers["x-error"], Attempts: m.Attempt, FailedAt: w.Core.Clock.Now()}
		if d.Error == "" {
			d.Error = "mensagem rejeitada"
		}
		if tid, err := uuid.Parse(m.TenantID); err == nil {
			d.TenantID = &tid
		}
		err := w.Core.Store.RecordDeadLetter(ctx, d)
		if err != nil && d.TenantID != nil {
			// Tenant inexistente ou inválido: registra sem tenant (visível só à operação).
			d.TenantID = nil
			err = w.Core.Store.RecordDeadLetter(ctx, d)
		}
		return err
	}
}

// Jobs retorna as tarefas agendadas.
func (w *Worker) Jobs() []scheduler.Job {
	c := w.Core
	return []scheduler.Job{
		{Name: "billing.run_cycle", Interval: time.Minute, Timeout: 10 * time.Minute, Run: func(ctx context.Context) error {
			res, err := c.Engine.RunAll(ctx)
			w.Metrics.InvoicesIssued.Add(float64(res.Invoices))
			return err
		}},
		{Name: "billing.correct_late_usage", Interval: 5 * time.Minute, Timeout: 10 * time.Minute, Run: func(ctx context.Context) error {
			_, err := c.Corrector.RunAll(ctx)
			return err
		}},
		{Name: "payments.recover_operations", Interval: time.Minute, Timeout: 5 * time.Minute, Run: func(ctx context.Context) error {
			_, err := c.Payments.RecoverOperations(ctx)
			return err
		}},
		{Name: "platform.close_statements", Interval: time.Hour, Timeout: 10 * time.Minute, Run: func(ctx context.Context) error {
			_, err := c.Platform.CloseDueStatements(ctx)
			return err
		}},
		{Name: "metrics.collect", Interval: 15 * time.Second, Timeout: 30 * time.Second, Run: w.collectMetrics},
		{Name: "outbox.purge", Interval: 24 * time.Hour, Timeout: 10 * time.Minute, Run: func(ctx context.Context) error {
			_, err := c.Store.PurgeOutbox(ctx, 7)
			return err
		}},
		{Name: "idempotency.purge", Interval: time.Hour, Timeout: 5 * time.Minute, Run: func(ctx context.Context) error {
			_, err := c.Store.PurgeIdempotency(ctx)
			return err
		}},
	}
}

func (w *Worker) collectMetrics(ctx context.Context) error {
	pending, oldest, err := w.Core.Store.OutboxStats(ctx)
	if err != nil {
		return err
	}
	w.Metrics.OutboxPending.Set(float64(pending))
	w.Metrics.OutboxOldestSeconds.Set(oldest)
	ops, err := w.Core.Store.PaymentOperationStats(ctx)
	if err != nil {
		return err
	}
	for _, s := range []string{"pending", "in_flight", "unknown"} {
		w.Metrics.PaymentOperations.WithLabelValues(s).Set(float64(ops[s]))
	}
	open, available, err := w.Core.Store.DatabaseConnections(ctx)
	if err != nil {
		return err
	}
	w.Metrics.DBServerConnections.Set(float64(open))
	w.Metrics.DBServerMaxConns.Set(float64(available))
	up := 0.0
	if w.Conn.Connected() {
		up = 1
	}
	w.Metrics.DependencyUp.WithLabelValues("rabbitmq").Set(up)
	return nil
}

// Run inicia conexão, relay, consumidores e agendador até ctx terminar.
func (w *Worker) Run(ctx context.Context) error {
	cfg := w.Core.Config
	log := w.Core.Log
	dlqSpecs := append([]rabbit.ConsumerSpec(nil), consumerSpecs...)
	topologyReady := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	wg.Go(func() {
		w.Conn.Run(ctx, func(conn *amqp.Connection) error {
			if err := rabbit.DeclareTopology(conn, dlqSpecs); err != nil {
				return err
			}
			once.Do(func() { close(topologyReady) })
			return nil
		})
	})
	pub := rabbit.NewPublisher(w.Conn)
	relay := outbox.NewRelay(relayStore{w.Core.Store}, eventPublisher{pub}, cfg.WorkerID, cfg.OutboxBatchSize, cfg.OutboxLease, 500*time.Millisecond, log, w.Metrics)
	jobs := traceJobs(w.Jobs())
	for _, j := range jobs {
		w.Metrics.InitJob(j.Name)
	}
	sched, err := scheduler.New(schedulerStore{w.Core.Store}, cfg.WorkerID, cfg.SchedulerTick, w.Core.Clock, log, w.Metrics, jobs...)
	if err != nil {
		return err
	}
	wg.Go(func() {
		if err := sched.Run(ctx); err != nil {
			log.Error("agendador encerrado com erro", slog.String("error", err.Error()))
		}
	})
	select {
	case <-ctx.Done():
		wg.Wait()
		return nil
	case <-topologyReady:
	}
	wg.Go(func() { relay.Run(ctx) })
	handlers := w.handlers()
	ccfg := rabbit.ConsumerConfig{Prefetch: cfg.WorkerPrefetch, Concurrency: cfg.WorkerConcurrency, HandlerTimeout: 2 * time.Minute}
	for _, spec := range consumerSpecs {
		spec.MaxAttempts = cfg.ConsumerMaxAttempts
		w.Metrics.ConsumerConcurrency.WithLabelValues(spec.Name).Set(float64(ccfg.Concurrency))
		w.Metrics.InitConsumer(spec.Name)
		c := rabbit.NewConsumer(w.Conn, pub, spec, ccfg, instrument(spec.Name, handlers[spec.Name]), log, w.Metrics)
		wg.Go(func() { c.Run(ctx) })
		dlq := rabbit.NewConsumer(w.Conn, pub, rabbit.DLQConsumerSpec(spec.Name), rabbit.ConsumerConfig{Prefetch: 4, Concurrency: 1},
			w.dlqRecorder(spec.Name), log, w.Metrics)
		queue := rabbit.DLQQueue(spec.Name)
		wg.Go(func() { dlq.ConsumeQueue(ctx, queue) })
	}
	log.Info("worker em execução", slog.Int("consumers", len(consumerSpecs)), slog.String("worker_id", cfg.WorkerID))
	<-ctx.Done()
	log.Info("encerrando worker: aguardando processamentos em andamento")
	wg.Wait()
	return nil
}
