package payments

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// creditInvoice aplica uma nota de crédito à fatura gravada, como faria a
// emissão da nota no faturamento.
func (f *payFixture) creditInvoice(t *testing.T, invoiceID uuid.UUID, amount int64) billing.Invoice {
	t.Helper()
	inv := f.store.invoices[invoiceID]
	if err := inv.ApplyCreditNote(brl(amount), f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	inv.PopChanges()
	f.store.invoices[invoiceID] = inv
	return inv
}

func (f *payFixture) chargesOf(invoiceID uuid.UUID) []Charge {
	var out []Charge
	for _, id := range f.store.chargeOrder {
		if c := f.store.charges[id]; c.InvoiceID == invoiceID {
			out = append(out, c)
		}
	}
	return out
}

func (f *payFixture) auditActions() []string {
	out := make([]string, len(f.store.audits))
	for i, a := range f.store.audits {
		out[i] = a.Action
	}
	return out
}

func confirmedWebhook(eventID, providerChargeID string, amount, fee int64) WebhookEvent {
	snap := providerSnapshot(ProviderConfirmed, "CONFIRMED", amount, fee, nil)
	snap.ProviderChargeID = providerChargeID
	return WebhookEvent{EventID: eventID, Type: "PAYMENT_CONFIRMED", ProviderChargeID: providerChargeID, Status: ProviderConfirmed, Charge: snap}
}

func TestHandleInvoiceCreditedReplacesDivergentCharge(t *testing.T) {
	f := newPayFixture(t)
	inv, old := awaitingCharge(t, f)
	f.creditInvoice(t, inv.ID, 3000)
	msg := uuid.New()

	if err := f.svc.HandleInvoiceCredited(context.Background(), f.tenantID, inv.ID, msg); err != nil {
		t.Fatal(err)
	}

	charges := f.chargesOf(inv.ID)
	if len(charges) != 2 || charges[0].ID != old.ID || charges[0].Status != ChargeCanceled {
		t.Fatalf("cobrança divergente cancelada localmente: %+v", charges)
	}
	if n := charges[1]; n.Attempt != 2 || n.Amount.Amount() != 7000 || n.Status != ChargeAwaitingPayment || n.ProviderChargeID != "pay_2" {
		t.Fatalf("nova cobrança pelo saldo: %+v", n)
	}
	if len(f.gw.createCalls) != 2 || f.gw.createCalls[1].Amount.Amount() != 7000 {
		t.Fatalf("criação no provedor pelo saldo: %+v", f.gw.createCalls)
	}
	if !slices.Contains(f.auditActions(), "charge.canceled_after_credit_note") || !f.store.inbox[inboxKey(ConsumerInvoiceCredited, msg)] {
		t.Fatalf("auditoria e inbox: %v", f.auditActions())
	}

	// Reentrega: nada a cancelar e a cobrança pelo saldo já existe.
	if err := f.svc.HandleInvoiceCredited(context.Background(), f.tenantID, inv.ID, msg); err != nil {
		t.Fatal(err)
	}
	if len(f.chargesOf(inv.ID)) != 2 || len(f.gw.createCalls) != 2 {
		t.Fatalf("reentrega não cria outra cobrança: %d cobranças, %d criações", len(f.chargesOf(inv.ID)), len(f.gw.createCalls))
	}
}

func TestHandleInvoiceCreditedFullCreditCancelsWithoutNewCharge(t *testing.T) {
	f := newPayFixture(t)
	inv, old := awaitingCharge(t, f)
	if got := f.creditInvoice(t, inv.ID, 10000); got.Status != billing.Paid {
		t.Fatalf("nota pelo saldo integral quita a fatura: %s", got.Status)
	}
	if err := f.svc.HandleInvoiceCredited(context.Background(), f.tenantID, inv.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if charges := f.chargesOf(inv.ID); len(charges) != 1 || charges[0].ID != old.ID || charges[0].Status != ChargeCanceled {
		t.Fatalf("sem saldo, a cobrança é cancelada e nenhuma outra é criada: %+v", charges)
	}
}

// Regressão: o pagamento pela cobrança antiga, cancelada só no Recorvia após a
// nota de crédito, era descartado como "ignored". A confirmação do provedor
// prevalece: o saldo é quitado, o excedente vira crédito do cliente e a taxa
// é reconhecida.
func TestProviderPaymentOfChargeCanceledAfterCreditNoteIsApplied(t *testing.T) {
	f := newPayFixture(t)
	inv, old := awaitingCharge(t, f)
	f.creditInvoice(t, inv.ID, 3000)
	if err := f.svc.HandleInvoiceCredited(context.Background(), f.tenantID, inv.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	f.store.posted, f.store.events, f.store.audits = nil, nil, nil
	f.clock.Advance(time.Hour)

	wid := f.storeWebhook(t, confirmedWebhook("evt_old", old.ProviderChargeID, 10000, 199))
	f.process(t, wid)

	if w := f.store.webhooks[wid]; w.Status != "processed" || w.Error != "" {
		t.Fatalf("confirmação de pagamento nunca é ignorada: %+v", w)
	}
	got := f.store.charges[old.ID]
	if got.Status != ChargePaid || got.AmountPaid.Amount() != 10000 || got.Fee.Amount() != 199 || got.ProviderStatus != "CONFIRMED" {
		t.Fatalf("cobrança antiga paga: %+v", got)
	}
	if len(f.store.payments) != 1 || *f.store.payments[0].ChargeID != old.ID || f.store.payments[0].Amount.Amount() != 10000 {
		t.Fatalf("pagamento registrado pela cobrança antiga: %+v", f.store.payments)
	}
	if updated := f.invoice(inv.ID); updated.Status != billing.Paid || updated.AmountPaid.Amount() != 7000 || !updated.AmountDue.IsZero() {
		t.Fatalf("saldo da fatura quitado: %s pago=%d saldo=%d", updated.Status, updated.AmountPaid.Amount(), updated.AmountDue.Amount())
	}
	if len(f.store.credits) != 1 || f.store.credits[0].amount.Amount() != 3000 || f.store.credits[0].kind != "overpayment" {
		t.Fatalf("excedente vira crédito do cliente: %+v", f.store.credits)
	}
	if kinds := f.store.ledgerKinds(); !slices.Equal(kinds, []string{"payment.confirmed", "payment.overpayment"}) {
		t.Fatalf("lançamentos: %v", kinds)
	}
	net := ledgerNet(f.store.posted)
	if net[ledger.AccountsReceivable] != -7000 || net[ledger.CustomerCredits] != -3000 || net[ledger.ProviderFees] != 199 ||
		net[ledger.ProviderClearing] != 10000-199 {
		t.Fatalf("ledger: %+v", net)
	}
	if a := f.auditActions(); len(a) == 0 || a[0] != "charge.paid_after_local_cancellation" || !slices.Contains(a, "payment.applied") {
		t.Fatalf("auditoria: %v", a)
	}
	if ev := f.store.eventTypes(); !slices.Equal(ev, []string{outbox.EventPaymentConfirmed, outbox.EventInvoicePaid}) {
		t.Fatalf("eventos: %v", ev)
	}

	// Confirmação repetida pelo provedor continua idempotente.
	f.process(t, f.storeWebhook(t, confirmedWebhook("evt_old_2", old.ProviderChargeID, 10000, 199)))
	if len(f.store.payments) != 1 || len(f.store.credits) != 1 {
		t.Fatal("confirmação repetida não duplica pagamento nem crédito")
	}
}

func TestProviderPaymentOfChargeCanceledWithVoidedInvoiceBecomesCredit(t *testing.T) {
	f := newPayFixture(t)
	inv, c := awaitingCharge(t, f)
	voided := f.invoice(inv.ID)
	if err := voided.Void("Emitida por engano", payNow); err != nil {
		t.Fatal(err)
	}
	f.store.invoices[inv.ID] = voided
	if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}

	wid := f.storeWebhook(t, confirmedWebhook("evt_1", c.ProviderChargeID, 10000, 199))
	f.process(t, wid)

	if w := f.store.webhooks[wid]; w.Status != "processed" {
		t.Fatalf("webhook: %+v", w)
	}
	if f.store.charges[c.ID].Status != ChargePaid || len(f.store.payments) != 1 {
		t.Fatal("o recebimento no provedor é registrado")
	}
	if len(f.store.credits) != 1 || f.store.credits[0].amount.Amount() != 10000 {
		t.Fatalf("pagamento de fatura cancelada vira crédito integral: %+v", f.store.credits)
	}
	if net := ledgerNet(f.store.posted); net[ledger.ProviderFees] != 199 || net[ledger.CustomerCredits] != -10000 {
		t.Fatalf("ledger: %+v", net)
	}
	if f.invoice(inv.ID).Status != billing.Void {
		t.Fatal("fatura cancelada não muda")
	}
}

// pendingCharge solicita a cobrança com o provedor indisponível: a cobrança
// fica pending com a operação aguardando nova tentativa.
func pendingCharge(t *testing.T, f *payFixture, failures int) (billing.Invoice, Charge) {
	t.Helper()
	inv := f.addInvoice(t, 10000)
	for range failures {
		f.gw.createResults = append(f.gw.createResults, gwResult{err: errRetryable})
	}
	c := f.requestCharge(t, inv.ID)
	if c.Status != ChargePending {
		t.Fatalf("cobrança deveria permanecer em criação: %s", c.Status)
	}
	return inv, c
}

// Regressão: com a cobrança ainda em criação, o consumidor devolvia erro a cada
// entrega e o ajuste se perdia quando a criação passava da janela de
// retentativas. Agora a mensagem é registrada e o ajuste acontece na
// conclusão da operação.
func TestHandleInvoiceCreditedWithChargeBeingCreatedAdjustsOnCompletion(t *testing.T) {
	f := newPayFixture(t)
	inv, first := pendingCharge(t, f, 5)
	f.creditInvoice(t, inv.ID, 3000)
	msg := uuid.New()

	if err := f.svc.HandleInvoiceCredited(context.Background(), f.tenantID, inv.ID, msg); err != nil {
		t.Fatalf("a nota de crédito com cobrança em criação não devolve erro: %v", err)
	}
	if !f.store.inbox[inboxKey(ConsumerInvoiceCredited, msg)] {
		t.Fatal("mensagem registrada no inbox sem depender de reentrega")
	}
	if charges := f.chargesOf(inv.ID); len(charges) != 1 || charges[0].Status != ChargePending {
		t.Fatalf("cobrança em criação não é alterada pelo consumidor: %+v", charges)
	}

	// A recuperação agendada repete a criação até o provedor responder; a
	// sexta tentativa cria a cobrança pelo valor anterior à nota.
	f.store.audits, f.store.events = nil, nil
	for range MaxOperationAttempts {
		f.clock.Advance(10 * time.Minute)
		if _, err := f.svc.RecoverOperations(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	charges := f.chargesOf(inv.ID)
	if len(charges) != 2 {
		t.Fatalf("cobranças: %+v", charges)
	}
	old, repl := charges[0], charges[1]
	if old.ID != first.ID || old.Status != ChargeCanceled || old.ProviderChargeID == "" || old.Amount.Amount() != 10000 {
		t.Fatalf("cobrança criada com valor anterior à nota é cancelada localmente: %+v", old)
	}
	if repl.Attempt != 2 || repl.Amount.Amount() != 7000 || repl.Status != ChargeAwaitingPayment {
		t.Fatalf("nova cobrança pelo saldo: %+v", repl)
	}
	calls := f.gw.createCalls
	if last := calls[len(calls)-1]; last.Amount.Amount() != 7000 || last.ExternalReference != repl.ID.String() {
		t.Fatalf("última criação no provedor: %+v", last)
	}
	if op := f.operationFor(t, first.ID); op.Status != OpSucceeded {
		t.Fatalf("operação original concluída: %+v", op)
	}
	var adjusted bool
	for _, a := range f.store.audits {
		if a.Action == "charge.canceled_after_balance_change" {
			adjusted = a.ResourceID == first.ID.String() && a.Details["charge_amount"] == int64(10000) &&
				a.Details["amount_due"] == int64(7000) && a.Details["replacement_charge_id"] == repl.ID
		}
	}
	if !adjusted {
		t.Fatalf("auditoria do ajuste: %+v", f.store.audits)
	}
	created := 0
	for _, e := range f.store.events {
		if e.Type == outbox.EventChargeCreated {
			created++
			if e.AggregateID != repl.ID {
				t.Fatalf("charge.created só para a cobrança vigente: %+v", e)
			}
		}
	}
	if created != 1 {
		t.Fatalf("eventos: %v", f.store.eventTypes())
	}

	// Pagamento que ainda chegue pela cobrança substituída é aplicado.
	wid := f.storeWebhook(t, confirmedWebhook("evt_old", old.ProviderChargeID, 10000, 0))
	f.process(t, wid)
	if w := f.store.webhooks[wid]; w.Status != "processed" || f.invoice(inv.ID).Status != billing.Paid {
		t.Fatalf("pagamento pela cobrança substituída: %+v %s", w, f.invoice(inv.ID).Status)
	}
}

func TestChargeCompletionAfterFullCreditCancelsWithoutReplacement(t *testing.T) {
	f := newPayFixture(t)
	inv, c := pendingCharge(t, f, 1)
	f.creditInvoice(t, inv.ID, 10000)
	if err := f.svc.HandleInvoiceCredited(context.Background(), f.tenantID, inv.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute)
	if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.operationFor(t, c.ID).ID); err != nil {
		t.Fatal(err)
	}
	charges := f.chargesOf(inv.ID)
	if len(charges) != 1 || charges[0].Status != ChargeCanceled || charges[0].ProviderChargeID == "" {
		t.Fatalf("fatura quitada pela nota: cobrança cancelada sem substituta: %+v", charges)
	}
	if a := f.store.audits; a[len(a)-2].Action != "charge.canceled_after_balance_change" || a[len(a)-2].Details["replacement_charge_id"] != nil {
		t.Fatalf("auditoria: %v", f.auditActions())
	}
}

func TestChargeCompletionWithUnchangedBalanceKeepsCharge(t *testing.T) {
	f := newPayFixture(t)
	inv, c := pendingCharge(t, f, 1)
	f.clock.Advance(time.Minute)
	if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.operationFor(t, c.ID).ID); err != nil {
		t.Fatal(err)
	}
	if charges := f.chargesOf(inv.ID); len(charges) != 1 || charges[0].Status != ChargeAwaitingPayment {
		t.Fatalf("saldo inalterado mantém a cobrança: %+v", charges)
	}
	if slices.Contains(f.auditActions(), "charge.canceled_after_balance_change") {
		t.Fatal("nenhum ajuste sem divergência")
	}
}

func TestChargeCompletionSkipsVoidedInvoice(t *testing.T) {
	f := newPayFixture(t)
	inv, c := pendingCharge(t, f, 1)
	voided := f.invoice(inv.ID)
	if err := voided.Void("Emitida por engano", payNow); err != nil {
		t.Fatal(err)
	}
	voided.PopChanges()
	f.store.invoices[inv.ID] = voided
	f.clock.Advance(time.Minute)
	if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.operationFor(t, c.ID).ID); err != nil {
		t.Fatal(err)
	}
	if charges := f.chargesOf(inv.ID); len(charges) != 1 || charges[0].Status != ChargeAwaitingPayment {
		t.Fatalf("fatura cancelada segue o consumidor de invoice.voided: %+v", charges)
	}
}

func TestChargeCompletionReplacementWithoutDefaultAccount(t *testing.T) {
	f := newPayFixture(t)
	inv, c := pendingCharge(t, f, 1)
	f.creditInvoice(t, inv.ID, 3000)
	acc := f.store.accounts[f.account.ID]
	acc.IsDefault = false
	f.store.accounts[f.account.ID] = acc
	f.clock.Advance(time.Minute)
	if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.operationFor(t, c.ID).ID); err != nil {
		t.Fatal(err)
	}
	if charges := f.chargesOf(inv.ID); len(charges) != 1 || charges[0].Status != ChargeCanceled {
		t.Fatalf("sem provedor padrão, a cobrança divergente é cancelada sem substituta: %+v", charges)
	}
}

func TestChargeCompletionReplacementCreationIsRecovered(t *testing.T) {
	f := newPayFixture(t)
	inv, c := pendingCharge(t, f, 1)
	f.creditInvoice(t, inv.ID, 3000)
	// A criação original responde; a da substituta falha e fica para a recuperação.
	f.gw.createResults = []gwResult{{pc: ProviderCharge{ProviderChargeID: "pay_original", Status: ProviderPending, RawStatus: "PENDING",
		Amount: brl(10000)}}, {err: errRetryable}}
	f.clock.Advance(time.Minute)
	if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.operationFor(t, c.ID).ID); err != nil {
		t.Fatalf("falha na substituta não desfaz a conclusão da operação original: %v", err)
	}
	charges := f.chargesOf(inv.ID)
	if len(charges) != 2 || charges[0].Status != ChargeCanceled || charges[1].Status != ChargePending || charges[1].Amount.Amount() != 7000 {
		t.Fatalf("substituta gravada aguardando a recuperação: %+v", charges)
	}
	if op := f.operationFor(t, charges[1].ID); op.Status != OpPending || op.Attempts != 1 {
		t.Fatalf("operação da substituta: %+v", op)
	}
	f.clock.Advance(time.Minute)
	if r, err := f.svc.RecoverOperations(context.Background()); err != nil || r.Executed != 1 {
		t.Fatalf("recuperação: %d %v", r.Executed, err)
	}
	if got := f.store.charges[charges[1].ID]; got.Status != ChargeAwaitingPayment {
		t.Fatalf("substituta criada pela recuperação: %+v", got)
	}
}

func TestChargeCompletionAdjustmentFailuresRollBack(t *testing.T) {
	for _, m := range []string{"Invoice", "InsertCharge", "Location", "Audit"} {
		t.Run(m, func(t *testing.T) {
			f := newPayFixture(t)
			inv, c := pendingCharge(t, f, 1)
			f.creditInvoice(t, inv.ID, 3000)
			f.clock.Advance(time.Minute)
			boom := errors.New("falha em " + m)
			f.store.errs[m] = boom
			op := f.operationFor(t, c.ID)
			// A reivindicação também lê a fatura: a falha é injetada depois dela.
			if m == "Invoice" {
				delete(f.store.errs, m)
				f.gw.onCreate = func() { f.store.errs[m] = boom }
			}
			if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID); !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			if got := f.store.charges[c.ID]; got.Status != ChargePending || len(f.chargesOf(inv.ID)) != 1 {
				t.Fatalf("falha desfaz o registro do resultado e o ajuste: %+v", got)
			}
			if got := f.store.operations[op.ID]; got.Status != OpInFlight {
				t.Fatalf("operação continua reivindicada para a recuperação: %+v", got)
			}
		})
	}
}

func TestHandleInvoiceCreditedFailures(t *testing.T) {
	for _, m := range []string{"Invoice", "OpenChargesForInvoice", "UpdateCharge", "Audit", "DefaultAccount", "MarkInbox"} {
		t.Run(m, func(t *testing.T) {
			f := newPayFixture(t)
			inv, old := awaitingCharge(t, f)
			f.creditInvoice(t, inv.ID, 3000)
			boom := errors.New("falha em " + m)
			f.store.errs[m] = boom
			if err := f.svc.HandleInvoiceCredited(context.Background(), f.tenantID, inv.ID, uuid.New()); !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			if len(f.store.inbox) != 0 {
				t.Fatal("sem ajuste concluído, a mensagem não é marcada")
			}
			wantStatus := ChargeAwaitingPayment
			if m == "DefaultAccount" || m == "MarkInbox" {
				wantStatus = ChargeCanceled // o cancelamento local já foi confirmado; a reentrega conclui o restante
			}
			if got := f.store.charges[old.ID]; got.Status != wantStatus {
				t.Fatalf("cobrança antiga: %s, esperado %s", got.Status, wantStatus)
			}
		})
	}
	t.Run("cobrança listada como aberta em situação final", func(t *testing.T) {
		f := newPayFixture(t)
		inv, old := awaitingCharge(t, f)
		f.creditInvoice(t, inv.ID, 3000)
		stale := paidCharge(t, 10000)
		stale.TenantID, stale.InvoiceID = f.tenantID, inv.ID
		f.store.extraOpen = []Charge{stale}
		err := f.svc.HandleInvoiceCredited(context.Background(), f.tenantID, inv.ID, uuid.New())
		wantAppErr(t, err, apperr.KindConflict, "invalid_charge_transition")
		if f.store.charges[old.ID].Status != ChargeAwaitingPayment || len(f.store.inbox) != 0 {
			t.Fatal("transição inválida desfaz os cancelamentos já feitos na transação")
		}
	})
}
