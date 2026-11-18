//go:build integration

package pgstore_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/paysim"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
)

// paidCharge emite a fatura, cria a cobrança no simulador e confirma o
// pagamento pelo webhook.
func (h *payHarness) paidCharge(t *testing.T, amount int64) (billing.Invoice, payments.Charge) {
	t.Helper()
	ctx := context.Background()
	inv := h.issueInvoice(t, amount)
	c, err := h.pay.RequestCharge(ctx, h.tn.ID, inv.ID, h.tn.Actor)
	if err != nil || c == nil {
		t.Fatalf("cobrança: %v", err)
	}
	if _, err := h.sim.Apply(c.ProviderChargeID, paysim.Action{Name: "pay"}); err != nil {
		t.Fatal(err)
	}
	h.deliver(t)
	got := h.charge(t, inv.ID)
	if got.Status != payments.ChargePaid {
		t.Fatalf("cobrança não paga: %s", got.Status)
	}
	return inv, got
}

// deliver entrega os webhooks pendentes do simulador e os processa.
func (h *payHarness) deliver(t *testing.T) {
	t.Helper()
	h.env.Clock.Advance(time.Second)
	h.sim.DispatchDue(context.Background())
	h.processWebhooks(t)
}

func (h *payHarness) sum(t *testing.T, query string) int64 {
	t.Helper()
	var n int64
	err := h.env.DB.InTenantTx(context.Background(), h.tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), query, h.tn.ID).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *payHarness) accountNet(t *testing.T, code string) int64 {
	t.Helper()
	return h.sum(t, `SELECT COALESCE(sum(CASE WHEN e.direction = 'debit' THEN e.amount ELSE -e.amount END), 0)::bigint
	                   FROM ledger_entries e JOIN ledger_accounts a ON a.tenant_id = e.tenant_id AND a.id = e.account_id
	                  WHERE e.tenant_id = $1 AND a.code = '`+code+`'`)
}

func refundInput(c payments.Charge, amount int64, key string) payments.RefundInput {
	return payments.RefundInput{ChargeID: c.ID, Amount: money.New(amount, money.BRL), Reason: "Cancelamento do contrato", RequestKey: key}
}

// Pedidos concorrentes de reembolso nunca somam mais que o valor pago: o
// bloqueio da cobrança serializa as reservas e as constraints do banco
// garantem o limite mesmo sem a regra do caso de uso.
func TestConcurrentRefundsNeverExceedPaidAmount(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	_, c := h.paidCharge(t, 10000)

	const workers = 10
	var wg sync.WaitGroup
	results := make([]error, workers)
	start := make(chan struct{})
	for i := range workers {
		wg.Go(func() {
			<-start
			_, results[i] = h.pay.RequestRefund(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, refundInput(c, 3000, fmt.Sprintf("conc-%d", i)))
		})
	}
	close(start)
	wg.Wait()
	accepted, rejected := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			accepted++
		case apperr.IsKind(err, apperr.KindConflict):
			rejected++
		default:
			t.Fatalf("erro inesperado: %v", err)
		}
	}
	if accepted != 3 || rejected != workers-3 {
		t.Fatalf("aceitos=%d recusados=%d", accepted, rejected)
	}
	// Reembolsos adiados pela execução serializada são concluídos pela recuperação.
	for range 3 {
		h.env.Clock.Advance(time.Minute)
		if _, err := h.pay.RecoverOperations(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := h.charge(t, c.InvoiceID)
	if got.AmountRefunded.Amount() != 9000 || !got.AmountRefundPending.IsZero() || got.Status != payments.ChargePartiallyRefunded {
		t.Fatalf("cobrança: reembolsado=%v reservado=%v %s", got.AmountRefunded, got.AmountRefundPending, got.Status)
	}
	if n := h.sum(t, `SELECT count(*) FROM refunds WHERE tenant_id = $1 AND status = 'succeeded'`); n != 3 {
		t.Fatalf("reembolsos efetivados = %d", n)
	}
	view, err := h.sim.View(h.apiKey, c.ProviderChargeID)
	if err != nil || view.RefundedAmount != 9000 || len(view.Refunds) != 3 {
		t.Fatalf("provedor: %+v %v", view, err)
	}
	if h.accountNet(t, "refunds") != 9000 {
		t.Fatalf("ledger de reembolsos = %d", h.accountNet(t, "refunds"))
	}

	// O banco recusa reserva acima do pago, mesmo fora do caso de uso.
	err = h.env.DB.InTenantTx(ctx, h.tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE charges SET amount_refund_pending = 1001 WHERE tenant_id = $1 AND id = $2`, h.tn.ID, c.ID)
		return err
	})
	if !postgres.IsNamedCheckViolation(err, "charges_refund_reservation_limit") {
		t.Fatalf("constraint da reserva: %v", err)
	}
	err = h.env.DB.InTenantTx(ctx, h.tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE charges SET amount_charged_back = 1001 WHERE tenant_id = $1 AND id = $2`, h.tn.ID, c.ID)
		return err
	})
	if !postgres.IsNamedCheckViolation(err, "charges_refund_limit") {
		t.Fatalf("constraint do limite: %v", err)
	}
}

func TestConcurrentRefundsWithSameRequestKeyCreateOne(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	_, c := h.paidCharge(t, 10000)
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, 5)
	errs := make([]error, 5)
	for i := range 5 {
		wg.Go(func() {
			r, err := h.pay.RequestRefund(ctx, h.tn.ID, h.tn.Actor, nil, refundInput(c, 2500, "mesma-chave"))
			ids[i], errs[i] = r.ID, err
		})
	}
	wg.Wait()
	for i := range 5 {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("pedido %d: %v %v", i, ids[i], errs[i])
		}
	}
	if n := h.sum(t, `SELECT count(*) FROM refunds WHERE tenant_id = $1`); n != 1 {
		t.Fatalf("reembolsos = %d", n)
	}
}

func TestRefundLifecycleWithSimulator(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	inv, c := h.paidCharge(t, 20000)
	r, err := h.pay.RequestRefund(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, refundInput(c, 5000, "lc-1"))
	if err != nil || r.Status != payments.RefundSucceeded || r.ProviderRefundID == "" {
		t.Fatalf("reembolso: %+v %v", r, err)
	}
	// O webhook do reembolso chega depois e não duplica efeitos.
	h.deliver(t)
	if n := h.sum(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND kind = 'refund.succeeded'`); n != 1 {
		t.Fatalf("lançamentos de reembolso = %d", n)
	}
	// Reembolso feito no painel do provedor é registrado pelo webhook.
	if _, err := h.sim.Apply(c.ProviderChargeID, paysim.Action{Name: "refund", Amount: 3000}); err != nil {
		t.Fatal(err)
	}
	h.deliver(t)
	if n := h.sum(t, `SELECT count(*) FROM refunds WHERE tenant_id = $1 AND origin = 'provider' AND status = 'succeeded' AND amount = 3000`); n != 1 {
		t.Fatalf("reembolso do provedor = %d", n)
	}
	got := h.charge(t, inv.ID)
	if got.AmountRefunded.Amount() != 8000 || got.Status != payments.ChargePartiallyRefunded {
		t.Fatalf("cobrança: %v %s", got.AmountRefunded, got.Status)
	}
	if h.accountNet(t, "refunds") != 8000 {
		t.Fatalf("ledger: %d", h.accountNet(t, "refunds"))
	}
	// A fatura continua paga e registra as devoluções no histórico.
	inv2, err := h.env.Billing.Get(ctx, h.tn.ID, inv.ID)
	if err != nil || inv2.Status != billing.Paid {
		t.Fatalf("fatura: %v %v", inv2.Status, err)
	}
	if n := h.sum(t, `SELECT count(*) FROM invoice_status_history WHERE tenant_id = $1 AND reason = 'refund'`); n != 2 {
		t.Fatalf("histórico de devoluções = %d", n)
	}
	refunds, next, err := h.env.Store.ListRefunds(ctx, h.tn.ID, &c.ID, 1, "")
	if err != nil || len(refunds) != 1 || next == "" {
		t.Fatalf("listagem paginada: %d %q %v", len(refunds), next, err)
	}
	more, _, err := h.env.Store.ListRefunds(ctx, h.tn.ID, nil, 10, next)
	if err != nil || len(more) != 1 || more[0].ID == refunds[0].ID {
		t.Fatalf("segunda página: %v", err)
	}
}

// Resposta perdida depois do reembolso efetivado: a recuperação consulta a
// cobrança e encontra o reembolso pela referência, sem repetir o pedido.
func TestRefundResponseLostIsResolvedWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	_, c := h.paidCharge(t, 10000)
	if err := h.sim.EnqueueScenario(h.apiKey, "refund_error_after_success"); err != nil {
		t.Fatal(err)
	}
	r, err := h.pay.RequestRefund(ctx, h.tn.ID, h.tn.Actor, nil, refundInput(c, 4000, "lost-1"))
	if err != nil || r.Status != payments.RefundUnknown {
		t.Fatalf("reembolso: %+v %v", r, err)
	}
	if _, err := h.pay.RequestRefund(ctx, h.tn.ID, h.tn.Actor, nil, refundInput(c, 100, "lost-2")); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("novo pedido com resultado desconhecido pendente: %v", err)
	}
	h.env.Clock.Advance(10 * time.Minute)
	if _, err := h.pay.RecoverOperations(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := h.pay.Refund(ctx, h.tn.ID, r.ID)
	if err != nil || got.Status != payments.RefundSucceeded {
		t.Fatalf("reembolso após a consulta: %+v %v", got, err)
	}
	view, _ := h.sim.View(h.apiKey, c.ProviderChargeID)
	if len(view.Refunds) != 1 || view.RefundedAmount != 4000 {
		t.Fatalf("o provedor recebeu %d reembolsos", len(view.Refunds))
	}
}

func TestChargebackLifecycleWithSimulator(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	_, won := h.paidCharge(t, 10000)
	_, lost := h.paidCharge(t, 6000)
	for _, step := range []struct {
		charge payments.Charge
		stage  string
	}{{won, "requested"}, {lost, "requested"}, {won, "won"}, {lost, "lost"}} {
		if _, err := h.sim.Apply(step.charge.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: step.stage}); err != nil {
			t.Fatal(err)
		}
		h.deliver(t)
	}
	gw, gl := h.charge(t, won.InvoiceID), h.charge(t, lost.InvoiceID)
	if gw.Status != payments.ChargePaid || gl.Status != payments.ChargeChargedBack || gl.AmountChargedBack.Amount() != 6000 {
		t.Fatalf("cobranças: ganha=%s perdida=%s %v", gw.Status, gl.Status, gl.AmountChargedBack)
	}
	// Abertura retém o valor; a disputa ganha estorna o lançamento.
	if net := h.accountNet(t, "chargebacks"); net != 6000 {
		t.Fatalf("chargebacks no ledger = %d", net)
	}
	disputes, err := h.env.Store.ListDisputes(ctx, h.tn.ID, nil, 10)
	if err != nil || len(disputes) != 2 {
		t.Fatalf("disputas: %d %v", len(disputes), err)
	}
	for _, d := range disputes {
		if d.Open() || len(d.History) != 2 || d.ClosedAt == nil {
			t.Fatalf("disputa: %+v", d)
		}
	}
	// Reenvio dos eventos não reabre disputas.
	if _, err := h.sim.Apply(won.ProviderChargeID, paysim.Action{Name: "replay"}); err != nil {
		t.Fatal(err)
	}
	h.deliver(t)
	if n := h.sum(t, `SELECT count(*) FROM disputes WHERE tenant_id = $1`); n != 2 {
		t.Fatalf("disputas após reenvio = %d", n)
	}
	err = h.env.DB.InTenantTx(ctx, h.tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE dispute_events SET to_status = 'won' WHERE tenant_id = $1`, h.tn.ID)
		return err
	})
	if err == nil {
		t.Fatal("histórico de disputa deve ser imutável")
	}
}

func TestVoidedInvoiceCancelsChargeAtSimulator(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	inv := h.issueInvoice(t, 8000)
	c, err := h.pay.RequestCharge(ctx, h.tn.ID, inv.ID, h.tn.Actor)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.pay.CancelOpenCharges(ctx, h.tn.ID, inv.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	view, err := h.sim.View(h.apiKey, c.ProviderChargeID)
	if err != nil || view.Status != "canceled" {
		t.Fatalf("cobrança no provedor: %s %v", view.Status, err)
	}
	if n := h.sum(t, `SELECT count(*) FROM payment_operations WHERE tenant_id = $1 AND kind = 'cancel_charge' AND status = 'succeeded'`); n != 1 {
		t.Fatalf("operações de cancelamento = %d", n)
	}
	// O webhook do cancelamento não altera a cobrança já cancelada.
	h.deliver(t)
	if got := h.charge(t, inv.ID); got.Status != payments.ChargeCanceled {
		t.Fatalf("cobrança: %s", got.Status)
	}
	// Uma segunda operação de cancelamento para a mesma cobrança é recusada pelo banco.
	err = h.env.DB.InTenantTx(ctx, h.tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payment_operations (id, tenant_id, charge_id, kind, status, external_reference)
		                        VALUES ($1, $2, $3, 'cancel_charge', 'pending', 'x')`, uuid.New(), h.tn.ID, c.ID)
		return err
	})
	if !postgres.IsUniqueViolation(err, "payment_operations_cancel_key") {
		t.Fatalf("operação de cancelamento única: %v", err)
	}
	err = h.env.DB.InTenantTx(ctx, h.tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payment_operations (id, tenant_id, charge_id, kind, status, external_reference)
		                        VALUES ($1, $2, $3, 'refund', 'pending', 'x')`, uuid.New(), h.tn.ID, c.ID)
		return err
	})
	if !postgres.IsNamedCheckViolation(err, "payment_operations_refund_link") {
		t.Fatalf("reembolso sem vínculo: %v", err)
	}
}

func TestReconciliationWithSimulator(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	_, feeCharge := h.paidCharge(t, 10000)
	silentInv := h.issueInvoice(t, 4000)
	silent, err := h.pay.RequestCharge(ctx, h.tn.ID, silentInv.ID, h.tn.Actor)
	if err != nil {
		t.Fatal(err)
	}
	fee := int64(500)
	if _, err := h.sim.Apply(feeCharge.ProviderChargeID, paysim.Action{Name: "diverge", Fee: &fee}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sim.Apply(silent.ProviderChargeID, paysim.Action{Name: "pay_silently"}); err != nil {
		t.Fatal(err)
	}
	h.env.Clock.Advance(time.Minute)
	run, err := h.pay.RunReconciliation(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, payments.RunInput{RequestKey: "rec-int-1"})
	if err != nil || run.Status != payments.RunCompleted || run.DiscrepancyCount != 2 {
		t.Fatalf("execução: %+v %v", run, err)
	}
	list, err := h.env.Store.ListDiscrepancies(ctx, h.tn.ID, "open", "", 50)
	if err != nil || len(list) != 2 {
		t.Fatalf("divergências: %d %v", len(list), err)
	}
	var feeD, payD payments.Discrepancy
	for _, d := range list {
		switch d.Kind {
		case payments.DiscrepancyFeeMismatch:
			feeD = d
		case payments.DiscrepancyPaymentNotReflected:
			payD = d
		}
	}
	if feeD.ProviderAmount == nil || feeD.ProviderAmount.Amount() != 500 || payD.ChargeID == nil || *payD.ChargeID != silent.ID {
		t.Fatalf("divergências: %+v %+v", feeD, payD)
	}
	// Nova execução não duplica divergências.
	h.env.Clock.Advance(time.Minute)
	if _, err := h.pay.RunReconciliation(ctx, h.tn.ID, h.tn.Actor, nil, payments.RunInput{}); err != nil {
		t.Fatal(err)
	}
	if n := h.sum(t, `SELECT count(*) FROM reconciliation_discrepancies WHERE tenant_id = $1`); n != 2 {
		t.Fatalf("divergências após nova execução = %d", n)
	}
	// Resoluções: pagamento aplicado e taxa aceita com ajuste no ledger.
	just := "Conferido com o extrato do simulador."
	if _, err := h.pay.ResolveDiscrepancy(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, payments.ResolveInput{ID: payD.ID,
		Resolution: payments.ResolutionAcceptProvider, Justification: just}); err != nil {
		t.Fatal(err)
	}
	paidInv, err := h.env.Billing.Get(ctx, h.tn.ID, silentInv.ID)
	if err != nil || paidInv.Status != billing.Paid {
		t.Fatalf("pagamento não refletido aplicado: %s %v", paidInv.Status, err)
	}
	resolved, err := h.pay.ResolveDiscrepancy(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, payments.ResolveInput{ID: feeD.ID,
		Resolution: payments.ResolutionAcceptProvider, Justification: just})
	if err != nil || resolved.LedgerTransactionID == nil || resolved.ResolvedBy == nil || *resolved.ResolvedBy != h.tn.Owner.ID {
		t.Fatalf("taxa aceita: %+v %v", resolved, err)
	}
	if net := h.accountNet(t, "reconciliation_adjustments"); net != 500-paysim.Fee(10000) {
		t.Fatalf("ajuste de conciliação = %d", net)
	}
	h.env.Clock.Advance(time.Minute)
	final, err := h.pay.RunReconciliation(ctx, h.tn.ID, h.tn.Actor, nil, payments.RunInput{})
	if err != nil || final.DiscrepancyCount != 0 {
		t.Fatalf("execução final: %+v %v", final, err)
	}
	history, _, err := h.env.Store.AuditEvents(ctx, h.tn.ID, pgstore.AuditFilter{ResourceType: "reconciliation_discrepancy",
		ResourceID: feeD.ID.String(), Limit: 50})
	if err != nil || len(history) < 2 {
		t.Fatalf("histórico da divergência: %d %v", len(history), err)
	}
	runs, err := h.env.Store.ListReconciliationRuns(ctx, h.tn.ID, 10)
	if err != nil || len(runs) != 3 {
		t.Fatalf("execuções: %d %v", len(runs), err)
	}
	// Outro tenant não vê as divergências (RLS e filtro explícito).
	other := h.env.NewTenant(t, "outro")
	if l, err := h.env.Store.ListDiscrepancies(ctx, other.ID, "", "", 50); err != nil || len(l) != 0 {
		t.Fatalf("isolamento: %d %v", len(l), err)
	}
	if _, err := h.pay.Discrepancy(ctx, other.ID, feeD.ID); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("divergência de outro tenant: %v", err)
	}
	stats, err := h.env.Store.OpenDiscrepancyStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for kind, n := range stats {
		if n < 0 || kind == "" {
			t.Fatalf("estatística inválida: %s=%d", kind, n)
		}
	}
	if _, err := h.env.Store.RefundStats(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledReconciliationDiscovery(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	due := func() bool {
		ids, err := h.env.Store.Payments().TenantsDueReconciliation(ctx, h.env.Clock.Now(), payments.ScheduledReconciliationEvery)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if id == h.tn.ID {
				return true
			}
		}
		return false
	}
	if due() {
		t.Fatal("plano sem o recurso não é elegível")
	}
	if _, err := h.env.Platform.ChangePlan(ctx, h.tn.ID, h.tn.Actor, "escala"); err != nil {
		t.Fatal(err)
	}
	if !due() {
		t.Fatal("plano com o recurso é elegível")
	}
	// Outros tenants do banco compartilhado podem ser elegíveis; vale a
	// execução deste tenant.
	if _, err := h.pay.RunScheduledReconciliations(ctx); err != nil {
		t.Logf("execuções agendadas de outros tenants: %v", err)
	}
	if n := h.sum(t, `SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1 AND trigger_type = 'scheduled' AND status = 'completed'`); n != 1 {
		t.Fatalf("execuções agendadas do tenant = %d", n)
	}
	if due() {
		t.Fatal("tenant já conciliado no intervalo")
	}
	h.env.Clock.Advance(payments.ScheduledReconciliationEvery + time.Minute)
	if !due() {
		t.Fatal("elegível após o intervalo")
	}
	// Execução em andamento da mesma conta: o índice único recusa outra.
	accounts, err := h.pay.Accounts(ctx, h.tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	lease := h.env.Clock.Now().Add(time.Hour)
	_, err = envOwner(t).Exec(ctx, `INSERT INTO reconciliation_runs (id, tenant_id, provider_account_id, provider, trigger_type, status,
	    window_start, window_end, lease_until, started_at) VALUES ($1, $2, $3, 'simulator', 'manual', 'running', $4, $5, $6, $5)`,
		uuid.New(), h.tn.ID, accounts[0].ID, h.env.Clock.Now().Add(-time.Hour), h.env.Clock.Now(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pay.RunReconciliation(ctx, h.tn.ID, h.tn.Actor, nil, payments.RunInput{}); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("execução simultânea: %v", err)
	}
	h.env.Clock.Advance(2 * time.Hour)
	if run, err := h.pay.RunReconciliation(ctx, h.tn.ID, h.tn.Actor, nil, payments.RunInput{}); err != nil || run.Status != payments.RunCompleted {
		t.Fatalf("execução após lease expirado: %+v %v", run, err)
	}
	if n := h.sum(t, `SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1 AND status = 'failed'`); n != 1 {
		t.Fatalf("execução interrompida encerrada = %d", n)
	}
}
