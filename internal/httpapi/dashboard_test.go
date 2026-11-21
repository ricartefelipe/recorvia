package httpapi

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/ricartefelipe/recorvia/api"
	"github.com/ricartefelipe/recorvia/internal/reporting"
)

func indicatorKeys() []string {
	keys := make([]string, 0, len(reporting.Indicators))
	for _, i := range reporting.Indicators {
		keys = append(keys, i.Key)
	}
	slices.Sort(keys)
	return keys
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func dashboardSchema(t *testing.T) *openapi3.Schema {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref, ok := doc.Components.Schemas["Dashboard"]
	if !ok || ref.Value == nil {
		t.Fatal("schema Dashboard ausente do contrato")
	}
	return ref.Value
}

// Cada indicador devolvido pelo painel tem fórmula e base temporal, com a
// mesma chave do campo na resposta, no backend e no contrato: a interface usa
// essas chaves para exibir a dica de cada card e a tabela "Como calculamos".
func TestDashboardEveryIndicatorHasDefinitionAndBasis(t *testing.T) {
	d, err := reporting.Compute("2026-11", time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC), reporting.Raw{})
	if err != nil {
		t.Fatal(err)
	}
	want := indicatorKeys()
	if got := sortedKeys(dashboardIndicators(d)); !slices.Equal(got, want) {
		t.Fatalf("indicadores da resposta = %v\nindicadores declarados = %v", got, want)
	}
	if got := sortedKeys(d.Definitions); !slices.Equal(got, want) {
		t.Errorf("definições = %v, esperado %v", got, want)
	}
	if got := sortedKeys(d.Bases); !slices.Equal(got, want) {
		t.Errorf("bases = %v, esperado %v", got, want)
	}
	for _, i := range reporting.Indicators {
		if len([]rune(i.Definition)) < 40 {
			t.Errorf("definição de %s curta demais: %q", i.Key, i.Definition)
		}
		switch i.Basis {
		case reporting.BasisMonth, reporting.BasisMonthEnd, reporting.BasisCurrent:
		default:
			t.Errorf("base de %s inválida: %q", i.Key, i.Basis)
		}
	}

	schema := dashboardSchema(t)
	for _, k := range want {
		if !slices.Contains(schema.Required, k) {
			t.Errorf("contrato: indicador %s fora de required", k)
		}
	}
	for _, obj := range []string{"definitions", "bases"} {
		p := schema.Properties[obj]
		if p == nil || p.Value == nil {
			t.Fatalf("contrato: %s ausente", obj)
		}
		req := slices.Clone(p.Value.Required)
		slices.Sort(req)
		if !slices.Equal(req, want) || !slices.Equal(sortedKeys(p.Value.Properties), want) {
			t.Errorf("contrato: chaves de %s = %v / %v, esperado %v", obj, req, sortedKeys(p.Value.Properties), want)
		}
	}
}

// A resposta do painel segue o schema do contrato.
func TestDashboardDTOConformsToContract(t *testing.T) {
	d, err := reporting.Compute("2026-10", time.Date(2026, 11, 1, 2, 59, 59, 999999000, time.UTC), reporting.Raw{Billed: 100, OpenDiscrepancies: 2})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(dashboardDTO(d))
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if err := dashboardSchema(t).VisitJSON(v); err != nil {
		t.Fatalf("resposta fora do schema Dashboard: %v\n%s", err, b)
	}
	m := v.(map[string]any)
	if m["position_at"] != "2026-11-01T02:59:59.999999Z" || m["open_discrepancies"] != float64(2) {
		t.Errorf("posição ou divergências: %v %v", m["position_at"], m["open_discrepancies"])
	}
}
