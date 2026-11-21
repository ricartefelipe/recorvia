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
	// HasPendingBilling informa se ainda há faturamento do cliente por
	// acontecer, mesmo sem assinatura ativa nem fatura em aberto: assinatura
	// com fechamento agendado (como o consumo final de uma assinatura
	// cancelada), item pendente sem fatura, consumo atrasado ou sem agregado
	// por corrigir, ou período fechado ainda dentro da tolerância de atraso
	// da métrica em now.
	HasPendingBilling(ctx context.Context, customerID uuid.UUID, now time.Time) (bool, error)
	// Anonymize grava o cadastro já anonimizado (dados neutros, arquivado e
	// com o instante da anonimização) e devolve o cliente como ficou gravado,
	// com a versão e o instante de atualização novos; depois dele o banco
	// recusa qualquer alteração do cliente.
	Anonymize(ctx context.Context, c Customer) (Customer, error)
	// ErasePersonalData remove, na mesma transação, os dados pessoais
	// guardados fora do cadastro do cliente já anonimizado e encerra o
	// acesso ao portal; devolve o que foi removido.
	ErasePersonalData(ctx context.Context, customerID uuid.UUID, at time.Time) (Erasure, error)
	// AnonymizationProviderRefs relê, da auditoria da anonimização, os
	// identificadores do cliente nos provedores de pagamento removidos do
	// cadastro (vazio se não havia nenhum).
	AnonymizationProviderRefs(ctx context.Context, customerID uuid.UUID) ([]ProviderRef, error)
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

// errAnonymized recusa a regravação de dados pessoais num cliente anonimizado.
var errAnonymized = apperr.Conflict("customer_anonymized", "Cliente anonimizado não pode ser alterado.")

// errPendingBilling recusa a anonimização enquanto ainda há faturamento do
// cliente por acontecer.
var errPendingBilling = apperr.Conflict("customer_has_obligations",
	"O cliente ainda tem faturamento pendente: fechamento de assinatura, itens ou correções de consumo a faturar, "+
		"ou consumo dentro da tolerância de atraso da métrica.")

// AuditProviderCustomers é a chave dos detalhes de customer.anonymized com os
// identificadores do cliente nos provedores de pagamento.
const AuditProviderCustomers = "provider_customers"

// ProviderRef é o identificador do cliente num provedor de pagamento. É um
// identificador opaco emitido pelo provedor, não um dado cadastral.
type ProviderRef struct {
	Provider           string `json:"provider"`
	ProviderCustomerID string `json:"provider_customer_id"`
}

// Erasure descreve os dados removidos fora do cadastro na anonimização.
type Erasure struct {
	// Contacts é o número de contatos excluídos (com as preferências de
	// e-mail de cobrança).
	Contacts int
	// Address informa se havia endereço de cobrança.
	Address bool
	// ProviderRefs são os identificadores do cliente nos provedores de
	// pagamento, excluídos do cadastro e registrados na auditoria da
	// anonimização. O cadastro no provedor não é alterado.
	ProviderRefs []ProviderRef
	// PortalLinksRevoked é o número de links do portal revogados (as sessões
	// abertas com eles terminam na requisição seguinte).
	PortalLinksRevoked int
	// Notifications é o número de notificações excluídas, com destinatários,
	// assunto, conteúdo e histórico de envio.
	Notifications int
	// ProviderWebhookPayloads é o número de webhooks recebidos do provedor
	// cujo corpo foi esvaziado.
	ProviderWebhookPayloads int
}

// Anonymization é o resultado da anonimização de um cliente.
type Anonymization struct {
	Customer Customer
	// AlreadyAnonymized indica que o cliente já estava anonimizado: nada foi
	// alterado nem auditado de novo. Erasure traz então somente os
	// identificadores nos provedores registrados na anonimização.
	AlreadyAnonymized bool
	Erasure           Erasure
}

// Anonymize substitui, de forma irreversível, os dados pessoais de um cliente
// sem assinatura ativa, fatura em aberto nem faturamento pendente (consumo
// final, itens ou correções a faturar, tolerância de consumo atrasado ainda
// aberta): o cadastro recebe valores neutros e é arquivado; contatos,
// endereço, identificadores nos provedores e notificações são excluídos; os
// links do portal são revogados e o corpo dos webhooks recebidos das
// cobranças do cliente é esvaziado. Registros financeiros (faturas emitidas e
// a cópia do cadastro nelas, pagamentos, créditos, ledger) e a trilha de
// auditoria não são alterados (ADR-061).
//
// A operação é idempotente: repetida para um cliente já anonimizado, devolve o
// estado atual sem efeito, com os identificadores nos provedores registrados
// na auditoria da anonimização (a resposta que os entrega pode se perder
// depois da confirmação da transação). A justificativa é obrigatória, não
// pode conter dados pessoais do cliente e fica na auditoria.
func (s *Service) Anonymize(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, reason string) (Anonymization, error) {
	// O formato da justificativa é conferido antes de qualquer leitura; o
	// conteúdo é comparado com os dados do cliente dentro da transação.
	if _, err := ValidateAnonymizationReason(reason, Customer{}); err != nil {
		return Anonymization{}, err
	}
	var out Anonymization
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		out = Anonymization{}
		c, err := tx.Get(ctx, id, true)
		if err != nil {
			return err
		}
		if c.Anonymized() {
			refs, err := tx.AnonymizationProviderRefs(ctx, id)
			if err != nil {
				return err
			}
			out = Anonymization{Customer: c, AlreadyAnonymized: true, Erasure: Erasure{ProviderRefs: refs}}
			return nil
		}
		reason, err := ValidateAnonymizationReason(reason, c)
		if err != nil {
			return err
		}
		open, err := tx.HasOpenObligations(ctx, id)
		if err != nil {
			return err
		}
		if open {
			return apperr.Conflict("customer_has_obligations", "O cliente possui assinaturas ativas ou faturas em aberto.")
		}
		now := s.clock.Now()
		// Com o cliente bloqueado: o faturamento e a ingestão de consumo
		// esperam esta transação antes de gravar fatura ou evento do cliente.
		pending, err := tx.HasPendingBilling(ctx, id, now)
		if err != nil {
			return err
		}
		if pending {
			return errPendingBilling
		}
		previous := c.Status
		fields := c.Anonymize(now)
		if c, err = tx.Anonymize(ctx, c); err != nil {
			return err
		}
		erased, err := tx.ErasePersonalData(ctx, id, now)
		if err != nil {
			return err
		}
		if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventCustomerUpdated, "customer", c.ID,
			map[string]any{"customer_id": c.ID, "fields": fields, "anonymized": true}, now)); err != nil {
			return err
		}
		providers := make([]string, 0, len(erased.ProviderRefs))
		for _, r := range erased.ProviderRefs {
			providers = append(providers, r.Provider)
		}
		out = Anonymization{Customer: c, Erasure: erased}
		// A auditoria guarda a justificativa, os nomes dos campos, as
		// contagens e os identificadores do cliente nos provedores (opacos,
		// emitidos pelo provedor); nenhum valor pessoal. Os identificadores
		// ficam aqui, na mesma transação, porque já não existem no cadastro e
		// a resposta que os entrega pode se perder.
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "customer.anonymized", "customer", id.String(), map[string]any{
			"reason": reason, "fields": fields, "previous_status": previous, "contacts_removed": erased.Contacts,
			"address_removed": erased.Address, "providers": providers, AuditProviderCustomers: append([]ProviderRef{}, erased.ProviderRefs...),
			"portal_links_revoked": erased.PortalLinksRevoked, "notifications_removed": erased.Notifications,
			"provider_webhook_payloads_cleared": erased.ProviderWebhookPayloads}))
	})
	if err != nil {
		return Anonymization{}, err
	}
	return out, nil
}

// AddContact adiciona um contato.
func (s *Service) AddContact(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, customerID uuid.UUID, in ContactInput) (Contact, error) {
	now := s.clock.Now()
	ct, err := NewContact(ids.New(), customerID, in, now)
	if err != nil {
		return Contact{}, err
	}
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		c, err := tx.Get(ctx, customerID, true)
		if err != nil {
			return err
		}
		if c.Anonymized() {
			return errAnonymized
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
		c, err := tx.Get(ctx, customerID, true)
		if err != nil {
			return err
		}
		if c.Anonymized() {
			return errAnonymized
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
