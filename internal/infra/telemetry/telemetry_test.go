package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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
	m.InitJob("billing.run_cycle")
	m.InitChargeFailures([]string{"simulator"})
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
			if _, ok := known[ref]; !ok {
				t.Errorf("%s referencia a métrica inexistente %s", filepath.Base(f), ref)
			}
		}
	}
	// Métricas de alerta exigidas pela operação precisam estar cobertas.
	for _, required := range []string{"recorvia_outbox_pending", "recorvia_outbox_oldest_pending_seconds", "recorvia_consumer_messages_total",
		"recorvia_charge_failures_total", "recorvia_payment_operations", "recorvia_reconciliation_open_discrepancies",
		"recorvia_db_pool_connections", "recorvia_db_server_connections", "recorvia_consumer_in_flight"} {
		if !used[required] {
			t.Errorf("métrica %s sem alerta ou painel", required)
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
