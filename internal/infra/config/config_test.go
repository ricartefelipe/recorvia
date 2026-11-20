package config

import (
	"encoding/base64"
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
