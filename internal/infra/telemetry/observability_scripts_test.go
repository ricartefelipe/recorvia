package telemetry

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeValidateDocker imita, no PATH de deploy/observability/validate.sh, os
// comandos docker build e docker run usados pelo script. FAKE_SCENARIO
// escolhe o defeito simulado:
//
//	ok                      imagens e configurações válidas
//	no-runbook-url          regras da imagem do Prometheus sem runbook_url
//	no-rules                imagem do Prometheus sem regras de alerta
//	rules-unreadable        o docker run que lê as regras falha
//	route-test-fails        o route-test do Alertmanager falha
//	user-without-password   a imagem do Alertmanager aceita usuário do SMTP sem senha
//	password-without-tls    a imagem do Alertmanager aceita senha do SMTP com TLS desligado
//
// O check-config segue um modelo mínimo das recusas do entrypoint.sh; os dois
// últimos cenários desligam uma delas, e o script precisa acusar.
const fakeValidateDocker = `#!/usr/bin/env bash
set -u
case ${1:-} in
  build) exit 0 ;;
  run) ;;
  *) exit 99 ;;
esac
args=" $* "
has() { [[ $args == *"$1"* ]]; }
case $args in
  *" route-test "*)
    if [[ $FAKE_SCENARIO == route-test-fails ]]; then
      echo "alertmanager: configuração gerada inválida" >&2
      exit 3
    fi
    echo "alertmanager: receptores configurados: e-mail e webhook" >&2
    if has alertname=RecorviaWatchdog; then
      echo recorvia-watchdog
    elif has severity=critical; then
      echo recorvia-critical
    else
      echo recorvia-warning
    fi
    ;;
  *"cat /etc/prometheus/rules/"*)
    case $FAKE_SCENARIO in
      rules-unreadable)
        echo "docker: imagem inexistente" >&2
        exit 125
        ;;
      no-rules) echo "groups: []" ;;
      *)
        echo "groups:"
        echo "  - name: recorvia"
        echo "    rules:"
        echo "      - alert: RecorviaWatchdog"
        echo "        annotations:"
        echo "          runbook: docs/operacao/runbooks/alertas.md#verificacao-do-caminho-de-alertas"
        if [[ $FAKE_SCENARIO != no-runbook-url ]]; then
          echo "          runbook_url: https://exemplo.test/docs/operacao/runbooks/alertas.md#verificacao-do-caminho-de-alertas"
        fi
        ;;
    esac
    ;;
  *" check-config "*)
    reject=0
    has ALERTMANAGER_EMAIL_TO= && ! has ALERTMANAGER_SMTP_SMARTHOST= && reject=1
    has ALERTMANAGER_SMTP_PASSWORD= && ! has ALERTMANAGER_SMTP_USERNAME= && reject=1
    has ALERTMANAGER_WEBHOOK_BEARER_TOKEN= && ! has ALERTMANAGER_WEBHOOK_URL= && reject=1
    has ALERTMANAGER_WEBHOOK_URL= && ! has ALERTMANAGER_WEBHOOK_URL=http && reject=1
    has ALERTMANAGER_SMTP_REQUIRE_TLS= && ! has ALERTMANAGER_SMTP_REQUIRE_TLS=true && ! has ALERTMANAGER_SMTP_REQUIRE_TLS=false && reject=1
    [[ $args == *$'\n'* ]] && reject=1
    if [[ $FAKE_SCENARIO != user-without-password ]]; then
      has ALERTMANAGER_SMTP_USERNAME= && ! has ALERTMANAGER_SMTP_PASSWORD= && reject=1
    fi
    if [[ $FAKE_SCENARIO != password-without-tls ]]; then
      has ALERTMANAGER_SMTP_PASSWORD= && has ALERTMANAGER_SMTP_REQUIRE_TLS=false && reject=1
    fi
    if ((reject)); then
      echo "alertmanager: variáveis incoerentes" >&2
      exit 1
    fi
    ;;
esac
exit 0
`

// fakeValidateGo substitui o go test do fim do script: os testes que ele
// chama já rodam neste pacote.
const fakeValidateGo = "#!/usr/bin/env bash\nexit 0\n"

// Uma verificação que falha em deploy/observability/validate.sh precisa
// dizer o que falhou: sob set -e, o status do grep sem ocorrências ou do
// docker run numa substituição de comando encerraria o script antes da
// mensagem. O script também precisa conferir que a imagem do Alertmanager
// recusa usuário do SMTP sem senha e senha com TLS desligado.
func TestValidateScriptExplainsEveryFailure(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash indisponível")
	}
	bin := t.TempDir()
	for name, content := range map[string]string{"docker": fakeValidateDocker, "go": fakeValidateGo} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := exec.Command(filepath.Join(bin, "docker"), "build").Run(); err != nil {
		t.Skipf("diretório temporário sem permissão de execução: %v", err)
	}
	cases := []struct {
		scenario string
		wantErr  []string
	}{
		{"ok", nil},
		{"no-runbook-url", []string{"erro: 1 regras e 0 anotações runbook_url na imagem do Prometheus"}},
		{"no-rules", []string{"erro: 0 regras e 0 anotações runbook_url na imagem do Prometheus"}},
		{"rules-unreadable", []string{"erro: não foi possível ler as regras da imagem do Prometheus", "docker: imagem inexistente"}},
		{"route-test-fails", []string{"erro: route-test falhou para {alertname=RecorviaOutboxStalled severity=critical}",
			"alertmanager: configuração gerada inválida"}},
		{"user-without-password", []string{"erro: configuração aceita com usuário do SMTP sem senha"}},
		{"password-without-tls", []string{"erro: configuração aceita com senha do SMTP com TLS desligado"}},
	}
	for _, c := range cases {
		t.Run(c.scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", filepath.Join(observabilityDir, "validate.sh"))
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_SCENARIO="+c.scenario)
			cmd.WaitDelay = 10 * time.Second
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if c.wantErr == nil {
				if err != nil || !strings.Contains(stdout.String(), "configurações de observabilidade válidas") {
					t.Fatalf("a validação deveria passar: %v\n%s\n%s", err, stdout.String(), stderr.String())
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("a validação deveria falhar: %v\n%s\n%s", err, stdout.String(), stderr.String())
			}
			for _, want := range c.wantErr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("saída de erro sem %q:\n%s", want, stderr.String())
				}
			}
			if strings.Contains(stdout.String(), "configurações de observabilidade válidas") {
				t.Errorf("a validação não pode ser dada como concluída:\n%s", stdout.String())
			}
		})
	}
}

// composeStorage reúne o armazenamento declarado para um serviço do
// compose.yaml: destinos em tmpfs (com as opções) e volumes (destino ->
// origem).
type composeStorage struct {
	Tmpfs   map[string]string
	Volumes map[string]string
}

var (
	composeServiceKey = regexp.MustCompile(`^    ([a-z_]+):`)
	composeListItem   = regexp.MustCompile(`^      - "?([^"]+)"?\s*$`)
)

// composeServiceStorage lê as chaves tmpfs e volumes (sintaxe curta) de um
// serviço do compose.yaml da raiz do repositório.
func composeServiceStorage(t *testing.T, service string) composeStorage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(observabilityDir, "..", "..", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := composeStorage{Tmpfs: map[string]string{}, Volumes: map[string]string{}}
	found, key := false, ""
	for _, line := range strings.Split(string(b), "\n") {
		if !found {
			found = line == "  "+service+":"
			continue
		}
		if line != "" && !strings.HasPrefix(line, "   ") {
			break
		}
		if m := composeServiceKey.FindStringSubmatch(line); m != nil {
			key = m[1]
			if (key == "tmpfs" || key == "volumes") && !strings.HasSuffix(strings.TrimSpace(line), ":") {
				t.Fatalf("serviço %s: %s fora da sintaxe de lista: %q", service, key, line)
			}
			continue
		}
		if key != "tmpfs" && key != "volumes" || strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		m := composeListItem.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("serviço %s: item de %s fora da sintaxe curta: %q", service, key, line)
		}
		parts := strings.SplitN(m[1], ":", 3)
		switch {
		case key == "tmpfs":
			out.Tmpfs[parts[0]] = strings.Join(parts[1:], ":")
		case len(parts) >= 2:
			out.Volumes[parts[1]] = parts[0]
		default:
			out.Volumes[parts[0]] = ""
		}
	}
	if !found {
		t.Fatalf("serviço %s ausente do compose.yaml", service)
	}
	return out
}

var alertmanagerStoragePath = regexp.MustCompile(`--storage\.path=(\S+)`)

// No compose, o registro de notificações do Alertmanager (nflog, no
// diretório de --storage.path) dura o mesmo que a caixa do Mailpit: nenhum
// dos dois sobrevive ao container. Com o registro num volume e a caixa vazia,
// make down seguido de make up-obs deixaria o RecorviaWatchdog sem e-mail até
// o intervalo de repetição (24 h) e o make obs-stack-check falharia. A imagem
// declara VOLUME para esse diretório: sem tmpfs, o volume anônimo seria
// reaproveitado quando o compose recria o container.
func TestComposeAlertmanagerStateDoesNotOutliveContainer(t *testing.T) {
	entrypoint, err := os.ReadFile(filepath.Join(observabilityDir, "alertmanager", "entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := alertmanagerStoragePath.FindSubmatch(entrypoint)
	if m == nil {
		t.Fatal("entrypoint.sh sem --storage.path")
	}
	path := string(m[1])
	am := composeServiceStorage(t, "alertmanager")
	if _, ok := am.Tmpfs[path]; !ok {
		t.Errorf("alertmanager: %s fora de tmpfs (tmpfs: %v)", path, am.Tmpfs)
	}
	if len(am.Volumes) != 0 {
		t.Errorf("alertmanager: estado em volume, que sobrevive ao container: %v", am.Volumes)
	}
	if mail := composeServiceStorage(t, "mailpit"); len(mail.Volumes) != 0 {
		t.Errorf("mailpit: caixa de mensagens em volume (%v): o registro de notificações do Alertmanager precisa durar o mesmo", mail.Volumes)
	}
}

// O leitor do compose reconhece volumes nomeados: sem isso, a conferência
// acima passaria com um volume que não foi lido.
func TestComposeServiceStorageReadsNamedVolumes(t *testing.T) {
	prom := composeServiceStorage(t, "prometheus")
	if !maps.Equal(prom.Volumes, map[string]string{"/prometheus": "prometheus-data"}) || len(prom.Tmpfs) != 0 {
		t.Errorf("prometheus: volumes %v e tmpfs %v", prom.Volumes, prom.Tmpfs)
	}
}
