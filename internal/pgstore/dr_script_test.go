package pgstore_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeComposeDocker imita, no PATH do exercício de recuperação de desastre
// (deploy/backup/dr-check.sh), o docker das primeiras etapas: informa a
// versão do compose, lista os containers e volumes do projeto conforme
// FAKE_CONTAINERS, FAKE_OWN_CONTAINERS, FAKE_VOLUMES e FAKE_OWN_VOLUMES (os
// "OWN" são os que levam o rótulo do exercício), registra em FAKE_LOG tudo o
// que alteraria o ambiente e falha na construção das imagens, onde o roteiro
// para.
const fakeComposeDocker = `#!/usr/bin/env bash
set -u
own=0
for arg in "$@"; do
  [[ $arg == label=recorvia.dr-check=1 ]] && own=1
done
case "${1:-} ${2:-}" in
  "compose version") echo 2.30.0; exit 0 ;;
  "ps -a")
    if ((own)); then printf %s "${FAKE_OWN_CONTAINERS:-}"; else printf %s "${FAKE_CONTAINERS:-}"; fi
    exit 0 ;;
  "volume ls")
    if ((own)); then printf %s "${FAKE_OWN_VOLUMES:-}"; else printf %s "${FAKE_VOLUMES:-}"; fi
    exit 0 ;;
esac
echo "$*" >>"$FAKE_LOG"
for arg in "$@"; do
  [[ $arg == build ]] && exit 1
done
exit 0
`

// drSandbox é uma cópia dos scripts de deploy/backup numa raiz temporária
// (o roteiro trata o diretório dois níveis acima como a raiz do repositório),
// com o docker falso no PATH: o que o roteiro apagar ou criar fica ali.
type drSandbox struct {
	root, bin, log string
}

func newDRSandbox(t *testing.T) drSandbox {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "jq", "openssl", "sha256sum", "base64", "od", "realpath"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s indisponível", tool)
		}
	}
	base := t.TempDir()
	s := drSandbox{root: filepath.Join(base, "repositorio"), bin: filepath.Join(base, "bin"), log: filepath.Join(base, "docker.log")}
	scripts := filepath.Join(s.root, "deploy", "backup")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"common.sh", "dr-lib.sh", "dr-check.sh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "backup", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scripts, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	docker := filepath.Join(s.bin, "docker")
	if err := os.WriteFile(docker, []byte(fakeComposeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(docker, "compose", "version").Run(); err != nil {
		t.Skipf("diretório temporário sem permissão de execução: %v", err)
	}
	return s
}

// run executa o roteiro na raiz temporária e devolve a saída de erro e o
// erro de execução. vars são variáveis KEY=valor do exercício.
func (s drSandbox) run(t *testing.T, vars ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(s.root, "deploy", "backup", "dr-check.sh"))
	var env []string
	for _, kv := range os.Environ() {
		// O ambiente de quem roda os testes não decide o exercício.
		if !strings.HasPrefix(kv, "RECORVIA_") && !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "PATH="+s.bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_LOG="+s.log, "TMPDIR="+t.TempDir())
	cmd.Env = append(cmd.Env, vars...)
	cmd.WaitDelay = 10 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
}

// dockerLog devolve os comandos que alterariam o ambiente recebidos pelo
// docker falso.
func (s drSandbox) dockerLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(s.log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("conteúdo de "+filepath.Base(name)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func requireFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("arquivo que deveria continuar no lugar: %v", err)
		}
		if string(b) != "conteúdo de "+filepath.Base(name) {
			t.Fatalf("%s foi alterado: %q", name, b)
		}
	}
}

// O exercício não apaga o diretório informado em RECORVIA_DR_DIR: recusa, sem
// tocar em nada e sem acionar o docker, o diretório dos backups, a raiz do
// repositório e qualquer diretório com conteúdo que ele não tenha criado.
func TestDisasterRecoveryScriptRefusesForeignOutputDirectory(t *testing.T) {
	backupFiles := []string{"recorvia-20261101T000000Z.dump", "recorvia-20261101T000000Z.manifest", "recorvia-20261101T000000Z.sha256"}
	cases := []struct {
		name string
		// setup prepara a raiz e devolve as variáveis do exercício e os
		// arquivos (relativos à raiz) que precisam continuar intactos.
		setup   func(t *testing.T, root string) (vars []string, keep []string)
		wantErr string
	}{
		{name: "diretório padrão dos backups", wantErr: "é o diretório dos backups",
			setup: func(t *testing.T, root string) ([]string, []string) {
				writeFiles(t, filepath.Join(root, "backups"), append(slices.Clone(backupFiles), "dr-check/relatorio.md")...)
				keep := []string{"backups/dr-check/relatorio.md"}
				for _, f := range backupFiles {
					keep = append(keep, "backups/"+f)
				}
				return []string{"RECORVIA_DR_DIR=backups"}, keep
			}},
		{name: "diretório dos backups informado por RECORVIA_BACKUP_DIR", wantErr: "é o diretório dos backups",
			setup: func(t *testing.T, root string) ([]string, []string) {
				dir := filepath.Join(root, "..", "copias")
				writeFiles(t, dir, backupFiles...)
				return []string{"RECORVIA_DR_DIR=" + dir, "RECORVIA_BACKUP_DIR=" + dir}, []string{"../copias/" + backupFiles[0]}
			}},
		{name: "diretório dos backups vazio", wantErr: "é o diretório dos backups",
			setup: func(t *testing.T, root string) ([]string, []string) {
				if err := os.MkdirAll(filepath.Join(root, "backups"), 0o755); err != nil {
					t.Fatal(err)
				}
				return []string{"RECORVIA_DR_DIR=./backups/"}, nil
			}},
		{name: "diretório com conteúdo que o exercício não criou", wantErr: "não foi criado pelo exercício",
			setup: func(t *testing.T, root string) ([]string, []string) {
				dir := filepath.Join(root, "..", "trabalho")
				writeFiles(t, dir, "planilha.ods", "notas/pendencias.md")
				return []string{"RECORVIA_DR_DIR=" + dir}, []string{"../trabalho/planilha.ods", "../trabalho/notas/pendencias.md"}
			}},
		{name: "raiz do repositório", wantErr: "raiz do repositório",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"RECORVIA_DR_DIR=."}, []string{"deploy/backup/common.sh"}
			}},
		{name: "diretório acima da raiz", wantErr: "raiz do repositório",
			setup: func(t *testing.T, root string) ([]string, []string) {
				writeFiles(t, filepath.Join(root, ".."), "vizinho.txt")
				return []string{"RECORVIA_DR_DIR=.."}, []string{"../vizinho.txt", "deploy/backup/common.sh"}
			}},
		{name: "arquivo comum", wantErr: "não é um diretório",
			setup: func(t *testing.T, root string) ([]string, []string) {
				writeFiles(t, root, "saida")
				return []string{"RECORVIA_DR_DIR=saida"}, []string{"saida"}
			}},
		{name: "diretório padrão de uma execução sem a marca do exercício", wantErr: "não foi criado pelo exercício",
			setup: func(t *testing.T, root string) ([]string, []string) {
				writeFiles(t, filepath.Join(root, "backups", "dr-check"), "arquivo-de-outra-origem.txt")
				return nil, []string{"backups/dr-check/arquivo-de-outra-origem.txt"}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newDRSandbox(t)
			vars, keep := tc.setup(t, s.root)
			stderr, err := s.run(t, vars...)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("o exercício deveria recusar o diretório: %v\n%s", err, stderr)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Fatalf("mensagem esperada %q:\n%s", tc.wantErr, stderr)
			}
			for _, name := range keep {
				if name == "deploy/backup/common.sh" {
					if _, err := os.Stat(filepath.Join(s.root, name)); err != nil {
						t.Fatalf("a raiz do repositório foi alterada: %v", err)
					}
					continue
				}
				requireFiles(t, s.root, name)
			}
			if log := s.dockerLog(t); log != "" {
				t.Fatalf("nenhum comando do docker deveria ter sido executado:\n%s", log)
			}
			if strings.Contains(stderr, "exercício interrompido") {
				t.Fatalf("a recusa acontece antes de o exercício começar:\n%s", stderr)
			}
		})
	}
}

// Num diretório novo, o exercício cria e marca o diretório; num diretório de
// uma execução anterior, os arquivos dela vão para anterior/ (no lugar da
// penúltima) e o que não é do exercício fica onde estava.
func TestDisasterRecoveryScriptKeepsPreviousRun(t *testing.T) {
	s := newDRSandbox(t)
	dir := filepath.Join(s.root, "backups", "dr-check")

	// Primeira execução: o docker falso falha na construção das imagens.
	stderr, err := s.run(t)
	if err == nil || !strings.Contains(stderr, "exercício interrompido") {
		t.Fatalf("esperava a interrupção na construção das imagens: %v\n%s", err, stderr)
	}
	for _, name := range []string{".recorvia-dr-check", "conferencias.txt", "observacoes.txt", "compose-logs.txt", "backup"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("a primeira execução deveria ter criado %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "anterior")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sem execução anterior não há o que guardar: %v", err)
	}

	// Resultado de uma execução completa e arquivos de quem opera.
	writeFiles(t, dir, "relatorio.md", "restaurado.manifest", "restaurado-final.efeitos", "seed.log",
		"backup/recorvia-20261120T010203Z.dump", "notas-do-incidente.md", "evidencias/captura.png", "anterior/penultima.txt")
	stderr, err = s.run(t)
	if err == nil || !strings.Contains(stderr, "exercício interrompido") {
		t.Fatalf("esperava a interrupção na construção das imagens: %v\n%s", err, stderr)
	}
	requireFiles(t, dir, "anterior/relatorio.md", "anterior/restaurado.manifest", "anterior/restaurado-final.efeitos", "anterior/seed.log",
		"anterior/backup/recorvia-20261120T010203Z.dump", "notas-do-incidente.md", "evidencias/captura.png")
	for _, name := range []string{"relatorio.md", "restaurado.manifest", "seed.log", "backup/recorvia-20261120T010203Z.dump", "anterior/penultima.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s deveria ter saído do lugar: %v", name, err)
		}
	}
	// As conferências da execução interrompida anterior também foram guardadas.
	if _, err := os.Stat(filepath.Join(dir, "anterior", "conferencias.txt")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.dockerLog(t), "-p recorvia-drcheck ") {
		t.Fatalf("projeto padrão do exercício inesperado:\n%s", s.dockerLog(t))
	}
}

// Antes de derrubar o projeto do compose com os volumes, o exercício confere
// que tudo o que existe nele foi criado por ele (rótulo recorvia.dr-check):
// um projeto de mesmo nome criado de outra forma não é removido.
func TestDisasterRecoveryScriptRefusesForeignProject(t *testing.T) {
	cases := []struct {
		name    string
		vars    []string
		wantErr string // vazio: o exercício segue e derruba o próprio projeto
		wantLog string
	}{
		{name: "projeto inexistente", wantLog: "-p recorvia-drcheck "},
		{name: "sobras do próprio exercício", wantLog: "-p restauracao-manual ",
			vars: []string{"RECORVIA_DR_PROJECT=restauracao-manual", "FAKE_CONTAINERS=aaa\nbbb\n", "FAKE_OWN_CONTAINERS=bbb\naaa\n",
				"FAKE_VOLUMES=restauracao-manual_postgres-data\n", "FAKE_OWN_VOLUMES=restauracao-manual_postgres-data\n"}},
		{name: "container que o exercício não criou", wantErr: "já tem containers que não foram criados pelo exercício",
			vars: []string{"RECORVIA_DR_PROJECT=restauracao-manual", "FAKE_CONTAINERS=aaa\nbbb\n", "FAKE_OWN_CONTAINERS=aaa\n"}},
		{name: "volume que o exercício não criou", wantErr: "já tem volumes que não foram criados pelo exercício",
			vars: []string{"RECORVIA_DR_PROJECT=restauracao-manual", "FAKE_VOLUMES=restauracao-manual_postgres-data\n"}},
		{name: "container e volume de outra origem", wantErr: "já tem containers e volumes que não foram criados pelo exercício",
			vars: []string{"RECORVIA_DR_PROJECT=restauracao-manual", "FAKE_CONTAINERS=aaa\n", "FAKE_VOLUMES=restauracao-manual_postgres-data\n"}},
		{name: "remoção confirmada por variável", wantLog: "-p restauracao-manual ",
			vars: []string{"RECORVIA_DR_PROJECT=restauracao-manual", "RECORVIA_DR_REUSE_PROJECT=1", "FAKE_CONTAINERS=aaa\n",
				"FAKE_VOLUMES=restauracao-manual_postgres-data\n"}},
		{name: "projeto do ambiente local", wantErr: "não pode usar o projeto do ambiente local",
			vars: []string{"RECORVIA_DR_PROJECT=recorvia", "RECORVIA_DR_REUSE_PROJECT=1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newDRSandbox(t)
			stderr, err := s.run(t, tc.vars...)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("o roteiro termina com erro nos dois casos (recusa ou docker falso): %v\n%s", err, stderr)
			}
			log := s.dockerLog(t)
			if tc.wantErr != "" {
				if !strings.Contains(stderr, tc.wantErr) {
					t.Fatalf("mensagem esperada %q:\n%s", tc.wantErr, stderr)
				}
				if log != "" {
					t.Fatalf("nada do projeto pode ser removido nem criado:\n%s", log)
				}
				if _, err := os.Stat(filepath.Join(s.root, "backups")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("a recusa não deveria criar o diretório de saída: %v", err)
				}
				return
			}
			if !strings.Contains(stderr, "exercício interrompido") {
				t.Fatalf("esperava a interrupção na construção das imagens:\n%s", stderr)
			}
			first, _, _ := strings.Cut(log, "\n")
			if !strings.Contains(first, tc.wantLog) || !strings.HasSuffix(first, "down -v --remove-orphans") {
				t.Fatalf("o exercício deveria começar derrubando o próprio projeto (%s): %q", tc.wantLog, first)
			}
		})
	}
}

// drLib executa um trecho de bash com as funções do exercício carregadas.
func drLib(t *testing.T, stdin, script string, args ...string) (string, string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash indisponível")
	}
	dir := filepath.Join("..", "..", "deploy", "backup")
	full := "set -euo pipefail\nsource " + filepath.Join(dir, "common.sh") + "\nsource " + filepath.Join(dir, "dr-lib.sh") + "\n" + script
	cmd := exec.Command("bash", append([]string{"-c", full, "dr-lib"}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

// A varredura do dump procura cada segredo como texto e como bytes em
// hexadecimal: é assim que o pg_dump escreve as colunas bytea, onde ficam os
// dados cifrados. Um segredo gravado sem cifrar numa dessas colunas não
// aparece como texto.
func TestDisasterRecoveryDumpScanFindsSecretsInBytea(t *testing.T) {
	const secret = "sim_dr_0123456789abcdef"
	const other = "whsec-de-outro-endpoint-0001"
	copyLine := func(value string) string {
		return "COPY public.payment_provider_accounts (id, name, api_key_ciphertext) FROM stdin;\n1\tCliente A do exercício\t" + value + "\n\\.\n"
	}
	cases := []struct {
		name    string
		dump    string
		secrets []string
		want    string
	}{
		{name: "dado cifrado", dump: copyLine(`\\x` + hex.EncodeToString([]byte("bytes sem relação com o segredo"))), secrets: []string{secret}, want: "0"},
		{name: "segredo em claro numa coluna de texto", dump: copyLine(secret), secrets: []string{secret}, want: "1"},
		{name: "segredo em claro numa coluna bytea", dump: copyLine(`\\x` + hex.EncodeToString([]byte(secret))), secrets: []string{secret}, want: "1"},
		{name: "segredo em claro no meio de uma coluna bytea", secrets: []string{other, secret}, want: "1",
			dump: copyLine(`\\x` + hex.EncodeToString([]byte(`{"api_key":"`+secret+`"}`)))},
		{name: "dois segredos em linhas diferentes", secrets: []string{secret, other}, want: "2",
			dump: copyLine(`\\x`+hex.EncodeToString([]byte(secret))) + copyLine(other)},
		{name: "segredo vazio é ignorado", dump: copyLine("qualquer coisa"), secrets: []string{"", secret}, want: "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, stderr, err := drLib(t, tc.dump, `count_secret_leaks "$@"`, tc.secrets...)
			if err != nil {
				t.Fatalf("varredura: %v\n%s", err, stderr)
			}
			if got != tc.want {
				t.Fatalf("linhas com segredo = %s, esperado %s", got, tc.want)
			}
		})
	}
	t.Run("sem segredos a procurar", func(t *testing.T) {
		_, stderr, err := drLib(t, copyLine(secret), `count_secret_leaks "$@"`, "")
		if err == nil || !strings.Contains(stderr, "sem segredos a procurar") {
			t.Fatalf("a varredura sem segredos deveria falhar: %v\n%s", err, stderr)
		}
	})
	t.Run("hexadecimal dos bytes", func(t *testing.T) {
		got, stderr, err := drLib(t, "", `hex_of "$1"`, "Fatura nº 10\n")
		if err != nil || got != hex.EncodeToString([]byte("Fatura nº 10\n")) {
			t.Fatalf("hex_of = %q (%v)\n%s", got, err, stderr)
		}
	})
}

// O roteiro do exercício faz a rotação da chave em duas fases, como o guia de
// operação: a chave nova é distribuída a todos os processos, com reinício,
// antes de virar a ativa; na ativação, a API é reiniciada primeiro e o
// trabalho da origem acontece com o worker ainda na fase anterior.
func TestDisasterRecoveryScriptRotatesKeyInTwoPhases(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "backup", "dr-check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	backup := strings.Index(script, "# ---------- 3. Backup")
	if backup < 0 {
		t.Fatal("etapa do backup não encontrada no roteiro")
	}
	// Passos relevantes da origem, na ordem em que aparecem: troca de chaves,
	// (re)início da API e do worker e o trabalho que depende das chaves.
	step := regexp.MustCompile(`(?m)^(use_keys "([^"]*)" (\S+)|dc up -d --wait [^\n]*\b(api|worker)\b[^\n]*|login|wait_until "cobrança da fatura A criada no provedor"[^\n]*)$`)
	var steps []string
	for _, m := range step.FindAllStringSubmatch(script[:backup], -1) {
		switch {
		case strings.HasPrefix(m[0], "use_keys"):
			steps = append(steps, "chaves="+strings.Join(regexp.MustCompile(`dr\d`).FindAllString(m[2], -1), "+")+" ativa="+m[3])
		case strings.HasPrefix(m[0], "dc up"):
			services := regexp.MustCompile(`\b(api|worker)\b`).FindAllString(m[0], -1)
			steps = append(steps, "reinicia="+strings.Join(services, "+"))
		case m[0] == "login":
			steps = append(steps, "trabalho")
		default:
			steps = append(steps, "worker-decifra")
		}
	}
	want := []string{
		"chaves=dr1 ativa=dr1", "reinicia=api+worker", // origem, antes da rotação
		"chaves=dr1+dr2 ativa=dr1", "reinicia=api+worker", // fase 1: todos conhecem a chave nova
		"chaves=dr1+dr2 ativa=dr2", "reinicia=api", // fase 2: a API ativa a chave nova primeiro
		"trabalho", "worker-decifra", // convivência das fases
		"reinicia=worker", // fim da fase 2
	}
	if !slices.Equal(steps, want) {
		t.Fatalf("rotação fora das duas fases:\n obtido  %q\n esperado %q", steps, want)
	}
}
