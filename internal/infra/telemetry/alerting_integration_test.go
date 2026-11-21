//go:build integration

package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ricartefelipe/recorvia/internal/testsupport/mailpit"
)

// Testes do caminho dos alertas (ADR-065) com as imagens do compose:
// Prometheus e Alertmanager construídos de deploy/observability e o Mailpit
// como servidor SMTP, numa rede própria com os nomes de serviço do compose.

// defaultRunbookBase é o valor padrão de RUNBOOK_BASE_URL na imagem do
// Prometheus.
const defaultRunbookBase = "https://github.com/ricartefelipe/recorvia/blob/main/"

// startMailpit inicia o Mailpit na rede com o nome de serviço do compose.
func startMailpit(t *testing.T, nw *testcontainers.DockerNetwork) mailpit.Server {
	t.Helper()
	_, srv := startMailpitContainer(t, nw)
	return srv
}

// startMailpitContainer faz o mesmo e devolve também o container, para os
// testes que o removem antes do fim.
func startMailpitContainer(t *testing.T, nw *testcontainers.DockerNetwork) (*testcontainers.DockerContainer, mailpit.Server) {
	t.Helper()
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, mailpit.Image, network.WithNetwork([]string{"mailpit"}, nw),
		testcontainers.WithExposedPorts("1025/tcp", "8025/tcp"),
		testcontainers.WithWaitStrategyAndDeadline(2*time.Minute, wait.ForListeningPort("1025/tcp"), wait.ForHTTP("/api/v1/info").WithPort("8025/tcp")))
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("mailpit: %v", err)
	}
	api, err := ctr.PortEndpoint(ctx, "8025/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	return ctr, mailpit.Server{APIURL: api}
}

// startAlertmanager inicia a imagem do Alertmanager na rede, com os
// receptores definidos pelas variáveis de ambiente.
func startAlertmanager(t *testing.T, nw *testcontainers.DockerNetwork, env map[string]string,
	opts ...testcontainers.ContainerCustomizer) (*testcontainers.DockerContainer, string) {
	t.Helper()
	opts = append([]testcontainers.ContainerCustomizer{network.WithNetwork([]string{"alertmanager"}, nw),
		testcontainers.WithExposedPorts("9093/tcp"), testcontainers.WithEnv(env),
		testcontainers.WithWaitStrategyAndDeadline(2*time.Minute, wait.ForHTTP("/-/ready").WithPort("9093/tcp").WithStartupTimeout(2*time.Minute))}, opts...)
	ctr, host := startImage(t, "alertmanager", opts...)
	return ctr, "http://" + endpoint(t, ctr, host, "9093/tcp")
}

type alertmanagerAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Receivers   []struct {
		Name string `json:"name"`
	} `json:"receivers"`
	Status struct {
		State       string   `json:"state"`
		InhibitedBy []string `json:"inhibitedBy"`
	} `json:"status"`
	Fingerprint string `json:"fingerprint"`
}

// alertmanagerAlerts lista os alertas do Alertmanager com o nome informado,
// inclusive os inibidos.
func alertmanagerAlerts(t *testing.T, base, alertname string) []alertmanagerAlert {
	t.Helper()
	var out []alertmanagerAlert
	q := url.Values{"filter": {`alertname="` + alertname + `"`}}
	if code := getJSON(t, base+"/api/v2/alerts?"+q.Encode(), "", "", &out); code != http.StatusOK {
		t.Fatalf("alertas do Alertmanager: %d", code)
	}
	return out
}

// postAlerts envia alertas pela API do Alertmanager, como o Prometheus.
func postAlerts(t *testing.T, base string, alerts ...map[string]any) {
	t.Helper()
	body, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/api/v2/alerts", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("envio dos alertas: %d", resp.StatusCode)
	}
}

func containerOutput(t *testing.T, ctr *testcontainers.DockerContainer, cmd ...string) string {
	t.Helper()
	code, out, err := ctr.Exec(context.Background(), cmd, tcexec.Multiplexed())
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("%v: código %d: %s", cmd, code, b)
	}
	return string(b)
}

// O caminho inteiro de um alerta: a regra RecorviaWatchdog dispara no
// Prometheus, é enviada ao Alertmanager descoberto pela configuração da
// imagem, segue a rota própria e chega por e-mail ao Mailpit com o link do
// runbook. As regras da imagem trazem a anotação runbook_url derivada do
// caminho do runbook.
func TestAlertPipelineFromPrometheusRuleToEmail(t *testing.T) {
	ctx := context.Background()
	nw, err := network.New(ctx)
	testcontainers.CleanupNetwork(t, nw)
	if err != nil {
		t.Fatal(err)
	}
	mail := startMailpit(t, nw)
	const recipient, sender = "plantao@recorvia.test", "alertas@recorvia.test"
	_, am := startAlertmanager(t, nw, map[string]string{
		"ALERTMANAGER_SMTP_SMARTHOST": "mailpit:1025", "ALERTMANAGER_SMTP_REQUIRE_TLS": "false",
		"ALERTMANAGER_SMTP_FROM": sender, "ALERTMANAGER_EMAIL_TO": recipient,
		"ALERTMANAGER_EXTERNAL_URL": "https://alertas.recorvia.test",
	})
	prom, host := startImage(t, "prometheus", network.WithNetwork([]string{"prometheus"}, nw), testcontainers.WithExposedPorts("9090/tcp"),
		testcontainers.WithWaitStrategyAndDeadline(2*time.Minute, wait.ForHTTP("/-/ready").WithPort("9090/tcp").WithStartupTimeout(2*time.Minute)))
	base := "http://" + endpoint(t, prom, host, "9090/tcp")

	var rules struct {
		Data struct {
			Groups []struct {
				Rules []struct {
					Name        string            `json:"name"`
					Annotations map[string]string `json:"annotations"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if code := getJSON(t, base+"/api/v1/rules?type=alert", "", "", &rules); code != http.StatusOK {
		t.Fatalf("regras: %d", code)
	}
	source, err := os.ReadFile(filepath.Join(observabilityDir, "prometheus", "rules", "recorvia.yml"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, watchdogLink := 0, ""
	for _, g := range rules.Data.Groups {
		for _, r := range g.Rules {
			loaded++
			path := r.Annotations["runbook"]
			if !strings.HasPrefix(path, "docs/operacao/runbooks/") || r.Annotations["runbook_url"] != defaultRunbookBase+path {
				t.Errorf("%s: runbook %q e runbook_url %q", r.Name, path, r.Annotations["runbook_url"])
			}
			if r.Name == "RecorviaWatchdog" {
				watchdogLink = r.Annotations["runbook_url"]
			}
		}
	}
	if want := len(alertRule.FindAll(source, -1)); loaded != want || watchdogLink == "" {
		t.Fatalf("%d regras carregadas, %d no arquivo; link do Watchdog: %q", loaded, want, watchdogLink)
	}

	var discovered struct {
		Data struct {
			Active []struct {
				URL string `json:"url"`
			} `json:"activeAlertmanagers"`
		} `json:"data"`
	}
	eventually(t, time.Minute, func() bool {
		return getJSON(t, base+"/api/v1/alertmanagers", "", "", &discovered) == http.StatusOK && len(discovered.Data.Active) == 1
	})
	if got := discovered.Data.Active[0].URL; got != "http://alertmanager:9093/api/v2/alerts" {
		t.Fatalf("Alertmanager descoberto em %q", got)
	}

	// A regra é avaliada a cada 15 s e a rota do Watchdog espera 10 s para
	// agrupar.
	var received []mailpit.Message
	eventually(t, 3*time.Minute, func() bool {
		received = nil
		for _, m := range mail.MessagesTo(t, recipient) {
			if strings.Contains(m.Subject, "RecorviaWatchdog") {
				received = append(received, m)
			}
		}
		return len(received) > 0
	})
	msg := received[0]
	if msg.From.Address != sender || msg.Subject != "[Recorvia][INFORMATIVO] RecorviaWatchdog (1 ativo)" {
		t.Errorf("remetente %q e assunto %q", msg.From.Address, msg.Subject)
	}
	for _, want := range []string{watchdogLink, "Verificação do caminho de alertas", "severity=info", "https://alertas.recorvia.test"} {
		if !strings.Contains(msg.Text, want) {
			t.Errorf("texto do e-mail sem %q:\n%s", want, msg.Text)
		}
	}
	if !strings.Contains(msg.HTML, `<a href="`+watchdogLink+`">Runbook</a>`) {
		t.Errorf("HTML do e-mail sem o link do runbook:\n%s", msg.HTML)
	}
	alerts := alertmanagerAlerts(t, am, "RecorviaWatchdog")
	if len(alerts) != 1 || alerts[0].Status.State != "active" || len(alerts[0].Receivers) != 1 || alerts[0].Receivers[0].Name != "recorvia-watchdog" {
		t.Fatalf("Watchdog no Alertmanager: %+v", alerts)
	}
}

// webhookSink registra as requisições recebidas pelo webhook genérico.
type webhookSink struct {
	mu       sync.Mutex
	requests []webhookRequest
}

type webhookRequest struct {
	Path, Query, Authorization, ContentType string
	Payload                                 struct {
		Version  string `json:"version"`
		Status   string `json:"status"`
		Receiver string `json:"receiver"`
		Alerts   []struct {
			Status      string            `json:"status"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"alerts"`
	}
}

func (s *webhookSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := webhookRequest{Path: r.URL.Path, Query: r.URL.RawQuery, Authorization: r.Header.Get("Authorization"), ContentType: r.Header.Get("Content-Type")}
	if err := json.NewDecoder(r.Body).Decode(&req.Payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *webhookSink) byReceiver(receiver string) []webhookRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []webhookRequest
	for _, r := range s.requests {
		if r.Payload.Receiver == receiver {
			out = append(out, r)
		}
	}
	return out
}

// O Alertmanager roteia por severidade (destinatários distintos para os
// críticos), inibe o aviso enquanto o crítico do mesmo alerta e dos mesmos
// rótulos está ativo, sem silenciar o aviso de outro consumidor, e notifica
// o webhook genérico com o token e o link do runbook. Os segredos do webhook
// ficam em arquivos de acesso restrito criados na partida e não aparecem na
// configuração exposta pela API.
func TestAlertmanagerRoutesBySeverityInhibitsAndNotifiesWebhook(t *testing.T) {
	ctx := context.Background()
	nw, err := network.New(ctx)
	testcontainers.CleanupNetwork(t, nw)
	if err != nil {
		t.Fatal(err)
	}
	mail := startMailpit(t, nw)
	sink := &webhookSink{}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	const (
		token  = "token-de-teste-do-webhook"
		warnTo = "avisos@recorvia.test"
		critTo = "plantao@recorvia.test"
		critCc = "gestao@recorvia.test"
	)
	hookURL := "http://" + net.JoinHostPort(testcontainers.HostInternal, strconv.Itoa(port)) + "/alertas?canal=teste"
	ctr, am := startAlertmanager(t, nw, map[string]string{
		"ALERTMANAGER_SMTP_SMARTHOST": "mailpit:1025", "ALERTMANAGER_SMTP_REQUIRE_TLS": "false",
		"ALERTMANAGER_EMAIL_TO": warnTo, "ALERTMANAGER_EMAIL_TO_CRITICAL": critTo + ", " + critCc,
		"ALERTMANAGER_WEBHOOK_URL": hookURL, "ALERTMANAGER_WEBHOOK_BEARER_TOKEN": token,
	}, testcontainers.WithHostPortAccess(port))

	// Segredos somente em arquivos do usuário do processo; a configuração
	// gerada referencia os arquivos.
	files := containerOutput(t, ctr, "sh", "-c", "stat -c '%a %U %n' /etc/alertmanager/runtime /etc/alertmanager/runtime/*")
	for _, want := range []string{"700 nobody /etc/alertmanager/runtime\n",
		"600 nobody /etc/alertmanager/runtime/webhook_url", "600 nobody /etc/alertmanager/runtime/webhook_token"} {
		if !strings.Contains(files, want) {
			t.Errorf("arquivos de execução sem %q:\n%s", want, files)
		}
	}
	// Sem autenticação no SMTP (o Mailpit não usa TLS), não há arquivo de senha.
	if strings.Contains(files, "smtp_password") {
		t.Errorf("arquivo de senha do SMTP sem senha configurada:\n%s", files)
	}
	generated := containerOutput(t, ctr, "cat", "/etc/alertmanager/runtime/alertmanager.yml")
	var status struct {
		Config struct {
			Original string `json:"original"`
		} `json:"config"`
	}
	if code := getJSON(t, am+"/api/v2/status", "", "", &status); code != http.StatusOK || status.Config.Original == "" {
		t.Fatalf("status do Alertmanager: %d", code)
	}
	for name, text := range map[string]string{"configuração gerada": generated, "configuração exposta pela API": status.Config.Original} {
		for _, secret := range []string{token, hookURL, "canal=teste"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s contém o segredo %q", name, secret)
			}
		}
	}

	link := defaultRunbookBase + "docs/operacao/runbooks/filas-e-dlq.md#dead-letters-abertas"
	alert := func(severity, consumer, summary string) map[string]any {
		return map[string]any{
			"labels": map[string]string{"alertname": "RecorviaDeadLettersOpen", "area": "mensageria", "severity": severity, "consumer": consumer},
			"annotations": map[string]string{"summary": summary, "description": "Descrição de teste.",
				"runbook": "docs/operacao/runbooks/filas-e-dlq.md#dead-letters-abertas", "runbook_url": link},
			"generatorURL": "http://localhost:9090/graph",
		}
	}
	postAlerts(t, am,
		alert("warning", "payments.provider_webhooks", "3 dead letters abertas do consumidor payments.provider_webhooks"),
		alert("critical", "payments.provider_webhooks", "Dead letter do consumidor payments.provider_webhooks aberta há 1d"),
		alert("warning", "webhooks.fanout", "1 dead letters abertas do consumidor webhooks.fanout"))

	// Inibição: somente o aviso do mesmo consumidor do crítico. O estado é
	// marcado quando o grupo dos avisos é avaliado (espera de 30 s).
	var alerts []alertmanagerAlert
	eventually(t, 3*time.Minute, func() bool {
		alerts = alertmanagerAlerts(t, am, "RecorviaDeadLettersOpen")
		for _, a := range alerts {
			if a.Status.State == "suppressed" {
				return true
			}
		}
		return false
	})
	if len(alerts) != 3 {
		t.Fatalf("alertas no Alertmanager: %+v", alerts)
	}
	critical := ""
	for _, a := range alerts {
		if a.Labels["severity"] == "critical" {
			critical = a.Fingerprint
		}
	}
	for _, a := range alerts {
		key := a.Labels["severity"] + "/" + a.Labels["consumer"]
		wantState, wantReceiver := "active", "recorvia-warning"
		switch key {
		case "critical/payments.provider_webhooks":
			wantReceiver = "recorvia-critical"
		case "warning/payments.provider_webhooks":
			wantState = "suppressed"
			if len(a.Status.InhibitedBy) != 1 || a.Status.InhibitedBy[0] != critical {
				t.Errorf("%s: inibido por %v, esperado o crítico %s", key, a.Status.InhibitedBy, critical)
			}
		}
		if a.Status.State != wantState || len(a.Receivers) != 1 || a.Receivers[0].Name != wantReceiver {
			t.Errorf("%s: estado %q e receptores %+v; esperado %q e %s", key, a.Status.State, a.Receivers, wantState, wantReceiver)
		}
	}

	// Webhook: um POST por grupo (crítico em 10 s, aviso em 30 s), com o
	// token e o link do runbook; o aviso inibido não é notificado.
	eventually(t, 3*time.Minute, func() bool {
		return len(sink.byReceiver("recorvia-critical")) > 0 && len(sink.byReceiver("recorvia-warning")) > 0
	})
	for receiver, wantConsumer := range map[string]string{"recorvia-critical": "payments.provider_webhooks", "recorvia-warning": "webhooks.fanout"} {
		got := sink.byReceiver(receiver)[0]
		if got.Path != "/alertas" || got.Query != "canal=teste" || got.Authorization != "Bearer "+token || got.ContentType != "application/json" {
			t.Errorf("%s: requisição %s?%s, Authorization %q, Content-Type %q", receiver, got.Path, got.Query, got.Authorization, got.ContentType)
		}
		p := got.Payload
		if p.Version != "4" || p.Status != "firing" || len(p.Alerts) != 1 {
			t.Fatalf("%s: carga %+v", receiver, p)
		}
		if a := p.Alerts[0]; a.Labels["consumer"] != wantConsumer || a.Annotations["runbook_url"] != link {
			t.Errorf("%s: alerta %+v", receiver, a)
		}
	}

	// E-mails: críticos para os destinatários dos críticos; avisos para os
	// demais, sem o aviso inibido.
	subjects := func(recipient string) []mailpit.Message {
		var out []mailpit.Message
		for _, m := range mail.MessagesTo(t, recipient) {
			if strings.Contains(m.Subject, "RecorviaDeadLettersOpen") {
				out = append(out, m)
			}
		}
		return out
	}
	eventually(t, 2*time.Minute, func() bool {
		return len(subjects(critTo)) > 0 && len(subjects(critCc)) > 0 && len(subjects(warnTo)) > 0
	})
	for _, rcpt := range []string{critTo, critCc} {
		got := subjects(rcpt)
		if len(got) != 1 || got[0].Subject != "[Recorvia][CRÍTICO] RecorviaDeadLettersOpen (1 ativo)" ||
			!strings.Contains(got[0].Text, "consumer=payments.provider_webhooks") || !strings.Contains(got[0].Text, "Runbook: "+link) {
			t.Errorf("e-mails críticos para %s: %+v", rcpt, got)
		}
	}
	warn := subjects(warnTo)
	if len(warn) != 1 || warn[0].Subject != "[Recorvia][AVISO] RecorviaDeadLettersOpen (1 ativo)" ||
		!strings.Contains(warn[0].Text, "consumer=webhooks.fanout") || strings.Contains(warn[0].Text, "payments.provider_webhooks") ||
		!strings.Contains(warn[0].HTML, `<a href="`+link+`">Runbook</a>`) {
		t.Errorf("e-mails de aviso: %+v", warn)
	}
}

// A senha do SMTP só é aceita com TLS exigido (ela trafegaria em claro numa
// conexão sem TLS). Ela fica num arquivo de acesso restrito, com o conteúdo
// exato, e não aparece na configuração gerada nem na exposta pela API.
func TestAlertmanagerKeepsSMTPPasswordInRestrictedFileAndRequiresTLS(t *testing.T) {
	ctx := context.Background()
	nw, err := network.New(ctx)
	testcontainers.CleanupNetwork(t, nw)
	if err != nil {
		t.Fatal(err)
	}
	const password = `senha: "de" 'teste' #1`
	ctr, am := startAlertmanager(t, nw, map[string]string{
		"ALERTMANAGER_SMTP_SMARTHOST": "smtp.exemplo.test:587", "ALERTMANAGER_SMTP_USERNAME": "alertas'recorvia",
		"ALERTMANAGER_SMTP_PASSWORD": password, "ALERTMANAGER_EMAIL_TO": "plantao@recorvia.test",
	})
	files := containerOutput(t, ctr, "sh", "-c", "stat -c '%a %U %n' /etc/alertmanager/runtime /etc/alertmanager/runtime/*")
	for _, want := range []string{"700 nobody /etc/alertmanager/runtime\n", "600 nobody /etc/alertmanager/runtime/smtp_password"} {
		if !strings.Contains(files, want) {
			t.Errorf("arquivos de execução sem %q:\n%s", want, files)
		}
	}
	if got := containerOutput(t, ctr, "cat", "/etc/alertmanager/runtime/smtp_password"); got != password {
		t.Errorf("senha gravada: %q", got)
	}
	generated := containerOutput(t, ctr, "cat", "/etc/alertmanager/runtime/alertmanager.yml")
	for _, want := range []string{"smtp_require_tls: true", "smtp_auth_username: 'alertas''recorvia'",
		"smtp_auth_password_file: /etc/alertmanager/runtime/smtp_password"} {
		if !strings.Contains(generated, want) {
			t.Errorf("configuração gerada sem %q:\n%s", want, generated)
		}
	}
	var status struct {
		Config struct {
			Original string `json:"original"`
		} `json:"config"`
	}
	if code := getJSON(t, am+"/api/v2/status", "", "", &status); code != http.StatusOK || status.Config.Original == "" {
		t.Fatalf("status do Alertmanager: %d", code)
	}
	for name, text := range map[string]string{"configuração gerada": generated, "configuração exposta pela API": status.Config.Original} {
		if strings.Contains(text, password) {
			t.Errorf("%s contém a senha do SMTP", name)
		}
	}
}

// composeAlertmanagerStorage monta, num container de teste, o armazenamento
// que o compose declara para o Alertmanager: tmpfs como tmpfs e cada volume
// nomeado como um volume do teste, que sobrevive à remoção do container (como
// no make down, que mantém os volumes) e é removido no fim do teste.
func composeAlertmanagerStorage(t *testing.T) (composeStorage, []testcontainers.ContainerCustomizer) {
	t.Helper()
	storage := composeServiceStorage(t, "alertmanager")
	var opts []testcontainers.ContainerCustomizer
	if len(storage.Tmpfs) > 0 {
		opts = append(opts, testcontainers.WithTmpfs(storage.Tmpfs))
	}
	var names []string
	for target, source := range storage.Volumes {
		if source == "" {
			continue
		}
		name := "recorvia-teste-" + source + "-" + strings.ToLower(rand.Text())
		names = append(names, name)
		opts = append(opts, testcontainers.WithMounts(testcontainers.VolumeMount(name, testcontainers.ContainerMountTarget(target))))
	}
	if len(names) > 0 {
		t.Cleanup(func() {
			if err := testcontainers.NewTerminateOptions(context.Background(), testcontainers.RemoveVolumes(names...)).Cleanup(); err != nil {
				t.Logf("remoção dos volumes do teste: %v", err)
			}
		})
	}
	return storage, opts
}

// Com o armazenamento declarado no compose, o RecorviaWatchdog é notificado
// de novo a cada subida do ambiente: depois de make down e make up-obs (o
// Mailpit volta vazio) e depois de recriar o container do Alertmanager para
// trocar o destinatário. Se o registro de notificações sobrevivesse ao
// container, o Alertmanager consideraria o grupo já notificado e só enviaria
// o e-mail depois do intervalo de repetição da rota (24 h).
func TestAlertmanagerNotifiesWatchdogAgainAfterEnvironmentRestart(t *testing.T) {
	ctx := context.Background()
	nw, err := network.New(ctx)
	testcontainers.CleanupNetwork(t, nw)
	if err != nil {
		t.Fatal(err)
	}
	storage, mounts := composeAlertmanagerStorage(t)
	const first, second = "plantao@recorvia.test", "novo-plantao@recorvia.test"
	start := func(recipient string) (*testcontainers.DockerContainer, string) {
		t.Helper()
		ctr, am := startAlertmanager(t, nw, map[string]string{
			"ALERTMANAGER_SMTP_SMARTHOST": "mailpit:1025", "ALERTMANAGER_SMTP_REQUIRE_TLS": "false", "ALERTMANAGER_EMAIL_TO": recipient,
		}, mounts...)
		// O diretório de estado precisa aceitar a escrita do usuário do processo.
		for path := range storage.Tmpfs {
			containerOutput(t, ctr, "sh", "-c", "touch "+path+"/.escrita && rm "+path+"/.escrita")
		}
		return ctr, am
	}
	stop := func(ctrs ...*testcontainers.DockerContainer) {
		t.Helper()
		for _, c := range ctrs {
			if err := testcontainers.TerminateContainer(c); err != nil {
				t.Fatal(err)
			}
		}
	}
	// O Prometheus reenvia o alerta ativo a cada avaliação; aqui, um envio
	// pela API por subida. A rota do Watchdog espera 10 s para agrupar.
	watchdog := map[string]any{
		"labels": map[string]string{"alertname": "RecorviaWatchdog", "severity": "info", "area": "alertas"},
		"annotations": map[string]string{"summary": "Verificação do caminho de alertas",
			"runbook": "docs/operacao/runbooks/alertas.md#verificacao-do-caminho-de-alertas"},
	}
	notified := func(phase, am string, mail mailpit.Server, recipient string) {
		t.Helper()
		postAlerts(t, am, watchdog)
		t.Logf("%s: aguardando o e-mail do RecorviaWatchdog para %s", phase, recipient)
		eventually(t, 2*time.Minute, func() bool {
			for _, m := range mail.MessagesTo(t, recipient) {
				if strings.Contains(m.Subject, "RecorviaWatchdog") {
					return true
				}
			}
			return false
		})
	}

	mailCtr, mail := startMailpitContainer(t, nw)
	amCtr, am := start(first)
	notified("primeira subida", am, mail, first)

	stop(amCtr, mailCtr)
	_, mail = startMailpitContainer(t, nw)
	amCtr, am = start(first)
	notified("ambiente derrubado e iniciado de novo", am, mail, first)

	stop(amCtr)
	_, am = start(second)
	notified("Alertmanager recriado com outro destinatário", am, mail, second)
}

// A partida gera a configuração a partir das variáveis de ambiente e a
// valida: variáveis incoerentes ou que alterariam a estrutura do YAML
// impedem a partida com uma mensagem clara, e os segredos não são escritos
// na saída.
func TestAlertmanagerEntrypointValidatesEnvironment(t *testing.T) {
	const secret = "segredo-que-nao-pode-aparecer"
	smtp := map[string]string{"ALERTMANAGER_SMTP_SMARTHOST": "smtp.exemplo.test:587", "ALERTMANAGER_EMAIL_TO": "plantao@exemplo.test"}
	with := func(base map[string]string, extra ...string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for i := 0; i+1 < len(extra); i += 2 {
			out[extra[i]] = extra[i+1]
		}
		return out
	}
	cases := []struct {
		name string
		env  map[string]string
		code int
		want string
	}{
		{"sem receptores", nil, 0, "nenhum receptor configurado"},
		{"somente e-mail", smtp, 0, "receptores configurados: e-mail"},
		{"e-mail com autenticação e webhook com token", with(smtp, "ALERTMANAGER_SMTP_USERNAME", "alertas", "ALERTMANAGER_SMTP_PASSWORD", secret,
			"ALERTMANAGER_WEBHOOK_URL", "https://alertas.exemplo.test/"+secret, "ALERTMANAGER_WEBHOOK_BEARER_TOKEN", secret), 0,
			"receptores configurados: e-mail e webhook"},
		{"somente webhook", map[string]string{"ALERTMANAGER_WEBHOOK_URL": "https://alertas.exemplo.test/hook"}, 0, "receptores configurados: webhook"},
		{"destinatário sem servidor SMTP", map[string]string{"ALERTMANAGER_EMAIL_TO": "plantao@exemplo.test"}, 1,
			"ALERTMANAGER_EMAIL_TO exige ALERTMANAGER_SMTP_SMARTHOST"},
		{"destinatário dos críticos sem o geral", map[string]string{"ALERTMANAGER_SMTP_SMARTHOST": "smtp.exemplo.test:587",
			"ALERTMANAGER_EMAIL_TO_CRITICAL": "plantao@exemplo.test"}, 1, "ALERTMANAGER_EMAIL_TO_CRITICAL exige ALERTMANAGER_EMAIL_TO"},
		{"e-mail sem autenticação e sem TLS", with(smtp, "ALERTMANAGER_SMTP_REQUIRE_TLS", "false"), 0, "receptores configurados: e-mail"},
		{"senha sem usuário", with(smtp, "ALERTMANAGER_SMTP_PASSWORD", secret), 1, "ALERTMANAGER_SMTP_PASSWORD exige ALERTMANAGER_SMTP_USERNAME"},
		{"usuário sem senha", with(smtp, "ALERTMANAGER_SMTP_USERNAME", "alertas"), 1, "ALERTMANAGER_SMTP_USERNAME exige ALERTMANAGER_SMTP_PASSWORD"},
		{"senha com TLS desligado", with(smtp, "ALERTMANAGER_SMTP_USERNAME", "alertas", "ALERTMANAGER_SMTP_PASSWORD", secret,
			"ALERTMANAGER_SMTP_REQUIRE_TLS", "false"), 1, "ALERTMANAGER_SMTP_PASSWORD exige ALERTMANAGER_SMTP_REQUIRE_TLS=true"},
		{"senha com TLS desligado e sem destinatário", map[string]string{"ALERTMANAGER_SMTP_USERNAME": "alertas", "ALERTMANAGER_SMTP_PASSWORD": secret,
			"ALERTMANAGER_SMTP_REQUIRE_TLS": "false"}, 1, "ALERTMANAGER_SMTP_PASSWORD exige ALERTMANAGER_SMTP_REQUIRE_TLS=true"},
		{"token sem webhook", map[string]string{"ALERTMANAGER_WEBHOOK_BEARER_TOKEN": secret}, 1,
			"ALERTMANAGER_WEBHOOK_BEARER_TOKEN exige ALERTMANAGER_WEBHOOK_URL"},
		{"webhook sem esquema", map[string]string{"ALERTMANAGER_WEBHOOK_URL": "alertas.exemplo.test/hook"}, 1, "deve começar com http:// ou https://"},
		{"quebra de linha no destinatário", with(smtp, "ALERTMANAGER_EMAIL_TO", "plantao@exemplo.test\nreceivers: []"), 1,
			"ALERTMANAGER_EMAIL_TO não pode conter quebras de linha"},
		{"modelo no destinatário", with(smtp, "ALERTMANAGER_EMAIL_TO", `{{ .CommonLabels.dono }}@exemplo.test`), 1, "ALERTMANAGER_EMAIL_TO não pode conter {{"},
		{"TLS inválido", with(smtp, "ALERTMANAGER_SMTP_REQUIRE_TLS", "talvez"), 1, "ALERTMANAGER_SMTP_REQUIRE_TLS deve ser true ou false"},
		{"servidor SMTP inválido", with(smtp, "ALERTMANAGER_SMTP_SMARTHOST", "smtp sem porta"), 1, "configuração gerada inválida"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			ctr, _ := startImage(t, "alertmanager", testcontainers.WithCmd("check-config"), testcontainers.WithEnv(c.env),
				testcontainers.WithWaitStrategyAndDeadline(time.Minute, wait.ForExit().WithExitTimeout(time.Minute)))
			state, err := ctr.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			logs, err := ctr.Logs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = logs.Close() }()
			b, err := io.ReadAll(logs)
			if err != nil {
				t.Fatal(err)
			}
			out := string(b)
			if state.ExitCode != c.code || !strings.Contains(out, c.want) {
				t.Errorf("código %d (esperado %d); saída sem %q:\n%s", state.ExitCode, c.code, c.want, out)
			}
			if strings.Contains(out, secret) {
				t.Errorf("segredo na saída:\n%s", out)
			}
		})
	}
}
