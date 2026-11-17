package customers

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// ListFilter define filtros e ordenação permitidos (allowlist).
type ListFilter struct {
	Status Status
	Search string
	Sort   string // "name", "-name", "created_at", "-created_at"
	Limit  int
	Cursor string
}

// SortAllowed valida a ordenação solicitada.
func SortAllowed(s string) bool {
	switch s {
	case "", "name", "-name", "created_at", "-created_at":
		return true
	}
	return false
}

// Page é uma página de resultados.
type Page struct {
	Items      []Customer
	NextCursor string
}

// Details reúne o cliente com contatos e endereço.
type Details struct {
	Customer
	Contacts []Contact
	Address  *Address
}

// Tx são as operações dentro da transação do tenant.
type Tx interface {
	Insert(ctx context.Context, c Customer) error
	Get(ctx context.Context, id uuid.UUID, forUpdate bool) (Customer, error)
	Update(ctx context.Context, c Customer) error
	List(ctx context.Context, f ListFilter) (Page, error)
	Contacts(ctx context.Context, customerID uuid.UUID) ([]Contact, error)
	InsertContact(ctx context.Context, c Contact) error
	DeleteContact(ctx context.Context, customerID, contactID uuid.UUID) error
	Address(ctx context.Context, customerID uuid.UUID) (*Address, error)
	UpsertAddress(ctx context.Context, customerID uuid.UUID, a Address) error
	HasOpenObligations(ctx context.Context, customerID uuid.UUID) (bool, error)
	Audit(ctx context.Context, e audit.Entry) error
	Emit(ctx context.Context, e outbox.Event) error
}

// Store abre transações do tenant.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
}

// Service implementa os casos de uso de clientes.
type Service struct {
	store Store
	clock clock.Clock
}

// NewService cria o serviço.
func NewService(store Store, c clock.Clock) *Service { return &Service{store: store, clock: c} }

// Create cadastra um cliente.
func (s *Service) Create(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, in Input) (Customer, error) {
	now := s.clock.Now()
	c, err := New(ids.New(), tenantID, in, now)
	if err != nil {
		return Customer{}, err
	}
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if err := tx.Insert(ctx, c); err != nil {
			return err
		}
		if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventCustomerCreated, "customer", c.ID, map[string]any{"customer_id": c.ID}, now)); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "customer.created", "customer", c.ID.String(),
			map[string]any{"kind": c.Kind}))
	})
	return c, err
}

// Get retorna o cliente com contatos e endereço.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (Details, error) {
	var d Details
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		c, err := tx.Get(ctx, id, false)
		if err != nil {
			return err
		}
		contacts, err := tx.Contacts(ctx, id)
		if err != nil {
			return err
		}
		addr, err := tx.Address(ctx, id)
		if err != nil {
			return err
		}
		d = Details{Customer: c, Contacts: contacts, Address: addr}
		return nil
	})
	return d, err
}

// List lista clientes com filtros em allowlist e paginação por cursor.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, f ListFilter) (Page, error) {
	if !SortAllowed(f.Sort) {
		return Page{}, apperr.Invalid("invalid_sort", "Ordenação não permitida.", apperr.FieldError{Field: "sort", Message: "Use name, -name, created_at ou -created_at."})
	}
	if f.Status != "" && f.Status != Active && f.Status != Archived {
		return Page{}, apperr.Invalid("invalid_filter", "Filtro de situação inválido.", apperr.FieldError{Field: "status", Message: "Use active ou archived."})
	}
	if len([]rune(f.Search)) > 100 {
		return Page{}, apperr.Invalid("invalid_filter", "Busca muito longa.", apperr.FieldError{Field: "q", Message: "Use no máximo 100 caracteres."})
	}
	var p Page
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		p, err = tx.List(ctx, f)
		return err
	})
	return p, err
}

// Update altera o cliente com concorrência otimista.
func (s *Service) Update(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, in Input, expectedVersion int) (Customer, error) {
	var c Customer
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if c, err = tx.Get(ctx, id, true); err != nil {
			return err
		}
		if c.Version != expectedVersion {
			return apperr.PreconditionFailed("version_mismatch", "O cliente foi alterado por outra pessoa. Recarregue e tente novamente.")
		}
		if c.Status == Archived {
			return apperr.Conflict("customer_archived", "Cliente arquivado não pode ser alterado.")
		}
		changed, err := c.Apply(in)
		if err != nil {
			return err
		}
		if len(changed) == 0 {
			return nil
		}
		if err := tx.Update(ctx, c); err != nil {
			return err
		}
		c.Version++
		now := s.clock.Now()
		if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventCustomerUpdated, "customer", c.ID, map[string]any{"customer_id": c.ID, "fields": changed}, now)); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "customer.updated", "customer", c.ID.String(), map[string]any{"fields": changed}))
	})
	return c, err
}

// Archive arquiva o cliente quando não há assinaturas ativas nem faturas em aberto.
func (s *Service) Archive(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID) (Customer, error) {
	var c Customer
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if c, err = tx.Get(ctx, id, true); err != nil {
			return err
		}
		if c.Status == Archived {
			return nil
		}
		open, err := tx.HasOpenObligations(ctx, id)
		if err != nil {
			return err
		}
		if open {
			return apperr.Conflict("customer_has_obligations", "O cliente possui assinaturas ativas ou faturas em aberto.")
		}
		c.Status = Archived
		if err := tx.Update(ctx, c); err != nil {
			return err
		}
		c.Version++
		return tx.Audit(ctx, audit.New(tenantID, s.clock.Now(), actor, "customer.archived", "customer", id.String(), nil))
	})
	return c, err
}

// AddContact adiciona um contato.
func (s *Service) AddContact(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, customerID uuid.UUID, in ContactInput) (Contact, error) {
	now := s.clock.Now()
	ct, err := NewContact(ids.New(), customerID, in, now)
	if err != nil {
		return Contact{}, err
	}
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if _, err := tx.Get(ctx, customerID, true); err != nil {
			return err
		}
		existing, err := tx.Contacts(ctx, customerID)
		if err != nil {
			return err
		}
		if len(existing) >= 20 {
			return apperr.Conflict("contact_limit", "O cliente já possui o máximo de 20 contatos.")
		}
		if err := tx.InsertContact(ctx, ct); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "customer.contact_added", "customer", customerID.String(),
			map[string]any{"contact_id": ct.ID, "receives_billing_email": ct.ReceivesBillingEmail}))
	})
	return ct, err
}

// RemoveContact remove um contato.
func (s *Service) RemoveContact(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, customerID, contactID uuid.UUID) error {
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if err := tx.DeleteContact(ctx, customerID, contactID); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, s.clock.Now(), actor, "customer.contact_removed", "customer", customerID.String(),
			map[string]any{"contact_id": contactID}))
	})
}

// SetAddress grava o endereço de cobrança.
func (s *Service) SetAddress(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, customerID uuid.UUID, a Address) (Address, error) {
	a, err := ValidateAddress(a)
	if err != nil {
		return Address{}, err
	}
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if _, err := tx.Get(ctx, customerID, true); err != nil {
			return err
		}
		if err := tx.UpsertAddress(ctx, customerID, a); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, s.clock.Now(), actor, "customer.address_updated", "customer", customerID.String(), nil))
	})
	return a, err
}

// Snapshot é a cópia dos dados do cliente gravada na fatura emitida.
type Snapshot struct {
	CustomerID     uuid.UUID `json:"customer_id"`
	Name           string    `json:"name"`
	LegalName      string    `json:"legal_name,omitempty"`
	DocumentType   string    `json:"document_type"`
	DocumentNumber string    `json:"document_number"`
	Email          string    `json:"email,omitempty"`
	Address        *Address  `json:"address,omitempty"`
	CapturedAt     time.Time `json:"captured_at"`
}

// SnapshotOf monta o snapshot do cliente.
func SnapshotOf(c Customer, a *Address, at time.Time) Snapshot {
	return Snapshot{CustomerID: c.ID, Name: c.Name, LegalName: c.LegalName, DocumentType: string(c.DocumentType),
		DocumentNumber: c.DocumentNumber, Email: c.Email, Address: a, CapturedAt: at}
}
