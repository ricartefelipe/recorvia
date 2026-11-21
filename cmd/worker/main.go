// Comando worker: eventos, agendamentos, cobranças e integrações assíncronas.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
	_ "time/tzdata"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ricartefelipe/recorvia/internal/app"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
)

var version = "dev"

func main() {
	log := logging.New(os.Stdout, os.Getenv("RECORVIA_LOG_LEVEL"), "worker")
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := app.Healthcheck(os.Getenv("RECORVIA_METRICS_ADDR"), ":9091"); err != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == app.KeyringCheckCommand {
		// Confere o keyring contra o banco e termina, sem iniciar o serviço.
		if err := app.RunKeyringCheck(context.Background(), os.Getenv, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "erro:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == app.ReconcilePendingCommand {
		// Executa uma vez as conciliações registradas e termina, sem
		// agendador, consumidores nem publicador da outbox (recuperação de
		// desastre: a janela perdida é conciliada antes de religar o worker).
		ctx, stop := app.SignalContext(context.Background())
		err := app.RunReconcilePending(ctx, os.Getenv, os.Stdout)
		stop()
		if err != nil {
			fmt.Fprintln(os.Stderr, "erro:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(log); err != nil {
		log.Error("worker encerrado com erro", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := app.SignalContext(context.Background())
	defer stop()
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if err := cfg.ValidateWorker(); err != nil {
		return err
	}
	shutdownTracing, err := telemetry.SetupTracing(ctx, "recorvia-worker", version)
	if err != nil {
		return err
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			log.Warn("encerramento do rastreamento", slog.String("error", err.Error()))
		}
	}()
	core, err := app.Open(ctx, cfg, log, "recorvia-worker")
	if err != nil {
		return err
	}
	defer core.Close()
	if err := app.VerifyKeyring(ctx, core.Store, cfg.EncryptionStartupCheck, log); err != nil {
		return err
	}
	metrics := telemetry.NewMetrics("worker")
	metrics.RegisterDBPool(app.DBPoolStats(core.DB))
	core.Resolver.Observe(metrics)
	metrics.InitChargeFailures(core.Resolver.Providers())
	conn := rabbit.NewConn(cfg.AMQPURL, log)
	w := &app.Worker{Core: core, Metrics: metrics, Conn: conn}

	// Liveness só do processo; readiness com banco e broker, não pronta
	// durante o encerramento (app.WorkerHealthMux). O servidor fica no ar até
	// o worker terminar a drenagem (app.RunWorker).
	mux := app.WorkerHealthMux(promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}),
		app.WorkerProbes{Database: core.DB.Ping, Broker: conn.Connected, ShuttingDown: func() bool { return ctx.Err() != nil }})
	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return app.RunWorker(ctx, log, w.Run, app.HTTPServer{Server: srv})
}
