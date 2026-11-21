//go:build integration

package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/reporting"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// Testes dos indicadores do painel (RF-17, ADR-074). As fórmulas estão em
// docs/arquitetura/indicadores.md.

func (a *advEnv) dashboard(t *testing.T, tn fixture.Tenant, month string) reporting.Dashboard {
	t.Helper()
	d, err := reporting.NewService(a.Store.Reporting(), a.Clock).Dashboard(context.Background(), tn.ID, month)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (a *advEnv) cancelNow(t *testing.T, tn fixture.Tenant, id uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	sub, err := a.Subscriptions.Get(ctx, tn.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Subscriptions.Cancel(ctx, tn.ID, tn.Actor, id, subscriptions.CancelImmediately, "Encerramento do contrato", sub.Version); err != nil {
		t.Fatal(err)
	}
}

func (a *advEnv) coupon(t *testing.T, tn fixture.Tenant, in catalog.CouponInput) {
	t.Helper()
	if _, err := a.Coupons.Create(context.Background(), tn.ID, tn.Actor, in); err != nil {
		t.Fatal(err)
	}
}

func (a *advEnv) applyCoupon(t *testing.T, tn fixture.Tenant, subID uuid.UUID, code string) {
	t.Helper()
	if _, err := a.Terms.ApplyCoupon(context.Background(), tn.ID, tn.Actor, &tn.Owner.ID, subID, code); err != nil {
		t.Fatal(err)
	}
}

// O MRR usa o componente fixo reajustado (o mesmo valor da fatura) e é
// líquido dos descontos recorrentes: cupom permanente e por ciclos com
// ciclos restantes entram; o de uso único não.
func TestDashboardMRRUsesAdjustedPriceAndRecurringDiscounts(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "painel-mrr")
	flat := a.newFlatPlan(t, tn, 10000)
	adjusted := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, flat.ID, nil)
	seats := a.newSubscriptionQty(t, tn, a.newPerUnitPlan(t, tn, "1000").ID, 3)
	forever := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, flat.ID, nil)
	repeating := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, flat.ID, nil)
	once := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, flat.ID, nil)

	pct15, amount30, cycles, pct50 := 1500, int64(3000), 3, 5000
	a.coupon(t, tn, catalog.CouponInput{Code: "SEMPRE15", Name: "Quinze por cento sempre", Type: catalog.DiscountPercent,
		PercentOffBP: &pct15, Duration: catalog.DurationForever})
	a.coupon(t, tn, catalog.CouponInput{Code: "TRES30", Name: "Trinta reais por três ciclos", Type: catalog.DiscountAmount,
		AmountOff: &amount30, Duration: catalog.DurationRepeating, DurationCycles: &cycles})
	a.coupon(t, tn, catalog.CouponInput{Code: "UNICO50", Name: "Metade na próxima fatura", Type: catalog.DiscountPercent,
		PercentOffBP: &pct50, Duration: catalog.DurationOnce})
	a.applyCoupon(t, tn, forever.ID, "SEMPRE15")
	a.applyCoupon(t, tn, repeating.ID, "TRES30")
	a.runEngine(t, tn, 5)

	newAmount, pct10 := int64(15000), 1000
	effective := calendar.Date{Year: 2026, Month: time.April, Day: 10}
	if _, err := a.Terms.ScheduleAdjustment(ctx, tn.ID, tn.Actor, &tn.Owner.ID, adjusted.ID, subscriptions.AdjustmentInput{
		EffectiveDate: effective, Kind: subscriptions.AdjustFixedAmount, NewAmount: &newAmount, Reason: "Renegociação comercial"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Terms.ScheduleAdjustment(ctx, tn.ID, tn.Actor, &tn.Owner.ID, seats.ID, subscriptions.AdjustmentInput{
		EffectiveDate: effective, Kind: subscriptions.AdjustPercent, PercentBP: &pct10, IndexName: "IPCA", Reason: "Reajuste anual"}); err != nil {
		t.Fatal(err)
	}
	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	a.runEngine(t, tn, 5)
	// O cupom de uso único ainda não consumido não é receita recorrente.
	a.applyCoupon(t, tn, once.ID, "UNICO50")

	april := spTime(t, 2026, time.April, 10, 0)
	for _, c := range []struct {
		name  string
		subID uuid.UUID
		total int64
	}{
		{"reajuste por valor", adjusted.ID, 15000},
		{"reajuste percentual por unidade", seats.ID, 3300},
		{"cupom permanente", forever.ID, 8500},
		{"cupom por ciclos", repeating.ID, 7000},
	} {
		if inv := findInvoice(t, a.invoicesOf(t, tn.ID, c.subID), "subscription_cycle", april); inv.Total != c.total {
			t.Errorf("%s: fatura de abril = %d, esperado %d", c.name, inv.Total, c.total)
		}
	}

	d := a.dashboard(t, tn, "")
	// 15000 + 3 × 1100 + 10000 × 0,85 + (10000 − 3000) + 10000.
	if d.MRR != 43800 || d.ARR != 525600 {
		t.Fatalf("MRR = %d, ARR = %d; esperado 43800 e 525600", d.MRR, d.ARR)
	}
	// As mensalidades emitidas em abril somam o mesmo valor: o MRR segue a fatura.
	if d.BilledRecurring != 43800 {
		t.Errorf("faturado recorrente de abril = %d, esperado 43800", d.BilledRecurring)
	}

	// Encerrado o desconto por ciclos (terceiro ciclo faturado em maio) e
	// consumido o de uso único, o MRR volta ao valor cheio dessas assinaturas.
	a.Clock.Set(spTime(t, 2026, time.May, 10, 9))
	a.runEngine(t, tn, 5)
	if d := a.dashboard(t, tn, ""); d.MRR != 15000+3300+8500+10000+10000 {
		t.Fatalf("MRR depois do fim dos descontos = %d", d.MRR)
	}
}

// Churn: o denominador são as assinaturas vigentes fora de trial no início do
// mês pela situação registrada no histórico; o numerador, as dessas
// encerradas no mês. Cancelada no trial e criada no próprio mês só contam em
// "canceladas". As posições de um mês encerrado usam o último instante dele.
func TestDashboardChurnAndSubscriptionPositionsByMonth(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "painel-churn")
	plan := a.newFlatPlan(t, tn, 10000)
	stays := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
	churned := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
	trialDays := 60
	trial, err := a.Subscriptions.Create(ctx, tn.ID, tn.Actor, subscriptions.CreateInput{CustomerID: a.NewCustomer(t, tn).ID,
		PlanID: plan.ID, Quantity: 1, TrialDays: &trialDays})
	if err != nil {
		t.Fatal(err)
	}
	a.runEngine(t, tn, 2)

	a.Clock.Set(spTime(t, 2026, time.April, 5, 10))
	a.cancelNow(t, tn, churned.ID)
	a.cancelNow(t, tn, trial.ID)
	sameMonth := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
	a.Clock.Set(spTime(t, 2026, time.April, 6, 10))
	a.cancelNow(t, tn, sameMonth.ID)

	check := func(label string, d reporting.Dashboard, churnBP int64, active, created, canceled int) {
		t.Helper()
		if d.ChurnRateBP != churnBP || d.ActiveSubscriptions != active || d.NewSubscriptions != created || d.CanceledSubscriptions != canceled {
			t.Errorf("%s: churn=%d ativas=%d novas=%d canceladas=%d; esperado %d %d %d %d", label, d.ChurnRateBP, d.ActiveSubscriptions,
				d.NewSubscriptions, d.CanceledSubscriptions, churnBP, active, created, canceled)
		}
	}
	// Abril: vigentes no início = {stays, churned}; encerrada delas = churned.
	check("abril em curso", a.dashboard(t, tn, "2026-04"), 5000, 1, 1, 3)
	// O link do card abre a lista com o mesmo filtro da contagem.
	if pg, err := a.Subscriptions.List(ctx, tn.ID, subscriptions.ListFilter{InForce: true, Limit: 100}); err != nil || len(pg.Items) != 1 ||
		pg.Items[0].ID != stays.ID {
		t.Errorf("lista das assinaturas em vigor: %d %v", len(pg.Items), err)
	}
	// Março (tenant criado em março): nenhuma vigente no início; no fim do
	// mês, as três assinaturas criadas estavam em trial ou ativas.
	march := a.dashboard(t, tn, "2026-03")
	check("março encerrado", march, 0, 3, 3, 0)
	if want := time.Date(2026, time.April, 1, 2, 59, 59, 999999000, time.UTC); !march.PositionAt.Equal(want) {
		t.Errorf("posição de março = %v, esperado %v", march.PositionAt, want)
	}

	// Depois do fim de abril, a posição de abril não muda com o tempo.
	a.Clock.Set(spTime(t, 2026, time.May, 12, 9))
	a.runEngine(t, tn, 2) // abril e maio da assinatura que permanece
	check("abril encerrado", a.dashboard(t, tn, "2026-04"), 5000, 1, 1, 3)
	if got, err := a.Subscriptions.Get(ctx, tn.ID, stays.ID); err != nil || got.Status != subscriptions.Active {
		t.Fatalf("assinatura que permanece: %v %v", got.Status, err)
	}

	// Mês anterior à organização: posições e fluxos zerados; o MRR é a
	// posição atual e vem marcado como tal.
	before := a.dashboard(t, tn, "2025-01")
	check("antes da organização", before, 0, 0, 0, 0)
	if before.OpenReceivables != 0 || before.Billed != 0 || before.Bases["active_subscriptions"] != reporting.BasisMonthEnd {
		t.Errorf("mês anterior à organização: %+v", before)
	}
	if before.MRR != 10000 || before.Bases["mrr"] != reporting.BasisCurrent || before.Bases["arr"] != reporting.BasisCurrent {
		t.Errorf("MRR da posição atual: %d %s", before.MRR, before.Bases["mrr"])
	}
}

// Assinatura retroativa com trial: o histórico só começa na criação. Antes
// dele, a situação vem do contrato (em trial até trial_ends_at, ativa depois):
// criada em 10/03 com início em 01/12 e 14 dias de trial, ela já estava ativa
// no início de janeiro, fevereiro e março.
func TestDashboardRetroactiveTrialUsesContractDates(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "painel-retroativa")
	plan := a.newFlatPlan(t, tn, 10000)
	start := calendar.Date{Year: 2025, Month: time.December, Day: 1}
	trialDays, zero := 14, 0
	create := func(trial *int) subscriptions.Subscription {
		t.Helper()
		s, err := a.Subscriptions.Create(ctx, tn.ID, tn.Actor, subscriptions.CreateInput{CustomerID: a.NewCustomer(t, tn).ID, PlanID: plan.ID,
			Quantity: 1, StartDate: &start, TrialDays: trial})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	withTrial := create(&trialDays)
	create(&zero)
	if withTrial.Status != subscriptions.Trialing || withTrial.TrialEndsAt == nil || !withTrial.TrialEndsAt.Before(spTime(t, 2025, time.December, 16, 0)) {
		t.Fatalf("assinatura retroativa: %s %v", withTrial.Status, withTrial.TrialEndsAt)
	}
	a.cancelNow(t, tn, withTrial.ID)

	for _, c := range []struct {
		month                  string
		churnBP                int64
		active, created, ended int
	}{
		{"2025-12", 0, 2, 2, 0}, // no fim de dezembro o trial (até 15/12) já tinha terminado
		{"2026-01", 0, 2, 0, 0},
		{"2026-02", 0, 2, 0, 0},
		{"2026-03", 5000, 1, 0, 1}, // vigentes no início: as duas; encerrada: a do trial
	} {
		d := a.dashboard(t, tn, c.month)
		if d.ChurnRateBP != c.churnBP || d.ActiveSubscriptions != c.active || d.NewSubscriptions != c.created || d.CanceledSubscriptions != c.ended {
			t.Errorf("%s: churn=%d ativas=%d novas=%d canceladas=%d; esperado %d %d %d %d", c.month, d.ChurnRateBP, d.ActiveSubscriptions,
				d.NewSubscriptions, d.CanceledSubscriptions, c.churnBP, c.active, c.created, c.ended)
		}
	}
	// Antes do início do contrato, as assinaturas não existem.
	if d := a.dashboard(t, tn, "2025-11"); d.ActiveSubscriptions != 0 || d.NewSubscriptions != 0 {
		t.Errorf("novembro, antes do início: %+v", d)
	}
}

// Recebíveis e inadimplência na posição do fim do mês, pelo ledger: o saldo
// por fatura no instante atual é o amount_due das faturas em aberto, e um
// mês encerrado mantém o saldo que tinha, mesmo com pagamento e cancelamento
// registrados depois.
func TestDashboardReceivablesAtMonthEnd(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	partial := h.issueInvoice(t, 10000)
	credited := h.issueInvoice(t, 8000)
	voided := h.issueInvoice(t, 5000)
	writtenOff := h.issueInvoice(t, 4000)
	paid := h.issueInvoice(t, 3000)
	lateVoid := h.issueInvoice(t, 2000)
	for _, inv := range []billing.Invoice{partial, credited, writtenOff, paid, lateVoid} {
		if inv.DueAt == nil || inv.DueAt.After(spTime(t, 2026, time.March, 31, 0)) {
			t.Fatalf("fatura %s sem vencimento em março: %v", inv.Number, inv.DueAt)
		}
	}
	pay := func(id uuid.UUID, amount int64, key string) {
		t.Helper()
		if _, err := h.pay.RecordManualPayment(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, payments.ManualPaymentInput{InvoiceID: id,
			Amount: money.New(amount, money.BRL), Method: "pix", RequestKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	version := func(id uuid.UUID) int {
		t.Helper()
		d, err := h.env.Billing.Get(ctx, h.tn.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		return d.Version
	}
	h.env.Clock.Advance(time.Hour)
	pay(partial.ID, 3000, "parcial-1")
	pay(paid.ID, 3000, "integral")
	notes := billing.NewCreditNoteService(h.env.Store.Billing(), h.env.Clock)
	if _, err := notes.Issue(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, credited.ID, money.New(2000, money.BRL), "Desconto comercial",
		"nota-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.env.Billing.Void(ctx, h.tn.ID, h.tn.Actor, voided.ID, "Emitida por engano", version(voided.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.env.Billing.MarkUncollectible(ctx, h.tn.ID, h.tn.Actor, writtenOff.ID, "Cliente encerrou as atividades", version(writtenOff.ID)); err != nil {
		t.Fatal(err)
	}

	openDue := func() int64 {
		return h.sum(t, `SELECT COALESCE(sum(amount_due), 0)::bigint FROM invoices WHERE tenant_id = $1 AND status IN ('open', 'partially_paid')`)
	}
	issuedAt := h.env.Clock.Now().Add(-time.Hour)
	checkReceivableForms(t, h.env.DB, h.tn.ID, issuedAt, false)
	checkReceivableForms(t, h.env.DB, h.tn.ID, h.env.Clock.Now(), true)
	// Março em curso: 7000 (parcial) + 6000 (nota de crédito) + 2000.
	if d := h.dashboard(t, "2026-03"); d.OpenReceivables != 15000 || d.OpenReceivables != openDue() || d.OverdueReceivables != 0 {
		t.Fatalf("março em curso: aberto=%d (amount_due %d) vencido=%d", d.OpenReceivables, openDue(), d.OverdueReceivables)
	}

	// Em abril: quitação da parcial e cancelamento de uma fatura de março.
	h.env.Clock.Set(spTime(t, 2026, time.April, 20, 10))
	pay(partial.ID, 7000, "parcial-2")
	if _, err := h.env.Billing.Void(ctx, h.tn.ID, h.tn.Actor, lateVoid.ID, "Cancelada na renegociação", version(lateVoid.ID)); err != nil {
		t.Fatal(err)
	}

	// As formas de cálculo coincidem antes, durante e depois de cada evento.
	for _, at := range []time.Time{issuedAt.Add(-time.Second), issuedAt, issuedAt.Add(time.Hour), spTime(t, 2026, time.March, 15, 12),
		time.Date(2026, time.April, 1, 2, 59, 59, 999999000, time.UTC), h.env.Clock.Now().Add(-time.Second)} {
		checkReceivableForms(t, h.env.DB, h.tn.ID, at, false)
	}
	checkReceivableForms(t, h.env.DB, h.tn.ID, h.env.Clock.Now(), true)

	cases := []struct {
		month          string
		open, overdue  int64
		delinquencyBP  int64
		matchesBalance bool
	}{
		// Fim de março: 7000 + 6000 + 2000, todas vencidas; base = 10000 +
		// 8000 + 4000 + 3000 + 2000 (a cancelada em abril ainda valia).
		{"2026-03", 15000, 15000, 5556, false},
		// Abril em curso: só a da nota de crédito; base sem as canceladas.
		{"2026-04", 6000, 6000, 2400, true},
	}
	for _, c := range cases {
		d := h.dashboard(t, c.month)
		if d.OpenReceivables != c.open || d.OverdueReceivables != c.overdue || d.DelinquencyRateBP != c.delinquencyBP {
			t.Errorf("%s: aberto=%d vencido=%d inadimplência=%d; esperado %d %d %d", c.month, d.OpenReceivables, d.OverdueReceivables,
				d.DelinquencyRateBP, c.open, c.overdue, c.delinquencyBP)
		}
		if c.matchesBalance && d.OpenReceivables != openDue() {
			t.Errorf("%s: ledger %d diverge do saldo das faturas %d", c.month, d.OpenReceivables, openDue())
		}
		if d.Received != map[string]int64{"2026-03": 6000, "2026-04": 7000}[c.month] {
			t.Errorf("%s: recebido = %d", c.month, d.Received)
		}
	}

	// Os links dos cards abrem as listas com os mesmos filtros da contagem.
	d := h.dashboard(t, "2026-04")
	listed := func(f billing.ListFilter) int64 {
		t.Helper()
		f.Limit = 100
		pg, err := h.env.Billing.List(ctx, h.tn.ID, f)
		if err != nil {
			t.Fatal(err)
		}
		var sum int64
		for _, inv := range pg.Items {
			sum += inv.AmountDue.Amount()
		}
		return sum
	}
	if open, overdue := listed(billing.ListFilter{OpenBalance: true}), listed(billing.ListFilter{Overdue: true}); open != d.OpenReceivables ||
		overdue != d.OverdueReceivables {
		t.Errorf("listas: com saldo %d (painel %d), vencidas %d (painel %d)", open, d.OpenReceivables, overdue, d.OverdueReceivables)
	}
}

// fullLedgerReceivables é a forma de referência dos recebíveis num instante:
// o saldo de contas a receber por fatura somando todas as partidas do tenant
// até o instante ($2), sem restringir as faturas. As consultas do painel
// precisam coincidir com ela.
const fullLedgerReceivables = `
WITH ar AS (
  SELECT COALESCE(p.invoice_id, cn.invoice_id, CASE WHEN t.reference_type = 'invoice' THEN t.reference_id END) AS invoice_id,
         sum(CASE WHEN e.direction = 'debit' THEN e.amount ELSE -e.amount END) AS balance
    FROM ledger_entries e
    JOIN ledger_accounts a ON a.tenant_id = e.tenant_id AND a.id = e.account_id
    JOIN ledger_transactions t ON t.tenant_id = e.tenant_id AND t.id = e.transaction_id
    LEFT JOIN payments p ON t.reference_type = 'payment' AND p.tenant_id = t.tenant_id AND p.id = t.reference_id
    LEFT JOIN credit_notes cn ON t.reference_type = 'credit_note' AND cn.tenant_id = t.tenant_id AND cn.id = t.reference_id
   WHERE e.tenant_id = $1 AND a.code = 'accounts_receivable' AND t.occurred_at <= $2
   GROUP BY 1)
SELECT COALESCE(sum(ar.balance) FILTER (WHERE ar.balance > 0), 0)::bigint,
       COALESCE(sum(ar.balance) FILTER (WHERE ar.balance > 0 AND i.due_at <= $2), 0)::bigint,
       COALESCE(sum(ar.balance) FILTER (WHERE ar.balance > 0 AND i.due_at > $3 AND i.due_at <= $2), 0)::bigint
  FROM ar JOIN invoices i ON i.tenant_id = $1 AND i.id = ar.invoice_id`

// checkReceivableForms confere, no instante at, a consulta restrita do mês
// encerrado contra a forma de referência e, no instante atual (current),
// também a consulta pelo amount_due do mês corrente.
func checkReceivableForms(t *testing.T, db *postgres.DB, tenantID uuid.UUID, at time.Time, current bool) {
	t.Helper()
	ctx := context.Background()
	since := at.AddDate(0, 0, -90)
	err := db.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var ref [3]int64
		if err := tx.QueryRow(ctx, fullLedgerReceivables, tenantID, at, since).Scan(&ref[0], &ref[1], &ref[2]); err != nil {
			return err
		}
		q := dbgen.New(tx)
		restricted, err := q.DashboardReceivablesAt(ctx, dbgen.DashboardReceivablesAtParams{TenantID: tenantID, PositionAt: &at, Since: &since})
		if err != nil {
			return err
		}
		if got := [3]int64{restricted.OpenTotal, restricted.OverdueTotal, restricted.DelinquencyOpen}; got != ref {
			t.Errorf("%v: consulta restrita %v, referência %v", at, got, ref)
		}
		if !current {
			return nil
		}
		cur, err := q.DashboardReceivablesCurrent(ctx, dbgen.DashboardReceivablesCurrentParams{TenantID: tenantID, PositionAt: &at, Since: &since})
		if err != nil {
			return err
		}
		if got := [3]int64{cur.OpenTotal, cur.OverdueTotal, cur.DelinquencyOpen}; got != ref {
			t.Errorf("%v: consulta do mês corrente %v, referência %v", at, got, ref)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
