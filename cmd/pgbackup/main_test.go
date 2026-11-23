package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ricartefelipe/recorvia/internal/infra/pgbackup"
)

func TestRunRefusesWithoutConfiguration(t *testing.T) {
	empty := func(string) string { return "" }
	cases := []struct {
		args []string
		code int
	}{
		{nil, 2},
		{[]string{"version"}, 0},
		{[]string{"check-config"}, 1},
		{[]string{"wal-push", "pg_wal/000000010000000000000001"}, 1},
		// Sem configuração, o restore_command devolve erro fatal: a recuperação
		// não pode concluir que o WAL acabou.
		{[]string{"wal-fetch", "000000010000000000000001", "pg_wal/RECOVERYXLOG"}, 126},
		{[]string{"restore", "--latest"}, 1},
	}
	for _, tc := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), tc.args, empty, &stdout, &stderr); code != tc.code {
			t.Errorf("%v: código %d, esperado %d (%s)", tc.args, code, tc.code, stderr.String())
		}
	}
}

func TestRunCheckConfigShowsFingerprintNotKey(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	env := map[string]string{"RECORVIA_BACKUP_S3_PREFIX": "s3://recorvia-backup/teste", "RECORVIA_BACKUP_ENCRYPTION_KEY": key}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"check-config"}, func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
		t.Fatalf("código %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "s3://recorvia-backup/teste") || strings.Contains(stdout.String(), key) {
		t.Errorf("saída inesperada: %q", stdout.String())
	}
}

func TestObjectNames(t *testing.T) {
	for name, ok := range map[string]bool{
		"keycloak/realm-recorvia-20261122T101500Z.json": true,
		"relatorio.txt":     true,
		"../wal_005/x":      false,
		"/absoluto":         false,
		"keycloak//duplo":   false,
		"keycloak/a b.json": false,
	} {
		if got := pgbackup.ValidObjectName(name); got != ok {
			t.Errorf("%q: %v, esperado %v", name, got, ok)
		}
	}
}

// Nos comandos que escrevem dados na saída padrão, o log (inclusive o de
// erro) vai para a saída de erro: list | ... e get-object > arquivo recebem
// só os dados.
func TestDataCommandsLogToStderr(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "wal-g")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"  'backup-list --json') echo '[{\"backup_name\":\"base_000000010000000000000003\",\"finish_time\":\"2026-11-22T10:00:00Z\"}]' ;;\n" +
		"  'st ls') echo 'obj 2 2026-11-22 10:00:00 +0000 UTC realm.json.lz4' ;;\n" +
		"  'st get') printf '{}' >\"$4\" ;;\n" +
		"  *) echo \"falha simulada: $*\" >&2; exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fake, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fake); err != nil {
		t.Fatal(err)
	}
	prev := walgBin
	walgBin = fake
	defer func() { walgBin = prev }()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	env := map[string]string{"RECORVIA_BACKUP_S3_PREFIX": "s3://recorvia-backup/teste", "RECORVIA_BACKUP_ENCRYPTION_KEY": key}
	getenv := func(k string) string { return env[k] }

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"list"}, getenv, &stdout, &stderr); code != 0 {
		t.Skipf("WAL-G falso não executou (disco sem permissão de execução?): %s", stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "BACKUP") || strings.Contains(stdout.String(), `"level"`) {
		t.Errorf("list: saída padrão com algo além da tabela: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"get-object", "keycloak/realm.json"}, getenv, &stdout, &stderr); code != 0 || stdout.String() != "{}" {
		t.Errorf("get-object: código %d, saída %q (%s)", code, stdout.String(), stderr.String())
	}

	// Com falha, o erro fica na saída de erro e a saída padrão, vazia.
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"get-object", "keycloak/outro.json"}, getenv, &stdout, &stderr); code != 1 {
		t.Errorf("get-object inexistente: código %d", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "não encontrado") {
		t.Errorf("get-object inexistente: saída %q, erro %q", stdout.String(), stderr.String())
	}
}
