// Package httpapi é o adaptador HTTP do Recorvia: rotas, autenticação por
// sessão, autorização por permissão, CSRF, validação de tenant,
// idempotência, limites e erros estruturados (RFC 9457).
//
// Toda rota é registrada com a permissão exigida (ou marcada explicitamente
// como pública/autenticada sem tenant); um teste garante que nenhuma rota
// fica sem declaração e que as rotas correspondem ao contrato OpenAPI.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ricartefelipe/recorvia/api"
	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/infra/redisstore"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/portal"
	"github.com/ricartefelipe/recorvia/internal/reporting"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

// Deps são as dependências da API, injetadas pelo executável.
type Deps struct {
	Log           *slog.Logger
	Config        *config.Config
	Clock         clock.Clock
	Metrics       *telemetry.Metrics
	Store         *pgstore.Store
	Sessions      *redisstore.Store
	Identity      *identity.Service
	Tenancy       *tenancy.Service
	Platform      *platformbilling.Service
	Customers     *customers.Service
	Catalog       *catalog.Service
	Subscriptions *subscriptions.Service
	Billing       *billing.Service
	Changes       *billing.ChangeService
	CreditNotes   *billing.CreditNoteService
	Coupons       *catalog.CouponService
	Terms         *subscriptions.TermsService
	Metering      *metering.Service
	Payments      *payments.Service
	Reporting     *reporting.Service
	Notifications *notifications.Service
	Collections   *collections.Service
	Webhooks      *webhooks.Service
	Portal        *portal.Service
	Credentials   *apicredentials.Service
	Providers     []string
	// InvoiceExportMaxRows limita as faturas por arquivo exportado; zero usa
	// o padrão (exportMaxRows). Os testes reduzem o limite para exercitar a
	// exportação em partes.
	InvoiceExportMaxRows int
	// Ready verifica dependências críticas (readiness).
	Ready func(ctx context.Context) map[string]error
}

// API mantém as dependências dos handlers.
type API struct {
	Deps
	log    *slog.Logger
	routes []RouteInfo
}

// Access define o nível de acesso de uma rota.
type Access int

const (
	// Public não exige autenticação (login, webhooks, health, portal access).
	Public Access = iota
	// Authenticated exige sessão, sem organização ativa.
	Authenticated
	// TenantScoped exige organização ativa e a permissão indicada.
	TenantScoped
	// PlatformStaff exige integrante da equipe da plataforma.
	PlatformStaff
	// Portal exige sessão do portal do cliente.
	Portal
)

// RouteInfo registra a política de acesso de cada rota.
type RouteInfo struct {
	Method     string
	Pattern    string
	Access     Access
	Permission tenancy.Permission
	Idempotent bool
	// UserSession indica que a rota exige a sessão de um usuário e recusa
	// credenciais de API (403 user_session_required). Vale para todas as
	// rotas Authenticated e para as rotas de tenant declaradas com
	// userSession(), cujo ato depende de uma pessoa identificada (ADR-059).
	UserSession bool
}

// Routes retorna as rotas registradas (usado pelos testes de contrato).
func (a *API) Routes() []RouteInfo { return a.routes }

type routeOpt func(*RouteInfo)

func idempotent() routeOpt { return func(r *RouteInfo) { r.Idempotent = true } }

// userSession restringe uma rota de tenant à sessão de um usuário: a
// operação exige uma pessoa identificada (gestão de membros, do plano da
// plataforma e de credenciais) e não é aceita por credenciais de API, mesmo
// que um escopo a conceda.
func userSession() routeOpt { return func(r *RouteInfo) { r.UserSession = true } }

func (a *API) handle(r chi.Router, method, pattern string, access Access, perm tenancy.Permission, h http.HandlerFunc, opts ...routeOpt) {
	info := RouteInfo{Method: method, Pattern: pattern, Access: access, Permission: perm, UserSession: access == Authenticated}
	for _, o := range opts {
		o(&info)
	}
	a.routes = append(a.routes, info)
	handler := a.requestRules(method, pattern, h)
	if info.Idempotent {
		handler = a.idempotency(info, handler)
	}
	switch access {
	case TenantScoped:
		handler = a.requireTenant(perm, handler)
	case PlatformStaff:
		handler = a.requireStaff(handler)
	}
	// A recusa da credencial vem antes da verificação de escopos, para que a
	// resposta seja sempre 403 user_session_required.
	if info.UserSession {
		handler = a.requireUserSession(handler)
	}
	r.Method(method, pattern, handler)
}

// NewRouter monta o roteador completo.
func NewRouter(d Deps) (*API, http.Handler) {
	a := &API{Deps: d, log: d.Log}
	r := chi.NewRouter()
	r.Use(a.requestID, a.recoverer, a.observe, a.securityHeaders, a.cors)

	r.Get("/livez", a.livez)
	r.Get("/readyz", a.readyz)
	r.Get("/api/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(api.OpenAPI) // falha de escrita ocorre apenas com o cliente desconectado
	})

	// Webhooks de provedores: limite próprio de corpo, sem sessão.
	r.Group(func(r chi.Router) {
		r.Use(a.limitBody(payments.MaxWebhookBody), a.timeout(10*time.Second), a.rateLimitIP(1200))
		a.handle(r, http.MethodPost, "/webhooks/providers/{provider}/{token}", Public, "", a.receiveProviderWebhook)
	})

	r.Group(func(r chi.Router) {
		r.Use(a.limitBody(1<<20), a.timeout(30*time.Second))
		r.Group(func(r chi.Router) {
			r.Use(a.rateLimitIP(120))
			a.handle(r, http.MethodGet, "/auth/login", Public, "", a.authLogin)
			a.handle(r, http.MethodGet, "/auth/callback", Public, "", a.authCallback)
		})
		r.Group(func(r chi.Router) {
			r.Use(a.authenticate, a.csrf, a.rateLimitPrincipal)
			a.registerAuthenticated(r)
			a.registerTenantRoutes(r)
		})
		a.registerPortal(r)
	})
	return a, r
}

func (a *API) registerAuthenticated(r chi.Router) {
	a.handle(r, http.MethodPost, "/auth/logout", Authenticated, "", a.authLogout)
	a.handle(r, http.MethodPost, "/auth/logout-all", Authenticated, "", a.authLogoutAll)
	a.handle(r, http.MethodGet, "/api/v1/session", Authenticated, "", a.getSession)
	a.handle(r, http.MethodPut, "/api/v1/session/tenant", Authenticated, "", a.switchTenant)
	a.handle(r, http.MethodPost, "/api/v1/organizations", Authenticated, "", a.createOrganization)
	a.handle(r, http.MethodPost, "/api/v1/invitations/accept", Authenticated, "", a.acceptInvitation)
	a.handle(r, http.MethodGet, "/api/v1/admin/tenants", PlatformStaff, "", a.adminTenants)
	// Atendimento dos serviços adicionais pela equipe da plataforma (ADR-077).
	a.handle(r, http.MethodGet, "/api/v1/admin/addon-orders", PlatformStaff, "", a.adminAddonOrders)
	a.handle(r, http.MethodPost, "/api/v1/admin/tenants/{tenant_id}/addon-orders/{id}/schedule", PlatformStaff, "", a.adminScheduleAddonOrder)
	a.handle(r, http.MethodPost, "/api/v1/admin/tenants/{tenant_id}/addon-orders/{id}/complete", PlatformStaff, "", a.adminCompleteAddonOrder)
	a.handle(r, http.MethodPost, "/api/v1/admin/tenants/{tenant_id}/addon-orders/{id}/cancel", PlatformStaff, "", a.adminCancelAddonOrder)
}

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b) // crypto/rand.Read não falha a partir do Go 1.24
	return hex.EncodeToString(b)
}

// requestID aceita um X-Request-ID bem formado ou gera um novo, devolve-o na
// resposta e o inclui nos logs e eventos.
func (a *API) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !requestIDRe.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		// O mesmo identificador no span HTTP permite ir do log ou do corpo de
		// erro ao rastro correspondente.
		trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("recorvia.request_id", id))
		ctx := logging.WithAttrs(r.Context(), slog.String("request_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *API) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				a.log.ErrorContext(r.Context(), "pânico no handler", slog.Any("panic", v), slog.String("stack", string(debug.Stack())))
				a.writeError(w, r, fmt.Errorf("pânico: %v", v))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// knownMethods são os métodos HTTP padronizados (RFC 9110 e PATCH); os
// demais viram otherMethod em métricas, nome de span e log.
var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true,
	http.MethodDelete: true, http.MethodConnect: true, http.MethodOptions: true, http.MethodTrace: true,
}

// otherMethod é o valor usado para métodos fora da lista (mesma convenção
// das convenções semânticas do OpenTelemetry).
const otherMethod = "_OTHER"

// maxOriginalMethod limita o método original guardado no span quando ele não
// é padronizado: o otelhttp registra em http.request.method_original o valor
// recebido inteiro (até o limite de cabeçalhos, 64 KiB).
const maxOriginalMethod = 32

// normalizeMethod limita os valores do método usados como rótulo de métrica
// e nome de span: o net/http aceita qualquer token como método, e um cliente
// sem autenticação criaria uma série nova por método inventado.
func normalizeMethod(m string) string {
	if knownMethods[m] {
		return m
	}
	return otherMethod
}

// observe registra log de acesso e métricas por padrão de rota (nunca pela
// URL concreta, que contém identificadores) e pelo método normalizado.
func (a *API) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		route := "unmatched"
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			route = rc.RoutePattern()
		}
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		d := time.Since(start)
		method := normalizeMethod(r.Method)
		// Nome do span pelo padrão de rota (nunca pela URL concreta).
		span := trace.SpanFromContext(r.Context())
		span.SetName(method + " " + route)
		span.SetAttributes(attribute.String("http.route", route))
		if method == otherMethod {
			// Métodos válidos são tokens ASCII: o corte não divide caracteres.
			span.SetAttributes(attribute.String("http.request.method_original", r.Method[:min(len(r.Method), maxOriginalMethod)]))
		}
		if a.Metrics != nil {
			a.Metrics.HTTPRequests.WithLabelValues(route, method, strconv.Itoa(rec.status/100)+"xx").Inc()
			a.Metrics.HTTPDuration.WithLabelValues(route, method).Observe(d.Seconds())
		}
		if route == "/livez" || route == "/readyz" {
			return
		}
		a.log.InfoContext(r.Context(), "requisição", slog.String("method", method), slog.String("route", route),
			slog.Int("status", rec.status), slog.Duration("duration", d))
	})
}

func (a *API) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		if a.Config != nil && a.Config.Env == config.Production {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// exposedHeaders são os cabeçalhos de resposta liberados para origens
// permitidas (Access-Control-Expose-Headers).
const exposedHeaders = "ETag, Location, X-Request-ID, Idempotent-Replayed, X-Recorvia-Export-Limit, X-Recorvia-Export-Truncated, " +
	"X-Recorvia-Export-Next-Cursor, X-Recorvia-Export-Next-Part"

// cors só responde a origens explicitamente permitidas; por padrão a
// aplicação é servida na mesma origem e nenhum header CORS é emitido.
func (a *API) cors(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	if a.Config != nil {
		for _, o := range a.Config.AllowedOrigins {
			allowed[o] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Vary", "Origin")
			// Cabeçalhos de resposta que a interface lê (concorrência,
			// idempotência, rastreio e o corte da exportação de faturas): sem
			// isto, o navegador os esconde do fetch em outra origem.
			h.Set("Access-Control-Expose-Headers", exposedHeaders)
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
				h.Set("Access-Control-Allow-Headers", "Content-Type, X-CSRF-Token, X-Recorvia-Tenant, If-Match, Idempotency-Key, X-Request-ID")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) limitBody(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

func (a *API) timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func clientIP(r *http.Request) string {
	// O proxy da aplicação (nginx) define X-Real-IP; sem ele usa o par remoto.
	if ip := r.Header.Get("X-Real-IP"); ip != "" && len(ip) <= 45 {
		return ip
	}
	host := r.RemoteAddr
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			return host[:i]
		}
	}
	return host
}

// rateLimitIP limita requisições por IP (por minuto). Falha do Redis libera
// a requisição: limites são proteção auxiliar, não regra financeira.
func (a *API) rateLimitIP(perMinute int) func(http.Handler) http.Handler {
	return a.rateLimitIPScoped("ip", perMinute)
}

// rateLimitIPScoped limita requisições por IP com contador próprio do escopo
// (o limite de uma rota não consome o de outra).
func (a *API) rateLimitIPScoped(scope string, perMinute int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a.Sessions != nil {
				ok, _, err := a.Sessions.Allow(r.Context(), scope+":"+clientIP(r), perMinute, time.Minute)
				if err != nil {
					a.log.WarnContext(r.Context(), "limite de taxa indisponível", slog.String("error", err.Error()))
				} else if !ok {
					a.writeError(w, r, apperr.RateLimited("rate_limited", "Muitas requisições. Aguarde e tente novamente."))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (a *API) rateLimitPrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := 600
		if a.Config != nil && a.Config.RateLimitPerMinute > 0 {
			limit = a.Config.RateLimitPerMinute
		}
		key := "anon:" + clientIP(r)
		if p, ok := principalFrom(r.Context()); ok {
			key = p.ClientID()
			if p.Credential != nil && a.Config != nil && a.Config.APICredentialRateLimitPerMinute > 0 {
				limit = a.Config.APICredentialRateLimitPerMinute
			}
		}
		if a.Sessions != nil {
			ok, _, err := a.Sessions.Allow(r.Context(), key, limit, time.Minute)
			if err != nil {
				a.log.WarnContext(r.Context(), "limite de taxa indisponível", slog.String("error", err.Error()))
			} else if !ok {
				a.writeError(w, r, apperr.RateLimited("rate_limited", "Muitas requisições. Aguarde e tente novamente."))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// livez indica apenas que o processo responde; não verifica dependências,
// para que indisponibilidades externas não provoquem reinícios em cascata.
func (a *API) livez(w http.ResponseWriter, r *http.Request) {
	a.writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz verifica dependências críticas (PostgreSQL e Redis).
func (a *API) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	checks := map[string]string{}
	status := "ok"
	code := http.StatusOK
	if a.Ready != nil {
		for name, err := range a.Ready(ctx) {
			if err != nil {
				checks[name] = "unavailable"
				status, code = "unavailable", http.StatusServiceUnavailable
				if a.Metrics != nil {
					a.Metrics.DependencyUp.WithLabelValues(name).Set(0)
				}
				continue
			}
			checks[name] = "ok"
			if a.Metrics != nil {
				a.Metrics.DependencyUp.WithLabelValues(name).Set(1)
			}
		}
	}
	a.writeJSON(w, r, code, map[string]any{"status": status, "checks": checks})
}
