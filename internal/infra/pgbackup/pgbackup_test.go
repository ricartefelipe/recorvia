package pgbackup

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

var testKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, KeySize))

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func noFiles(string) ([]byte, error) { return nil, os.ErrNotExist }

func validEnv() map[string]string {
	return map[string]string{EnvS3Prefix: "s3://recorvia-backup/prod", EnvEncryptionKey: testKey, "PGDATA": "/var/lib/postgresql/18/docker"}
}

func TestLoadDefaultsAndValidation(t *testing.T) {
	cfg, err := Load(env(validEnv()), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3Region != "us-east-1" || cfg.BaseInterval != 24*time.Hour || cfg.RetryInterval != 15*time.Minute ||
		cfg.RetainFull != 7 || cfg.RetainDays != 7 || cfg.MetricsAddr != ":9187" || cfg.S3ForcePath {
		t.Errorf("padrões inesperados: %+v", cfg)
	}

	cases := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"sem destino", map[string]string{EnvS3Prefix: ""}, EnvS3Prefix + " é obrigatório"},
		{"destino sem s3://", map[string]string{EnvS3Prefix: "recorvia-backup"}, "s3://bucket"},
		{"sem chave", map[string]string{EnvEncryptionKey: ""}, "chave de cifragem do backup ausente"},
		{"chave curta", map[string]string{EnvEncryptionKey: base64.StdEncoding.EncodeToString([]byte("curta"))}, "chave de cifragem do backup inválida"},
		{"chave e arquivo", map[string]string{EnvEncryptionKey + "_FILE": "/run/secrets/chave"}, "não os dois"},
		{"credencial incompleta", map[string]string{EnvS3AccessKeyID: "AKIA"}, "devem ser informados juntos"},
		{"intervalo curto", map[string]string{EnvBaseInterval: "10s"}, EnvBaseInterval},
		{"retenção zero", map[string]string{EnvRetainFull: "0"}, EnvRetainFull},
		{"dias negativos", map[string]string{EnvRetainDays: "-1"}, EnvRetainDays},
		{"booleano inválido", map[string]string{EnvS3ForcePathStyle: "talvez"}, EnvS3ForcePathStyle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validEnv()
			for k, v := range tc.set {
				m[k] = v
			}
			_, err := Load(env(m), noFiles)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("erro %v; esperado com %q", err, tc.want)
			}
		})
	}
}

func TestLoadSecretsFromFiles(t *testing.T) {
	files := map[string]string{"/run/secrets/chave": testKey + "\n", "/run/secrets/id": "AKIA\n", "/run/secrets/segredo": "s3cr3t\n"}
	read := func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, os.ErrNotExist
	}
	m := validEnv()
	delete(m, EnvEncryptionKey)
	m[EnvEncryptionKey+"_FILE"] = "/run/secrets/chave"
	m[EnvS3AccessKeyID+"_FILE"] = "/run/secrets/id"
	m[EnvS3SecretAccessKey+"_FILE"] = "/run/secrets/segredo"
	m[EnvS3Endpoint] = "http://minio:9000"
	cfg, err := Load(env(m), read)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.WalgEnv([]string{"PATH=/usr/bin", "PGHOST=postgres", "PGPASSWORD=do-ambiente", "WALG_LIBSODIUM_KEY=antiga",
		"RECORVIA_BACKUP_ENCRYPTION_KEY_FILE=/run/secrets/chave", "WALG_UPLOAD_CONCURRENCY=4"})
	for _, want := range []string{"PATH=/usr/bin", "PGHOST=postgres", "PGPASSWORD=do-ambiente", "WALG_UPLOAD_CONCURRENCY=4",
		"WALG_S3_PREFIX=s3://recorvia-backup/prod", "AWS_ENDPOINT=http://minio:9000", "AWS_ACCESS_KEY_ID=AKIA", "AWS_SECRET_ACCESS_KEY=s3cr3t",
		"WALG_LIBSODIUM_KEY_PATH=/run/secrets/chave", "WALG_LIBSODIUM_KEY_TRANSFORM=base64", "WALG_PREVENT_WAL_OVERWRITE=true"} {
		if !slices.Contains(got, want) {
			t.Errorf("ambiente sem %s: %v", want, got)
		}
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, "RECORVIA_BACKUP_") || strings.HasPrefix(kv, "WALG_LIBSODIUM_KEY=") {
			t.Errorf("variável que não deveria chegar ao WAL-G: %s", kv)
		}
	}

	m[EnvEncryptionKey+"_FILE"] = "/run/secrets/inexistente"
	if _, err := Load(env(m), read); err == nil || !strings.Contains(err.Error(), EnvEncryptionKey+"_FILE") {
		t.Fatalf("arquivo de chave ausente aceito: %v", err)
	}
}

func TestWalgEnvWithKeyValueAndPassword(t *testing.T) {
	m := validEnv()
	m[EnvPGPassword] = "senha-do-agente"
	cfg, err := Load(env(m), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.WalgEnv([]string{"PGPASSWORD=do-ambiente"})
	if !slices.Contains(got, "WALG_LIBSODIUM_KEY="+testKey) || !slices.Contains(got, "PGPASSWORD=senha-do-agente") ||
		slices.Contains(got, "PGPASSWORD=do-ambiente") || slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "AWS_ACCESS_KEY_ID=") }) {
		t.Errorf("ambiente inesperado: %v", got)
	}
}

func TestFingerprintIdentifiesKeyWithoutRevealingIt(t *testing.T) {
	cfg, err := Load(env(validEnv()), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	fp := cfg.KeyFingerprint()
	other := Fingerprint(bytes.Repeat([]byte{8}, KeySize))
	if len(fp) != 16 || fp == other || fp != Fingerprint(bytes.Repeat([]byte{7}, KeySize)) {
		t.Errorf("impressões digitais %q e %q", fp, other)
	}
	if strings.Contains(testKey, fp) {
		t.Error("a impressão digital não pode conter a chave")
	}
}

func TestParseBackupListAndChooseForTarget(t *testing.T) {
	raw := `[
	 {"backup_name":"base_000000020000000000000009","start_time":"2026-11-22T12:00:00Z","finish_time":"2026-11-22T12:01:00Z","user_data":{"recorvia_key_fingerprint":"abc"}},
	 {"backup_name":"base_000000010000000000000003","start_time":"2026-11-21T12:00:00Z","finish_time":"2026-11-21T12:01:00Z","start_lsn":50331688,"finish_lsn":50331960}
	]`
	list, err := parseBackupList([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Name != "base_000000010000000000000003" || list[0].Timeline() != 1 || list[1].Timeline() != 2 {
		t.Fatalf("ordem ou linha do tempo inesperadas: %+v", list)
	}
	if list[0].StartLSN != 0x3000028 || list[0].FinishLSN != 0x3000138 {
		t.Errorf("LSN do backup lidos errado: %x %x", list[0].StartLSN, list[0].FinishLSN)
	}
	if list[1].Fingerprint() != "abc" || list[0].Fingerprint() != "" {
		t.Error("impressão digital lida errada")
	}
	if (Backup{Name: "outro"}).Timeline() != 0 {
		t.Error("nome fora do padrão deveria ter linha do tempo zero")
	}
	empty, err := parseBackupList([]byte("\n"))
	if err != nil || len(empty) != 0 || Latest(empty) != nil {
		t.Fatalf("lista vazia: %v %v", empty, err)
	}
	if _, err := parseBackupList([]byte("No backups found")); err == nil {
		t.Error("saída ilegível aceita")
	}

	cases := []struct {
		target time.Time
		want   string
		err    error
	}{
		{time.Time{}, "base_000000020000000000000009", nil},
		{time.Date(2026, 11, 22, 12, 30, 0, 0, time.UTC), "base_000000020000000000000009", nil},
		{time.Date(2026, 11, 22, 12, 0, 30, 0, time.UTC), "base_000000010000000000000003", nil},
		{time.Date(2026, 11, 21, 12, 1, 0, 0, time.UTC), "", ErrNoBackupForTarget},
	}
	for _, tc := range cases {
		b, err := ForTarget(list, tc.target, NewAncestry(2, map[uint32]uint64{1: 0x5000000}))
		if !errors.Is(err, tc.err) || b.Name != tc.want {
			t.Errorf("ForTarget(%s) = %q, %v; esperado %q, %v", tc.target, b.Name, err, tc.want, tc.err)
		}
	}
}

func TestNextBackup(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	recent := &Backup{Name: "base_000000010000000000000003", FinishTime: now.Add(-time.Hour)}
	old := &Backup{Name: "base_000000010000000000000003", FinishTime: now.Add(-25 * time.Hour)}
	cases := []struct {
		name     string
		latest   *Backup
		timeline uint32
		due      time.Time
		reason   string
	}{
		{"sem backup", nil, 1, now, ReasonNone},
		{"recente", recent, 1, now.Add(23 * time.Hour), ""},
		{"vencido", old, 1, now, ReasonInterval},
		{"linha do tempo nova", recent, 2, now, ReasonTimeline},
		{"linha do tempo desconhecida", recent, 0, now.Add(23 * time.Hour), ""},
	}
	for _, tc := range cases {
		due, reason := NextBackup(now, tc.latest, tc.timeline, 24*time.Hour)
		if !due.Equal(tc.due) || reason != tc.reason {
			t.Errorf("%s: %s %q; esperado %s %q", tc.name, due, reason, tc.due, tc.reason)
		}
	}
}

// fakeRunner registra as chamadas e responde conforme o subcomando.
type fakeRunner struct {
	calls  [][]string
	list   string
	fail   map[string]error
	onCall func(args []string)
	// files são os arquivos do WAL arquivado (wal-fetch); ausente devolve o
	// código 74 do WAL-G (arquivo inexistente).
	files map[string]string
	// stdin recebe a entrada de RunInput.
	stdin []byte
}

// exitCode devolve um *exec.ExitError real com o código informado.
func exitCode(code int) error {
	return exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
}

func (f *fakeRunner) Run(_ context.Context, stdout io.Writer, args ...string) error {
	f.calls = append(f.calls, args)
	if f.onCall != nil {
		f.onCall(args)
	}
	if err := f.fail[args[0]]; err != nil {
		return err
	}
	switch args[0] {
	case "backup-list":
		_, _ = io.WriteString(stdout, f.list)
	case "wal-fetch":
		content, ok := f.files[args[1]]
		if !ok {
			return exitCode(74)
		}
		return os.WriteFile(args[2], []byte(content), 0o600)
	}
	return nil
}

func (f *fakeRunner) RunInput(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	b, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	f.stdin = b
	return f.Run(ctx, stdout, args...)
}

func (f *fakeRunner) commands() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

type fakeServer struct {
	timeline uint32
	err      error
}

func (s fakeServer) Timeline(context.Context) (uint32, error) { return s.timeline, s.err }

func testAgent(t *testing.T, r *fakeRunner, now time.Time) (*Agent, *telemetry.BackupMetrics) {
	t.Helper()
	cfg, err := Load(env(validEnv()), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RetainFull, cfg.RetainDays = 3, 2
	m := telemetry.NewBackupMetrics()
	return &Agent{Config: cfg, Runner: r, Server: fakeServer{timeline: 1}, Clock: clock.NewManual(now), Metrics: m,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Lock: func() (func(), error) { return func() {}, nil }}, m
}

func TestBackupNowPushesWithFingerprintAppliesRetentionAndUpdatesMetrics(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	r := &fakeRunner{list: `[{"backup_name":"base_000000010000000000000003","finish_time":"2026-11-22T12:00:00Z"}]`}
	a, m := testAgent(t, r, now)
	if err := a.BackupNow(context.Background(), ReasonNone); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`backup-push /var/lib/postgresql/18/docker --add-user-data {"recorvia_key_fingerprint":"` + a.Config.KeyFingerprint() + `"}`,
		"delete retain FULL 3 --after 2026-11-20T12:00:00Z --confirm",
		"backup-list --json --detail",
	}
	if got := r.commands(); !slices.Equal(got, want) {
		t.Fatalf("chamadas %q; esperado %q", got, want)
	}
	if v := testutil.ToFloat64(m.BaseRuns.WithLabelValues("success")); v != 1 {
		t.Errorf("runs success = %v", v)
	}
	if v := testutil.ToFloat64(m.BaseLastSuccess); v != float64(now.Unix()) {
		t.Errorf("último sucesso = %v", v)
	}
	if v := testutil.ToFloat64(m.BaseRetained); v != 1 {
		t.Errorf("retidos = %v", v)
	}
	if v := testutil.ToFloat64(m.DependencyUp.WithLabelValues("storage")); v != 1 {
		t.Errorf("storage = %v", v)
	}

	// Só a contagem: sem --after.
	r.calls = nil
	a.Config.RetainDays = 0
	if err := a.BackupNow(context.Background(), ReasonInterval); err != nil {
		t.Fatal(err)
	}
	if got := r.commands()[1]; got != "delete retain FULL 3 --confirm" {
		t.Errorf("retenção só pela contagem: %q", got)
	}
}

func TestBackupNowFailures(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	r := &fakeRunner{fail: map[string]error{"backup-push": errors.New("falhou")}}
	a, m := testAgent(t, r, now)
	if err := a.BackupNow(context.Background(), ReasonNone); err == nil {
		t.Fatal("falha do backup não devolvida")
	}
	if len(r.calls) != 1 || testutil.ToFloat64(m.BaseRuns.WithLabelValues("failure")) != 1 {
		t.Errorf("depois da falha do backup: %q", r.commands())
	}

	r = &fakeRunner{fail: map[string]error{"delete": errors.New("sem permissão")}}
	a, m = testAgent(t, r, now)
	if err := a.BackupNow(context.Background(), ReasonNone); err == nil || !strings.Contains(err.Error(), "retenção") {
		t.Fatalf("falha da retenção: %v", err)
	}
	if testutil.ToFloat64(m.BaseRuns.WithLabelValues("success")) != 1 {
		t.Error("o backup feito deveria contar como sucesso")
	}

	a.Lock = func() (func(), error) { return nil, errors.New("outro backup base está em andamento neste container") }
	r.calls = nil
	if err := a.BackupNow(context.Background(), ReasonNone); err == nil || len(r.calls) != 0 {
		t.Errorf("backup com o bloqueio ocupado: %v %q", err, r.commands())
	}

	r = &fakeRunner{fail: map[string]error{"backup-list": errors.New("sem rede")}}
	a, m = testAgent(t, r, now)
	if _, err := a.Refresh(context.Background()); err == nil || testutil.ToFloat64(m.DependencyUp.WithLabelValues("storage")) != 0 {
		t.Error("falha da listagem deveria marcar o armazenamento indisponível")
	}
}

func TestRunSchedulesBackupsAndRetries(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	clk := clock.NewManual(now)
	r := &fakeRunner{fail: map[string]error{"backup-push": errors.New("falhou")}}
	a, _ := testAgent(t, r, now)
	a.Clock = clk
	a.RefreshEvery = 5 * time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sleeps []time.Duration
	a.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		switch len(sleeps) {
		case 1:
			// A primeira tentativa falhou: a seguinte espera o RetryInterval
			// (limitado pela releitura periódica do armazenamento).
			delete(r.fail, "backup-push")
			clk.Set(now.Add(15 * time.Minute))
		case 3:
			cancel()
			return context.Canceled
		}
		return nil
	}
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	var pushes int
	for _, c := range r.calls {
		if c[0] == "backup-push" {
			pushes++
		}
	}
	if pushes != 2 {
		t.Errorf("%d backups; esperado uma falha e um sucesso: %q", pushes, r.commands())
	}
	if sleeps[0] != 5*time.Minute || sleeps[1] != 5*time.Minute {
		t.Errorf("esperas %v", sleeps)
	}

	// Linha do tempo indisponível: nenhuma tentativa, espera RetryInterval.
	r2 := &fakeRunner{list: "[]"}
	a2, _ := testAgent(t, r2, now)
	a2.Server = fakeServer{err: errors.New("recusado")}
	a2.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if err := a2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(r2.calls, func(c []string) bool { return c[0] == "backup-push" }) {
		t.Error("backup sem conhecer o servidor")
	}
}

func TestPrepareRestore(t *testing.T) {
	list := `[{"backup_name":"base_000000010000000000000003","finish_time":"2026-11-22T12:00:00Z","user_data":{"recorvia_key_fingerprint":"FP"}}]`
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	target := time.Date(2026, 11, 22, 12, 30, 0, 123456000, time.UTC)
	newCfg := func(t *testing.T) Config {
		m := validEnv()
		m["PGDATA"] = filepath.Join(t.TempDir(), "18", "docker")
		cfg, err := Load(env(m), noFiles)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("instante", func(t *testing.T) {
		cfg := newCfg(t)
		r := &fakeRunner{list: strings.ReplaceAll(list, "FP", cfg.KeyFingerprint()), onCall: func(args []string) {
			if args[0] == "backup-fetch" {
				_ = os.WriteFile(filepath.Join(args[1], "PG_VERSION"), []byte("18\n"), 0o600)
				_ = os.WriteFile(filepath.Join(args[1], "postgresql.auto.conf"), []byte("# gerado\n"), 0o600)
			}
		}}
		plan, err := PrepareRestore(context.Background(), r, cfg, RestoreOptions{Target: target}, log)
		if err != nil {
			t.Fatal(err)
		}
		cmds := r.commands()
		if plan.Backup.Name != "base_000000010000000000000003" || plan.Timeline != 1 || plan.LatestTimeline != 1 ||
			cmds[len(cmds)-1] != "backup-fetch "+cfg.PGData+" base_000000010000000000000003" {
			t.Fatalf("restauração de %+v: %q", plan, cmds)
		}
		if _, err := os.Stat(filepath.Join(cfg.PGData, "recovery.signal")); err != nil {
			t.Error("recovery.signal ausente")
		}
		auto, _ := os.ReadFile(filepath.Join(cfg.PGData, "postgresql.auto.conf"))
		if !strings.Contains(string(auto), "# gerado\n") || !strings.Contains(string(auto), "recovery_target_time = '2026-11-22 12:30:00.123456+00'") ||
			!strings.Contains(string(auto), "recovery_target_action = 'promote'") ||
			!strings.Contains(string(auto), "recovery_target_timeline = '1'") {
			t.Errorf("postgresql.auto.conf:\n%s", auto)
		}
		if st, _ := os.Stat(cfg.PGData); st.Mode().Perm() != 0o700 {
			t.Errorf("permissão do diretório de dados %v", st.Mode().Perm())
		}

		// O diretório agora tem conteúdo: uma segunda restauração é recusada.
		r.calls = nil
		if _, err := PrepareRestore(context.Background(), r, cfg, RestoreOptions{Target: target}, log); err == nil ||
			!strings.Contains(err.Error(), "não está vazio") || len(r.calls) != 0 {
			t.Errorf("restauração sobre dados existentes: %v %q", err, r.commands())
		}
	})

	t.Run("fim do WAL", func(t *testing.T) {
		cfg := newCfg(t)
		r := &fakeRunner{list: strings.ReplaceAll(list, "FP", cfg.KeyFingerprint())}
		if _, err := PrepareRestore(context.Background(), r, cfg, RestoreOptions{Latest: true}, log); err != nil {
			t.Fatal(err)
		}
		auto, _ := os.ReadFile(filepath.Join(cfg.PGData, "postgresql.auto.conf"))
		if strings.Contains(string(auto), "recovery_target_time =") || !strings.Contains(string(auto), "recovery_target_timeline = '1'") {
			t.Errorf("sem instante, só a linha do tempo deveria ser gravada:\n%s", auto)
		}
	})

	t.Run("chave diferente", func(t *testing.T) {
		cfg := newCfg(t)
		r := &fakeRunner{list: strings.ReplaceAll(list, "FP", "0011223344556677")}
		_, err := PrepareRestore(context.Background(), r, cfg, RestoreOptions{Target: target}, log)
		if err == nil || !strings.Contains(err.Error(), "chave de cifragem do backup diferente") || len(r.calls) != 1 {
			t.Fatalf("chave diferente: %v %q", err, r.commands())
		}
	})

	t.Run("extração falha e esvazia o diretório", func(t *testing.T) {
		cfg := newCfg(t)
		r := &fakeRunner{list: strings.ReplaceAll(list, "FP", ""), fail: map[string]error{"backup-fetch": errors.New("decrypt")},
			onCall: func(args []string) {
				if args[0] == "backup-fetch" {
					_ = os.MkdirAll(filepath.Join(args[1], "base"), 0o700)
				}
			}}
		_, err := PrepareRestore(context.Background(), r, cfg, RestoreOptions{Target: target}, log)
		if err == nil || !strings.Contains(err.Error(), "diretório de dados foi esvaziado") {
			t.Fatalf("falha da extração: %v", err)
		}
		if entries, _ := os.ReadDir(cfg.PGData); len(entries) != 0 {
			t.Errorf("diretório de dados com %d entradas depois da falha", len(entries))
		}
	})

	t.Run("escolhas inválidas", func(t *testing.T) {
		cfg := newCfg(t)
		r := &fakeRunner{list: strings.ReplaceAll(list, "FP", cfg.KeyFingerprint())}
		for name, opts := range map[string]RestoreOptions{
			"sem ponto":             {},
			"ponto duplo":           {Latest: true, Target: target},
			"backup inexistente":    {Target: target, BackupName: "base_x"},
			"backup depois do alvo": {Target: time.Date(2026, 11, 22, 11, 0, 0, 0, time.UTC), BackupName: "base_000000010000000000000003"},
			"alvo antes de todos":   {Target: time.Date(2026, 11, 22, 11, 0, 0, 0, time.UTC)},
			"linha inexistente":     {Target: target, Timeline: 2},
		} {
			if _, err := PrepareRestore(context.Background(), r, cfg, opts, log); err == nil {
				t.Errorf("%s: aceito", name)
			}
		}
	})
}

func TestFetchExitCode(t *testing.T) {
	exitWith := func(code int) error {
		err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
		if err == nil {
			t.Fatalf("sh saiu com 0")
		}
		return err
	}
	cases := []struct {
		err  error
		want int
	}{
		{nil, FetchOK},
		{exitWith(74), FetchNotFound},
		{exitWith(1), FetchFatal},
		{errors.New("sem executável"), FetchFatal},
	}
	for _, tc := range cases {
		if got := FetchExitCode(tc.err); got != tc.want {
			t.Errorf("FetchExitCode(%v) = %d; esperado %d", tc.err, got, tc.want)
		}
	}
	var stderr bytes.Buffer
	r := &fakeRunner{fail: map[string]error{"wal-fetch": errors.New("decrypt")}}
	if code := WALFetch(context.Background(), r, "000000010000000000000003", "pg_wal/RECOVERYXLOG", &stderr); code != FetchFatal ||
		!strings.Contains(stderr.String(), "a recuperação é interrompida") {
		t.Errorf("WALFetch = %d, %q", code, stderr.String())
	}
}

func TestParseTarget(t *testing.T) {
	got, err := ParseTarget("2026-11-22T07:15:00.5-03:00")
	if err != nil || !got.Equal(time.Date(2026, 11, 22, 10, 15, 0, 500000000, time.UTC)) || got.Location() != time.UTC {
		t.Errorf("ParseTarget = %v, %v", got, err)
	}
	if _, err := ParseTarget("2026-11-22 10:15"); err == nil {
		t.Error("instante sem fuso aceito")
	}
}

func TestFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	unlock, err := FileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FileLock(path); err == nil {
		t.Error("segundo bloqueio obtido")
	}
	unlock()
	unlock2, err := FileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	unlock2()
}
