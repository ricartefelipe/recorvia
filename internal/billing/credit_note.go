package billing

import (
	"context"
	"fmt"
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

// CreditNote reduz o saldo devedor de uma fatura emitida sem reescrevê-la.
// É imutável; a numeração deriva do número da fatura.
type CreditNote struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	InvoiceID  uuid.UUID
	Number     string
	Amount     money.Money
	Reason     string
	CreatedBy  *uuid.UUID
	RequestKey string
	CreatedAt  time.Time
}

// CreditNoteTx são as operações de notas de crédito.
type CreditNoteTx interface {
	TenantSettings(ctx context.Context) (TenantSettings, error)
	Invoice(ctx context.Context, id uuid.UUID, forUpdate bool) (Invoice, error)
	UpdateInvoiceProgress(ctx context.Context, inv *Invoice) error
	InsertStatusHistory(ctx context.Context, invoiceID uuid.UUID, actor audit.Actor, changes []StatusChange) error
	InsertCreditNote(ctx context.Context, cn CreditNote) (bool, error)
	CreditNoteByRequestKey(ctx context.Context, key string) (CreditNote, error)
	CountCreditNotes(ctx context.Context, invoiceID uuid.UUID) (int, error)
	RefreshSubscriptionStanding(ctx context.Context, subscriptionID uuid.UUID, actor audit.Actor, now time.Time) error
	PostLedger(ctx context.Context, t ledger.Transaction) error
	Audit(ctx context.Context, e audit.Entry) error
	Emit(ctx context.Context, e outbox.Event) error
}

// CreditNoteStore abre transações de notas de crédito.
type CreditNoteStore interface {
	InCreditNoteTx(ctx context.Context, tenantID uuid.UUID, fn func(CreditNoteTx) error) error
}

// CreditNoteService emite notas de crédito.
type CreditNoteService struct {
	store CreditNoteStore
	clock clock.Clock
}

// NewCreditNoteService cria o serviço.
func NewCreditNoteService(store CreditNoteStore, c clock.Clock) *CreditNoteService {
	return &CreditNoteService{store: store, clock: c}
}

// Issue emite uma nota de crédito limitada ao saldo em aberto da fatura.
// Valores já pagos não são devolvidos por nota de crédito (use reembolso ou
// crédito do cliente). A chave do pedido torna a emissão repetível: a mesma
// chave devolve a nota já emitida.
func (s *CreditNoteService) Issue(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, by *uuid.UUID, invoiceID uuid.UUID,
	amount money.Money, reason, requestKey string) (CreditNote, error) {
	if l := len([]rune(reason)); l < 3 || l > 500 {
		return CreditNote{}, apperr.Invalid("invalid_credit_note", "Nota de crédito inválida.",
			apperr.FieldError{Field: "reason", Message: "Informe o motivo (3 a 500 caracteres)."})
	}
	var cn CreditNote
	err := s.store.InCreditNoteTx(ctx, tenantID, func(tx CreditNoteTx) error {
		inv, err := tx.Invoice(ctx, invoiceID, true)
		if err != nil {
			return err
		}
		if requestKey != "" {
			existing, err := tx.CreditNoteByRequestKey(ctx, requestKey)
			if err == nil {
				if existing.InvoiceID != inv.ID {
					return apperr.Conflict("request_key_reused", "A chave do pedido já foi usada em outra fatura.")
				}
				cn = existing
				return nil
			}
			if !apperr.IsKind(err, apperr.KindNotFound) {
				return err
			}
		}
		now := s.clock.Now()
		if err := inv.ApplyCreditNote(amount, now); err != nil {
			return err
		}
		n, err := tx.CountCreditNotes(ctx, inv.ID)
		if err != nil {
			return err
		}
		cn = CreditNote{ID: ids.New(), TenantID: tenantID, InvoiceID: inv.ID, Number: fmt.Sprintf("%s-NC%02d", inv.Number, n+1),
			Amount: amount, Reason: reason, CreatedBy: by, RequestKey: requestKey, CreatedAt: now}
		inserted, err := tx.InsertCreditNote(ctx, cn)
		if err != nil {
			return err
		}
		if !inserted {
			return apperr.Conflict("credit_note_in_progress", "Pedido de nota de crédito em processamento; tente novamente.")
		}
		if err := tx.UpdateInvoiceProgress(ctx, &inv); err != nil {
			return err
		}
		changes := inv.PopChanges()
		for i := range changes {
			changes[i].Details["credit_note_id"], changes[i].Details["credit_note_number"] = cn.ID, cn.Number
		}
		if err := tx.InsertStatusHistory(ctx, inv.ID, actor, changes); err != nil {
			return err
		}
		t, ok, err := ledger.CreditNoteIssued(ids.New(), tenantID, cn.ID, amount, now)
		if err != nil {
			return err
		}
		if ok {
			if err := tx.PostLedger(ctx, t); err != nil {
				return err
			}
		}
		payload := map[string]any{"invoice_id": inv.ID, "number": inv.Number, "credit_note_id": cn.ID, "credit_note_number": cn.Number,
			"amount": amount.Amount(), "amount_due": inv.AmountDue.Amount(), "status": inv.Status}
		if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventInvoiceCredited, "invoice", inv.ID, payload, now)); err != nil {
			return err
		}
		if inv.Status == Paid {
			if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventInvoicePaid, "invoice", inv.ID,
				map[string]any{"invoice_id": inv.ID, "number": inv.Number, "amount_due": int64(0), "amount_paid": inv.AmountPaid.Amount()}, now)); err != nil {
				return err
			}
			if inv.SubscriptionID != nil {
				if err := tx.RefreshSubscriptionStanding(ctx, *inv.SubscriptionID, actor, now); err != nil {
					return err
				}
			}
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "invoice.credit_note_issued", "invoice", inv.ID.String(),
			map[string]any{"credit_note_id": cn.ID, "number": cn.Number, "amount": amount.Amount(), "amount_due": inv.AmountDue.Amount()}))
	})
	return cn, err
}
