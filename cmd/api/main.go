// Comando api: servidor HTTP do Recorvia (autenticação, sessões e endpoints).
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
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/app"
	"github.com/ricartefelipe/recorvia/internal/httpapi"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/identity/oidc"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/infra/redisstore"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/portal"
)

var version = "dev"

func main() {
	log := logging.New(os.Stdout, os.Getenv("RECORVIA_LOG_LEVEL"), "api")
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := app.Healthcheck(os.Getenv("RECORVIA_HTTP_ADDR"), ":8081"); err != nil {
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
	if err := run(log); err != nil {
		log.Error("api encerrada com erro", slog.String("error", err.Error()))
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
	if err := cfg.ValidateAPI(); err != nil {
		return err
	}
	shutdownTracing, err := telemetry.SetupTracing(ctx, "recorvia-api", version)
	if err != nil {
		return err
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			log.Warn("encerramento do rastreamento", slog.String("error", err.Error()))
		}
	}()
	core, err := app.Open(ctx, cfg, log, "recorvia-api")
	if err != nil {
		return err
	}
	defer core.Close()
	if err := app.VerifyKeyring(ctx, core.Store, cfg.EncryptionStartupCheck, log); err != nil {
		return err
	}
	rdb, err := redisstore.Open(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := rdb.Close(); err != nil {
			log.Warn("fechamento do redis", slog.String("error", err.Error()))
		}
	}()
	sessions := redisstore.New(rdb, core.Keyring, "web")
	idp := oidc.NewLazy(oidc.Config{Issuer: cfg.OIDCIssuer, DiscoveryURL: cfg.OIDCDiscoveryURL, ClientID: cfg.OIDCClientID,
		ClientSecret: cfg.OIDCClientSecret, RedirectURL: cfg.OIDCRedirectURL})
	policy := identity.DefaultPolicy()
	policy.Absolute, policy.Idle = cfg.SessionTTL, cfg.SessionIdleTTL
	ids := identity.NewService(idp, sessions, core.Store.Users(), core.Clock, policy)
	portalPolicy := portal.DefaultPolicy()
	portalPolicy.LinkTTL, portalPolicy.SessionTTL, portalPolicy.SessionIdle = cfg.PortalLinkTTL, cfg.PortalSessionTTL, cfg.PortalSessionIdleTTL
	if err := portalPolicy.Validate(); err != nil {
		return err
	}
	portalSessions := redisstore.NewPortalSessions(redisstore.New(rdb, core.Keyring, "portal"))
	portalSvc := portal.NewService(core.Store.Portal(), portalSessions, sessions, core.Clock, portalPolicy)
	credentials := apicredentials.NewService(core.Store.APICredentials(), sessions, core.Clock, apicredentials.DefaultPolicy())
	metrics := telemetry.NewMetrics("api")
	metrics.RegisterDBPool(app.DBPoolStats(core.DB))
	core.Resolver.Observe(metrics)
	metrics.InitChargeFailures(core.Resolver.Providers())
	metrics.InitProviderWebhooks(core.Resolver.Providers())
	// Cada dependência é verificada em paralelo e com prazo próprio: um banco
	// lento não faz o Redis aparecer como indisponível.
	deps := map[string]telemetry.DependencyCheck{"postgres": core.DB.Ping, "redis": sessions.Ping}
	ready := func(ctx context.Context) map[string]error {
		return telemetry.ProbeDependencies(ctx, 2*time.Second, deps)
	}
	// recorvia_dependency_up reflete o estado atual a cada coleta, sem
	// depender de consultas a /readyz.
	metrics.ObserveDependencies(2*time.Second, deps)
	// A API registra os pedidos de conciliação e exporta, a cada coleta, as
	// execuções em andamento: a execução que nenhum worker assumiu aparece
	// nas métricas (e no alerta) mesmo sem worker em execução.
	metrics.ObserveReconciliationRuns(2*time.Second, app.ReconciliationRunStats(core.Store, log))

	_, router := httpapi.NewRouter(httpapi.Deps{
		Log: log, Config: cfg, Clock: core.Clock, Metrics: metrics, Store: core.Store, Sessions: sessions, Identity: ids,
		Tenancy: core.Tenancy, Platform: core.Platform, Customers: core.Customers, Catalog: core.Catalog, Subscriptions: core.Subscriptions,
		Billing: core.Billing, Changes: core.Changes, CreditNotes: core.CreditNotes, Coupons: core.Coupons, Terms: core.Terms,
		Metering: core.Metering, Payments: core.Payments, Reporting: core.Reporting, Notifications: core.Notifications,
		Collections: core.Collections, Webhooks: core.Webhooks, Portal: portalSvc, Credentials: credentials,
		Providers: core.Resolver.Providers(),
		Ready:     ready,
	})
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: otelhttp.NewHandler(router, "http"), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
	metricsSrv := &http.Server{Addr: cfg.MetricsAddr, Handler: promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}),
		ReadHeaderTimeout: 5 * time.Second}

	log.Info("api ouvindo", slog.String("addr", cfg.HTTPAddr), slog.String("metrics", cfg.MetricsAddr))
	// Com SIGTERM, as requisições em andamento terminam dentro de
	// RECORVIA_SHUTDOWN_TIMEOUT (app.Serve).
	return app.Serve(ctx, log, "api", cfg.ShutdownTimeout, app.HTTPServer{Server: srv}, app.HTTPServer{Server: metricsSrv})
}
