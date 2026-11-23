package pgstore_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePITRDocker imita o docker compose para pitr-restore.sh e
// keycloak-export.sh: serviços do componente de backup, estado do container
// do PostgreSQL (FAKE_PG_STATE), banco do Keycloak (FAKE_KC_DB) e resultado da
// preparação da restauração (FAKE_RESTORE_CODE). Registra em FAKE_LOG tudo o
// que alteraria o ambiente.
const fakePITRDocker = `#!/usr/bin/env bash
set -u
[[ ${1:-} == compose ]] || { echo "$*" >>"$FAKE_LOG"; exit 0; }
shift
case "${1:-}" in
  config) printf 'postgres\npostgres-backup\nkeycloak\n'; exit 0 ;;
  ps)
    if [[ " $* " == *" -a "* ]]; then printf '%s\n' "${FAKE_PG_STATE:-}"; exit 0; fi
    case "${@: -1}" in
      postgres-backup) echo agente ;;
      keycloak) echo keycloak ;;
    esac
    exit 0 ;;
  exec)
    if [[ " $* " == *" keycloak sh -c "* ]]; then echo "${FAKE_KC_DB:-}"; exit 0; fi
    echo "compose $*" >>"$FAKE_LOG"; cat >/dev/null; exit 0 ;;
  run)
    echo "compose $*" >>"$FAKE_LOG"
    if [[ " $* " == *" restore "* ]]; then exit "${FAKE_RESTORE_CODE:-1}"; fi
    if [[ " $* " == *" keycloak "* ]]; then echo '{"realm":"recorvia","users":[{"username":"a"}]}'; fi
    exit 0 ;;
esac
echo "compose $*" >>"$FAKE_LOG"
exit 0
`

type pitrSandbox struct{ root, bin, log string }

func newPITRSandbox(t *testing.T) pitrSandbox {
	t.Helper()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s indisponível", tool)
		}
	}
	base := t.TempDir()
	s := pitrSandbox{root: filepath.Join(base, "repositorio"), bin: filepath.Join(base, "bin"), log: filepath.Join(base, "docker.log")}
	scripts := filepath.Join(s.root, "deploy", "backup")
	for _, dir := range []string{scripts, s.bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"common.sh", "pitr-restore.sh", "keycloak-export.sh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "backup", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scripts, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	docker := filepath.Join(s.bin, "docker")
	if err := os.WriteFile(docker, []byte(fakePITRDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(docker, "compose", "config").Run(); err != nil {
		t.Skipf("diretório temporário sem permissão de execução: %v", err)
	}
	return s
}

func (s pitrSandbox) run(t *testing.T, script string, args []string, vars ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{filepath.Join(s.root, "deploy", "backup", script)}, args...)...)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "RECORVIA_") && !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "COMPOSE_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "PATH="+s.bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_LOG="+s.log)
	cmd.Env = append(cmd.Env, vars...)
	cmd.Dir = s.root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
}

func (s pitrSandbox) dockerLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(s.log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// Um servidor em execução, reiniciando (crash loop) ou pausado ainda usa o
// volume: a restauração recusa sem parar o agente nem remover o container.
func TestPITRRestoreRefusesServerThatIsNotStopped(t *testing.T) {
	for _, state := range []string{"running", "restarting", "paused", "dead"} {
		t.Run(state, func(t *testing.T) {
			s := newPITRSandbox(t)
			stderr, err := s.run(t, "pitr-restore.sh", []string{"--latest"}, "FAKE_PG_STATE="+state)
			if err == nil || !strings.Contains(stderr, "está no estado "+state) {
				t.Fatalf("estado %s aceito: %v\n%s", state, err, stderr)
			}
			if log := s.dockerLog(t); log != "" {
				t.Errorf("o roteiro alterou o ambiente antes de recusar:\n%s", log)
			}
		})
	}
	for _, state := range []string{"", "exited", "created"} {
		s := newPITRSandbox(t)
		_, _ = s.run(t, "pitr-restore.sh", []string{"--latest"}, "FAKE_PG_STATE="+state)
		if !strings.Contains(s.dockerLog(t), " restore --latest") {
			t.Errorf("estado %q deveria seguir para a restauração:\n%s", state, s.dockerLog(t))
		}
	}
}

// A restauração recusada (chave, volume com dados, backup ausente) religa o
// agente de backup parado no passo 1, e a mensagem não afirma nada sobre o
// volume.
func TestPITRRestoreRestartsAgentWhenRestoreFails(t *testing.T) {
	s := newPITRSandbox(t)
	stderr, err := s.run(t, "pitr-restore.sh", []string{"--target-time", "2026-11-22T10:15:00Z", "--timeline", "1"}, "FAKE_PG_STATE=exited")
	if err == nil || !strings.Contains(stderr, "restauração recusada ou interrompida") {
		t.Fatalf("falha da preparação: %v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "continua vazio") {
		t.Errorf("a mensagem afirma o estado do volume:\n%s", stderr)
	}
	log := s.dockerLog(t)
	stop := strings.Index(log, "compose stop postgres-backup")
	up := strings.Index(log, "compose up -d --no-deps postgres-backup")
	if stop < 0 || up < stop {
		t.Errorf("o agente deveria ser parado e religado:\n%s", log)
	}
	if !strings.Contains(log, "restore --target-time 2026-11-22T10:15:00Z --timeline 1") {
		t.Errorf("--timeline não repassado:\n%s", log)
	}
	if _, err := s.run(t, "pitr-restore.sh", []string{"--latest", "--timeline", "x"}); err == nil {
		t.Error("linha do tempo inválida aceita")
	}
}

// Com o banco embutido do Keycloak (dev-file, compose local), um container
// avulso exportaria um banco vazio: a exportação é recusada.
func TestKeycloakExportRequiresExternalDatabase(t *testing.T) {
	for _, db := range []string{"", "dev-file", "dev-mem"} {
		s := newPITRSandbox(t)
		stderr, err := s.run(t, "keycloak-export.sh", nil, "FAKE_KC_DB="+db)
		if err == nil || !strings.Contains(stderr, "banco embutido") {
			t.Fatalf("KC_DB=%q aceito: %v\n%s", db, err, stderr)
		}
		if log := s.dockerLog(t); strings.Contains(log, " run ") || strings.Contains(log, "put-object") {
			t.Errorf("KC_DB=%q: exportação executada:\n%s", db, log)
		}
	}
	s := newPITRSandbox(t)
	stderr, err := s.run(t, "keycloak-export.sh", nil, "FAKE_KC_DB=postgres")
	if err != nil {
		t.Fatalf("KC_DB=postgres: %v\n%s", err, stderr)
	}
	if log := s.dockerLog(t); !strings.Contains(log, "recorvia-pgbackup put-object keycloak/realm-recorvia-") {
		t.Errorf("exportação não enviada ao agente:\n%s", log)
	}
}
