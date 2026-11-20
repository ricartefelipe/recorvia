package payments

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// paidFixture devolve uma cobrança de 100,00 paga pelo provedor (taxa 1,99).
func paidFixture(t *testing.T) (*payFixture, billing.Invoice, Charge) {
	t.Helper()
	f := newPayFixture(t)
	inv, c := awaitingCharge(t, f)
	f.process(t, f.storeWebhook(t, confirmedWebhook("evt_paid", c.ProviderChargeID, 10000, 199)))
	c = f.store.charges[c.ID]
	if c.Status != ChargePaid {
		t.Fatalf("cobrança não paga: %s", c.Status)
	}
	f.store.events, f.store.audits, f.store.posted = nil, nil, nil
	return f, f.invoice(inv.ID), c
}

func (f *payFixture) refund(t *testing.T, chargeID uuid.UUID, amount int64, key string) Refund {
	t.Helper()
	r, err := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, &f.userID, RefundInput{ChargeID: chargeID, Amount: brl(amount),
		Reason: "Cancelamento do contrato", RequestKey: key})
	if err != nil {
		t.Fatalf("RequestRefund: %v", err)
	}
	return r
}

func (f *payFixture) refundOperation(t *testing.T, refundID uuid.UUID) Operation {
	t.Helper()
	for _, op := range f.store.operations {
		if op.RefundID != nil && *op.RefundID == refundID {
			return op
		}
	}
	t.Fatalf("operação do reembolso %s não encontrada", refundID)
	return Operation{}
}

func refundWebhook(eventID, providerChargeID string, status ProviderStatus, refunds ...ProviderRefund) WebhookEvent {
	snap := providerSnapshot(status, string(status), 10000, 199, nil)
	snap.ProviderChargeID, snap.Refunds = providerChargeID, refunds
	return WebhookEvent{EventID: eventID, Type: "charge." + string(status), ProviderChargeID: providerChargeID, Status: status, Charge: snap}
}

func TestRequestRefundSucceedsAtProvider(t *testing.T) {
	f, inv, c := paidFixture(t)
	r := f.refund(t, c.ID, 3000, "chave-1")

	if r.Status != RefundSucceeded || r.ProviderRefundID != "rf_1" || r.CompletedAt == nil || r.Origin != RefundOriginRecorvia {
		t.Fatalf("reembolso: %+v", r)
	}
	if len(f.gw.refundCalls) != 1 || f.gw.refundCalls[0].ExternalReference != r.ID.String() || f.gw.refundCalls[0].ProviderChargeID != c.ProviderChargeID ||
		f.gw.refundCalls[0].Amount.Amount() != 3000 {
		t.Fatalf("pedido ao provedor: %+v", f.gw.refundCalls)
	}
	got := f.store.charges[c.ID]
	if got.Status != ChargePartiallyRefunded || got.AmountRefunded.Amount() != 3000 || !got.AmountRefundPending.IsZero() {
		t.Fatalf("cobrança: %s reembolsado=%v reservado=%v", got.Status, got.AmountRefunded, got.AmountRefundPending)
	}
	if op := f.refundOperation(t, r.ID); op.Status != OpSucceeded || op.Attempts != 1 || op.Kind != OpRefund {
		t.Fatalf("operação: %+v", op)
	}
	net := ledgerNet(postedByKind(f.store.posted, "refund.succeeded"))
	if net[ledger.Refunds] != 3000 || net[ledger.ProviderClearing] != -3000 || len(net) != 2 {
		t.Fatalf("ledger: %v", net)
	}
	if f.invoice(inv.ID).Status != billing.Paid {
		t.Fatal("o reembolso não reabre a fatura")
	}
	h := f.store.invoiceHistory[len(f.store.invoiceHistory)-1]
	if h.invoiceID != inv.ID || h.changes[0].Reason != "refund" || h.changes[0].To != billing.Paid || h.changes[0].Details["amount"] != int64(3000) {
		t.Fatalf("histórico da fatura: %+v", h)
	}
	if !slices.Contains(f.store.eventTypes(), outbox.EventRefundSucceeded) {
		t.Fatalf("eventos: %v", f.store.eventTypes())
	}
	actions := f.auditActions()
	if !slices.Contains(actions, "refund.requested") || !slices.Contains(actions, "refund.succeeded") {
		t.Fatalf("auditoria: %v", actions)
	}
	if f.store.audits[0].Actor != f.actor {
		t.Fatal("o pedido é auditado com o usuário")
	}
	// Segundo reembolso completa o valor pago.
	if r2 := f.refund(t, c.ID, 7000, ""); r2.Status != RefundSucceeded || f.store.charges[c.ID].Status != ChargeRefunded {
		t.Fatalf("reembolso integral: %+v %s", r2, f.store.charges[c.ID].Status)
	}
	if got, err := f.svc.Refund(context.Background(), f.tenantID, r.ID); err != nil || got.ID != r.ID {
		t.Fatalf("consulta: %v", err)
	}
}

func TestRequestRefundIsIdempotentByRequestKey(t *testing.T) {
	f, _, c := paidFixture(t)
	first := f.refund(t, c.ID, 3000, "chave-1")
	again := f.refund(t, c.ID, 3000, "chave-1")
	if again.ID != first.ID || len(f.gw.refundCalls) != 1 || len(f.store.refunds) != 1 {
		t.Fatalf("repetição devolve o reembolso registrado: %v %d", again.ID, len(f.gw.refundCalls))
	}
	_, err := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, nil, RefundInput{ChargeID: c.ID, Amount: brl(2000),
		Reason: "Outro motivo", RequestKey: "chave-1"})
	wantAppErr(t, err, apperr.KindConflict, "request_key_reused")
}

func TestRequestRefundRules(t *testing.T) {
	t.Run("validação", func(t *testing.T) {
		f, _, c := paidFixture(t)
		_, err := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, nil, RefundInput{ChargeID: c.ID, Amount: brl(0), Reason: "motivo"})
		wantAppErr(t, err, apperr.KindInvalid, "invalid_refund")
	})
	t.Run("cobrança inexistente", func(t *testing.T) {
		f, _, _ := paidFixture(t)
		_, err := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, nil, RefundInput{ChargeID: uuid.New(), Amount: brl(1), Reason: "motivo"})
		wantAppErr(t, err, apperr.KindNotFound, "charge_not_found")
	})
	t.Run("cobrança sem vínculo no provedor", func(t *testing.T) {
		f := newPayFixture(t)
		inv, c := pendingCharge(t, f, 1)
		_ = inv
		_, err := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, nil, RefundInput{ChargeID: c.ID, Amount: brl(1), Reason: "motivo"})
		wantAppErr(t, err, apperr.KindConflict, "charge_not_refundable")
	})
	t.Run("cobrança aguardando pagamento", func(t *testing.T) {
		f := newPayFixture(t)
		_, c := awaitingCharge(t, f)
		_, err := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, nil, RefundInput{ChargeID: c.ID, Amount: brl(1), Reason: "motivo"})
		wantAppErr(t, err, apperr.KindConflict, "charge_not_refundable")
	})
	t.Run("acima do valor pago", func(t *testing.T) {
		f, _, c := paidFixture(t)
		_, err := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, nil, RefundInput{ChargeID: c.ID, Amount: brl(10001), Reason: "motivo"})
		wantAppErr(t, err, apperr.KindConflict, "refund_exceeds_balance")
		if len(f.store.refunds) != 0 || len(f.gw.refundCalls) != 0 {
			t.Fatal("pedido recusado não grava reembolso")
		}
	})
	t.Run("falhas do armazenamento", func(t *testing.T) {
		for _, m := range []string{"RefundByRequestKey", "ChargeRefunds", "InsertRefund", "UpdateCharge", "InsertOperation", "Audit"} {
			t.Run(m, func(t *testing.T) {
				f, _, c := paidFixture(t)
				boom := errors.New("falha em " + m)
				f.store.errs[m] = boom
				_, err := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, nil, RefundInput{ChargeID: c.ID, Amount: brl(1),
					Reason: "motivo", RequestKey: "k"})
				if !errors.Is(err, boom) || len(f.store.refunds) != 0 || !f.store.charges[c.ID].AmountRefundPending.IsZero() {
					t.Fatalf("falha desfaz o pedido: %v", err)
				}
			})
		}
	})
}

func TestRefundReservationsNeverExceedPaidAmount(t *testing.T) {
	f, _, c := paidFixture(t)
	// O provedor aceita o primeiro pedido, mas ainda não o efetivou.
	f.gw.refundResults = []refundRes{{pr: ProviderRefund{ProviderRefundID: "rf_1", Status: "pending", State: RefundStatePending, Amount: brl(7000)}}}
	first := f.refund(t, c.ID, 7000, "")
	if first.Status != RefundRequested || f.store.charges[c.ID].AmountRefundPending.Amount() != 7000 {
		t.Fatalf("pedido em processamento reserva o valor: %+v", first)
	}
	if op := f.refundOperation(t, first.ID); op.Status != OpSucceeded {
		t.Fatalf("o provedor aceitou o pedido: %s", op.Status)
	}
	_, err := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, nil, RefundInput{ChargeID: c.ID, Amount: brl(3001), Reason: "motivo"})
	wantAppErr(t, err, apperr.KindConflict, "refund_exceeds_balance")
	// O saldo restante é reservado, mas o envio aguarda a confirmação do
	// primeiro reembolso (no máximo um reembolso aberto no provedor).
	second := f.refund(t, c.ID, 3000, "")
	if second.Status != RefundRequested || len(f.gw.refundCalls) != 1 {
		t.Fatalf("saldo restante aguarda o primeiro: %+v (%d chamadas)", second, len(f.gw.refundCalls))
	}
	got := f.store.charges[c.ID]
	if got.AmountRefunded.Amount()+got.AmountRefundPending.Amount() != 10000 {
		t.Fatalf("reembolsado %v + reservado %v", got.AmountRefunded, got.AmountRefundPending)
	}
	if _, err := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, nil, RefundInput{ChargeID: c.ID, Amount: brl(1),
		Reason: "motivo"}); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("saldo esgotado pelas reservas: %v", err)
	}
	// A confirmação do primeiro chega por webhook com a referência do pedido.
	wid := f.storeWebhook(t, refundWebhook("evt_rf", c.ProviderChargeID, ProviderPartiallyRefunded,
		ProviderRefund{ProviderRefundID: "rf_1", ExternalReference: first.ID.String(), State: RefundStateDone, Amount: brl(7000)}))
	f.gw.getResult = *f.verifier.events["evento:evt_rf"].Charge
	f.process(t, wid)
	got = f.store.charges[c.ID]
	if got.Status != ChargePartiallyRefunded || got.AmountRefunded.Amount() != 7000 || got.AmountRefundPending.Amount() != 3000 {
		t.Fatalf("cobrança após a primeira confirmação: %s %v %v", got.Status, got.AmountRefunded, got.AmountRefundPending)
	}
	if f.store.refunds[first.ID].Status != RefundSucceeded {
		t.Fatal("webhook conclui o reembolso pendente")
	}
	// A recuperação envia o segundo pedido depois da espera.
	f.clock.Advance(refundSerializationDelay)
	if _, err := f.svc.RecoverOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	got = f.store.charges[c.ID]
	if f.store.refunds[second.ID].Status != RefundSucceeded || got.Status != ChargeRefunded || got.AmountRefunded.Amount() != 10000 ||
		!got.AmountRefundPending.IsZero() || len(f.gw.refundCalls) != 2 {
		t.Fatalf("cobrança após o segundo reembolso: %s %v %v", got.Status, got.AmountRefunded, got.AmountRefundPending)
	}
}

func TestRefundAmbiguousOutcomeQueriesProviderBeforeRepeating(t *testing.T) {
	f, _, c := paidFixture(t)
	f.gw.refundResults = []refundRes{{err: errAmbiguous}}
	r := f.refund(t, c.ID, 4000, "")
	if r.Status != RefundUnknown || f.store.charges[c.ID].AmountRefundPending.Amount() != 4000 {
		t.Fatalf("resultado desconhecido mantém a reserva: %+v", r)
	}
	op := f.refundOperation(t, r.ID)
	if op.Status != OpUnknown || op.LastError == "" {
		t.Fatalf("operação: %+v", op)
	}
	_, err := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, nil, RefundInput{ChargeID: c.ID, Amount: brl(100), Reason: "motivo"})
	wantAppErr(t, err, apperr.KindConflict, "refund_outcome_unknown")

	// O provedor efetivou o reembolso: a consulta evita repetir o pedido.
	f.gw.getResult = ProviderCharge{ProviderChargeID: c.ProviderChargeID, Status: ProviderPartiallyRefunded, RawStatus: "partially_refunded",
		Amount: brl(10000), Refunds: []ProviderRefund{{ProviderRefundID: "rf_77", ExternalReference: r.ID.String(), State: RefundStateDone, Amount: brl(4000)}}}
	f.clock.Advance(time.Hour)
	if r, err := f.svc.RecoverOperations(context.Background()); err != nil || r.Executed != 1 {
		t.Fatalf("recuperação: %d %v", r.Executed, err)
	}
	if len(f.gw.refundCalls) != 1 || !slices.Equal(f.gw.getCalls, []string{c.ProviderChargeID}) {
		t.Fatalf("consulta antes de repetir: reembolsos=%d consultas=%v", len(f.gw.refundCalls), f.gw.getCalls)
	}
	got := f.store.refunds[r.ID]
	if got.Status != RefundSucceeded || got.ProviderRefundID != "rf_77" || f.refundOperation(t, r.ID).Status != OpSucceeded {
		t.Fatalf("reembolso confirmado pela consulta: %+v %s", got, f.refundOperation(t, r.ID).Status)
	}
	if ch := f.store.charges[c.ID]; ch.AmountRefunded.Amount() != 4000 || !ch.AmountRefundPending.IsZero() {
		t.Fatalf("cobrança: %v %v", ch.AmountRefunded, ch.AmountRefundPending)
	}
}

func TestRefundAmbiguousOutcomeRepeatsWhenProviderHasNoRecord(t *testing.T) {
	f, _, c := paidFixture(t)
	f.gw.refundResults = []refundRes{{err: errAmbiguous}}
	r := f.refund(t, c.ID, 4000, "")
	f.gw.getResult = ProviderCharge{ProviderChargeID: c.ProviderChargeID, Status: ProviderReceived, Amount: brl(10000)}
	f.clock.Advance(time.Hour)
	if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.refundOperation(t, r.ID).ID); err != nil {
		t.Fatal(err)
	}
	if len(f.gw.refundCalls) != 2 || f.gw.refundCalls[1].ExternalReference != r.ID.String() {
		t.Fatalf("repetição com a mesma referência: %+v", f.gw.refundCalls)
	}
	if got := f.store.refunds[r.ID]; got.Status != RefundSucceeded {
		t.Fatalf("reembolso: %+v", got)
	}
}

func TestRefundOutcomes(t *testing.T) {
	t.Run("falha repetível aguarda nova tentativa", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{err: errRetryable}}
		r := f.refund(t, c.ID, 1000, "")
		op := f.refundOperation(t, r.ID)
		if r.Status != RefundRequested || op.Status != OpPending || !op.NextAttemptAt.After(payNow) {
			t.Fatalf("reembolso %s operação %+v", r.Status, op)
		}
		f.clock.Advance(time.Hour)
		if _, err := f.svc.RecoverOperations(context.Background()); err != nil {
			t.Fatal(err)
		}
		if f.store.refunds[r.ID].Status != RefundSucceeded || len(f.gw.getCalls) != 1 {
			t.Fatalf("nova tentativa consulta e conclui: %s %v", f.store.refunds[r.ID].Status, f.gw.getCalls)
		}
	})
	t.Run("recusa libera a reserva", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{err: errPermanent}}
		r := f.refund(t, c.ID, 1000, "")
		got := f.store.charges[c.ID]
		if r.Status != RefundFailed || r.FailureReason == "" || !got.AmountRefundPending.IsZero() || got.Status != ChargePaid {
			t.Fatalf("recusa: %+v %v", r, got.AmountRefundPending)
		}
		if f.refundOperation(t, r.ID).Status != OpFailed || !slices.Contains(f.store.eventTypes(), outbox.EventRefundFailed) {
			t.Fatal("operação e evento de recusa")
		}
	})
	t.Run("cobrança inexistente no provedor é recusa", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{err: &GatewayError{Kind: ErrKindNotFound, StatusCode: 404}}}
		if r := f.refund(t, c.ID, 1000, ""); r.Status != RefundFailed {
			t.Fatalf("reembolso: %s", r.Status)
		}
	})
	t.Run("provedor indisponível na instalação", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.resolver.gwErr = errors.New("sem adaptador")
		if r := f.refund(t, c.ID, 1000, ""); r.Status != RefundFailed {
			t.Fatalf("reembolso: %s", r.Status)
		}
	})
	t.Run("provedor recusa na resposta", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{pr: ProviderRefund{Status: "canceled", State: RefundStateFailed, Amount: brl(1000)}}}
		r := f.refund(t, c.ID, 1000, "")
		if r.Status != RefundFailed || f.refundOperation(t, r.ID).Status != OpFailed {
			t.Fatalf("reembolso: %s", r.Status)
		}
	})
	t.Run("consulta posterior com falha", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{err: errAmbiguous}}
		r := f.refund(t, c.ID, 1000, "")
		f.gw.getErr = errAmbiguous
		f.clock.Advance(time.Hour)
		if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.refundOperation(t, r.ID).ID); err != nil {
			t.Fatal(err)
		}
		if op := f.refundOperation(t, r.ID); op.Status != OpUnknown || op.Attempts != 2 || len(f.gw.refundCalls) != 1 {
			t.Fatalf("sem consulta não há repetição: %+v", op)
		}
	})
	t.Run("resposta pendente com retrato confirma por valor", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.onRefund = func(req RefundRequest) {
			f.gw.refundResults = []refundRes{{pr: ProviderRefund{Status: "RECEIVED", State: RefundStatePending, Amount: req.Amount,
				Charge: &ProviderCharge{ProviderChargeID: c.ProviderChargeID, Status: ProviderPartiallyRefunded, Amount: brl(10000),
					Refunds: []ProviderRefund{{Status: "DONE", State: RefundStateDone, Amount: req.Amount}}}}}}
		}
		r := f.refund(t, c.ID, 2500, "")
		if r.Status != RefundSucceeded || r.ProviderRefundID != "" || f.store.charges[c.ID].AmountRefunded.Amount() != 2500 {
			t.Fatalf("reembolso confirmado pela resposta: %+v", r)
		}
	})
}

func TestRefundPendingAtProviderConfirmedByWebhookWithoutReference(t *testing.T) {
	f, _, c := paidFixture(t)
	f.gw.refundResults = []refundRes{{pr: ProviderRefund{Status: "RECEIVED", State: RefundStatePending, Amount: brl(2000),
		Charge: &ProviderCharge{ProviderChargeID: c.ProviderChargeID, Status: ProviderReceived, Amount: brl(10000),
			Refunds: []ProviderRefund{{Status: "PENDING", State: RefundStatePending, Amount: brl(2000)}}}}}}
	r := f.refund(t, c.ID, 2000, "")
	if r.Status != RefundRequested {
		t.Fatalf("aguarda confirmação: %s", r.Status)
	}
	wid := f.storeWebhook(t, refundWebhook("evt_partial", c.ProviderChargeID, ProviderPartiallyRefunded,
		ProviderRefund{Status: "DONE", State: RefundStateDone, Amount: brl(2000)}))
	f.process(t, wid)
	if got := f.store.refunds[r.ID]; got.Status != RefundSucceeded {
		t.Fatalf("webhook confirma o reembolso por valor: %s", got.Status)
	}
	if len(f.store.refunds) != 1 {
		t.Fatal("o valor não vira reembolso do provedor")
	}
	// Reenvio do mesmo retrato não duplica efeitos.
	f.process(t, f.storeWebhook(t, refundWebhook("evt_partial_2", c.ProviderChargeID, ProviderPartiallyRefunded,
		ProviderRefund{Status: "DONE", State: RefundStateDone, Amount: brl(2000)})))
	if len(postedByKind(f.store.posted, "refund.succeeded")) != 1 || f.store.charges[c.ID].AmountRefunded.Amount() != 2000 {
		t.Fatal("retrato repetido não duplica o reembolso")
	}
}

func TestRefundMadeAtProviderIsRecorded(t *testing.T) {
	f, _, c := paidFixture(t)
	wid := f.storeWebhook(t, refundWebhook("evt_panel", c.ProviderChargeID, ProviderPartiallyRefunded,
		ProviderRefund{ProviderRefundID: "rf_panel", State: RefundStateDone, Amount: brl(1500)}))
	f.process(t, wid)
	refunds := f.store.refundList()
	if len(refunds) != 1 || refunds[0].Origin != RefundOriginProvider || refunds[0].Status != RefundSucceeded || refunds[0].ProviderRefundID != "rf_panel" {
		t.Fatalf("reembolso do provedor: %+v", refunds)
	}
	if got := f.store.charges[c.ID]; got.AmountRefunded.Amount() != 1500 || got.Status != ChargePartiallyRefunded {
		t.Fatalf("cobrança: %s %v", got.Status, got.AmountRefunded)
	}
	if !slices.Contains(f.auditActions(), "refund.recorded_from_provider") {
		t.Fatalf("auditoria: %v", f.auditActions())
	}
	// Repetição com o mesmo ID do provedor não registra de novo.
	f.process(t, f.storeWebhook(t, refundWebhook("evt_panel_2", c.ProviderChargeID, ProviderPartiallyRefunded,
		ProviderRefund{ProviderRefundID: "rf_panel", State: RefundStateDone, Amount: brl(1500)})))
	if len(f.store.refunds) != 1 {
		t.Fatal("duplicado")
	}
	// Valor acima do saldo elegível fica para a conciliação.
	f.process(t, f.storeWebhook(t, refundWebhook("evt_panel_3", c.ProviderChargeID, ProviderPartiallyRefunded,
		ProviderRefund{ProviderRefundID: "rf_panel", State: RefundStateDone, Amount: brl(1500)},
		ProviderRefund{ProviderRefundID: "rf_big", State: RefundStateDone, Amount: brl(9000)})))
	if len(f.store.refunds) != 1 {
		t.Fatal("reembolso acima do saldo não é registrado")
	}
}

func TestRefundConsumesCustomerCreditFromOverpayment(t *testing.T) {
	f, inv, c := paidFixture(t)
	f.store.overpaid[c.ID] = brl(2000)
	f.store.creditBalance[f.customer] = brl(1500)
	r := f.refund(t, c.ID, 3000, "")
	if r.CreditAmount.Amount() != 1500 {
		t.Fatalf("parte do crédito: %v", r.CreditAmount)
	}
	last := f.store.credits[len(f.store.credits)-1]
	if last.amount.Amount() != -1500 || last.kind != "consumption" || *last.invoiceID != inv.ID {
		t.Fatalf("consumo do crédito: %+v", last)
	}
	net := ledgerNet(postedByKind(f.store.posted, "refund.succeeded"))
	if net[ledger.Refunds] != 1500 || net[ledger.CustomerCredits] != 1500 || net[ledger.ProviderClearing] != -3000 {
		t.Fatalf("ledger: %v", net)
	}
	// O excedente já devolvido não é usado de novo.
	f.store.creditBalance[f.customer] = brl(5000)
	if r2 := f.refund(t, c.ID, 1000, ""); r2.CreditAmount.Amount() != 500 {
		t.Fatalf("restante do excedente: %v", r2.CreditAmount)
	}
	if r3 := f.refund(t, c.ID, 1000, ""); !r3.CreditAmount.IsZero() {
		t.Fatalf("sem excedente: %v", r3.CreditAmount)
	}
	for _, m := range []string{"RefundCreditBase", "CustomerCreditBalance", "GrantCustomerCredit"} {
		t.Run("falha em "+m, func(t *testing.T) {
			f, _, c := paidFixture(t)
			f.store.overpaid[c.ID] = brl(2000)
			f.store.creditBalance[f.customer] = brl(1500)
			boom := errors.New(m)
			f.store.errs[m] = boom
			r := f.refund(t, c.ID, 3000, "")
			if r.Status != RefundRequested || f.refundOperation(t, r.ID).Status != OpInFlight {
				t.Fatalf("falha ao registrar mantém a operação para a recuperação: %+v", r)
			}
		})
	}
}

// Conflito ao aplicar o webhook desfaz todos os efeitos da transação: o
// reembolso confirmado antes do consumo do crédito não fica gravado sem a
// cobrança, o ledger e o evento, a reserva não fica presa e o retrato
// seguinte confirma o reembolso.
func TestWebhookConflictRollsBackPartialEffects(t *testing.T) {
	f, _, c := paidFixture(t)
	f.store.overpaid[c.ID] = brl(2000)
	f.store.creditBalance[f.customer] = brl(1500)
	f.gw.refundResults = []refundRes{{pr: ProviderRefund{Status: "PENDING", State: RefundStatePending, Amount: brl(3000)}}}
	r := f.refund(t, c.ID, 3000, "")
	historyBefore := len(f.store.invoiceHistory)
	// Outra fatura consome o crédito do cliente ao mesmo tempo: o consumo
	// pelo reembolso é recusado.
	f.store.errs["GrantCustomerCredit"] = apperr.Conflict("insufficient_credit", "Saldo de crédito insuficiente.")
	done := ProviderRefund{ExternalReference: r.ID.String(), State: RefundStateDone, Amount: brl(3000)}
	wid := f.storeWebhook(t, refundWebhook("evt_done", c.ProviderChargeID, ProviderPartiallyRefunded, done))
	msg := f.process(t, wid)
	if w := f.store.webhooks[wid]; w.Status != "ignored" || !strings.Contains(w.Error, "insufficient_credit") ||
		!f.store.inbox[inboxKey(ConsumerWebhooks, msg)] {
		t.Fatalf("webhook ignorado e registrado no inbox: %s %q", w.Status, w.Error)
	}
	if got := f.store.refunds[r.ID]; got.Status != RefundRequested || !got.CreditAmount.IsZero() || got.CompletedAt != nil {
		t.Fatalf("reembolso sem efeito parcial: %+v", got)
	}
	if ch := f.store.charges[c.ID]; ch.AmountRefunded.IsPositive() || ch.AmountRefundPending.Amount() != 3000 || ch.Status != ChargePaid {
		t.Fatalf("cobrança: %s reembolsado=%v reservado=%v", ch.Status, ch.AmountRefunded, ch.AmountRefundPending)
	}
	if len(f.store.invoiceHistory) != historyBefore || len(postedByKind(f.store.posted, "refund.succeeded")) != 0 ||
		slices.Contains(f.store.eventTypes(), outbox.EventRefundSucceeded) {
		t.Fatalf("histórico, ledger e eventos sem efeito parcial: %d %v %v", len(f.store.invoiceHistory)-historyBefore,
			f.store.ledgerKinds(), f.store.eventTypes())
	}
	// O crédito foi consumido pela outra fatura: o retrato seguinte confirma
	// o reembolso sem a parte do crédito.
	delete(f.store.errs, "GrantCustomerCredit")
	f.store.creditBalance[f.customer] = brl(0)
	wid = f.storeWebhook(t, refundWebhook("evt_done_2", c.ProviderChargeID, ProviderPartiallyRefunded, done))
	f.process(t, wid)
	got := f.store.refunds[r.ID]
	if f.store.webhooks[wid].Status != "processed" || got.Status != RefundSucceeded || !got.CreditAmount.IsZero() {
		t.Fatalf("confirmação seguinte: %s %+v", f.store.webhooks[wid].Status, got)
	}
	if ch := f.store.charges[c.ID]; ch.AmountRefunded.Amount() != 3000 || !ch.AmountRefundPending.IsZero() ||
		len(postedByKind(f.store.posted, "refund.succeeded")) != 1 {
		t.Fatalf("cobrança e ledger: reembolsado=%v reservado=%v %v", ch.AmountRefunded, ch.AmountRefundPending, f.store.ledgerKinds())
	}
	t.Run("falha ao marcar o webhook ignorado", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.store.errs["UpdateCharge"] = apperr.Conflict("x", "x")
		wid := f.storeWebhook(t, refundWebhook("evt_x", c.ProviderChargeID, ProviderPartiallyRefunded,
			ProviderRefund{ProviderRefundID: "rf_painel", State: RefundStateDone, Amount: brl(1000)}))
		f.store.errs["UpdateWebhookStatus"] = errors.New("banco indisponível")
		if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, uuid.New()); err == nil {
			t.Fatal("esperava erro")
		}
		if f.store.webhooks[wid].Status != "received" || len(f.store.refunds) != 0 {
			t.Fatal("nada gravado")
		}
	})
}

// O crédito do cliente é bloqueado antes da leitura do saldo usado na parte
// do reembolso que devolve crédito, para que o consumo valide o mesmo saldo.
func TestRefundCreditPortionLocksCreditBeforeReadingBalance(t *testing.T) {
	f, _, c := paidFixture(t)
	f.store.overpaid[c.ID] = brl(2000)
	f.store.creditBalance[f.customer] = brl(1500)
	f.store.creditSteps = nil
	if r := f.refund(t, c.ID, 3000, ""); r.Status != RefundSucceeded || r.CreditAmount.Amount() != 1500 {
		t.Fatalf("reembolso: %+v", r)
	}
	if !slices.Equal(f.store.creditSteps, []string{"lock", "balance"}) {
		t.Fatalf("ordem: %v", f.store.creditSteps)
	}
	f.store.errs["LockCustomerCredits"] = errors.New("bloqueio indisponível")
	if r := f.refund(t, c.ID, 1000, ""); r.Status != RefundRequested {
		t.Fatalf("falha no bloqueio mantém o reembolso para a recuperação: %s", r.Status)
	}
}

func TestRefundClaimCases(t *testing.T) {
	t.Run("reembolso já resolvido conclui a operação", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{err: errAmbiguous}}
		r := f.refund(t, c.ID, 1000, "")
		f.process(t, f.storeWebhook(t, refundWebhook("evt_ok", c.ProviderChargeID, ProviderPartiallyRefunded,
			ProviderRefund{ProviderRefundID: "rf_1", ExternalReference: r.ID.String(), State: RefundStateDone, Amount: brl(1000)})))
		if f.store.refunds[r.ID].Status != RefundSucceeded || f.refundOperation(t, r.ID).Status != OpSucceeded {
			t.Fatalf("webhook conclui reembolso e operação: %s %s", f.store.refunds[r.ID].Status, f.refundOperation(t, r.ID).Status)
		}
		op := f.refundOperation(t, r.ID)
		op.Status = OpPending
		f.store.operations[op.ID] = op
		f.clock.Advance(time.Hour)
		if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID); err != nil {
			t.Fatal(err)
		}
		if f.refundOperation(t, r.ID).Status != OpSucceeded || len(f.gw.refundCalls) != 1 {
			t.Fatal("sem nova chamada")
		}
	})
	t.Run("reembolso recusado conclui a operação como falha", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{err: errAmbiguous}}
		r := f.refund(t, c.ID, 1000, "")
		f.process(t, f.storeWebhook(t, refundWebhook("evt_no", c.ProviderChargeID, ProviderReceived,
			ProviderRefund{ExternalReference: r.ID.String(), State: RefundStateFailed, Amount: brl(1000)})))
		if f.store.refunds[r.ID].Status != RefundFailed || f.refundOperation(t, r.ID).Status != OpFailed {
			t.Fatalf("recusa: %s %s", f.store.refunds[r.ID].Status, f.refundOperation(t, r.ID).Status)
		}
		op := f.refundOperation(t, r.ID)
		op.Status = OpUnknown
		f.store.operations[op.ID] = op
		f.clock.Advance(time.Hour)
		if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID); err != nil || f.refundOperation(t, r.ID).Status != OpFailed {
			t.Fatalf("operação: %v %s", err, f.refundOperation(t, r.ID).Status)
		}
	})
	t.Run("um reembolso por vez no provedor", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{err: errRetryable}}
		first := f.refund(t, c.ID, 1000, "")
		op1 := f.refundOperation(t, first.ID)
		op1.Status = OpInFlight
		f.store.operations[op1.ID] = op1
		second := f.refund(t, c.ID, 2000, "")
		op2 := f.refundOperation(t, second.ID)
		if second.Status != RefundRequested || op2.Attempts != 0 || !op2.NextAttemptAt.Equal(payNow.Add(refundSerializationDelay)) {
			t.Fatalf("segundo pedido aguarda o primeiro: %s %+v", second.Status, op2)
		}
		if len(f.gw.refundCalls) != 1 {
			t.Fatal("sem chamada concorrente")
		}
	})
	t.Run("operação sem reembolso vinculado", func(t *testing.T) {
		f, _, c := paidFixture(t)
		op := Operation{ID: uuid.New(), TenantID: f.tenantID, ChargeID: c.ID, Kind: OpRefund, Status: OpPending, NextAttemptAt: payNow}
		f.store.operations[op.ID] = op
		if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID); err == nil {
			t.Fatal("esperava erro")
		}
	})
	for _, m := range []string{"Refund", "ChargeOperations", "ChargeRefunds"} {
		t.Run("falha ao reivindicar: "+m, func(t *testing.T) {
			f, _, c := paidFixture(t)
			f.gw.refundResults = []refundRes{{err: errRetryable}}
			r := f.refund(t, c.ID, 1000, "")
			boom := errors.New(m)
			f.store.errs[m] = boom
			f.clock.Advance(time.Hour)
			if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.refundOperation(t, r.ID).ID); !errors.Is(err, boom) {
				t.Fatalf("erro: %v", err)
			}
		})
	}
}

func TestRecordRefundOutcomeFailures(t *testing.T) {
	for _, m := range []string{"Operation", "Charge", "Refund", "UpdateRefund", "UpdateCharge", "UpdateOperation", "Audit", "PostLedger",
		"InsertInvoiceHistory", "Emit", "Invoice"} {
		t.Run(m, func(t *testing.T) {
			f, _, c := paidFixture(t)
			boom := errors.New(m)
			f.gw.onRefund = func(RefundRequest) { f.store.errs[m] = boom }
			r, err := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, nil, RefundInput{ChargeID: c.ID, Amount: brl(1000), Reason: "motivo"})
			delete(f.store.errs, m)
			if err != nil && !errors.Is(err, boom) {
				t.Fatalf("erro inesperado: %v", err)
			}
			if err == nil && r.Status == RefundSucceeded {
				t.Fatalf("falha no registro do resultado não conclui o reembolso: %+v", r)
			}
			if f.store.charges[c.ID].AmountRefunded.IsPositive() {
				t.Fatal("sem efeito parcial")
			}
		})
	}
	t.Run("recusa com falha ao liberar", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.onRefund = func(RefundRequest) {
			ch := f.store.charges[c.ID]
			ch.AmountRefundPending = brl(0)
			f.store.charges[c.ID] = ch
		}
		f.gw.refundResults = []refundRes{{err: errPermanent}}
		r, _ := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, nil, RefundInput{ChargeID: c.ID, Amount: brl(1000), Reason: "motivo"})
		if r.Status == RefundFailed {
			t.Fatal("reserva inconsistente impede a recusa")
		}
	})
	t.Run("resultado desconhecido com falha ao gravar", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{err: errAmbiguous}}
		f.gw.onRefund = func(RefundRequest) { f.store.errs["UpdateRefund"] = errors.New("x") }
		r, _ := f.svc.RequestRefund(context.Background(), f.tenantID, f.actor, nil, RefundInput{ChargeID: c.ID, Amount: brl(1000), Reason: "motivo"})
		if r.Status == RefundUnknown {
			t.Fatal("falha desfaz o registro")
		}
	})
	t.Run("operação concluída por outra instância", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.onRefund = func(req RefundRequest) {
			for id, op := range f.store.operations {
				if op.Kind == OpRefund {
					op.Status = OpSucceeded
					f.store.operations[id] = op
				}
			}
		}
		r := f.refund(t, c.ID, 1000, "")
		if r.Status != RefundRequested {
			t.Fatalf("resultado ignorado: %s", r.Status)
		}
	})
}

// A reivindicação da operação de reembolso serializa os reembolsos da
// cobrança; sem o bloqueio, a operação não é enviada ao provedor e fica para
// a recuperação.
func TestRefundClaimSerializesByCharge(t *testing.T) {
	f, _, c := paidFixture(t)
	r := f.refund(t, c.ID, 3000, "serializa-1")
	if r.Status != RefundSucceeded || !slices.Contains(f.store.refundClaimLocks, c.ID) {
		t.Fatalf("reembolso: %s bloqueios=%v", r.Status, f.store.refundClaimLocks)
	}
	f.store.errs["LockChargeRefundClaims"] = errors.New("banco indisponível")
	pending := f.refund(t, c.ID, 2000, "serializa-2")
	op := f.refundOperation(t, pending.ID)
	if pending.Status != RefundRequested || op.Status != OpPending || op.Attempts != 0 || len(f.gw.refundCalls) != 1 {
		t.Fatalf("reembolso sem bloqueio: %s operação=%s tentativas=%d chamadas=%d", pending.Status, op.Status, op.Attempts, len(f.gw.refundCalls))
	}
	delete(f.store.errs, "LockChargeRefundClaims")
	if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.store.refunds[pending.ID]; got.Status != RefundSucceeded || len(f.gw.refundCalls) != 2 {
		t.Fatalf("reembolso após a recuperação: %s chamadas=%d", got.Status, len(f.gw.refundCalls))
	}
}
