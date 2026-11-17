package api

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// TestOpenAPIIsValid valida a estrutura do contrato (referências, schemas e
// parâmetros de caminho).
func TestOpenAPIIsValid(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(OpenAPI)
	if err != nil {
		t.Fatalf("carregar contrato: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("contrato inválido: %v", err)
	}
	ops := 0
	for _, item := range doc.Paths.Map() {
		ops += len(item.Operations())
	}
	if ops < 100 {
		t.Fatalf("contrato com poucas operações: %d", ops)
	}
	seen := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.OperationID == "" {
				t.Errorf("%s %s sem operationId", method, path)
			}
			if seen[op.OperationID] {
				t.Errorf("operationId duplicado: %s", op.OperationID)
			}
			seen[op.OperationID] = true
		}
	}
}
