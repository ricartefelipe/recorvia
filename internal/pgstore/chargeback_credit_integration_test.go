//go:build integration

package pgstore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/paysim"
)

// overpaidCharge emite a fatura de 100,00 e confirma no simulador o pagamento
// de 125,00: os 25,00 excedentes viram crédito do cliente.
func (h *payHarness) overpaidCharge(t *testing.T) (billing.Invoice, payments.Charge) {
	t.Helper()
	ctx := context.Background()
	inv := h.issueInvoice(t, 10000)
	c, err := h.pay.RequestCharge(ctx, h.tn.ID, inv.ID, h.tn.Actor)
	if err != nil || c == nil {
		t.Fatalf("cobrança: %v", err)
	}
	if _, err := h.sim.Apply(c.ProviderChargeID, paysim.Action{Name: "pay", Amount: 12500}); err != nil {
		t.Fatal(err)
	}
	h.deliver(t)
	got := h.charge(t, inv.ID)
	if got.Status != payments.ChargePaid || got.AmountPaid.Amount() != 12500 || h.creditBalance(t, inv.CustomerID) != 2500 {
		t.Fatalf("pagamento excedente: %s pago=%v crédito=%d", got.Status, got.AmountPaid, h.creditBalance(t, inv.CustomerID))
	}
	return inv, got
}

func (h *payHarness) creditBalance(t *testing.T, customerID uuid.UUID) int64 {
	t.Helper()
	bal, _, err := h.env.Store.Customers().Credits(context.Background(), h.tn.ID, customerID)
	if err != nil {
		t.Fatal(err)
	}
	return bal.Amount()
}

// issueInvoiceFor emite outra fatura para o mesmo cliente: o motor aplica o
// crédito disponível na emissão.
func (h *payHarness) issueInvoiceFor(t *testing.T, customerID uuid.UUID, amount int64) billing.Invoice {
	t.Helper()
	plan := h.env.NewFlatPlan(t, h.tn, amount)
	sub := h.env.NewSubscription(t, h.tn, customerID, plan.ID, nil)
	if _, err := h.env.Engine.RunTenant(context.Background(), h.tn.ID, h.tn.Actor); err != nil {
		t.Fatal(err)
	}
	pg, err := h.env.Billing.List(context.Background(), h.tn.ID, billing.ListFilter{SubscriptionID: &sub.ID, Limit: 10})
	if err != nil || len(pg.Items) != 1 {
		t.Fatalf("fatura não emitida: %v %d", err, len(pg.Items))
	}
	return pg.Items[0]
}

func (h *payHarness) chargeback(t *testing.T, c payments.Charge, stages ...string) {
	t.Helper()
	for _, stage := range stages {
		if _, err := h.sim.Apply(c.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: stage}); err != nil {
			t.Fatal(err)
		}
		h.deliver(t)
	}
}

func (h *payHarness) dispute(t *testing.T, chargeID uuid.UUID) payments.Dispute {
	t.Helper()
	d, err := h.pay.LatestDispute(context.Background(), h.tn.ID, chargeID)
	if err != nil || d == nil {
		t.Fatalf("disputa: %v %v", d, err)
	}
	return *d
}

// A disputa perdida de cobrança paga com excedente consome o crédito do
// cliente gerado por ele, com o ledger balanceado; o crédito que o cliente já
// usou não deixa o saldo negativo e vira valor a recuperar; webhooks
// repetidos não repetem o consumo (ADR-070).
func TestChargebackConsumesOverpaymentCreditWithSimulator(t *testing.T) {
	ctx := context.Background()
	fee := paysim.Fee(12500)
	t.Run("crédito disponível é consumido", func(t *testing.T) {
		h := newPayHarness(t, 2*time.Second)
		inv, c := h.overpaidCharge(t)
		h.chargeback(t, c, "requested")
		if d := h.dispute(t, c.ID); d.Status != payments.DisputeOpened || !d.CreditAmount.IsZero() || h.creditBalance(t, inv.CustomerID) != 2500 {
			t.Fatalf("a abertura não mexe no crédito: %+v", d)
		}
		h.chargeback(t, c, "lost")
		d := h.dispute(t, c.ID)
		got := h.charge(t, inv.ID)
		if d.Status != payments.DisputeLost || d.Amount.Amount() != 12500 || d.CreditAmount.Amount() != 2500 || !d.RecoveryAmount.IsZero() ||
			got.Status != payments.ChargeChargedBack || got.AmountChargedBack.Amount() != 12500 {
			t.Fatalf("disputa perdida: %+v cobrança: %s %v", d, got.Status, got.AmountChargedBack)
		}
		if bal := h.creditBalance(t, inv.CustomerID); bal != 0 {
			t.Fatalf("saldo de crédito = %d", bal)
		}
		h.assertLedgerBalanced(t)
		net := ledgerNetByCode(t, h)
		if net["chargebacks"] != 10000 || net["customer_credits"] != 0 || net["customer_recoverables"] != 0 || net["provider_clearing"] != -fee ||
			net["refunds"] != 0 {
			t.Fatalf("ledger: %v", net)
		}
		if n := h.sum(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND kind = 'dispute.credit_reversed'
		                    AND reference_type = 'dispute'`); n != 1 {
			t.Fatalf("lançamentos do crédito estornado = %d", n)
		}
		if n := h.sum(t, `SELECT COALESCE(sum(amount), 0)::bigint FROM customer_credit_entries WHERE tenant_id = $1 AND kind = 'consumption'`); n != -2500 {
			t.Fatalf("consumo registrado = %d", n)
		}
		// Painel: o chargeback do mês é a receita estornada, sem o excedente.
		if dash := h.dashboard(t, "2026-03"); dash.Chargebacks != 10000 || dash.Refunds != 0 {
			t.Fatalf("painel: chargebacks=%d", dash.Chargebacks)
		}
		// Webhooks repetidos (reenvio de todos os eventos da cobrança) não
		// consomem o crédito de novo nem alteram o ledger.
		entries := h.sum(t, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`)
		if _, err := h.sim.Apply(c.ProviderChargeID, paysim.Action{Name: "replay"}); err != nil {
			t.Fatal(err)
		}
		h.deliver(t)
		if n := h.sum(t, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`); n != entries {
			t.Fatalf("partidas depois do reenvio: %d, antes %d", n, entries)
		}
		if again := h.dispute(t, c.ID); again.CreditAmount.Amount() != 2500 || h.creditBalance(t, inv.CustomerID) != 0 ||
			h.sum(t, `SELECT count(*) FROM disputes WHERE tenant_id = $1`) != 1 {
			t.Fatalf("reenvio alterou a disputa: %+v", again)
		}
		list, err := h.env.Store.ListDisputes(ctx, h.tn.ID, &c.ID, 10)
		if err != nil || len(list) != 1 || list[0].CreditAmount.Amount() != 2500 {
			t.Fatalf("disputa listada: %+v %v", list, err)
		}
		events := list[0].History
		if last := events[len(events)-1]; last.To != payments.DisputeLost || last.Details["credit_amount"] != float64(2500) {
			t.Fatalf("histórico da disputa: %+v", last)
		}
	})
	t.Run("crédito já usado vira valor a recuperar, sem saldo negativo", func(t *testing.T) {
		h := newPayHarness(t, 2*time.Second)
		inv, c := h.overpaidCharge(t)
		// O cliente usa 20,00 do crédito em outra fatura.
		other := h.issueInvoiceFor(t, inv.CustomerID, 2000)
		if other.CreditApplied.Amount() != 2000 || h.creditBalance(t, inv.CustomerID) != 500 {
			t.Fatalf("crédito aplicado em outra fatura: %v saldo=%d", other.CreditApplied, h.creditBalance(t, inv.CustomerID))
		}
		h.chargeback(t, c, "requested", "lost")
		d := h.dispute(t, c.ID)
		if d.Status != payments.DisputeLost || d.CreditAmount.Amount() != 500 || d.RecoveryAmount.Amount() != 2000 {
			t.Fatalf("disputa perdida: %+v", d)
		}
		if bal := h.creditBalance(t, inv.CustomerID); bal != 0 {
			t.Fatalf("saldo de crédito = %d", bal)
		}
		h.assertLedgerBalanced(t)
		net := ledgerNetByCode(t, h)
		// A outra fatura (20,00) foi quitada pelo crédito; a receita
		// estornada é a da fatura contestada e os 20,00 usados ficam a
		// recuperar do cliente.
		if net["chargebacks"] != 10000 || net["customer_recoverables"] != 2000 || net["customer_credits"] != 0 ||
			net["provider_clearing"] != -fee || net["accounts_receivable"] != 0 {
			t.Fatalf("ledger: %v", net)
		}
		if n := h.sum(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'dispute.lost'
		                    AND (details->>'recovery_amount')::bigint = 2000 AND (details->>'credit_amount')::bigint = 500`); n != 1 {
			t.Fatalf("auditoria da disputa perdida = %d", n)
		}
		if n := h.sum(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'dispute.closed'
		                    AND (payload->>'recovery_amount')::bigint = 2000`); n != 1 {
			t.Fatalf("evento da disputa perdida = %d", n)
		}
	})
	t.Run("reembolso antes da disputa e chargeback dividem o excedente", func(t *testing.T) {
		h := newPayHarness(t, 2*time.Second)
		inv, c := h.overpaidCharge(t)
		r, err := h.pay.RequestRefund(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, refundInput(c, 1000, "reembolso-"+uuid.NewString()))
		if err != nil {
			t.Fatal(err)
		}
		h.deliver(t)
		if r, err = h.pay.Refund(ctx, h.tn.ID, r.ID); err != nil || r.Status != payments.RefundSucceeded || r.CreditAmount.Amount() != 1000 {
			t.Fatalf("reembolso: %+v %v", r, err)
		}
		h.chargeback(t, c, "requested", "lost")
		d := h.dispute(t, c.ID)
		got := h.charge(t, inv.ID)
		if d.CreditAmount.Amount() != 1500 || !d.RecoveryAmount.IsZero() || got.AmountChargedBack.Amount() != 11500 || got.AmountRefunded.Amount() != 1000 ||
			h.creditBalance(t, inv.CustomerID) != 0 {
			t.Fatalf("disputa: %+v cobrança: estornado=%v reembolsado=%v", d, got.AmountChargedBack, got.AmountRefunded)
		}
		h.assertLedgerBalanced(t)
		if net := ledgerNetByCode(t, h); net["chargebacks"] != 10000 || net["refunds"] != 0 || net["customer_credits"] != 0 ||
			net["provider_clearing"] != -fee {
			t.Fatalf("ledger: %v", net)
		}
	})
	t.Run("disputa ganha mantém o crédito", func(t *testing.T) {
		h := newPayHarness(t, 2*time.Second)
		inv, c := h.overpaidCharge(t)
		h.chargeback(t, c, "requested", "won")
		d := h.dispute(t, c.ID)
		if d.Status != payments.DisputeWon || !d.CreditAmount.IsZero() || !d.RecoveryAmount.IsZero() || h.creditBalance(t, inv.CustomerID) != 2500 {
			t.Fatalf("disputa ganha: %+v saldo=%d", d, h.creditBalance(t, inv.CustomerID))
		}
		h.assertLedgerBalanced(t)
		if net := ledgerNetByCode(t, h); net["chargebacks"] != 0 || net["customer_credits"] != -2500 || net["customer_recoverables"] != 0 {
			t.Fatalf("ledger: %v", net)
		}
		// O banco recusa crédito estornado em disputa que não foi perdida e
		// partes do crédito acima do valor contestado.
		owner := envOwner(t)
		for name, stmt := range map[string]string{
			"disputes_credit_only_lost":     `UPDATE disputes SET credit_amount = 100 WHERE id = $1`,
			"disputes_credit_nonnegative":   `UPDATE disputes SET recovery_amount = -1 WHERE id = $1`,
			"disputes_credit_within_amount": `UPDATE disputes SET status = 'lost', credit_amount = amount, recovery_amount = 1 WHERE id = $1`,
		} {
			if _, err := owner.Exec(ctx, stmt, d.ID); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
}

// settlePendingRefund conclui no provedor o reembolso aceito sem confirmação
// (pendingRefundGateway) e aplica o resultado pela conciliação: a execução
// aponta o reembolso pendente e a adoção do provedor o confirma ou recusa.
func (h *payHarness) settlePendingRefund(t *testing.T, st *pendingRefunds, c payments.Charge, r payments.Refund, state payments.RefundState) {
	t.Helper()
	ctx := context.Background()
	st.settle(c.ProviderChargeID, payments.ProviderRefund{ExternalReference: r.ID.String(), State: state, Amount: r.Amount})
	h.env.Clock.Advance(2 * time.Hour)
	run, err := h.pay.RunReconciliation(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, payments.RunInput{})
	if err != nil || run.Status != payments.RunCompleted {
		t.Fatalf("conciliação: %+v %v", run, err)
	}
	list, err := h.env.Store.ListDiscrepancies(ctx, h.tn.ID, "open", string(payments.DiscrepancyRefundPending), 10)
	if err != nil || len(list) != 1 || list[0].RefundID == nil || *list[0].RefundID != r.ID {
		t.Fatalf("reembolso pendente na conciliação: %+v %v", list, err)
	}
	if _, err := h.pay.ResolveDiscrepancy(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, payments.ResolveInput{ID: list[0].ID,
		Resolution: payments.ResolutionAcceptProvider, Justification: "Resultado do reembolso conferido no extrato do provedor."}); err != nil {
		t.Fatalf("adoção do provedor: %v", err)
	}
}

// Reembolso reservado concluído depois da disputa perdida de cobrança paga
// com excedente: a base do excedente já devolvido soma, no banco, o crédito
// estornado pelas disputas perdidas (ChargeChargebackCreditAmount), tanto a
// parte consumida do saldo quanto a parte a recuperar do cliente. O
// reembolso confirmado depois da perda não consome de novo o crédito do
// cliente (nem o de outra origem), inclusive quando o excedente estornado
// pela perda já havia sido usado, e o reembolso recusado depois da perda
// leva ao chargeback somente o excedente que a perda ainda não havia
// estornado (ADR-070).
func TestChargebackCreditWithRefundSettledAfterLossWithSimulator(t *testing.T) {
	ctx := context.Background()
	fee := paysim.Fee(12500)
	newHarness := func(t *testing.T) (*payHarness, *pendingRefunds) {
		st := &pendingRefunds{settled: map[string][]payments.ProviderRefund{}}
		return newPayHarnessWith(t, 2*time.Second, func(gw payments.Gateway) payments.Gateway { return pendingRefundGateway{Gateway: gw, st: st} }), st
	}
	pendingRefund := func(t *testing.T, h *payHarness, c payments.Charge, amount int64) payments.Refund {
		t.Helper()
		r, err := h.pay.RequestRefund(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, refundInput(c, amount, "pendente-"+uuid.NewString()))
		if err != nil || r.Status != payments.RefundRequested {
			t.Fatalf("reembolso pendente: %+v %v", r, err)
		}
		return r
	}
	creditReversals := func(t *testing.T, h *payHarness, refType string) int64 {
		t.Helper()
		return h.sum(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND kind = 'dispute.credit_reversed'
		                    AND reference_type = '`+refType+`'`)
	}
	t.Run("reembolso confirmado depois da perda não consome o crédito de novo", func(t *testing.T) {
		h, st := newHarness(t)
		inv, c := h.overpaidCharge(t)
		// Crédito de outra origem: não foi gerado pelo excedente desta
		// cobrança e não pode ser consumido pelo reembolso dela.
		if err := h.env.Billing.GrantCredit(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, inv.CustomerID, brlCents(700),
			"Cortesia comercial", "cortesia-"+uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		r := pendingRefund(t, h, c, 3000)
		h.chargeback(t, c, "requested", "lost")
		d := h.dispute(t, c.ID)
		got := h.charge(t, inv.ID)
		if d.Amount.Amount() != 12500 || d.CreditAmount.Amount() != 2500 || !d.RecoveryAmount.IsZero() ||
			got.AmountChargedBack.Amount() != 9500 || got.AmountRefundPending.Amount() != 3000 {
			t.Fatalf("disputa perdida: %+v cobrança: estornado=%v reservado=%v", d, got.AmountChargedBack, got.AmountRefundPending)
		}
		if bal := h.creditBalance(t, inv.CustomerID); bal != 700 {
			t.Fatalf("saldo de crédito depois da perda = %d", bal)
		}
		h.settlePendingRefund(t, st, c, r, payments.RefundStateDone)
		done, err := h.pay.Refund(ctx, h.tn.ID, r.ID)
		if err != nil || done.Status != payments.RefundSucceeded || !done.CreditAmount.IsZero() {
			t.Fatalf("reembolso confirmado depois da perda: %+v %v", done, err)
		}
		if n := h.sum(t, `SELECT COALESCE(sum(credit_amount), 0)::bigint FROM refunds WHERE tenant_id = $1`); n != 0 {
			t.Fatalf("refunds.credit_amount = %d", n)
		}
		if bal := h.creditBalance(t, inv.CustomerID); bal != 700 {
			t.Fatalf("o reembolso consumiu crédito de outra origem: saldo = %d", bal)
		}
		got = h.charge(t, inv.ID)
		if got.AmountRefunded.Amount() != 3000 || !got.AmountRefundPending.IsZero() || got.AmountChargedBack.Amount() != 9500 {
			t.Fatalf("cobrança: reembolsado=%v reservado=%v estornado=%v", got.AmountRefunded, got.AmountRefundPending, got.AmountChargedBack)
		}
		if again := h.dispute(t, c.ID); again.CreditAmount.Amount() != 2500 || !again.RecoveryAmount.IsZero() {
			t.Fatalf("a confirmação do reembolso alterou a disputa: %+v", again)
		}
		h.assertLedgerBalanced(t)
		// Receita devolvida: 100,00 da fatura, 70,00 por chargeback e 30,00
		// por reembolso; o excedente saiu do crédito uma única vez e a
		// cortesia continua no saldo do cliente.
		net := ledgerNetByCode(t, h)
		if net["chargebacks"] != 7000 || net["refunds"] != 3000 || net["customer_credits"] != -700 || net["customer_recoverables"] != 0 ||
			net["provider_clearing"] != -fee {
			t.Fatalf("ledger: %v", net)
		}
		if n := h.sum(t, `SELECT COALESCE(sum(amount), 0)::bigint FROM customer_credit_entries WHERE tenant_id = $1 AND kind = 'consumption'`); n != -2500 {
			t.Fatalf("consumo de crédito registrado = %d", n)
		}
		if dispute, refund := creditReversals(t, h, "dispute"), creditReversals(t, h, "refund"); dispute != 1 || refund != 0 {
			t.Fatalf("lançamentos do crédito estornado: disputa=%d reembolso=%d", dispute, refund)
		}
	})
	t.Run("reembolso confirmado depois da perda com valor a recuperar não consome crédito de outra origem", func(t *testing.T) {
		h, st := newHarness(t)
		inv, c := h.overpaidCharge(t)
		// O cliente usa 20,00 do crédito em outra fatura: sobram 5,00.
		other := h.issueInvoiceFor(t, inv.CustomerID, 2000)
		if other.CreditApplied.Amount() != 2000 || h.creditBalance(t, inv.CustomerID) != 500 {
			t.Fatalf("crédito aplicado em outra fatura: %v saldo=%d", other.CreditApplied, h.creditBalance(t, inv.CustomerID))
		}
		r := pendingRefund(t, h, c, 3000)
		// A perda estorna 95,00 e leva o excedente inteiro: 5,00 do saldo e
		// 20,00, já usados, a recuperar do cliente.
		h.chargeback(t, c, "requested", "lost")
		d := h.dispute(t, c.ID)
		got := h.charge(t, inv.ID)
		if d.CreditAmount.Amount() != 500 || d.RecoveryAmount.Amount() != 2000 || got.AmountChargedBack.Amount() != 9500 ||
			got.AmountRefundPending.Amount() != 3000 || h.creditBalance(t, inv.CustomerID) != 0 {
			t.Fatalf("disputa perdida: %+v cobrança: estornado=%v reservado=%v saldo=%d", d, got.AmountChargedBack, got.AmountRefundPending,
				h.creditBalance(t, inv.CustomerID))
		}
		// Crédito de outra origem, concedido depois da perda: o excedente da
		// cobrança já voltou inteiro ao pagador (5,00 + 20,00) e o reembolso
		// não tem mais o que consumir.
		if err := h.env.Billing.GrantCredit(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, inv.CustomerID, brlCents(700),
			"Cortesia comercial", "cortesia-"+uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		movements := h.sum(t, `SELECT count(*) FROM customer_credit_entries WHERE tenant_id = $1`)
		h.settlePendingRefund(t, st, c, r, payments.RefundStateDone)
		done, err := h.pay.Refund(ctx, h.tn.ID, r.ID)
		if err != nil || done.Status != payments.RefundSucceeded || !done.CreditAmount.IsZero() {
			t.Fatalf("reembolso confirmado depois da perda: %+v %v", done, err)
		}
		if n := h.sum(t, `SELECT COALESCE(sum(credit_amount), 0)::bigint FROM refunds WHERE tenant_id = $1`); n != 0 {
			t.Fatalf("refunds.credit_amount = %d", n)
		}
		if bal := h.creditBalance(t, inv.CustomerID); bal != 700 {
			t.Fatalf("o reembolso consumiu crédito de outra origem: saldo = %d", bal)
		}
		if n := h.sum(t, `SELECT count(*) FROM customer_credit_entries WHERE tenant_id = $1`); n != movements {
			t.Fatalf("movimentações de crédito depois do reembolso: %d, antes %d", n, movements)
		}
		got = h.charge(t, inv.ID)
		if got.AmountRefunded.Amount() != 3000 || !got.AmountRefundPending.IsZero() || got.AmountChargedBack.Amount() != 9500 {
			t.Fatalf("cobrança: reembolsado=%v reservado=%v estornado=%v", got.AmountRefunded, got.AmountRefundPending, got.AmountChargedBack)
		}
		if again := h.dispute(t, c.ID); again.CreditAmount.Amount() != 500 || again.RecoveryAmount.Amount() != 2000 {
			t.Fatalf("a confirmação do reembolso alterou a disputa: %+v", again)
		}
		h.assertLedgerBalanced(t)
		// Receita devolvida: 100,00 da fatura contestada, 70,00 por
		// chargeback e 30,00 por reembolso. A outra fatura (20,00) continua
		// quitada pelo crédito, esses 20,00 ficam a recuperar do cliente e a
		// cortesia continua no saldo.
		net := ledgerNetByCode(t, h)
		if net["chargebacks"] != 7000 || net["refunds"] != 3000 || net["customer_credits"] != -700 || net["customer_recoverables"] != 2000 ||
			net["provider_clearing"] != -fee || net["accounts_receivable"] != 0 {
			t.Fatalf("ledger: %v", net)
		}
		if dispute, refund := creditReversals(t, h, "dispute"), creditReversals(t, h, "refund"); dispute != 1 || refund != 0 {
			t.Fatalf("lançamentos do crédito estornado: disputa=%d reembolso=%d", dispute, refund)
		}
	})
	t.Run("reembolso recusado depois da perda leva só o excedente restante", func(t *testing.T) {
		h, st := newHarness(t)
		inv, c := h.overpaidCharge(t)
		// A reserva de 110,00 deixa 15,00 para o chargeback da perda, menos
		// que o excedente de 25,00: a perda estorna 15,00 do crédito e os
		// 10,00 restantes ficam para o chargeback do reembolso recusado.
		r := pendingRefund(t, h, c, 11000)
		h.chargeback(t, c, "requested", "lost")
		d := h.dispute(t, c.ID)
		got := h.charge(t, inv.ID)
		if d.CreditAmount.Amount() != 1500 || !d.RecoveryAmount.IsZero() || got.AmountChargedBack.Amount() != 1500 ||
			got.AmountRefundPending.Amount() != 11000 || h.creditBalance(t, inv.CustomerID) != 1000 {
			t.Fatalf("disputa perdida: %+v cobrança: estornado=%v reservado=%v saldo=%d", d, got.AmountChargedBack, got.AmountRefundPending,
				h.creditBalance(t, inv.CustomerID))
		}
		h.settlePendingRefund(t, st, c, r, payments.RefundStateFailed)
		failed, err := h.pay.Refund(ctx, h.tn.ID, r.ID)
		if err != nil || failed.Status != payments.RefundFailed || !failed.CreditAmount.IsZero() {
			t.Fatalf("reembolso recusado depois da perda: %+v %v", failed, err)
		}
		d = h.dispute(t, c.ID)
		got = h.charge(t, inv.ID)
		// Crédito estornado e valor a recuperar somam exatamente o excedente.
		if total := d.CreditAmount.Amount() + d.RecoveryAmount.Amount(); total != 2500 || d.CreditAmount.Amount() != 2500 {
			t.Fatalf("crédito estornado pela disputa: crédito=%v a recuperar=%v", d.CreditAmount, d.RecoveryAmount)
		}
		if got.AmountChargedBack.Amount() != 12500 || !got.AmountRefunded.IsZero() || !got.AmountRefundPending.IsZero() {
			t.Fatalf("cobrança: estornado=%v reembolsado=%v reservado=%v", got.AmountChargedBack, got.AmountRefunded, got.AmountRefundPending)
		}
		if bal := h.creditBalance(t, inv.CustomerID); bal != 0 {
			t.Fatalf("saldo de crédito = %d", bal)
		}
		if n := h.sum(t, `SELECT count(*) FROM customer_credit_entries e WHERE e.tenant_id = $1 AND
		                    (SELECT sum(x.amount) FROM customer_credit_entries x WHERE x.tenant_id = e.tenant_id AND x.customer_id = e.customer_id
		                        AND (x.created_at, x.id) <= (e.created_at, e.id)) < 0`); n != 0 {
			t.Fatalf("o saldo de crédito ficou negativo em %d movimentação(ões)", n)
		}
		if dispute, refund := creditReversals(t, h, "dispute"), creditReversals(t, h, "refund"); dispute != 1 || refund != 1 {
			t.Fatalf("lançamentos do crédito estornado: disputa=%d reembolso=%d", dispute, refund)
		}
		h.assertLedgerBalanced(t)
		net := ledgerNetByCode(t, h)
		if net["chargebacks"] != 10000 || net["refunds"] != 0 || net["customer_credits"] != 0 || net["customer_recoverables"] != 0 ||
			net["provider_clearing"] != -fee {
			t.Fatalf("ledger: %v", net)
		}
		if n := h.sum(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'dispute.chargeback_increased'
		                    AND (details->>'credit_amount')::bigint = 1000 AND (details->>'recovery_amount')::bigint = 0`); n != 1 {
			t.Fatalf("auditoria do aumento do chargeback = %d", n)
		}
	})
}
