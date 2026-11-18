package payments

import (
	"context"
	"errors"
	"slices"
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

func TestChargeCanceledDuringCreationIsCanceledAtProvider(t *testing.T) {
	f := newPayFixture(t)
	inv, c := pendingCharge(t, f, 1)
	if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if len(f.gw.cancelCalls) != 0 {
		t.Fatal("sem vínculo no provedor ainda não há o que cancelar")
	}
	f.clock.Advance(time.Hour)
	if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.operationFor(t, c.ID).ID); err != nil {
		t.Fatal(err)
	}
	got := f.store.charges[c.ID]
	if got.Status != ChargeCanceled || got.ProviderChargeID == "" || !slices.Equal(f.gw.cancelCalls, []string{got.ProviderChargeID}) {
		t.Fatalf("cancelamento após a criação: %s %q %v", got.Status, got.ProviderChargeID, f.gw.cancelCalls)
	}
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
					w, err = f.svc.claimOperation(context.Background(), tx, op.ID)
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
