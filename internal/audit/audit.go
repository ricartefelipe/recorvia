// Package audit define a trilha de auditoria imutável. As entradas são
// gravadas na mesma transação da ação auditada; nenhuma rotina altera ou
// remove entradas (o banco rejeita UPDATE e DELETE).
//
// Os detalhes não guardam valores pessoais (nome, documento, e-mail): apenas
// identificadores, nomes de campos alterados e valores operacionais. Assim, a
// anonimização de um cliente não exige reescrever a auditoria.
package audit

import (
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
)

// ActorType identifica quem executou a ação.
type ActorType string

const (
	// ActorUser é um membro autenticado do tenant.
	ActorUser ActorType = "user"
	// ActorAPICredential é uma integração máquina a máquina.
	ActorAPICredential ActorType = "api_credential" //nolint:gosec // nome do tipo de ator, não é credencial
	// ActorSystem é um processo interno (worker, agendador).
	ActorSystem ActorType = "system"
	// ActorCustomerPortal é um cliente do tenant no portal.
	ActorCustomerPortal ActorType = "customer_portal"
	// ActorProvider é um provedor externo (webhook).
	ActorProvider ActorType = "provider"
)

// Actor descreve o executor de uma ação.
type Actor struct {
	Type  ActorType
	ID    string
	Label string
}

// System cria um ator interno identificado pelo componente.
func System(component string) Actor {
	return Actor{Type: ActorSystem, ID: component, Label: component}
}

// User cria um ator usuário.
func User(id uuid.UUID, label string) Actor {
	return Actor{Type: ActorUser, ID: id.String(), Label: label}
}

// APICredential cria um ator credencial de API, identificado pelo ID da
// credencial e pelo nome dado pela organização.
func APICredential(id uuid.UUID, label string) Actor {
	return Actor{Type: ActorAPICredential, ID: id.String(), Label: label}
}

// CustomerPortal cria um ator cliente no portal, identificado pelo ID do
// cliente. O rótulo é fixo para não gravar dados pessoais na trilha.
func CustomerPortal(customerID uuid.UUID) Actor {
	return Actor{Type: ActorCustomerPortal, ID: customerID.String(), Label: "Portal do cliente"}
}

// Entry é uma entrada da trilha.
type Entry struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	OccurredAt   time.Time
	Actor        Actor
	Action       string
	ResourceType string
	ResourceID   string
	Details      map[string]any
}

// New cria uma entrada com identificador próprio.
func New(tenantID uuid.UUID, at time.Time, actor Actor, action, resourceType, resourceID string, details map[string]any) Entry {
	if details == nil {
		details = map[string]any{}
	}
	return Entry{
		ID: ids.New(), TenantID: tenantID, OccurredAt: at.UTC(), Actor: actor,
		Action: action, ResourceType: resourceType, ResourceID: resourceID, Details: details,
	}
}
