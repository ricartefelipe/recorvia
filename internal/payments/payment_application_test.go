package payments

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// failedCharge cria a cobrança aguardando pagamento e aplica a recusa
// informada pelo provedor por webhook.
func failedCharge(t *testing.T, f *payFixture) (billing.Invoice, Charge) {
	t.Helper()
	inv, c := awaitingCharge(t, f)
	f.process(t, f.storeWebhook(t, WebhookEvent{EventID: "evt_refused_" + c.ProviderChargeID, Type: "PAYMENT_CREDIT_CARD_CAPTURE_REFUSED",
		ProviderChargeID: c.ProviderChargeID, Status: ProviderFailed, Charge: providerSnapshot(ProviderFailed, "REFUSED", 10000, 0, nil)}))
	got := f.store.charges[c.ID]
	if got.Status != ChargeFailed || got.FailureReason != "recusada pelo provedor" {
		t.Fatalf("cobrança recusada pelo provedor: %+v", got)
	}
	f.store.posted, f.store.events, f.store.audits = nil, nil, nil
	return inv, got
}

// Regressão: a confirmação do provedor para cobrança recusada era registrada
// como "ignored" (transição failed para paid rejeitada) e o valor recebido
// não era aplicado. O dinheiro recebido nunca é descartado: a cobrança passa
// a paga, o saldo é quitado e a taxa é reconhecida, com auditoria da
// transição incomum.
func TestProviderPaymentOfFailedChargeIsApplied(t *testing.T) {
	f := newPayFixture(t)
	inv, c := failedCharge(t, f)
	f.clock.Advance(time.Hour)

	wid := f.storeWebhook(t, confirmedWebhook("evt_paid", c.ProviderChargeID, 10000, 199))
	f.process(t, wid)

	if w := f.store.webhooks[wid]; w.Status != "processed" || w.Error != "" {
		t.Fatalf("confirmação de pagamento nunca é ignorada: %+v", w)
	}
	if len(f.gw.getCalls) != 0 {
		t.Fatal("confirmação após recusa não exige consulta ao provedor")
	}
	got := f.store.charges[c.ID]
	if got.Status != ChargePaid || got.AmountPaid.Amount() != 10000 || got.Fee.Amount() != 199 || got.ProviderStatus != "CONFIRMED" {
		t.Fatalf("cobrança recusada passa a paga: %+v", got)
	}
	if got.FailureReason != "" {
		t.Fatalf("o motivo da recusa fica somente na auditoria: %q", got.FailureReason)
	}
	if len(f.store.payments) != 1 || *f.store.payments[0].ChargeID != c.ID || f.store.payments[0].Amount.Amount() != 10000 {
		t.Fatalf("pagamento registrado pela cobrança recusada: %+v", f.store.payments)
	}
	if updated := f.invoice(inv.ID); updated.Status != billing.Paid || updated.AmountPaid.Amount() != 10000 || !updated.AmountDue.IsZero() {
		t.Fatalf("fatura quitada: %s pago=%d saldo=%d", updated.Status, updated.AmountPaid.Amount(), updated.AmountDue.Amount())
	}
	if len(f.store.credits) != 0 {
		t.Fatalf("sem excedente, nenhum crédito: %+v", f.store.credits)
	}
	if kinds := f.store.ledgerKinds(); !slices.Equal(kinds, []string{"payment.confirmed"}) {
		t.Fatalf("lançamentos: %v", kinds)
	}
	net := ledgerNet(f.store.posted)
	if net[ledger.AccountsReceivable] != -10000 || net[ledger.ProviderConfirmed] != 10000 || net[ledger.ProviderFees] != 0 {
		t.Fatalf("ledger: bruto confirmado, taxa só no recebimento: %+v", net)
	}
	if len(f.store.audits) == 0 || f.store.audits[0].Action != "charge.paid_after_failure" || !slices.Contains(f.auditActions(), "payment.applied") {
		t.Fatalf("auditoria: %v", f.auditActions())
	}
	d := f.store.audits[0].Details
	if f.store.audits[0].ResourceID != c.ID.String() || d["previous_status"] != string(ChargeFailed) || d["failure_reason"] != "recusada pelo provedor" ||
		d["payment_id"] != f.store.payments[0].ID || d["amount"] != int64(10000) || d["invoice_status"] != string(billing.Open) {
		t.Fatalf("detalhes da auditoria da transição incomum: %+v", f.store.audits[0])
	}
	if ev := f.store.eventTypes(); !slices.Equal(ev, []string{outbox.EventPaymentConfirmed, outbox.EventInvoicePaid}) {
		t.Fatalf("eventos: %v", ev)
	}
	if !slices.Equal(f.store.standing, []uuid.UUID{*inv.SubscriptionID}) {
		t.Fatalf("situação da assinatura atualizada: %v", f.store.standing)
	}

	// Confirmação repetida pelo provedor continua idempotente.
	f.process(t, f.storeWebhook(t, confirmedWebhook("evt_paid_2", c.ProviderChargeID, 10000, 199)))
	if len(f.store.payments) != 1 || len(f.store.posted) != 1 {
		t.Fatal("confirmação repetida não duplica pagamento nem lançamento")
	}
}

// A fatura foi quitada pela nova tentativa; o pagamento que ainda chega pela
// cobrança recusada vira crédito integral do cliente e a taxa é reconhecida.
func TestProviderPaymentOfFailedChargeAfterRetryBecomesCredit(t *testing.T) {
	f := newPayFixture(t)
	inv, old := failedCharge(t, f)
	retry, err := f.svc.RetryCharge(context.Background(), f.tenantID, f.actor, inv.ID)
	if err != nil || retry.Attempt != 2 || retry.Status != ChargeAwaitingPayment {
		t.Fatalf("nova tentativa: %+v %v", retry, err)
	}
	f.process(t, f.storeWebhook(t, confirmedWebhook("evt_retry_paid", retry.ProviderChargeID, 10000, 199)))
	if f.invoice(inv.ID).Status != billing.Paid {
		t.Fatal("a nova tentativa quita a fatura")
	}
	f.store.posted, f.store.audits = nil, nil

	wid := f.storeWebhook(t, confirmedWebhook("evt_old_paid", old.ProviderChargeID, 10000, 199))
	f.process(t, wid)

	if w := f.store.webhooks[wid]; w.Status != "processed" {
		t.Fatalf("webhook: %+v", w)
	}
	if f.store.charges[old.ID].Status != ChargePaid || len(f.store.payments) != 2 {
		t.Fatal("o recebimento pela cobrança recusada é registrado")
	}
	if len(f.store.credits) != 1 || f.store.credits[0].amount.Amount() != 10000 || f.store.credits[0].kind != "overpayment" {
		t.Fatalf("valor integral vira crédito do cliente: %+v", f.store.credits)
	}
	net := ledgerNet(f.store.posted)
	if net[ledger.CustomerCredits] != -10000 || net[ledger.ProviderConfirmed] != 10000 || net[ledger.ProviderFees] != 0 {
		t.Fatalf("ledger: bruto confirmado, taxa só no recebimento: %+v", net)
	}
	if a := f.auditActions(); len(a) == 0 || a[0] != "charge.paid_after_failure" || f.store.audits[0].Details["invoice_status"] != string(billing.Paid) {
		t.Fatalf("auditoria: %v", a)
	}
	if got := f.invoice(inv.ID); got.AmountPaid.Amount() != 10000 {
		t.Fatalf("fatura já quitada não muda: %+v", got)
	}
}

// A assinatura é bloqueada antes do cliente em toda aplicação de pagamento,
// na mesma ordem do motor, das mudanças de assinatura, da correção de consumo
// e da ingestão; antes, o crédito do excedente (bloqueio do cliente) vinha
// antes da atualização da situação da assinatura, o que permitia impasse.
func TestPaymentLocksSubscriptionBeforeCustomer(t *testing.T) {
	cases := []struct {
		name  string
		run   func(t *testing.T, f *payFixture) billing.Invoice
		locks []string
	}{
		{"excedente confirmado pelo provedor", func(t *testing.T, f *payFixture) billing.Invoice {
			inv, c := awaitingCharge(t, f)
			f.store.locks = nil
			f.process(t, f.storeWebhook(t, confirmedWebhook("evt_1", c.ProviderChargeID, 10350, 199)))
			return inv
		}, []string{"subscription", "customer"}},
		{"excedente em pagamento manual", func(t *testing.T, f *payFixture) billing.Invoice {
			inv := f.addInvoice(t, 10000)
			if _, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID,
				ManualPaymentInput{InvoiceID: inv.ID, Amount: brl(10350), Method: "pix", RequestKey: "k1"}); err != nil {
				t.Fatal(err)
			}
			return inv
		}, []string{"subscription", "customer"}},
		{"cobrança encontrada já paga ao concluir a criação", func(t *testing.T, f *payFixture) billing.Invoice {
			inv := f.addInvoice(t, 10000)
			f.gw.createResults = []gwResult{{err: errAmbiguous}}
			c := f.requestCharge(t, inv.ID)
			found := providerSnapshot(ProviderReceived, "RECEIVED", 10350, 199, nil)
			found.ProviderChargeID, found.ProviderCustomerID = "pay_88", "cus_1"
			f.gw.findResult = found
			f.store.locks = nil
			f.clock.Advance(30 * time.Second)
			if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.operationFor(t, c.ID).ID); err != nil {
				t.Fatal(err)
			}
			if f.store.providerCustomers[f.customer.String()+"|simulator"] != "cus_1" {
				t.Fatal("vínculo do cliente no provedor gravado")
			}
			return inv
		}, []string{"subscription", "customer", "customer"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPayFixture(t)
			inv := tc.run(t, f)
			if !slices.Equal(f.store.locks, tc.locks) {
				t.Fatalf("ordem de bloqueio: %v, esperado %v", f.store.locks, tc.locks)
			}
			// A ordem não altera o resultado.
			got := f.invoice(inv.ID)
			if got.Status != billing.Paid || got.AmountPaid.Amount() != 10000 || !slices.Equal(f.store.standing, []uuid.UUID{*inv.SubscriptionID}) {
				t.Fatalf("fatura e assinatura: %s pago=%d situação=%v", got.Status, got.AmountPaid.Amount(), f.store.standing)
			}
			if len(f.store.credits) != 1 || f.store.credits[0].amount.Amount() != 350 {
				t.Fatalf("excedente como crédito do cliente: %+v", f.store.credits)
			}
		})
	}
}

// Regressão: a resposta da solicitação devolvia a cobrança original mesmo
// quando ela era substituída na conclusão síncrona da criação (saldo alterado
// durante a chamada ao provedor). A resposta é a cobrança vigente, inclusive
// quando a substituta também é substituída.
func TestRequestChargeReturnsCurrentChargeAfterReplacement(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	credits := []int64{3000, 2000}
	f.gw.onCreate = func() {
		if len(credits) > 0 {
			f.creditInvoice(t, inv.ID, credits[0])
			credits = credits[1:]
		}
	}

	c := f.requestCharge(t, inv.ID)

	charges := f.chargesOf(inv.ID)
	if len(charges) != 3 || charges[0].Status != ChargeCanceled || charges[1].Status != ChargeCanceled {
		t.Fatalf("cobranças substituídas na conclusão: %+v", charges)
	}
	current := charges[2]
	if c.ID != current.ID || c.Attempt != 3 || c.Amount.Amount() != 5000 || c.Status != ChargeAwaitingPayment || c.ProviderChargeID != "pay_3" {
		t.Fatalf("a resposta é a cobrança vigente: %+v (vigente %+v)", c, current)
	}
}

func TestRetryChargeReturnsReplacementAfterPartialPaymentDuringCreation(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.gw.createResults = []gwResult{{err: errPermanent}}
	failed := f.requestCharge(t, inv.ID)
	if failed.Status != ChargeFailed {
		t.Fatalf("primeira tentativa recusada: %s", failed.Status)
	}
	paid := false
	f.gw.onCreate = func() {
		if paid {
			return
		}
		paid = true
		// Pagamento manual parcial registrado enquanto a criação está em andamento.
		if _, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID,
			ManualPaymentInput{InvoiceID: inv.ID, Amount: brl(4000), Method: "pix", RequestKey: "parcial"}); err != nil {
			t.Fatal(err)
		}
	}

	c, err := f.svc.RetryCharge(context.Background(), f.tenantID, f.actor, inv.ID)
	if err != nil {
		t.Fatal(err)
	}

	charges := f.chargesOf(inv.ID)
	if len(charges) != 3 || charges[1].Attempt != 2 || charges[1].Status != ChargeCanceled {
		t.Fatalf("tentativa 2 substituída pelo saldo: %+v", charges)
	}
	if c.ID != charges[2].ID || c.Attempt != 3 || c.Amount.Amount() != 6000 || c.Status != ChargeAwaitingPayment {
		t.Fatalf("a nova tentativa devolve a cobrança vigente: %+v", c)
	}
}
