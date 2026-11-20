package billing

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// ListFilter define filtros e ordenação permitidos.
type ListFilter struct {
	Status         Status
	CustomerID     *uuid.UUID
	SubscriptionID *uuid.UUID
	IssuedFrom     *time.Time
	IssuedTo       *time.Time
	Overdue        bool
	Now            time.Time
	Limit          int
	Cursor         string
}

// Page é uma página de faturas.
type Page struct {
	Items      []Invoice
	NextCursor string
}

// HistoryEntry é uma transição persistida.
type HistoryEntry struct {
	ID         uuid.UUID
	FromStatus *Status
	ToStatus   Status
	OccurredAt time.Time
	ActorType  string
	ActorID    string
	Reason     string
	Details    map[string]any
}

// Tx são as operações de consulta e manutenção de faturas.
type Tx interface {
	TenantSettings(ctx context.Context) (TenantSettings, error)
	Invoice(ctx context.Context, id uuid.UUID, forUpdate bool) (Invoice, error)
	UpdateInvoiceProgress(ctx context.Context, inv *Invoice) error
	List(ctx context.Context, f ListFilter) (Page, error)
	StatusHistory(ctx context.Context, invoiceID uuid.UUID) ([]HistoryEntry, error)
	InsertStatusHistory(ctx context.Context, invoiceID uuid.UUID, actor audit.Actor, changes []StatusChange) error
	ReverseLedger(ctx context.Context, originalKey string, description string, at time.Time) error
	PostLedger(ctx context.Context, t ledger.Transaction) error
	ReturnCustomerCredit(ctx context.Context, customerID, invoiceID uuid.UUID, amount int64, at time.Time) error
	GrantCustomerCredit(ctx context.Context, customerID uuid.UUID, amount money.Money, reason string, by *uuid.UUID, requestKey string) (uuid.UUID, bool, error)
	Audit(ctx context.Context, e audit.Entry) error
	Emit(ctx context.Context, e outbox.Event) error
}

// Store abre transações do tenant.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
}

// Service implementa consultas e correções de faturas.
type Service struct {
	store  Store
	engine *Engine
	clock  clock.Clock
}

// NewService cria o serviço.
func NewService(store Store, engine *Engine, c clock.Clock) *Service {
	return &Service{store: store, engine: engine, clock: c}
}

// Details reúne fatura e histórico.
type Details struct {
	Invoice
	History []HistoryEntry
	// Location é o fuso do tenant para exibição de datas civis.
	Location *time.Location
}

// Get retorna a fatura com itens e histórico.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (Details, error) {
	var d Details
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		inv, err := tx.Invoice(ctx, id, false)
		if err != nil {
			return err
		}
		h, err := tx.StatusHistory(ctx, id)
		if err != nil {
			return err
		}
		st, err := tx.TenantSettings(ctx)
		if err != nil {
			return err
		}
		d = Details{Invoice: inv, History: h, Location: st.Location}
		return nil
	})
	return d, err
}

// List lista faturas.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, f ListFilter) (Page, error) {
	if f.Status != "" {
		if _, ok := transitions[f.Status]; !ok {
			return Page{}, apperr.Invalid("invalid_filter", "Filtro de situação inválido.", apperr.FieldError{Field: "status", Message: "Situação desconhecida."})
		}
	}
	if f.IssuedFrom != nil && f.IssuedTo != nil && f.IssuedTo.Before(*f.IssuedFrom) {
		return Page{}, apperr.Invalid("invalid_filter", "Intervalo de datas inválido.", apperr.FieldError{Field: "issued_to", Message: "A data final deve ser posterior à inicial."})
	}
	f.Now = s.clock.Now()
	var p Page
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		p, err = tx.List(ctx, f)
		return err
	})
	return p, err
}

// Location retorna o fuso do tenant.
func (s *Service) Location(ctx context.Context, tenantID uuid.UUID) (*time.Location, error) {
	var loc *time.Location
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		st, err := tx.TenantSettings(ctx)
		loc = st.Location
		return err
	})
	return loc, err
}

// RunNow executa o motor de recorrência para o tenant imediatamente.
func (s *Service) RunNow(ctx context.Context, tenantID uuid.UUID, actor audit.Actor) (RunResult, error) {
	return s.engine.RunTenant(ctx, tenantID, actor)
}

func (s *Service) mutate(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, expectedVersion int,
	fn func(tx Tx, inv *Invoice, now time.Time) error) (Invoice, error) {
	var inv Invoice
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if inv, err = tx.Invoice(ctx, id, true); err != nil {
			return err
		}
		if expectedVersion != 0 && inv.Version != expectedVersion {
			return apperr.PreconditionFailed("version_mismatch", "A fatura foi alterada por outra pessoa. Recarregue e tente novamente.")
		}
		now := s.clock.Now()
		if err := fn(tx, &inv, now); err != nil {
			return err
		}
		if err := tx.UpdateInvoiceProgress(ctx, &inv); err != nil {
			return err
		}
		inv.Version++
		return tx.InsertStatusHistory(ctx, inv.ID, actor, inv.PopChanges())
	})
	return inv, err
}

// Void cancela a fatura (sem pagamentos), estorna o lançamento de emissão e
// devolve créditos aplicados.
func (s *Service) Void(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, reason string, expectedVersion int) (Invoice, error) {
	return s.mutate(ctx, tenantID, actor, id, expectedVersion, func(tx Tx, inv *Invoice, now time.Time) error {
		if err := inv.Void(reason, now); err != nil {
			return err
		}
		if err := tx.ReverseLedger(ctx, "invoice.issued:"+inv.ID.String(), "Cancelamento da fatura "+inv.Number, now); err != nil {
			return err
		}
		if inv.CreditApplied.IsPositive() {
			if err := tx.ReturnCustomerCredit(ctx, inv.CustomerID, inv.ID, inv.CreditApplied.Amount(), now); err != nil {
				return err
			}
		}
		if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventInvoiceVoided, "invoice", inv.ID,
			map[string]any{"invoice_id": inv.ID, "number": inv.Number}, now)); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "invoice.voided", "invoice", inv.ID.String(), map[string]any{"number": inv.Number}))
	})
}

// MarkUncollectible baixa o saldo devedor como incobrável.
func (s *Service) MarkUncollectible(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, reason string, expectedVersion int) (Invoice, error) {
	if l := len([]rune(reason)); l < 3 || l > 500 {
		return Invoice{}, apperr.Invalid("invalid_reason", "Informe o motivo da baixa (3 a 500 caracteres).")
	}
	return s.mutate(ctx, tenantID, actor, id, expectedVersion, func(tx Tx, inv *Invoice, now time.Time) error {
		amount, err := inv.MarkUncollectible(reason, now)
		if err != nil {
			return err
		}
		if t, ok, err := ledger.InvoiceWrittenOff(ids.New(), tenantID, inv.ID, amount, now); err != nil {
			return err
		} else if ok {
			if err := tx.PostLedger(ctx, t); err != nil {
				return err
			}
		}
		if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventInvoiceUncollectible, "invoice", inv.ID,
			map[string]any{"invoice_id": inv.ID, "written_off": amount.Amount()}, now)); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "invoice.marked_uncollectible", "invoice", inv.ID.String(),
			map[string]any{"written_off": amount.Amount()}))
	})
}

// GrantCredit concede crédito ao cliente para abatimento em faturas futuras.
// requestKey (derivado do Idempotency-Key) impede concessão duplicada. by é o
// usuário que concede; nil quando a concessão vem de uma credencial de API (a
// autoria fica na auditoria, pelo ator).
func (s *Service) GrantCredit(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, by *uuid.UUID, customerID uuid.UUID, amount money.Money, reason, requestKey string) error {
	if !amount.IsPositive() || amount.CheckBusinessRange() != nil {
		return apperr.Invalid("invalid_amount", "Valor inválido.", apperr.FieldError{Field: "amount", Message: "Informe um valor positivo em centavos."})
	}
	if l := len([]rune(reason)); l < 3 || l > 500 {
		return apperr.Invalid("invalid_reason", "Motivo inválido.", apperr.FieldError{Field: "reason", Message: "Informe de 3 a 500 caracteres."})
	}
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		creditID, inserted, err := tx.GrantCustomerCredit(ctx, customerID, amount, reason, by, requestKey)
		if err != nil || !inserted {
			return err
		}
		now := s.clock.Now()
		t, ok, err := ledger.CustomerCreditGranted(ids.New(), tenantID, creditID, amount, now)
		if err != nil {
			return err
		}
		if ok {
			if err := tx.PostLedger(ctx, t); err != nil {
				return err
			}
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "customer.credit_granted", "customer", customerID.String(),
			map[string]any{"amount": amount.Amount(), "credit_id": creditID}))
	})
}
