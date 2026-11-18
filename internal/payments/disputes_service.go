package payments

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// disputeOccurredAt é o instante informado pelo provedor para o retrato.
func disputeOccurredAt(pc ProviderCharge, now time.Time) time.Time {
	if pc.UpdatedAt != nil {
		return pc.UpdatedAt.UTC()
	}
	return now
}

// syncDispute aplica a situação de chargeback informada pelo provedor à
// cobrança paga (bloqueada pelo chamador). Abre a disputa quando ainda não
// há disputa aberta (lançamento dispute.opened, que retém o valor), registra
// a análise, estorna o lançamento quando a disputa é ganha e registra o
// chargeback na cobrança quando é perdida. Eventos de disputa já encerrada
// ou anteriores ao encerramento não reabrem disputas. Com retrato obtido por
// consulta (authoritative), a cobrança novamente recebida sem chargeback
// (inclusive parcialmente reembolsada) encerra a disputa aberta como ganha.
func (s *Service) syncDispute(ctx context.Context, tx Tx, c *Charge, pc ProviderCharge, now time.Time, actor audit.Actor, authoritative bool) error {
	target, ok := disputeTarget(pc.Status)
	latest, err := tx.LatestDispute(ctx, c.ID)
	if err != nil {
		return err
	}
	open := latest != nil && latest.Open()
	if !ok {
		if !open || !authoritative || !receivedWithoutChargeback(pc.Status) {
			return nil
		}
		target = DisputeWon
	}
	occurred := disputeOccurredAt(pc, now)
	var d Dispute
	if open {
		d = *latest
	} else {
		if latest != nil && (target == DisputeWon || target == DisputeLost) {
			return nil // decisão já registrada
		}
		if latest != nil && latest.ClosedAt != nil && !occurred.After(*latest.ClosedAt) {
			return nil // retrato anterior ao encerramento da última disputa
		}
		opened, created, err := s.openDispute(ctx, tx, c, pc, occurred, now, actor)
		if err != nil || !created {
			return err
		}
		d = opened
	}
	if target == DisputeOpened || target == d.Status || !slices.Contains(disputeTransitions[d.Status], target) {
		return nil // nada a mudar ou situação antiga do provedor (por exemplo, análise depois da decisão)
	}
	ev, err := d.transition(target, pc.RawStatus, occurred, now)
	if err != nil {
		return err
	}
	ev.ID = ids.New()
	if err := tx.UpdateDispute(ctx, &d); err != nil {
		return err
	}
	if err := tx.InsertDisputeEvent(ctx, ev); err != nil {
		return err
	}
	switch target {
	case DisputeWon:
		if err := c.CloseDispute(true, money.Zero(c.AmountPaid.Currency()), now); err != nil {
			return err
		}
		if err := tx.ReverseLedger(ctx, "dispute.opened:"+d.ID.String(), "Chargeback revertido: disputa ganha", now); err != nil {
			return err
		}
	case DisputeLost:
		if err := c.CloseDispute(false, d.Amount, now); err != nil {
			return err
		}
		if err := s.recordChargebackOnInvoice(ctx, tx, c, d, actor, now); err != nil {
			return err
		}
	default:
		return tx.Audit(ctx, audit.New(c.TenantID, now, actor, "dispute.under_review", "dispute", d.ID.String(),
			map[string]any{"charge_id": c.ID, "provider_status": pc.RawStatus}))
	}
	if err := tx.Emit(ctx, outbox.New(c.TenantID, outbox.EventDisputeClosed, "dispute", d.ID,
		map[string]any{"dispute_id": d.ID, "charge_id": c.ID, "invoice_id": c.InvoiceID, "result": string(d.Status), "amount": d.Amount.Amount()}, now)); err != nil {
		return err
	}
	return tx.Audit(ctx, audit.New(c.TenantID, now, actor, "dispute."+string(d.Status), "dispute", d.ID.String(),
		map[string]any{"charge_id": c.ID, "amount": d.Amount.Amount(), "provider_status": pc.RawStatus}))
}

// receivedWithoutChargeback informa se a situação do provedor é de cobrança
// recebida sem chargeback em andamento.
func receivedWithoutChargeback(st ProviderStatus) bool {
	return st == ProviderReceived || st == ProviderConfirmed || st == ProviderPartiallyRefunded
}

// openDispute abre a disputa pelo saldo ainda não reembolsado nem estornado
// e retém o valor no ledger. created=false quando não há saldo a contestar
// ou a cobrança não está paga.
func (s *Service) openDispute(ctx context.Context, tx Tx, c *Charge, pc ProviderCharge, occurred, now time.Time, actor audit.Actor) (Dispute, bool, error) {
	if c.Status != ChargePaid && c.Status != ChargePartiallyRefunded {
		return Dispute{}, false, nil
	}
	amount, err := c.AmountPaid.Sub(c.AmountRefunded)
	if err != nil {
		return Dispute{}, false, err
	}
	if amount, err = amount.Sub(c.AmountChargedBack); err != nil || !amount.IsPositive() {
		return Dispute{}, false, err
	}
	if err := c.OpenDispute(now); err != nil {
		return Dispute{}, false, err
	}
	d := Dispute{ID: ids.New(), TenantID: c.TenantID, ChargeID: c.ID, Provider: c.Provider, Status: DisputeOpened, Amount: amount,
		ProviderStatus: pc.RawStatus, OpenedAt: occurred, CreatedAt: now, UpdatedAt: now, Version: 1}
	if err := tx.InsertDispute(ctx, d); err != nil {
		return Dispute{}, false, err
	}
	if err := tx.InsertDisputeEvent(ctx, DisputeEvent{ID: ids.New(), TenantID: c.TenantID, DisputeID: d.ID, To: DisputeOpened,
		ProviderStatus: pc.RawStatus, OccurredAt: occurred, RecordedAt: now, Details: map[string]any{"amount": amount.Amount()}}); err != nil {
		return Dispute{}, false, err
	}
	t, ok, err := ledger.ChargebackOpened(ids.New(), c.TenantID, d.ID, amount, now)
	if err != nil {
		return Dispute{}, false, err
	}
	if ok {
		if err := tx.PostLedger(ctx, t); err != nil {
			return Dispute{}, false, err
		}
	}
	if err := tx.Emit(ctx, outbox.New(c.TenantID, outbox.EventDisputeOpened, "dispute", d.ID,
		map[string]any{"dispute_id": d.ID, "charge_id": c.ID, "invoice_id": c.InvoiceID, "amount": amount.Amount()}, now)); err != nil {
		return Dispute{}, false, err
	}
	err = tx.Audit(ctx, audit.New(c.TenantID, now, actor, "dispute.opened", "dispute", d.ID.String(),
		map[string]any{"charge_id": c.ID, "amount": amount.Amount(), "provider_status": pc.RawStatus}))
	return d, true, err
}

// recordChargebackOnInvoice registra o chargeback no histórico da fatura,
// que não é reaberta (ADR-034).
func (s *Service) recordChargebackOnInvoice(ctx context.Context, tx Tx, c *Charge, d Dispute, actor audit.Actor, now time.Time) error {
	inv, err := tx.Invoice(ctx, c.InvoiceID, false)
	if err != nil {
		return err
	}
	status := inv.Status
	return tx.InsertInvoiceHistory(ctx, inv.ID, actor, []billing.StatusChange{{From: &status, To: status, At: now, Reason: "chargeback",
		Details: map[string]any{"dispute_id": d.ID, "charge_id": c.ID, "amount": d.Amount.Amount()}}})
}

// LatestDispute retorna a disputa mais recente da cobrança (nil se não houver).
func (s *Service) LatestDispute(ctx context.Context, tenantID, chargeID uuid.UUID) (*Dispute, error) {
	var d *Dispute
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		d, err = tx.LatestDispute(ctx, chargeID)
		return err
	})
	return d, err
}
