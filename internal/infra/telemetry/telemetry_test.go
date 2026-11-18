package telemetry

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func exposition(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTotalsCounterIsMonotonic(t *testing.T) {
	m := NewMetrics("worker")
	if out := exposition(t, m); strings.Contains(out, "recorvia_refunds_total{") {
		t.Fatal("sem leitura não há séries")
	}
	m.Refunds.Set("succeeded", 5)
	m.Refunds.Set("failed", 2)
	m.Refunds.Set("succeeded", 3) // leitura menor é ignorada
	m.Discrepancies.WithLabelValues("fee_mismatch").Set(4)
	out := exposition(t, m)
	for _, want := range []string{
		"# TYPE recorvia_refunds_total counter",
		`recorvia_refunds_total{outcome="failed",service="worker"} 2`,
		`recorvia_refunds_total{outcome="succeeded",service="worker"} 5`,
		`recorvia_reconciliation_open_discrepancies{kind="fee_mismatch",service="worker"} 4`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposição sem %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tenant") {
		t.Fatal("métricas sem rótulos de tenant")
	}
}
