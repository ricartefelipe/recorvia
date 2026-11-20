// Comando worker: eventos, agendamentos, cobranças e integrações assíncronas.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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
	if err := run(log); err != nil {
		log.Error("worker encerrado com erro", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	metrics := telemetry.NewMetrics("worker")
	metrics.RegisterDBPool(app.DBPoolStats(core.DB))
	core.Resolver.Observe(metrics)
	metrics.InitChargeFailures(core.Resolver.Providers())
	conn := rabbit.NewConn(cfg.AMQPURL, log)
	w := &app.Worker{Core: core, Metrics: metrics, Conn: conn}

	// Liveness só do processo; readiness com banco e broker (app.WorkerHealthMux).
	mux := app.WorkerHealthMux(promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}),
		app.WorkerProbes{Database: core.DB.Ping, Broker: conn.Connected})
	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	runErr := w.Run(ctx)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutErr := srv.Shutdown(sctx)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.Join(runErr, err)
		}
	default:
	}
	return errors.Join(runErr, shutErr)
}
