package audit_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
)

func TestNewEntry(t *testing.T) {
	tenantID := uuid.New()
	sp := time.FixedZone("BRT", -3*60*60)
	at := time.Date(2026, time.November, 16, 9, 30, 0, 0, sp)
	actor := audit.User(uuid.New(), "Ana")

	e := audit.New(tenantID, at, actor, "customer.created", "customer", "c-1", map[string]any{"kind": "company"})

	if e.ID == uuid.Nil {
		t.Fatal("a entrada deve receber identificador próprio")
	}
	if e.ID.Version() != 7 {
		t.Errorf("identificador deve ser UUIDv7, obtido v%d", e.ID.Version())
	}
	if e.OccurredAt.Location() != time.UTC {
		t.Errorf("instante deve ser normalizado para UTC, obtido %v", e.OccurredAt.Location())
	}
	if !e.OccurredAt.Equal(at) {
		t.Errorf("instante alterado: %v, esperado %v", e.OccurredAt, at)
	}
	if e.TenantID != tenantID || e.Actor != actor || e.Action != "customer.created" ||
		e.ResourceType != "customer" || e.ResourceID != "c-1" || e.Details["kind"] != "company" {
		t.Errorf("campos inesperados: %+v", e)
	}
}

func TestNewEntryDetailsNeverNil(t *testing.T) {
	e := audit.New(uuid.New(), time.Now(), audit.System("worker"), "x", "y", "z", nil)
	if e.Details == nil {
		t.Fatal("detalhes nulos devem virar mapa vazio")
	}
	if len(e.Details) != 0 {
		t.Fatalf("detalhes deveriam estar vazios: %v", e.Details)
	}
}

func TestNewEntryUniqueIDs(t *testing.T) {
	seen := map[uuid.UUID]bool{}
	for range 100 {
		e := audit.New(uuid.New(), time.Now(), audit.System("worker"), "x", "y", "z", nil)
		if seen[e.ID] {
			t.Fatalf("identificador repetido: %s", e.ID)
		}
		seen[e.ID] = true
	}
}

func TestActorConstructors(t *testing.T) {
	id := uuid.New()
	cases := []struct {
		name string
		got  audit.Actor
		want audit.Actor
	}{
		{"usuário", audit.User(id, "Ana Souza"), audit.Actor{Type: audit.ActorUser, ID: id.String(), Label: "Ana Souza"}},
		{"sistema", audit.System("scheduler"), audit.Actor{Type: audit.ActorSystem, ID: "scheduler", Label: "scheduler"}},
		{"credencial de API", audit.APICredential(id, "ERP interno"), audit.Actor{Type: audit.ActorAPICredential, ID: id.String(), Label: "ERP interno"}},
		{"portal do cliente", audit.CustomerPortal(id), audit.Actor{Type: audit.ActorCustomerPortal, ID: id.String(), Label: "Portal do cliente"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("ator = %+v, esperado %+v", tc.got, tc.want)
			}
		})
	}
}
