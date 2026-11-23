package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
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
		if got := objectName.MatchString(name); got != ok {
			t.Errorf("%q: %v, esperado %v", name, got, ok)
		}
	}
}
