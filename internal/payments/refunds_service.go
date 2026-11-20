package payments

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// RequestRefund registra o pedido de reembolso total ou parcial de uma
// cobrança paga e o executa no provedor logo após o commit. O pedido bloqueia
// a cobrança e reserva o valor (amount_refund_pending), de modo que pedidos
// concorrentes nunca somam mais que o saldo elegível; a chave do pedido torna
// a repetição idempotente. Enquanto houver reembolso da cobrança com
// resultado desconhecido, novos pedidos são recusados até a consulta ao
// provedor ou a conciliação resolverem o anterior; enquanto outro reembolso
// já enviado ao provedor aguardar confirmação, o pedido é registrado e o
// envio aguarda (ver prepareRefundWork).
func (s *Service) RequestRefund(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, userID *uuid.UUID, in RefundInput) (Refund, error) {
	in, err := validateRefundInput(in)
	if err != nil {
		return Refund{}, err
	}
	var (
		r        Refund
		opID     uuid.UUID
		existing bool
	)
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		existing = false
		// A cobrança é bloqueada antes da verificação da chave: pedidos
		// concorrentes com a mesma chave se serializam e o segundo encontra
		// o reembolso gravado pelo primeiro.
		c, err := tx.Charge(ctx, in.ChargeID, true)
		if err != nil {
			return err
		}
		if in.RequestKey != "" {
			ex, err := tx.RefundByRequestKey(ctx, in.RequestKey)
			if err == nil {
				if ex.ChargeID != in.ChargeID || ex.Amount != in.Amount {
					return apperr.Conflict("request_key_reused", "A chave do pedido já foi usada em outro reembolso.")
				}
				r, existing = ex, true
				return nil
			}
			if !apperr.IsKind(err, apperr.KindNotFound) {
				return err
			}
		}
		if c.ProviderChargeID == "" {
			return apperr.Conflict("charge_not_refundable", "A cobrança não está vinculada a uma cobrança do provedor.")
		}
		refunds, err := tx.ChargeRefunds(ctx, c.ID)
		if err != nil {
			return err
		}
		for _, x := range refunds {
			if x.Status == RefundUnknown {
				return apperr.Conflict("refund_outcome_unknown",
					"Há reembolso desta cobrança com resultado desconhecido no provedor; aguarde a nova consulta ou resolva na conciliação.")
			}
		}
		now := s.clock.Now()
		if err := c.ReserveRefund(in.Amount, now); err != nil {
			return err
		}
		r = Refund{ID: ids.New(), TenantID: tenantID, ChargeID: c.ID, Provider: c.Provider, Origin: RefundOriginRecorvia, Status: RefundRequested,
			Amount: in.Amount, CreditAmount: money.Zero(in.Amount.Currency()), Reason: in.Reason, RequestedBy: userID, RequestKey: in.RequestKey,
			CreatedAt: now, UpdatedAt: now, Version: 1}
		refundID := r.ID
		op := Operation{ID: ids.New(), TenantID: tenantID, ChargeID: c.ID, RefundID: &refundID, Kind: OpRefund, Status: OpPending,
			ExternalReference: r.ID.String(), NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
		if err := tx.InsertRefund(ctx, r); err != nil {
			return err
		}
		if err := tx.UpdateCharge(ctx, &c); err != nil {
			return err
		}
		if err := tx.InsertOperation(ctx, op); err != nil {
			return err
		}
		opID = op.ID
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "refund.requested", "refund", r.ID.String(),
			map[string]any{"charge_id": c.ID, "invoice_id": c.InvoiceID, "amount": r.Amount.Amount(), "reason": r.Reason}))
	})
	if err != nil || existing {
		return r, err
	}
	if _, err := s.executeOperation(ctx, tenantID, opID); err != nil && s.cfg.Log != nil {
		s.cfg.Log.WarnContext(ctx, "reembolso adiado para a recuperação", slog.String("refund_id", r.ID.String()),
			slog.String("error", err.Error()))
	}
	return s.Refund(context.WithoutCancel(ctx), tenantID, r.ID)
}

// Refund retorna um reembolso.
func (s *Service) Refund(ctx context.Context, tenantID, id uuid.UUID) (Refund, error) {
	var r Refund
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		r, err = tx.Refund(ctx, id, false)
		return err
	})
	return r, err
}

// prepareRefundWork carrega o reembolso da operação. Devolve done=true
// quando não há chamada a fazer: reembolso já resolvido (a operação é
// concluída aqui) ou envio que precisa aguardar (a operação é adiada). Um
// reembolso ainda não enviado aguarda enquanto outro reembolso da mesma
// cobrança já enviado ao provedor continuar aberto (em execução, aceito e
// aguardando confirmação ou com resultado desconhecido): com no máximo um
// reembolso aberto no provedor por cobrança, a evidência sem identificação
// (associada por valor) não é ambígua e a repetição depois de resposta
// ambígua não duplica o estorno. Novas tentativas de um reembolso já enviado
// aguardam somente outra chamada em execução dentro do lease.
func (s *Service) prepareRefundWork(ctx context.Context, tx Tx, w *opWork, now time.Time) (bool, error) {
	op := w.op
	if op.RefundID == nil {
		return false, errors.New("payments: operação de reembolso sem reembolso vinculado")
	}
	r, err := tx.Refund(ctx, *op.RefundID, false)
	if err != nil {
		return false, err
	}
	if !r.Open() {
		op.Status, op.CompletedAt, op.UpdatedAt, op.LeaseUntil = OpSucceeded, &now, now, nil
		if r.Status == RefundFailed {
			op.Status = OpFailed
		}
		return true, tx.UpdateOperation(ctx, op)
	}
	// Reivindicações concorrentes de reembolsos da mesma cobrança (API e
	// recuperação) se serializam aqui: a segunda enxerga a primeira em
	// execução e é adiada.
	if err := tx.LockChargeRefundClaims(ctx, w.charge.ID); err != nil {
		return false, err
	}
	ops, err := tx.ChargeOperations(ctx, w.charge.ID, OpRefund)
	if err != nil {
		return false, err
	}
	if w.refunds, err = tx.ChargeRefunds(ctx, w.charge.ID); err != nil {
		return false, err
	}
	w.dispatched = map[uuid.UUID]bool{}
	for _, o := range ops {
		if o.RefundID != nil && o.Attempts > 0 {
			w.dispatched[*o.RefundID] = true
		}
	}
	wait := false
	for _, o := range ops {
		if o.ID != op.ID && o.Running(now) {
			wait = true
		}
	}
	for _, x := range w.refunds {
		if op.Attempts == 0 && x.ID != r.ID && x.Open() && w.dispatched[x.ID] {
			wait = true
		}
	}
	// O pedido nunca enviado de cobrança que deixou de aceitar reembolso
	// (disputa ou chargeback) não chega ao provedor e é encerrado na
	// execução (ver refundAtProvider): não aguarda os demais.
	if wait && (w.charge.AcceptsRefund() || op.Attempts > 0) {
		op.NextAttemptAt, op.UpdatedAt = now.Add(refundSerializationDelay), now
		return true, tx.UpdateOperation(ctx, op)
	}
	w.refund = r
	return false, nil
}

// refundResult é o resultado da chamada de reembolso ao provedor.
type refundResult struct {
	outcome  outcome
	refund   *ProviderRefund
	snapshot *ProviderCharge
	err      error
}

func (s *Service) runRefund(ctx context.Context, tenantID uuid.UUID, w opWork) error {
	gw, err := s.resolver.Gateway(w.account)
	if err != nil {
		// Depois de uma tentativa anterior, o pedido pode já estar no
		// provedor: a falha local não comprova a recusa.
		res := refundResult{outcome: outcomePermanent, err: err}
		if refundMayBeAtProvider(w) {
			res.outcome = outcomeAmbiguous
		}
		return s.recordRefundOutcome(ctx, tenantID, w.op.ID, res)
	}
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	res := refundAtProvider(callCtx, gw, w)
	cancel()
	return s.recordRefundOutcome(ctx, tenantID, w.op.ID, res)
}

// refundMayBeAtProvider informa se uma tentativa anterior pode ter levado o
// pedido ao provedor (resposta ambígua, falha repetível seguida de
// interrupção ou lease expirado durante a chamada).
func refundMayBeAtProvider(w opWork) bool {
	return w.op.Attempts > 1 || w.refund.Status == RefundUnknown
}

// refundKind classifica falhas de reembolso: cobrança inexistente no
// provedor é recusa definitiva.
func refundKind(err error) outcome {
	if GatewayErrorKind(err) == ErrKindNotFound {
		return outcomePermanent
	}
	return classify(err)
}

// lookupKind classifica falhas da consulta feita antes de repetir o pedido:
// sem a consulta, nada comprova que o reembolso anterior não foi executado,
// e nenhuma falha (credencial, cobrança não encontrada, validação) encerra o
// reembolso como recusado. Falha repetível apenas adia a nova tentativa.
func lookupKind(err error) outcome {
	if GatewayErrorKind(err) == ErrKindRetryable {
		return outcomeRetryable
	}
	return outcomeAmbiguous
}

// errRefundChargeDisputed encerra o reembolso que não chegou ao provedor
// porque a cobrança entrou em disputa (ou foi estornada por chargeback)
// depois do pedido.
var errRefundChargeDisputed = errors.New("reembolso não enviado ao provedor: a cobrança entrou em disputa (chargeback) depois do pedido")

// refundAtProvider executa o reembolso. Depois de uma tentativa anterior,
// consulta a cobrança no provedor antes de repetir: se o reembolso já consta
// (efetivado, recusado ou em processamento), não repete o pedido. Cobrança
// que entrou em disputa ou foi estornada depois do pedido não recebe o envio
// (o pagador receberia o valor duas vezes, pelo estorno e pelo reembolso): o
// pedido que não está no provedor é encerrado como recusado e a reserva é
// liberada.
func refundAtProvider(ctx context.Context, gw Gateway, w opWork) refundResult {
	if refundMayBeAtProvider(w) {
		pc, err := gw.GetCharge(ctx, w.charge.ProviderChargeID)
		if err != nil {
			return refundResult{outcome: lookupKind(err), err: err}
		}
		if planRefundSync(pc, w.refunds, w.dispatched).covers(w.refund.ID) {
			return refundResult{outcome: outcomeSucceeded, snapshot: &pc}
		}
	}
	if !w.charge.AcceptsRefund() {
		return refundResult{outcome: outcomePermanent, err: errRefundChargeDisputed}
	}
	pr, err := gw.Refund(ctx, RefundRequest{ProviderChargeID: w.charge.ProviderChargeID, Amount: w.refund.Amount,
		Description: w.refund.Reason, ExternalReference: w.refund.ID.String()})
	if err != nil {
		return refundResult{outcome: refundKind(err), err: err}
	}
	return refundResult{outcome: outcomeSucceeded, refund: &pr, snapshot: pr.Charge}
}

// recordRefundOutcome registra o resultado da chamada de reembolso. Pedido
// aceito sem confirmação imediata permanece solicitado até o webhook ou a
// conciliação; resposta ambígua deixa o reembolso com resultado desconhecido
// (a reserva é mantida) e a próxima tentativa consulta o provedor.
func (s *Service) recordRefundOutcome(ctx context.Context, tenantID, opID uuid.UUID, res refundResult) error {
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		op, err := tx.Operation(ctx, opID, true)
		if err != nil {
			return err
		}
		if op.Status == OpSucceeded || op.Status == OpFailed {
			return nil // outra instância concluiu
		}
		c, err := tx.Charge(ctx, op.ChargeID, true)
		if err != nil {
			return err
		}
		r, err := tx.Refund(ctx, *op.RefundID, true)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		op.UpdatedAt, op.LeaseUntil = now, nil
		if !r.Open() {
			// Resolvido durante a chamada (webhook ou consulta).
			op.Status, op.CompletedAt, op.LastError = OpSucceeded, &now, ""
			if r.Status == RefundFailed {
				op.Status = OpFailed
			}
			return tx.UpdateOperation(ctx, op)
		}
		if res.err != nil {
			op.LastError = truncate(res.err.Error(), 1000)
		}
		switch res.outcome {
		case outcomeSucceeded:
			if res.snapshot != nil {
				if err := s.syncRefunds(ctx, tx, &c, *res.snapshot, now, providerActor); err != nil {
					return err
				}
				if r, err = tx.Refund(ctx, r.ID, true); err != nil {
					return err
				}
			}
			if r.Open() && res.refund != nil {
				switch res.refund.State {
				case RefundStateDone:
					if err := s.completeRefund(ctx, tx, &c, &r, res.refund.ProviderRefundID, now, providerActor); err != nil {
						return err
					}
				case RefundStateFailed:
					if err := s.failRefund(ctx, tx, &c, &r, "recusado pelo provedor: "+res.refund.Status, now, providerActor); err != nil {
						return err
					}
				}
			}
			if r.Status == RefundUnknown {
				// O provedor tem o pedido: volta a aguardar a confirmação.
				r.Status, r.UpdatedAt = RefundRequested, now
				if err := tx.UpdateRefund(ctx, &r); err != nil {
					return err
				}
			}
			op.Status, op.CompletedAt, op.LastError = OpSucceeded, &now, ""
			op.ProviderResponse = map[string]any{"refund_status": string(r.Status)}
			if res.refund != nil {
				op.ProviderResponse["provider_refund_id"], op.ProviderResponse["status"] = res.refund.ProviderRefundID, res.refund.Status
			}
			if r.Status == RefundFailed {
				op.Status = OpFailed
			}
		case outcomePermanent:
			if err := s.failRefund(ctx, tx, &c, &r, op.LastError, now, providerActor); err != nil {
				return err
			}
			op.Status, op.CompletedAt = OpFailed, &now
		case outcomeRetryable:
			op.Status, op.NextAttemptAt = OpPending, now.Add(Backoff(op.Attempts))
		default:
			op.Status, op.NextAttemptAt = OpUnknown, now.Add(Backoff(op.Attempts))
			if r.Status == RefundRequested {
				r.Status, r.UpdatedAt = RefundUnknown, now
				if err := tx.UpdateRefund(ctx, &r); err != nil {
					return err
				}
			}
		}
		if err := tx.UpdateCharge(ctx, &c); err != nil {
			return err
		}
		if err := tx.UpdateOperation(ctx, op); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, systemActor, "payment_operation."+string(op.Status), "payment_operation", op.ID.String(),
			map[string]any{"charge_id": op.ChargeID, "refund_id": r.ID, "attempts": op.Attempts, "kind": op.Kind}))
	})
}

// syncRefunds aplica os reembolsos informados pelo provedor à cobrança
// (bloqueada pelo chamador): confirma ou recusa reembolsos locais, registra
// reembolsos feitos diretamente no provedor e conclui as operações dos
// reembolsos resolvidos que não estão em execução. A cobrança é gravada pelo
// chamador.
func (s *Service) syncRefunds(ctx context.Context, tx Tx, c *Charge, pc ProviderCharge, now time.Time, actor audit.Actor) error {
	refunds, err := tx.ChargeRefunds(ctx, c.ID)
	if err != nil {
		return err
	}
	ops, err := tx.ChargeOperations(ctx, c.ID, OpRefund)
	if err != nil {
		return err
	}
	dispatched := map[uuid.UUID]bool{}
	opByRefund := map[uuid.UUID]Operation{}
	for _, o := range ops {
		if o.RefundID == nil {
			continue
		}
		opByRefund[*o.RefundID] = o
		if o.Attempts > 0 {
			dispatched[*o.RefundID] = true
		}
	}
	plan := planRefundSync(pc, refunds, dispatched)
	if plan.empty() {
		return nil
	}
	byID := map[uuid.UUID]Refund{}
	for _, r := range refunds {
		byID[r.ID] = r
	}
	for _, m := range plan.confirm {
		r := byID[m.refundID]
		if err := s.completeRefund(ctx, tx, c, &r, m.providerRefundID, now, actor); err != nil {
			return err
		}
		if err := s.finishRefundOperation(ctx, tx, opByRefund, r, now); err != nil {
			return err
		}
	}
	for _, m := range plan.fail {
		r := byID[m.refundID]
		if err := s.failRefund(ctx, tx, c, &r, "recusado pelo provedor", now, actor); err != nil {
			return err
		}
		if err := s.finishRefundOperation(ctx, tx, opByRefund, r, now); err != nil {
			return err
		}
	}
	for _, m := range plan.reopen {
		r := byID[m.refundID]
		if err := s.confirmFailedRefund(ctx, tx, c, &r, m.providerRefundID, now, actor); err != nil {
			return err
		}
	}
	for _, e := range plan.external {
		if err := s.recordProviderRefund(ctx, tx, c, e, now, actor); err != nil {
			return err
		}
	}
	return nil
}

// finishRefundOperation conclui a operação de um reembolso resolvido por
// webhook ou consulta, exceto quando ela está em execução dentro do lease (o
// registro do resultado da chamada a conclui). Operação que ficou em
// execução depois do lease (processo interrompido) é concluída aqui.
func (s *Service) finishRefundOperation(ctx context.Context, tx Tx, ops map[uuid.UUID]Operation, r Refund, now time.Time) error {
	op, ok := ops[r.ID]
	if !ok || op.Status == OpSucceeded || op.Status == OpFailed || op.Running(now) {
		return nil
	}
	op.Status, op.CompletedAt, op.UpdatedAt, op.LeaseUntil = OpSucceeded, &now, now, nil
	if r.Status == RefundFailed {
		op.Status = OpFailed
	}
	return tx.UpdateOperation(ctx, op)
}

// refundCreditPortion calcula a parte do reembolso que devolve crédito do
// cliente gerado pelo excedente do pagamento desta cobrança, limitada ao
// excedente ainda não devolvido e ao saldo de crédito disponível. O crédito
// do cliente é bloqueado antes da leitura do saldo: a parte calculada é a
// mesma que o consumo valida, mesmo com outra fatura consumindo o crédito ao
// mesmo tempo.
func refundCreditPortion(ctx context.Context, tx Tx, c *Charge, amount money.Money) (money.Money, error) {
	zero := money.Zero(amount.Currency())
	overpaid, used, err := tx.RefundCreditBase(ctx, c.ID)
	if err != nil {
		return money.Money{}, err
	}
	avail, err := overpaid.Sub(used)
	if err != nil || !avail.IsPositive() {
		return zero, err
	}
	if err := tx.LockCustomerCredits(ctx, c.CustomerID); err != nil {
		return money.Money{}, err
	}
	balance, err := tx.CustomerCreditBalance(ctx, c.CustomerID)
	if err != nil || !balance.IsPositive() {
		return zero, err
	}
	part, err := money.Min(amount, avail)
	if err != nil {
		return money.Money{}, err
	}
	return money.Min(part, balance)
}

// completeRefund registra o reembolso efetivado: atualiza cobrança e
// reembolso, devolve a parte de crédito do cliente, lança refund.succeeded
// no ledger e registra a devolução no histórico da fatura, que não é
// reaberta. Ordem de bloqueio: cobrança (já bloqueada), fatura e cliente
// (crédito), a mesma da confirmação de pagamento que gera crédito.
func (s *Service) completeRefund(ctx context.Context, tx Tx, c *Charge, r *Refund, providerRefundID string, now time.Time, actor audit.Actor) error {
	if !r.Open() {
		return nil
	}
	return s.settleRefund(ctx, tx, c, r, r.Origin == RefundOriginRecorvia, providerRefundID, now, actor)
}

// confirmFailedRefund confirma o reembolso encerrado localmente como
// recusado (por exemplo, pela resolução da conciliação) que o provedor passou
// a informar, com associação exata, como efetivado. A reserva já foi
// liberada: o valor precisa caber no saldo elegível atual; se não couber, a
// diferença de totais aparece na conciliação.
func (s *Service) confirmFailedRefund(ctx context.Context, tx Tx, c *Charge, r *Refund, providerRefundID string, now time.Time, actor audit.Actor) error {
	if r.Status != RefundFailed {
		return nil
	}
	elig, err := c.Refundable()
	if err != nil {
		return err
	}
	if cmp, err := r.Amount.Cmp(elig); err != nil || cmp > 0 {
		return err
	}
	previous := r.FailureReason
	if err := s.settleRefund(ctx, tx, c, r, false, providerRefundID, now, actor); err != nil {
		return err
	}
	return tx.Audit(ctx, audit.New(c.TenantID, now, actor, "refund.reopened_by_provider", "refund", r.ID.String(),
		map[string]any{"charge_id": c.ID, "amount": r.Amount.Amount(), "previous_failure_reason": previous}))
}

// settleRefund implementa a confirmação do reembolso; reserved indica que o
// valor ainda está reservado na cobrança.
func (s *Service) settleRefund(ctx context.Context, tx Tx, c *Charge, r *Refund, reserved bool, providerRefundID string, now time.Time, actor audit.Actor) error {
	if err := c.ConfirmRefund(r.Amount, reserved, now); err != nil {
		return err
	}
	inv, err := tx.Invoice(ctx, c.InvoiceID, true)
	if err != nil {
		return err
	}
	fromCredit, err := refundCreditPortion(ctx, tx, c, r.Amount)
	if err != nil {
		return err
	}
	r.Status, r.CreditAmount, r.CompletedAt, r.UpdatedAt, r.FailureReason = RefundSucceeded, fromCredit, &now, now, ""
	if providerRefundID != "" {
		r.ProviderRefundID = providerRefundID
	}
	if err := tx.UpdateRefund(ctx, r); err != nil {
		return err
	}
	status := inv.Status
	details := map[string]any{"refund_id": r.ID, "charge_id": c.ID, "amount": r.Amount.Amount(), "credit_amount": fromCredit.Amount()}
	if err := tx.InsertInvoiceHistory(ctx, inv.ID, actor, []billing.StatusChange{{From: &status, To: status, At: now, Reason: "refund", Details: details}}); err != nil {
		return err
	}
	if fromCredit.IsPositive() {
		neg, err := fromCredit.Neg()
		if err != nil {
			return err
		}
		invID := inv.ID
		if err := tx.GrantCustomerCredit(ctx, c.CustomerID, neg, "consumption", &invID, "Devolvido por reembolso da cobrança "+c.ID.String(), now); err != nil {
			return err
		}
	}
	t, ok, err := ledger.RefundSucceeded(ids.New(), c.TenantID, r.ID, r.Amount, fromCredit, now)
	if err != nil {
		return err
	}
	if ok {
		if err := tx.PostLedger(ctx, t); err != nil {
			return err
		}
	}
	if err := tx.Emit(ctx, outbox.New(c.TenantID, outbox.EventRefundSucceeded, "refund", r.ID,
		map[string]any{"refund_id": r.ID, "charge_id": c.ID, "invoice_id": c.InvoiceID, "amount": r.Amount.Amount(),
			"credit_amount": fromCredit.Amount(), "origin": string(r.Origin)}, now)); err != nil {
		return err
	}
	return tx.Audit(ctx, audit.New(c.TenantID, now, actor, "refund.succeeded", "refund", r.ID.String(),
		map[string]any{"charge_id": c.ID, "amount": r.Amount.Amount(), "credit_amount": fromCredit.Amount(), "provider_refund_id": r.ProviderRefundID}))
}

// failRefund registra o reembolso recusado e libera a reserva. Na cobrança
// estornada por chargeback, o valor liberado passa ao chargeback
// (chargeBackFailedRefund).
func (s *Service) failRefund(ctx context.Context, tx Tx, c *Charge, r *Refund, reason string, now time.Time, actor audit.Actor) error {
	if !r.Open() {
		return nil
	}
	if r.Origin == RefundOriginRecorvia {
		if err := c.ReleaseRefund(r.Amount, now); err != nil {
			return err
		}
		if c.Status == ChargeChargedBack {
			if err := s.chargeBackFailedRefund(ctx, tx, c, *r, now, actor); err != nil {
				return err
			}
		}
	}
	r.Status, r.FailureReason, r.CompletedAt, r.UpdatedAt = RefundFailed, truncate(reason, 500), &now, now
	if err := tx.UpdateRefund(ctx, r); err != nil {
		return err
	}
	if err := tx.Emit(ctx, outbox.New(c.TenantID, outbox.EventRefundFailed, "refund", r.ID,
		map[string]any{"refund_id": r.ID, "charge_id": c.ID, "invoice_id": c.InvoiceID, "amount": r.Amount.Amount()}, now)); err != nil {
		return err
	}
	return tx.Audit(ctx, audit.New(c.TenantID, now, actor, "refund.failed", "refund", r.ID.String(),
		map[string]any{"charge_id": c.ID, "amount": r.Amount.Amount(), "reason": r.FailureReason}))
}

// chargeBackFailedRefund estorna por chargeback o valor do reembolso recusado
// depois da disputa perdida. A disputa contestou esse valor, que ficou fora
// do estorno por estar reservado ao pagador (closeLostDispute); sem o
// reembolso, o valor contestado é debitado pelo chargeback. O lançamento
// dispute.hold_restored devolve a retenção liberada na perda, e chargebacks e
// reembolsos da cobrança continuam somando o valor contestado.
func (s *Service) chargeBackFailedRefund(ctx context.Context, tx Tx, c *Charge, r Refund, now time.Time, actor audit.Actor) error {
	d, err := tx.LatestDispute(ctx, c.ID)
	if err != nil || d == nil || d.Status != DisputeLost {
		return err
	}
	if c.AmountChargedBack, err = c.AmountChargedBack.Add(r.Amount); err != nil {
		return err
	}
	t, ok, err := ledger.ChargebackHoldRestored(ids.New(), c.TenantID, r.ID, r.Amount, now)
	if err != nil {
		return err
	}
	if ok {
		if err := tx.PostLedger(ctx, t); err != nil {
			return err
		}
	}
	return tx.Audit(ctx, audit.New(c.TenantID, now, actor, "dispute.chargeback_increased", "dispute", d.ID.String(),
		map[string]any{"charge_id": c.ID, "refund_id": r.ID, "amount": r.Amount.Amount(), "charged_back_amount": c.AmountChargedBack.Amount()}))
}

// recordProviderRefund registra um reembolso feito diretamente no provedor.
// Valor que não cabe no saldo elegível (por exemplo, com reembolso local
// ainda em andamento) não é registrado: a divergência aparece na conciliação.
func (s *Service) recordProviderRefund(ctx context.Context, tx Tx, c *Charge, e ProviderRefund, now time.Time, actor audit.Actor) error {
	elig, err := c.Refundable()
	if err != nil {
		return err
	}
	cmp, err := e.Amount.Cmp(elig)
	if err != nil {
		return err
	}
	if cmp > 0 {
		return nil
	}
	r := Refund{ID: ids.New(), TenantID: c.TenantID, ChargeID: c.ID, Provider: c.Provider, Origin: RefundOriginProvider, Status: RefundRequested,
		Amount: e.Amount, CreditAmount: money.Zero(e.Amount.Currency()), Reason: "Reembolso efetuado diretamente no provedor",
		ProviderRefundID: e.ProviderRefundID, CreatedAt: now, UpdatedAt: now, Version: 1}
	if err := tx.InsertRefund(ctx, r); err != nil {
		return err
	}
	if err := tx.Audit(ctx, audit.New(c.TenantID, now, actor, "refund.recorded_from_provider", "refund", r.ID.String(),
		map[string]any{"charge_id": c.ID, "amount": r.Amount.Amount(), "provider_refund_id": r.ProviderRefundID})); err != nil {
		return err
	}
	return s.completeRefund(ctx, tx, c, &r, e.ProviderRefundID, now, actor)
}
