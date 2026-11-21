package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/retention"
	"github.com/ricartefelipe/recorvia/internal/scheduler"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
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
	{Name: notifications.ConsumerInvoiceIssued, EventTypes: []string{outbox.EventInvoiceIssued}},
	{Name: notifications.ConsumerPaymentConfirmed, EventTypes: []string{outbox.EventPaymentConfirmed}},
	{Name: notifications.ConsumerMemberInvited, EventTypes: []string{outbox.EventMemberInvited}},
	{Name: collections.ConsumerInvoiceResolved, EventTypes: []string{outbox.EventInvoicePaid, outbox.EventInvoiceVoided,
		outbox.EventInvoiceUncollectible, outbox.EventInvoiceCredited}},
	{Name: webhooks.ConsumerFanout, EventTypes: webhooks.EventTypes()},
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

// idHandler extrai tenant, mensagem e um identificador do payload.
func idHandler(field string, fn func(ctx context.Context, tenantID, id, messageID uuid.UUID) error) rabbit.Handler {
	return func(ctx context.Context, m rabbit.Message) error {
		tenantID, msgID, err := parseMessage(m)
		if err != nil {
			return err
		}
		id, err := payloadID(m, field)
		if err != nil {
			return err
		}
		return fn(ctx, tenantID, id, msgID)
	}
}

func (w *Worker) memberInvited(ctx context.Context, m rabbit.Message) error {
	tenantID, msgID, err := parseMessage(m)
	if err != nil {
		return err
	}
	var p struct {
		InvitationID    uuid.UUID `json:"invitation_id"`
		TokenCiphertext string    `json:"token_ciphertext"`
		TokenKeyID      string    `json:"token_key_id"`
	}
	if err := json.Unmarshal(m.Body, &p); err != nil || p.InvitationID == uuid.Nil {
		return rabbit.Permanent(fmt.Errorf("payload de convite inválido"))
	}
	return w.Core.Notifications.HandleMemberInvited(ctx, tenantID, msgID, notifications.MemberInvited{InvitationID: p.InvitationID,
		TokenCiphertext: p.TokenCiphertext, TokenKeyID: p.TokenKeyID})
}

func (w *Worker) webhookFanout(ctx context.Context, m rabbit.Message) error {
	tenantID, msgID, err := parseMessage(m)
	if err != nil {
		return err
	}
	return w.Core.Webhooks.HandleEvent(ctx, tenantID, msgID, m.Type, m.OccurredAt, m.Body)
}

func (w *Worker) handlers() map[string]rabbit.Handler {
	pay, notif := w.Core.Payments, w.Core.Notifications
	return map[string]rabbit.Handler{
		notifications.ConsumerInvoiceIssued: idHandler("invoice_id", func(ctx context.Context, t, id, msg uuid.UUID) error {
			return notif.HandleInvoiceIssued(ctx, t, id, msg)
		}),
		notifications.ConsumerPaymentConfirmed: idHandler("payment_id", func(ctx context.Context, t, id, msg uuid.UUID) error {
			return notif.HandlePaymentConfirmed(ctx, t, id, msg)
		}),
		notifications.ConsumerMemberInvited: w.memberInvited,
		collections.ConsumerInvoiceResolved: idHandler("invoice_id", func(ctx context.Context, t, id, msg uuid.UUID) error {
			return w.Core.Collections.HandleInvoiceResolved(ctx, t, id, msg)
		}),
		webhooks.ConsumerFanout: w.webhookFanout,
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
// de consumo filho do contexto propagado no evento). Os logs do handler e o
// registro do processamento de cada mensagem levam consumer, message_id,
// tenant_id, o request_id da requisição que originou o evento (gravado na
// outbox) e trace_id e span_id do span de consumo; a auditoria gravada pelo
// consumidor recebe o mesmo request_id. O registro de cada mensagem é em
// debug, nos dois desfechos: o aviso de falha é do consumidor (rabbit).
func instrument(consumer string, h rabbit.Handler, log *slog.Logger) rabbit.Handler {
	logged := func(ctx context.Context, m rabbit.Message) error {
		start := time.Now()
		err := h(ctx, m)
		if err != nil {
			// Em debug: o aviso da falha, com a decisão (retentativa, DLQ ou
			// devolução), é registrado uma vez pelo consumidor (rabbit).
			log.DebugContext(ctx, "falha no processamento da mensagem", slog.String("event_type", m.Type), slog.Int("attempt", m.Attempt),
				slog.String("error", err.Error()))
			return err
		}
		log.DebugContext(ctx, "mensagem processada", slog.String("event_type", m.Type), slog.Int("attempt", m.Attempt),
			slog.Duration("duration", time.Since(start)))
		return nil
	}
	traced := traceConsumer(consumer, logged)
	return func(ctx context.Context, m rabbit.Message) error {
		ctx = telemetry.ExtractContext(ctx, m.Headers)
		attrs := []slog.Attr{slog.String("consumer", consumer), slog.String("message_id", m.ID), slog.String("tenant_id", m.TenantID)}
		if rid := m.Headers["request_id"]; rid != "" {
			attrs = append(attrs, slog.String("request_id", rid))
		}
		return traced(logging.WithAttrs(ctx, attrs...), m)
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
			res, err := c.Payments.RecoverOperations(ctx)
			recordRecovery(w.Metrics, res)
			return err
		}},
		{Name: JobCollections, Interval: 5 * time.Minute, Timeout: 10 * time.Minute, Run: func(ctx context.Context) error {
			res, err := c.Collections.Run(ctx)
			recordCollections(w.Metrics, res)
			return err
		}},
		{Name: JobNotifications, Interval: 15 * time.Second, Timeout: 3 * time.Minute, Run: func(ctx context.Context) error {
			res, err := c.Notifications.Dispatch(ctx)
			recordDispatch(w.Metrics, res)
			return err
		}},
		{Name: JobWebhooks, Interval: 10 * time.Second, Timeout: 3 * time.Minute, Run: func(ctx context.Context) error {
			res, err := c.Webhooks.Deliver(ctx)
			recordDeliveries(w.Metrics, res)
			return err
		}},
		// Conciliação agendada dos tenants com o recurso
		// reconciliation_automation (no máximo uma por tenant a cada
		// payments.ScheduledReconciliationEvery).
		{Name: "payments.reconcile", Interval: time.Hour, Timeout: 50 * time.Minute, Run: func(ctx context.Context) error {
			_, err := c.Payments.RunScheduledReconciliations(ctx)
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
		// Expurgo por idade dos dados técnicos (ADR-060): cada execução é
		// limitada pelo orçamento da política e continua na seguinte.
		{Name: JobRetention, Interval: time.Hour, Timeout: 10 * time.Minute, Run: func(ctx context.Context) error {
			res, err := c.Retention.Run(ctx)
			recordRetention(w.Metrics, res)
			return err
		}},
	}
}

// Executor das conciliações registradas (ADR-071). A execução manual pedida
// pela API fica registrada em reconciliation_runs e é assumida aqui, com
// lease; a interrompida (worker reiniciado) é retomada pelo mesmo caminho. O
// executor roda fora do agendador, que executa uma tarefa por vez: uma
// conciliação longa não atrasa as demais tarefas.
const (
	// reconciliationRunners é o número de execuções simultâneas por worker.
	reconciliationRunners = 2
	// reconciliationPoll é o intervalo entre as consultas por execuções a
	// assumir: limita a espera de quem pediu a conciliação manual.
	reconciliationPoll = 2 * time.Second
	// JobRegisteredReconciliations identifica o executor nos logs (job).
	JobRegisteredReconciliations = "payments.registered_reconciliations"
)

// reconciliationPass assume e executa as conciliações registradas que
// aguardam um worker. Pânico inesperado vira erro, para que o executor
// continue (o lease da execução em andamento expira e ela é retomada).
func (w *Worker) reconciliationPass(ctx context.Context) (res payments.PendingRunsResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pânico no executor de conciliações: %v", r)
		}
	}()
	return w.Core.Payments.RunPendingReconciliations(ctx)
}

// runReconciliations executa reconciliationPass na partida e a cada sinal de
// ticks, até ctx terminar. Com o worker encerrando, a execução em andamento é
// devolvida e retomada por outra instância ou na próxima partida.
func (w *Worker) runReconciliations(ctx context.Context, ticks <-chan time.Time) {
	for {
		res, err := w.reconciliationPass(ctx)
		if n := res.Completed + res.Failed + res.Released; n > 0 {
			w.Core.Log.InfoContext(ctx, "conciliações registradas executadas", slog.Int("completed", res.Completed),
				slog.Int("failed", res.Failed), slog.Int("released", res.Released))
		}
		if err != nil && ctx.Err() == nil {
			w.Core.Log.WarnContext(ctx, "executor de conciliações falhou", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

// Tarefas da régua de cobrança, do envio de notificações, das entregas de
// webhooks e do expurgo por idade.
const (
	JobCollections   = "collections.run"
	JobNotifications = "notifications.dispatch"
	JobWebhooks      = "webhooks.deliver"
	JobRetention     = "retention.purge"
)

func addCount(v *prometheus.CounterVec, label string, n int) {
	if n > 0 {
		v.WithLabelValues(label).Add(float64(n))
	}
}

// recordCollections registra as ações de uma execução da régua (resultados
// parciais também são contados quando algum tenant falha).
func recordCollections(m *telemetry.Metrics, r collections.RunResult) {
	addCount(m.CollectionActions, "reminder_scheduled", r.Reminders)
	addCount(m.CollectionActions, "invoice_overdue", r.Overdue)
	addCount(m.CollectionActions, "subscription_suspended", r.Suspended)
	addCount(m.CollectionActions, "subscription_reactivated", r.Reactivated)
}

// recordRetention registra os resultados de uma execução do expurgo por
// idade: registros removidos, tipos com falha e tipos interrompidos com
// registros vencidos restantes (resultados parciais também são contados).
func recordRetention(m *telemetry.Metrics, r retention.Result) {
	for k, n := range r.Purged {
		addCount(m.RetentionPurged, string(k), n)
	}
	for _, k := range r.Failed {
		m.RetentionPurgeFailures.WithLabelValues(string(k)).Inc()
	}
	for _, k := range r.Incomplete {
		m.RetentionPurgeIncomplete.WithLabelValues(string(k)).Inc()
	}
}

// exhaustedStatuses são as situações exportadas em
// recorvia_payment_operations_exhausted.
var exhaustedStatuses = []payments.OperationStatus{payments.OpPending, payments.OpInFlight, payments.OpUnknown}

// recordRecovery atualiza a contagem de operações esgotadas. Uma execução
// parcial (tenant sem consulta ou cancelamento) mantém a contagem anterior:
// zerar com dados incompletos resolveria o alerta sem a causa ter sido
// tratada.
func recordRecovery(m *telemetry.Metrics, r payments.RecoveryResult) {
	if !r.Complete {
		return
	}
	for _, s := range exhaustedStatuses {
		m.PaymentOperationsExhausted.WithLabelValues(string(s)).Set(float64(r.Exhausted[s]))
	}
}

// recordDispatch registra os resultados de uma execução do envio.
func recordDispatch(m *telemetry.Metrics, r notifications.DispatchResult) {
	addCount(m.NotificationsDispatched, "sent", r.Sent)
	addCount(m.NotificationsDispatched, "retried", r.Retried)
	addCount(m.NotificationsDispatched, "failed", r.Failed)
	addCount(m.NotificationsDispatched, "deferred", r.Deferred)
	addCount(m.NotificationsDispatched, "skipped", r.Skipped)
	addCount(m.NotificationsDispatched, "lost_lease", r.LostLease)
}

// recordDeliveries registra os resultados de uma execução das entregas.
func recordDeliveries(m *telemetry.Metrics, r webhooks.DeliverResult) {
	addCount(m.WebhookDeliveries, "succeeded", r.Succeeded)
	addCount(m.WebhookDeliveries, "retrying", r.Retrying)
	addCount(m.WebhookDeliveries, "exhausted", r.Exhausted)
	addCount(m.WebhookDeliveries, "lost_lease", r.LostLease)
	addCount(m.WebhookDeliveries, "deferred", r.Deferred)
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
	conns, available, err := w.Core.Store.DatabaseConnections(ctx)
	if err != nil {
		return err
	}
	w.Metrics.DBServerConnections.Set(float64(conns))
	w.Metrics.DBServerMaxConns.Set(float64(available))
	open, err := w.Core.Store.OpenDiscrepancyStats(ctx)
	if err != nil {
		return err
	}
	for _, k := range payments.DiscrepancyKinds {
		w.Metrics.Discrepancies.WithLabelValues(string(k)).Set(float64(open[string(k)]))
	}
	refunds, err := w.Core.Store.RefundStats(ctx)
	if err != nil {
		return err
	}
	for _, s := range []payments.RefundStatus{payments.RefundSucceeded, payments.RefundFailed} {
		w.Metrics.Refunds.Set(string(s), float64(refunds[string(s)]))
	}
	for _, s := range []payments.RefundStatus{payments.RefundRequested, payments.RefundUnknown} {
		w.Metrics.RefundsInProgress.WithLabelValues(string(s)).Set(float64(refunds[string(s)]))
	}
	if err := w.collectQueueMetrics(ctx); err != nil {
		return err
	}
	runs, err := w.Core.Store.ReconciliationRunStats(ctx)
	if err != nil {
		return err
	}
	w.Metrics.SetReconciliationRuns(reconciliationRunStats(runs))
	up := 0.0
	if w.Conn.Connected() {
		up = 1
	}
	w.Metrics.DependencyUp.WithLabelValues("rabbitmq").Set(up)
	return nil
}

// notificationFailedWindow é a janela das falhas definitivas contadas em
// recorvia_notifications_backlog{state="failed"}: a situação é final e, sem
// janela, o medidor só cresceria.
const notificationFailedWindow = 24 * time.Hour

// collectQueueMetrics atualiza as filas medidas no banco (ADR-066): dead
// letters abertas por consumidor, notificações e entregas de webhooks. O
// instante de referência é o do relógio da aplicação.
func (w *Worker) collectQueueMetrics(ctx context.Context) error {
	now := w.Core.Clock.Now()
	dead, err := w.Core.Store.DeadLetterStats(ctx, now)
	if err != nil {
		return err
	}
	recordDeadLetters(w.Metrics, consumerNames(), dead)
	queued, err := w.Core.Store.NotificationBacklogStats(ctx, now, now.Add(-notificationFailedWindow))
	if err != nil {
		return err
	}
	recordQueue(w.Metrics.NotificationQueue(), telemetry.NotificationBacklogStates, queued)
	deliveries, err := w.Core.Store.WebhookDeliveryBacklogStats(ctx, now)
	if err != nil {
		return err
	}
	recordQueue(w.Metrics.WebhookDeliveryQueue(), telemetry.WebhookBacklogStates, deliveries)
	return nil
}

// consumerNames são os consumidores do worker (rótulo consumer).
func consumerNames() []string {
	names := make([]string, 0, len(consumerSpecs))
	for _, spec := range consumerSpecs {
		names = append(names, spec.Name)
	}
	return names
}

// recordDeadLetters grava as dead letters abertas de cada consumidor
// conhecido (zero quando não há) e reúne em telemetry.OtherConsumer as de
// consumidores que o worker não tem mais: o nome vem do banco e não cria
// séries novas.
func recordDeadLetters(m *telemetry.Metrics, consumers []string, stats map[string]pgstore.BacklogStat) {
	var other pgstore.BacklogStat
	for name, s := range stats {
		if !slices.Contains(consumers, name) {
			other.Total += s.Total
			other.OldestSeconds = max(other.OldestSeconds, s.OldestSeconds)
		}
	}
	g := m.DeadLetters()
	for _, name := range consumers {
		g.Set(name, stats[name].Total, stats[name].OldestSeconds)
	}
	g.Set(telemetry.OtherConsumer, other.Total, other.OldestSeconds)
}

// recordQueue grava o tamanho e a idade de cada situação declarada da fila;
// as ausentes da leitura valem zero e as não declaradas são ignoradas.
func recordQueue(g telemetry.QueueGauge, states []string, stats map[string]pgstore.BacklogStat) {
	for _, state := range states {
		g.Set(state, stats[state].Total, stats[state].OldestSeconds)
	}
}

// initMetrics cria com zero as séries das tarefas (execuções e intervalo) e
// dos resultados de envio e da régua, para que increase() detecte a primeira
// ocorrência e os alertas proporcionais ao intervalo tenham referência.
func (w *Worker) initMetrics(jobs []scheduler.Job) {
	for _, j := range jobs {
		w.Metrics.InitJob(j.Name, j.Interval)
	}
	w.Metrics.InitDispatchResults()
	w.Metrics.InitRetention()
}

// Run inicia conexão, relay, consumidores e agendador até ctx terminar. No
// encerramento, o cancelamento chega ao agendador (e à tarefa em execução),
// ao executor de conciliações, ao relay e aos consumidores, que param de
// receber mensagens; Run só retorna depois que todos terminam. A conexão com
// o broker fecha por último: as mensagens em processamento terminam e são
// confirmadas (ou encaminhadas à retentativa) na mesma conexão em que
// chegaram.
func (w *Worker) Run(ctx context.Context) error {
	cfg := w.Core.Config
	log := w.Core.Log
	dlqSpecs := append([]rabbit.ConsumerSpec(nil), consumerSpecs...)
	topologyReady := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	connCtx, closeConn := context.WithCancel(context.WithoutCancel(ctx))
	var connDone sync.WaitGroup
	defer func() {
		closeConn()
		connDone.Wait()
	}()
	connDone.Go(func() {
		w.Conn.Run(connCtx, func(conn *amqp.Connection) error {
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
	w.initMetrics(jobs)
	sched, err := scheduler.New(schedulerStore{w.Core.Store}, cfg.WorkerID, cfg.SchedulerTick, w.Core.Clock, log, w.Metrics, jobs...)
	if err != nil {
		return err
	}
	wg.Go(func() {
		if err := sched.Run(ctx); err != nil {
			log.Error("agendador encerrado com erro", slog.String("error", err.Error()))
		}
	})
	// As conciliações registradas não dependem do RabbitMQ: o executor
	// parte com o agendador. Os logs do executor levam o nome da tarefa,
	// como os das tarefas agendadas.
	recCtx := logging.WithAttrs(ctx, slog.String("job", JobRegisteredReconciliations))
	for range reconciliationRunners {
		wg.Go(func() {
			t := time.NewTicker(reconciliationPoll)
			defer t.Stop()
			w.runReconciliations(recCtx, t.C)
		})
	}
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
		c := rabbit.NewConsumer(w.Conn, pub, spec, ccfg, instrument(spec.Name, handlers[spec.Name], log), log, w.Metrics)
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
