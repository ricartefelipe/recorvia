// Comando pgbackup (recorvia-pgbackup na imagem deploy/postgres): backup
// contínuo do PostgreSQL com o WAL-G (ADR-087).
//
//	recorvia-pgbackup check-config            confere destino e chave de cifragem (entrypoint do servidor)
//	recorvia-pgbackup wal-push <caminho>      archive_command
//	recorvia-pgbackup wal-fetch <nome> <dest> restore_command
//	recorvia-pgbackup serve                   agente: backup base agendado, retenção e métricas
//	recorvia-pgbackup backup                  backup base imediato e retenção
//	recorvia-pgbackup list                    backups base no armazenamento
//	recorvia-pgbackup restore (--target-time <RFC 3339> | --latest) [--backup <nome>]
//	                                          prepara um servidor novo para recuperar até o instante
//	recorvia-pgbackup put-object <nome>       envia a entrada padrão, compactada e cifrada, para extra/<nome> no destino
//	recorvia-pgbackup get-object <nome>       escreve extra/<nome>, decifrado, na saída padrão
//	recorvia-pgbackup healthcheck             saúde do agente (healthcheck do container)
//	recorvia-pgbackup version
//
// Configuração: variáveis RECORVIA_BACKUP_* (internal/infra/pgbackup) e as
// variáveis do libpq (PGHOST, PGUSER...) para o acesso ao servidor.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/infra/pgbackup"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

var version = "dev"

// walgBin é o executável do WAL-G na imagem (variável para os testes).
var walgBin = "/usr/local/bin/wal-g"

// lockFile impede dois backups base simultâneos no mesmo container.
const lockFile = "/tmp/recorvia-pgbackup.lock"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "uso: recorvia-pgbackup check-config|wal-push|wal-fetch|serve|backup|list|restore|put-object|get-object|healthcheck|version")
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	case "healthcheck":
		return healthcheck(getenv, stderr)
	}
	cfg, err := pgbackup.Load(getenv, os.ReadFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recorvia-pgbackup: configuração do backup inválida:\n%v\n", err)
		// O restore_command precisa de um código fatal: sem configuração, a
		// recuperação não pode concluir que o WAL acabou.
		if cmd == "wal-fetch" {
			return pgbackup.FetchFatal
		}
		return 1
	}
	runner := pgbackup.ExecRunner{Bin: walgBin, Env: cfg.WalgEnv(os.Environ()), Stderr: stderr}
	switch cmd {
	case "check-config":
		_, _ = fmt.Fprintf(stdout, "configuração do backup válida: destino %s, chave de cifragem %s\n", cfg.S3Prefix, cfg.KeyFingerprint())
		return 0
	case "wal-push":
		if len(rest) != 1 {
			_, _ = fmt.Fprintln(stderr, "uso: recorvia-pgbackup wal-push <caminho do segmento>")
			return 1
		}
		if err := runner.Run(ctx, stdout, "wal-push", rest[0]); err != nil {
			_, _ = fmt.Fprintf(stderr, "recorvia-pgbackup: arquivamento de %s falhou: %v\n", rest[0], err)
			return 1
		}
		return 0
	case "wal-fetch":
		if len(rest) != 2 {
			_, _ = fmt.Fprintln(stderr, "uso: recorvia-pgbackup wal-fetch <nome> <destino>")
			return pgbackup.FetchFatal
		}
		return pgbackup.WALFetch(ctx, runner, rest[0], rest[1], stderr)
	}

	// O log vai para a saída de erro nos comandos que escrevem dados na saída
	// padrão (list, get-object, restore); o agente e o backup avulso registram
	// na saída padrão, como os demais processos.
	logOut := stderr
	if cmd == "serve" || cmd == "backup" {
		logOut = stdout
	}
	log := logging.New(logOut, getenv("RECORVIA_LOG_LEVEL"), "pgbackup")
	switch cmd {
	case "serve":
		err = serve(ctx, cfg, runner, log)
	case "backup":
		err = backupOnce(ctx, cfg, runner, log)
	case "list":
		err = list(ctx, cfg, runner, stdout)
	case "restore":
		err = restore(ctx, cfg, runner, rest, log, stdout)
	case "put-object", "get-object":
		err = object(ctx, runner, cmd, rest, os.Stdin, stdout)
	default:
		_, _ = fmt.Fprintf(stderr, "subcomando desconhecido: %s\n", cmd)
		return 2
	}
	if err != nil {
		log.Error("recorvia-pgbackup "+cmd+" falhou", slog.String("error", err.Error()))
		return 1
	}
	return 0
}

func newAgent(cfg pgbackup.Config, runner pgbackup.Runner, server pgbackup.Server, metrics *telemetry.BackupMetrics, log *slog.Logger) *pgbackup.Agent {
	return &pgbackup.Agent{Config: cfg, Runner: runner, Server: server, Clock: clock.System{}, Metrics: metrics, Log: log,
		Lock:  func() (func(), error) { return pgbackup.FileLock(lockFile) },
		Sleep: pgbackup.SleepContext, RefreshEvery: 5 * time.Minute}
}

// serve executa o agente: métricas em RECORVIA_BACKUP_METRICS_ADDR e o
// agendamento dos backups base até o sinal de término.
func serve(ctx context.Context, cfg pgbackup.Config, runner pgbackup.Runner, log *slog.Logger) error {
	if cfg.PGData == "" {
		return errors.New("PGDATA não definido: o agente lê o diretório de dados do servidor (volume compartilhado)")
	}
	server, err := pgbackup.OpenServer(ctx, cfg)
	if err != nil {
		return err
	}
	defer server.Close()
	metrics := telemetry.NewBackupMetrics()
	metrics.ObserveArchiver(5*time.Second, server.Archiver)

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	log.Info("agente de backup iniciado", slog.String("destination", cfg.S3Prefix), slog.String("key_fingerprint", cfg.KeyFingerprint()),
		slog.Duration("base_interval", cfg.BaseInterval), slog.Int("retain_full", cfg.RetainFull), slog.Int("retain_days", cfg.RetainDays),
		slog.String("metrics_addr", cfg.MetricsAddr))

	agentCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- newAgent(cfg, runner, server, metrics, log).Run(agentCtx) }()
	select {
	case err = <-errc:
		cancel()
		if derr := <-done; derr != nil {
			log.Error("agendamento encerrado com erro", slog.String("error", derr.Error()))
		}
	case err = <-done:
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_ = srv.Shutdown(shutdownCtx)
	log.Info("agente de backup encerrado")
	return err
}

// backupOnce faz um backup base imediato (por exemplo, depois de uma
// restauração ou de uma troca da chave de cifragem) e aplica a retenção.
func backupOnce(ctx context.Context, cfg pgbackup.Config, runner pgbackup.Runner, log *slog.Logger) error {
	if cfg.PGData == "" {
		return errors.New("PGDATA não definido")
	}
	agent := newAgent(cfg, runner, nil, telemetry.NewBackupMetrics(), log)
	return agent.BackupNow(ctx, "pedido manual")
}

func list(ctx context.Context, cfg pgbackup.Config, runner pgbackup.Runner, stdout io.Writer) error {
	backups, err := pgbackup.ListBackups(ctx, runner)
	if err != nil {
		return err
	}
	if len(backups) == 0 {
		_, _ = fmt.Fprintln(stdout, "nenhum backup base no armazenamento")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "BACKUP\tINÍCIO (UTC)\tFIM (UTC)\tLINHA DO TEMPO\tTAMANHO\tCHAVE")
	for _, b := range backups {
		key := "esta chave"
		switch fp := b.Fingerprint(); {
		case fp == "":
			key = "não registrada"
		case fp != cfg.KeyFingerprint():
			key = "outra (" + fp + ")"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n", b.Name, b.StartTime.UTC().Format(time.RFC3339), b.FinishTime.UTC().Format(time.RFC3339),
			b.Timeline(), b.CompressedSize, key)
	}
	return tw.Flush()
}

func restore(ctx context.Context, cfg pgbackup.Config, runner pgbackup.Runner, args []string, log *slog.Logger, stdout io.Writer) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	target := fs.String("target-time", "", "instante da recuperação (RFC 3339)")
	latest := fs.Bool("latest", false, "recuperar até o fim do WAL arquivado")
	name := fs.String("backup", "", "backup base a usar (padrão: o mais recente do histórico da linha do tempo terminado antes do instante)")
	timeline := fs.Uint("timeline", 0, "linha do tempo seguida na recuperação (padrão: a mais recente do arquivo)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *timeline > math.MaxUint32 {
		return fmt.Errorf("linha do tempo inválida: %d", *timeline)
	}
	opts := pgbackup.RestoreOptions{Latest: *latest, BackupName: *name, Timeline: uint32(*timeline)}
	if *target != "" {
		t, err := pgbackup.ParseTarget(*target)
		if err != nil {
			return err
		}
		opts.Target = t
	}
	plan, err := pgbackup.PrepareRestore(ctx, runner, cfg, opts, log)
	if err != nil {
		return err
	}
	point := "fim do WAL arquivado"
	if !opts.Target.IsZero() {
		point = opts.Target.Format(time.RFC3339Nano)
	}
	_, _ = fmt.Fprintf(stdout, "backup base %s (fim %s) extraído em %s; ao iniciar, o servidor recupera até %s na linha do tempo %d e é promovido\n",
		plan.Backup.Name, plan.Backup.FinishTime.UTC().Format(time.RFC3339), cfg.PGData, point, plan.Timeline)
	if plan.Timeline < plan.LatestTimeline {
		_, _ = fmt.Fprintf(stdout, "atenção: o arquivo tem linhas do tempo mais novas (até a %d); a recuperação segue a %d, como pedido\n",
			plan.LatestTimeline, plan.Timeline)
	} else if plan.LatestTimeline > 1 {
		_, _ = fmt.Fprintf(stdout, "atenção: o arquivo tem %d linhas do tempo; a recuperação segue a mais recente (%d). Para seguir outra, use --timeline\n",
			plan.LatestTimeline, plan.Timeline)
	}
	return nil
}

// object envia ou lê um arquivo extra do destino do backup, com a mesma
// compactação e cifragem do backup (wal-g st put/get). Usado para a
// exportação do realm do Keycloak (deploy/backup/keycloak-export.sh).
func object(ctx context.Context, runner pgbackup.ExecRunner, cmd string, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 1 || !pgbackup.ValidObjectName(args[0]) {
		return fmt.Errorf("uso: recorvia-pgbackup %s <nome> (letras, dígitos, ., _, - e /)", cmd)
	}
	if cmd == "put-object" {
		// A entrada inteira é lida antes do envio: uma exportação que falhou
		// no meio não chega ao destino como arquivo vazio ou truncado.
		data, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		return pgbackup.PutObject(ctx, runner, args[0], data)
	}
	return pgbackup.GetObject(ctx, runner, args[0], stdout)
}

// healthcheck confere se o agente responde em RECORVIA_BACKUP_METRICS_ADDR.
func healthcheck(getenv func(string) string, stderr io.Writer) int {
	addr := getenv(pgbackup.EnvMetricsAddr)
	if addr == "" {
		addr = ":9187"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "endereço inválido %q: %v\n", addr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/healthz", nil)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(stderr, "healthz respondeu %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
