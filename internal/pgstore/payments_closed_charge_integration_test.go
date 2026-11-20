//go:build integration

package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/paysim"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// ledgerNetByCode devolve, por conta, débitos menos créditos do ledger do
// tenant; a chave vazia traz o total geral (zero quando o ledger está
// balanceado).
func ledgerNetByCode(t *testing.T, h *payHarness) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	err := h.env.DB.InTenantTx(context.Background(), h.tn.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT a.code,
			sum(CASE e.direction WHEN 'debit' THEN e.amount ELSE -e.amount END)::bigint
			FROM ledger_entries e JOIN ledger_accounts a ON a.tenant_id = e.tenant_id AND a.id = e.account_id
			WHERE e.tenant_id = $1 GROUP BY a.code`, h.tn.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var code string
			var net int64
			if err := rows.Scan(&code, &net); err != nil {
				return err
			}
			out[code] = net
			out[""] += net
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// requestAwaitingCharge solicita a cobrança da fatura e processa o webhook
// de criação enviado pelo simulador.
func (h *payHarness) requestAwaitingCharge(t *testing.T, invoiceID uuid.UUID) payments.Charge {
	t.Helper()
	c, err := h.pay.RequestCharge(context.Background(), h.tn.ID, invoiceID, h.tn.Actor)
	if err != nil || c == nil || c.Status != payments.ChargeAwaitingPayment {
		t.Fatalf("cobrança: %+v %v", c, err)
	}
	h.sim.DispatchDue(context.Background())
	h.processWebhooks(t)
	return *c
}

func (h *payHarness) assertNoIgnoredWebhooks(t *testing.T) {
	t.Helper()
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM provider_webhook_events WHERE tenant_id = $1 AND status = 'ignored'`, h.tn.ID); n != 0 {
		t.Fatalf("webhooks ignorados: %d", n)
	}
}

// Cobrança recusada pelo provedor e paga depois (por exemplo, por outro meio
// no mesmo checkout): a confirmação era registrada como "ignored" e o valor
// recebido não era aplicado. Agora a cobrança passa a paga, a fatura é
// quitada, a taxa é lançada no ledger e a transição incomum fica auditada.
func TestProviderPaymentOfFailedChargeIsApplied(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	inv := h.issueInvoice(t, 10000)
	c := h.requestAwaitingCharge(t, inv.ID)
	if _, err := h.sim.Apply(c.ProviderChargeID, paysim.Action{Name: "fail"}); err != nil {
		t.Fatal(err)
	}
	h.sim.DispatchDue(ctx)
	h.processWebhooks(t)
	if got, err := h.pay.Charge(ctx, h.tn.ID, c.ID); err != nil || got.Status != payments.ChargeFailed {
		t.Fatalf("cobrança recusada: %+v %v", got, err)
	}

	h.env.Clock.Advance(time.Hour)
	h.payAtProvider(t, c.ProviderChargeID)

	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM provider_webhook_events WHERE tenant_id = $1 AND provider_charge_id = $2
		AND event_type = 'charge.paid' AND status = 'processed'`, h.tn.ID, c.ProviderChargeID); n != 1 {
		t.Fatalf("webhook de pagamento processado: %d", n)
	}
	h.assertNoIgnoredWebhooks(t)
	got, err := h.pay.Charge(ctx, h.tn.ID, c.ID)
	if err != nil || got.Status != payments.ChargePaid || got.AmountPaid.Amount() != 10000 || got.FailureReason != "" {
		t.Fatalf("cobrança recusada passa a paga: %+v %v", got, err)
	}
	paid, err := h.env.Billing.Get(ctx, h.tn.ID, inv.ID)
	if err != nil || paid.Status != billing.Paid || paid.AmountPaid.Amount() != 10000 || !paid.AmountDue.IsZero() {
		t.Fatalf("fatura quitada: %s pago=%d %v", paid.Status, paid.AmountPaid.Amount(), err)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM payments WHERE tenant_id = $1 AND charge_id = $2`, h.tn.ID, c.ID); n != 1 {
		t.Fatalf("pagamentos pela cobrança recusada: %d", n)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM customer_credit_entries WHERE tenant_id = $1`, h.tn.ID); n != 0 {
		t.Fatalf("sem excedente, nenhum crédito: %d", n)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'charge.paid_after_failure'
		AND resource_id = $2 AND details->>'previous_status' = 'failed' AND details->>'failure_reason' = 'recusada pelo provedor'`,
		h.tn.ID, c.ID.String()); n != 1 {
		t.Fatalf("auditoria do pagamento após recusa: %d", n)
	}
	fee := paysim.Fee(10000)
	net := ledgerNetByCode(t, h)
	if net[""] != 0 || net["accounts_receivable"] != 0 || net["provider_fees"] != fee || net["provider_clearing"] != 10000-fee ||
		net["billed_revenue"] != -10000 {
		t.Fatalf("ledger balanceado com recebimento e taxa: %+v", net)
	}
}

// Fatura cancelada (invoice.voided) com a cobrança ainda válida no provedor
// (o cancelamento no provedor falhou e aguarda nova tentativa): o pagamento
// recebido vira crédito integral do cliente e a taxa do provedor é lançada no
// ledger, que permanece balanceado. A nova tentativa de cancelamento consulta
// o provedor, encontra a cobrança paga e encerra a operação sem repetir.
func TestProviderPaymentOfChargeOfVoidedInvoiceBecomesCredit(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	inv := h.issueInvoice(t, 10000)
	c := h.requestAwaitingCharge(t, inv.ID)
	if err := h.sim.EnqueueScenario(h.apiKey, "cancel_unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.env.Billing.Void(ctx, h.tn.ID, h.tn.Actor, inv.ID, "Emitida por engano", inv.Version); err != nil {
		t.Fatal(err)
	}
	if err := h.pay.CancelOpenCharges(ctx, h.tn.ID, inv.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if got, err := h.pay.Charge(ctx, h.tn.ID, c.ID); err != nil || got.Status != payments.ChargeCanceled {
		t.Fatalf("cobrança cancelada localmente: %+v %v", got, err)
	}

	h.payAtProvider(t, c.ProviderChargeID)

	h.assertNoIgnoredWebhooks(t)
	got, err := h.pay.Charge(ctx, h.tn.ID, c.ID)
	if err != nil || got.Status != payments.ChargePaid || got.AmountPaid.Amount() != 10000 {
		t.Fatalf("cobrança paga no provedor: %+v %v", got, err)
	}
	voided, err := h.env.Billing.Get(ctx, h.tn.ID, inv.ID)
	if err != nil || voided.Status != billing.Void || !voided.AmountPaid.IsZero() {
		t.Fatalf("fatura cancelada não muda: %s pago=%d %v", voided.Status, voided.AmountPaid.Amount(), err)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT COALESCE(sum(amount), 0)::int FROM customer_credit_entries
		WHERE tenant_id = $1 AND customer_id = $2 AND kind = 'overpayment' AND invoice_id = $3`, h.tn.ID, inv.CustomerID, inv.ID); n != 10000 {
		t.Fatalf("valor integral como crédito do cliente: %d", n)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM payments WHERE tenant_id = $1 AND charge_id = $2`, h.tn.ID, c.ID); n != 1 {
		t.Fatalf("pagamento registrado: %d", n)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'charge.paid_after_local_cancellation'
		AND resource_id = $2 AND details->>'invoice_status' = 'void'`, h.tn.ID, c.ID.String()); n != 1 {
		t.Fatalf("auditoria do pagamento após cancelamento local: %d", n)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND kind IN ('payment.overpayment', 'payment.provider_fee')`,
		h.tn.ID); n != 2 {
		t.Fatalf("lançamentos de crédito e taxa: %d", n)
	}
	fee := paysim.Fee(10000)
	net := ledgerNetByCode(t, h)
	if net[""] != 0 || net["accounts_receivable"] != 0 || net["billed_revenue"] != 0 || net["customer_credits"] != -10000 ||
		net["provider_fees"] != fee || net["provider_clearing"] != 10000-fee {
		t.Fatalf("ledger balanceado: crédito integral e taxa reconhecida: %+v", net)
	}
	h.env.Clock.Advance(10 * time.Minute)
	if _, err := h.pay.RecoverOperations(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM payment_operations WHERE tenant_id = $1 AND charge_id = $2
		AND kind = 'cancel_charge' AND status = 'failed' AND attempts = 2`, h.tn.ID, c.ID); n != 1 {
		t.Fatalf("cancelamento encerrado ao encontrar a cobrança paga: %d", n)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM payments WHERE tenant_id = $1 AND charge_id = $2`, h.tn.ID, c.ID); n != 1 {
		t.Fatalf("pagamento único após a consulta: %d", n)
	}
}

// Ordem de bloqueio: a transação do teste faz o papel do motor (ou de uma
// mudança de assinatura), que bloqueia a assinatura e depois o cliente. A
// aplicação de um pagamento com excedente espera pela assinatura sem ter
// bloqueado o cliente; antes, o crédito do excedente bloqueava o cliente
// primeiro e as duas transações entravam em impasse.
func TestOverpaymentWaitsForSubscriptionWithoutHoldingCustomer(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	inv := h.issueInvoice(t, 10000)
	c := h.requestAwaitingCharge(t, inv.ID)
	if _, err := h.sim.Apply(c.ProviderChargeID, paysim.Action{Name: "pay", Amount: 10350}); err != nil {
		t.Fatal(err)
	}
	h.sim.DispatchDue(ctx)
	var webhookID, messageID uuid.UUID
	err := h.env.DB.InTenantTx(ctx, h.tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT w.id, o.id FROM provider_webhook_events w
			JOIN outbox_events o ON o.tenant_id = w.tenant_id AND o.event_type = 'provider_webhook.received'
			 AND o.payload->>'webhook_event_id' = w.id::text
			WHERE w.tenant_id = $1 AND w.status = 'received' AND w.event_type = 'charge.paid'`, h.tn.ID).Scan(&webhookID, &messageID)
	})
	if err != nil {
		t.Fatalf("webhook de pagamento recebido: %v", err)
	}

	blocker, err := h.env.DB.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
	if _, err := blocker.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, h.tn.ID.String()); err != nil {
		t.Fatal(err)
	}
	// Limita a espera do teste: com a ordem antiga, o impasse seria detectado
	// pelo banco e uma das transações, abortada.
	if _, err := blocker.Exec(ctx, `SELECT set_config('lock_timeout', '10s', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SELECT id FROM subscriptions WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, h.tn.ID, *inv.SubscriptionID); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var processErr error
	go func() {
		defer close(done)
		processErr = h.pay.ProcessWebhook(ctx, h.tn.ID, webhookID, messageID)
	}()
	if !waitLockWaiters(t, pgtest.OwnerPool(t), 1, done) {
		t.Fatalf("a aplicação do pagamento deveria aguardar a assinatura bloqueada: %v", processErr)
	}
	// Com a assinatura bloqueada, o cliente continua livre para quem segue a
	// ordem assinatura, cliente.
	if _, err := blocker.Exec(ctx, `SELECT id FROM customers WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, h.tn.ID, inv.CustomerID); err != nil {
		t.Fatalf("o cliente não pode estar bloqueado pela aplicação do pagamento: %v", err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	if processErr != nil {
		t.Fatalf("aplicação do pagamento após a liberação da assinatura: %v", processErr)
	}

	paid, err := h.env.Billing.Get(ctx, h.tn.ID, inv.ID)
	if err != nil || paid.Status != billing.Paid || paid.AmountPaid.Amount() != 10000 {
		t.Fatalf("fatura quitada: %s pago=%d %v", paid.Status, paid.AmountPaid.Amount(), err)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT COALESCE(sum(amount), 0)::int FROM customer_credit_entries
		WHERE tenant_id = $1 AND kind = 'overpayment' AND invoice_id = $2`, h.tn.ID, inv.ID); n != 350 {
		t.Fatalf("excedente como crédito do cliente: %d", n)
	}
	if net := ledgerNetByCode(t, h); net[""] != 0 || net["customer_credits"] != -350 || net["accounts_receivable"] != 0 {
		t.Fatalf("ledger: %+v", net)
	}
}

// Criação que esgotou as tentativas com recusas transitórias do provedor e
// cuja fatura foi anulada depois: a contagem de esgotadas do banco deixa de
// incluí-la (o alerta se resolve) e a reexecução manual só consulta o
// provedor e encerra a operação, sem criar cobrança para a fatura anulada.
// As tentativas e a contagem são feitas só no tenant do teste: a
// recuperação agendada percorre todos os tenants do banco compartilhado.
func TestExhaustedCreationOfVoidedInvoiceIsClosedWithoutCharge(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	for range payments.MaxOperationAttempts {
		if err := h.sim.EnqueueScenario(h.apiKey, "unavailable"); err != nil {
			t.Fatal(err)
		}
	}
	inv := h.issueInvoice(t, 6000)
	c, err := h.pay.RequestCharge(ctx, h.tn.ID, inv.ID, h.tn.Actor)
	if err != nil || c.Status != payments.ChargePending {
		t.Fatalf("cobrança aguardando nova tentativa: %+v %v", c, err)
	}
	var opID uuid.UUID
	if err := h.env.DB.InTenantTx(ctx, h.tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_operations WHERE tenant_id = $1 AND charge_id = $2`, h.tn.ID, c.ID).Scan(&opID)
	}); err != nil {
		t.Fatal(err)
	}
	for range payments.MaxOperationAttempts - 1 {
		h.env.Clock.Advance(time.Hour)
		if err := h.pay.ExecuteOperation(ctx, h.tn.ID, opID); err != nil {
			t.Fatal(err)
		}
	}
	h.env.Clock.Advance(time.Hour)
	exhausted := func() map[payments.OperationStatus]int {
		t.Helper()
		var out map[payments.OperationStatus]int
		if err := h.env.Store.Payments().InTenant(ctx, h.tn.ID, func(tx payments.Tx) error {
			var err error
			out, err = tx.ExhaustedOperations(ctx, h.env.Clock.Now())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := exhausted(); got[payments.OpPending] != 1 || len(got) != 1 {
		t.Fatalf("pré-condição: criação pendente esgotada: %v", got)
	}

	if _, err := h.env.Billing.Void(ctx, h.tn.ID, h.tn.Actor, inv.ID, "Emitida por engano", inv.Version); err != nil {
		t.Fatal(err)
	}
	if err := h.pay.CancelOpenCharges(ctx, h.tn.ID, inv.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if got := exhausted(); len(got) != 0 {
		t.Fatalf("a criação de cobrança cancelada não conta como esgotada: %v", got)
	}

	if err := h.pay.RetryOperation(ctx, h.tn.ID, opID); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM payment_operations WHERE tenant_id = $1 AND id = $2
		AND status = 'failed' AND completed_at IS NOT NULL AND attempts = $3`, h.tn.ID, opID, payments.MaxOperationAttempts+1); n != 1 {
		t.Fatal("a reexecução encerra a operação")
	}
	got, err := h.pay.Charge(ctx, h.tn.ID, c.ID)
	if err != nil || got.Status != payments.ChargeCanceled || got.ProviderChargeID != "" {
		t.Fatalf("cobrança cancelada e sem vínculo: %+v %v", got, err)
	}
	if charges, _ := h.sim.ListCharges(h.apiKey, c.ID.String(), time.Time{}, time.Time{}, 0, 100); len(charges) != 0 {
		t.Fatalf("nenhuma cobrança no provedor para a fatura anulada: %d", len(charges))
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'charge.created'`,
		h.tn.ID); n != 0 {
		t.Fatalf("charge.created: %d", n)
	}
}
