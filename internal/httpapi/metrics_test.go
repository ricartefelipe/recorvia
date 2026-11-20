package httpapi

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

// Métodos inventados por um cliente sem autenticação não criam séries
// novas nem viram nome de span ou campo de log: viram "_OTHER".
func TestArbitraryHTTPMethodsDoNotCreateSeries(t *testing.T) {
	metrics := telemetry.NewMetrics("api")
	logs := &bytes.Buffer{}
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	_, router := NewRouter(Deps{Log: slog.New(slog.NewTextHandler(logs, nil)), Clock: clock.System{}, Metrics: metrics})

	invented := []string{"PROPFIND", strings.Repeat("X", 20000)}
	for i := range 50 {
		invented = append(invented, fmt.Sprintf("X%d", i))
	}
	cases := []struct {
		methods []string
		want    string
	}{
		{[]string{http.MethodGet, http.MethodPost, http.MethodOptions}, ""},
		{invented, "_OTHER"},
	}
	for _, c := range cases {
		for _, m := range c.methods {
			ctx, span := tp.Tracer("teste").Start(t.Context(), "http")
			req := httptest.NewRequest(http.MethodGet, "/api/v1/customers", nil).WithContext(ctx)
			req.Method = m
			router.ServeHTTP(httptest.NewRecorder(), req)
			span.End()
			want := c.want
			if want == "" {
				want = m
			}
			ended := rec.Ended()
			last := ended[len(ended)-1]
			if name := last.Name(); !strings.HasPrefix(name, want+" ") {
				t.Fatalf("nome do span %.40q, esperado prefixo %q", name, want)
			}
			for _, kv := range last.Attributes() {
				if kv.Key == "http.request.method_original" && len(kv.Value.AsString()) > maxOriginalMethod {
					t.Fatalf("método original no span com %d bytes", len(kv.Value.AsString()))
				}
			}
		}
	}

	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{http.MethodGet: true, http.MethodPost: true, http.MethodOptions: true, "_OTHER": true}
	for _, f := range families {
		if f.GetName() != "recorvia_http_requests_total" && f.GetName() != "recorvia_http_request_duration_seconds" {
			continue
		}
		methods := map[string]bool{}
		for _, s := range f.GetMetric() {
			for _, l := range s.GetLabel() {
				if l.GetName() == "method" {
					methods[l.GetValue()] = true
				}
			}
		}
		for m := range methods {
			if !allowed[m] {
				t.Fatalf("%s com método fora da lista: %.40q", f.GetName(), m)
			}
		}
		if len(f.GetMetric()) > len(allowed) {
			t.Fatalf("%s: %d séries para %d métodos permitidos", f.GetName(), len(f.GetMetric()), len(allowed))
		}
	}
	if strings.Contains(logs.String(), "PROPFIND") || strings.Contains(logs.String(), "X49") ||
		!strings.Contains(logs.String(), "method=_OTHER") {
		t.Fatalf("log de acesso com método não normalizado: %.300s", logs.String())
	}
}

func TestNormalizeMethod(t *testing.T) {
	for in, want := range map[string]string{
		"GET": "GET", "HEAD": "HEAD", "POST": "POST", "PUT": "PUT", "PATCH": "PATCH", "DELETE": "DELETE",
		"CONNECT": "CONNECT", "OPTIONS": "OPTIONS", "TRACE": "TRACE", "get": "_OTHER", "PROPFIND": "_OTHER", "": "_OTHER",
	} {
		if got := normalizeMethod(in); got != want {
			t.Errorf("normalizeMethod(%q) = %q, esperado %q", in, got, want)
		}
	}
}
