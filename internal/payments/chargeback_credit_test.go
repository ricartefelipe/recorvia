package payments

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// overpaidFixture devolve uma cobrança de 100,00 paga com 125,00 pelo
// provedor (taxa 1,99): os 25,00 excedentes viraram crédito do cliente, com o
// lançamento payment.overpayment. O ledger do pagamento é mantido, para que
// os testes confiram o saldo das contas no ciclo inteiro.
func overpaidFixture(t *testing.T) (*payFixture, Charge) {
	t.Helper()
	f := newPayFixture(t)
	_, c := awaitingCharge(t, f)
	f.process(t, f.storeWebhook(t, confirmedWebhook("evt_paid", c.ProviderChargeID, 12500, 199)))
	c = f.store.charges[c.ID]
	if c.Status != ChargePaid || c.AmountPaid.Amount() != 12500 || f.store.creditBalance[f.customer].Amount() != 2500 {
		t.Fatalf("pagamento excedente: %s pago=%v crédito=%v", c.Status, c.AmountPaid, f.store.creditBalance[f.customer])
	}
	f.store.events, f.store.audits = nil, nil
	return f, c
}

// useCredit simula o uso do crédito do cliente em outra fatura.
func (f *payFixture) useCredit(t *testing.T, amount int64) {
	t.Helper()
	if err := f.store.GrantCustomerCredit(context.Background(), f.customer, brl(-amount), "consumption", nil, "Aplicado em outra fatura", f.clock.Now()); err != nil {
		t.Fatal(err)
	}
}

func (f *payFixture) lose(t *testing.T, c Charge, eventID string, hours int, refunds ...ProviderRefund) StoredWebhook {
	t.Helper()
	return f.sendSnapshot(t, eventID, withRefunds(chargebackSnapshot(c.ProviderChargeID, ProviderChargebackLost,
		payNow.Add(time.Duration(hours)*time.Hour)), refunds...))
}

func creditReversals(f *payFixture) []ledger.Transaction {
	return postedByKind(f.store.posted, "dispute.credit_reversed")
}

func consumedCredits(f *payFixture) []creditGrant {
	var out []creditGrant
	for _, g := range f.store.credits {
		if g.kind == "consumption" {
			out = append(out, g)
		}
	}
	return out
}

// A disputa perdida de cobrança paga com excedente consome o crédito do
// cliente gerado por esse excedente, como o reembolso; a parte que o cliente
// já usou não deixa o saldo negativo e vira valor a recuperar (ADR-070).
func TestDisputeLostConsumesOverpaymentCredit(t *testing.T) {
	cases := []struct {
		name string
		// used é o crédito do excedente que o cliente já aplicou em outra
		// fatura antes da disputa perdida.
		used     int64
		consumed int64
		recovery int64
	}{
		{name: "crédito intacto", consumed: 2500},
		{name: "crédito usado em parte", used: 1500, consumed: 1000, recovery: 1500},
		{name: "crédito todo usado", used: 2500, recovery: 2500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := overpaidFixture(t)
			if tc.used > 0 {
				f.useCredit(t, tc.used)
			}
			before := len(consumedCredits(f))
			if w := f.lose(t, c, "evt_lost", 1); w.Status != "processed" {
				t.Fatalf("webhook: %s (%s)", w.Status, w.Error)
			}
			d := f.latestDispute(t, c.ID)
			got := f.store.charges[c.ID]
			if d.Status != DisputeLost || d.Amount.Amount() != 12500 || d.CreditAmount.Amount() != tc.consumed || d.RecoveryAmount.Amount() != tc.recovery ||
				got.Status != ChargeChargedBack || got.AmountChargedBack.Amount() != 12500 {
				t.Fatalf("disputa: %+v cobrança: %s %v", d, got.Status, got.AmountChargedBack)
			}
			if bal := f.store.creditBalance[f.customer]; bal.Amount() != 2500-tc.used-tc.consumed || bal.IsNegative() {
				t.Fatalf("saldo de crédito: %v", bal)
			}
			grants := consumedCredits(f)[before:]
			if tc.consumed == 0 && len(grants) != 0 || tc.consumed > 0 && (len(grants) != 1 || grants[0].amount.Amount() != -tc.consumed ||
				*grants[0].invoiceID != c.InvoiceID) {
				t.Fatalf("consumo do crédito: %+v", grants)
			}
			reversed := creditReversals(f)
			if len(reversed) != 1 || reversed[0].ReferenceType != "dispute" || reversed[0].ReferenceID != d.ID {
				t.Fatalf("lançamento do crédito estornado: %+v", reversed)
			}
			if net := ledgerNet(reversed); net[ledger.CustomerCredits] != tc.consumed || net[ledger.CustomerRecoverables] != tc.recovery ||
				net[ledger.Chargebacks] != -2500 {
				t.Fatalf("partidas: %v", net)
			}
			// Ciclo inteiro: o provedor ficou só com a taxa (o disponível
			// fica negativo nela), chargebacks
			// registra a receita estornada e o excedente não fica nas duas
			// contas (chargebacks e crédito do cliente).
			net := ledgerNet(f.store.posted)
			if net[ledger.Cash] != -199 || net[ledger.ProviderClearing] != 0 || net[ledger.ProviderConfirmed] != 0 || net[ledger.Chargebacks] != 10000 || net[ledger.CustomerCredits] != -2500+tc.consumed ||
				net[ledger.CustomerRecoverables] != tc.recovery {
				t.Fatalf("ledger: %v", net)
			}
			for _, tx := range f.store.posted {
				if err := tx.Validate(); err != nil {
					t.Fatalf("lançamento %s desbalanceado: %v", tx.Kind, err)
				}
			}
			var closed map[string]any
			for _, e := range f.store.events {
				if e.Type == outbox.EventDisputeClosed {
					closed = e.Payload.(map[string]any)
				}
			}
			if closed["credit_amount"] != tc.consumed || closed["recovery_amount"] != tc.recovery || closed["charged_back_amount"] != int64(12500) {
				t.Fatalf("evento dispute.closed: %v", closed)
			}
			h := f.store.invoiceHistory[len(f.store.invoiceHistory)-1].changes[0]
			if h.Reason != "chargeback" || h.Details["credit_amount"] != tc.consumed || h.Details["recovery_amount"] != tc.recovery {
				t.Fatalf("histórico da fatura: %+v", h)
			}
			ev := f.store.disputeEvents[len(f.store.disputeEvents)-1]
			if ev.To != DisputeLost || ev.Details["credit_amount"] != tc.consumed || ev.Details["recovery_amount"] != tc.recovery {
				t.Fatalf("histórico da disputa: %+v", ev)
			}

			// Webhook repetido e retrato mais novo da mesma decisão não
			// consomem o crédito de novo.
			posted, credits := len(f.store.posted), len(f.store.credits)
			for i, id := range []string{"evt_lost", "evt_lost_2"} {
				if w := f.lose(t, c, id, 2+i); w.Status != "processed" {
					t.Fatalf("repetição %s: %s (%s)", id, w.Status, w.Error)
				}
			}
			again := f.latestDispute(t, c.ID)
			if len(f.store.posted) != posted || len(f.store.credits) != credits || again.CreditAmount != d.CreditAmount ||
				again.RecoveryAmount != d.RecoveryAmount || f.store.charges[c.ID].AmountChargedBack.Amount() != 12500 {
				t.Fatalf("repetição alterou o estado: %+v", again)
			}
		})
	}
}

// A disputa ganha não mexe no crédito do cliente, e uma cobrança sem
// excedente não gera lançamento de crédito.
func TestDisputeWithoutCreditEffect(t *testing.T) {
	t.Run("disputa ganha", func(t *testing.T) {
		f, c := overpaidFixture(t)
		f.sendSnapshot(t, "evt_open", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour)))
		f.sendSnapshot(t, "evt_won", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackReversed, payNow.Add(2*time.Hour)))
		d := f.latestDispute(t, c.ID)
		if d.Status != DisputeWon || !d.CreditAmount.IsZero() || !d.RecoveryAmount.IsZero() || f.store.charges[c.ID].Status != ChargePaid {
			t.Fatalf("disputa ganha: %+v", d)
		}
		if f.store.creditBalance[f.customer].Amount() != 2500 || len(creditReversals(f)) != 0 || len(consumedCredits(f)) != 0 {
			t.Fatalf("crédito alterado pela disputa ganha: %v", f.store.creditBalance[f.customer])
		}
		if net := ledgerNet(f.store.posted); net[ledger.Chargebacks] != 0 || net[ledger.CustomerCredits] != -2500 {
			t.Fatalf("ledger: %v", net)
		}
		// Nova disputa da mesma cobrança, perdida: o crédito é consumido uma vez.
		f.sendSnapshot(t, "evt_open_2", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(4*time.Hour)))
		f.lose(t, c, "evt_lost", 5)
		if d2 := f.latestDispute(t, c.ID); d2.ID == d.ID || d2.CreditAmount.Amount() != 2500 || f.store.creditBalance[f.customer].Amount() != 0 {
			t.Fatalf("segunda disputa: %+v", d2)
		}
	})
	t.Run("cobrança sem excedente", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.store.creditBalance[f.customer] = brl(5000) // crédito de outra origem
		f.lose(t, c, "evt_lost", 1)
		d := f.latestDispute(t, c.ID)
		if !d.CreditAmount.IsZero() || !d.RecoveryAmount.IsZero() || f.store.creditBalance[f.customer].Amount() != 5000 || len(creditReversals(f)) != 0 {
			t.Fatalf("crédito de outra origem não é consumido: %+v", d)
		}
	})
}

// Reembolsos antes, durante e depois da disputa e o chargeback dividem o
// mesmo excedente: cada centavo do crédito é devolvido uma única vez.
func TestDisputeLostCreditWithRefunds(t *testing.T) {
	at := func(h int) time.Time { return payNow.Add(time.Duration(h) * time.Hour) }
	pending := func(f *payFixture, c Charge, amount int64) Refund {
		f.gw.refundResults = []refundRes{{pr: ProviderRefund{Status: "PENDING", State: RefundStatePending, Amount: brl(amount)}}}
		r := f.refund(t, c.ID, amount, "")
		if r.Status != RefundRequested {
			t.Fatalf("reembolso aguarda o provedor: %s", r.Status)
		}
		return r
	}
	exact := func(r Refund, state RefundState) ProviderRefund {
		return ProviderRefund{ExternalReference: r.ID.String(), State: state, Amount: r.Amount}
	}
	check := func(t *testing.T, f *payFixture, c Charge, chargedBack, refunded, refundCredit, disputeCredit, recovery int64) {
		t.Helper()
		got := f.store.charges[c.ID]
		d := f.latestDispute(t, c.ID)
		if got.Status != ChargeChargedBack || got.AmountChargedBack.Amount() != chargedBack || got.AmountRefunded.Amount() != refunded ||
			!got.AmountRefundPending.IsZero() {
			t.Fatalf("cobrança: %s estornado=%v reembolsado=%v reservado=%v", got.Status, got.AmountChargedBack, got.AmountRefunded,
				got.AmountRefundPending)
		}
		var fromRefunds int64
		for _, r := range f.store.refundList() {
			if r.Status == RefundSucceeded {
				fromRefunds += r.CreditAmount.Amount()
			}
		}
		if fromRefunds != refundCredit || d.CreditAmount.Amount() != disputeCredit || d.RecoveryAmount.Amount() != recovery {
			t.Fatalf("crédito: reembolsos=%d disputa=%v a recuperar=%v", fromRefunds, d.CreditAmount, d.RecoveryAmount)
		}
		if bal := f.store.creditBalance[f.customer]; bal.Amount() != 2500-refundCredit-disputeCredit {
			t.Fatalf("saldo de crédito: %v", bal)
		}
		// O excedente nunca é devolvido duas vezes, e as três contas somam
		// tudo o que voltou ao pagador.
		net := ledgerNet(f.store.posted)
		credit := refundCredit + disputeCredit + recovery
		if credit > 2500 || net[ledger.Refunds] != refunded-refundCredit || net[ledger.Chargebacks] != chargedBack-disputeCredit-recovery ||
			net[ledger.CustomerCredits] != -2500+refundCredit+disputeCredit || net[ledger.CustomerRecoverables] != recovery ||
			net[ledger.Cash] != 12500-199-chargedBack-refunded || net[ledger.ProviderClearing] != 0 {
			t.Fatalf("ledger: %v", net)
		}
	}
	t.Run("reembolso antes da disputa consome o crédito primeiro", func(t *testing.T) {
		f, c := overpaidFixture(t)
		if r := f.refund(t, c.ID, 1000, ""); r.CreditAmount.Amount() != 1000 {
			t.Fatalf("parte do crédito: %v", r.CreditAmount)
		}
		f.lose(t, c, "evt_lost", 1)
		check(t, f, c, 11500, 1000, 1000, 1500, 0)
	})
	t.Run("reembolso antes da disputa esgota o excedente", func(t *testing.T) {
		f, c := overpaidFixture(t)
		f.refund(t, c.ID, 4000, "")
		f.lose(t, c, "evt_lost", 1)
		check(t, f, c, 8500, 4000, 2500, 0, 0)
		if len(creditReversals(f)) != 0 {
			t.Fatal("sem excedente restante não há lançamento de crédito")
		}
	})
	t.Run("reembolso feito no provedor durante a disputa", func(t *testing.T) {
		f, c := overpaidFixture(t)
		panel := ProviderRefund{ProviderRefundID: "rf_painel", State: RefundStateDone, Amount: brl(1000)}
		f.sendSnapshot(t, "evt_open", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, at(1)))
		f.sendSnapshot(t, "evt_refund", withRefunds(chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, at(2)), panel))
		f.lose(t, c, "evt_lost", 3, panel)
		check(t, f, c, 11500, 1000, 1000, 1500, 0)
	})
	t.Run("reembolso pendente confirmado depois da disputa perdida", func(t *testing.T) {
		f, c := overpaidFixture(t)
		r := pending(f, c, 3000)
		f.lose(t, c, "evt_lost", 1)
		// O chargeback (95,00) leva o excedente; o reembolso confirmado
		// depois não o devolve de novo.
		f.lose(t, c, "evt_done", 2, exact(r, RefundStateDone))
		check(t, f, c, 9500, 3000, 0, 2500, 0)
	})
	t.Run("reembolso pendente confirmado depois da perda com valor a recuperar", func(t *testing.T) {
		f, c := overpaidFixture(t)
		// O cliente já usou 20,00 do excedente: a perda consome os 5,00
		// restantes e deixa 20,00 a recuperar.
		f.useCredit(t, 2000)
		r := pending(f, c, 3000)
		f.lose(t, c, "evt_lost", 1)
		if d := f.latestDispute(t, c.ID); d.CreditAmount.Amount() != 500 || d.RecoveryAmount.Amount() != 2000 ||
			!f.store.creditBalance[f.customer].IsZero() {
			t.Fatalf("perda com crédito já usado: %+v saldo=%v", d, f.store.creditBalance[f.customer])
		}
		// Crédito de outra origem depois da perda: o valor a recuperar conta
		// como excedente já devolvido, e o reembolso confirmado não o consome.
		if err := f.store.GrantCustomerCredit(context.Background(), f.customer, brl(700), "manual", nil, "Cortesia comercial", f.clock.Now()); err != nil {
			t.Fatal(err)
		}
		f.lose(t, c, "evt_done", 2, exact(r, RefundStateDone))
		got := f.store.charges[c.ID]
		d := f.latestDispute(t, c.ID)
		if done := f.store.refunds[r.ID]; done.Status != RefundSucceeded || !done.CreditAmount.IsZero() {
			t.Fatalf("reembolso confirmado depois da perda: %s crédito=%v", done.Status, done.CreditAmount)
		}
		if got.AmountChargedBack.Amount() != 9500 || got.AmountRefunded.Amount() != 3000 || !got.AmountRefundPending.IsZero() ||
			d.CreditAmount.Amount() != 500 || d.RecoveryAmount.Amount() != 2000 || f.store.creditBalance[f.customer].Amount() != 700 {
			t.Fatalf("cobrança: estornado=%v reembolsado=%v disputa=%+v saldo=%v", got.AmountChargedBack, got.AmountRefunded, d,
				f.store.creditBalance[f.customer])
		}
		if reversed := creditReversals(f); len(reversed) != 1 || reversed[0].ReferenceType != "dispute" {
			t.Fatalf("lançamentos do crédito estornado: %+v", reversed)
		}
		net := ledgerNet(f.store.posted)
		if net[ledger.Chargebacks] != 7000 || net[ledger.Refunds] != 3000 || net[ledger.CustomerRecoverables] != 2000 ||
			net[ledger.Cash] != -199 || net[ledger.ProviderClearing] != 0 || net[ledger.ProviderConfirmed] != 0 {
			t.Fatalf("ledger: %v", net)
		}
	})
	t.Run("reembolso pendente recusado depois da disputa perdida", func(t *testing.T) {
		f, c := overpaidFixture(t)
		r := pending(f, c, 11000)
		f.lose(t, c, "evt_lost", 1)
		if d := f.latestDispute(t, c.ID); d.CreditAmount.Amount() != 1500 || f.store.charges[c.ID].AmountChargedBack.Amount() != 1500 {
			t.Fatalf("perda com reembolso reservado: %+v", d)
		}
		// O valor do reembolso recusado passa ao chargeback e leva o
		// restante do excedente, parte dele já usada pelo cliente.
		f.useCredit(t, 600)
		f.lose(t, c, "evt_failed", 2, exact(r, RefundStateFailed))
		got := f.store.charges[c.ID]
		d := f.latestDispute(t, c.ID)
		if got.AmountChargedBack.Amount() != 12500 || d.CreditAmount.Amount() != 1900 || d.RecoveryAmount.Amount() != 600 ||
			!f.store.creditBalance[f.customer].IsZero() {
			t.Fatalf("chargeback do reembolso recusado: %v %+v saldo=%v", got.AmountChargedBack, d, f.store.creditBalance[f.customer])
		}
		reversed := creditReversals(f)
		if len(reversed) != 2 || reversed[1].ReferenceType != "refund" || reversed[1].ReferenceID != r.ID ||
			ledgerNet(reversed[1:])[ledger.CustomerRecoverables] != 600 || ledgerNet(reversed[1:])[ledger.CustomerCredits] != 400 {
			t.Fatalf("lançamento por origem: %+v", reversed)
		}
		var increased map[string]any
		for _, a := range f.store.audits {
			if a.Action == "dispute.chargeback_increased" {
				increased = a.Details
			}
		}
		if increased["credit_amount"] != int64(400) || increased["recovery_amount"] != int64(600) {
			t.Fatalf("auditoria do aumento do chargeback: %v", increased)
		}
		net := ledgerNet(f.store.posted)
		if net[ledger.Chargebacks] != 10000 || net[ledger.CustomerRecoverables] != 600 || net[ledger.Cash] != -199 || net[ledger.ProviderClearing] != 0 || net[ledger.ProviderConfirmed] != 0 {
			t.Fatalf("ledger: %v", net)
		}
	})
}

// Falha ao aplicar o crédito desfaz a decisão inteira: a disputa não fica
// perdida sem o consumo do crédito, e o retrato seguinte aplica tudo.
func TestDisputeLostCreditFailuresLeaveNoPartialEffect(t *testing.T) {
	for _, m := range []string{"OverpaymentCreditBase", "LockCustomerCredits", "CustomerCreditBalance", "GrantCustomerCredit", "PostLedger", "UpdateDispute"} {
		t.Run(m, func(t *testing.T) {
			f, c := overpaidFixture(t)
			f.sendSnapshot(t, "evt_open", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour)))
			posted := len(f.store.posted)
			boom := errors.New(m)
			f.store.errs[m] = boom
			snap := chargebackSnapshot(c.ProviderChargeID, ProviderChargebackLost, payNow.Add(2*time.Hour))
			f.gw.getResult = snap
			wid := f.storeWebhook(t, chargebackWebhook("evt_lost", snap))
			if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, uuid.New()); !errors.Is(err, boom) {
				t.Fatalf("erro: %v", err)
			}
			d := f.latestDispute(t, c.ID)
			if d.Status != DisputeOpened || f.store.charges[c.ID].Status != ChargeDisputed || len(f.store.posted) != posted ||
				f.store.creditBalance[f.customer].Amount() != 2500 {
				t.Fatalf("efeito parcial: %+v %s", d, f.store.charges[c.ID].Status)
			}
			delete(f.store.errs, m)
			if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, uuid.New()); err != nil {
				t.Fatal(err)
			}
			if d := f.latestDispute(t, c.ID); d.Status != DisputeLost || d.CreditAmount.Amount() != 2500 {
				t.Fatalf("nova tentativa: %+v", d)
			}
		})
	}
	// O reembolso reservado recusado depois da perda passa ao chargeback e
	// leva o restante do excedente: a falha em qualquer etapa desfaz tudo.
	for _, m := range []string{"PostLedger", "OverpaymentCreditBase", "GrantCustomerCredit", "UpdateDispute"} {
		t.Run("reembolso recusado depois da perda: "+m, func(t *testing.T) {
			f, c := overpaidFixture(t)
			f.gw.refundResults = []refundRes{{pr: ProviderRefund{Status: "PENDING", State: RefundStatePending, Amount: brl(11000)}}}
			r := f.refund(t, c.ID, 11000, "")
			f.lose(t, c, "evt_lost", 1)
			posted, credits := len(f.store.posted), len(f.store.credits)
			boom := errors.New(m)
			f.store.errs[m] = boom
			snap := withRefunds(chargebackSnapshot(c.ProviderChargeID, ProviderChargebackLost, payNow.Add(2*time.Hour)),
				ProviderRefund{ExternalReference: r.ID.String(), State: RefundStateFailed, Amount: r.Amount})
			f.gw.getResult = snap
			wid := f.storeWebhook(t, chargebackWebhook("evt_failed", snap))
			if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, uuid.New()); !errors.Is(err, boom) {
				t.Fatalf("erro: %v", err)
			}
			d := f.latestDispute(t, c.ID)
			if got := f.store.charges[c.ID]; got.AmountChargedBack.Amount() != 1500 || got.AmountRefundPending.Amount() != 11000 ||
				f.store.refunds[r.ID].Status != RefundRequested || d.CreditAmount.Amount() != 1500 || len(f.store.posted) != posted ||
				len(f.store.credits) != credits {
				t.Fatalf("efeito parcial: estornado=%v reservado=%v disputa=%+v", got.AmountChargedBack, got.AmountRefundPending, d)
			}
			delete(f.store.errs, m)
			if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, uuid.New()); err != nil {
				t.Fatal(err)
			}
			if d := f.latestDispute(t, c.ID); d.CreditAmount.Amount() != 2500 || f.store.charges[c.ID].AmountChargedBack.Amount() != 12500 ||
				!f.store.creditBalance[f.customer].IsZero() {
				t.Fatalf("nova tentativa: %+v", d)
			}
		})
	}
	t.Run("retenção liberada sem lançamento", func(t *testing.T) {
		f, c := overpaidFixture(t)
		f.refund(t, c.ID, 1000, "")
		f.sendSnapshot(t, "evt_open", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour)))
		// Reembolso feito no provedor durante a disputa: a perda libera essa
		// parte da retenção; a falha do lançamento desfaz a decisão.
		panel := ProviderRefund{ProviderRefundID: "rf_painel", State: RefundStateDone, Amount: brl(2000)}
		f.sendSnapshot(t, "evt_refund", withRefunds(chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(2*time.Hour)), panel))
		boom := errors.New("ledger indisponível")
		f.store.errs["PostLedger"] = boom
		snap := withRefunds(chargebackSnapshot(c.ProviderChargeID, ProviderChargebackLost, payNow.Add(3*time.Hour)), panel)
		f.gw.getResult = snap
		wid := f.storeWebhook(t, chargebackWebhook("evt_lost", snap))
		if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, uuid.New()); !errors.Is(err, boom) {
			t.Fatalf("erro: %v", err)
		}
		if d := f.latestDispute(t, c.ID); d.Status != DisputeOpened || len(postedByKind(f.store.posted, "dispute.hold_released")) != 0 {
			t.Fatalf("efeito parcial: %+v", d)
		}
	})
	t.Run("fatura da cobrança indisponível", func(t *testing.T) {
		f, c := overpaidFixture(t)
		delete(f.store.invoices, c.InvoiceID)
		snap := chargebackSnapshot(c.ProviderChargeID, ProviderChargebackLost, payNow.Add(time.Hour))
		f.gw.getResult = snap
		wid := f.storeWebhook(t, chargebackWebhook("evt_lost", snap))
		if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, uuid.New()); err == nil {
			t.Fatal("esperava erro")
		}
		if f.store.charges[c.ID].Status != ChargePaid || len(creditReversals(f)) != 0 {
			t.Fatal("efeito parcial sem a fatura")
		}
	})
}

// A base do crédito (excedente ainda não devolvido) e o saldo travado
// limitam a parte do crédito do reembolso e do chargeback.
func TestOverpaymentPortion(t *testing.T) {
	cases := []struct {
		overpaid, amount, want int64
	}{
		{0, 5000, 0}, {2500, 1000, 1000}, {2500, 2500, 2500}, {2500, 9000, 2500}, {2500, 0, 0},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("excedente %d valor %d", tc.overpaid, tc.amount), func(t *testing.T) {
			f, _, c := paidFixture(t)
			f.store.overpaid[c.ID] = brl(tc.overpaid)
			got, err := overpaymentPortion(context.Background(), f.store, &c, brl(tc.amount))
			if err != nil || got.Amount() != tc.want {
				t.Fatalf("parte do excedente: %v %v", got, err)
			}
		})
	}
	t.Run("saldo negativo ou ausente conta como zero", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.store.creditBalance[f.customer] = brl(-100)
		if got, err := lockedCreditBalance(context.Background(), f.store, &c, money.BRL); err != nil || !got.IsZero() {
			t.Fatalf("saldo: %v %v", got, err)
		}
		if !slices.Equal(f.store.creditSteps, []string{"lock", "balance"}) {
			t.Fatalf("o bloqueio antecede a leitura: %v", f.store.creditSteps)
		}
	})
	t.Run("valor não inicializado", func(t *testing.T) {
		if got := amountOrZero(money.Money{}, brl(0)); got.Currency() != money.BRL || !got.IsZero() {
			t.Fatalf("zero na moeda: %v", got)
		}
		if got := amountOrZero(brl(7), brl(0)); got.Amount() != 7 {
			t.Fatalf("valor preservado: %v", got)
		}
	})
}
