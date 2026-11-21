package payments

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// Baixa do valor a recuperar de cliente (ADR-073). Na disputa perdida de
// pagamento com excedente, a parte do crédito que o cliente já havia usado
// vira valor a recuperar (Dispute.RecoveryAmount, conta customer_recoverables,
// ADR-070). A baixa tira esse valor da conta: recebida, quando o cliente paga
// por fora do provedor, ou perdida, quando o tenant desiste de cobrar.

// RecoveryOutcome é o desfecho da baixa do valor a recuperar.
type RecoveryOutcome string

const (
	// RecoveryReceived: o cliente pagou o valor fora do provedor
	// (transferência, acerto comercial). Lançamento dispute.recovery_received.
	RecoveryReceived RecoveryOutcome = "received"
	// RecoveryWrittenOff: o tenant desistiu de cobrar o valor, reconhecido
	// como perda. Lançamento dispute.recovery_written_off.
	RecoveryWrittenOff RecoveryOutcome = "written_off"
)

// Valid informa se o desfecho é conhecido.
func (o RecoveryOutcome) Valid() bool { return o == RecoveryReceived || o == RecoveryWrittenOff }

// Limites da justificativa da baixa, os mesmos da resolução de divergência.
const (
	recoveryJustificationMin = 10
	recoveryJustificationMax = 2000
)

// RecoverySettlement é uma baixa, total ou parcial, do valor a recuperar de
// uma disputa perdida. É imutável; a soma das baixas da disputa nunca passa do
// valor a recuperar (garantido no banco, sob o bloqueio da disputa).
type RecoverySettlement struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	DisputeID uuid.UUID
	Outcome   RecoveryOutcome
	Amount    money.Money
	// EffectiveDate é a data do recebimento ou da decisão, no fuso do
	// tenant. O lançamento no ledger usa o instante do registro (CreatedAt):
	// meses já fechados não mudam.
	EffectiveDate calendar.Date
	Justification string
	// SettledBy é o usuário que registrou a baixa; nulo para credencial de
	// API, cuja autoria fica na auditoria (ADR-059).
	SettledBy           *uuid.UUID
	RequestKey          string
	LedgerTransactionID uuid.UUID
	CreatedAt           time.Time
}

// RecoverySettlementInput é o pedido de baixa do valor a recuperar.
type RecoverySettlementInput struct {
	DisputeID uuid.UUID
	Outcome   RecoveryOutcome
	Amount    money.Money
	// EffectiveDate é a data efetiva no fuso do tenant; nil usa a data de
	// hoje.
	EffectiveDate *calendar.Date
	Justification string
	// RequestKey identifica o pedido (derivado do Idempotency-Key); repetir
	// a chave devolve a baixa já registrada.
	RequestKey string
}

func validateRecoveryInput(in RecoverySettlementInput) (RecoverySettlementInput, error) {
	var f []apperr.FieldError
	if !in.Outcome.Valid() {
		f = append(f, apperr.FieldError{Field: "outcome", Message: "Use received (recebido) ou written_off (perdido)."})
	}
	if !in.Amount.IsPositive() || in.Amount.CheckBusinessRange() != nil {
		f = append(f, apperr.FieldError{Field: "amount", Message: "Informe um valor positivo em centavos."})
	}
	in.Justification = strings.TrimSpace(in.Justification)
	if n := len([]rune(in.Justification)); n < recoveryJustificationMin || n > recoveryJustificationMax || strings.ContainsRune(in.Justification, 0) {
		f = append(f, apperr.FieldError{Field: "justification", Message: "Informe a justificativa com 10 a 2000 caracteres."})
	}
	if len(in.RequestKey) > 300 {
		f = append(f, apperr.FieldError{Field: "request_key", Message: "Chave do pedido muito longa."})
	}
	if len(f) > 0 {
		return in, apperr.Invalid("invalid_recovery_settlement", "Baixa do valor a recuperar inválida.", f...)
	}
	return in, nil
}

// RecoverySettled é o total já baixado do valor a recuperar (recebido mais
// perdido).
func (d Dispute) RecoverySettled() (money.Money, error) {
	zero := money.Zero(d.Amount.Currency())
	return amountOrZero(d.RecoveryReceived, zero).Add(amountOrZero(d.RecoveryWrittenOff, zero))
}

// RecoveryOutstanding é o valor que ainda falta recuperar do cliente: o valor
// a recuperar menos as baixas recebidas e perdidas.
func (d Dispute) RecoveryOutstanding() (money.Money, error) {
	settled, err := d.RecoverySettled()
	if err != nil {
		return money.Money{}, err
	}
	return amountOrZero(d.RecoveryAmount, money.Zero(d.Amount.Currency())).Sub(settled)
}

// recoveryLedger monta o lançamento da baixa: débito no disponível liquidado
// (recebida) ou em perdas com inadimplência (perdida) e crédito em valores a
// recuperar de clientes.
func recoveryLedger(st RecoverySettlement, at time.Time) (ledger.Transaction, error) {
	build := ledger.RecoveryReceived
	if st.Outcome == RecoveryWrittenOff {
		build = ledger.RecoveryWrittenOff
	}
	t, _, err := build(ids.New(), st.TenantID, st.ID, st.Amount, at)
	return t, err
}

// SettleRecovery registra a baixa, total ou parcial, do valor a recuperar de
// uma disputa perdida. Na mesma transação: bloqueia a disputa (baixas
// concorrentes e a repetição da chave se serializam), confere o saldo, grava
// o lançamento balanceado no ledger, a baixa (o banco acumula o total na
// disputa e recusa o que passaria do valor a recuperar), o evento
// dispute.recovery_settled e a auditoria. A repetição com a mesma chave
// devolve a baixa já registrada; a chave usada em outra baixa é recusada.
// userID é nulo para credencial de API (ator api_credential na auditoria).
func (s *Service) SettleRecovery(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, userID *uuid.UUID,
	in RecoverySettlementInput) (RecoverySettlement, Dispute, error) {
	in, err := validateRecoveryInput(in)
	if err != nil {
		return RecoverySettlement{}, Dispute{}, err
	}
	var (
		st RecoverySettlement
		d  Dispute
	)
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if d, err = tx.Dispute(ctx, in.DisputeID, true); err != nil {
			return err
		}
		if in.RequestKey != "" {
			ex, err := tx.RecoverySettlementByRequestKey(ctx, in.RequestKey)
			if err == nil {
				if ex.DisputeID != in.DisputeID || ex.Outcome != in.Outcome || ex.Amount != in.Amount {
					return apperr.Conflict("request_key_reused", "A chave do pedido já foi usada em outra baixa.")
				}
				st = ex
				return nil
			}
			if !apperr.IsKind(err, apperr.KindNotFound) {
				return err
			}
		}
		now := s.clock.Now()
		if st, err = s.newRecoverySettlement(ctx, tx, d, in, userID, now); err != nil {
			return err
		}
		t, err := recoveryLedger(st, now)
		if err != nil {
			return err
		}
		if err := tx.PostLedger(ctx, t); err != nil {
			return err
		}
		st.LedgerTransactionID = t.ID
		if err := tx.InsertRecoverySettlement(ctx, st); err != nil {
			return err
		}
		// Os totais da disputa são acumulados pelo banco na gravação da baixa.
		if d, err = tx.Dispute(ctx, d.ID, false); err != nil {
			return err
		}
		return s.recordRecoverySettled(ctx, tx, d, st, actor, now)
	})
	if err != nil {
		return RecoverySettlement{}, Dispute{}, err
	}
	return st, d, nil
}

// newRecoverySettlement confere a disputa (bloqueada pelo chamador) e a data
// efetiva e monta a baixa. A data efetiva, no fuso do tenant, não fica no
// futuro nem antes do encerramento da disputa.
func (s *Service) newRecoverySettlement(ctx context.Context, tx Tx, d Dispute, in RecoverySettlementInput, userID *uuid.UUID,
	now time.Time) (RecoverySettlement, error) {
	if d.Status != DisputeLost || !amountOrZero(d.RecoveryAmount, money.Zero(d.Amount.Currency())).IsPositive() {
		return RecoverySettlement{}, apperr.Conflict("dispute_without_recovery", "A disputa não tem valor a recuperar do cliente.")
	}
	outstanding, err := d.RecoveryOutstanding()
	if err != nil {
		return RecoverySettlement{}, err
	}
	if !outstanding.IsPositive() {
		return RecoverySettlement{}, apperr.Conflict("recovery_already_settled", "O valor a recuperar desta disputa já foi baixado por completo.")
	}
	cmp, err := in.Amount.Cmp(outstanding)
	if err != nil {
		return RecoverySettlement{}, err
	}
	if cmp > 0 {
		return RecoverySettlement{}, apperr.Conflict("recovery_exceeds_outstanding", "A baixa excede o saldo a recuperar da disputa.")
	}
	loc, err := tx.Location(ctx)
	if err != nil {
		return RecoverySettlement{}, err
	}
	today := calendar.DateOf(now, loc)
	effective := today
	if in.EffectiveDate != nil {
		effective = *in.EffectiveDate
	}
	if today.Before(effective) {
		return RecoverySettlement{}, apperr.Invalid("invalid_recovery_settlement", "Baixa do valor a recuperar inválida.",
			apperr.FieldError{Field: "effective_date", Message: "A data efetiva não pode estar no futuro."})
	}
	if d.ClosedAt != nil && effective.Before(calendar.DateOf(*d.ClosedAt, loc)) {
		return RecoverySettlement{}, apperr.Invalid("invalid_recovery_settlement", "Baixa do valor a recuperar inválida.",
			apperr.FieldError{Field: "effective_date", Message: "A data efetiva não pode ser anterior ao encerramento da disputa."})
	}
	return RecoverySettlement{ID: ids.New(), TenantID: d.TenantID, DisputeID: d.ID, Outcome: in.Outcome, Amount: in.Amount,
		EffectiveDate: effective, Justification: in.Justification, SettledBy: userID, RequestKey: in.RequestKey, CreatedAt: now}, nil
}

// recordRecoverySettled grava o evento dispute.recovery_settled e a
// auditoria da baixa, com o saldo que ainda falta recuperar.
func (s *Service) recordRecoverySettled(ctx context.Context, tx Tx, d Dispute, st RecoverySettlement, actor audit.Actor, now time.Time) error {
	outstanding, err := d.RecoveryOutstanding()
	if err != nil {
		return err
	}
	c, err := tx.Charge(ctx, d.ChargeID, false)
	if err != nil {
		return err
	}
	if err := tx.Emit(ctx, outbox.New(d.TenantID, outbox.EventDisputeRecoverySettled, "dispute", d.ID,
		map[string]any{"settlement_id": st.ID, "dispute_id": d.ID, "charge_id": c.ID, "invoice_id": c.InvoiceID, "customer_id": c.CustomerID,
			"outcome": string(st.Outcome), "amount": st.Amount.Amount(), "effective_date": st.EffectiveDate.String(),
			"recovery_amount": d.RecoveryAmount.Amount(), "recovery_outstanding_amount": outstanding.Amount()}, now)); err != nil {
		return err
	}
	return tx.Audit(ctx, audit.New(d.TenantID, now, actor, "dispute.recovery_settled", "dispute", d.ID.String(),
		map[string]any{"settlement_id": st.ID, "charge_id": c.ID, "outcome": string(st.Outcome), "amount": st.Amount.Amount(),
			"effective_date": st.EffectiveDate.String(), "justification": st.Justification, "ledger_transaction_id": st.LedgerTransactionID,
			"recovery_amount": d.RecoveryAmount.Amount(), "recovery_outstanding_amount": outstanding.Amount()}))
}

// Dispute retorna uma disputa do tenant.
func (s *Service) Dispute(ctx context.Context, tenantID, id uuid.UUID) (Dispute, error) {
	var d Dispute
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		d, err = tx.Dispute(ctx, id, false)
		return err
	})
	return d, err
}

// RecoverySettlements lista as baixas do valor a recuperar da disputa, em
// ordem de registro, com a disputa (404 quando ela não existe no tenant).
func (s *Service) RecoverySettlements(ctx context.Context, tenantID, disputeID uuid.UUID) (Dispute, []RecoverySettlement, error) {
	var (
		d   Dispute
		out []RecoverySettlement
	)
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if d, err = tx.Dispute(ctx, disputeID, false); err != nil {
			return err
		}
		out, err = tx.RecoverySettlements(ctx, disputeID)
		return err
	})
	return d, out, err
}
