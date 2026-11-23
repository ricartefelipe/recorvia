package pgbackup

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseHistoryAndAncestry(t *testing.T) {
	content := "1\t0/5000098\tbefore 2026-11-22 11:00:00+00\n# comentário\n\n2\t0/9000000\tno recovery target specified\n"
	a, err := ParseHistory(3, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		finish uint64
		want   bool
	}{
		{"base_000000010000000000000003", 0x3000000, true},  // linha 1 antes da ramificação
		{"base_000000010000000000000007", 0x7000000, false}, // linha 1 depois da ramificação: abandonada
		{"base_000000020000000000000006", 0x6000000, true},
		{"base_00000002000000000000000A", 0xA000000, false},
		{"base_00000003000000000000000B", 0xB000000, true},  // a própria linha alvo
		{"base_00000004000000000000000C", 0xC000000, false}, // linha fora do histórico
		{"base_000000010000000000000003", 0, false},         // sem o LSN final não há como conferir
	}
	for _, tc := range cases {
		if got := a.Contains(Backup{Name: tc.name, FinishLSN: tc.finish}); got != tc.want {
			t.Errorf("Contains(%s, %x) = %v, esperado %v", tc.name, tc.finish, got, tc.want)
		}
	}
	for _, bad := range []string{"x\t0/1\tmotivo", "1\tsem-barra\tmotivo", "3\t0/1\tlinha igual à alvo", "1"} {
		if _, err := ParseHistory(3, []byte(bad)); err == nil {
			t.Errorf("histórico inválido aceito: %q", bad)
		}
	}
	if lsn, err := ParseLSN("1A/2B"); err != nil || lsn != 0x1A0000002B {
		t.Errorf("ParseLSN = %x, %v", lsn, err)
	}
}

// Depois de uma restauração para T1 (linha 2 ramificada da 1 em 0/5000000),
// um backup feito na linha 1 abandonada, depois da ramificação, não serve à
// linha 2, mesmo sendo o mais recente antes do alvo: a recuperação terminaria
// com "requested timeline 2 is not a child of this server's history".
func TestForTargetSkipsBackupsOfAbandonedTimeline(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 11, 22, h, 0, 0, 0, time.UTC) }
	list := []Backup{
		{Name: "base_000000010000000000000003", FinishTime: at(10), FinishLSN: 0x3000100},
		{Name: "base_000000010000000000000008", FinishTime: at(12), FinishLSN: 0x8000100},
		{Name: "base_000000020000000000000006", FinishTime: at(14), FinishLSN: 0x6000100},
	}
	tl2 := NewAncestry(2, map[uint32]uint64{1: 0x5000000})
	b, err := ForTarget(list, time.Date(2026, 11, 22, 13, 0, 0, 0, time.UTC), tl2)
	if err != nil || b.Name != "base_000000010000000000000003" {
		t.Fatalf("linha 2: %q, %v; esperado o backup anterior à ramificação", b.Name, err)
	}
	// Na linha 1 original, o backup posterior à ramificação é o certo.
	b, err = ForTarget(list, time.Date(2026, 11, 22, 13, 0, 0, 0, time.UTC), NewAncestry(1, nil))
	if err != nil || b.Name != "base_000000010000000000000008" {
		t.Fatalf("linha 1: %q, %v", b.Name, err)
	}
}

func TestPrepareRestoreFollowsTimelines(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := validEnv()
	newCfg := func(t *testing.T) Config {
		m["PGDATA"] = filepath.Join(t.TempDir(), "18", "docker")
		cfg, err := Load(env(m), noFiles)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cfg := newCfg(t)
	fp := cfg.KeyFingerprint()
	list := `[
	 {"backup_name":"base_000000010000000000000003","finish_time":"2026-11-22T10:00:00Z","finish_lsn":50331904,"user_data":{"recorvia_key_fingerprint":"` + fp + `"}},
	 {"backup_name":"base_000000010000000000000008","finish_time":"2026-11-22T12:00:00Z","finish_lsn":134217984,"user_data":{"recorvia_key_fingerprint":"` + fp + `"}},
	 {"backup_name":"base_000000020000000000000006","finish_time":"2026-11-22T14:00:00Z","finish_lsn":100663552,"user_data":{"recorvia_key_fingerprint":"` + fp + `"}}
	]`
	files := map[string]string{"00000002.history": "1\t0/5000000\tbefore 2026-11-22 11:00:00+00\n"}
	target := time.Date(2026, 11, 22, 13, 0, 0, 0, time.UTC)
	fetched := func(r *fakeRunner) string {
		for _, c := range r.calls {
			if c[0] == "backup-fetch" {
				return c[2]
			}
		}
		return ""
	}
	auto := func(cfg Config) string {
		b, _ := os.ReadFile(filepath.Join(cfg.PGData, "postgresql.auto.conf"))
		return string(b)
	}

	// Padrão: a linha mais recente (2), explícita na configuração, com o
	// backup anterior à ramificação.
	r := &fakeRunner{list: list, files: files}
	plan, err := PrepareRestore(context.Background(), r, cfg, RestoreOptions{Target: target}, log)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Timeline != 2 || plan.LatestTimeline != 2 || fetched(r) != "base_000000010000000000000003" ||
		!strings.Contains(auto(cfg), "recovery_target_timeline = '2'") {
		t.Fatalf("linha padrão: %+v, backup %s\n%s", plan, fetched(r), auto(cfg))
	}

	// Linha 1 pedida: segue a original e avisa que há linha mais nova.
	cfg = newCfg(t)
	r = &fakeRunner{list: list, files: files}
	plan, err = PrepareRestore(context.Background(), r, cfg, RestoreOptions{Target: target, Timeline: 1}, log)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Timeline != 1 || plan.LatestTimeline != 2 || fetched(r) != "base_000000010000000000000008" ||
		!strings.Contains(auto(cfg), "recovery_target_timeline = '1'") {
		t.Fatalf("linha 1: %+v, backup %s\n%s", plan, fetched(r), auto(cfg))
	}

	// Backup indicado fora do histórico da linha: recusado antes da extração.
	cfg = newCfg(t)
	r = &fakeRunner{list: list, files: files}
	_, err = PrepareRestore(context.Background(), r, cfg, RestoreOptions{Target: target, BackupName: "base_000000010000000000000008"}, log)
	if err == nil || !strings.Contains(err.Error(), "não está no histórico da linha do tempo 2") || fetched(r) != "" {
		t.Fatalf("backup fora do histórico: %v (extraído: %q)", err, fetched(r))
	}

	// Só backups da linha abandonada antes do alvo: mensagem que sugere --backup.
	cfg = newCfg(t)
	r = &fakeRunner{list: list, files: map[string]string{"00000002.history": "1\t0/1000000\tbefore 2026-11-22 09:00:00+00\n"}}
	_, err = PrepareRestore(context.Background(), r, cfg, RestoreOptions{Target: target}, log)
	if err == nil || !strings.Contains(err.Error(), "--backup") || fetched(r) != "" {
		t.Fatalf("sem backup no histórico: %v", err)
	}

	// Histórico ilegível (chave errada nos arquivos .history): erro, não "fim".
	cfg = newCfg(t)
	r = &fakeRunner{list: list, fail: map[string]error{"wal-fetch": exitCode(1)}}
	if _, err = PrepareRestore(context.Background(), r, cfg, RestoreOptions{Target: target}, log); err == nil ||
		!strings.Contains(err.Error(), "00000002.history") {
		t.Fatalf("histórico ilegível: %v", err)
	}
}
