//go:build integration

package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Testes do perfil observability com as imagens construídas a partir dos
// Dockerfiles de deploy/observability (as mesmas do compose), sem portas fixas
// no host: cada teste publica portas aleatórias e usa uma rede própria.

// startImage constrói a imagem do diretório de deploy/observability e inicia
// o container. A imagem mantém um nome fixo (recorvia-<dir>:integration-test)
// e é reaproveitada entre execuções, sem acumular imagens.
func startImage(t *testing.T, dir string, opts ...testcontainers.ContainerCustomizer) (*testcontainers.DockerContainer, string) {
	t.Helper()
	ctx := context.Background()
	opts = append([]testcontainers.ContainerCustomizer{testcontainers.WithDockerfile(testcontainers.FromDockerfile{
		Context: filepath.Join(observabilityDir, dir), Repo: "recorvia-" + dir, Tag: "integration-test", KeepImage: true,
	})}, opts...)
	ctr, err := testcontainers.Run(ctx, "", opts...)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return ctr, host
}

func endpoint(t *testing.T, ctr *testcontainers.DockerContainer, host, port string) string {
	t.Helper()
	p, err := ctr.MappedPort(context.Background(), port)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s:%s", host, p.Port())
}

// getJSON consulta url e decodifica o corpo em out quando o status é 200.
func getJSON(t *testing.T, url, user, password string, out any) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("GET %s: %v: %s", url, err, b)
		}
	}
	return resp.StatusCode
}

// eventually repete cond até ser verdadeira ou o prazo expirar.
func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condição não atingida no prazo")
		}
		<-tick.C
	}
}

// A aplicação exporta OTLP para o Collector, que remove atributos sensíveis
// antes de encaminhar ao Jaeger (ADR-052). O rastro é consultado na API v3
// do Jaeger e não pode conter o token do caminho do webhook, a consulta da
// URL nem o endereço do cliente.
func TestCollectorRedactsSensitiveSpanAttributes(t *testing.T) {
	ctx := context.Background()
	nw, err := network.New(ctx)
	testcontainers.CleanupNetwork(t, nw)
	if err != nil {
		t.Fatal(err)
	}
	jaeger, host := startImage(t, "jaeger", network.WithNetwork([]string{"jaeger"}, nw),
		testcontainers.WithExposedPorts("16686/tcp", "13133/tcp"),
		testcontainers.WithWaitStrategyAndDeadline(2*time.Minute, wait.ForHTTP("/status").WithPort("13133/tcp").WithStartupTimeout(2*time.Minute)))
	collector, _ := startImage(t, "otel-collector", network.WithNetwork([]string{"otel-collector"}, nw),
		testcontainers.WithExposedPorts("4317/tcp", "13133/tcp"),
		testcontainers.WithWaitStrategyAndDeadline(2*time.Minute, wait.ForHTTP("/").WithPort("13133/tcp").WithStartupTimeout(2*time.Minute)))

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+endpoint(t, collector, host, "4317/tcp"))
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	shutdown, err := SetupTracing(ctx, "recorvia-redaction-test", "test")
	if err != nil {
		t.Fatal(err)
	}
	const token = "tok-0123456789abcdef0123456789abcdef"
	_, span := otel.Tracer("redaction-test").Start(ctx, "POST /webhooks/providers/{provider}/{token}",
		trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
			attribute.String("url.path", "/webhooks/providers/simulator/"+token),
			attribute.String("url.full", "http://api:8081/webhooks/providers/simulator/"+token+"?signature=assinatura-secreta"),
			attribute.String("url.query", "signature=assinatura-secreta"),
			attribute.String("client.address", "203.0.113.7"),
			attribute.String("network.peer.address", "203.0.113.8"),
			attribute.String("http.route", "/webhooks/providers/{provider}/{token}"),
			attribute.String("recorvia.request_id", "req-redaction-test"),
		))
	traceID := span.SpanContext().TraceID().String()
	span.End()
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := shutdown(sctx); err != nil {
		t.Fatalf("exportação para o Collector: %v", err)
	}

	url := "http://" + endpoint(t, jaeger, host, "16686/tcp") + "/api/v3/traces/" + traceID
	var body json.RawMessage
	eventually(t, 90*time.Second, func() bool { return getJSON(t, url, "", "", &body) == http.StatusOK })
	got := string(body)
	for _, leaked := range []string{token, "assinatura-secreta", "203.0.113.7", "203.0.113.8", `"url.query"`, `"client.address"`, `"network.peer.address"`} {
		if strings.Contains(got, leaked) {
			t.Errorf("rastro exportado contém %q: %s", leaked, got)
		}
	}
	for _, kept := range []string{"/webhooks/providers/simulator/{token}", "http://api:8081/webhooks/providers/simulator/{token}", "req-redaction-test",
		"recorvia-redaction-test"} {
		if !strings.Contains(got, kept) {
			t.Errorf("rastro exportado sem %q: %s", kept, got)
		}
	}
}

// O Grafana carrega a fonte de dados e os dashboards provisionados na imagem
// (somente leitura) e usa a visão geral como página inicial.
func TestGrafanaProvisionsDatasourceAndDashboards(t *testing.T) {
	const password = "grafana-test-admin"
	grafana, host := startImage(t, "grafana", testcontainers.WithExposedPorts("3000/tcp"),
		testcontainers.WithEnv(map[string]string{"GF_SECURITY_ADMIN_PASSWORD": password}),
		testcontainers.WithWaitStrategyAndDeadline(5*time.Minute, wait.ForHTTP("/api/health").WithPort("3000/tcp").WithStartupTimeout(5*time.Minute)))
	base := "http://" + endpoint(t, grafana, host, "3000/tcp")

	var ds struct {
		UID, Type, URL string
		IsDefault      bool
		ReadOnly       bool
	}
	if code := getJSON(t, base+"/api/datasources/uid/prometheus", "admin", password, &ds); code != http.StatusOK {
		t.Fatalf("fonte de dados prometheus: %d", code)
	}
	if ds.Type != "prometheus" || ds.URL != "http://prometheus:9090" || !ds.IsDefault || !ds.ReadOnly {
		t.Fatalf("fonte de dados %+v", ds)
	}

	want := map[string]bool{}
	files, err := filepath.Glob(filepath.Join(observabilityDir, "grafana", "dashboards", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		want[strings.TrimSuffix(filepath.Base(f), ".json")] = true
	}
	var found []struct {
		UID, Title, FolderTitle string
	}
	// O provisionamento termina pouco depois de /api/health responder.
	eventually(t, 2*time.Minute, func() bool {
		found = nil
		return getJSON(t, base+"/api/search?type=dash-db", "admin", password, &found) == http.StatusOK && len(found) >= len(want)
	})
	var uids []string
	for _, d := range found {
		if d.FolderTitle != "Recorvia" {
			t.Errorf("dashboard %s fora da pasta Recorvia: %q", d.UID, d.FolderTitle)
		}
		uids = append(uids, d.UID)
	}
	for uid := range want {
		if !slices.Contains(uids, uid) {
			t.Errorf("dashboard %s não provisionado (encontrados %v)", uid, uids)
		}
		var d struct {
			Meta struct {
				Provisioned bool `json:"provisioned"`
				CanSave     bool `json:"canSave"`
			} `json:"meta"`
		}
		if code := getJSON(t, base+"/api/dashboards/uid/"+uid, "admin", password, &d); code != http.StatusOK || !d.Meta.Provisioned {
			t.Errorf("dashboard %s: status %d, provisionado %v", uid, code, d.Meta.Provisioned)
		}
	}
	// O Grafana responde à página inicial com um redirecionamento para o
	// dashboard lido de GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH (o endereço
	// usa o título, "Recorvia: visão geral").
	var home struct {
		RedirectURI string `json:"redirectUri"`
		Dashboard   struct {
			Title string `json:"title"`
		} `json:"dashboard"`
	}
	code := getJSON(t, base+"/api/dashboards/home", "", "", &home)
	redirected := strings.HasPrefix(home.RedirectURI, "/d/") && strings.HasSuffix(home.RedirectURI, "-visao-geral")
	if code != http.StatusOK || (!redirected && home.Dashboard.Title != "Recorvia: visão geral") {
		t.Fatalf("página inicial (acesso anônimo de leitura): %d %+v", code, home)
	}
}

// O Prometheus carrega as regras de alerta embutidas na imagem e os alvos de
// coleta do compose.
func TestPrometheusLoadsAlertRulesAndScrapeTargets(t *testing.T) {
	prom, host := startImage(t, "prometheus", testcontainers.WithExposedPorts("9090/tcp"),
		testcontainers.WithWaitStrategyAndDeadline(2*time.Minute, wait.ForHTTP("/-/ready").WithPort("9090/tcp").WithStartupTimeout(2*time.Minute)))
	base := "http://" + endpoint(t, prom, host, "9090/tcp")

	var rules struct {
		Data struct {
			Groups []struct {
				Name  string `json:"name"`
				Rules []struct {
					Name   string `json:"name"`
					Type   string `json:"type"`
					Health string `json:"health"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if code := getJSON(t, base+"/api/v1/rules", "", "", &rules); code != http.StatusOK {
		t.Fatalf("regras: %d", code)
	}
	var alerts []string
	for _, g := range rules.Data.Groups {
		for _, r := range g.Rules {
			if r.Type != "alerting" || r.Health == "err" {
				t.Errorf("regra %s/%s: tipo %s, saúde %s", g.Name, r.Name, r.Type, r.Health)
			}
			alerts = append(alerts, r.Name)
		}
	}
	for _, required := range []string{"RecorviaOutboxStalled", "RecorviaQueueLag", "RecorviaMessagesDeadLettered", "RecorviaChargesDeclined",
		"RecorviaPaymentOperationsUnknown", "RecorviaReconciliationDiscrepanciesOpen", "RecorviaDBServerConnectionsHigh", "RecorviaWorkerSaturated"} {
		if !slices.Contains(alerts, required) {
			t.Errorf("alerta %s não carregado", required)
		}
	}

	var targets struct {
		Data struct {
			ActiveTargets []struct {
				Labels map[string]string `json:"labels"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	jobs := map[string]bool{}
	eventually(t, time.Minute, func() bool {
		if getJSON(t, base+"/api/v1/targets", "", "", &targets) != http.StatusOK {
			return false
		}
		for _, tg := range targets.Data.ActiveTargets {
			jobs[tg.Labels["job"]] = true
		}
		return len(jobs) >= 6
	})
	for _, job := range []string{"recorvia-api", "recorvia-worker", "rabbitmq", "rabbitmq-queues", "otel-collector", "prometheus"} {
		if !jobs[job] {
			t.Errorf("alvo de coleta %s ausente: %v", job, jobs)
		}
	}
}
