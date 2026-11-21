package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ricartefelipe/recorvia/internal/infra/config"
)

// Origem permitida recebe os cabeçalhos de resposta que a interface lê
// (ETag, Location, X-Request-ID, idempotência e o corte da exportação), sem
// os quais o fetch em outra origem os veria nulos; origem não permitida não
// recebe nenhum cabeçalho CORS.
func TestCORSExposesResponseHeaders(t *testing.T) {
	a := &API{Deps: Deps{Config: &config.Config{AllowedOrigins: []string{"https://app.exemplo.test"}}}}
	h := a.cors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Recorvia-Export-Truncated", "true")
		w.WriteHeader(http.StatusOK)
	}))
	want := []string{"ETag", "Location", "X-Request-ID", "Idempotent-Replayed", "X-Recorvia-Export-Limit", "X-Recorvia-Export-Truncated",
		"X-Recorvia-Export-Next-Cursor", "X-Recorvia-Export-Next-Part"}
	cases := []struct {
		name, method, origin string
		allowed              bool
	}{
		{"GET de origem permitida", http.MethodGet, "https://app.exemplo.test", true},
		{"pré-voo de origem permitida", http.MethodOptions, "https://app.exemplo.test", true},
		{"origem não permitida", http.MethodGet, "https://outra.exemplo.test", false},
		{"sem origem", http.MethodGet, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/v1/invoices/export", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			exposed := w.Header().Get("Access-Control-Expose-Headers")
			if !tc.allowed {
				if exposed != "" || w.Header().Get("Access-Control-Allow-Origin") != "" {
					t.Fatalf("origem não permitida recebeu CORS: %v", w.Header())
				}
				return
			}
			if w.Header().Get("Access-Control-Allow-Origin") != tc.origin {
				t.Fatalf("Allow-Origin: %v", w.Header())
			}
			got := map[string]bool{}
			for _, s := range strings.Split(exposed, ",") {
				got[strings.TrimSpace(s)] = true
			}
			for _, name := range want {
				if !got[name] {
					t.Errorf("cabeçalho %s não exposto: %q", name, exposed)
				}
			}
		})
	}
}
