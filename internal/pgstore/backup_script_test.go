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

// fakeDocker imita, no PATH do script, o docker exec dos comandos do
// PostgreSQL usados por deploy/backup/backup.sh. A sessão do snapshot (psql)
// responde ao manifesto conforme FAKE_MANIFEST:
//
//	complete   todas as linhas e a sentinela
//	slow       para antes das somas das partidas e espera (consulta lenta);
//	           termina com status 0 ao receber \q, como o psql
//	no-ledger  sentinela sem as linhas do ledger
//	closed     a sessão termina antes da sentinela
const fakeDocker = `#!/usr/bin/env bash
set -u
[[ ${1:-} == exec ]] || exit 99
shift
[[ $1 == -i ]] && shift
shift
case $1 in
  pg_isready) exit 0 ;;
  pg_dump) printf 'PGDMP-falso'; exit 0 ;;
  pg_restore) cat >/dev/null; exit 0 ;;
  psql) ;;
  *) exit 98 ;;
esac
while IFS= read -r line; do
  case $line in
    "SELECT pg_export_snapshot();") echo "00000003-0000001B-1" ;;
    "SELECT 'end-of-manifest';")
      printf '%s\n' "table invoices 3" "goose_version 12" "rls_tables 40" "policies 41" "triggers 9"
      case $FAKE_MANIFEST in
        complete) printf '%s\n' "ledger_transactions_md5 aaaa" "ledger_entries_md5 bbbb" end-of-manifest ;;
        slow) echo "ledger_transactions_md5 aaaa" ;;
        no-ledger) echo end-of-manifest ;;
        closed) exit 0 ;;
      esac ;;
    '\q') exit 0 ;;
  esac
done
exit 0
`

// Um manifesto incompleto (prazo esgotado na leitura, sessão encerrada ou
// linhas obrigatórias ausentes) não pode ser publicado como backup
// concluído: a verificação posterior falharia sempre no diff.
func TestBackupScriptRejectsIncompleteManifest(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash indisponível")
	}
	bin := t.TempDir()
	docker := filepath.Join(bin, "docker")
	if err := os.WriteFile(docker, []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(docker, "exec", "x", "pg_isready").Run(); err != nil {
		t.Skipf("diretório temporário sem permissão de execução: %v", err)
	}
	cases := []struct {
		scenario string
		wantErr  string
	}{
		{"complete", ""},
		{"slow", "manifesto incompleto: a sessão do snapshot não concluiu"},
		{"closed", "manifesto incompleto"},
		{"no-ledger", "manifesto incompleto: sem a linha ledger_transactions_md5"},
	}
	for _, c := range cases {
		t.Run(c.scenario, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", filepath.Join("..", "..", "deploy", "backup", "backup.sh"))
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "RECORVIA_PG_CONTAINER=falso",
				"RECORVIA_BACKUP_DIR="+dir, "RECORVIA_BACKUP_MANIFEST_TIMEOUT=1", "FAKE_MANIFEST="+c.scenario)
			cmd.WaitDelay = 10 * time.Second
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			published, globErr := filepath.Glob(filepath.Join(dir, "*"))
			if globErr != nil {
				t.Fatal(globErr)
			}
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("backup completo falhou: %v\n%s", err, stderr.String())
				}
				if len(published) != 3 {
					t.Fatalf("esperava .dump, .manifest e .sha256: %v", published)
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("o backup deveria falhar: %v\n%s", err, stderr.String())
			}
			if !strings.Contains(stderr.String(), c.wantErr) {
				t.Fatalf("mensagem esperada %q:\n%s", c.wantErr, stderr.String())
			}
			if strings.Contains(stderr.String(), "backup concluído") || len(published) != 0 {
				t.Fatalf("nada pode ser publicado: %v\n%s", published, stderr.String())
			}
		})
	}
}
