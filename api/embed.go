// Package api embute o contrato OpenAPI do Recorvia, usado pelo teste de
// contrato dos handlers e servido em /api/openapi.yaml.
package api

import _ "embed"

// OpenAPI é o conteúdo de api/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
