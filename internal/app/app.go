// Package app é a raiz de composição: cria infraestrutura e serviços a
// partir da configuração, com dependências explícitas por construtor. É
// usado por cmd/api, cmd/worker e cmd/seed.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/exaring/otelpgx"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/infra/secrets"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways/asaas"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways/simulator"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/reporting"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

type httpClient = http.Client

// Core reúne banco, store e serviços de domínio.
type Core struct {
	Config        *config.Config
	Log           *slog.Logger
	Clock         clock.Clock
	DB            *postgres.DB
	Keyring       *secrets.Keyring
	Store         *pgstore.Store
	Resolver      *gateways.Resolver
	Tenancy       *tenancy.Service
	Platform      *platformbilling.Service
	Customers     *customers.Service
	Catalog       *catalog.Service
	Subscriptions *subscriptions.Service
	Engine        *billing.Engine
	Billing       *billing.Service
	Payments      *payments.Service
	Reporting     *reporting.Service
}

// NewResolver registra os provedores de pagamento disponíveis.
func NewResolver(cfg *config.Config) *gateways.Resolver {
	var factories []gateways.Factory
	if cfg.PaySimURL != "" {
		base := cfg.PaySimURL
		factories = append(factories, gateways.Factory{Provider: "simulator", Verifier: simulator.Verifier{},
			Gateway: func(a payments.ProviderAccount, hc *httpClient) (payments.Gateway, error) {
				return simulator.New(base, a.APIKey, hc), nil
			}})
	}
	webhookEmail := cfg.SMTPFrom
	factories = append(factories, gateways.Factory{Provider: "asaas", Verifier: asaas.Verifier{},
		Gateway: func(a payments.ProviderAccount, hc *httpClient) (payments.Gateway, error) {
			base, err := asaas.BaseURL(a.Environment)
			if err != nil {
				return nil, err
			}
			return asaas.New(base, a.APIKey, webhookEmail, hc), nil
		}})
	return gateways.NewResolver(20*time.Second, factories...)
}

// Open conecta ao banco e monta os serviços.
func Open(ctx context.Context, cfg *config.Config, log *slog.Logger, appName string) (*Core, error) {
	keyring, err := secrets.NewKeyring(cfg.EncryptionKeys, cfg.EncryptionActiveKey)
	if err != nil {
		return nil, err
	}
	db, err := postgres.Open(ctx, cfg.DatabaseURL, postgres.Options{MaxConns: cfg.DatabaseMaxConns, ApplicationName: appName,
		Tracer: otelpgx.NewTracer()})
	if err != nil {
		return nil, err
	}
	c := clock.System{}
	store := pgstore.New(db, keyring, c, telemetry.InjectHeaders)
	resolver := NewResolver(cfg)
	engine := billing.NewEngine(store.Billing(), c)
	webhookBase := cfg.WebhookPublicURL
	if webhookBase == "" && cfg.PublicURL != nil {
		webhookBase = strings.TrimRight(cfg.PublicURL.String(), "/") + "/webhooks/providers"
	}
	return &Core{
		Config: cfg, Log: log, Clock: c, DB: db, Keyring: keyring, Store: store, Resolver: resolver,
		Tenancy:       tenancy.NewService(store.Tenancy(), c),
		Platform:      platformbilling.NewService(store.Platform(), c),
		Customers:     customers.NewService(store.Customers(), c),
		Catalog:       catalog.NewService(store.Catalog(), c),
		Subscriptions: subscriptions.NewService(store.Subscriptions(), c),
		Engine:        engine,
		Billing:       billing.NewService(store.Billing(), engine, c),
		Payments: payments.NewService(store.Payments(), resolver, c, payments.Config{WebhookBaseURL: webhookBase,
			CallTimeout: 15 * time.Second, Lease: time.Minute, Log: log}),
		Reporting: reporting.NewService(store.Reporting(), c),
	}, nil
}

// Close libera recursos.
func (c *Core) Close() { c.DB.Close() }

// ErrShutdown sinaliza encerramento solicitado.
var ErrShutdown = errors.New("encerramento solicitado")
