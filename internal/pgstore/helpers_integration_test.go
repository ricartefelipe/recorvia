//go:build integration

package pgstore_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/payments"
)

func customersFilter() customers.ListFilter { return customers.ListFilter{Limit: 100} }

// discrepancyVersion devolve a versão atual da divergência, a esperada pela
// resolução (If-Match): novas detecções geram versões novas.
func discrepancyVersion(t *testing.T, pay *payments.Service, tenantID, id uuid.UUID) int {
	t.Helper()
	d, err := pay.Discrepancy(context.Background(), tenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	return d.Version
}
