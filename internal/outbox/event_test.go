package outbox_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/outbox"
)

func TestEventNew(t *testing.T) {
	tenantID, aggID := uuid.New(), uuid.New()
	local := time.FixedZone("BRT", -3*60*60)
	at := time.Date(2026, time.November, 16, 21, 0, 0, 0, local)
	payload := map[string]any{"customer_id": aggID}

	e := outbox.New(tenantID, outbox.EventCustomerCreated, "customer", aggID, payload, at)

	if e.ID == uuid.Nil || e.ID.Version() != 7 {
		t.Fatalf("evento deve receber UUIDv7 próprio, obtido %s", e.ID)
	}
	if e.OccurredAt.Location() != time.UTC || !e.OccurredAt.Equal(at) {
		t.Errorf("instante deve ser o mesmo, em UTC: %v", e.OccurredAt)
	}
	if e.TenantID != tenantID || e.Type != "customer.created" || e.AggregateType != "customer" || e.AggregateID != aggID {
		t.Errorf("campos inesperados: %+v", e)
	}
	if got, ok := e.Payload.(map[string]any); !ok || got["customer_id"] != aggID {
		t.Errorf("payload alterado: %#v", e.Payload)
	}
	if !e.AvailableAt.IsZero() {
		t.Errorf("publicação deve ser imediata por padrão, AvailableAt=%v", e.AvailableAt)
	}
}

func TestEventNewDistinctIDs(t *testing.T) {
	at := time.Now()
	a := outbox.New(uuid.New(), outbox.EventInvoicePaid, "invoice", uuid.New(), nil, at)
	b := outbox.New(uuid.New(), outbox.EventInvoicePaid, "invoice", uuid.New(), nil, at)
	if a.ID == b.ID {
		t.Fatal("eventos distintos devem ter identificadores distintos")
	}
}
