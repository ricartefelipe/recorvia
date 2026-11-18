package payments

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
)

// scheduleProviderCancel grava, na transação corrente, a operação que cancela
// no provedor uma cobrança cancelada localmente (fatura cancelada,
// substituição por mudança de saldo ou nota de crédito). Cobrança ainda sem
// vínculo no provedor não gera operação: o cancelamento ocorre quando a
// criação termina. ok=false indica que não há operação nova a executar.
func (s *Service) scheduleProviderCancel(ctx context.Context, tx Tx, c Charge, now time.Time) (uuid.UUID, bool, error) {
	if c.Status != ChargeCanceled || c.ProviderChargeID == "" {
		return uuid.Nil, false, nil
	}
	op := Operation{ID: ids.New(), TenantID: c.TenantID, ChargeID: c.ID, Kind: OpCancelCharge, Status: OpPending,
		ExternalReference: c.ID.String(), NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
	inserted, err := tx.EnsureOperation(ctx, op)
	if err != nil || !inserted {
		return uuid.Nil, false, err
	}
	return op.ID, true, nil
}

// cancelResult é o resultado do cancelamento no provedor.
type cancelResult struct {
	outcome  outcome
	snapshot *ProviderCharge
	err      error
}

// errPaidAtProvider indica cobrança já paga no provedor: não há o que
// cancelar, e o pagamento segue a aplicação normal (ADR-030).
var errPaidAtProvider = errors.New("payments: a cobrança já foi paga no provedor")

func (s *Service) runCancel(ctx context.Context, tenantID uuid.UUID, w opWork) error {
	gw, err := s.resolver.Gateway(w.account)
	if err != nil {
		return s.recordCancelOutcome(ctx, tenantID, w.op.ID, cancelResult{outcome: outcomePermanent, err: err})
	}
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	res := cancelAtProvider(callCtx, gw, w)
	cancel()
	return s.recordCancelOutcome(ctx, tenantID, w.op.ID, res)
}

// cancelAtProvider cancela a cobrança. Depois de uma tentativa ambígua,
// consulta o provedor antes de repetir. Cobrança inexistente no provedor
// equivale a cancelada.
func cancelAtProvider(ctx context.Context, gw Gateway, w opWork) cancelResult {
	if w.op.Attempts > 1 {
		pc, err := gw.GetCharge(ctx, w.charge.ProviderChargeID)
		switch {
		case GatewayErrorKind(err) == ErrKindNotFound:
			return cancelResult{outcome: outcomeSucceeded}
		case err != nil:
			return cancelResult{outcome: classify(err), err: err}
		case pc.Status == ProviderCanceled:
			return cancelResult{outcome: outcomeSucceeded, snapshot: &pc}
		case pc.Status.IsPaid():
			return cancelResult{outcome: outcomePermanent, snapshot: &pc, err: errPaidAtProvider}
		}
	}
	err := gw.CancelCharge(ctx, w.charge.ProviderChargeID)
	if err == nil || GatewayErrorKind(err) == ErrKindNotFound {
		return cancelResult{outcome: outcomeSucceeded}
	}
	return cancelResult{outcome: classify(err), err: err}
}

// recordCancelOutcome registra o resultado do cancelamento. Se o provedor
// informa que a cobrança já foi paga, o pagamento é aplicado normalmente.
func (s *Service) recordCancelOutcome(ctx context.Context, tenantID, opID uuid.UUID, res cancelResult) error {
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		op, err := tx.Operation(ctx, opID, true)
		if err != nil {
			return err
		}
		if op.Status == OpSucceeded || op.Status == OpFailed {
			return nil
		}
		c, err := tx.Charge(ctx, op.ChargeID, true)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		op.UpdatedAt, op.LeaseUntil = now, nil
		if res.err != nil {
			op.LastError = truncate(res.err.Error(), 1000)
		}
		action := ""
		switch res.outcome {
		case outcomeSucceeded:
			op.Status, op.CompletedAt, op.LastError = OpSucceeded, &now, ""
			action = "charge.provider_canceled"
		case outcomePermanent:
			op.Status, op.CompletedAt = OpFailed, &now
			action = "charge.provider_cancel_failed"
			if res.snapshot != nil && res.snapshot.Status.IsPaid() {
				if err := s.applyProviderStateFrom(ctx, tx, &c, *res.snapshot, now, providerActor, true); err != nil {
					return err
				}
			}
		case outcomeRetryable:
			op.Status, op.NextAttemptAt = OpPending, now.Add(Backoff(op.Attempts))
		default:
			op.Status, op.NextAttemptAt = OpUnknown, now.Add(Backoff(op.Attempts))
		}
		if err := tx.UpdateOperation(ctx, op); err != nil {
			return err
		}
		if action != "" {
			if err := tx.Audit(ctx, audit.New(tenantID, now, systemActor, action, "charge", c.ID.String(),
				map[string]any{"provider_charge_id": c.ProviderChargeID, "operation_id": op.ID, "error": op.LastError})); err != nil {
				return err
			}
		}
		return tx.Audit(ctx, audit.New(tenantID, now, systemActor, "payment_operation."+string(op.Status), "payment_operation", op.ID.String(),
			map[string]any{"charge_id": op.ChargeID, "attempts": op.Attempts, "kind": op.Kind}))
	})
}
