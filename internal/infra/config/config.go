// Package config carrega e valida a configuração a partir de variáveis de
// ambiente. A validação ocorre na inicialização: um executável não sobe com
// configuração incompleta ou insegura para o ambiente declarado.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Environment identifica o ambiente de execução.
type Environment string

const (
	// Development é o ambiente local com Docker Compose.
	Development Environment = "development"
	// Test é usado por testes automatizados.
	Test Environment = "test"
	// Production exige cookies seguros, HTTPS e segredos explícitos.
	Production Environment = "production"
)

// Config reúne a configuração de todos os executáveis. Cada executável chama
// o validador correspondente ao que efetivamente utiliza.
type Config struct {
	Env      Environment
	LogLevel string

	HTTPAddr        string
	MetricsAddr     string
	PublicURL       *url.URL
	AllowedOrigins  []string
	ShutdownTimeout time.Duration

	DatabaseURL         string
	DatabaseMaxConns    int32
	MigrationsDBURL     string
	RedisURL            string
	AMQPURL             string
	WorkerConcurrency   int
	WorkerPrefetch      int
	WorkerID            string
	OutboxBatchSize     int
	OutboxLease         time.Duration
	SchedulerTick       time.Duration
	ConsumerMaxAttempts int

	OIDCIssuer       string
	OIDCDiscoveryURL string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCRedirectURL  string

	SessionTTL     time.Duration
	SessionIdleTTL time.Duration
	CookieSecure   bool
	CookieName     string

	// Portal do cliente: validade do link de uso único, duração máxima e
	// inatividade da sessão e nome do cookie próprio.
	PortalLinkTTL        time.Duration
	PortalSessionTTL     time.Duration
	PortalSessionIdleTTL time.Duration
	PortalCookieName     string

	EncryptionKeys      map[string][]byte
	EncryptionActiveKey string
	// EncryptionStartupCheck define o que a API e o worker fazem, ao iniciar,
	// quando o keyring não decifra os dados cifrados em uso no banco.
	EncryptionStartupCheck KeyringCheckMode
	// EncryptionLostKeys lista os identificadores de chaves declaradas
	// perdidas (RECORVIA_ENCRYPTION_LOST_KEYS): os dados cifrados com elas
	// não são recuperáveis e deixam de impedir a inicialização.
	EncryptionLostKeys []string

	SMTPAddr     string
	SMTPFrom     string
	SMTPUsername string
	SMTPPassword string
	SMTPTLS      string
	SMTPTimeout  time.Duration

	PaySimURL        string
	PaySimPublicURL  string
	WebhookPublicURL string

	WebhookAllowedPrivate []netip.Prefix

	RateLimitPerMinute int
	// APICredentialRateLimitPerMinute limita requisições por credencial de API.
	APICredentialRateLimitPerMinute int

	// Retention são os prazos do expurgo automático executado pelo worker.
	Retention Retention
}

// Retention reúne os prazos de retenção dos dados técnicos, em dias contados
// do instante em que o registro deixou de ser necessário, e o tamanho do
// lote do expurgo. Registros financeiros e de auditoria não têm prazo: não
// são expurgados.
type Retention struct {
	// InboxDays: registros de deduplicação dos consumidores.
	InboxDays int
	// DeadLettersDays: mensagens não processadas já tratadas.
	DeadLettersDays int
	// WebhooksDays: entregas concluídas, eventos e segredos vencidos dos
	// webhooks enviados.
	WebhooksDays int
	// ProviderWebhookPayloadsDays: corpo dos webhooks recebidos dos provedores.
	ProviderWebhookPayloadsDays int
	// NotificationContentDays: conteúdo cifrado das notificações encerradas.
	NotificationContentDays int
	// NotificationsDays: notificações encerradas e o histórico de envio.
	NotificationsDays int
	// PortalLinksDays: links do portal com a validade encerrada.
	PortalLinksDays int
	// UsageEventsDays: eventos de consumo de períodos faturados.
	UsageEventsDays int
	// InvitationsDays: convites não aceitos com a validade encerrada.
	InvitationsDays int
	// BatchSize: registros removidos por transação.
	BatchSize int
}

// retentionLimit descreve os limites aceitos de uma variável de retenção.
type retentionLimit struct {
	env      string
	value    int
	min, max int
}

// Limites das variáveis de retenção. Os mínimos são os de
// internal/retention (Minimum), conferidos por teste em internal/app.
const (
	maxRetentionDays      = 3650
	minRetentionBatchSize = 10
	maxRetentionBatchSize = 5000
)

func (r Retention) limits() []retentionLimit {
	return []retentionLimit{
		{"RECORVIA_RETENTION_INBOX_DAYS", r.InboxDays, 14, maxRetentionDays},
		{"RECORVIA_RETENTION_DEAD_LETTERS_DAYS", r.DeadLettersDays, 30, maxRetentionDays},
		{"RECORVIA_RETENTION_WEBHOOKS_DAYS", r.WebhooksDays, 7, maxRetentionDays},
		{"RECORVIA_RETENTION_PROVIDER_WEBHOOK_PAYLOADS_DAYS", r.ProviderWebhookPayloadsDays, 7, maxRetentionDays},
		{"RECORVIA_RETENTION_NOTIFICATION_CONTENT_DAYS", r.NotificationContentDays, 7, maxRetentionDays},
		{"RECORVIA_RETENTION_NOTIFICATIONS_DAYS", r.NotificationsDays, 30, maxRetentionDays},
		{"RECORVIA_RETENTION_PORTAL_LINKS_DAYS", r.PortalLinksDays, 1, maxRetentionDays},
		{"RECORVIA_RETENTION_USAGE_EVENTS_DAYS", r.UsageEventsDays, 30, maxRetentionDays},
		{"RECORVIA_RETENTION_INVITATIONS_DAYS", r.InvitationsDays, 7, maxRetentionDays},
		{"RECORVIA_RETENTION_BATCH_SIZE", r.BatchSize, minRetentionBatchSize, maxRetentionBatchSize},
	}
}

// KeyringCheckMode é o modo da conferência do keyring na inicialização
// (RECORVIA_ENCRYPTION_STARTUP_CHECK).
type KeyringCheckMode string

// Modos da conferência do keyring na inicialização.
const (
	// KeyringCheckEnforce impede a inicialização com keyring incompatível.
	KeyringCheckEnforce KeyringCheckMode = "enforce"
	// KeyringCheckWarn registra o problema e inicia assim mesmo, inclusive
	// quando a conferência não pôde ser concluída (uso: emergência; para uma
	// chave perdida, declare-a em RECORVIA_ENCRYPTION_LOST_KEYS).
	KeyringCheckWarn KeyringCheckMode = "warn"
	// KeyringCheckOff não confere.
	KeyringCheckOff KeyringCheckMode = "off"
)

// Getenv abstrai a leitura do ambiente para testes.
type Getenv func(string) string

type loader struct {
	get  Getenv
	errs []error
}

func (l *loader) str(key, def string) string {
	if v := strings.TrimSpace(l.get(key)); v != "" {
		return v
	}
	return def
}

func (l *loader) intv(key string, def int) int {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		l.errs = append(l.errs, fmt.Errorf("%s: inteiro não negativo esperado", key))
		return def
	}
	return n
}

func (l *loader) dur(key string, def time.Duration) time.Duration {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		l.errs = append(l.errs, fmt.Errorf("%s: duração positiva esperada", key))
		return def
	}
	return d
}

func (l *loader) boolv(key string, def bool) bool {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: booleano esperado", key))
		return def
	}
	return b
}

// Load lê a configuração. Erros de formato são acumulados e retornados
// juntos para facilitar o diagnóstico.
func Load(get Getenv) (*Config, error) {
	if get == nil {
		get = os.Getenv
	}
	l := &loader{get: get}
	c := &Config{
		Env:                    Environment(l.str("RECORVIA_ENV", string(Development))),
		LogLevel:               l.str("RECORVIA_LOG_LEVEL", "info"),
		HTTPAddr:               l.str("RECORVIA_HTTP_ADDR", ":8081"),
		MetricsAddr:            l.str("RECORVIA_METRICS_ADDR", ":9091"),
		ShutdownTimeout:        l.dur("RECORVIA_SHUTDOWN_TIMEOUT", 20*time.Second),
		DatabaseURL:            l.str("RECORVIA_DATABASE_URL", ""),
		DatabaseMaxConns:       int32(l.intv("RECORVIA_DATABASE_MAX_CONNS", 20)),
		MigrationsDBURL:        l.str("RECORVIA_MIGRATIONS_DATABASE_URL", ""),
		RedisURL:               l.str("RECORVIA_REDIS_URL", ""),
		AMQPURL:                l.str("RECORVIA_AMQP_URL", ""),
		WorkerConcurrency:      l.intv("RECORVIA_WORKER_CONCURRENCY", 8),
		WorkerPrefetch:         l.intv("RECORVIA_WORKER_PREFETCH", 16),
		WorkerID:               l.str("RECORVIA_WORKER_ID", hostnameOr("worker")),
		OutboxBatchSize:        l.intv("RECORVIA_OUTBOX_BATCH_SIZE", 100),
		OutboxLease:            l.dur("RECORVIA_OUTBOX_LEASE", 30*time.Second),
		SchedulerTick:          l.dur("RECORVIA_SCHEDULER_TICK", 5*time.Second),
		ConsumerMaxAttempts:    l.intv("RECORVIA_CONSUMER_MAX_ATTEMPTS", 5),
		OIDCIssuer:             l.str("RECORVIA_OIDC_ISSUER", ""),
		OIDCDiscoveryURL:       l.str("RECORVIA_OIDC_DISCOVERY_URL", ""),
		OIDCClientID:           l.str("RECORVIA_OIDC_CLIENT_ID", ""),
		OIDCClientSecret:       l.str("RECORVIA_OIDC_CLIENT_SECRET", ""),
		OIDCRedirectURL:        l.str("RECORVIA_OIDC_REDIRECT_URL", ""),
		SessionTTL:             l.dur("RECORVIA_SESSION_TTL", 12*time.Hour),
		SessionIdleTTL:         l.dur("RECORVIA_SESSION_IDLE_TTL", 2*time.Hour),
		CookieSecure:           l.boolv("RECORVIA_COOKIE_SECURE", true),
		CookieName:             l.str("RECORVIA_COOKIE_NAME", "__Host-recorvia_session"),
		EncryptionActiveKey:    l.str("RECORVIA_ENCRYPTION_ACTIVE_KEY", ""),
		EncryptionStartupCheck: KeyringCheckMode(l.str("RECORVIA_ENCRYPTION_STARTUP_CHECK", string(KeyringCheckEnforce))),
		SMTPAddr:               l.str("RECORVIA_SMTP_ADDR", ""),
		SMTPFrom:               l.str("RECORVIA_SMTP_FROM", ""),
		SMTPUsername:           l.str("RECORVIA_SMTP_USERNAME", ""),
		SMTPPassword:           l.str("RECORVIA_SMTP_PASSWORD", ""),
		SMTPTLS:                l.str("RECORVIA_SMTP_TLS", "starttls"),
		SMTPTimeout:            l.dur("RECORVIA_SMTP_TIMEOUT", 15*time.Second),
		PaySimURL:              l.str("RECORVIA_PAYSIM_URL", ""),
		PaySimPublicURL:        l.str("RECORVIA_PAYSIM_PUBLIC_URL", ""),
		WebhookPublicURL:       l.str("RECORVIA_WEBHOOK_PUBLIC_URL", ""),
		RateLimitPerMinute:     l.intv("RECORVIA_RATE_LIMIT_PER_MINUTE", 600),

		PortalLinkTTL:        l.dur("RECORVIA_PORTAL_LINK_TTL", 72*time.Hour),
		PortalSessionTTL:     l.dur("RECORVIA_PORTAL_SESSION_TTL", 2*time.Hour),
		PortalSessionIdleTTL: l.dur("RECORVIA_PORTAL_SESSION_IDLE_TTL", 30*time.Minute),

		APICredentialRateLimitPerMinute: l.intv("RECORVIA_API_CREDENTIAL_RATE_LIMIT_PER_MINUTE", 600),

		Retention: Retention{
			InboxDays:                   l.intv("RECORVIA_RETENTION_INBOX_DAYS", 90),
			DeadLettersDays:             l.intv("RECORVIA_RETENTION_DEAD_LETTERS_DAYS", 180),
			WebhooksDays:                l.intv("RECORVIA_RETENTION_WEBHOOKS_DAYS", 90),
			ProviderWebhookPayloadsDays: l.intv("RECORVIA_RETENTION_PROVIDER_WEBHOOK_PAYLOADS_DAYS", 90),
			NotificationContentDays:     l.intv("RECORVIA_RETENTION_NOTIFICATION_CONTENT_DAYS", 90),
			NotificationsDays:           l.intv("RECORVIA_RETENTION_NOTIFICATIONS_DAYS", 365),
			PortalLinksDays:             l.intv("RECORVIA_RETENTION_PORTAL_LINKS_DAYS", 90),
			UsageEventsDays:             l.intv("RECORVIA_RETENTION_USAGE_EVENTS_DAYS", 400),
			InvitationsDays:             l.intv("RECORVIA_RETENTION_INVITATIONS_DAYS", 90),
			BatchSize:                   l.intv("RECORVIA_RETENTION_BATCH_SIZE", 500),
		},
	}
	// Sem configuração explícita, o cookie do portal segue o prefixo do
	// cookie de sessão (__Host- quando seguro).
	c.PortalCookieName = l.str("RECORVIA_PORTAL_COOKIE_NAME", defaultPortalCookie(c.CookieName))
	switch c.Env {
	case Development, Test, Production:
	default:
		l.errs = append(l.errs, fmt.Errorf("RECORVIA_ENV: valor desconhecido %q", c.Env))
	}
	switch c.EncryptionStartupCheck {
	case KeyringCheckEnforce, KeyringCheckWarn, KeyringCheckOff:
	default:
		l.errs = append(l.errs, fmt.Errorf("RECORVIA_ENCRYPTION_STARTUP_CHECK: use enforce, warn ou off (recebido %q)", c.EncryptionStartupCheck))
	}
	if raw := l.str("RECORVIA_PUBLIC_URL", "http://localhost:8080"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			l.errs = append(l.errs, errors.New("RECORVIA_PUBLIC_URL: URL absoluta esperada"))
		} else {
			c.PublicURL = u
		}
	}
	for _, o := range strings.Split(l.str("RECORVIA_ALLOWED_ORIGINS", ""), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.AllowedOrigins = append(c.AllowedOrigins, o)
		}
	}
	c.EncryptionKeys = parseKeys(l, l.str("RECORVIA_ENCRYPTION_KEYS", ""))
	c.EncryptionLostKeys = parseLostKeys(l, l.str("RECORVIA_ENCRYPTION_LOST_KEYS", ""), c.EncryptionKeys)
	for _, p := range strings.Split(l.str("RECORVIA_WEBHOOK_ALLOWED_PRIVATE_CIDRS", ""), ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			l.errs = append(l.errs, fmt.Errorf("RECORVIA_WEBHOOK_ALLOWED_PRIVATE_CIDRS: CIDR inválido %q", p))
			continue
		}
		c.WebhookAllowedPrivate = append(c.WebhookAllowedPrivate, prefix)
	}
	if c.WorkerConcurrency < 1 {
		c.WorkerConcurrency = 1
	}
	if c.WorkerPrefetch < c.WorkerConcurrency {
		c.WorkerPrefetch = c.WorkerConcurrency
	}
	return c, errors.Join(l.errs...)
}

func defaultPortalCookie(sessionCookie string) string {
	if strings.HasPrefix(sessionCookie, "__Host-") {
		return "__Host-recorvia_portal"
	}
	return "recorvia_portal"
}

func parseKeys(l *loader, raw string) map[string][]byte {
	keys := map[string][]byte{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		id, b64, ok := strings.Cut(item, ":")
		if !ok || id == "" {
			l.errs = append(l.errs, errors.New("RECORVIA_ENCRYPTION_KEYS: formato id:base64 esperado"))
			continue
		}
		k, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(k) != 32 {
			l.errs = append(l.errs, fmt.Errorf("RECORVIA_ENCRYPTION_KEYS: chave %q deve ter 32 bytes em base64", id))
			continue
		}
		keys[id] = k
	}
	return keys
}

// parseLostKeys lê a lista de identificadores de chaves declaradas perdidas:
// só o identificador (nunca o valor), e nenhum que esteja configurado em
// RECORVIA_ENCRYPTION_KEYS.
func parseLostKeys(l *loader, raw string, keys map[string][]byte) []string {
	var lost []string
	for _, id := range strings.Split(raw, ",") {
		id = strings.TrimSpace(id)
		switch {
		case id == "" || slices.Contains(lost, id):
			continue
		case strings.Contains(id, ":"):
			l.errs = append(l.errs, errors.New("RECORVIA_ENCRYPTION_LOST_KEYS: informe somente os identificadores, separados por vírgula"))
			continue
		}
		if _, ok := keys[id]; ok {
			l.errs = append(l.errs, fmt.Errorf("RECORVIA_ENCRYPTION_LOST_KEYS: a chave %q está em RECORVIA_ENCRYPTION_KEYS; "+
				"uma chave configurada não pode ser declarada perdida", id))
			continue
		}
		lost = append(lost, id)
	}
	slices.Sort(lost)
	return lost
}

func hostnameOr(def string) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return def
}

func require(errs []error, name, v string) []error {
	if v == "" {
		return append(errs, fmt.Errorf("%s é obrigatório", name))
	}
	return errs
}

func (c *Config) validateCommon() []error {
	var errs []error
	errs = require(errs, "RECORVIA_DATABASE_URL", c.DatabaseURL)
	if len(c.EncryptionKeys) == 0 {
		errs = append(errs, errors.New("RECORVIA_ENCRYPTION_KEYS é obrigatório"))
	} else if _, ok := c.EncryptionKeys[c.EncryptionActiveKey]; !ok {
		errs = append(errs, errors.New("RECORVIA_ENCRYPTION_ACTIVE_KEY deve referenciar uma chave configurada"))
	}
	if c.Env == Production {
		if c.PublicURL != nil && c.PublicURL.Scheme != "https" {
			errs = append(errs, errors.New("em produção RECORVIA_PUBLIC_URL deve usar https"))
		}
		if len(c.WebhookAllowedPrivate) > 0 {
			errs = append(errs, errors.New("em produção destinos privados de webhook não são permitidos"))
		}
	}
	return append(errs, c.validateRetention()...)
}

// ValidateStore valida o que os comandos que usam somente o banco e o
// keyring exigem (por exemplo, a conferência do keyring).
func (c *Config) ValidateStore() error { return errors.Join(c.validateCommon()...) }

// ValidateAPI valida o que a API utiliza.
func (c *Config) ValidateAPI() error {
	errs := c.validateCommon()
	errs = require(errs, "RECORVIA_REDIS_URL", c.RedisURL)
	errs = require(errs, "RECORVIA_OIDC_ISSUER", c.OIDCIssuer)
	errs = require(errs, "RECORVIA_OIDC_CLIENT_ID", c.OIDCClientID)
	errs = require(errs, "RECORVIA_OIDC_CLIENT_SECRET", c.OIDCClientSecret)
	errs = require(errs, "RECORVIA_OIDC_REDIRECT_URL", c.OIDCRedirectURL)
	if c.Env == Production {
		if !c.CookieSecure {
			errs = append(errs, errors.New("em produção RECORVIA_COOKIE_SECURE deve ser true"))
		}
		if !strings.HasPrefix(c.CookieName, "__Host-") {
			errs = append(errs, errors.New("em produção o cookie de sessão deve usar o prefixo __Host-"))
		}
	}
	if strings.HasPrefix(c.CookieName, "__Host-") && !c.CookieSecure {
		errs = append(errs, errors.New("cookies __Host- exigem RECORVIA_COOKIE_SECURE=true"))
	}
	if c.SessionIdleTTL > c.SessionTTL {
		errs = append(errs, errors.New("RECORVIA_SESSION_IDLE_TTL não pode exceder RECORVIA_SESSION_TTL"))
	}
	return errors.Join(append(errs, c.validatePortal()...)...)
}

// Limites aceitos para a validade do link do portal.
const (
	minPortalLinkTTL = 15 * time.Minute
	maxPortalLinkTTL = 30 * 24 * time.Hour
)

func (c *Config) validatePortal() []error {
	var errs []error
	if c.PortalLinkTTL < minPortalLinkTTL || c.PortalLinkTTL > maxPortalLinkTTL {
		errs = append(errs, errors.New("RECORVIA_PORTAL_LINK_TTL deve ficar entre 15m e 720h"))
	}
	if c.PortalSessionIdleTTL > c.PortalSessionTTL {
		errs = append(errs, errors.New("RECORVIA_PORTAL_SESSION_IDLE_TTL não pode exceder RECORVIA_PORTAL_SESSION_TTL"))
	}
	if c.PortalSessionIdleTTL <= time.Minute {
		errs = append(errs, errors.New("RECORVIA_PORTAL_SESSION_IDLE_TTL deve ser maior que 1m"))
	}
	switch {
	case c.PortalCookieName == "" || c.PortalCookieName == c.CookieName:
		errs = append(errs, errors.New("RECORVIA_PORTAL_COOKIE_NAME deve ser diferente do cookie de sessão"))
	case c.Env == Production && !strings.HasPrefix(c.PortalCookieName, "__Host-"):
		errs = append(errs, errors.New("em produção o cookie do portal deve usar o prefixo __Host-"))
	case strings.HasPrefix(c.PortalCookieName, "__Host-") && !c.CookieSecure:
		errs = append(errs, errors.New("cookies __Host- exigem RECORVIA_COOKIE_SECURE=true"))
	}
	return errs
}

// ValidateWorker valida o que o worker utiliza.
func (c *Config) ValidateWorker() error {
	errs := c.validateCommon()
	errs = require(errs, "RECORVIA_AMQP_URL", c.AMQPURL)
	errs = require(errs, "RECORVIA_SMTP_ADDR", c.SMTPAddr)
	errs = require(errs, "RECORVIA_SMTP_FROM", c.SMTPFrom)
	switch c.SMTPTLS {
	case "none", "starttls", "tls":
	default:
		errs = append(errs, errors.New("RECORVIA_SMTP_TLS deve ser none, starttls ou tls"))
	}
	if c.Env == Production && c.SMTPTLS == "none" {
		errs = append(errs, errors.New("em produção o SMTP deve usar TLS"))
	}
	return errors.Join(errs...)
}

// validateRetention confere os prazos do expurgo automático: nenhum
// executável sobe com um prazo abaixo do mínimo (que removeria dados ainda
// úteis à operação) nem acima do máximo. O expurgo é executado pelo worker; a
// API valida os mesmos valores porque monta os mesmos serviços.
func (c *Config) validateRetention() []error {
	var errs []error
	r := c.Retention
	for _, l := range r.limits() {
		if l.value < l.min || l.value > l.max {
			errs = append(errs, fmt.Errorf("%s deve ficar entre %d e %d", l.env, l.min, l.max))
		}
	}
	if r.NotificationsDays < r.NotificationContentDays {
		errs = append(errs, errors.New("RECORVIA_RETENTION_NOTIFICATIONS_DAYS não pode ser menor que RECORVIA_RETENTION_NOTIFICATION_CONTENT_DAYS"))
	}
	// A sessão do portal é conferida contra o link que a originou enquanto
	// durar: o link não pode ser expurgado antes do fim da sessão.
	if time.Duration(r.PortalLinksDays)*24*time.Hour < c.PortalSessionTTL {
		errs = append(errs, errors.New("RECORVIA_RETENTION_PORTAL_LINKS_DAYS não pode ser menor que RECORVIA_PORTAL_SESSION_TTL"))
	}
	return errs
}

// ValidateMigrations valida o executável de migrations.
func (c *Config) ValidateMigrations() error {
	return errors.Join(require(nil, "RECORVIA_MIGRATIONS_DATABASE_URL", c.MigrationsDBURL)...)
}
