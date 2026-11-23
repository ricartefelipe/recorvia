package config

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) Getenv { return func(k string) string { return m[k] } }

var key32 = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func baseEnv() map[string]string {
	return map[string]string{
		"RECORVIA_ENV":                   "development",
		"RECORVIA_DATABASE_URL":          "postgres://app@localhost/recorvia",
		"RECORVIA_REDIS_URL":             "redis://localhost:6379/0",
		"RECORVIA_AMQP_URL":              "amqp://localhost",
		"RECORVIA_OIDC_ISSUER":           "http://localhost:8180/realms/recorvia",
		"RECORVIA_OIDC_CLIENT_ID":        "recorvia-web",
		"RECORVIA_OIDC_CLIENT_SECRET":    "s",
		"RECORVIA_OIDC_REDIRECT_URL":     "http://localhost:8080/auth/callback",
		"RECORVIA_ENCRYPTION_KEYS":       "k1:" + key32,
		"RECORVIA_ENCRYPTION_ACTIVE_KEY": "k1",
		"RECORVIA_COOKIE_SECURE":         "false",
		"RECORVIA_COOKIE_NAME":           "recorvia_session",
		"RECORVIA_SMTP_ADDR":             "localhost:1025",
		"RECORVIA_SMTP_FROM":             "cobranca@recorvia.local",
		"RECORVIA_SMTP_TLS":              "none",
	}
}

func TestLoadDefaultsAndValidate(t *testing.T) {
	c, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8081" || c.SessionTTL != 12*time.Hour || c.PublicURL.String() != "http://localhost:8080" {
		t.Fatalf("defaults inesperados: %+v", c)
	}
	if err := c.ValidateAPI(); err != nil {
		t.Fatalf("ValidateAPI: %v", err)
	}
	if err := c.ValidateWorker(); err != nil {
		t.Fatalf("ValidateWorker: %v", err)
	}
	if len(c.EncryptionKeys["k1"]) != 32 {
		t.Fatal("chave não carregada")
	}
}

func TestLoadFormatErrorsAreAccumulated(t *testing.T) {
	m := baseEnv()
	m["RECORVIA_ENV"] = "staging"
	m["RECORVIA_DATABASE_MAX_CONNS"] = "-1"
	m["RECORVIA_SESSION_TTL"] = "abc"
	m["RECORVIA_COOKIE_SECURE"] = "talvez"
	m["RECORVIA_PUBLIC_URL"] = "localhost"
	m["RECORVIA_ENCRYPTION_KEYS"] = "semdoispontos,k2:curta"
	m["RECORVIA_WEBHOOK_ALLOWED_PRIVATE_CIDRS"] = "10.0.0.0/8,xx"
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("esperava erros")
	}
	for _, frag := range []string{"RECORVIA_ENV", "MAX_CONNS", "SESSION_TTL", "COOKIE_SECURE", "PUBLIC_URL", "id:base64", "k2", "CIDR"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("erro não menciona %s: %v", frag, err)
		}
	}
}

func TestValidateAPIRequiresIdentity(t *testing.T) {
	m := baseEnv()
	delete(m, "RECORVIA_OIDC_CLIENT_SECRET")
	delete(m, "RECORVIA_REDIS_URL")
	m["RECORVIA_ENCRYPTION_ACTIVE_KEY"] = "k9"
	m["RECORVIA_SESSION_IDLE_TTL"] = "24h"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	err = c.ValidateAPI()
	for _, frag := range []string{"CLIENT_SECRET", "REDIS_URL", "ACTIVE_KEY", "IDLE_TTL"} {
		if err == nil || !strings.Contains(err.Error(), frag) {
			t.Errorf("esperava erro sobre %s: %v", frag, err)
		}
	}
}

func TestProductionSafetyRules(t *testing.T) {
	m := baseEnv()
	m["RECORVIA_ENV"] = "production"
	m["RECORVIA_PUBLIC_URL"] = "http://recorvia.example"
	m["RECORVIA_WEBHOOK_ALLOWED_PRIVATE_CIDRS"] = "10.0.0.0/8"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	err = c.ValidateAPI()
	for _, frag := range []string{"https", "COOKIE_SECURE", "__Host-", "privados"} {
		if err == nil || !strings.Contains(err.Error(), frag) {
			t.Errorf("produção deveria exigir %s: %v", frag, err)
		}
	}
	if err := c.ValidateWorker(); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Errorf("produção deveria exigir SMTP com TLS: %v", err)
	}
}

func TestHostCookieRequiresSecure(t *testing.T) {
	m := baseEnv()
	m["RECORVIA_COOKIE_NAME"] = "__Host-recorvia_session"
	c, _ := Load(env(m))
	if err := c.ValidateAPI(); err == nil || !strings.Contains(err.Error(), "__Host-") {
		t.Fatalf("esperava erro de cookie __Host-: %v", err)
	}
}

// Tenants faturados ao mesmo tempo pelo motor (ADR-088): 4 por padrão, de 1
// a 32.
func TestBillingConcurrency(t *testing.T) {
	cases := []struct {
		value   string
		want    int
		wantErr bool
	}{
		{"", 4, false},
		{"1", 1, false},
		{"32", 32, false},
		{"0", 0, true},
		{"33", 33, true},
		{"dois", 0, true},
	}
	for _, tc := range cases {
		m := baseEnv()
		if tc.value != "" {
			m["RECORVIA_BILLING_CONCURRENCY"] = tc.value
		}
		c, err := Load(env(m))
		if gotErr := err != nil && strings.Contains(err.Error(), "RECORVIA_BILLING_CONCURRENCY"); gotErr != tc.wantErr {
			t.Errorf("%q: erro %v, esperado erro=%v", tc.value, err, tc.wantErr)
		}
		if !tc.wantErr && c.BillingConcurrency != tc.want {
			t.Errorf("%q: concorrência %d, esperada %d", tc.value, c.BillingConcurrency, tc.want)
		}
	}
}

// Webhooks de provedores por IP de origem (ADR-088): 1200 por minuto por
// padrão; zero ou negativo é recusado.
func TestWebhookRateLimit(t *testing.T) {
	m := baseEnv()
	c, err := Load(env(m))
	if err != nil || c.WebhookRateLimitPerMinute != 1200 {
		t.Fatalf("padrão: %d, %v", c.WebhookRateLimitPerMinute, err)
	}
	m["RECORVIA_WEBHOOK_RATE_LIMIT_PER_MINUTE"] = "6000"
	if c, err = Load(env(m)); err != nil || c.WebhookRateLimitPerMinute != 6000 {
		t.Fatalf("configurado: %d, %v", c.WebhookRateLimitPerMinute, err)
	}
	m["RECORVIA_WEBHOOK_RATE_LIMIT_PER_MINUTE"] = "0"
	if _, err = Load(env(m)); err == nil || !strings.Contains(err.Error(), "RECORVIA_WEBHOOK_RATE_LIMIT_PER_MINUTE") {
		t.Fatalf("zero deveria ser recusado: %v", err)
	}
}

func TestWorkerAndMigrationsValidation(t *testing.T) {
	m := baseEnv()
	m["RECORVIA_SMTP_TLS"] = "ssl"
	m["RECORVIA_WORKER_CONCURRENCY"] = "0"
	m["RECORVIA_WORKER_PREFETCH"] = "0"
	delete(m, "RECORVIA_AMQP_URL")
	c, _ := Load(env(m))
	if c.WorkerConcurrency != 1 || c.WorkerPrefetch != 1 {
		t.Fatalf("concorrência mínima: %d/%d", c.WorkerConcurrency, c.WorkerPrefetch)
	}
	if err := c.ValidateWorker(); err == nil || !strings.Contains(err.Error(), "AMQP") || !strings.Contains(err.Error(), "SMTP_TLS") {
		t.Fatalf("ValidateWorker: %v", err)
	}
	if err := c.ValidateMigrations(); err == nil {
		t.Fatal("migrations exigem URL")
	}
	m["RECORVIA_MIGRATIONS_DATABASE_URL"] = "postgres://owner@localhost/recorvia"
	m["RECORVIA_ALLOWED_ORIGINS"] = "http://localhost:5173, http://localhost:8080"
	c, _ = Load(env(m))
	if err := c.ValidateMigrations(); err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedOrigins) != 2 {
		t.Fatalf("origens: %v", c.AllowedOrigins)
	}
	if err := (&Config{}).ValidateMigrations(); err == nil {
		t.Fatal("config vazia deve falhar")
	}
	if _, err := Load(nil); err != nil && !strings.Contains(err.Error(), "RECORVIA") {
		t.Fatalf("Load(nil) deveria usar os.Getenv: %v", err)
	}
}

func TestSMTPTimeout(t *testing.T) {
	c, err := Load(env(baseEnv()))
	if err != nil || c.SMTPTimeout != 15*time.Second {
		t.Fatalf("tempo limite padrão do SMTP: %v %v", c.SMTPTimeout, err)
	}
	m := baseEnv()
	m["RECORVIA_SMTP_TIMEOUT"] = "5s"
	if c, err := Load(env(m)); err != nil || c.SMTPTimeout != 5*time.Second {
		t.Fatalf("tempo limite configurado: %v %v", c.SMTPTimeout, err)
	}
	m["RECORVIA_SMTP_TIMEOUT"] = "zero"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "RECORVIA_SMTP_TIMEOUT") {
		t.Fatalf("valor inválido deve falhar: %v", err)
	}
}

func TestPortalAndCredentialDefaults(t *testing.T) {
	c, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.PortalLinkTTL != 72*time.Hour || c.PortalSessionTTL != 2*time.Hour || c.PortalSessionIdleTTL != 30*time.Minute {
		t.Fatalf("validades padrão do portal: %v %v %v", c.PortalLinkTTL, c.PortalSessionTTL, c.PortalSessionIdleTTL)
	}
	if c.PortalCookieName != "recorvia_portal" || c.APICredentialRateLimitPerMinute != 600 {
		t.Fatalf("cookie do portal %q, limite por credencial %d", c.PortalCookieName, c.APICredentialRateLimitPerMinute)
	}
	m := baseEnv()
	m["RECORVIA_COOKIE_NAME"] = "__Host-recorvia_session"
	m["RECORVIA_COOKIE_SECURE"] = "true"
	c, err = Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.PortalCookieName != "__Host-recorvia_portal" {
		t.Fatalf("cookie do portal deveria seguir o prefixo __Host-: %q", c.PortalCookieName)
	}
	if err := c.ValidateAPI(); err != nil {
		t.Fatalf("configuração segura deveria valer: %v", err)
	}
}

func TestPortalValidation(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
		frag string
	}{
		{"link curto", map[string]string{"RECORVIA_PORTAL_LINK_TTL": "5m"}, "PORTAL_LINK_TTL"},
		{"link longo", map[string]string{"RECORVIA_PORTAL_LINK_TTL": "721h"}, "PORTAL_LINK_TTL"},
		{"inatividade acima da duração", map[string]string{"RECORVIA_PORTAL_SESSION_TTL": "1h", "RECORVIA_PORTAL_SESSION_IDLE_TTL": "2h"}, "PORTAL_SESSION_IDLE_TTL"},
		{"inatividade mínima", map[string]string{"RECORVIA_PORTAL_SESSION_IDLE_TTL": "30s"}, "maior que 1m"},
		{"cookie igual ao da sessão", map[string]string{"RECORVIA_PORTAL_COOKIE_NAME": "recorvia_session"}, "diferente do cookie de sessão"},
		{"__Host- sem Secure", map[string]string{"RECORVIA_PORTAL_COOKIE_NAME": "__Host-portal"}, "__Host-"},
		{"produção sem __Host-", map[string]string{"RECORVIA_ENV": "production", "RECORVIA_PUBLIC_URL": "https://recorvia.example",
			"RECORVIA_COOKIE_SECURE": "true", "RECORVIA_COOKIE_NAME": "__Host-recorvia_session", "RECORVIA_PORTAL_COOKIE_NAME": "recorvia_portal"},
			"cookie do portal deve usar o prefixo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := baseEnv()
			for k, v := range tc.set {
				m[k] = v
			}
			c, err := Load(env(m))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.ValidateAPI(); err == nil || !strings.Contains(err.Error(), tc.frag) {
				t.Fatalf("esperava erro com %q: %v", tc.frag, err)
			}
		})
	}
}

func TestRetentionDefaults(t *testing.T) {
	c, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	want := Retention{InboxDays: 90, DeadLettersDays: 180, WebhooksDays: 90, ProviderWebhookPayloadsDays: 90, NotificationContentDays: 90,
		NotificationsDays: 365, PortalLinksDays: 90, UsageEventsDays: 400, InvitationsDays: 90, BatchSize: 500}
	if c.Retention != want {
		t.Fatalf("prazos padrão: %+v", c.Retention)
	}
	if err := c.ValidateWorker(); err != nil {
		t.Fatalf("os prazos padrão devem ser válidos: %v", err)
	}
	// A API monta os mesmos serviços e recusa os mesmos valores.
	m := baseEnv()
	m["RECORVIA_RETENTION_INBOX_DAYS"] = "1"
	c, err = Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateAPI(); err == nil || !strings.Contains(err.Error(), "RECORVIA_RETENTION_INBOX_DAYS deve ficar entre 14 e 3650") {
		t.Fatalf("ValidateAPI com prazo abaixo do mínimo: %v", err)
	}
}

// O worker não sobe com prazo de retenção abaixo do mínimo, acima do máximo
// ou incoerente.
func TestRetentionValidation(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
		want string // trecho esperado do erro; vazio = válido
	}{
		{"mínimos", map[string]string{"RECORVIA_RETENTION_INBOX_DAYS": "14", "RECORVIA_RETENTION_DEAD_LETTERS_DAYS": "30",
			"RECORVIA_RETENTION_WEBHOOKS_DAYS": "7", "RECORVIA_RETENTION_PROVIDER_WEBHOOK_PAYLOADS_DAYS": "7",
			"RECORVIA_RETENTION_NOTIFICATION_CONTENT_DAYS": "7", "RECORVIA_RETENTION_NOTIFICATIONS_DAYS": "30",
			"RECORVIA_RETENTION_PORTAL_LINKS_DAYS": "1", "RECORVIA_RETENTION_USAGE_EVENTS_DAYS": "30",
			"RECORVIA_RETENTION_INVITATIONS_DAYS": "7", "RECORVIA_RETENTION_BATCH_SIZE": "10"}, ""},
		{"máximos", map[string]string{"RECORVIA_RETENTION_USAGE_EVENTS_DAYS": "3650", "RECORVIA_RETENTION_BATCH_SIZE": "5000"}, ""},
		{"inbox", map[string]string{"RECORVIA_RETENTION_INBOX_DAYS": "13"}, "RECORVIA_RETENTION_INBOX_DAYS deve ficar entre 14 e 3650"},
		{"dead letters", map[string]string{"RECORVIA_RETENTION_DEAD_LETTERS_DAYS": "29"}, "RECORVIA_RETENTION_DEAD_LETTERS_DAYS deve ficar entre 30 e 3650"},
		{"webhooks", map[string]string{"RECORVIA_RETENTION_WEBHOOKS_DAYS": "6"}, "RECORVIA_RETENTION_WEBHOOKS_DAYS deve ficar entre 7 e 3650"},
		{"corpo de webhooks recebidos", map[string]string{"RECORVIA_RETENTION_PROVIDER_WEBHOOK_PAYLOADS_DAYS": "0"},
			"RECORVIA_RETENTION_PROVIDER_WEBHOOK_PAYLOADS_DAYS deve ficar entre 7 e 3650"},
		{"conteúdo", map[string]string{"RECORVIA_RETENTION_NOTIFICATION_CONTENT_DAYS": "6"},
			"RECORVIA_RETENTION_NOTIFICATION_CONTENT_DAYS deve ficar entre 7 e 3650"},
		{"notificações", map[string]string{"RECORVIA_RETENTION_NOTIFICATIONS_DAYS": "29", "RECORVIA_RETENTION_NOTIFICATION_CONTENT_DAYS": "7"},
			"RECORVIA_RETENTION_NOTIFICATIONS_DAYS deve ficar entre 30 e 3650"},
		{"links do portal", map[string]string{"RECORVIA_RETENTION_PORTAL_LINKS_DAYS": "0"}, "RECORVIA_RETENTION_PORTAL_LINKS_DAYS deve ficar entre 1 e 3650"},
		{"consumo", map[string]string{"RECORVIA_RETENTION_USAGE_EVENTS_DAYS": "29"}, "RECORVIA_RETENTION_USAGE_EVENTS_DAYS deve ficar entre 30 e 3650"},
		{"convites", map[string]string{"RECORVIA_RETENTION_INVITATIONS_DAYS": "6"}, "RECORVIA_RETENTION_INVITATIONS_DAYS deve ficar entre 7 e 3650"},
		{"acima do máximo", map[string]string{"RECORVIA_RETENTION_INBOX_DAYS": "3651"}, "RECORVIA_RETENTION_INBOX_DAYS deve ficar entre 14 e 3650"},
		{"lote pequeno", map[string]string{"RECORVIA_RETENTION_BATCH_SIZE": "9"}, "RECORVIA_RETENTION_BATCH_SIZE deve ficar entre 10 e 5000"},
		{"lote grande", map[string]string{"RECORVIA_RETENTION_BATCH_SIZE": "5001"}, "RECORVIA_RETENTION_BATCH_SIZE deve ficar entre 10 e 5000"},
		{"notificações antes do conteúdo", map[string]string{"RECORVIA_RETENTION_NOTIFICATIONS_DAYS": "100",
			"RECORVIA_RETENTION_NOTIFICATION_CONTENT_DAYS": "120"}, "não pode ser menor que RECORVIA_RETENTION_NOTIFICATION_CONTENT_DAYS"},
		{"link expurgado antes do fim da sessão do portal", map[string]string{"RECORVIA_RETENTION_PORTAL_LINKS_DAYS": "1",
			"RECORVIA_PORTAL_SESSION_TTL": "48h"}, "não pode ser menor que RECORVIA_PORTAL_SESSION_TTL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := baseEnv()
			for k, v := range tc.set {
				m[k] = v
			}
			c, err := Load(env(m))
			if err != nil {
				t.Fatal(err)
			}
			err = c.ValidateWorker()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("erro inesperado: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("erro %v, esperado conter %q", err, tc.want)
			}
		})
	}
	// Valor que não é inteiro é erro de formato, acumulado no carregamento.
	m := baseEnv()
	m["RECORVIA_RETENTION_INBOX_DAYS"] = "noventa"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "RECORVIA_RETENTION_INBOX_DAYS") {
		t.Fatalf("formato inválido: %v", err)
	}
}

func TestEncryptionStartupCheckMode(t *testing.T) {
	cases := []struct {
		value   string
		want    KeyringCheckMode
		wantErr bool
	}{
		{value: "", want: KeyringCheckEnforce},
		{value: "enforce", want: KeyringCheckEnforce},
		{value: "warn", want: KeyringCheckWarn},
		{value: "off", want: KeyringCheckOff},
		{value: "talvez", wantErr: true},
	}
	for _, tc := range cases {
		m := baseEnv()
		m["RECORVIA_ENCRYPTION_STARTUP_CHECK"] = tc.value
		c, err := Load(env(m))
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), "RECORVIA_ENCRYPTION_STARTUP_CHECK") {
				t.Errorf("%q: erro = %v, esperado erro citando a variável", tc.value, err)
			}
			continue
		}
		if err != nil || c.EncryptionStartupCheck != tc.want {
			t.Errorf("%q: modo = %q, erro = %v; esperado %q", tc.value, c.EncryptionStartupCheck, err, tc.want)
		}
	}
}

func TestValidateStoreRequiresDatabaseAndKeys(t *testing.T) {
	c, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateStore(); err != nil {
		t.Fatalf("ValidateStore: %v", err)
	}
	m := baseEnv()
	delete(m, "RECORVIA_DATABASE_URL")
	delete(m, "RECORVIA_ENCRYPTION_KEYS")
	// Sem identidade, Redis ou broker: a conferência do keyring só usa o banco.
	delete(m, "RECORVIA_OIDC_ISSUER")
	delete(m, "RECORVIA_REDIS_URL")
	c, err = Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	err = c.ValidateStore()
	if err == nil {
		t.Fatal("esperava erro sem banco e sem chaves")
	}
	for _, frag := range []string{"RECORVIA_DATABASE_URL", "RECORVIA_ENCRYPTION_KEYS"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("erro não menciona %s: %v", frag, err)
		}
	}
	for _, frag := range []string{"OIDC", "REDIS"} {
		if strings.Contains(err.Error(), frag) {
			t.Errorf("erro não deveria exigir %s: %v", frag, err)
		}
	}
}

func TestEncryptionLostKeys(t *testing.T) {
	cases := []struct {
		value   string
		want    []string
		wantErr string
	}{
		{value: ""},
		{value: "antiga", want: []string{"antiga"}},
		{value: " k9 , antiga,,k9 ", want: []string{"antiga", "k9"}},
		{value: "antiga:c2VncmVkbw==", wantErr: "informe somente os identificadores"},
		{value: "k1", wantErr: `a chave "k1" está em RECORVIA_ENCRYPTION_KEYS`},
	}
	for _, tc := range cases {
		m := baseEnv()
		m["RECORVIA_ENCRYPTION_LOST_KEYS"] = tc.value
		c, err := Load(env(m))
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), "RECORVIA_ENCRYPTION_LOST_KEYS") || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%q: erro = %v, esperado conter %q", tc.value, err, tc.wantErr)
			}
			continue
		}
		if err != nil || !slices.Equal(c.EncryptionLostKeys, tc.want) {
			t.Errorf("%q: chaves perdidas = %v, erro = %v; esperado %v", tc.value, c.EncryptionLostKeys, err, tc.want)
		}
	}
}

// Chave ativa perdida: a fase 1 da rotação (acrescentar a chave nova
// mantendo a ativa) não é possível, porque a ativa não está mais na lista; a
// troca num único passo (chave nova ativa e a perdida declarada) é aceita
// pela API, pelo worker e pelo keyring-check.
func TestLostActiveKeyIsReplacedInOneStep(t *testing.T) {
	validators := map[string]func(*Config) error{
		"api": (*Config).ValidateAPI, "worker": (*Config).ValidateWorker, "keyring-check": (*Config).ValidateStore,
	}
	cases := []struct {
		name, keys, active, lost string
		wantErr                  string
	}{
		{name: "fase 1 com a chave ativa perdida", keys: "k1:" + key32 + ",k3:" + key32, active: "k2",
			wantErr: "RECORVIA_ENCRYPTION_ACTIVE_KEY deve referenciar uma chave configurada"},
		{name: "fase 1 com a perda já declarada", keys: "k1:" + key32 + ",k3:" + key32, active: "k2", lost: "k2",
			wantErr: "RECORVIA_ENCRYPTION_ACTIVE_KEY deve referenciar uma chave configurada"},
		{name: "troca num único passo", keys: "k1:" + key32 + ",k3:" + key32, active: "k3", lost: "k2"},
		{name: "única chave perdida", keys: "k3:" + key32, active: "k3", lost: "k2"},
	}
	for _, tc := range cases {
		m := baseEnv()
		m["RECORVIA_ENCRYPTION_KEYS"] = tc.keys
		m["RECORVIA_ENCRYPTION_ACTIVE_KEY"] = tc.active
		m["RECORVIA_ENCRYPTION_LOST_KEYS"] = tc.lost
		c, err := Load(env(m))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for process, validate := range validators {
			err := validate(c)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("%s (%s): erro inesperado: %v", tc.name, process, err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("%s (%s): erro = %v, esperado conter %q", tc.name, process, err, tc.wantErr)
			}
		}
		if tc.wantErr == "" && (c.EncryptionActiveKey != tc.active || !slices.Equal(c.EncryptionLostKeys, []string{tc.lost})) {
			t.Errorf("%s: ativa = %q, perdidas = %v", tc.name, c.EncryptionActiveKey, c.EncryptionLostKeys)
		}
	}
	// O identificador da chave perdida não pode ser reutilizado pela chave
	// nova: uma chave configurada não pode ser declarada perdida.
	m := baseEnv()
	m["RECORVIA_ENCRYPTION_KEYS"] = "k1:" + key32 + ",k2:" + key32
	m["RECORVIA_ENCRYPTION_ACTIVE_KEY"] = "k2"
	m["RECORVIA_ENCRYPTION_LOST_KEYS"] = "k2"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), `a chave "k2" está em RECORVIA_ENCRYPTION_KEYS`) {
		t.Errorf("erro = %v, esperado recusar o identificador reutilizado", err)
	}
}
