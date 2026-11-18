package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/api"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

func contractSchema(t *testing.T, name string) *openapi3.Schema {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := doc.Components.Schemas[name]
	if !ok || ref.Value == nil {
		t.Fatalf("schema %s ausente do contrato", name)
	}
	return ref.Value
}

// assertMatchesSchema valida o DTO serializado contra o schema do contrato
// e exige que todo campo devolvido esteja declarado nele.
func assertMatchesSchema(t *testing.T, schema string, dto map[string]any) {
	t.Helper()
	s := contractSchema(t, schema)
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := s.VisitJSON(decoded); err != nil {
		t.Errorf("%s fora do contrato: %v", schema, err)
	}
	for k := range dto {
		if _, ok := s.Properties[k]; !ok {
			t.Errorf("%s: campo %q não declarado no contrato", schema, k)
		}
	}
}

func TestReceivablesDTOsMatchContract(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	user := uuid.New()
	charge := uuid.New()
	brl := func(v int64) money.Money { return money.New(v, money.BRL) }
	cases := []struct {
		name, schema string
		dto          map[string]any
	}{
		{"reembolso solicitado", "Refund", refundDTO(payments.Refund{ID: uuid.New(), ChargeID: charge, Status: payments.RefundRequested,
			Amount: brl(3000), Reason: "Cancelamento do contrato", CreatedAt: now})},
		{"reembolso efetivado", "Refund", refundDTO(payments.Refund{ID: uuid.New(), ChargeID: charge, Status: payments.RefundSucceeded,
			Amount: brl(3000), Reason: "Cancelamento do contrato", ProviderRefundID: "rf_1", CreatedAt: now, CompletedAt: &later})},
		{"disputa aberta", "Dispute", disputeDTO(pgstore.DisputeWithHistory{Dispute: payments.Dispute{ID: uuid.New(), ChargeID: charge,
			Status: payments.DisputeOpened, Amount: brl(5000), OpenedAt: now}})},
		{"disputa perdida", "Dispute", disputeDTO(pgstore.DisputeWithHistory{Dispute: payments.Dispute{ID: uuid.New(), ChargeID: charge,
			Status: payments.DisputeLost, Amount: brl(5000), OpenedAt: now, ClosedAt: &later}})},
		{"execução em andamento", "ReconciliationRun", runDTO(payments.ReconciliationRun{ID: uuid.New(), Provider: "simulator",
			Status: payments.RunRunning, WindowStart: now.Add(-24 * time.Hour), WindowEnd: now, StartedAt: now})},
		{"execução concluída", "ReconciliationRun", runDTO(payments.ReconciliationRun{ID: uuid.New(), Provider: "asaas",
			Status: payments.RunCompleted, WindowStart: now.Add(-24 * time.Hour), WindowEnd: now, CheckedCount: 4, DiscrepancyCount: 1,
			Error: "Execução parcial", StartedAt: now, FinishedAt: &later})},
		{"divergência aberta sem retratos", "Discrepancy", discrepancyDTO(payments.Discrepancy{ID: uuid.New(), RunID: uuid.New(),
			Kind: payments.DiscrepancyMissingLocal, Status: payments.DiscrepancyOpen, ProviderChargeID: "ch_9", CreatedAt: now})},
		{"divergência resolvida", "Discrepancy", discrepancyDTO(payments.Discrepancy{ID: uuid.New(), RunID: uuid.New(),
			Kind: payments.DiscrepancyFeeMismatch, Status: payments.DiscrepancyResolved, ChargeID: &charge,
			LocalValue: map[string]any{"fee": 299}, ProviderValue: map[string]any{"fee": 500}, Resolution: payments.ResolutionAcceptProvider,
			Justification: "Conferido com o extrato do provedor.", ResolvedBy: &user, ResolvedAt: &later, CreatedAt: now})},
		{"divergência resolvida automaticamente", "Discrepancy", discrepancyDTO(payments.Discrepancy{ID: uuid.New(), RunID: uuid.New(),
			Kind: payments.DiscrepancyUnknownOperation, Status: payments.DiscrepancyResolved, Resolution: payments.ResolutionAutoResolved,
			Justification: "Divergência não constatada na execução.", ResolvedAt: &later, CreatedAt: now})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertMatchesSchema(t, tc.schema, tc.dto) })
	}
}

// Os handlers validam caminho, filtros e corpo antes de consultar o banco
// ou o provedor.
func TestReceivablesHandlersRejectInvalidInput(t *testing.T) {
	a, _ := NewRouter(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.System{}})
	p := Principal{TenantID: uuid.New(), Role: tenancy.RoleOwner}
	id := uuid.NewString()
	cases := []struct {
		name, method, target, body string
		params                     map[string]string
		handler                    http.HandlerFunc
		status                     int
		code                       string
	}{
		{"reembolso com cobrança inválida", http.MethodPost, "/", `{"amount":1,"reason":"abc"}`, map[string]string{"id": "x"}, a.refundCharge, 404, "not_found"},
		{"reembolso com campo desconhecido", http.MethodPost, "/", `{"amount":1,"reason":"abc","fee":1}`, map[string]string{"id": id}, a.refundCharge, 422, "invalid_json"},
		{"reembolso sem corpo", http.MethodPost, "/", ``, map[string]string{"id": id}, a.refundCharge, 422, "empty_body"},
		{"reembolso com valor textual", http.MethodPost, "/", `{"amount":"10","reason":"abc"}`, map[string]string{"id": id}, a.refundCharge, 422, "invalid_json"},
		{"limite de reembolsos inválido", http.MethodGet, "/?limit=0", ``, nil, a.listRefunds, 422, "invalid_limit"},
		{"situação de divergência desconhecida", http.MethodGet, "/?status=auto_resolved", ``, nil, a.listDiscrepancies, 422, "invalid_field"},
		{"tipo de divergência desconhecido", http.MethodGet, "/?kind=duplicate", ``, nil, a.listDiscrepancies, 422, "invalid_field"},
		{"divergência com identificador inválido", http.MethodGet, "/", ``, map[string]string{"id": "123"}, a.getDiscrepancy, 404, "not_found"},
		{"resolução com identificador inválido", http.MethodPost, "/", `{}`, map[string]string{"id": "123"}, a.resolveDiscrepancy, 404, "not_found"},
		{"resolução com campo desconhecido", http.MethodPost, "/", `{"resolution":"dismiss","justification":"conferido no extrato","x":1}`,
			map[string]string{"id": id}, a.resolveDiscrepancy, 422, "invalid_json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rc := chi.NewRouteContext()
			for k, v := range tc.params {
				rc.URLParams.Add(k, v)
			}
			req = req.WithContext(context.WithValue(withPrincipal(req.Context(), p), chi.RouteCtxKey, rc))
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tc.status || body["code"] != tc.code {
				t.Fatalf("status %d código %v, esperado %d %s", rec.Code, body["code"], tc.status, tc.code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Fatalf("content-type %q", ct)
			}
		})
	}
}
