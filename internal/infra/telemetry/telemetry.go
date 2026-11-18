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
	ChargeFailures      *prometheus.CounterVec
	PaymentOperations   *prometheus.GaugeVec
	Discrepancies       *prometheus.GaugeVec
	WebhooksReceived    *prometheus.CounterVec
	InvoicesIssued      prometheus.Counter
	DependencyUp        *prometheus.GaugeVec
	DBServerConnections prometheus.Gauge
	DBServerMaxConns    prometheus.Gauge
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
		ChargeFailures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_charge_failures_total", Help: "Falhas de criação de cobrança por provedor e tipo.", ConstLabels: cl},
			[]string{"provider", "kind"}),
		PaymentOperations: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_payment_operations", Help: "Operações externas por situação (pending, in_flight, unknown).", ConstLabels: cl},
			[]string{"status"}),
		Discrepancies: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_reconciliation_open_discrepancies", Help: "Divergências de conciliação abertas por tipo.", ConstLabels: cl},
			[]string{"kind"}),
		WebhooksReceived: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_provider_webhooks_total", Help: "Webhooks de provedores por resultado.", ConstLabels: cl},
			[]string{"provider", "outcome"}),
		InvoicesIssued: prometheus.NewCounter(prometheus.CounterOpts{Name: "recorvia_invoices_issued_total", Help: "Faturas emitidas pelo motor.", ConstLabels: cl}),
		DependencyUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_dependency_up", Help: "Disponibilidade de dependências (1 = disponível).", ConstLabels: cl},
			[]string{"dependency"}),
		DBServerConnections: prometheus.NewGauge(prometheus.GaugeOpts{Name: "recorvia_db_server_connections", Help: "Conexões de clientes abertas no servidor PostgreSQL (todas as aplicações).", ConstLabels: cl}),
		DBServerMaxConns:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "recorvia_db_server_max_connections", Help: "Limite de conexões do servidor PostgreSQL para papéis comuns (max_connections menos as reservadas).", ConstLabels: cl}),
	}
	reg.MustRegister(m.HTTPRequests, m.HTTPDuration, m.ConsumerProcessed, m.ConsumerDuration, m.ConsumerInFlight, m.ConsumerConcurrency,
		m.OutboxPublished, m.OutboxPublishFailed, m.OutboxPending, m.OutboxOldestSeconds, m.JobRuns, m.JobDuration, m.JobLastSuccess,
		m.ChargeFailures, m.PaymentOperations, m.Discrepancies, m.WebhooksReceived, m.InvoicesIssued, m.DependencyUp,
		m.DBServerConnections, m.DBServerMaxConns)
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

// InitJob cria as séries de execução da tarefa com zero. O último sucesso não
// é inicializado: só existe depois de um sucesso real.
func (m *Metrics) InitJob(name string) {
	for _, o := range []string{"success", "failure"} {
		m.JobRuns.WithLabelValues(name, o)
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
