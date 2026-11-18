// Package telemetry configura métricas Prometheus e rastreamento
// OpenTelemetry. Rótulos de métricas nunca incluem identificadores de
// tenant, cliente, fatura ou usuário (cardinalidade controlada): somente
// rota, método, classe de status, consumidor, tarefa e resultado.
package telemetry

import (
	"context"
	"errors"
	"os"
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
	Refunds             *TotalsCounter
	RefundsInProgress   *prometheus.GaugeVec
	WebhooksReceived    *prometheus.CounterVec
	InvoicesIssued      prometheus.Counter
	DependencyUp        *prometheus.GaugeVec
}

// NewMetrics registra os coletores.
func NewMetrics(service string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	cl := prometheus.Labels{"service": service}
	m := &Metrics{Registry: reg,
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
		JobRuns: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_job_runs_total", Help: "Execuções de tarefas agendadas por resultado.", ConstLabels: cl},
			[]string{"job", "outcome"}),
		JobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "recorvia_job_duration_seconds", Help: "Duração das tarefas agendadas.", ConstLabels: cl,
			Buckets: []float64{.1, .5, 1, 5, 15, 30, 60, 300}}, []string{"job"}),
		JobLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_job_last_success_timestamp_seconds", Help: "Último sucesso de cada tarefa.", ConstLabels: cl},
			[]string{"job"}),
		ChargeFailures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_charge_failures_total", Help: "Falhas de criação de cobrança por provedor e tipo.", ConstLabels: cl},
			[]string{"provider", "kind"}),
		PaymentOperations: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_payment_operations", Help: "Operações externas por situação (pending, in_flight, unknown).", ConstLabels: cl},
			[]string{"status"}),
		Discrepancies: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_reconciliation_open_discrepancies", Help: "Divergências de conciliação abertas por tipo.", ConstLabels: cl},
			[]string{"kind"}),
		Refunds: NewTotalsCounter("recorvia_refunds_total", "Reembolsos concluídos por resultado (succeeded, failed), totais mantidos no banco.", "outcome", cl),
		RefundsInProgress: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_refunds_in_progress", Help: "Reembolsos sem conclusão por situação (requested, unknown).", ConstLabels: cl},
			[]string{"status"}),
		WebhooksReceived: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_provider_webhooks_total", Help: "Webhooks de provedores por resultado.", ConstLabels: cl},
			[]string{"provider", "outcome"}),
		InvoicesIssued: prometheus.NewCounter(prometheus.CounterOpts{Name: "recorvia_invoices_issued_total", Help: "Faturas emitidas pelo motor.", ConstLabels: cl}),
		DependencyUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_dependency_up", Help: "Disponibilidade de dependências (1 = disponível).", ConstLabels: cl},
			[]string{"dependency"}),
	}
	reg.MustRegister(m.HTTPRequests, m.HTTPDuration, m.ConsumerProcessed, m.ConsumerDuration, m.ConsumerInFlight, m.ConsumerConcurrency,
		m.OutboxPublished, m.OutboxPublishFailed, m.OutboxPending, m.OutboxOldestSeconds, m.JobRuns, m.JobDuration, m.JobLastSuccess,
		m.ChargeFailures, m.PaymentOperations, m.Discrepancies, m.Refunds, m.RefundsInProgress, m.WebhooksReceived, m.InvoicesIssued, m.DependencyUp)
	return m
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
	res, err := sdkresource.Merge(sdkresource.Default(), sdkresource.NewWithAttributes(semconv.SchemaURL,
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
