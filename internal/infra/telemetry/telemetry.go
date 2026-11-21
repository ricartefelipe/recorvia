// Package telemetry configura métricas Prometheus e rastreamento
// OpenTelemetry. Rótulos de métricas nunca incluem identificadores de
// tenant, cliente, fatura ou usuário (cardinalidade controlada): somente
// rota, método, classe de status, consumidor, tarefa, provedor, tipo,
// situação e resultado. O catálogo das métricas e os alertas que as usam
// estão em docs/operacao/observabilidade.md.
package telemetry

import (
	"context"
	"errors"
	"os"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Metrics reúne os coletores da aplicação.
type Metrics struct {
	Registry *prometheus.Registry
	service  string

	HTTPRequests        *prometheus.CounterVec
	HTTPDuration        *prometheus.HistogramVec
	ConsumerProcessed   *prometheus.CounterVec
	ConsumerDuration    *prometheus.HistogramVec
	ConsumerInFlight    *prometheus.GaugeVec
	ConsumerConcurrency *prometheus.GaugeVec
	OutboxPublished     prometheus.Counter
	OutboxPublishFailed prometheus.Counter
	OutboxPending       prometheus.Gauge
	OutboxOldestSeconds prometheus.Gauge
	JobRuns             *prometheus.CounterVec
	JobDuration         *prometheus.HistogramVec
	JobLastSuccess      *prometheus.GaugeVec
	// JobInterval é o intervalo configurado de cada tarefa, para alertas
	// proporcionais ao intervalo (tarefa sem sucesso há vários intervalos).
	JobInterval       *prometheus.GaugeVec
	ChargeFailures    *prometheus.CounterVec
	PaymentOperations *prometheus.GaugeVec
	// PaymentOperationsExhausted conta as operações paradas depois de
	// esgotar as tentativas automáticas (pending, in_flight, unknown),
	// atualizada pela tarefa payments.recover_operations.
	PaymentOperationsExhausted *prometheus.GaugeVec
	Discrepancies              *prometheus.GaugeVec
	// ReconciliationRuns conta as execuções de conciliação em andamento por
	// estado: queued (aguardando um worker), executing (lease vigente) e
	// stalled (lease expirado, aguardando a retomada).
	ReconciliationRuns *prometheus.GaugeVec
	// ReconciliationOldestWaiting é a espera mais longa, em segundos, entre
	// as execuções que aguardam um worker ou a retomada. As duas métricas
	// são atualizadas por SetReconciliationRuns: no worker, pela tarefa
	// metrics.collect; na API, a cada coleta (ObserveReconciliationRuns).
	ReconciliationOldestWaiting prometheus.Gauge
	Refunds                     *TotalsCounter
	RefundsInProgress           *prometheus.GaugeVec
	WebhooksReceived            *prometheus.CounterVec
	InvoicesIssued              prometheus.Counter
	DependencyUp                *prometheus.GaugeVec
	DBServerConnections         prometheus.Gauge
	DBServerMaxConns            prometheus.Gauge
	// NotificationsDispatched conta os resultados do envio de notificações
	// (sent, retried, failed, deferred, skipped, lost_lease).
	NotificationsDispatched *prometheus.CounterVec
	// WebhookDeliveries conta os resultados das entregas de webhooks
	// (succeeded, retrying, exhausted, lost_lease).
	WebhookDeliveries *prometheus.CounterVec
	// CollectionActions conta as ações da régua de cobrança
	// (reminder_scheduled, invoice_overdue, subscription_suspended,
	// subscription_reactivated).
	CollectionActions *prometheus.CounterVec
}

// NewMetrics registra os coletores.
func NewMetrics(service string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	cl := prometheus.Labels{"service": service}
	m := &Metrics{Registry: reg, service: service,
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_http_requests_total", Help: "Requisições HTTP por rota, método e classe de status.", ConstLabels: cl},
			[]string{"route", "method", "status_class"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "recorvia_http_request_duration_seconds", Help: "Latência HTTP por rota.", ConstLabels: cl,
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}}, []string{"route", "method"}),
		ConsumerProcessed: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_consumer_messages_total", Help: "Mensagens processadas por consumidor e resultado (ack, retry, dead_letter, requeue).", ConstLabels: cl},
			[]string{"consumer", "outcome"}),
		ConsumerDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "recorvia_consumer_processing_seconds", Help: "Duração do processamento de mensagens.", ConstLabels: cl,
			Buckets: prometheus.DefBuckets}, []string{"consumer"}),
		ConsumerInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_consumer_in_flight", Help: "Mensagens em processamento (saturação).", ConstLabels: cl},
			[]string{"consumer"}),
		ConsumerConcurrency: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_consumer_concurrency", Help: "Concorrência máxima configurada por consumidor.", ConstLabels: cl},
			[]string{"consumer"}),
		OutboxPublished:     prometheus.NewCounter(prometheus.CounterOpts{Name: "recorvia_outbox_published_total", Help: "Eventos publicados pela outbox.", ConstLabels: cl}),
		OutboxPublishFailed: prometheus.NewCounter(prometheus.CounterOpts{Name: "recorvia_outbox_publish_failures_total", Help: "Falhas de publicação da outbox.", ConstLabels: cl}),
		OutboxPending:       prometheus.NewGauge(prometheus.GaugeOpts{Name: "recorvia_outbox_pending", Help: "Eventos pendentes na outbox.", ConstLabels: cl}),
		OutboxOldestSeconds: prometheus.NewGauge(prometheus.GaugeOpts{Name: "recorvia_outbox_oldest_pending_seconds", Help: "Idade do evento pendente mais antigo.", ConstLabels: cl}),
		// O nome da tarefa usa o rótulo "task": "job" é o rótulo que o
		// Prometheus atribui ao alvo da coleta e seria renomeado.
		JobRuns: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_job_runs_total", Help: "Execuções de tarefas agendadas por resultado.", ConstLabels: cl},
			[]string{"task", "outcome"}),
		JobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "recorvia_job_duration_seconds", Help: "Duração das tarefas agendadas.", ConstLabels: cl,
			Buckets: []float64{.1, .5, 1, 5, 15, 30, 60, 300}}, []string{"task"}),
		JobLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_job_last_success_timestamp_seconds", Help: "Último sucesso de cada tarefa.", ConstLabels: cl},
			[]string{"task"}),
		JobInterval: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_job_interval_seconds", Help: "Intervalo configurado de cada tarefa agendada.", ConstLabels: cl},
			[]string{"task"}),
		ChargeFailures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_charge_failures_total", Help: "Falhas de criação de cobrança por provedor e tipo.", ConstLabels: cl},
			[]string{"provider", "kind"}),
		PaymentOperations: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_payment_operations", Help: "Operações externas por situação (pending, in_flight, unknown).", ConstLabels: cl},
			[]string{"status"}),
		PaymentOperationsExhausted: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_payment_operations_exhausted",
			Help: "Operações externas paradas depois de esgotar as tentativas automáticas, por situação (pending, in_flight, unknown).", ConstLabels: cl},
			[]string{"status"}),
		Discrepancies: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_reconciliation_open_discrepancies", Help: "Divergências de conciliação abertas por tipo.", ConstLabels: cl},
			[]string{"kind"}),
		ReconciliationRuns: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_reconciliation_runs_in_progress",
			Help: "Execuções de conciliação em andamento por estado (queued, executing, stalled).", ConstLabels: cl}, []string{"state"}),
		ReconciliationOldestWaiting: prometheus.NewGauge(prometheus.GaugeOpts{Name: "recorvia_reconciliation_oldest_waiting_seconds",
			Help: "Espera mais longa entre as execuções de conciliação que aguardam um worker ou a retomada.", ConstLabels: cl}),
		Refunds: NewTotalsCounter("recorvia_refunds_total", "Reembolsos concluídos por resultado (succeeded, failed), totais mantidos no banco.", "outcome", cl),
		RefundsInProgress: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_refunds_in_progress", Help: "Reembolsos sem conclusão por situação (requested, unknown).", ConstLabels: cl},
			[]string{"status"}),
		WebhooksReceived: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_provider_webhooks_total", Help: "Webhooks de provedores por resultado.", ConstLabels: cl},
			[]string{"provider", "outcome"}),
		InvoicesIssued: prometheus.NewCounter(prometheus.CounterOpts{Name: "recorvia_invoices_issued_total", Help: "Faturas emitidas pelo motor.", ConstLabels: cl}),
		DependencyUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_dependency_up", Help: "Disponibilidade de dependências (1 = disponível).", ConstLabels: cl},
			[]string{"dependency"}),
		DBServerConnections: prometheus.NewGauge(prometheus.GaugeOpts{Name: "recorvia_db_server_connections", Help: "Conexões de clientes abertas no servidor PostgreSQL (todas as aplicações).", ConstLabels: cl}),
		DBServerMaxConns:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "recorvia_db_server_max_connections", Help: "Limite de conexões do servidor PostgreSQL para papéis comuns (max_connections menos as reservadas).", ConstLabels: cl}),
		NotificationsDispatched: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_notifications_dispatched_total",
			Help: "Resultados do envio de notificações por e-mail (sent, retried, failed, deferred, skipped, lost_lease).", ConstLabels: cl},
			[]string{"outcome"}),
		WebhookDeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_webhook_deliveries_total",
			Help: "Resultados das tentativas de entrega de webhooks enviados (succeeded, retrying, exhausted, lost_lease).", ConstLabels: cl},
			[]string{"outcome"}),
		CollectionActions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_collection_actions_total",
			Help: "Ações da régua de cobrança (reminder_scheduled, invoice_overdue, subscription_suspended, subscription_reactivated).", ConstLabels: cl},
			[]string{"action"}),
	}
	reg.MustRegister(m.HTTPRequests, m.HTTPDuration, m.ConsumerProcessed, m.ConsumerDuration, m.ConsumerInFlight, m.ConsumerConcurrency,
		m.OutboxPublished, m.OutboxPublishFailed, m.OutboxPending, m.OutboxOldestSeconds, m.JobRuns, m.JobDuration, m.JobLastSuccess,
		m.JobInterval, m.ChargeFailures, m.PaymentOperations, m.PaymentOperationsExhausted, m.Discrepancies, m.ReconciliationRuns,
		m.ReconciliationOldestWaiting, m.Refunds, m.RefundsInProgress,
		m.WebhooksReceived, m.InvoicesIssued, m.DependencyUp, m.DBServerConnections, m.DBServerMaxConns,
		m.NotificationsDispatched, m.WebhookDeliveries, m.CollectionActions)
	return m
}

// DBPoolStats é a fotografia do pool de conexões do processo no momento da
// coleta.
type DBPoolStats struct {
	Acquired     int32
	Idle         int32
	Constructing int32
	Max          int32
	// Acquires conta todas as aquisições concluídas; EmptyAcquires, as que
	// esperaram porque não havia conexão ociosa; CanceledAcquires, as
	// abandonadas por cancelamento do contexto.
	Acquires         int64
	EmptyAcquires    int64
	CanceledAcquires int64
	// EmptyAcquireWait é o tempo acumulado de espera das aquisições sem
	// conexão ociosa.
	EmptyAcquireWait time.Duration
}

// dbPoolCollector lê as estatísticas do pool a cada coleta (sem estado
// próprio nem goroutines).
type dbPoolCollector struct {
	stats                                            func() DBPoolStats
	conns, maxConns, acquires, empty, canceled, wait *prometheus.Desc
}

func newDBPoolCollector(service string, stats func() DBPoolStats) *dbPoolCollector {
	cl := prometheus.Labels{"service": service}
	return &dbPoolCollector{stats: stats,
		conns:    prometheus.NewDesc("recorvia_db_pool_connections", "Conexões do pool por estado (acquired, idle, constructing).", []string{"state"}, cl),
		maxConns: prometheus.NewDesc("recorvia_db_pool_max_connections", "Tamanho máximo configurado do pool.", nil, cl),
		acquires: prometheus.NewDesc("recorvia_db_pool_acquires_total", "Aquisições de conexão concluídas.", nil, cl),
		empty:    prometheus.NewDesc("recorvia_db_pool_empty_acquires_total", "Aquisições que esperaram por falta de conexão ociosa.", nil, cl),
		canceled: prometheus.NewDesc("recorvia_db_pool_canceled_acquires_total", "Aquisições abandonadas por cancelamento do contexto.", nil, cl),
		wait:     prometheus.NewDesc("recorvia_db_pool_empty_acquire_wait_seconds_total", "Tempo acumulado de espera por conexão.", nil, cl),
	}
}

// RegisterDBPool registra as métricas do pool de conexões do banco, lidas de
// stats a cada coleta.
func (m *Metrics) RegisterDBPool(stats func() DBPoolStats) {
	m.Registry.MustRegister(newDBPoolCollector(m.service, stats))
}

// ReconciliationRunStats são as execuções de conciliação em andamento, de
// todos os tenants, por estado, e a espera mais longa entre as que aguardam
// um worker (desde o registro) ou a retomada (desde o fim do lease).
type ReconciliationRunStats struct {
	Queued               int64
	Executing            int64
	Stalled              int64
	OldestWaitingSeconds float64
}

// SetReconciliationRuns publica as execuções de conciliação em andamento.
func (m *Metrics) SetReconciliationRuns(s ReconciliationRunStats) {
	m.ReconciliationRuns.WithLabelValues("queued").Set(float64(s.Queued))
	m.ReconciliationRuns.WithLabelValues("executing").Set(float64(s.Executing))
	m.ReconciliationRuns.WithLabelValues("stalled").Set(float64(s.Stalled))
	m.ReconciliationOldestWaiting.Set(s.OldestWaitingSeconds)
}

// reconciliationRunsCollector lê as execuções de conciliação em andamento a
// cada coleta, para o processo que não tem a tarefa metrics.collect.
type reconciliationRunsCollector struct {
	m       *Metrics
	timeout time.Duration
	read    func(ctx context.Context) (ReconciliationRunStats, error)
}

// ObserveReconciliationRuns passa a atualizar, a cada coleta, as métricas das
// execuções de conciliação em andamento com a leitura de read (prazo
// timeout). É o modo da API, que registra os pedidos de conciliação e
// continua em execução quando nenhum worker está: a execução registrada e
// não assumida aparece nas métricas mesmo sem worker para exportá-las. Se a
// leitura falhar ou não terminar no prazo, as séries mantêm o último valor
// lido (a indisponibilidade do banco aparece em recorvia_dependency_up).
// Deve ser chamada uma vez, antes de expor as métricas.
func (m *Metrics) ObserveReconciliationRuns(timeout time.Duration, read func(ctx context.Context) (ReconciliationRunStats, error)) {
	m.Registry.Unregister(m.ReconciliationRuns)
	m.Registry.Unregister(m.ReconciliationOldestWaiting)
	m.Registry.MustRegister(&reconciliationRunsCollector{m: m, timeout: timeout, read: read})
}

func (c *reconciliationRunsCollector) Describe(ch chan<- *prometheus.Desc) {
	c.m.ReconciliationRuns.Describe(ch)
	c.m.ReconciliationOldestWaiting.Describe(ch)
}

func (c *reconciliationRunsCollector) Collect(ch chan<- prometheus.Metric) {
	type result struct {
		stats ReconciliationRunStats
		err   error
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	done := make(chan result, 1)
	go func() {
		s, err := c.read(ctx)
		done <- result{s, err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			c.m.SetReconciliationRuns(r.stats)
		}
	case <-ctx.Done():
		// A leitura que ignora o cancelamento não trava a coleta.
	}
	c.m.ReconciliationRuns.Collect(ch)
	c.m.ReconciliationOldestWaiting.Collect(ch)
}

// DependencyCheck verifica uma dependência; nil indica disponível.
type DependencyCheck func(ctx context.Context) error

// ProbeDependencies executa as verificações em paralelo, cada uma com prazo
// próprio (timeout a partir da chamada, limitado pelo prazo de ctx), e
// devolve o resultado por dependência. A demora de uma dependência (por
// exemplo, o pool do banco esgotado) não consome o prazo das outras, que
// assim não aparecem como indisponíveis sem estar. Uma verificação que não
// retorna no prazo conta como indisponível, com o erro do contexto, mesmo
// que ignore o cancelamento.
func ProbeDependencies(ctx context.Context, timeout time.Duration, checks map[string]DependencyCheck) map[string]error {
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(checks))
	for name, check := range checks {
		go func() {
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- check(cctx) }()
			select {
			case err := <-done:
				results <- result{name, err}
			case <-cctx.Done():
				results <- result{name, cctx.Err()}
			}
		}()
	}
	out := make(map[string]error, len(checks))
	for range len(checks) {
		r := <-results
		out[r.name] = r.err
	}
	return out
}

// dependencyCollector verifica as dependências a cada coleta, para que
// recorvia_dependency_up reflita o estado atual mesmo sem consultas a
// /readyz (a sonda dos containers usa /livez).
type dependencyCollector struct {
	gauge   *prometheus.GaugeVec
	timeout time.Duration
	checks  map[string]DependencyCheck
}

// ObserveDependencies passa a atualizar recorvia_dependency_up a cada coleta
// com o resultado das verificações (ProbeDependencies, com prazo timeout
// para cada uma). Deve ser chamada uma vez, antes de expor as métricas.
func (m *Metrics) ObserveDependencies(timeout time.Duration, checks map[string]DependencyCheck) {
	m.Registry.Unregister(m.DependencyUp)
	m.Registry.MustRegister(&dependencyCollector{gauge: m.DependencyUp, timeout: timeout, checks: checks})
}

func (c *dependencyCollector) Describe(ch chan<- *prometheus.Desc) { c.gauge.Describe(ch) }

func (c *dependencyCollector) Collect(ch chan<- prometheus.Metric) {
	for name, err := range ProbeDependencies(context.Background(), c.timeout, c.checks) {
		up := 1.0
		if err != nil {
			up = 0
		}
		c.gauge.WithLabelValues(name).Set(up)
	}
	c.gauge.Collect(ch)
}

func (c *dbPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.conns, c.maxConns, c.acquires, c.empty, c.canceled, c.wait} {
		ch <- d
	}
}

func (c *dbPoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.stats()
	ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, float64(s.Acquired), "acquired")
	ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, float64(s.Idle), "idle")
	ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, float64(s.Constructing), "constructing")
	ch <- prometheus.MustNewConstMetric(c.maxConns, prometheus.GaugeValue, float64(s.Max))
	ch <- prometheus.MustNewConstMetric(c.acquires, prometheus.CounterValue, float64(s.Acquires))
	ch <- prometheus.MustNewConstMetric(c.empty, prometheus.CounterValue, float64(s.EmptyAcquires))
	ch <- prometheus.MustNewConstMetric(c.canceled, prometheus.CounterValue, float64(s.CanceledAcquires))
	ch <- prometheus.MustNewConstMetric(c.wait, prometheus.CounterValue, s.EmptyAcquireWait.Seconds())
}

// TotalsCounter expõe como contador Prometheus totais monotônicos mantidos
// fora do processo (por exemplo, contagens no banco lidas pelo job
// metrics.collect). O valor nunca diminui: leituras menores que a anterior
// são ignoradas. Sem leitura, não expõe séries.
type TotalsCounter struct {
	desc   *prometheus.Desc
	mu     sync.Mutex
	values map[string]float64
}

// NewTotalsCounter cria o coletor com um rótulo de baixa cardinalidade.
func NewTotalsCounter(name, help, label string, constLabels prometheus.Labels) *TotalsCounter {
	return &TotalsCounter{desc: prometheus.NewDesc(name, help, []string{label}, constLabels), values: map[string]float64{}}
}

// Set registra o total lido para o valor do rótulo.
func (c *TotalsCounter) Set(labelValue string, total float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.values[labelValue]; !ok || total > cur {
		c.values[labelValue] = total
	}
}

// Describe implementa prometheus.Collector.
func (c *TotalsCounter) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect implementa prometheus.Collector.
func (c *TotalsCounter) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	labels := make([]string, 0, len(c.values))
	for l := range c.values {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	values := make([]float64, len(labels))
	for i, l := range labels {
		values[i] = c.values[l]
	}
	c.mu.Unlock()
	for i, l := range labels {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.CounterValue, values[i], l)
	}
}

// Processed implementa rabbit.Observer.
func (m *Metrics) Processed(consumer, outcome string, d time.Duration) {
	m.ConsumerProcessed.WithLabelValues(consumer, outcome).Inc()
	m.ConsumerDuration.WithLabelValues(consumer).Observe(d.Seconds())
}

// InFlight implementa rabbit.Observer.
func (m *Metrics) InFlight(consumer string, delta int) {
	m.ConsumerInFlight.WithLabelValues(consumer).Add(float64(delta))
}

// Published implementa outbox.RelayObserver.
func (m *Metrics) Published(n int) { m.OutboxPublished.Add(float64(n)) }

// PublishFailed implementa outbox.RelayObserver.
func (m *Metrics) PublishFailed() { m.OutboxPublishFailed.Inc() }

// ConsumerOutcomes são os resultados possíveis do processamento de uma
// mensagem (rótulo outcome de recorvia_consumer_messages_total).
var ConsumerOutcomes = []string{"ack", "retry", "dead_letter", "requeue"}

// ChargeFailureKinds são os tipos de falha na criação de cobranças (rótulo
// kind de recorvia_charge_failures_total).
var ChargeFailureKinds = []string{"permanent", "retryable", "ambiguous", "not_found"}

// As séries de contadores são criadas com zero na inicialização: sem isso, a
// primeira ocorrência cria a série já com valor 1 e increase() não a detecta,
// atrasando os alertas de DLQ, falhas de tarefa e falhas de cobrança.

// InitConsumer cria as séries do consumidor com zero.
func (m *Metrics) InitConsumer(consumer string) {
	for _, o := range ConsumerOutcomes {
		m.ConsumerProcessed.WithLabelValues(consumer, o)
	}
}

// InitJob cria as séries de execução da tarefa com zero e registra o
// intervalo configurado. O último sucesso não é inicializado: só existe
// depois de um sucesso real (os alertas tratam a ausência pela idade do
// processo).
func (m *Metrics) InitJob(name string, interval time.Duration) {
	for _, o := range []string{"success", "failure"} {
		m.JobRuns.WithLabelValues(name, o)
	}
	m.JobInterval.WithLabelValues(name).Set(interval.Seconds())
}

// ProviderWebhookOutcomes são os resultados do recebimento de webhooks de
// provedores (rótulo outcome de recorvia_provider_webhooks_total).
var ProviderWebhookOutcomes = []string{"accepted", "duplicate", "unauthenticated", "unknown_endpoint", "invalid", "too_large", "error"}

// OtherProvider substitui, no rótulo provider, os provedores fora da lista
// conhecida (o nome vem da URL e não pode criar séries novas).
const OtherProvider = "other"

// InitProviderWebhooks cria com zero as séries de recebimento de webhooks
// dos provedores conhecidos e de OtherProvider.
func (m *Metrics) InitProviderWebhooks(providers []string) {
	for _, p := range append(slices.Clone(providers), OtherProvider) {
		for _, o := range ProviderWebhookOutcomes {
			m.WebhooksReceived.WithLabelValues(p, o)
		}
	}
}

// Resultados das tarefas de envio e da régua de cobrança (rótulos de
// recorvia_notifications_dispatched_total, recorvia_webhook_deliveries_total
// e recorvia_collection_actions_total).
var (
	NotificationOutcomes    = []string{"sent", "retried", "failed", "deferred", "skipped", "lost_lease"}
	WebhookDeliveryOutcomes = []string{"succeeded", "retrying", "exhausted", "lost_lease", "deferred"}
	CollectionActionKinds   = []string{"reminder_scheduled", "invoice_overdue", "subscription_suspended", "subscription_reactivated"}
)

// InitDispatchResults cria com zero as séries de resultados do envio de
// notificações, das entregas de webhooks e das ações da régua.
func (m *Metrics) InitDispatchResults() {
	for _, o := range NotificationOutcomes {
		m.NotificationsDispatched.WithLabelValues(o)
	}
	for _, o := range WebhookDeliveryOutcomes {
		m.WebhookDeliveries.WithLabelValues(o)
	}
	for _, a := range CollectionActionKinds {
		m.CollectionActions.WithLabelValues(a)
	}
}

// InitChargeFailures cria as séries de falhas de cobrança dos provedores com
// zero.
func (m *Metrics) InitChargeFailures(providers []string) {
	for _, p := range providers {
		for _, k := range ChargeFailureKinds {
			m.ChargeFailures.WithLabelValues(p, k)
		}
	}
}

// ChargeFailed implementa gateways.ChargeObserver: conta falhas das chamadas
// que criam cobranças no provedor, por provedor e tipo de falha.
func (m *Metrics) ChargeFailed(provider, kind string) {
	m.ChargeFailures.WithLabelValues(provider, kind).Inc()
}

// JobFinished implementa scheduler.Observer.
func (m *Metrics) JobFinished(name string, d time.Duration, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	} else {
		m.JobLastSuccess.WithLabelValues(name).SetToCurrentTime()
	}
	m.JobRuns.WithLabelValues(name, outcome).Inc()
	m.JobDuration.WithLabelValues(name).Observe(d.Seconds())
}

// SetupTracing configura o OpenTelemetry. Sem OTEL_EXPORTER_OTLP_ENDPOINT,
// apenas a propagação de contexto é habilitada (sem exportação).
func SetupTracing(ctx context.Context, service, version string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}
	// Atributos sem URL de esquema: o recurso padrão do SDK usa a versão de
	// convenções semânticas do próprio SDK, e a fusão com outra versão falha
	// ("conflicting Schema URL"), impedindo a partida com o OTLP habilitado.
	res, err := sdkresource.Merge(sdkresource.Default(), sdkresource.NewSchemaless(
		semconv.ServiceName(service), semconv.ServiceVersion(version)))
	if err != nil && !errors.Is(err, sdkresource.ErrPartialResource) {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(1.0))))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// InjectHeaders acrescenta o contexto de rastreamento a um mapa de headers
// (eventos da outbox e mensagens do broker).
func InjectHeaders(ctx context.Context, h map[string]string) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(h))
}

// ExtractContext recupera o contexto de rastreamento de headers.
func ExtractContext(ctx context.Context, h map[string]string) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(h))
}
