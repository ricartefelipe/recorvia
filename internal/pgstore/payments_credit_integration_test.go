//go:build integration

package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/paysim"
)

// issueCreditNote emite uma nota de crédito pela fatura, como a API faria.
func (h *payHarness) issueCreditNote(t *testing.T, invoiceID uuid.UUID, amount int64) {
	t.Helper()
	svc := billing.NewCreditNoteService(h.env.Store.Billing(), h.env.Clock)
	if _, err := svc.Issue(context.Background(), h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, invoiceID, brlCents(amount),
		"Desconto comercial negociado", "nota-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
}

// payAtProvider paga no simulador a cobrança informada e processa os webhooks.
func (h *payHarness) payAtProvider(t *testing.T, providerChargeID string) {
	t.Helper()
	if _, err := h.sim.Apply(providerChargeID, paysim.Action{Name: "pay"}); err != nil {
		t.Fatal(err)
	}
	h.sim.DispatchDue(context.Background())
	h.processWebhooks(t)
}

func (h *payHarness) charges(t *testing.T, invoiceID uuid.UUID) []payments.Charge {
	t.Helper()
	cs, err := h.env.Store.InvoiceCharges(context.Background(), h.tn.ID, invoiceID)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// assertOldChargePaymentApplied verifica que o pagamento pela cobrança
// cancelada localmente quitou o saldo e que o excedente virou crédito.
func (h *payHarness) assertOldChargePaymentApplied(t *testing.T, invoiceID uuid.UUID, old payments.Charge, total, due int64) {
	t.Helper()
	ctx := context.Background()
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM provider_webhook_events WHERE tenant_id = $1 AND provider_charge_id = $2
		AND event_type = 'charge.paid' AND status = 'processed'`, h.tn.ID, old.ProviderChargeID); n != 1 {
		t.Fatalf("webhook de pagamento processado (nunca ignorado): %d", n)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM provider_webhook_events WHERE tenant_id = $1 AND status = 'ignored'`, h.tn.ID); n != 0 {
		t.Fatalf("webhooks ignorados: %d", n)
	}
	got, err := h.pay.Charge(ctx, h.tn.ID, old.ID)
	if err != nil || got.Status != payments.ChargePaid || got.AmountPaid.Amount() != total {
		t.Fatalf("cobrança antiga paga: %+v %v", got, err)
	}
	inv, err := h.env.Billing.Get(ctx, h.tn.ID, invoiceID)
	if err != nil || inv.Status != billing.Paid || inv.AmountPaid.Amount() != due || !inv.AmountDue.IsZero() {
		t.Fatalf("fatura quitada pelo saldo: %s pago=%d saldo=%d %v", inv.Status, inv.AmountPaid.Amount(), inv.AmountDue.Amount(), err)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT COALESCE(sum(amount), 0)::int FROM customer_credit_entries
		WHERE tenant_id = $1 AND kind = 'overpayment' AND invoice_id = $2`, h.tn.ID, invoiceID); int64(n) != total-due {
		t.Fatalf("excedente como crédito do cliente: %d, esperado %d", n, total-due)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM payments WHERE tenant_id = $1 AND charge_id = $2`, h.tn.ID, old.ID); n != 1 {
		t.Fatalf("pagamentos pela cobrança antiga: %d", n)
	}
	fee := paysim.Fee(total)
	if n := countRows(t, h.env, h.tn.ID, `SELECT COALESCE(sum(e.amount),0)::int FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
		WHERE e.tenant_id = $1 AND a.code = 'provider_fees' AND e.direction = 'debit'`, h.tn.ID); int64(n) != fee {
		t.Fatalf("taxa no ledger = %d, esperado %d", n, fee)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'charge.paid_after_local_cancellation'
		AND resource_id = $2`, h.tn.ID, old.ID.String()); n != 1 {
		t.Fatalf("auditoria do pagamento após cancelamento local: %d", n)
	}
}

// Após a nota de crédito, a cobrança aguardando pagamento é cancelada só no
// Recorvia e continua válida no provedor. O pagamento por ela é aplicado ao
// saldo e o excedente vira crédito do cliente.
func TestProviderPaymentOfChargeCanceledAfterCreditNote(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	inv := h.issueInvoice(t, 10000)
	old, err := h.pay.RequestCharge(ctx, h.tn.ID, inv.ID, h.tn.Actor)
	if err != nil || old == nil || old.Status != payments.ChargeAwaitingPayment {
		t.Fatalf("cobrança: %+v %v", old, err)
	}
	h.issueCreditNote(t, inv.ID, 3000)
	if err := h.pay.HandleInvoiceCredited(ctx, h.tn.ID, inv.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	cs := h.charges(t, inv.ID)
	if len(cs) != 2 || cs[0].Status != payments.ChargeCanceled || cs[1].Status != payments.ChargeAwaitingPayment || cs[1].Amount.Amount() != 7000 {
		t.Fatalf("cobrança divergente substituída pelo saldo: %+v", cs)
	}

	h.payAtProvider(t, old.ProviderChargeID)

	h.assertOldChargePaymentApplied(t, inv.ID, *old, 10000, 7000)
}

// Nota de crédito emitida com a cobrança ainda em criação (provedor
// indisponível): o consumidor registra a mensagem sem erro e o ajuste ocorre
// quando a recuperação conclui a criação, sem depender de reentrega.
func TestCreditNoteWhileChargeIsBeingCreatedIsAdjustedOnCompletion(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	if err := h.sim.EnqueueScenario(h.apiKey, "unavailable"); err != nil {
		t.Fatal(err)
	}
	inv := h.issueInvoice(t, 10000)
	first, err := h.pay.RequestCharge(ctx, h.tn.ID, inv.ID, h.tn.Actor)
	if err != nil || first == nil || first.Status != payments.ChargePending {
		t.Fatalf("cobrança deveria aguardar nova tentativa: %+v %v", first, err)
	}
	h.issueCreditNote(t, inv.ID, 3000)
	msg := uuid.New()
	if err := h.pay.HandleInvoiceCredited(ctx, h.tn.ID, inv.ID, msg); err != nil {
		t.Fatalf("nota de crédito com cobrança em criação não devolve erro: %v", err)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM inbox_messages WHERE tenant_id = $1 AND consumer = $2 AND message_id = $3`,
		h.tn.ID, payments.ConsumerInvoiceCredited, msg); n != 1 {
		t.Fatalf("mensagem registrada no inbox: %d", n)
	}
	if cs := h.charges(t, inv.ID); len(cs) != 1 || cs[0].Status != payments.ChargePending {
		t.Fatalf("cobrança em criação não é alterada pelo consumidor: %+v", cs)
	}

	h.env.Clock.Advance(time.Minute)
	if _, err := h.pay.RecoverOperations(ctx); err != nil {
		t.Fatal(err)
	}

	cs := h.charges(t, inv.ID)
	if len(cs) != 2 {
		t.Fatalf("cobranças após a conclusão: %+v", cs)
	}
	old, repl := cs[0], cs[1]
	if old.ID != first.ID || old.Status != payments.ChargeCanceled || old.ProviderChargeID == "" || old.Amount.Amount() != 10000 {
		t.Fatalf("cobrança criada pelo valor anterior à nota é cancelada localmente: %+v", old)
	}
	if repl.Attempt != 2 || repl.Status != payments.ChargeAwaitingPayment || repl.Amount.Amount() != 7000 || repl.ProviderChargeID == "" {
		t.Fatalf("nova cobrança pelo saldo: %+v", repl)
	}
	atProvider, _ := h.sim.ListCharges(h.apiKey, repl.ID.String(), time.Time{}, time.Time{}, 0, 100)
	if len(atProvider) != 1 || atProvider[0].Amount != 7000 {
		t.Fatalf("cobrança pelo saldo no provedor: %+v", atProvider)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'charge.canceled_after_balance_change'
		AND resource_id = $2 AND details->>'replacement_charge_id' = $3`, h.tn.ID, old.ID.String(), repl.ID.String()); n != 1 {
		t.Fatalf("auditoria do ajuste: %d", n)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM payment_operations WHERE tenant_id = $1 AND status <> 'succeeded'`, h.tn.ID); n != 0 {
		t.Fatalf("operações pendentes: %d", n)
	}

	// O cliente ainda paga a cobrança antiga, válida no provedor.
	h.payAtProvider(t, old.ProviderChargeID)
	h.assertOldChargePaymentApplied(t, inv.ID, old, 10000, 7000)
}
