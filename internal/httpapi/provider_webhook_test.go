package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/payments"
)

// A rota de webhooks recebidos de provedores recusa com 413 o corpo acima de
// payments.MaxWebhookBody, antes de autenticar ou persistir (RF-11), e conta
// o resultado na métrica sem rótulo derivado da URL. O corpo no limite exato
// passa pelo limite e só então é validado.
func TestProviderWebhookRejectsOversizedBody(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		size     int
		status   int
		code     string
		outcome  string
		label    string
	}{
		{"um byte acima do limite", "simulator", payments.MaxWebhookBody + 1, http.StatusRequestEntityTooLarge, "payload_too_large", "too_large", "simulator"},
		{"muito acima do limite", "asaas", 4 * payments.MaxWebhookBody, http.StatusRequestEntityTooLarge, "payload_too_large", "too_large", "asaas"},
		{"provedor desconhecido", "outro", payments.MaxWebhookBody + 1, http.StatusRequestEntityTooLarge, "payload_too_large", "too_large",
			telemetry.OtherProvider},
		{"no limite exato", "simulator", payments.MaxWebhookBody, http.StatusUnprocessableEntity, "webhook_invalid", "invalid", "simulator"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			metrics := telemetry.NewMetrics("api")
			_, router := NewRouter(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.System{}, Metrics: metrics})
			body := bytes.Repeat([]byte("x"), c.size)
			req := httptest.NewRequest(http.MethodPost, "/webhooks/providers/"+c.provider+"/"+strings.Repeat("t", 32), bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != c.status || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/problem+json") {
				t.Fatalf("status %d (%s), esperado %d", rec.Code, rec.Header().Get("Content-Type"), c.status)
			}
			var p problem
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Code != c.code || p.Status != c.status {
				t.Fatalf("problema: %+v %v", p, err)
			}
			if got := testutil.ToFloat64(metrics.WebhooksReceived.WithLabelValues(c.label, c.outcome)); got != 1 {
				t.Fatalf("métrica %s/%s = %v", c.label, c.outcome, got)
			}
		})
	}
}
