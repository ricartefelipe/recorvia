//go:build integration

package pgstore_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/paysim"
)

// Fluxos de ponta a ponta de recebíveis com o simulador real: webhooks
// repetidos e fora de ordem, disputas em cada desfecho e divergências criadas
// no provedor, sempre com o ledger balanceado.

// receive entrega os webhooks pendentes do simulador à recepção (persistência
// e outbox), sem processá-los.
func (h *payHarness) receive(t *testing.T) {
	t.Helper()
	h.env.Clock.Advance(time.Second)
	h.sim.DispatchDue(context.Background())
}

// processWebhooksReversed processa os webhooks recebidos do mais recente
// para o mais antigo, como entregas fora de ordem; os já processados são
// ignorados pelo inbox.
func (h *payHarness) processWebhooksReversed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	type event struct {
		id      uuid.UUID
		payload []byte
	}
	var evs []event
	err := h.env.DB.InTenantTx(ctx, h.tn.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, payload FROM outbox_events WHERE tenant_id = $1 AND event_type = 'provider_webhook.received'
		                             ORDER BY occurred_at DESC, id DESC`, h.tn.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e event
			if err := rows.Scan(&e.id, &e.payload); err != nil {
				return err
			}
			evs = append(evs, e)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		var p struct {
			ID uuid.UUID `json:"webhook_event_id"`
		}
		if err := json.Unmarshal(e.payload, &p); err != nil {
			t.Fatal(err)
		}
		if err := h.pay.ProcessWebhook(ctx, h.tn.ID, p.ID, e.id); err != nil {
			t.Fatalf("processamento do webhook: %v", err)
		}
	}
}

// assertLedgerBalanced exige partidas dobradas no tenant: a soma dos débitos
// é igual à dos créditos.
func (h *payHarness) assertLedgerBalanced(t *testing.T) {
	t.Helper()
	if net := h.sum(t, `SELECT COALESCE(sum(CASE WHEN direction = 'debit' THEN amount ELSE -amount END), 0)::bigint
	                      FROM ledger_entries WHERE tenant_id = $1`); net != 0 {
		t.Fatalf("ledger desbalanceado: diferença de %d", net)
	}
}

func (h *payHarness) run(t *testing.T) payments.ReconciliationRun {
	t.Helper()
	h.env.Clock.Advance(time.Minute)
	run, err := h.pay.RunReconciliation(context.Background(), h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, payments.RunInput{})
	if err != nil || run.Status != payments.RunCompleted {
		t.Fatalf("execução: %+v %v", run, err)
	}
	return run
}

func (h *payHarness) openDiscrepancies(t *testing.T) map[payments.DiscrepancyKind]payments.Discrepancy {
	t.Helper()
	list, _, err := h.env.Store.ListDiscrepancies(context.Background(), h.tn.ID, "open", "", 100, "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[payments.DiscrepancyKind]payments.Discrepancy{}
	for _, d := range list {
		if _, dup := out[d.Kind]; dup {
			t.Fatalf("mais de uma divergência aberta do tipo %s", d.Kind)
		}
		out[d.Kind] = d
	}
	return out
}

// Reembolsos parcial e total feitos no painel do provedor chegam por
// webhooks processados fora de ordem e repetidos: cada reembolso é
// registrado uma vez, a cobrança fica reembolsada e a fatura continua paga.
func TestProviderRefundWebhooksOutOfOrderAndRepeated(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	inv, c := h.paidCharge(t, 10000)
	h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "refund", Amount: 3000})
	h.receive(t)
	h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "refund"}) // saldo restante
	h.receive(t)
	h.processWebhooksReversed(t)

	got := h.charge(t, inv.ID)
	if got.Status != payments.ChargeRefunded || got.AmountRefunded.Amount() != 10000 || !got.AmountRefundPending.IsZero() {
		t.Fatalf("cobrança: %s reembolsado=%v reservado=%v", got.Status, got.AmountRefunded, got.AmountRefundPending)
	}
	check := func(stage string) {
		t.Helper()
		if n := h.sum(t, `SELECT count(*) FROM refunds WHERE tenant_id = $1 AND origin = 'provider' AND status = 'succeeded'`); n != 2 {
			t.Fatalf("%s: reembolsos do provedor = %d", stage, n)
		}
		if n := h.sum(t, `SELECT count(DISTINCT provider_refund_id) FROM refunds WHERE tenant_id = $1`); n != 2 {
			t.Fatalf("%s: identificadores do provedor = %d", stage, n)
		}
		if n := h.sum(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND kind = 'refund.succeeded'`); n != 2 {
			t.Fatalf("%s: lançamentos de reembolso = %d", stage, n)
		}
		if net := h.accountNet(t, "refunds"); net != 10000 {
			t.Fatalf("%s: conta de reembolsos = %d", stage, net)
		}
		h.assertLedgerBalanced(t)
	}
	check("fora de ordem")

	// Reenvio de todos os eventos da cobrança, também fora de ordem.
	h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "replay"})
	h.receive(t)
	h.processWebhooksReversed(t)
	check("após o reenvio")
	if n := h.sum(t, `SELECT count(*) FROM provider_webhook_events WHERE tenant_id = $1`); n < 3 {
		t.Fatalf("webhooks recebidos = %d", n)
	}
	paid, err := h.env.Billing.Get(ctx, h.tn.ID, inv.ID)
	if err != nil || paid.Status != billing.Paid {
		t.Fatalf("fatura: %s %v", paid.Status, err)
	}
	if _, err := h.pay.RequestRefund(ctx, h.tn.ID, h.tn.Actor, nil, refundInput(c, 100, "sem-saldo")); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("reembolso de cobrança reembolsada: %v", err)
	}
}

// Reembolso pedido pelo Recorvia com a confirmação do provedor chegando por
// webhook antes do registro do resultado da chamada: a resposta e o webhook
// confirmam o mesmo reembolso uma única vez.
func TestRecorviaRefundsPartialAndTotalWithWebhooks(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	inv, c := h.paidCharge(t, 12000)
	partial, err := h.pay.RequestRefund(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, refundInput(c, 2000, "parcial-1"))
	if err != nil || partial.Status != payments.RefundSucceeded {
		t.Fatalf("reembolso parcial: %+v %v", partial, err)
	}
	h.receive(t)
	rest, err := h.pay.RequestRefund(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, refundInput(c, 10000, "total-1"))
	if err != nil || rest.Status != payments.RefundSucceeded {
		t.Fatalf("reembolso do saldo: %+v %v", rest, err)
	}
	h.receive(t)
	h.processWebhooksReversed(t)
	h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "replay"})
	h.deliver(t)

	got := h.charge(t, inv.ID)
	if got.Status != payments.ChargeRefunded || got.AmountRefunded.Amount() != 12000 {
		t.Fatalf("cobrança: %s %v", got.Status, got.AmountRefunded)
	}
	if n := h.sum(t, `SELECT count(*) FROM refunds WHERE tenant_id = $1`); n != 2 {
		t.Fatalf("reembolsos = %d (nenhum registrado como feito no provedor)", n)
	}
	if n := h.sum(t, `SELECT count(*) FROM payment_operations WHERE tenant_id = $1 AND kind = 'refund' AND status = 'succeeded' AND attempts = 1`); n != 2 {
		t.Fatalf("operações de reembolso concluídas em uma tentativa = %d", n)
	}
	view, err := h.sim.View(h.apiKey, c.ProviderChargeID)
	if err != nil || len(view.Refunds) != 2 || view.RefundedAmount != 12000 {
		t.Fatalf("provedor: %+v %v", view, err)
	}
	if h.accountNet(t, "refunds") != 12000 {
		t.Fatalf("conta de reembolsos = %d", h.accountNet(t, "refunds"))
	}
	h.assertLedgerBalanced(t)
	// Sem divergências: os dois lados concordam.
	if run := h.run(t); run.DiscrepancyCount != 0 {
		t.Fatalf("divergências após os reembolsos = %d", run.DiscrepancyCount)
	}
}

// Chargebacks: aberto (valor retido, reembolso recusado, conciliação sem
// divergência), ganho e perdido, com os webhooks da decisão chegando antes
// dos da abertura.
func TestChargebackOpenWonAndLostOutOfOrder(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	_, open := h.paidCharge(t, 5000)
	_, won := h.paidCharge(t, 10000)
	_, lost := h.paidCharge(t, 6000)
	for _, c := range []payments.Charge{open, won, lost} {
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: "requested"})
	}
	h.receive(t)
	h.simApply(t, won.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: "won"})
	h.simApply(t, lost.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: "lost"})
	h.receive(t)
	h.processWebhooksReversed(t)

	gOpen, gWon, gLost := h.charge(t, open.InvoiceID), h.charge(t, won.InvoiceID), h.charge(t, lost.InvoiceID)
	if gOpen.Status != payments.ChargeDisputed || gWon.Status != payments.ChargePaid || gLost.Status != payments.ChargeChargedBack ||
		gLost.AmountChargedBack.Amount() != 6000 || !gWon.AmountChargedBack.IsZero() {
		t.Fatalf("cobranças: aberta=%s ganha=%s perdida=%s %v", gOpen.Status, gWon.Status, gLost.Status, gLost.AmountChargedBack)
	}
	disputes, _, err := h.env.Store.ListDisputes(ctx, h.tn.ID, nil, 10, "")
	if err != nil || len(disputes) != 3 {
		t.Fatalf("disputas: %d %v", len(disputes), err)
	}
	for _, d := range disputes {
		switch d.ChargeID {
		case open.ID:
			if d.Status != payments.DisputeOpened || d.ClosedAt != nil || len(d.History) != 1 || d.Amount.Amount() != 5000 {
				t.Fatalf("disputa aberta: %+v", d)
			}
		case won.ID:
			if d.Status != payments.DisputeWon || d.ClosedAt == nil || len(d.History) != 2 {
				t.Fatalf("disputa ganha: %+v", d)
			}
		case lost.ID:
			if d.Status != payments.DisputeLost || d.ClosedAt == nil || len(d.History) != 2 {
				t.Fatalf("disputa perdida: %+v", d)
			}
		}
	}
	// Retido: aberta (5000) e perdida (6000); a ganha teve a retenção estornada.
	if net := h.accountNet(t, "chargebacks"); net != 11000 {
		t.Fatalf("chargebacks no ledger = %d", net)
	}
	if n := h.sum(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND reverses_transaction_id IS NOT NULL`); n != 1 {
		t.Fatalf("estornos de retenção = %d", n)
	}
	h.assertLedgerBalanced(t)
	if _, err := h.pay.RequestRefund(ctx, h.tn.ID, h.tn.Actor, nil, refundInput(open, 100, "durante-disputa")); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("reembolso durante a disputa: %v", err)
	}
	if n := h.sum(t, `SELECT count(*) FROM invoice_status_history WHERE tenant_id = $1 AND reason = 'chargeback'`); n != 1 {
		t.Fatalf("histórico de chargeback = %d", n)
	}
	// Os dois lados concordam: sem divergências de chargeback.
	if run := h.run(t); run.DiscrepancyCount != 0 {
		t.Fatalf("divergências: %d %v", run.DiscrepancyCount, h.openDiscrepancies(t))
	}

	// Reenvio dos eventos não reabre disputas decididas nem duplica a aberta.
	for _, c := range []payments.Charge{open, won, lost} {
		h.simApply(t, c.ProviderChargeID, paysim.Action{Name: "replay"})
	}
	h.receive(t)
	h.processWebhooksReversed(t)
	if n := h.sum(t, `SELECT count(*) FROM disputes WHERE tenant_id = $1`); n != 3 {
		t.Fatalf("disputas após o reenvio = %d", n)
	}
	if net := h.accountNet(t, "chargebacks"); net != 11000 {
		t.Fatalf("chargebacks após o reenvio = %d", net)
	}
}

// Divergências criadas no simulador (ação diverge) são detectadas, resolvidas
// com justificativa e lançamento de ajuste, não voltam com os mesmos valores,
// são resolvidas automaticamente quando somem e ficam isoladas por tenant.
func TestReconciliationDivergeResolvedWithBalancedLedger(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	_, amountCharge := h.paidCharge(t, 10000)
	_, feeCharge := h.paidCharge(t, 8000)
	h.simApply(t, amountCharge.ProviderChargeID, paysim.Action{Name: "diverge", Amount: 9000})
	fee := int64(700)
	h.simApply(t, feeCharge.ProviderChargeID, paysim.Action{Name: "diverge", Fee: &fee})

	if run := h.run(t); run.DiscrepancyCount != 2 {
		t.Fatalf("divergências detectadas = %d", run.DiscrepancyCount)
	}
	open := h.openDiscrepancies(t)
	amountD, feeD := open[payments.DiscrepancyAmountMismatch], open[payments.DiscrepancyFeeMismatch]
	if amountD.ChargeID == nil || *amountD.ChargeID != amountCharge.ID || amountD.LocalAmount.Amount() != 10000 || amountD.ProviderAmount.Amount() != 9000 {
		t.Fatalf("divergência de valor: %+v", amountD)
	}
	if feeD.ChargeID == nil || *feeD.ChargeID != feeCharge.ID || feeD.ProviderAmount.Amount() != 700 {
		t.Fatalf("divergência de taxa: %+v", feeD)
	}

	// Outro tenant não vê nem resolve as divergências.
	other := h.env.NewTenant(t, "outro-conciliacao")
	if _, err := h.pay.ResolveDiscrepancy(ctx, other.ID, other.Actor, &other.Owner.ID, payments.ResolveInput{ID: feeD.ID,
		Resolution: payments.ResolutionDismiss, Justification: "Tentativa de outro tenant.",
		ExpectedVersion: discrepancyVersion(t, h.pay, h.tn.ID, feeD.ID)}); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("resolução por outro tenant: %v", err)
	}
	if l, _, err := h.env.Store.ListDiscrepancies(ctx, other.ID, "", "", 100, ""); err != nil || len(l) != 0 {
		t.Fatalf("isolamento: %d %v", len(l), err)
	}

	// Valor pago menor no provedor: o ajuste reconhece a diferença como despesa.
	adjusted, err := h.pay.ResolveDiscrepancy(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, payments.ResolveInput{ID: amountD.ID,
		Resolution: payments.ResolutionAdjustLedger, Justification: "Diferença confirmada no extrato do provedor.",
		ExpectedVersion: discrepancyVersion(t, h.pay, h.tn.ID, amountD.ID)})
	if err != nil || adjusted.LedgerTransactionID == nil || adjusted.Status != payments.DiscrepancyResolved {
		t.Fatalf("ajuste de valor: %+v %v", adjusted, err)
	}
	if net := h.accountNet(t, "reconciliation_adjustments"); net != 1000 {
		t.Fatalf("ajuste de conciliação = %d", net)
	}
	// Taxa mantida como está no Recorvia, sem lançamento.
	kept, err := h.pay.ResolveDiscrepancy(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, payments.ResolveInput{ID: feeD.ID,
		Resolution: payments.ResolutionKeepLocal, Justification: "Taxa contratada difere da cobrada; contestada com o provedor.",
		ExpectedVersion: discrepancyVersion(t, h.pay, h.tn.ID, feeD.ID)})
	if err != nil || kept.LedgerTransactionID != nil || kept.Resolution != payments.ResolutionKeepLocal {
		t.Fatalf("manter local: %+v %v", kept, err)
	}
	h.assertLedgerBalanced(t)
	if n := h.sum(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.discrepancy_resolved'`); n != 2 {
		t.Fatalf("resoluções auditadas = %d", n)
	}

	// Mesmos valores: as divergências resolvidas não são reabertas.
	if run := h.run(t); run.DiscrepancyCount != 0 {
		t.Fatalf("divergências após as resoluções = %d", run.DiscrepancyCount)
	}
	// Nova taxa divergente gera nova divergência; quando o provedor volta ao
	// valor local, ela é resolvida automaticamente.
	newFee := int64(900)
	h.simApply(t, feeCharge.ProviderChargeID, paysim.Action{Name: "diverge", Fee: &newFee})
	if run := h.run(t); run.DiscrepancyCount != 1 {
		t.Fatalf("divergência com valores novos = %d", run.DiscrepancyCount)
	}
	reopened := h.openDiscrepancies(t)[payments.DiscrepancyFeeMismatch]
	if reopened.ID == feeD.ID || reopened.ProviderAmount.Amount() != 900 {
		t.Fatalf("nova divergência de taxa: %+v", reopened)
	}
	localFee := paysim.Fee(8000)
	h.simApply(t, feeCharge.ProviderChargeID, paysim.Action{Name: "diverge", Fee: &localFee})
	if run := h.run(t); run.DiscrepancyCount != 0 {
		t.Fatalf("divergências após o provedor corrigir a taxa = %d", run.DiscrepancyCount)
	}
	auto, err := h.pay.Discrepancy(ctx, h.tn.ID, reopened.ID)
	if err != nil || auto.Status != payments.DiscrepancyResolved || auto.Resolution != payments.ResolutionAutoResolved || auto.ResolvedBy != nil {
		t.Fatalf("resolução automática: %+v %v", auto, err)
	}
	h.assertLedgerBalanced(t)
	// Tenant sem provedor configurado não executa a conciliação.
	if _, err := h.pay.RunReconciliation(ctx, other.ID, other.Actor, nil, payments.RunInput{}); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("tenant sem provedor: %v", err)
	}
}

func (h *payHarness) simApply(t *testing.T, providerChargeID string, a paysim.Action) {
	t.Helper()
	if _, err := h.sim.Apply(providerChargeID, a); err != nil {
		t.Fatalf("ação %s no simulador: %v", a.Name, err)
	}
}

// As tabelas de recebíveis ficam isoladas pela RLS mesmo em consultas sem
// filtro de tenant: outro tenant não lê nem altera reembolsos, disputas,
// execuções ou divergências, e o filtro explícito do adaptador devolve vazio.
func TestReceivablesTablesIsolatedByRLS(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	_, refunded := h.paidCharge(t, 10000)
	if _, err := h.pay.RequestRefund(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, refundInput(refunded, 2500, "rls-1")); err != nil {
		t.Fatal(err)
	}
	_, disputed := h.paidCharge(t, 6000)
	h.simApply(t, disputed.ProviderChargeID, paysim.Action{Name: "chargeback", Stage: "requested"})
	h.deliver(t)
	fee := int64(450)
	h.simApply(t, refunded.ProviderChargeID, paysim.Action{Name: "diverge", Fee: &fee})
	if run := h.run(t); run.DiscrepancyCount == 0 {
		t.Fatalf("execução sem divergências: %+v", run)
	}

	tables := []string{"refunds", "disputes", "dispute_events", "reconciliation_runs", "reconciliation_discrepancies"}
	count := func(tenantID uuid.UUID, table string) int64 {
		t.Helper()
		var n int64
		err := h.env.DB.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	other := h.env.NewTenant(t, "outro-rls")
	for _, table := range tables {
		if n := count(h.tn.ID, table); n == 0 {
			t.Fatalf("%s sem registros no tenant de origem", table)
		}
		if n := count(other.ID, table); n != 0 {
			t.Errorf("%s: %d registro(s) visíveis a outro tenant", table, n)
		}
	}
	for _, stmt := range []string{
		`UPDATE refunds SET reason = 'alterado por outro tenant'`,
		`UPDATE disputes SET status = 'won'`,
		`UPDATE reconciliation_discrepancies SET justification = 'alterado por outro tenant'`,
		`UPDATE reconciliation_runs SET error = 'alterado por outro tenant'`,
	} {
		err := h.env.DB.InTenantTx(ctx, other.ID, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, stmt)
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("%s: %d linha(s) alteradas por outro tenant", stmt, tag.RowsAffected())
			}
			return err
		})
		if err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	refunds, _, err := h.env.Store.ListRefunds(ctx, other.ID, &refunded.ID, 10, "")
	if err != nil || len(refunds) != 0 {
		t.Fatalf("reembolsos de outro tenant: %d %v", len(refunds), err)
	}
	disputes, _, err := h.env.Store.ListDisputes(ctx, other.ID, nil, 10, "")
	if err != nil || len(disputes) != 0 {
		t.Fatalf("disputas de outro tenant: %d %v", len(disputes), err)
	}
	runs, _, err := h.env.Store.ListReconciliationRuns(ctx, other.ID, 10, "")
	if err != nil || len(runs) != 0 {
		t.Fatalf("execuções de outro tenant: %d %v", len(runs), err)
	}
	if _, err := h.pay.Refund(ctx, other.ID, firstRefundID(t, h, refunded.ID)); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("reembolso lido por outro tenant: %v", err)
	}
}

// firstRefundID devolve o ID do primeiro reembolso da cobrança no tenant do harness.
func firstRefundID(t *testing.T, h *payHarness, chargeID uuid.UUID) uuid.UUID {
	t.Helper()
	list, _, err := h.env.Store.ListRefunds(context.Background(), h.tn.ID, &chargeID, 1, "")
	if err != nil || len(list) == 0 {
		t.Fatalf("reembolsos da cobrança: %d %v", len(list), err)
	}
	return list[0].ID
}
