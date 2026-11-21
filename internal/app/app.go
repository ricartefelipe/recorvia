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
	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/mailer"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/infra/safehttp"
	"github.com/ricartefelipe/recorvia/internal/infra/secrets"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways/asaas"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways/simulator"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/reporting"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
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
	Changes       *billing.ChangeService
	CreditNotes   *billing.CreditNoteService
	Corrector     *billing.UsageCorrector
	Coupons       *catalog.CouponService
	Terms         *subscriptions.TermsService
	Metering      *metering.Service
	Payments      *payments.Service
	Reporting     *reporting.Service
	Notifications *notifications.Service
	Collections   *collections.Service
	Webhooks      *webhooks.Service
}

// mailSender adapta o cliente SMTP à porta de envio das notificações.
type mailSender struct{ m *mailer.Mailer }

func (s mailSender) Send(ctx context.Context, e notifications.Email) (notifications.Receipt, error) {
	r, err := s.m.Send(ctx, mailer.Message{ID: e.ID.String(), FromName: e.FromName, To: e.To, Subject: e.Subject, Text: e.Text, HTML: e.HTML,
		Date: e.Date})
	return notifications.Receipt{MessageID: r.MessageID, Refused: r.Refused}, err
}

// webhookTransport adapta o cliente HTTP protegido contra SSRF à porta de
// entrega dos webhooks.
type webhookTransport struct{ c *safehttp.Client }

func (t webhookTransport) Post(ctx context.Context, r webhooks.Request) (int, error) {
	return t.c.Post(ctx, r.URL, r.Header, r.Body)
}

// Tempos das integrações de saída.
const (
	webhookRequestTimeout = 10 * time.Second
	webhookLease          = time.Minute
	notificationLease     = 2 * time.Minute
)

// NewWebhookClient cria o cliente HTTP dos webhooks enviados (https
// obrigatório em produção; destinos privados apenas pelos CIDRs configurados).
func NewWebhookClient(cfg *config.Config) *safehttp.Client {
	return safehttp.New(safehttp.Options{Policy: safehttp.Policy{AllowedPrivate: cfg.WebhookAllowedPrivate},
		RequireHTTPS: cfg.Env == config.Production, Timeout: webhookRequestTimeout})
}

// newMailSender cria o envio SMTP quando configurado (o worker o exige; a
// API não envia e-mails).
func newMailSender(cfg *config.Config) (notifications.Sender, error) {
	if cfg.SMTPAddr == "" || cfg.SMTPFrom == "" {
		return nil, nil
	}
	m, err := mailer.New(mailer.Config{Addr: cfg.SMTPAddr, From: cfg.SMTPFrom, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword,
		TLS: cfg.SMTPTLS, Timeout: cfg.SMTPTimeout})
	if err != nil {
		return nil, err
	}
	return mailSender{m: m}, nil
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
	core, err := Assemble(cfg, log, db, keyring, clock.System{})
	if err != nil {
		db.Close()
		return nil, err
	}
	return core, nil
}

// Assemble monta o store e os serviços sobre banco, keyring e relógio já
// criados. Open a usa com o relógio do sistema; os testes de ponta a ponta,
// com relógio manual.
func Assemble(cfg *config.Config, log *slog.Logger, db *postgres.DB, keyring *secrets.Keyring, c clock.Clock) (*Core, error) {
	store := pgstore.New(db, keyring, c, telemetry.InjectHeaders)
	sender, err := newMailSender(cfg)
	if err != nil {
		return nil, err
	}
	publicURL := ""
	if cfg.PublicURL != nil {
		publicURL = cfg.PublicURL.String()
	}
	webhookClient := NewWebhookClient(cfg)
	resolver := NewResolver(cfg)
	engine := billing.NewEngine(store.Billing(), c)
	webhookBase := cfg.WebhookPublicURL
	if webhookBase == "" && cfg.PublicURL != nil {
		webhookBase = strings.TrimRight(cfg.PublicURL.String(), "/") + "/webhooks/providers"
	}
	return &Core{
		Config: cfg, Log: log, Clock: c, DB: db, Keyring: keyring, Store: store, Resolver: resolver,
		Tenancy:       tenancy.NewService(store.Tenancy(), c).WithTokenSealer(keyring),
		Platform:      platformbilling.NewService(store.Platform(), c),
		Customers:     customers.NewService(store.Customers(), c),
		Catalog:       catalog.NewService(store.Catalog(), c),
		Subscriptions: subscriptions.NewService(store.Subscriptions(), c),
		Engine:        engine,
		Billing:       billing.NewService(store.Billing(), engine, c),
		Changes:       billing.NewChangeService(store.Billing(), c),
		CreditNotes:   billing.NewCreditNoteService(store.Billing(), c),
		Corrector:     billing.NewUsageCorrector(store.Billing(), c),
		Coupons:       catalog.NewCouponService(store.Catalog(), c),
		Terms:         subscriptions.NewTermsService(store.Subscriptions(), c),
		Metering:      metering.NewService(store.Metering(), c),
		Payments: payments.NewService(store.Payments(), resolver, c, payments.Config{WebhookBaseURL: webhookBase,
			CallTimeout: 15 * time.Second, Lease: time.Minute, Log: log}),
		Reporting: reporting.NewService(store.Reporting(), c),
		Notifications: notifications.NewService(store.Notifications(), sender, keyring, c, notifications.Config{Lease: notificationLease,
			SendTimeout: cfg.SMTPTimeout, PublicURL: publicURL, IsPermanent: mailer.IsPermanent, Log: log}),
		Collections: collections.NewService(store.Collections(), c, log),
		Webhooks: webhooks.NewService(store.Webhooks(), webhookTransport{c: webhookClient}, webhookClient, c, webhooks.Config{
			RequireHTTPS: cfg.Env == config.Production, Lease: webhookLease, Timeout: webhookRequestTimeout + 5*time.Second,
			IsPermanent: func(err error) bool { return errors.Is(err, safehttp.ErrBlocked) }, Log: log}),
	}, nil
}

// Close libera recursos.
func (c *Core) Close() { c.DB.Close() }

// DBPoolStats adapta as estatísticas do pool pgx às métricas do pool de
// conexões (lidas a cada coleta do Prometheus).
func DBPoolStats(db *postgres.DB) func() telemetry.DBPoolStats {
	return func() telemetry.DBPoolStats {
		s := db.Pool().Stat()
		return telemetry.DBPoolStats{Acquired: s.AcquiredConns(), Idle: s.IdleConns(), Constructing: s.ConstructingConns(), Max: s.MaxConns(),
			Acquires: s.AcquireCount(), EmptyAcquires: s.EmptyAcquireCount(), CanceledAcquires: s.CanceledAcquireCount(),
			EmptyAcquireWait: s.EmptyAcquireWaitTime()}
	}
}

func reconciliationRunStats(s pgstore.ReconciliationRunStats) telemetry.ReconciliationRunStats {
	return telemetry.ReconciliationRunStats{Queued: s.Queued, Executing: s.Executing, Stalled: s.Stalled,
		OldestWaitingSeconds: s.OldestWaitingSeconds}
}

// ReconciliationRunStats adapta as contagens agregadas das execuções de
// conciliação em andamento às métricas lidas a cada coleta do Prometheus
// (telemetry.Metrics.ObserveReconciliationRuns). É usada pela API: ela
// registra os pedidos de conciliação e, com as métricas, mostra a execução
// que nenhum worker assumiu mesmo quando não há worker em execução. A falha
// de leitura é registrada no log e devolvida (as séries mantêm o último
// valor).
func ReconciliationRunStats(store *pgstore.Store, log *slog.Logger) func(ctx context.Context) (telemetry.ReconciliationRunStats, error) {
	return func(ctx context.Context) (telemetry.ReconciliationRunStats, error) {
		s, err := store.ReconciliationRunStats(ctx)
		if err != nil {
			log.WarnContext(ctx, "leitura das execuções de conciliação em andamento falhou", slog.String("error", err.Error()))
			return telemetry.ReconciliationRunStats{}, err
		}
		return reconciliationRunStats(s), nil
	}
}

// ErrShutdown sinaliza encerramento solicitado.
var ErrShutdown = errors.New("encerramento solicitado")
