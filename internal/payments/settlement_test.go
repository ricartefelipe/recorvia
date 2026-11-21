package payments

import (
	"slices"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/ledger"
)

// Ciclo do pagamento pelo provedor no ledger (ADR-080): confirmado (a
// receber do provedor), recebido (taxa reconhecida) e liquidado (líquido no
// disponível), em qualquer ordem e sem duplicidade.

// cycleBalances confere os saldos das contas do ciclo do pagamento sobre todos
// os lançamentos gravados.
func cycleBalances(t *testing.T, f *payFixture, stage string, confirmed, clearing, cash, fees int64) {
	t.Helper()
	net := ledgerNet(f.store.ledger)
	if net[ledger.ProviderConfirmed] != confirmed || net[ledger.ProviderClearing] != clearing || net[ledger.Cash] != cash ||
		net[ledger.ProviderFees] != fees || net[ledger.AccountsReceivable] != -10000 {
		t.Fatalf("%s: %v", stage, net)
	}
	var sum int64
	for _, v := range net {
		sum += v
	}
	if sum != 0 {
		t.Fatalf("%s: ledger desbalanceado em %d", stage, sum)
	}
}

func ledgerKindsOf(txs []ledger.Transaction) []string {
	out := make([]string, len(txs))
	for i, t := range txs {
		out[i] = t.Kind
	}
	return out
}

func TestProviderPaymentCycleInAnyOrder(t *testing.T) {
	paidAt := payNow.Add(time.Hour)
	confirmed := func(id string) WebhookEvent {
		return WebhookEvent{EventID: id, ProviderChargeID: "pay_1", Status: ProviderConfirmed,
			Charge: providerSnapshot(ProviderConfirmed, "CONFIRMED", 10000, 199, &paidAt)}
	}
	received := func(id string) WebhookEvent {
		return WebhookEvent{EventID: id, ProviderChargeID: "pay_1", Status: ProviderReceived,
			Charge: providerSnapshot(ProviderReceived, "RECEIVED", 10000, 199, &paidAt)}
	}
	cases := []struct {
		name   string
		events []WebhookEvent
	}{
		{"confirmado e depois recebido", []WebhookEvent{confirmed("evt_c"), received("evt_r")}},
		{"recebido direto", []WebhookEvent{received("evt_r")}},
		{"recebido repetido", []WebhookEvent{received("evt_r1"), received("evt_r2"), received("evt_r3")}},
		{"fora de ordem: recebido antes do confirmado", []WebhookEvent{received("evt_r"), confirmed("evt_c")}},
		{"confirmado repetido e recebido repetido", []WebhookEvent{confirmed("evt_c1"), confirmed("evt_c2"), received("evt_r1"), received("evt_r2")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPayFixture(t)
			_, c := awaitingCharge(t, f)
			for _, ev := range tc.events {
				wid := f.storeWebhook(t, ev)
				f.process(t, wid)
				if w := f.store.webhooks[wid]; w.Status != "processed" {
					t.Fatalf("webhook %s: %s (%s)", ev.EventID, w.Status, w.Error)
				}
			}
			if len(f.store.payments) != 1 || f.store.charges[c.ID].Status != ChargePaid {
				t.Fatalf("um pagamento: %d %s", len(f.store.payments), f.store.charges[c.ID].Status)
			}
			if got := ledgerKindsOf(f.store.ledger); !slices.Equal(got, []string{"payment.confirmed", "payment.received", "settlement.received"}) {
				t.Fatalf("lançamentos: %v", got)
			}
			cycleBalances(t, f, "recebido", 0, 0, 9801, 199)
			pay := f.store.payments[0]
			for _, tx := range f.store.ledger {
				if tx.ReferenceType != "payment" || tx.ReferenceID != pay.ID {
					t.Fatalf("lançamento %s sem referência ao pagamento: %s %v", tx.Kind, tx.ReferenceType, tx.ReferenceID)
				}
			}
		})
	}
}

func TestConfirmedPaymentWaitsForReceipt(t *testing.T) {
	f := newPayFixture(t)
	awaitingCharge(t, f)
	f.process(t, f.storeWebhook(t, confirmedWebhook("evt_c", "pay_1", 10000, 199)))
	// Confirmado e ainda não recebido: o bruto fica a receber do provedor, sem
	// taxa, valores em liquidação nem disponível.
	cycleBalances(t, f, "confirmado", 10000, 0, 0, 0)
	f.process(t, f.storeWebhook(t, receivedWebhook("evt_r", "pay_1", 10000, 199)))
	cycleBalances(t, f, "recebido", 0, 0, 9801, 199)
	st := postedByKind(f.store.ledger, "settlement.received")
	if len(st) != 1 || st[0].IdempotencyKey != "settlement:payment.received:"+f.store.payments[0].ID.String() {
		t.Fatalf("liquidação do recebimento: %+v", st)
	}
}

// Reembolso e chargeback de pagamento só confirmado pressupõem valor
// processado pelo provedor: o recebimento é registrado antes da movimentação,
// e nada fica esquecido em pagamentos confirmados.
func TestMovementOfConfirmedPaymentImpliesReceipt(t *testing.T) {
	t.Run("reembolso pedido no Recorvia", func(t *testing.T) {
		f := newPayFixture(t)
		_, c := awaitingCharge(t, f)
		f.process(t, f.storeWebhook(t, confirmedWebhook("evt_c", c.ProviderChargeID, 10000, 199)))
		r := f.refund(t, c.ID, 3000, "")
		if r.Status != RefundSucceeded {
			t.Fatalf("reembolso: %s", r.Status)
		}
		kinds := ledgerKindsOf(f.store.ledger)
		if !slices.Equal(kinds, []string{"payment.confirmed", "payment.received", "settlement.received", "refund.succeeded", "settlement.debited"}) {
			t.Fatalf("lançamentos: %v", kinds)
		}
		net := ledgerNet(f.store.ledger)
		if net[ledger.ProviderConfirmed] != 0 || net[ledger.ProviderClearing] != 0 || net[ledger.Cash] != 9801-3000 || net[ledger.Refunds] != 3000 {
			t.Fatalf("ledger: %v", net)
		}
	})
	t.Run("chargeback informado pelo provedor", func(t *testing.T) {
		f := newPayFixture(t)
		_, c := awaitingCharge(t, f)
		f.process(t, f.storeWebhook(t, confirmedWebhook("evt_c", c.ProviderChargeID, 10000, 199)))
		if w := f.sendSnapshot(t, "evt_cb", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour))); w.Status != "processed" {
			t.Fatalf("webhook: %s (%s)", w.Status, w.Error)
		}
		kinds := ledgerKindsOf(f.store.ledger)
		if !slices.Equal(kinds, []string{"payment.confirmed", "payment.received", "settlement.received", "dispute.opened", "settlement.debited"}) {
			t.Fatalf("lançamentos: %v", kinds)
		}
		net := ledgerNet(f.store.ledger)
		if net[ledger.ProviderConfirmed] != 0 || net[ledger.ProviderClearing] != 0 || net[ledger.Cash] != 9801-10000 || net[ledger.Chargebacks] != 10000 {
			t.Fatalf("ledger: %v", net)
		}
	})
}

// A disputa ganha devolve ao disponível a retenção do chargeback.
func TestDisputeWonSettlesReleasedHold(t *testing.T) {
	f, _, c := paidFixture(t)
	f.sendSnapshot(t, "evt_cb", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour)))
	if net := ledgerNet(f.store.posted); net[ledger.Cash] != -10000 || net[ledger.ProviderClearing] != 0 || net[ledger.Chargebacks] != 10000 {
		t.Fatalf("aberta: retenção sai do disponível: %v", net)
	}
	f.sendSnapshot(t, "evt_won", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackReversed, payNow.Add(2*time.Hour)))
	if d := f.latestDispute(t, c.ID); d.Status != DisputeWon {
		t.Fatalf("disputa: %s", d.Status)
	}
	net := ledgerNet(f.store.posted)
	if net[ledger.Cash] != 0 || net[ledger.ProviderClearing] != 0 || net[ledger.Chargebacks] != 0 {
		t.Fatalf("ganha: retenção devolvida ao disponível: %v", net)
	}
	d := f.latestDispute(t, c.ID)
	st := postedByKind(f.store.posted, "settlement.received")
	if len(st) != 1 || st[0].IdempotencyKey != "settlement:reversal:dispute.opened:"+d.ID.String() || st[0].ReferenceID != d.ID {
		t.Fatalf("liquidação do estorno: %+v", st)
	}
}

// O estorno da retenção na disputa ganha aparece em todo o histórico do
// ledger, como no banco, e não só nos lançamentos observados depois da
// abertura: o saldo completo das contas fecha com o líquido do pagamento, e
// os chargebacks e os valores em liquidação voltam a zero.
func TestDisputeWonReversalIsInFullLedger(t *testing.T) {
	f, _, c := paidFixture(t)
	f.sendSnapshot(t, "evt_cb", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour)))
	// Os testes limpam posted para observar só os lançamentos seguintes; o
	// estorno precisa achar a abertura da disputa no histórico completo.
	f.store.posted = nil
	f.sendSnapshot(t, "evt_won", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackReversed, payNow.Add(2*time.Hour)))
	d := f.latestDispute(t, c.ID)
	if d.Status != DisputeWon || !slices.Equal(f.store.reversals, []string{"dispute.opened:" + d.ID.String()}) {
		t.Fatalf("disputa %s, estornos %v", d.Status, f.store.reversals)
	}
	if got := ledgerKindsOf(f.store.posted); !slices.Equal(got, []string{"dispute.opened.reversal", "settlement.received"}) {
		t.Fatalf("lançamentos depois da disputa ganha: %v", got)
	}
	net := ledgerNet(f.store.ledger)
	if net[ledger.Chargebacks] != 0 || net[ledger.ProviderClearing] != 0 || net[ledger.ProviderConfirmed] != 0 || net[ledger.Cash] != 9801 ||
		net[ledger.ProviderFees] != 199 || net[ledger.AccountsReceivable] != -10000 {
		t.Fatalf("ledger completo depois da disputa ganha: %v", net)
	}
	// A reentrega do mesmo evento não estorna de novo.
	f.sendSnapshot(t, "evt_won_2", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackReversed, payNow.Add(3*time.Hour)))
	if n := len(postedByKind(f.store.ledger, "dispute.opened.reversal")); n != 1 {
		t.Fatalf("estornos no ledger: %d", n)
	}
}

// Pagamento confirmado antes da ADR-080 foi lançado direto nos valores em
// liquidação: o recebimento posterior não lança nada (não há valor em
// pagamentos confirmados), sem deixar a conta negativa.
func TestReceiptOfLegacyConfirmationPostsNothing(t *testing.T) {
	f := newPayFixture(t)
	awaitingCharge(t, f)
	f.process(t, f.storeWebhook(t, confirmedWebhook("evt_c", "pay_1", 10000, 199)))
	pay := f.store.payments[0]
	legacy := ledger.Transaction{ID: pay.ID, TenantID: f.tenantID, Kind: "payment.confirmed", ReferenceType: "payment", ReferenceID: pay.ID,
		IdempotencyKey: "payment.confirmed:" + pay.ID.String(), Entries: []ledger.Entry{
			{Account: ledger.ProviderClearing, Direction: ledger.Debit, Amount: brl(9801)},
			{Account: ledger.ProviderFees, Direction: ledger.Debit, Amount: brl(199)},
			{Account: ledger.AccountsReceivable, Direction: ledger.Credit, Amount: brl(10000)}}}
	f.store.ledger, f.store.posted = []ledger.Transaction{legacy}, nil
	f.process(t, f.storeWebhook(t, receivedWebhook("evt_r", "pay_1", 10000, 199)))
	if len(f.store.posted) != 0 {
		t.Fatalf("recebimento de confirmação antiga não lança: %v", ledgerKindsOf(f.store.posted))
	}
}
