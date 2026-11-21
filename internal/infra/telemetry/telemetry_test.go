package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// Rótulos proibidos: identificadores de alta cardinalidade ou dados de
// negócio (RNF-08).
var forbiddenLabels = []string{"tenant", "tenant_id", "customer", "customer_id", "invoice", "invoice_id", "user", "user_id",
	"charge_id", "subscription_id", "message_id", "request_id", "email", "document"}

var descName = regexp.MustCompile(`fqName: "([^"]+)"`)
var descLabels = regexp.MustCompile(`variableLabels: \{([^}]*)\}`)

// declared reúne nomes e rótulos variáveis de todos os coletores da
// aplicação, inclusive os que ainda não têm séries.
func declared(t *testing.T) map[string][]string {
	t.Helper()
	m := NewMetrics("test")
	out := map[string][]string{}
	add := func(c prometheus.Collector) {
		ch := make(chan *prometheus.Desc, 16)
		go func() { c.Describe(ch); close(ch) }()
		for d := range ch {
			s := d.String()
			name := descName.FindStringSubmatch(s)
			if name == nil || !strings.HasPrefix(name[1], "recorvia_") {
				continue
			}
			var labels []string
			if l := descLabels.FindStringSubmatch(s); l != nil && l[1] != "" {
				for _, x := range strings.Split(l[1], ",") {
					labels = append(labels, strings.TrimSpace(x))
				}
			}
			out[name[1]] = labels
		}
	}
	v := reflect.ValueOf(m).Elem()
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !f.IsExported() || f.Name == "Registry" {
			continue
		}
		if c, ok := v.Field(i).Interface().(prometheus.Collector); ok {
			add(c)
		}
	}
	// Coletor registrado à parte (pool de conexões).
	add(newDBPoolCollector("test", func() DBPoolStats { return DBPoolStats{} }))
	return out
}

func TestMetricsHaveNoHighCardinalityLabels(t *testing.T) {
	for name, labels := range declared(t) {
		for _, l := range labels {
			if slices.Contains(forbiddenLabels, strings.Trim(l, `"`)) {
				t.Errorf("métrica %s com rótulo proibido %s", name, l)
			}
			if strings.Trim(l, `"`) == "job" || strings.Trim(l, `"`) == "instance" {
				t.Errorf("métrica %s usa o rótulo reservado %s do Prometheus", name, l)
			}
		}
	}
}

func gather(t *testing.T, m *Metrics) map[string]*dto.MetricFamily {
	t.Helper()
	mf, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, f := range mf {
		out[f.GetName()] = f
	}
	return out
}

func value(t *testing.T, f *dto.MetricFamily, labels map[string]string) float64 {
	t.Helper()
	if f == nil {
		t.Fatal("família de métricas ausente")
	}
	for _, m := range f.GetMetric() {
		got := map[string]string{}
		for _, lp := range m.GetLabel() {
			got[lp.GetName()] = lp.GetValue()
		}
		match := true
		for k, v := range labels {
			if got[k] != v {
				match = false
			}
		}
		if !match {
			continue
		}
		switch {
		case m.GetCounter() != nil:
			return m.GetCounter().GetValue()
		case m.GetGauge() != nil:
			return m.GetGauge().GetValue()
		case m.GetHistogram() != nil:
			return float64(m.GetHistogram().GetSampleCount())
		}
	}
	t.Fatalf("série %v ausente em %s", labels, f.GetName())
	return 0
}

func TestDBPoolCollectorReadsStatsOnEachGather(t *testing.T) {
	m := NewMetrics("api")
	stats := DBPoolStats{Acquired: 3, Idle: 2, Constructing: 1, Max: 20, Acquires: 40, EmptyAcquires: 5, CanceledAcquires: 1,
		EmptyAcquireWait: 1500 * time.Millisecond}
	m.RegisterDBPool(func() DBPoolStats { return stats })
	cases := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"recorvia_db_pool_connections", map[string]string{"state": "acquired", "service": "api"}, 3},
		{"recorvia_db_pool_connections", map[string]string{"state": "idle"}, 2},
		{"recorvia_db_pool_connections", map[string]string{"state": "constructing"}, 1},
		{"recorvia_db_pool_max_connections", nil, 20},
		{"recorvia_db_pool_acquires_total", nil, 40},
		{"recorvia_db_pool_empty_acquires_total", nil, 5},
		{"recorvia_db_pool_canceled_acquires_total", nil, 1},
		{"recorvia_db_pool_empty_acquire_wait_seconds_total", nil, 1.5},
	}
	got := gather(t, m)
	for _, tc := range cases {
		if v := value(t, got[tc.name], tc.labels); v != tc.want {
			t.Errorf("%s%v = %v, esperado %v", tc.name, tc.labels, v, tc.want)
		}
	}
	stats.Acquired = 7
	if v := value(t, gather(t, m)["recorvia_db_pool_connections"], map[string]string{"state": "acquired"}); v != 7 {
		t.Fatalf("a coleta deve ler o valor atual do pool: %v", v)
	}
}

func TestObserversRecordWithoutIdentifiers(t *testing.T) {
	m := NewMetrics("worker")
	m.ChargeFailed("simulator", "permanent")
	m.ChargeFailed("simulator", "permanent")
	m.JobFinished("billing.run_cycle", time.Second, nil)
	m.JobFinished("billing.run_cycle", time.Second, errors.New("falha"))
	m.Processed("payments.webhooks", "ack", 10*time.Millisecond)
	m.InFlight("payments.webhooks", 2)
	m.InFlight("payments.webhooks", -1)
	m.Published(3)
	m.PublishFailed()
	got := gather(t, m)
	checks := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"recorvia_charge_failures_total", map[string]string{"provider": "simulator", "kind": "permanent"}, 2},
		{"recorvia_job_runs_total", map[string]string{"task": "billing.run_cycle", "outcome": "success"}, 1},
		{"recorvia_job_runs_total", map[string]string{"task": "billing.run_cycle", "outcome": "failure"}, 1},
		{"recorvia_job_duration_seconds", map[string]string{"task": "billing.run_cycle"}, 2},
		{"recorvia_consumer_messages_total", map[string]string{"consumer": "payments.webhooks", "outcome": "ack"}, 1},
		{"recorvia_consumer_in_flight", map[string]string{"consumer": "payments.webhooks"}, 1},
		{"recorvia_outbox_published_total", nil, 3},
		{"recorvia_outbox_publish_failures_total", nil, 1},
	}
	for _, tc := range checks {
		if v := value(t, got[tc.name], tc.labels); v != tc.want {
			t.Errorf("%s%v = %v, esperado %v", tc.name, tc.labels, v, tc.want)
		}
	}
	if v := value(t, got["recorvia_job_last_success_timestamp_seconds"], map[string]string{"task": "billing.run_cycle"}); v <= 0 {
		t.Fatalf("último sucesso não registrado: %v", v)
	}
}

func TestInitSeriesStartAtZero(t *testing.T) {
	m := NewMetrics("worker")
	m.InitConsumer("payments.webhooks")
	m.InitJob("billing.run_cycle", time.Minute)
	m.InitJob("platform.close_statements", time.Hour)
	m.InitChargeFailures([]string{"simulator"})
	m.InitProviderWebhooks([]string{"simulator", "asaas"})
	m.InitDispatchResults()
	got := gather(t, m)
	for _, outcome := range ConsumerOutcomes {
		if v := value(t, got["recorvia_consumer_messages_total"], map[string]string{"consumer": "payments.webhooks", "outcome": outcome}); v != 0 {
			t.Errorf("consumidor %s: %v", outcome, v)
		}
	}
	for _, outcome := range []string{"success", "failure"} {
		if v := value(t, got["recorvia_job_runs_total"], map[string]string{"task": "billing.run_cycle", "outcome": outcome}); v != 0 {
			t.Errorf("tarefa %s: %v", outcome, v)
		}
	}
	for _, kind := range ChargeFailureKinds {
		if v := value(t, got["recorvia_charge_failures_total"], map[string]string{"provider": "simulator", "kind": kind}); v != 0 {
			t.Errorf("falha %s: %v", kind, v)
		}
	}
	if _, ok := got["recorvia_job_last_success_timestamp_seconds"]; ok {
		t.Fatal("o último sucesso não pode ser inicializado: o alerta de faturamento parado dependeria de um valor falso")
	}
	// O intervalo de cada tarefa permite alertas proporcionais (tarefas
	// horárias e diárias não alcançam três falhas em 30 minutos).
	for task, want := range map[string]float64{"billing.run_cycle": 60, "platform.close_statements": 3600} {
		if v := value(t, got["recorvia_job_interval_seconds"], map[string]string{"task": task}); v != want {
			t.Errorf("intervalo de %s: %v, esperado %v", task, v, want)
		}
	}
	// Webhooks recebidos, inclusive de provedor desconhecido (other), e
	// resultados das tarefas de envio e da régua.
	for _, p := range []string{"simulator", "asaas", OtherProvider} {
		for _, o := range ProviderWebhookOutcomes {
			if v := value(t, got["recorvia_provider_webhooks_total"], map[string]string{"provider": p, "outcome": o}); v != 0 {
				t.Errorf("webhook %s/%s: %v", p, o, v)
			}
		}
	}
	for name, values := range map[string][]string{
		"recorvia_notifications_dispatched_total": NotificationOutcomes,
		"recorvia_webhook_deliveries_total":       WebhookDeliveryOutcomes,
	} {
		for _, o := range values {
			if v := value(t, got[name], map[string]string{"outcome": o}); v != 0 {
				t.Errorf("%s{outcome=%q}: %v", name, o, v)
			}
		}
	}
	for _, a := range CollectionActionKinds {
		if v := value(t, got["recorvia_collection_actions_total"], map[string]string{"action": a}); v != 0 {
			t.Errorf("régua %s: %v", a, v)
		}
	}
}

// recorvia_dependency_up acompanha o estado atual das dependências a cada
// coleta, sem depender de consultas a /readyz: a queda aparece sem nenhuma
// requisição e a recuperação zera o alerta.
func TestDependencyUpIsProbedOnEachGather(t *testing.T) {
	m := NewMetrics("api")
	down := errors.New("conexão recusada")
	var (
		mu        sync.Mutex
		state     map[string]error
		deadlines []time.Duration
	)
	check := func(dep string) DependencyCheck {
		return func(ctx context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			if dl, ok := ctx.Deadline(); ok {
				deadlines = append(deadlines, time.Until(dl))
			}
			return state[dep]
		}
	}
	m.ObserveDependencies(2*time.Second, map[string]DependencyCheck{"postgres": check("postgres"), "redis": check("redis")})
	steps := []struct {
		name  string
		state map[string]error
		want  map[string]float64
	}{
		{"redis fora", map[string]error{"postgres": nil, "redis": down}, map[string]float64{"postgres": 1, "redis": 0}},
		{"redis recuperado", map[string]error{"postgres": nil, "redis": nil}, map[string]float64{"postgres": 1, "redis": 1}},
		{"banco fora", map[string]error{"postgres": down, "redis": nil}, map[string]float64{"postgres": 0, "redis": 1}},
	}
	for _, st := range steps {
		mu.Lock()
		state = st.state
		mu.Unlock()
		got := gather(t, m)
		for dep, want := range st.want {
			if v := value(t, got["recorvia_dependency_up"], map[string]string{"dependency": dep, "service": "api"}); v != want {
				t.Errorf("%s: recorvia_dependency_up{dependency=%q} = %v, esperado %v", st.name, dep, v, want)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deadlines) != 2*len(steps) {
		t.Fatalf("uma verificação por dependência a cada coleta: %d", len(deadlines))
	}
	for _, d := range deadlines {
		if d <= 0 || d > 2*time.Second {
			t.Fatalf("as verificações têm prazo de 2 s: %v", d)
		}
	}
}

// Cada dependência tem prazo próprio: um banco lento (pool esgotado, rede
// descartando pacotes) consome só o próprio prazo, sem que o Redis saudável
// apareça como indisponível, e uma verificação que ignora o cancelamento
// não trava a coleta.
func TestDependencyChecksHaveIndependentDeadlines(t *testing.T) {
	const timeout = 500 * time.Millisecond
	cases := []struct {
		name string
		slow func(started chan<- struct{}, release <-chan struct{}) DependencyCheck
	}{
		{"banco lento até o prazo", func(started chan<- struct{}, _ <-chan struct{}) DependencyCheck {
			return func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			}
		}},
		{"banco que ignora o cancelamento", func(started chan<- struct{}, release <-chan struct{}) DependencyCheck {
			return func(context.Context) error {
				close(started)
				<-release
				return nil
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() { close(release) })
			// Como o go-redis, o Redis devolve o erro do contexto se o prazo
			// já venceu. Ele só responde depois que a verificação do banco
			// começou: verificações em sequência com prazo único falham em
			// qualquer ordem.
			redis := func(ctx context.Context) error {
				select {
				case <-started:
				case <-ctx.Done():
				}
				return ctx.Err()
			}
			m := NewMetrics("api")
			m.ObserveDependencies(timeout, map[string]DependencyCheck{"postgres": c.slow(started, release), "redis": redis})
			var families []*dto.MetricFamily
			done := make(chan error, 1)
			go func() {
				var err error
				families, err = m.Registry.Gather()
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * timeout):
				t.Fatal("a coleta não respeitou o prazo das verificações")
			}
			var up *dto.MetricFamily
			for _, f := range families {
				if f.GetName() == "recorvia_dependency_up" {
					up = f
				}
			}
			for dep, want := range map[string]float64{"postgres": 0, "redis": 1} {
				if v := value(t, up, map[string]string{"dependency": dep, "service": "api"}); v != want {
					t.Errorf("recorvia_dependency_up{dependency=%q} = %v, esperado %v", dep, v, want)
				}
			}
		})
	}
}

// A API lê as execuções de conciliação em andamento a cada coleta: a
// execução registrada e não assumida aparece nas métricas do processo que
// registra os pedidos, sem depender do worker (que as publica pela tarefa
// metrics.collect). A leitura que falha mantém o último valor lido.
func TestReconciliationRunsAreReadOnEachGather(t *testing.T) {
	m := NewMetrics("api")
	var (
		mu        sync.Mutex
		stats     ReconciliationRunStats
		failure   error
		deadlines []time.Duration
	)
	m.ObserveReconciliationRuns(2*time.Second, func(ctx context.Context) (ReconciliationRunStats, error) {
		mu.Lock()
		defer mu.Unlock()
		if dl, ok := ctx.Deadline(); ok {
			deadlines = append(deadlines, time.Until(dl))
		}
		return stats, failure
	})
	type series struct{ queued, executing, stalled, oldest float64 }
	steps := []struct {
		name    string
		stats   ReconciliationRunStats
		failure error
		want    series
	}{
		{"nenhuma execução", ReconciliationRunStats{}, nil, series{}},
		{"execução aguardando sem worker", ReconciliationRunStats{Queued: 1, OldestWaitingSeconds: 420}, nil, series{queued: 1, oldest: 420}},
		{"leitura falhou: último valor", ReconciliationRunStats{}, errors.New("conexão recusada"), series{queued: 1, oldest: 420}},
		{"execuções interrompidas e em curso", ReconciliationRunStats{Queued: 2, Executing: 1, Stalled: 3, OldestWaitingSeconds: 900.5}, nil,
			series{queued: 2, executing: 1, stalled: 3, oldest: 900.5}},
		{"todas assumidas e concluídas", ReconciliationRunStats{}, nil, series{}},
	}
	for _, st := range steps {
		mu.Lock()
		stats, failure = st.stats, st.failure
		mu.Unlock()
		got := gather(t, m)
		for state, want := range map[string]float64{"queued": st.want.queued, "executing": st.want.executing, "stalled": st.want.stalled} {
			if v := value(t, got["recorvia_reconciliation_runs_in_progress"], map[string]string{"state": state, "service": "api"}); v != want {
				t.Errorf("%s: recorvia_reconciliation_runs_in_progress{state=%q} = %v, esperado %v", st.name, state, v, want)
			}
		}
		if v := value(t, got["recorvia_reconciliation_oldest_waiting_seconds"], map[string]string{"service": "api"}); v != st.want.oldest {
			t.Errorf("%s: recorvia_reconciliation_oldest_waiting_seconds = %v, esperado %v", st.name, v, st.want.oldest)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deadlines) != len(steps) {
		t.Fatalf("uma leitura a cada coleta: %d", len(deadlines))
	}
	for _, d := range deadlines {
		if d <= 0 || d > 2*time.Second {
			t.Fatalf("a leitura tem prazo de 2 s: %v", d)
		}
	}
}

// A leitura lenta (pool do banco esgotado) ou que ignora o cancelamento não
// trava a coleta das métricas: vencido o prazo, as séries saem com o último
// valor lido.
func TestReconciliationRunsReadDoesNotBlockGather(t *testing.T) {
	const timeout = 500 * time.Millisecond
	cases := []struct {
		name string
		slow func(ctx context.Context, release <-chan struct{}) (ReconciliationRunStats, error)
	}{
		{"leitura lenta até o prazo", func(ctx context.Context, _ <-chan struct{}) (ReconciliationRunStats, error) {
			<-ctx.Done()
			return ReconciliationRunStats{}, ctx.Err()
		}},
		{"leitura que ignora o cancelamento", func(_ context.Context, release <-chan struct{}) (ReconciliationRunStats, error) {
			<-release
			return ReconciliationRunStats{Queued: 9, OldestWaitingSeconds: 9}, nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			var calls atomic.Int32
			m := NewMetrics("api")
			m.ObserveReconciliationRuns(timeout, func(ctx context.Context) (ReconciliationRunStats, error) {
				if calls.Add(1) == 1 {
					return ReconciliationRunStats{Queued: 1, OldestWaitingSeconds: 600}, nil
				}
				return c.slow(ctx, release)
			})
			gather(t, m) // primeira leitura, concluída
			var families []*dto.MetricFamily
			done := make(chan error, 1)
			go func() {
				var err error
				families, err = m.Registry.Gather()
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * timeout):
				t.Fatal("a coleta não respeitou o prazo da leitura")
			}
			got := map[string]*dto.MetricFamily{}
			for _, f := range families {
				got[f.GetName()] = f
			}
			if v := value(t, got["recorvia_reconciliation_oldest_waiting_seconds"], map[string]string{"service": "api"}); v != 600 {
				t.Errorf("espera mais longa = %v, esperado o último valor lido (600)", v)
			}
			if v := value(t, got["recorvia_reconciliation_runs_in_progress"], map[string]string{"state": "queued", "service": "api"}); v != 1 {
				t.Errorf("execuções aguardando = %v, esperado o último valor lido (1)", v)
			}
		})
	}
}

// SetReconciliationRuns publica os três estados e a espera mais longa (modo
// do worker, pela tarefa metrics.collect).
func TestSetReconciliationRunsPublishesEveryState(t *testing.T) {
	m := NewMetrics("worker")
	m.SetReconciliationRuns(ReconciliationRunStats{Queued: 4, Executing: 2, Stalled: 1, OldestWaitingSeconds: 37.5})
	got := gather(t, m)
	for state, want := range map[string]float64{"queued": 4, "executing": 2, "stalled": 1} {
		if v := value(t, got["recorvia_reconciliation_runs_in_progress"], map[string]string{"state": state, "service": "worker"}); v != want {
			t.Errorf("recorvia_reconciliation_runs_in_progress{state=%q} = %v, esperado %v", state, v, want)
		}
	}
	if v := value(t, got["recorvia_reconciliation_oldest_waiting_seconds"], map[string]string{"service": "worker"}); v != 37.5 {
		t.Errorf("recorvia_reconciliation_oldest_waiting_seconds = %v", v)
	}
}

// Arquivos de observabilidade versionados no repositório.
const observabilityDir = "../../../deploy/observability"

var metricRef = regexp.MustCompile(`\brecorvia_[a-z0-9_]+`)

// referencedMetrics extrai os nomes recorvia_* usados num arquivo,
// removendo os sufixos de séries de histograma.
func referencedMetrics(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ref := range metricRef.FindAllString(string(b), -1) {
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if base, ok := strings.CutSuffix(ref, suffix); ok && strings.HasSuffix(base, "_seconds") {
				ref = base
			}
		}
		out = append(out, ref)
	}
	return out
}

// pendingStageMetrics são métricas referenciadas por regras e pelo catálogo
// que chegam com a etapa 5 (recebíveis e conciliação), ainda não integrada.
// Na integração, a declaração em NewMetrics torna a entrada desnecessária.
var pendingStageMetrics = map[string]string{
	"recorvia_refunds_total":       "etapa 5",
	"recorvia_refunds_in_progress": "etapa 5",
}

func TestAlertRulesAndDashboardsUseDeclaredMetrics(t *testing.T) {
	known := declared(t)
	files, err := filepath.Glob(filepath.Join(observabilityDir, "prometheus", "rules", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	dashboards, err := filepath.Glob(filepath.Join(observabilityDir, "grafana", "dashboards", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 || len(dashboards) == 0 {
		t.Fatalf("regras (%d) ou dashboards (%d) não encontrados", len(files), len(dashboards))
	}
	used := map[string]bool{}
	for _, f := range append(files, dashboards...) {
		for _, ref := range referencedMetrics(t, f) {
			used[ref] = true
			_, ok := known[ref]
			_, pending := pendingStageMetrics[ref]
			if !ok && !pending {
				t.Errorf("%s referencia a métrica inexistente %s", filepath.Base(f), ref)
			}
			if pending && strings.HasSuffix(f, ".json") {
				t.Errorf("%s: painel com a métrica %s da %s, ainda não integrada", filepath.Base(f), ref, pendingStageMetrics[ref])
			}
		}
	}
	// Métricas de alerta exigidas pela operação precisam estar cobertas.
	for _, required := range []string{"recorvia_outbox_pending", "recorvia_outbox_oldest_pending_seconds", "recorvia_consumer_messages_total",
		"recorvia_charge_failures_total", "recorvia_payment_operations", "recorvia_payment_operations_exhausted",
		"recorvia_reconciliation_open_discrepancies", "recorvia_reconciliation_oldest_waiting_seconds", "recorvia_db_pool_connections",
		"recorvia_db_server_connections",
		"recorvia_consumer_in_flight", "recorvia_job_interval_seconds", "recorvia_job_last_success_timestamp_seconds",
		"recorvia_notifications_dispatched_total", "recorvia_webhook_deliveries_total", "recorvia_collection_actions_total",
		"recorvia_provider_webhooks_total", "recorvia_dependency_up"} {
		if !used[required] {
			t.Errorf("métrica %s sem alerta ou painel", required)
		}
	}
}

// O catálogo de docs/operacao/observabilidade.md lista toda métrica
// declarada e, na linha da métrica, cada valor dos rótulos de conjunto
// fechado: quem monta consultas e alertas pelo catálogo não deixa de ver
// resultados como too_large ou o provedor other.
func TestMetricsCatalogDocumentsEveryMetricAndLabelValue(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(observabilityDir, "..", "..", "docs", "operacao", "observabilidade.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	rows := map[string]string{}
	for line := range strings.SplitSeq(doc, "\n") {
		if !strings.HasPrefix(line, "| `recorvia_") {
			continue
		}
		cells := strings.Split(line, "|")
		for _, name := range metricRef.FindAllString(cells[1], -1) {
			rows[name] += line
		}
	}
	for name := range declared(t) {
		if _, ok := rows[name]; !ok {
			t.Errorf("métrica %s ausente do catálogo (tabela de métricas)", name)
		}
	}
	for name := range pendingStageMetrics {
		if _, ok := rows[name]; !ok {
			t.Errorf("métrica %s (%s) ausente do catálogo", name, pendingStageMetrics[name])
		}
	}
	values := map[string][]string{
		"recorvia_consumer_messages_total":        ConsumerOutcomes,
		"recorvia_charge_failures_total":          ChargeFailureKinds,
		"recorvia_provider_webhooks_total":        append(slices.Clone(ProviderWebhookOutcomes), OtherProvider),
		"recorvia_notifications_dispatched_total": NotificationOutcomes,
		"recorvia_webhook_deliveries_total":       WebhookDeliveryOutcomes,
		"recorvia_collection_actions_total":       CollectionActionKinds,
		"recorvia_payment_operations":             {"pending", "in_flight", "unknown"},
		"recorvia_payment_operations_exhausted":   {"pending", "in_flight", "unknown"},
		"recorvia_http_requests_total":            {"_OTHER"},
		"recorvia_dependency_up":                  {"postgres", "redis", "rabbitmq"},
		"recorvia_refunds_total":                  {"succeeded", "failed"},
		"recorvia_refunds_in_progress":            {"requested", "unknown"},
	}
	for name, want := range values {
		for _, v := range want {
			if !strings.Contains(rows[name], "`"+v+"`") {
				t.Errorf("catálogo: %s sem o valor `%s`", name, v)
			}
		}
	}
}

var (
	alertRule     = regexp.MustCompile(`(?m)^\s*- alert: (\S+)`)
	runbookLink   = regexp.MustCompile(`(?m)^\s*runbook: (\S+)`)
	mdHeading     = regexp.MustCompile(`(?m)^#{1,6} (.+)$`)
	explicitID    = regexp.MustCompile(`<a id="([^"]+)"></a>`)
	slugDisallows = regexp.MustCompile(`[^\p{L}\p{N} _-]`)
)

// githubSlug reproduz o identificador que o GitHub gera para um título
// Markdown (minúsculas, sem pontuação, espaços trocados por hífens; letras
// acentuadas são mantidas).
func githubSlug(heading string) string {
	s := slugDisallows.ReplaceAllString(strings.ToLower(strings.TrimSpace(heading)), "")
	return strings.ReplaceAll(s, " ", "-")
}

// Toda regra de alerta aponta para uma seção existente de um runbook.
func TestAlertRulesLinkToExistingRunbookSections(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(observabilityDir, "prometheus", "rules", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(observabilityDir, "..", "..")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		alerts, links := alertRule.FindAllSubmatch(b, -1), runbookLink.FindAllSubmatch(b, -1)
		if len(alerts) == 0 || len(alerts) != len(links) {
			t.Fatalf("%s: %d alertas e %d anotações runbook", filepath.Base(f), len(alerts), len(links))
		}
		for i, l := range links {
			doc, anchor, ok := strings.Cut(string(l[1]), "#")
			if !ok || !strings.HasPrefix(doc, "docs/operacao/runbooks/") {
				t.Errorf("%s: runbook %q sem seção", alerts[i][1], l[1])
				continue
			}
			md, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(doc)))
			if err != nil {
				t.Errorf("%s: %v", alerts[i][1], err)
				continue
			}
			anchors := map[string]bool{}
			for _, h := range mdHeading.FindAllSubmatch(md, -1) {
				anchors[githubSlug(string(h[1]))] = true
			}
			for _, id := range explicitID.FindAllSubmatch(md, -1) {
				anchors[string(id[1])] = true
			}
			if !anchors[anchor] {
				t.Errorf("%s: seção %q inexistente em %s", alerts[i][1], anchor, doc)
			}
		}
	}
}

func TestGithubSlug(t *testing.T) {
	cases := map[string]string{
		"Motor de faturamento parado": "motor-de-faturamento-parado",
		"Conexões do banco":           "conexões-do-banco",
		"1. Preservar evidências":     "1-preservar-evidências",
	}
	for in, want := range cases {
		if got := githubSlug(in); got != want {
			t.Errorf("githubSlug(%q) = %q, esperado %q", in, got, want)
		}
	}
}

type dashboard struct {
	UID    string `json:"uid"`
	Title  string `json:"title"`
	Panels []struct {
		ID         int    `json:"id"`
		Type       string `json:"type"`
		Title      string `json:"title"`
		Datasource *struct {
			UID string `json:"uid"`
		} `json:"datasource"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	} `json:"panels"`
}

func TestDashboardsAreValidAndUseProvisionedDatasource(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(observabilityDir, "grafana", "dashboards", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	uids := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var d dashboard
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatalf("%s: JSON inválido: %v", filepath.Base(f), err)
		}
		if d.UID == "" || d.Title == "" || len(d.Panels) == 0 {
			t.Fatalf("%s: uid, título e painéis são obrigatórios", filepath.Base(f))
		}
		if other, dup := uids[d.UID]; dup {
			t.Fatalf("uid %s repetido em %s e %s", d.UID, other, filepath.Base(f))
		}
		uids[d.UID] = filepath.Base(f)
		ids := map[int]bool{}
		for _, p := range d.Panels {
			if ids[p.ID] {
				t.Errorf("%s: id de painel repetido %d", d.UID, p.ID)
			}
			ids[p.ID] = true
			if p.Type == "row" || p.Type == "text" {
				continue
			}
			if p.Datasource == nil || p.Datasource.UID != "prometheus" {
				t.Errorf("%s/%s: fonte de dados diferente da provisionada (prometheus)", d.UID, p.Title)
			}
			if len(p.Targets) == 0 {
				t.Errorf("%s/%s: painel sem consulta", d.UID, p.Title)
			}
			for _, tg := range p.Targets {
				if strings.TrimSpace(tg.Expr) == "" {
					t.Errorf("%s/%s: consulta vazia", d.UID, p.Title)
				}
			}
		}
	}
	keys := make([]string, 0, len(uids))
	for k := range uids {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !slices.Contains(keys, "recorvia-visao-geral") {
		t.Fatalf("dashboard inicial ausente: %v", keys)
	}
}

func TestSetupTracing(t *testing.T) {
	t.Run("sem endpoint, apenas propagação", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
		shutdown, err := SetupTracing(context.Background(), "recorvia-test", "dev")
		if err != nil {
			t.Fatal(err)
		}
		if err := shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("com endpoint OTLP", func(t *testing.T) {
		// O exportador não conecta na criação: o teste não depende de rede.
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
		shutdown, err := SetupTracing(context.Background(), "recorvia-test", "dev")
		if err != nil {
			t.Fatalf("a partida com OTLP habilitado não pode falhar: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shutdown(ctx) // sem coletor acessível, o encerramento pode expirar
	})
	t.Run("propaga contexto de rastreamento em headers", func(t *testing.T) {
		h := map[string]string{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
		ctx := ExtractContext(context.Background(), h)
		out := map[string]string{}
		InjectHeaders(ctx, out)
		if out["traceparent"] != h["traceparent"] {
			t.Fatalf("traceparent não propagado: %v", out)
		}
	})
}

func exposition(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTotalsCounterIsMonotonic(t *testing.T) {
	m := NewMetrics("worker")
	if out := exposition(t, m); strings.Contains(out, "recorvia_refunds_total{") {
		t.Fatal("sem leitura não há séries")
	}
	m.Refunds.Set("succeeded", 5)
	m.Refunds.Set("failed", 2)
	m.Refunds.Set("succeeded", 3) // leitura menor é ignorada
	m.Discrepancies.WithLabelValues("fee_mismatch").Set(4)
	m.ReconciliationRuns.WithLabelValues("queued").Set(2)
	m.ReconciliationOldestWaiting.Set(90)
	out := exposition(t, m)
	for _, want := range []string{
		"# TYPE recorvia_refunds_total counter",
		`recorvia_refunds_total{outcome="failed",service="worker"} 2`,
		`recorvia_refunds_total{outcome="succeeded",service="worker"} 5`,
		`recorvia_reconciliation_open_discrepancies{kind="fee_mismatch",service="worker"} 4`,
		`recorvia_reconciliation_runs_in_progress{service="worker",state="queued"} 2`,
		`recorvia_reconciliation_oldest_waiting_seconds{service="worker"} 90`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposição sem %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tenant") {
		t.Fatal("métricas sem rótulos de tenant")
	}
}
