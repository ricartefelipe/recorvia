package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

// Toda resposta da API leva os cabeçalhos de segurança, também quando ela é
// alcançada sem o nginx da interface: sem sniffing de tipo, sem frame, sem
// carregar recursos, sem referrer e sem cache; HSTS só em produção.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	for _, env := range []config.Environment{config.Development, config.Production} {
		_, router := NewRouter(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.System{}, Config: &config.Config{Env: env}})
		for _, path := range []string{"/livez", "/rota-inexistente", "/api/openapi.yaml"} {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			h := rec.Header()
			want := map[string]string{
				"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'",
				"X-Content-Type-Options":  "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store",
			}
			for k, v := range want {
				if got := h.Get(k); got != v {
					t.Errorf("%s %s: %s = %q, esperado %q", env, path, k, got, v)
				}
			}
			if hsts := h.Get("Strict-Transport-Security"); (env == config.Production) != (hsts != "") {
				t.Errorf("%s %s: HSTS %q", env, path, hsts)
			}
		}
	}
}
