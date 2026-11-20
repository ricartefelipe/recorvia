package payments

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

func chargebackSnapshot(pid string, status ProviderStatus, at time.Time) ProviderCharge {
	fee := brl(199)
	return ProviderCharge{ProviderChargeID: pid, Status: status, RawStatus: string(status), Amount: brl(10000), Fee: &fee, UpdatedAt: &at}
}

func chargebackWebhook(id string, snap ProviderCharge) WebhookEvent {
	return WebhookEvent{EventID: id, Type: "charge." + string(snap.Status), ProviderChargeID: snap.ProviderChargeID, Status: snap.Status, Charge: &snap}
}

func (f *payFixture) latestDispute(t *testing.T, chargeID uuid.UUID) Dispute {
	t.Helper()
	d, err := f.svc.LatestDispute(context.Background(), f.tenantID, chargeID)
	if err != nil || d == nil {
		t.Fatalf("disputa: %v %v", d, err)
	}
	return *d
}

func TestDisputeWonReversesChargeback(t *testing.T) {
	f, _, c := paidFixture(t)
	opened := payNow.Add(time.Hour)
	f.gw.getResult = chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, opened)
	f.process(t, f.storeWebhook(t, chargebackWebhook("evt_cb1", f.gw.getResult)))

	d := f.latestDispute(t, c.ID)
	if d.Status != DisputeOpened || d.Amount.Amount() != 10000 || !d.OpenedAt.Equal(opened) || f.store.charges[c.ID].Status != ChargeDisputed {
		t.Fatalf("abertura: %+v %s", d, f.store.charges[c.ID].Status)
	}
	if net := ledgerNet(postedByKind(f.store.posted, "dispute.opened")); net[ledger.Chargebacks] != 10000 || net[ledger.ProviderClearing] != -10000 {
		t.Fatalf("ledger na abertura: %v", net)
	}
	if !slices.Contains(f.store.eventTypes(), outbox.EventDisputeOpened) || len(f.store.disputeEvents) != 1 {
		t.Fatalf("evento e histórico: %v %d", f.store.eventTypes(), len(f.store.disputeEvents))
	}

	// Análise: evento sem instante usa o instante do evento.
	review := chargebackSnapshot(c.ProviderChargeID, ProviderChargebackDispute, opened)
	review.UpdatedAt = nil
	ev := chargebackWebhook("evt_cb2", review)
	at := opened.Add(time.Hour)
	ev.OccurredAt = &at
	f.process(t, f.storeWebhook(t, ev))
	if d = f.latestDispute(t, c.ID); d.Status != DisputeUnderReview || !slices.Contains(f.auditActions(), "dispute.under_review") {
		t.Fatalf("análise: %s", d.Status)
	}

	won := chargebackSnapshot(c.ProviderChargeID, ProviderChargebackReversed, opened.Add(2*time.Hour))
	f.process(t, f.storeWebhook(t, chargebackWebhook("evt_cb3", won)))
	d = f.latestDispute(t, c.ID)
	if d.Status != DisputeWon || d.ClosedAt == nil || f.store.charges[c.ID].Status != ChargePaid {
		t.Fatalf("disputa ganha: %+v %s", d, f.store.charges[c.ID].Status)
	}
	if !slices.Equal(f.store.reversals, []string{"dispute.opened:" + d.ID.String()}) || ledgerNet(f.store.posted)[ledger.Chargebacks] != 0 {
		t.Fatalf("estorno do lançamento: %v %v", f.store.reversals, ledgerNet(f.store.posted))
	}
	if len(f.store.disputeEvents) != 3 || !slices.Contains(f.store.eventTypes(), outbox.EventDisputeClosed) {
		t.Fatalf("histórico: %d", len(f.store.disputeEvents))
	}

	// Evento atrasado da abertura não reabre a disputa.
	old := chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, opened)
	f.process(t, f.storeWebhook(t, chargebackWebhook("evt_cb1_late", old)))
	if len(f.store.disputes) != 1 || f.store.charges[c.ID].Status != ChargePaid {
		t.Fatalf("evento antigo: %d %s", len(f.store.disputes), f.store.charges[c.ID].Status)
	}
	// Decisão repetida não altera nada.
	f.process(t, f.storeWebhook(t, chargebackWebhook("evt_cb3_again", won)))
	if len(f.store.reversals) != 1 {
		t.Fatal("decisão repetida")
	}
	// Nova contestação posterior abre outra disputa.
	again := chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, opened.Add(24*time.Hour))
	f.gw.getResult = again
	f.process(t, f.storeWebhook(t, chargebackWebhook("evt_cb4", again)))
	if len(f.store.disputes) != 2 || f.latestDispute(t, c.ID).Status != DisputeOpened {
		t.Fatalf("nova disputa: %d", len(f.store.disputes))
	}
}

func TestDisputeLostRecordsChargeback(t *testing.T) {
	f, inv, c := paidFixture(t)
	f.refund(t, c.ID, 4000, "")
	f.gw.getResult = chargebackSnapshot(c.ProviderChargeID, ProviderChargebackLost, payNow.Add(time.Hour))
	f.process(t, f.storeWebhook(t, chargebackWebhook("evt_lost", f.gw.getResult)))

	d := f.latestDispute(t, c.ID)
	got := f.store.charges[c.ID]
	if d.Status != DisputeLost || d.Amount.Amount() != 6000 || got.Status != ChargeChargedBack || got.AmountChargedBack.Amount() != 6000 {
		t.Fatalf("perdida: %+v %s %v", d, got.Status, got.AmountChargedBack)
	}
	if len(f.store.disputeEvents) != 2 || len(f.store.reversals) != 0 {
		t.Fatalf("abertura e decisão no mesmo retrato: %d eventos", len(f.store.disputeEvents))
	}
	h := f.store.invoiceHistory[len(f.store.invoiceHistory)-1]
	if h.invoiceID != inv.ID || h.changes[0].Reason != "chargeback" || f.invoice(inv.ID).Status != billing.Paid {
		t.Fatalf("histórico da fatura: %+v", h)
	}
	if !slices.Contains(f.auditActions(), "dispute.lost") {
		t.Fatalf("auditoria: %v", f.auditActions())
	}
}

func TestDisputeEdgeCases(t *testing.T) {
	t.Run("cobrança integralmente reembolsada não abre disputa", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.refund(t, c.ID, 10000, "")
		f.gw.getResult = chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour))
		f.process(t, f.storeWebhook(t, chargebackWebhook("evt", f.gw.getResult)))
		if len(f.store.disputes) != 0 {
			t.Fatal("sem saldo contestável")
		}
	})
	t.Run("retrato anterior ao último registrado não altera a disputa", func(t *testing.T) {
		f, _, c := paidFixture(t)
		at := payNow.Add(2 * time.Hour)
		f.gw.getResult = chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, at)
		f.process(t, f.storeWebhook(t, chargebackWebhook("evt_1", f.gw.getResult)))
		old := chargebackSnapshot(c.ProviderChargeID, ProviderChargebackLost, at.Add(-time.Hour))
		f.process(t, f.storeWebhook(t, chargebackWebhook("evt_0", old)))
		if f.latestDispute(t, c.ID).Status != DisputeOpened {
			t.Fatal("retrato antigo ignorado")
		}
	})
	t.Run("análise depois da decisão é ignorada", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.getResult = chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour))
		f.process(t, f.storeWebhook(t, chargebackWebhook("evt_1", f.gw.getResult)))
		d := f.latestDispute(t, c.ID)
		d.Status = DisputeWon
		f.store.disputes[d.ID] = d
		ch := f.store.charges[c.ID]
		if err := f.svc.syncDispute(context.Background(), f.store, &ch, chargebackSnapshot(c.ProviderChargeID, ProviderChargebackDispute,
			payNow.Add(2*time.Hour)), payNow, providerActor, false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("consulta sem chargeback encerra disputa aberta como ganha", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.getResult = chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour))
		f.process(t, f.storeWebhook(t, chargebackWebhook("evt_1", f.gw.getResult)))
		ch := f.store.charges[c.ID]
		received := chargebackSnapshot(c.ProviderChargeID, ProviderReceived, payNow.Add(2*time.Hour))
		if err := f.svc.syncDispute(context.Background(), f.store, &ch, received, payNow, providerActor, false); err != nil ||
			f.latestDispute(t, c.ID).Status != DisputeOpened {
			t.Fatal("evento sem chargeback não decide a disputa")
		}
		if err := f.svc.syncDispute(context.Background(), f.store, &ch, received, payNow, providerActor, true); err != nil ||
			f.latestDispute(t, c.ID).Status != DisputeWon || ch.Status != ChargePaid {
			t.Fatalf("consulta autoritativa: %v %s", err, ch.Status)
		}
	})
	for _, m := range []string{"LatestDispute", "InsertDispute", "InsertDisputeEvent", "PostLedger", "Emit", "UpdateDispute", "ReverseLedger"} {
		t.Run("falha em "+m, func(t *testing.T) {
			f, _, c := paidFixture(t)
			snap := chargebackSnapshot(c.ProviderChargeID, ProviderChargebackReversed, payNow.Add(time.Hour))
			boom := errors.New(m)
			f.store.errs[m] = boom
			ch := f.store.charges[c.ID]
			if err := f.svc.syncDispute(context.Background(), f.store, &ch, snap, payNow, providerActor, false); !errors.Is(err, boom) {
				t.Fatalf("erro: %v", err)
			}
		})
	}
	for _, m := range []string{"Audit", "Invoice", "InsertInvoiceHistory"} {
		t.Run("falha na perda em "+m, func(t *testing.T) {
			f, _, c := paidFixture(t)
			f.gw.getResult = chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour))
			f.process(t, f.storeWebhook(t, chargebackWebhook("evt_1", f.gw.getResult)))
			boom := errors.New(m)
			f.store.errs[m] = boom
			ch := f.store.charges[c.ID]
			lost := chargebackSnapshot(c.ProviderChargeID, ProviderChargebackLost, payNow.Add(2*time.Hour))
			if err := f.svc.syncDispute(context.Background(), f.store, &ch, lost, payNow, providerActor, false); !errors.Is(err, boom) {
				t.Fatalf("erro: %v", err)
			}
		})
	}
	t.Run("consulta da disputa com falha", func(t *testing.T) {
		f := newPayFixture(t)
		f.store.errs["LatestDispute"] = errors.New("x")
		if _, err := f.svc.LatestDispute(context.Background(), f.tenantID, uuid.New()); err == nil {
			t.Fatal("esperava erro")
		}
	})
}

// sendSnapshot processa o webhook com o retrato informado (também usado na
// consulta ao provedor) e devolve a situação registrada do webhook.
func (f *payFixture) sendSnapshot(t *testing.T, id string, snap ProviderCharge) StoredWebhook {
	t.Helper()
	f.gw.getResult = snap
	wid := f.storeWebhook(t, chargebackWebhook(id, snap))
	f.process(t, wid)
	return f.store.webhooks[wid]
}

// Reembolsos e disputa na mesma cobrança: o chargeback da disputa perdida
// não estorna de novo o valor reembolsado (confirmado ou reservado), e o
// ledger registra em chargebacks somente o valor estornado, de modo que
// chargebacks e reembolsos nunca somam mais que o pago (ADR-036).
func TestDisputeLostWithRefundsKeepsLedgerConsistent(t *testing.T) {
	at := func(h int) time.Time { return payNow.Add(time.Duration(h) * time.Hour) }
	exact := func(r Refund, state RefundState) ProviderRefund {
		return ProviderRefund{ExternalReference: r.ID.String(), State: state, Amount: brl(3000)}
	}
	panel := ProviderRefund{ProviderRefundID: "rf_painel", State: RefundStateDone, Amount: brl(3000)}
	cases := []struct {
		name string
		// pending pede, antes do chargeback, um reembolso de 30,00 que o
		// provedor aceita sem confirmação imediata.
		pending      bool
		snapshots    func(pid string, r Refund) []ProviderCharge
		chargedBack  int64
		refunded     int64
		refundStatus RefundStatus
		released     int64
		restored     int64
	}{
		{name: "reembolso pendente confirmado durante a disputa", pending: true,
			snapshots: func(pid string, r Refund) []ProviderCharge {
				return []ProviderCharge{withRefunds(chargebackSnapshot(pid, ProviderChargebackRequested, at(1))),
					withRefunds(chargebackSnapshot(pid, ProviderChargebackRequested, at(2)), exact(r, RefundStateDone)),
					withRefunds(chargebackSnapshot(pid, ProviderChargebackLost, at(3)), exact(r, RefundStateDone))}
			}, chargedBack: 7000, refunded: 3000, refundStatus: RefundSucceeded, released: 3000},
		{name: "reembolso pendente confirmado depois da disputa perdida", pending: true,
			snapshots: func(pid string, r Refund) []ProviderCharge {
				return []ProviderCharge{withRefunds(chargebackSnapshot(pid, ProviderChargebackRequested, at(1))),
					withRefunds(chargebackSnapshot(pid, ProviderChargebackLost, at(2))),
					withRefunds(chargebackSnapshot(pid, ProviderChargebackLost, at(3)), exact(r, RefundStateDone))}
			}, chargedBack: 7000, refunded: 3000, refundStatus: RefundSucceeded, released: 3000},
		{name: "reembolso pendente recusado durante a disputa", pending: true,
			snapshots: func(pid string, r Refund) []ProviderCharge {
				return []ProviderCharge{withRefunds(chargebackSnapshot(pid, ProviderChargebackRequested, at(1))),
					withRefunds(chargebackSnapshot(pid, ProviderChargebackRequested, at(2)), exact(r, RefundStateFailed)),
					withRefunds(chargebackSnapshot(pid, ProviderChargebackLost, at(3)), exact(r, RefundStateFailed))}
			}, chargedBack: 10000, refundStatus: RefundFailed},
		{name: "reembolso pendente recusado depois da disputa perdida", pending: true,
			snapshots: func(pid string, r Refund) []ProviderCharge {
				return []ProviderCharge{withRefunds(chargebackSnapshot(pid, ProviderChargebackRequested, at(1))),
					withRefunds(chargebackSnapshot(pid, ProviderChargebackLost, at(2))),
					withRefunds(chargebackSnapshot(pid, ProviderChargebackLost, at(3)), exact(r, RefundStateFailed))}
			}, chargedBack: 10000, refundStatus: RefundFailed, released: 3000, restored: 3000},
		{name: "reembolso feito no provedor durante a disputa",
			snapshots: func(pid string, _ Refund) []ProviderCharge {
				return []ProviderCharge{withRefunds(chargebackSnapshot(pid, ProviderChargebackRequested, at(1))),
					withRefunds(chargebackSnapshot(pid, ProviderChargebackRequested, at(2)), panel),
					withRefunds(chargebackSnapshot(pid, ProviderChargebackLost, at(3)), panel)}
			}, chargedBack: 7000, refunded: 3000, released: 3000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _, c := paidFixture(t)
			var r Refund
			if tc.pending {
				f.gw.refundResults = []refundRes{{pr: ProviderRefund{Status: "PENDING", State: RefundStatePending, Amount: brl(3000)}}}
				if r = f.refund(t, c.ID, 3000, ""); r.Status != RefundRequested {
					t.Fatalf("reembolso aguarda o provedor: %s", r.Status)
				}
			}
			for i, snap := range tc.snapshots(c.ProviderChargeID, r) {
				if w := f.sendSnapshot(t, fmt.Sprintf("evt_%d", i), snap); w.Status != "processed" {
					t.Fatalf("webhook %d: %s (%s)", i, w.Status, w.Error)
				}
			}
			got := f.store.charges[c.ID]
			if got.Status != ChargeChargedBack || got.AmountChargedBack.Amount() != tc.chargedBack || got.AmountRefunded.Amount() != tc.refunded ||
				!got.AmountRefundPending.IsZero() {
				t.Fatalf("cobrança: %s estornado=%v reembolsado=%v reservado=%v", got.Status, got.AmountChargedBack, got.AmountRefunded,
					got.AmountRefundPending)
			}
			if tc.pending && f.store.refunds[r.ID].Status != tc.refundStatus {
				t.Fatalf("reembolso: %s", f.store.refunds[r.ID].Status)
			}
			net := ledgerNet(f.store.posted)
			if net[ledger.Chargebacks] != tc.chargedBack || net[ledger.Refunds] != tc.refunded ||
				net[ledger.ProviderClearing] != -(tc.chargedBack+tc.refunded) {
				t.Fatalf("ledger: %v", net)
			}
			released := postedByKind(f.store.posted, "dispute.hold_released")
			if tc.released == 0 && len(released) != 0 || tc.released > 0 &&
				(len(released) != 1 || ledgerNet(released)[ledger.ProviderClearing] != tc.released) {
				t.Fatalf("retenção liberada: %+v", released)
			}
			if restored := postedByKind(f.store.posted, "dispute.hold_restored"); ledgerNet(restored)[ledger.Chargebacks] != tc.restored ||
				(tc.restored > 0) != slices.Contains(f.auditActions(), "dispute.chargeback_increased") {
				t.Fatalf("chargeback do reembolso recusado: %+v", restored)
			}
			d := f.latestDispute(t, c.ID)
			h := f.store.invoiceHistory[slices.IndexFunc(f.store.invoiceHistory, func(w invoiceHistoryWrite) bool {
				return w.changes[0].Reason == "chargeback"
			})]
			if d.Amount.Amount() != 10000 || h.changes[0].Details["amount"] != tc.chargedBack-tc.restored ||
				h.changes[0].Details["disputed_amount"] != int64(10000) {
				t.Fatalf("disputa e histórico: %v %+v", d.Amount, h.changes[0].Details)
			}
		})
	}
}

func withRefunds(snap ProviderCharge, refunds ...ProviderRefund) ProviderCharge {
	snap.Refunds = refunds
	return snap
}

// Reembolso pedido com a cobrança paga e ainda não enviado (aguardando outro
// reembolso da cobrança) não é enviado ao provedor depois do chargeback: o
// pagador receberia o valor duas vezes. O pedido é encerrado como recusado e
// a reserva é liberada; reembolso com envio anterior só é consultado.
func TestRefundNotSentAfterChargeback(t *testing.T) {
	t.Run("pedido aguardando outro reembolso", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{pr: ProviderRefund{Status: "PENDING", State: RefundStatePending, Amount: brl(2000)}}}
		first := f.refund(t, c.ID, 2000, "")
		second := f.refund(t, c.ID, 3000, "")
		if first.Status != RefundRequested || second.Status != RefundRequested || len(f.gw.refundCalls) != 1 {
			t.Fatalf("segundo pedido aguarda o primeiro: %s %s %d", first.Status, second.Status, len(f.gw.refundCalls))
		}
		if w := f.sendSnapshot(t, "evt_cb", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour))); w.Status != "processed" {
			t.Fatalf("chargeback: %s", w.Status)
		}
		f.clock.Advance(time.Hour)
		if _, err := f.svc.RecoverOperations(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := f.store.refunds[second.ID]
		if len(f.gw.refundCalls) != 1 || got.Status != RefundFailed || !strings.Contains(got.FailureReason, "disputa") {
			t.Fatalf("pedido não enviado: chamadas=%d %s %q", len(f.gw.refundCalls), got.Status, got.FailureReason)
		}
		if op := f.refundOperation(t, second.ID); op.Status != OpFailed {
			t.Fatalf("operação: %s", op.Status)
		}
		if ch := f.store.charges[c.ID]; ch.Status != ChargeDisputed || ch.AmountRefundPending.Amount() != 2000 {
			t.Fatalf("reserva do pedido recusado liberada: %s %v", ch.Status, ch.AmountRefundPending)
		}
		if !slices.Contains(f.store.eventTypes(), outbox.EventRefundFailed) {
			t.Fatalf("eventos: %v", f.store.eventTypes())
		}
	})
	t.Run("pedido aguardando com a disputa já perdida", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{pr: ProviderRefund{Status: "PENDING", State: RefundStatePending, Amount: brl(2000)}}}
		first := f.refund(t, c.ID, 2000, "")
		second := f.refund(t, c.ID, 3000, "")
		f.sendSnapshot(t, "evt_cb", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour)))
		f.sendSnapshot(t, "evt_lost", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackLost, payNow.Add(2*time.Hour)))
		if ch := f.store.charges[c.ID]; ch.AmountChargedBack.Amount() != 5000 || ch.AmountRefundPending.Amount() != 5000 {
			t.Fatalf("perda com reservas: estornado=%v reservado=%v", ch.AmountChargedBack, ch.AmountRefundPending)
		}
		f.clock.Advance(time.Hour)
		if _, err := f.svc.RecoverOperations(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := f.store.refunds[second.ID]; got.Status != RefundFailed || len(f.gw.refundCalls) != 1 {
			t.Fatalf("pedido não enviado: %s chamadas=%d", got.Status, len(f.gw.refundCalls))
		}
		f.sendSnapshot(t, "evt_done", withRefunds(chargebackSnapshot(c.ProviderChargeID, ProviderChargebackLost, payNow.Add(3*time.Hour)),
			ProviderRefund{ExternalReference: first.ID.String(), State: RefundStateDone, Amount: brl(2000)}))
		ch := f.store.charges[c.ID]
		if ch.AmountChargedBack.Amount() != 8000 || ch.AmountRefunded.Amount() != 2000 || !ch.AmountRefundPending.IsZero() {
			t.Fatalf("cobrança: estornado=%v reembolsado=%v reservado=%v", ch.AmountChargedBack, ch.AmountRefunded, ch.AmountRefundPending)
		}
		if net := ledgerNet(f.store.posted); net[ledger.Chargebacks] != 8000 || net[ledger.Refunds] != 2000 || net[ledger.ProviderClearing] != -10000 {
			t.Fatalf("ledger: %v", net)
		}
	})
	t.Run("pedido com resultado desconhecido só é consultado", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{err: errAmbiguous}}
		r := f.refund(t, c.ID, 3000, "")
		if r.Status != RefundUnknown {
			t.Fatalf("resultado desconhecido: %s", r.Status)
		}
		f.sendSnapshot(t, "evt_cb", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour)))
		f.clock.Advance(time.Hour)
		if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.refundOperation(t, r.ID).ID); err != nil {
			t.Fatal(err)
		}
		if len(f.gw.refundCalls) != 1 || len(f.gw.getCalls) == 0 || f.store.refunds[r.ID].Status != RefundFailed ||
			!f.store.charges[c.ID].AmountRefundPending.IsZero() {
			t.Fatalf("sem novo envio: chamadas=%d consultas=%d %s", len(f.gw.refundCalls), len(f.gw.getCalls), f.store.refunds[r.ID].Status)
		}
	})
}

func (f *payFixture) cancelOperation(t *testing.T, chargeID uuid.UUID) Operation {
	t.Helper()
	for _, op := range f.store.operations {
		if op.ChargeID == chargeID && op.Kind == OpCancelCharge {
			return op
		}
	}
	t.Fatalf("cancelamento da cobrança %s não encontrado", chargeID)
	return Operation{}
}

func TestVoidedInvoiceCancelsChargeAtProvider(t *testing.T) {
	f := newPayFixture(t)
	inv, c := awaitingCharge(t, f)
	if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if f.store.charges[c.ID].Status != ChargeCanceled || !slices.Equal(f.gw.cancelCalls, []string{c.ProviderChargeID}) {
		t.Fatalf("cancelamento: %s %v", f.store.charges[c.ID].Status, f.gw.cancelCalls)
	}
	if op := f.cancelOperation(t, c.ID); op.Status != OpSucceeded || op.ExternalReference != c.ID.String() {
		t.Fatalf("operação: %+v", op)
	}
	if !slices.Contains(f.auditActions(), "charge.provider_canceled") {
		t.Fatalf("auditoria: %v", f.auditActions())
	}
	// A cobrança já cancelada não gera nova operação.
	ch := f.store.charges[c.ID]
	if id, ok, err := f.svc.scheduleProviderCancel(context.Background(), f.store, ch, payNow); err != nil || ok || id != uuid.Nil {
		t.Fatalf("operação única por cobrança: %v %v", ok, err)
	}
}

func TestProviderCancelOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status OperationStatus
		action string
	}{
		{"cobrança inexistente no provedor", &GatewayError{Kind: ErrKindNotFound, StatusCode: 404}, OpSucceeded, "charge.provider_canceled"},
		{"recusa", errPermanent, OpFailed, "charge.provider_cancel_failed"},
		{"falha repetível", errRetryable, OpPending, ""},
		{"resposta ambígua", errAmbiguous, OpUnknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPayFixture(t)
			inv, c := awaitingCharge(t, f)
			f.gw.cancelErrs = []error{tc.err}
			if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, uuid.New()); err != nil {
				t.Fatal(err)
			}
			if op := f.cancelOperation(t, c.ID); op.Status != tc.status {
				t.Fatalf("operação %s, esperada %s", op.Status, tc.status)
			}
			if tc.action != "" && !slices.Contains(f.auditActions(), tc.action) {
				t.Fatalf("auditoria: %v", f.auditActions())
			}
		})
	}
	t.Run("provedor indisponível na instalação", func(t *testing.T) {
		f := newPayFixture(t)
		inv, c := awaitingCharge(t, f)
		f.resolver.gwErr = errors.New("sem adaptador")
		if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, uuid.New()); err != nil {
			t.Fatal(err)
		}
		if f.cancelOperation(t, c.ID).Status != OpFailed {
			t.Fatal("falha definitiva")
		}
	})
}

func TestProviderCancelAfterAmbiguousOutcomeQueriesFirst(t *testing.T) {
	cases := []struct {
		name    string
		get     gwResult
		calls   int
		status  OperationStatus
		paidNow bool
	}{
		{"já cancelada", gwResult{pc: ProviderCharge{ProviderChargeID: "pay_1", Status: ProviderCanceled, Amount: brl(10000)}}, 1, OpSucceeded, false},
		{"inexistente", gwResult{err: &GatewayError{Kind: ErrKindNotFound}}, 1, OpSucceeded, false},
		{"ainda pendente", gwResult{pc: ProviderCharge{ProviderChargeID: "pay_1", Status: ProviderPending, Amount: brl(10000)}}, 2, OpSucceeded, false},
		{"consulta sem resposta", gwResult{err: errAmbiguous}, 1, OpUnknown, false},
		{"paga no provedor", gwResult{pc: *providerSnapshot(ProviderReceived, "RECEIVED", 10000, 199, nil)}, 1, OpFailed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPayFixture(t)
			inv, c := awaitingCharge(t, f)
			f.gw.cancelErrs = []error{errAmbiguous}
			if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, uuid.New()); err != nil {
				t.Fatal(err)
			}
			f.gw.getResult, f.gw.getErr = tc.get.pc, tc.get.err
			f.clock.Advance(time.Hour)
			if _, err := f.svc.RecoverOperations(context.Background()); err != nil {
				t.Fatal(err)
			}
			op := f.cancelOperation(t, c.ID)
			if len(f.gw.cancelCalls) != tc.calls || op.Status != tc.status {
				t.Fatalf("chamadas=%d operação=%s", len(f.gw.cancelCalls), op.Status)
			}
			if tc.paidNow && (f.store.charges[c.ID].Status != ChargePaid || f.invoice(inv.ID).Status != billing.Paid) {
				t.Fatalf("pagamento no provedor é aplicado: %s %s", f.store.charges[c.ID].Status, f.invoice(inv.ID).Status)
			}
		})
	}
}

// Cobrança cancelada localmente antes da criação no provedor: a criação
// pendente passa a só consultar o provedor pela referência externa
// (Operation.Obsolete) e nunca cria a cobrança. Se uma tentativa anterior
// chegou a criá-la, a cobrança encontrada é vinculada e cancelada também no
// provedor; caso contrário, a operação é encerrada sem chamada de criação.
func TestChargeCanceledBeforeCreationIsNeverCreatedAtProvider(t *testing.T) {
	t.Run("sem cobrança no provedor: encerra sem criar", func(t *testing.T) {
		f := newPayFixture(t)
		inv, c := pendingCharge(t, f, 1)
		if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, uuid.New()); err != nil {
			t.Fatal(err)
		}
		creates := len(f.gw.createCalls) // a primeira tentativa, recusada de forma transitória
		f.clock.Advance(time.Hour)
		op := f.operationFor(t, c.ID)
		if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID); err != nil {
			t.Fatal(err)
		}
		got := f.store.charges[c.ID]
		if len(f.gw.createCalls) != creates || len(f.gw.cancelCalls) != 0 || !slices.Equal(f.gw.findCalls, []string{op.ExternalReference}) {
			t.Fatalf("chamadas: criação %d (antes %d), cancelamento %v, consulta %v", len(f.gw.createCalls), creates, f.gw.cancelCalls, f.gw.findCalls)
		}
		if got.Status != ChargeCanceled || got.ProviderChargeID != "" {
			t.Fatalf("cobrança: %s %q", got.Status, got.ProviderChargeID)
		}
		if after := f.store.operations[op.ID]; after.Status != OpFailed || after.LastError != obsoleteOperationError {
			t.Fatalf("operação: %s %q", after.Status, after.LastError)
		}
	})
	t.Run("cobrança criada por tentativa anterior: vincula e cancela no provedor", func(t *testing.T) {
		f := newPayFixture(t)
		inv, c := pendingCharge(t, f, 1)
		if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, uuid.New()); err != nil {
			t.Fatal(err)
		}
		creates := len(f.gw.createCalls)
		f.clock.Advance(time.Hour)
		f.gw.findResult = &ProviderCharge{ProviderChargeID: "ch_anterior", Status: ProviderPending, RawStatus: "pending", Amount: c.Amount}
		if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.operationFor(t, c.ID).ID); err != nil {
			t.Fatal(err)
		}
		got := f.store.charges[c.ID]
		if len(f.gw.createCalls) != creates {
			t.Fatalf("a cobrança cancelada não pode ser criada no provedor: %d chamadas novas", len(f.gw.createCalls)-creates)
		}
		if got.Status != ChargeCanceled || got.ProviderChargeID != "ch_anterior" || !slices.Equal(f.gw.cancelCalls, []string{"ch_anterior"}) {
			t.Fatalf("cancelamento após o vínculo: %s %q %v", got.Status, got.ProviderChargeID, f.gw.cancelCalls)
		}
	})
}

func TestProviderCancelEdgeCases(t *testing.T) {
	t.Run("cobrança sem vínculo conclui sem chamada", func(t *testing.T) {
		f := newPayFixture(t)
		_, c := pendingCharge(t, f, 1)
		op := Operation{ID: uuid.New(), TenantID: f.tenantID, ChargeID: c.ID, Kind: OpCancelCharge, Status: OpPending, NextAttemptAt: payNow}
		f.store.operations[op.ID] = op
		if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID); err != nil || f.store.operations[op.ID].Status != OpSucceeded {
			t.Fatalf("operação: %v %s", err, f.store.operations[op.ID].Status)
		}
	})
	for _, m := range []string{"EnsureOperation", "UpdateCharge"} {
		t.Run("falha ao agendar: "+m, func(t *testing.T) {
			f := newPayFixture(t)
			inv, _ := awaitingCharge(t, f)
			boom := errors.New(m)
			f.store.errs[m] = boom
			if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, uuid.New()); !errors.Is(err, boom) {
				t.Fatalf("erro: %v", err)
			}
		})
	}
	t.Run("falha ao agendar após nota de crédito", func(t *testing.T) {
		f := newPayFixture(t)
		inv, _ := awaitingCharge(t, f)
		f.creditInvoice(t, inv.ID, 3000)
		boom := errors.New("x")
		f.store.errs["EnsureOperation"] = boom
		if err := f.svc.HandleInvoiceCredited(context.Background(), f.tenantID, inv.ID, uuid.New()); !errors.Is(err, boom) {
			t.Fatalf("erro: %v", err)
		}
	})
	for _, m := range []string{"Operation", "Charge", "UpdateOperation", "Audit"} {
		t.Run("falha ao registrar: "+m, func(t *testing.T) {
			f := newPayFixture(t)
			inv, c := awaitingCharge(t, f)
			boom := errors.New(m)
			if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, uuid.New()); err != nil {
				t.Fatal(err)
			}
			op := f.cancelOperation(t, c.ID)
			op.Status, op.CompletedAt = OpUnknown, nil
			f.store.operations[op.ID] = op
			f.clock.Advance(time.Hour)
			f.gw.getResult = ProviderCharge{ProviderChargeID: c.ProviderChargeID, Status: ProviderCanceled, Amount: brl(10000)}
			w, err := func() (opWork, error) {
				var w opWork
				err := f.store.InTenant(context.Background(), f.tenantID, func(tx Tx) error {
					var err error
					w, err = f.svc.claimOperation(context.Background(), tx, op.ID, execRecovery)
					return err
				})
				return w, err
			}()
			if err != nil {
				t.Fatal(err)
			}
			f.store.errs[m] = boom
			if err := f.svc.runCancel(context.Background(), f.tenantID, w); !errors.Is(err, boom) {
				t.Fatalf("erro: %v", err)
			}
		})
	}
}
