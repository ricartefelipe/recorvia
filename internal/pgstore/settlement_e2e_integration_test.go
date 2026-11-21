//go:build integration

package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/paysim"
)

// Ciclo do pagamento pelo provedor no ledger com o simulador real (ADR-080):
// confirmado (a receber do provedor), recebido (bruto nos valores em
// liquidação, taxa reconhecida) e liquidado (líquido no disponível), com
// webhooks em ordem, fora de ordem e repetidos, sempre com o ledger
// balanceado.

// cycle confere os saldos das contas do ciclo do pagamento.
func (h *payHarness) cycle(t *testing.T, stage string, confirmed, clearing, cash, fees int64) {
	t.Helper()
	got := map[string]int64{"provider_confirmed": h.accountNet(t, "provider_confirmed"), "provider_clearing": h.accountNet(t, "provider_clearing"),
		"cash": h.accountNet(t, "cash"), "provider_fees": h.accountNet(t, "provider_fees"), "accounts_receivable": h.accountNet(t, "accounts_receivable")}
	want := map[string]int64{"provider_confirmed": confirmed, "provider_clearing": clearing, "cash": cash, "provider_fees": fees, "accounts_receivable": 0}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %s = %d, esperado %d (%v)", stage, k, got[k], v, got)
		}
	}
	h.assertLedgerBalanced(t)
}

// kinds conta os lançamentos do tenant por tipo.
func (h *payHarness) kinds(t *testing.T, kind string) int64 {
	t.Helper()
	return h.sum(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND kind = '`+kind+`'`)
}

// awaitingCharge emite a fatura e cria a cobrança no simulador.
func (h *payHarness) awaitingCharge(t *testing.T, amount int64) (billing.Invoice, payments.Charge) {
	t.Helper()
	inv := h.issueInvoice(t, amount)
	c, err := h.pay.RequestCharge(context.Background(), h.tn.ID, inv.ID, h.tn.Actor)
	if err != nil || c == nil {
		t.Fatalf("cobrança: %v", err)
	}
	return inv, *c
}

func TestProviderPaymentCycleWithSimulator(t *testing.T) {
	const amount = 10000
	fee := paysim.Fee(amount)
	net := amount - fee
	assertReceived := func(t *testing.T, h *payHarness) {
		t.Helper()
		h.cycle(t, "recebido", 0, 0, net, fee)
		for kind, want := range map[string]int64{"payment.confirmed": 1, "payment.received": 1, "settlement.received": 1} {
			if n := h.kinds(t, kind); n != want {
				t.Fatalf("lançamentos %s = %d, esperado %d", kind, n, want)
			}
		}
		if n := h.sum(t, `SELECT count(*) FROM payments WHERE tenant_id = $1`); n != 1 {
			t.Fatalf("pagamentos = %d", n)
		}
		// Todos os lançamentos do ciclo referenciam o pagamento.
		if n := h.sum(t, `SELECT count(*) FROM ledger_transactions t JOIN payments p ON p.tenant_id = t.tenant_id AND p.id = t.reference_id
		                   WHERE t.tenant_id = $1 AND t.reference_type = 'payment'
		                     AND t.kind IN ('payment.confirmed', 'payment.received', 'settlement.received')`); n != 3 {
			t.Fatalf("lançamentos do pagamento = %d", n)
		}
	}

	t.Run("confirmado e depois recebido", func(t *testing.T) {
		h := newPayHarness(t, 2*time.Second)
		inv, c := h.awaitingCharge(t, amount)
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "confirm"})
		h.deliver(t)
		if got := h.charge(t, inv.ID); got.Status != payments.ChargePaid {
			t.Fatalf("cobrança confirmada: %s", got.Status)
		}
		// Confirmado e ainda não creditado: o bruto fica a receber do provedor
		// e já conta em Recebido; nada nos valores em liquidação nem no
		// disponível, e a taxa ainda não foi retida.
		h.cycle(t, "confirmado", amount, 0, 0, 0)
		d := h.dashboard(t, "2026-03")
		if d.Received != amount || d.PendingProviderReceipt != amount {
			t.Fatalf("painel confirmado: recebido=%d a receber do provedor=%d", d.Received, d.PendingProviderReceipt)
		}
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "receive"})
		h.deliver(t)
		assertReceived(t, h)
		d = h.dashboard(t, "2026-03")
		if d.Received != amount || d.PendingProviderReceipt != 0 {
			t.Fatalf("painel recebido: recebido=%d a receber do provedor=%d", d.Received, d.PendingProviderReceipt)
		}

		// O reembolso sai do disponível pela liquidação; os valores em
		// liquidação continuam zerados.
		if _, err := h.pay.RequestRefund(context.Background(), h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, refundInput(c, 3000, "reembolso-ciclo")); err != nil {
			t.Fatal(err)
		}
		h.deliver(t)
		h.cycle(t, "reembolsado", 0, 0, net-3000, fee)
		if h.accountNet(t, "refunds") != 3000 || h.kinds(t, "settlement.debited") != 1 {
			t.Fatalf("reembolso: reembolsos=%d liquidações=%d", h.accountNet(t, "refunds"), h.kinds(t, "settlement.debited"))
		}
	})

	t.Run("recebido direto", func(t *testing.T) {
		h := newPayHarness(t, 2*time.Second)
		_, c := h.awaitingCharge(t, amount)
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "pay"})
		h.deliver(t)
		assertReceived(t, h)
	})

	t.Run("webhooks repetidos", func(t *testing.T) {
		h := newPayHarness(t, 2*time.Second)
		_, c := h.awaitingCharge(t, amount)
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "confirm"})
		h.deliver(t)
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "receive"})
		h.deliver(t)
		// Reentrega de todos os eventos da cobrança: recebidos como
		// duplicados, sem novo lançamento.
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "replay"})
		h.deliver(t)
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "replay"})
		h.deliver(t)
		assertReceived(t, h)
	})

	t.Run("fora de ordem: recebido processado antes do confirmado", func(t *testing.T) {
		h := newPayHarness(t, 2*time.Second)
		_, c := h.awaitingCharge(t, amount)
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "confirm"})
		h.receive(t)
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "receive"})
		h.receive(t)
		h.processWebhooksReversed(t)
		assertReceived(t, h)
		if n := h.sum(t, `SELECT count(*) FROM provider_webhook_events WHERE tenant_id = $1 AND status = 'processed'`); n < 2 {
			t.Fatalf("webhooks processados = %d", n)
		}
	})

	t.Run("cenário duplicate_webhooks do simulador", func(t *testing.T) {
		h := newPayHarness(t, 2*time.Second)
		if err := h.sim.EnqueueScenario(h.apiKey, "duplicate_webhooks"); err != nil {
			t.Fatal(err)
		}
		_, c := h.awaitingCharge(t, amount)
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "confirm"})
		h.deliver(t)
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "receive"})
		h.deliver(t)
		assertReceived(t, h)
	})
}

// Chargeback de pagamento só confirmado: o recebimento é registrado antes da
// retenção (situação posterior ao recebimento), e a disputa ganha devolve a
// retenção ao disponível.
func TestChargebackOfConfirmedPaymentWithSimulator(t *testing.T) {
	const amount = 10000
	fee := paysim.Fee(amount)
	h := newPayHarness(t, 2*time.Second)
	_, c := h.awaitingCharge(t, amount)
	h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "confirm"})
	h.deliver(t)
	h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "receive"})
	h.receive(t) // recebido entregue, ainda não processado
	h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: "requested"})
	h.receive(t)
	h.processWebhooksReversed(t)
	h.cycle(t, "chargeback aberto", 0, 0, amount-fee-amount, fee)
	if h.accountNet(t, "chargebacks") != amount || h.kinds(t, "payment.received") != 1 {
		t.Fatalf("chargeback: %d recebimentos=%d", h.accountNet(t, "chargebacks"), h.kinds(t, "payment.received"))
	}
	h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: "won"})
	h.deliver(t)
	h.cycle(t, "disputa ganha", 0, 0, amount-fee, fee)
	if h.accountNet(t, "chargebacks") != 0 {
		t.Fatalf("chargebacks depois da disputa ganha: %d", h.accountNet(t, "chargebacks"))
	}
}

// Reembolso e chargeback de pagamento confirmado e ainda não recebido no
// provedor (o cartão no prazo de recebimento), pelo caminho completo do
// Recorvia com o simulador, que aceita as duas movimentações nessa situação
// como o provedor real. O pagamento é dado como recebido antes da
// movimentação (ADR-080): o bruto sai de pagamentos confirmados para os
// valores em liquidação, a taxa é reconhecida, o líquido vai ao disponível e
// a movimentação é liquidada no disponível. No fim, pagamentos confirmados e
// valores em liquidação ficam zerados e o disponível guarda o líquido menos a
// movimentação.
func TestMovementsOfConfirmedNotReceivedPaymentWithSimulator(t *testing.T) {
	const amount, refunded = 10000, 3000
	fee := paysim.Fee(amount)
	net := int64(amount) - fee

	t.Run("reembolso parcial", func(t *testing.T) {
		h := newPayHarness(t, 2*time.Second)
		inv, c := h.awaitingCharge(t, amount)
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "confirm"})
		h.deliver(t)
		h.cycle(t, "confirmado", amount, 0, 0, 0)

		r, err := h.pay.RequestRefund(context.Background(), h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, refundInput(c, refunded, "reembolso-confirmado"))
		if err != nil {
			t.Fatal(err)
		}
		h.deliver(t) // charge.partially_refunded do simulador, já refletido
		got, err := h.pay.Refund(context.Background(), h.tn.ID, r.ID)
		if err != nil || got.Status != payments.RefundSucceeded {
			t.Fatalf("reembolso da cobrança confirmada: %+v %v", got, err)
		}
		if ch := h.charge(t, inv.ID); ch.AmountRefunded.Amount() != refunded || !ch.AmountRefundPending.IsZero() {
			t.Fatalf("cobrança: reembolsado=%v reservado=%v", ch.AmountRefunded, ch.AmountRefundPending)
		}
		view, err := h.sim.View(h.apiKey, c.ProviderChargeID)
		if err != nil || view.Status != "partially_refunded" || view.RefundedAmount != refunded {
			t.Fatalf("cobrança no simulador: %+v %v", view, err)
		}

		// Conta a conta: confirmado 10.000 - 10.000 = 0; em liquidação
		// +10.000 - taxa - líquido - reembolso + reembolso = 0; disponível
		// líquido - reembolso; taxas e reembolsos reconhecidos.
		h.cycle(t, "reembolsado", 0, 0, net-refunded, fee)
		if got := h.accountNet(t, "refunds"); got != refunded {
			t.Fatalf("reembolsos = %d, esperado %d", got, refunded)
		}
		for kind, want := range map[string]int64{"payment.confirmed": 1, "payment.received": 1, "settlement.received": 1,
			"refund.succeeded": 1, "settlement.debited": 1} {
			if n := h.kinds(t, kind); n != want {
				t.Fatalf("lançamentos %s = %d, esperado %d", kind, n, want)
			}
		}
		if d := h.dashboard(t, "2026-03"); d.Received != amount || d.PendingProviderReceipt != 0 || d.Refunds != refunded {
			t.Fatalf("painel: recebido=%d a receber do provedor=%d reembolsos=%d", d.Received, d.PendingProviderReceipt, d.Refunds)
		}

		// A reentrega dos eventos da cobrança não lança nada de novo.
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "replay"})
		h.deliver(t)
		h.cycle(t, "reentregue", 0, 0, net-refunded, fee)
		if n := h.kinds(t, "refund.succeeded"); n != 1 {
			t.Fatalf("reembolsos lançados depois da reentrega: %d", n)
		}
	})

	t.Run("chargeback e disputa ganha", func(t *testing.T) {
		h := newPayHarness(t, 2*time.Second)
		_, c := h.awaitingCharge(t, amount)
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "confirm"})
		h.deliver(t)
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: "requested"})
		h.deliver(t)
		h.cycle(t, "chargeback aberto", 0, 0, net-amount, fee)
		if h.accountNet(t, "chargebacks") != amount || h.kinds(t, "payment.received") != 1 {
			t.Fatalf("chargeback: %d recebimentos=%d", h.accountNet(t, "chargebacks"), h.kinds(t, "payment.received"))
		}
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: "won"})
		h.deliver(t)
		h.cycle(t, "disputa ganha", 0, 0, net, fee)
		if h.accountNet(t, "chargebacks") != 0 {
			t.Fatalf("chargebacks depois da disputa ganha: %d", h.accountNet(t, "chargebacks"))
		}
	})
}

// Recebimento perdido com o simulador: a cobrança é confirmada e o crédito
// acontece no provedor sem que o webhook de recebimento seja processado. A
// conciliação aponta payment_not_reflected na direção provider_received, e a
// adoção do provedor lança o recebimento e a liquidação, zerando "A receber
// do provedor" com o ledger balanceado.
func TestReconciliationAppliesMissedReceiptWithSimulator(t *testing.T) {
	const amount = 10000
	fee := paysim.Fee(amount)
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	_, c := h.awaitingCharge(t, amount)
	h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "confirm"})
	h.deliver(t)
	h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "receive"}) // webhook não entregue
	h.cycle(t, "confirmado", amount, 0, 0, 0)

	h.run(t)
	open := h.openDiscrepancies(t)
	d, ok := open[payments.DiscrepancyPaymentNotReflected]
	if !ok || d.DedupeKey != "payment_not_reflected:"+c.ID.String()+":provider_received" || d.LocalValue["direction"] != "provider_received" ||
		d.LocalAmount == nil || !d.LocalAmount.IsZero() || d.ProviderAmount == nil || d.ProviderAmount.Amount() != amount {
		t.Fatalf("recebimento perdido: %+v (%v)", d, open)
	}
	if len(open) != 1 {
		t.Fatalf("somente a pendência de recebimento: %v", open)
	}
	got, err := h.pay.ResolveDiscrepancy(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, payments.ResolveInput{ID: d.ID,
		Resolution: payments.ResolutionAcceptProvider, Justification: "Crédito conferido no painel do provedor.",
		ExpectedVersion: discrepancyVersion(t, h.pay, h.tn.ID, d.ID)})
	if err != nil || got.Status != payments.DiscrepancyResolved {
		t.Fatalf("adoção do provedor: %+v %v", got, err)
	}
	h.cycle(t, "recebido pela conciliação", 0, 0, amount-fee, fee)
	if dash := h.dashboard(t, "2026-03"); dash.PendingProviderReceipt != 0 || dash.Received != amount {
		t.Fatalf("painel: %+v", dash)
	}
	// O webhook atrasado chega depois: nada é lançado de novo.
	h.deliver(t)
	h.cycle(t, "webhook atrasado", 0, 0, amount-fee, fee)
	if n := h.kinds(t, "payment.received"); n != 1 {
		t.Fatalf("recebimentos = %d", n)
	}
	h.run(t)
	if open := h.openDiscrepancies(t); len(open) != 0 {
		t.Fatalf("sem divergências depois da adoção: %v", open)
	}
}
