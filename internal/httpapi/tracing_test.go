package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

// O span HTTP (criado pelo otelhttp em cmd/api) recebe o padrão de rota como
// nome e o X-Request-ID como atributo, sem identificadores da URL concreta.
func TestHTTPSpanNamedByRouteWithRequestID(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	_, router := NewRouter(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.System{}})
	cases := []struct {
		path, wantName, wantRoute string
	}{
		{"/livez", "GET /livez", "/livez"},
		{"/api/v1/customers/0194b7c2-0000-7000-8000-000000000001", "GET /api/v1/customers/{id}", "/api/v1/customers/{id}"},
	}
	for _, tc := range cases {
		ctx, span := tp.Tracer("teste").Start(t.Context(), "http")
		req := httptest.NewRequest(http.MethodGet, tc.path, nil).WithContext(ctx)
		req.Header.Set("X-Request-ID", "req-0123456789abcdef")
		router.ServeHTTP(httptest.NewRecorder(), req)
		span.End()
		ended := rec.Ended()
		s := ended[len(ended)-1]
		if s.Name() != tc.wantName {
			t.Fatalf("nome do span = %q, esperado %q", s.Name(), tc.wantName)
		}
		attrs := map[string]string{}
		for _, a := range s.Attributes() {
			attrs[string(a.Key)] = a.Value.String()
		}
		if attrs["recorvia.request_id"] != "req-0123456789abcdef" || attrs["http.route"] != tc.wantRoute {
			t.Fatalf("atributos %v", attrs)
		}
	}
}
