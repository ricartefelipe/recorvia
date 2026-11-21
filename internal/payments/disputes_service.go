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
// chargeback na cobrança quando é perdida (liberando a retenção da parte já
// reembolsada ao pagador, ver closeLostDispute). Eventos de disputa já encerrada
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
	charged := money.Zero(d.Amount.Currency())
	var credit chargebackCredit
	switch target {
	case DisputeWon:
		// A disputa ganha não mexe no crédito do cliente: só a perdida
		// devolve ao pagador o excedente que o gerou.
		if err := c.CloseDispute(true, charged, now); err != nil {
			return err
		}
		if err := tx.ReverseLedger(ctx, "dispute.opened:"+d.ID.String(), "Chargeback revertido: disputa ganha", now); err != nil {
			return err
		}
	case DisputeLost:
		if charged, credit, err = s.closeLostDispute(ctx, tx, c, &d, now); err != nil {
			return err
		}
		ev.Details = map[string]any{"charged_back_amount": charged.Amount(), "credit_amount": credit.consumed.Amount(),
			"recovery_amount": credit.recovery.Amount()}
		if err := s.recordChargebackOnInvoice(ctx, tx, c, d, charged, credit, actor, now); err != nil {
			return err
		}
	}
	if err := tx.UpdateDispute(ctx, &d); err != nil {
		return err
	}
	if err := tx.InsertDisputeEvent(ctx, ev); err != nil {
		return err
	}
	if target == DisputeUnderReview {
		return tx.Audit(ctx, audit.New(c.TenantID, now, actor, "dispute.under_review", "dispute", d.ID.String(),
			map[string]any{"charge_id": c.ID, "provider_status": pc.RawStatus}))
	}
	if err := tx.Emit(ctx, outbox.New(c.TenantID, outbox.EventDisputeClosed, "dispute", d.ID,
		map[string]any{"dispute_id": d.ID, "charge_id": c.ID, "invoice_id": c.InvoiceID, "result": string(d.Status), "amount": d.Amount.Amount(),
			"charged_back_amount": charged.Amount(), "credit_amount": credit.consumed.Amount(), "recovery_amount": credit.recovery.Amount()},
		now)); err != nil {
		return err
	}
	return tx.Audit(ctx, audit.New(c.TenantID, now, actor, "dispute."+string(d.Status), "dispute", d.ID.String(),
		map[string]any{"charge_id": c.ID, "amount": d.Amount.Amount(), "charged_back_amount": charged.Amount(),
			"credit_amount": credit.consumed.Amount(), "recovery_amount": credit.recovery.Amount(), "provider_status": pc.RawStatus}))
}

// chargebackCredit é o efeito do chargeback sobre o crédito do cliente gerado
// pelo excedente do pagamento: consumed saiu do saldo disponível e recovery é
// a parte já usada pelo cliente, que passa a ser valor a recuperar.
type chargebackCredit struct {
	consumed money.Money
	recovery money.Money
}

func (c chargebackCredit) changed() bool { return c.consumed.IsPositive() || c.recovery.IsPositive() }

// reverseChargebackCredit aplica ao crédito do cliente a parte do valor
// estornado por chargeback (charged) que corresponde ao excedente do
// pagamento da cobrança, com a mesma base do reembolso (overpaymentPortion):
// o excedente ainda não devolvido ao pagador por reembolso nem por chargeback
// anterior. O crédito ainda disponível é consumido; o que o cliente já usou
// não deixa o saldo negativo e vira valor a recuperar dele. O lançamento
// dispute.credit_reversed tira as duas partes de chargebacks (não são receita
// perdida): débito em créditos de clientes pelo consumido e em valores a
// recuperar de clientes pelo restante. A disputa (atualizada em memória; o
// chamador a grava) acumula as duas partes, que passam a contar como
// excedente já devolvido. Ordem de bloqueio: cobrança (já bloqueada pelo
// chamador), fatura e cliente (crédito), a mesma do reembolso.
func (s *Service) reverseChargebackCredit(ctx context.Context, tx Tx, c *Charge, d *Dispute, charged money.Money, refType string,
	refID uuid.UUID, now time.Time) (chargebackCredit, error) {
	zero := money.Zero(charged.Currency())
	none := chargebackCredit{consumed: zero, recovery: zero}
	part, err := overpaymentPortion(ctx, tx, c, charged)
	if err != nil || !part.IsPositive() {
		return none, err
	}
	inv, err := tx.Invoice(ctx, c.InvoiceID, true)
	if err != nil {
		return none, err
	}
	balance, err := lockedCreditBalance(ctx, tx, c, charged.Currency())
	if err != nil {
		return none, err
	}
	consumed, err := money.Min(part, balance)
	if err != nil {
		return none, err
	}
	recovery, err := part.Sub(consumed)
	if err != nil {
		return none, err
	}
	if consumed.IsPositive() {
		neg, err := consumed.Neg()
		if err != nil {
			return none, err
		}
		invID := inv.ID
		if err := tx.GrantCustomerCredit(ctx, c.CustomerID, neg, "consumption", &invID, "Estornado por chargeback da cobrança "+c.ID.String(), now); err != nil {
			return none, err
		}
	}
	t, ok, err := ledger.ChargebackCreditReversed(ids.New(), c.TenantID, refType, refID, consumed, recovery, now)
	if err != nil {
		return none, err
	}
	if ok {
		if err := tx.PostLedger(ctx, t); err != nil {
			return none, err
		}
	}
	if d.CreditAmount, err = amountOrZero(d.CreditAmount, zero).Add(consumed); err != nil {
		return none, err
	}
	if d.RecoveryAmount, err = amountOrZero(d.RecoveryAmount, zero).Add(recovery); err != nil {
		return none, err
	}
	return chargebackCredit{consumed: consumed, recovery: recovery}, nil
}

// amountOrZero devolve zero na moeda informada para o valor não inicializado.
func amountOrZero(m, zero money.Money) money.Money {
	if m.Currency().IsZero() {
		return zero
	}
	return m
}

// closeLostDispute registra na cobrança o chargeback da disputa perdida e
// devolve o valor efetivamente estornado e o efeito sobre o crédito do
// cliente. A retenção da abertura cobre o saldo contestado naquele momento; a
// parte devolvida ao pagador por reembolso confirmado durante a disputa, ou
// ainda reservada por reembolso em andamento, não é estornada de novo (ver
// Charge.CloseDispute) e volta aos valores em liquidação pelo lançamento
// dispute.hold_released. A parte do valor estornado que corresponde ao
// excedente do pagamento consome o crédito do cliente
// (reverseChargebackCredit). Assim, o ledger registra em chargebacks o valor
// estornado menos o crédito estornado, e chargebacks, reembolsos e crédito
// devolvido nunca somam mais que o pago.
func (s *Service) closeLostDispute(ctx context.Context, tx Tx, c *Charge, d *Dispute, now time.Time) (money.Money, chargebackCredit, error) {
	before := c.AmountChargedBack
	if err := c.CloseDispute(false, d.Amount, now); err != nil {
		return money.Money{}, chargebackCredit{}, err
	}
	charged, err := c.AmountChargedBack.Sub(before)
	if err != nil {
		return money.Money{}, chargebackCredit{}, err
	}
	released, err := d.Amount.Sub(charged)
	if err != nil {
		return money.Money{}, chargebackCredit{}, err
	}
	if released.IsPositive() {
		t, ok, err := ledger.ChargebackHoldReleased(ids.New(), c.TenantID, d.ID, released, now)
		if err != nil {
			return money.Money{}, chargebackCredit{}, err
		}
		if ok {
			if err := tx.PostLedger(ctx, t); err != nil {
				return money.Money{}, chargebackCredit{}, err
			}
		}
	}
	credit, err := s.reverseChargebackCredit(ctx, tx, c, d, charged, "dispute", d.ID, now)
	return charged, credit, err
}

// receivedWithoutChargeback informa se a situação do provedor é de cobrança
// recebida sem chargeback em andamento.
func receivedWithoutChargeback(st ProviderStatus) bool {
	return st == ProviderReceived || st == ProviderConfirmed || st == ProviderPartiallyRefunded
}

// openDispute abre a disputa pelo saldo ainda não reembolsado nem estornado
// e retém o valor no ledger. Reembolsos reservados e ainda não confirmados
// continuam no saldo contestado: o provedor pode recusá-los, e, se forem
// confirmados, a disputa perdida libera essa parte da retenção
// (closeLostDispute). created=false quando não há saldo a contestar ou a
// cobrança não está paga.
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
	zero := money.Zero(amount.Currency())
	d := Dispute{ID: ids.New(), TenantID: c.TenantID, ChargeID: c.ID, Provider: c.Provider, Status: DisputeOpened, Amount: amount,
		CreditAmount: zero, RecoveryAmount: zero, ProviderStatus: pc.RawStatus, OpenedAt: occurred, CreatedAt: now, UpdatedAt: now, Version: 1}
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
// que não é reaberta (ADR-034): amount é o valor estornado, disputed_amount
// o valor contestado na abertura, credit_amount o crédito do cliente
// consumido e recovery_amount o valor a recuperar do cliente.
func (s *Service) recordChargebackOnInvoice(ctx context.Context, tx Tx, c *Charge, d Dispute, charged money.Money, credit chargebackCredit,
	actor audit.Actor, now time.Time) error {
	inv, err := tx.Invoice(ctx, c.InvoiceID, false)
	if err != nil {
		return err
	}
	status := inv.Status
	return tx.InsertInvoiceHistory(ctx, inv.ID, actor, []billing.StatusChange{{From: &status, To: status, At: now, Reason: "chargeback",
		Details: map[string]any{"dispute_id": d.ID, "charge_id": c.ID, "amount": charged.Amount(), "disputed_amount": d.Amount.Amount(),
			"credit_amount": credit.consumed.Amount(), "recovery_amount": credit.recovery.Amount()}}})
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
