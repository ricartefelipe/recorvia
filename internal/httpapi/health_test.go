package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

// RNF-09 e ADR-054: /livez só indica que o processo responde; /readyz exige
// PostgreSQL e Redis acessíveis, informa o estado de cada verificação e
// atualiza recorvia_dependency_up.
func TestLivenessAndReadinessHaveDistinctSemantics(t *testing.T) {
	down := errors.New("conexão recusada")
	cases := []struct {
		name       string
		ready      map[string]error
		path       string
		wantCode   int
		wantStatus string
		wantChecks map[string]string
		wantUp     map[string]float64
	}{
		{"liveness com dependências indisponíveis", map[string]error{"postgres": down, "redis": down}, "/livez", http.StatusOK, "ok", nil, nil},
		{"pronto", map[string]error{"postgres": nil, "redis": nil}, "/readyz", http.StatusOK, "ok",
			map[string]string{"postgres": "ok", "redis": "ok"}, map[string]float64{"postgres": 1, "redis": 1}},
		{"banco indisponível", map[string]error{"postgres": down, "redis": nil}, "/readyz", http.StatusServiceUnavailable, "unavailable",
			map[string]string{"postgres": "unavailable", "redis": "ok"}, map[string]float64{"postgres": 0, "redis": 1}},
		{"redis indisponível", map[string]error{"postgres": nil, "redis": down}, "/readyz", http.StatusServiceUnavailable, "unavailable",
			map[string]string{"postgres": "ok", "redis": "unavailable"}, map[string]float64{"postgres": 1, "redis": 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics := telemetry.NewMetrics("api")
			_, router := NewRouter(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.System{}, Metrics: metrics,
				Ready: func(context.Context) map[string]error { return tc.ready }})
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("status %d, esperado %d", rec.Code, tc.wantCode)
			}
			var body struct {
				Status string            `json:"status"`
				Checks map[string]string `json:"checks"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Status != tc.wantStatus || len(body.Checks) != len(tc.wantChecks) {
				t.Fatalf("corpo %+v", body)
			}
			for k, v := range tc.wantChecks {
				if body.Checks[k] != v {
					t.Fatalf("verificação %s = %q, esperado %q", k, body.Checks[k], v)
				}
			}
			if got := dependencyUp(t, metrics); len(got) != len(tc.wantUp) {
				t.Fatalf("recorvia_dependency_up = %v, esperado %v (a liveness não consulta dependências)", got, tc.wantUp)
			}
			for dep, want := range tc.wantUp {
				if got := dependencyUp(t, metrics)[dep]; got != want {
					t.Fatalf("recorvia_dependency_up{dependency=%q} = %v, esperado %v", dep, got, want)
				}
			}
		})
	}
}

// dependencyUp lê as séries de recorvia_dependency_up por dependência.
func dependencyUp(t *testing.T, m *telemetry.Metrics) map[string]float64 {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "recorvia_dependency_up" {
			continue
		}
		for _, s := range f.GetMetric() {
			for _, l := range s.GetLabel() {
				if l.GetName() == "dependency" {
					out[l.GetValue()] = s.GetGauge().GetValue()
				}
			}
		}
	}
	return out
}
