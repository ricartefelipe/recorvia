//go:build integration

package pgstore_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/db"
	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/payments"
)

// recoveryDispute prepara, com o simulador, a disputa perdida de cobrança
// paga com 25,00 de excedente, dos quais o cliente já havia usado 20,00 em
// outra fatura: 5,00 de crédito consumido e 20,00 a recuperar (ADR-070).
func (h *payHarness) recoveryDispute(t *testing.T) payments.Dispute {
	t.Helper()
	inv, c := h.overpaidCharge(t)
	if other := h.issueInvoiceFor(t, inv.CustomerID, 2000); other.CreditApplied.Amount() != 2000 {
		t.Fatalf("crédito aplicado em outra fatura: %v", other.CreditApplied)
	}
	h.chargeback(t, c, "requested", "lost")
	d := h.dispute(t, c.ID)
	if d.Status != payments.DisputeLost || d.RecoveryAmount.Amount() != 2000 || !d.RecoveryReceived.IsZero() || !d.RecoveryWrittenOff.IsZero() {
		t.Fatalf("pré-condição: disputa perdida com valor a recuperar: %+v", d)
	}
	return d
}

func settlementInput(d payments.Dispute, outcome payments.RecoveryOutcome, amount int64, key string) payments.RecoverySettlementInput {
	return payments.RecoverySettlementInput{DisputeID: d.ID, Outcome: outcome, Amount: money.New(amount, money.BRL),
		Justification: "Cliente pagou por transferência identificada no extrato.", RequestKey: key}
}

func appCode(err error) string {
	if ae, ok := apperr.As(err); ok {
		return ae.Code
	}
	return ""
}

// A baixa recebida leva o valor ao disponível liquidado e a perdida às
// perdas com inadimplência; as duas tiram o valor de customer_recoverables,
// com o ledger balanceado, a baixa imutável, o evento e a auditoria na mesma
// transação. A repetição da chave devolve a mesma baixa.
func TestRecoverySettlementWithSimulator(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	d := h.recoveryDispute(t)
	before := ledgerNetByCode(t, h)

	received, after, err := h.pay.SettleRecovery(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, settlementInput(d, payments.RecoveryReceived, 1200, "baixa-1"))
	if err != nil || received.SettledBy == nil || *received.SettledBy != h.tn.Owner.ID {
		t.Fatalf("baixa recebida: %+v %v", received, err)
	}
	if left, _ := after.RecoveryOutstanding(); after.RecoveryReceived.Amount() != 1200 || left.Amount() != 800 {
		t.Fatalf("disputa depois da baixa recebida: %+v", after)
	}
	// Repetição da mesma chave: mesma baixa, nenhum efeito novo.
	entries := h.sum(t, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`)
	again, _, err := h.pay.SettleRecovery(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, settlementInput(d, payments.RecoveryReceived, 1200, "baixa-1"))
	if err != nil || again.ID != received.ID {
		t.Fatalf("repetição: %+v %v", again, err)
	}
	if n := h.sum(t, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`); n != entries {
		t.Fatalf("a repetição lançou de novo: %d partidas, antes %d", n, entries)
	}
	if _, _, err := h.pay.SettleRecovery(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID,
		settlementInput(d, payments.RecoveryWrittenOff, 1200, "baixa-1")); appCode(err) != "request_key_reused" {
		t.Fatalf("chave reutilizada: %v", err)
	}
	if _, _, err := h.pay.SettleRecovery(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID,
		settlementInput(d, payments.RecoveryWrittenOff, 801, "baixa-2")); appCode(err) != "recovery_exceeds_outstanding" {
		t.Fatalf("baixa acima do saldo: %v", err)
	}
	// Perda do restante por credencial de API: autoria nula na tabela e ator
	// api_credential na auditoria.
	cred := audit.APICredential(uuid.New(), "ERP interno")
	lost, after, err := h.pay.SettleRecovery(ctx, h.tn.ID, cred, nil, settlementInput(d, payments.RecoveryWrittenOff, 800, "baixa-3"))
	if err != nil || lost.SettledBy != nil {
		t.Fatalf("baixa perdida: %+v %v", lost, err)
	}
	if left, _ := after.RecoveryOutstanding(); after.RecoveryWrittenOff.Amount() != 800 || !left.IsZero() {
		t.Fatalf("disputa depois da perda: %+v", after)
	}
	if _, _, err := h.pay.SettleRecovery(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID,
		settlementInput(d, payments.RecoveryReceived, 1, "baixa-4")); appCode(err) != "recovery_already_settled" {
		t.Fatalf("baixa sem saldo: %v", err)
	}

	h.assertLedgerBalanced(t)
	net := ledgerNetByCode(t, h)
	if net["customer_recoverables"] != 0 || net["cash"]-before["cash"] != 1200 || net["bad_debt"]-before["bad_debt"] != 800 ||
		net["chargebacks"] != before["chargebacks"] {
		t.Fatalf("ledger: antes %v depois %v", before, net)
	}
	for kind, n := range map[string]int64{"dispute.recovery_received": 1, "dispute.recovery_written_off": 1} {
		if got := h.sum(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND reference_type = 'dispute_recovery_settlement'
		                     AND kind = '`+kind+`'`); got != n {
			t.Fatalf("lançamentos %s = %d", kind, got)
		}
	}
	if n := h.sum(t, `SELECT count(*) FROM dispute_recovery_settlements s JOIN ledger_transactions l
	                    ON l.tenant_id = s.tenant_id AND l.id = s.ledger_transaction_id AND l.reference_id = s.id
	                   WHERE s.tenant_id = $1`); n != 2 {
		t.Fatalf("baixas vinculadas ao próprio lançamento = %d", n)
	}
	if n := h.sum(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'dispute.recovery_settled'
	                    AND payload->>'dispute_id' = '`+d.ID.String()+`'`); n != 2 {
		t.Fatalf("eventos da baixa = %d", n)
	}
	if n := h.sum(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'dispute.recovery_settled' AND actor_type = 'api_credential'
	                    AND (details->>'recovery_outstanding_amount')::bigint = 0 AND details->>'outcome' = 'written_off'`); n != 1 {
		t.Fatalf("auditoria da perda por credencial = %d", n)
	}
	if n := h.sum(t, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'dispute.recovery_settled' AND actor_type = 'user'
	                    AND details->>'justification' LIKE 'Cliente pagou%'`); n != 1 {
		t.Fatalf("auditoria da baixa recebida = %d", n)
	}
	// Painel: o saldo a recuperar zera e o chargeback do mês não muda.
	if dash := h.dashboard(t, "2026-03"); dash.OpenRecoverables != 0 || dash.Chargebacks != 10000 {
		t.Fatalf("painel: a recuperar=%d chargebacks=%d", dash.OpenRecoverables, dash.Chargebacks)
	}
	got, list, err := h.pay.RecoverySettlements(ctx, h.tn.ID, d.ID)
	if err != nil || len(list) != 2 || list[0].ID != received.ID || list[1].ID != lost.ID || got.RecoveryReceived.Amount() != 1200 ||
		list[0].EffectiveDate != received.EffectiveDate || list[1].Justification != lost.Justification {
		t.Fatalf("baixas listadas: %+v %v", list, err)
	}
	listed, err := h.env.Store.ListDisputes(ctx, h.tn.ID, &d.ChargeID, 10)
	if err != nil || len(listed) != 1 || listed[0].RecoveryWrittenOff.Amount() != 800 {
		t.Fatalf("disputa listada: %+v %v", listed, err)
	}
}

// O painel mostra o saldo a recuperar antes das baixas.
func TestDashboardShowsOpenRecoverables(t *testing.T) {
	h := newPayHarness(t, 2*time.Second)
	d := h.recoveryDispute(t)
	if dash := h.dashboard(t, ""); dash.OpenRecoverables != 2000 {
		t.Fatalf("a recuperar = %d", dash.OpenRecoverables)
	}
	if _, _, err := h.pay.SettleRecovery(context.Background(), h.tn.ID, h.tn.Actor, &h.tn.Owner.ID,
		settlementInput(d, payments.RecoveryReceived, 500, "")); err != nil {
		t.Fatal(err)
	}
	if dash := h.dashboard(t, ""); dash.OpenRecoverables != 1500 {
		t.Fatalf("a recuperar depois da baixa = %d", dash.OpenRecoverables)
	}
}

// Baixas concorrentes da mesma disputa nunca somam mais que o valor a
// recuperar: pelo caso de uso (bloqueio da disputa) e, mesmo sem a
// verificação da aplicação, pela constraint avaliada sob o bloqueio da linha
// que o gatilho atualiza.
func TestRecoverySettlementConcurrency(t *testing.T) {
	ctx := context.Background()
	t.Run("pelo caso de uso", func(t *testing.T) {
		h := newPayHarness(t, 2*time.Second)
		d := h.recoveryDispute(t)
		const n = 8
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Go(func() {
				<-start
				outcome := payments.RecoveryReceived
				if i%2 == 1 {
					outcome = payments.RecoveryWrittenOff
				}
				_, _, errs[i] = h.pay.SettleRecovery(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID,
					settlementInput(d, outcome, 600, "concorrente-"+string(rune('a'+i))))
			})
		}
		close(start)
		wg.Wait()
		accepted := 0
		for _, err := range errs {
			switch {
			case err == nil:
				accepted++
			case appCode(err) != "recovery_exceeds_outstanding":
				t.Fatalf("erro inesperado: %v", err)
			}
		}
		got := h.dispute(t, d.ChargeID)
		settled, _ := got.RecoverySettled()
		if accepted != 3 || settled.Amount() != 1800 {
			t.Fatalf("baixas aceitas = %d, total %v", accepted, settled)
		}
		if sum := h.sum(t, `SELECT COALESCE(sum(amount), 0)::bigint FROM dispute_recovery_settlements WHERE tenant_id = $1`); sum != 1800 {
			t.Fatalf("soma das baixas gravadas = %d", sum)
		}
		h.assertLedgerBalanced(t)
		if net := ledgerNetByCode(t, h); net["customer_recoverables"] != 200 {
			t.Fatalf("customer_recoverables = %d", net["customer_recoverables"])
		}
	})
	t.Run("pela constraint do banco", func(t *testing.T) {
		h := newPayHarness(t, 2*time.Second)
		d := h.recoveryDispute(t)
		// Lançamentos existentes servem de vínculo: a gravação direta
		// dispensa o caso de uso e a verificação do saldo na aplicação.
		var ledgerIDs []uuid.UUID
		err := h.env.DB.InTenantTx(ctx, h.tn.ID, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id FROM ledger_transactions WHERE tenant_id = $1 ORDER BY created_at, id LIMIT 2`, h.tn.ID)
			if err != nil {
				return err
			}
			ledgerIDs, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			return err
		})
		if err != nil || len(ledgerIDs) != 2 {
			t.Fatalf("lançamentos: %v %v", ledgerIDs, err)
		}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range 2 {
			wg.Go(func() {
				<-start
				errs[i] = h.env.DB.InTenantTx(ctx, h.tn.ID, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `INSERT INTO dispute_recovery_settlements (id, tenant_id, dispute_id, outcome, amount, effective_date,
					     justification, ledger_transaction_id, created_at)
					     VALUES ($1, $2, $3, 'received', 1500, '2026-03-02', 'Gravação direta concorrente.', $4, now())`,
						uuid.New(), h.tn.ID, d.ID, ledgerIDs[i])
					return err
				})
			})
		}
		close(start)
		wg.Wait()
		ok, rejected := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case postgres.IsNamedCheckViolation(err, "disputes_recovery_settled_within"):
				rejected++
			default:
				t.Fatalf("erro inesperado: %v", err)
			}
		}
		if ok != 1 || rejected != 1 {
			t.Fatalf("gravações aceitas=%d recusadas=%d", ok, rejected)
		}
		if got := h.dispute(t, d.ChargeID); got.RecoveryReceived.Amount() != 1500 {
			t.Fatalf("total recebido = %v", got.RecoveryReceived)
		}
	})
}

// O banco protege o registro: baixas imutáveis, totais da disputa alterados
// só pelo gatilho, desfecho e justificativa validados e a disputa nascida sem
// baixas; o papel da aplicação não altera nem exclui baixas.
func TestRecoverySettlementDatabaseGuards(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	d := h.recoveryDispute(t)
	st, _, err := h.pay.SettleRecovery(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, settlementInput(d, payments.RecoveryReceived, 300, ""))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(stmt string, args ...any) error {
		return h.env.DB.InTenantTx(ctx, h.tn.ID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, args...)
			return err
		})
	}
	for name, stmt := range map[string]string{
		"alteração direta do total recebido": `UPDATE disputes SET recovery_received_amount = 0 WHERE id = $1`,
		"alteração direta do total perdido":  `UPDATE disputes SET recovery_written_off_amount = 100 WHERE id = $1`,
	} {
		if err := exec(stmt, d.ID); !postgres.IsRaisedException(err) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Alterações da disputa que não tocam nos totais continuam permitidas.
	if err := exec(`UPDATE disputes SET provider_status = provider_status WHERE id = $1`, d.ID); err != nil {
		t.Fatalf("alteração sem os totais: %v", err)
	}
	for _, stmt := range []string{`UPDATE dispute_recovery_settlements SET amount = 1 WHERE id = $1`,
		`DELETE FROM dispute_recovery_settlements WHERE id = $1`} {
		if err := exec(stmt, st.ID); !postgres.IsInsufficientPrivilege(err) {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	owner := envOwner(t)
	for _, stmt := range []string{`UPDATE dispute_recovery_settlements SET amount = 1 WHERE id = $1`,
		`DELETE FROM dispute_recovery_settlements WHERE id = $1`} {
		if _, err := owner.Exec(ctx, stmt, st.ID); err == nil || !strings.Contains(err.Error(), "imutável") {
			t.Fatalf("dono: %s: %v", stmt, err)
		}
	}
	for name, stmt := range map[string]string{
		"dispute_recovery_settlements_outcome_check":       `INSERT INTO dispute_recovery_settlements (id, tenant_id, dispute_id, outcome, amount, effective_date, justification, ledger_transaction_id, created_at) VALUES ($1, $2, $3, 'lost', 1, current_date, 'Desfecho desconhecido.', $4, now())`,
		"dispute_recovery_settlements_justification_check": `INSERT INTO dispute_recovery_settlements (id, tenant_id, dispute_id, outcome, amount, effective_date, justification, ledger_transaction_id, created_at) VALUES ($1, $2, $3, 'received', 1, current_date, 'curta', $4, now())`,
		"dispute_recovery_settlements_amount_check":        `INSERT INTO dispute_recovery_settlements (id, tenant_id, dispute_id, outcome, amount, effective_date, justification, ledger_transaction_id, created_at) VALUES ($1, $2, $3, 'received', 0, current_date, 'Valor zerado inválido.', $4, now())`,
	} {
		if err := exec(stmt, uuid.New(), h.tn.ID, d.ID, st.LedgerTransactionID); !postgres.IsNamedCheckViolation(err, name) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := owner.Exec(ctx, `INSERT INTO disputes (id, tenant_id, charge_id, provider, status, amount, opened_at, created_at,
	        recovery_received_amount) SELECT $1, tenant_id, charge_id, provider, 'won', amount, opened_at, created_at, 1 FROM disputes WHERE id = $2`,
		uuid.New(), d.ID); err == nil || !strings.Contains(err.Error(), "disputa criada com baixa") {
		t.Fatalf("disputa nascida com baixa: %v", err)
	}
}

// Baixas de um tenant não aparecem para outro: a disputa de outro tenant não
// é encontrada pelo caso de uso, a leitura direta não vê as baixas (RLS) e a
// gravação direta não se vincula à disputa de outro tenant.
func TestRecoverySettlementTenantIsolation(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	d := h.recoveryDispute(t)
	if _, _, err := h.pay.SettleRecovery(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, settlementInput(d, payments.RecoveryReceived, 400, "")); err != nil {
		t.Fatal(err)
	}
	other := h.env.NewTenant(t, "outro-recuperar")
	if _, _, err := h.pay.SettleRecovery(ctx, other.ID, other.Actor, &other.Owner.ID,
		settlementInput(d, payments.RecoveryReceived, 100, "")); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("baixa em disputa de outro tenant: %v", err)
	}
	if _, _, err := h.pay.RecoverySettlements(ctx, other.ID, d.ID); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("baixas de outro tenant: %v", err)
	}
	if _, err := h.pay.Dispute(ctx, other.ID, d.ID); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("disputa de outro tenant: %v", err)
	}
	var visible int
	var insertErr error
	err := h.env.DB.InTenantTx(ctx, other.ID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dispute_recovery_settlements`).Scan(&visible); err != nil {
			return err
		}
		_, insertErr = tx.Exec(ctx, `SAVEPOINT s1`)
		if insertErr != nil {
			return insertErr
		}
		_, insertErr = tx.Exec(ctx, `INSERT INTO dispute_recovery_settlements (id, tenant_id, dispute_id, outcome, amount, effective_date,
		     justification, ledger_transaction_id, created_at) VALUES ($1, $2, $3, 'received', 1, current_date, 'Vínculo com outro tenant.', $4, now())`,
			uuid.New(), other.ID, d.ID, uuid.New())
		_, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT s1`)
		return err
	})
	if err != nil || visible != 0 {
		t.Fatalf("baixas visíveis em outro tenant: %d %v", visible, err)
	}
	if !postgres.IsForeignKeyViolation(insertErr) {
		t.Fatalf("baixa vinculada à disputa de outro tenant: %v", insertErr)
	}
}

// A seção Down da migration remove a tabela de baixas, os gatilhos, as
// funções e os totais da disputa; os lançamentos das baixas permanecem no
// ledger imutável, e a Up pode ser reaplicada em seguida (executado numa
// transação desfeita ao fim, como dono das tabelas).
func TestRecoverySettlementMigrationDownWithData(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	d := h.recoveryDispute(t)
	if _, _, err := h.pay.SettleRecovery(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, settlementInput(d, payments.RecoveryWrittenOff, 2000, "")); err != nil {
		t.Fatal(err)
	}
	raw, err := db.Migrations.ReadFile("migrations/00018_baixa_de_valor_a_recuperar.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("migration sem seção Down")
	}
	tx, err := envOwner(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatalf("Down: %v", err)
	}
	var objects int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'disputes'
	          AND column_name IN ('recovery_received_amount', 'recovery_written_off_amount'))
	      + (SELECT count(*) FROM pg_proc WHERE proname IN ('app_dispute_recovery_settled', 'app_disputes_recovery_guard'))
	      + (SELECT count(*) FROM pg_constraint WHERE conname = 'disputes_recovery_settled_within')
	      + (CASE WHEN to_regclass('public.dispute_recovery_settlements') IS NULL THEN 0 ELSE 1 END)`).Scan(&objects); err != nil || objects != 0 {
		t.Fatalf("objetos depois do Down: %d %v", objects, err)
	}
	var kept int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND kind = 'dispute.recovery_written_off'`,
		h.tn.ID).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("lançamento da baixa depois do Down: %d %v", kept, err)
	}
	// Sem o gatilho de proteção, a disputa volta a ser alterada como antes.
	if _, err := tx.Exec(ctx, `UPDATE disputes SET provider_status = provider_status WHERE id = $1`, d.ID); err != nil {
		t.Fatalf("disputa depois do Down: %v", err)
	}
	if _, err := tx.Exec(ctx, strings.TrimPrefix(up, "-- +goose Up")); err != nil {
		t.Fatalf("Up após Down: %v", err)
	}
	var totals int64
	if err := tx.QueryRow(ctx, `SELECT recovery_received_amount + recovery_written_off_amount FROM disputes WHERE id = $1`, d.ID).Scan(&totals); err != nil ||
		totals != 0 {
		t.Fatalf("totais recriados com zero: %d %v", totals, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	got := h.dispute(t, d.ChargeID)
	if got.RecoveryWrittenOff.Amount() != 2000 {
		t.Fatalf("disputa depois da transação desfeita: %+v", got)
	}
	if _, list, err := h.pay.RecoverySettlements(ctx, h.tn.ID, d.ID); err != nil || len(list) != 1 {
		t.Fatalf("baixas depois da transação desfeita: %v %v", list, err)
	}
}
