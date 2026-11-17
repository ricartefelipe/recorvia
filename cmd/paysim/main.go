// Comando paysim: provedor de pagamentos simulado para o ambiente local.
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

	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/paysim"
)

func main() {
	log := logging.New(os.Stdout, os.Getenv("PAYSIM_LOG_LEVEL"), "paysim")
	if err := run(log); err != nil {
		log.Error("simulador encerrado com erro", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run(log *slog.Logger) error {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		return healthcheck(envOr("PAYSIM_ADDR", ":8090"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := paysim.Load(os.Getenv("PAYSIM_STATE_FILE"))
	if err != nil {
		return err
	}
	sim := paysim.New(st, envOr("PAYSIM_PUBLIC_URL", "http://localhost:8090"), func() time.Time { return time.Now().UTC() }, log)
	if v := os.Getenv("PAYSIM_TIMEOUT_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return err
		}
		sim.TimeoutDelay = d
	}
	srv := &http.Server{Addr: envOr("PAYSIM_ADDR", ":8090"), Handler: sim.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	go sim.RunDispatcher(ctx, 500*time.Millisecond)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("simulador ouvindo", slog.String("addr", srv.Addr))
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func healthcheck(addr string) error {
	host := addr
	if len(host) > 0 && host[0] == ':' {
		host = "127.0.0.1" + host
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	if err := resp.Body.Close(); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New("simulador indisponível")
	}
	return nil
}
