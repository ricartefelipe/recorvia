//go:build integration

package pgstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// advEnv acrescenta ao ambiente de integração os serviços de faturamento
// avançado, todos sobre o mesmo banco, store e relógio manual.
type advEnv struct {
	*fixture.Env
	Changes     *billing.ChangeService
	CreditNotes *billing.CreditNoteService
	Corrector   *billing.UsageCorrector
	Coupons     *catalog.CouponService
	Terms       *subscriptions.TermsService
	Metering    *metering.Service
}

func newAdvEnv(t *testing.T, maxConns int32) *advEnv {
	t.Helper()
	env := fixture.New(t, maxConns, now)
	return &advEnv{Env: env,
		Changes:     billing.NewChangeService(env.Store.Billing(), env.Clock),
		CreditNotes: billing.NewCreditNoteService(env.Store.Billing(), env.Clock),
		Corrector:   billing.NewUsageCorrector(env.Store.Billing(), env.Clock),
		Coupons:     catalog.NewCouponService(env.Store.Catalog(), env.Clock),
		Terms:       subscriptions.NewTermsService(env.Store.Subscriptions(), env.Clock),
		Metering:    metering.NewService(env.Store.Metering(), env.Clock)}
}

// spTime converte data e hora civis de São Paulo (fuso dos tenants de teste)
// para UTC.
func spTime(t *testing.T, year int, month time.Month, day, hour int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(year, month, day, hour, 0, 0, 0, loc).UTC()
}

func brlCents(v int64) money.Money { return money.New(v, money.BRL) }

// errCode devolve o código estável do erro de aplicação ("" se não houver).
func errCode(err error) string {
	if e, ok := apperr.As(err); ok {
		return e.Code
	}
	return ""
}

func ratEqual(t *testing.T, got, want string) bool {
	t.Helper()
	g, ok1 := new(big.Rat).SetString(got)
	w, ok2 := new(big.Rat).SetString(want)
	if !ok1 || !ok2 {
		t.Fatalf("decimal inválido: %q ou %q", got, want)
	}
	return g.Cmp(w) == 0
}

func (a *advEnv) inTenant(t *testing.T, tenantID uuid.UUID, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := a.DB.InTenantTx(context.Background(), tenantID, fn); err != nil {
		t.Fatal(err)
	}
}

func (a *advEnv) runEngine(t *testing.T, tn fixture.Tenant, wantInvoices int) {
	t.Helper()
	res, err := a.Engine.RunTenant(context.Background(), tn.ID, tn.Actor)
	if err != nil {
		t.Fatalf("motor: %v", err)
	}
	if res.Invoices != wantInvoices {
		t.Fatalf("faturas emitidas = %d, esperado %d", res.Invoices, wantInvoices)
	}
}

// uniqueSuffix usa a parte aleatória do identificador: o prefixo do UUIDv7
// deriva do relógio e se repete em criações próximas.
func uniqueSuffix() string { return ids.New().String()[24:] }

// newFlatPlan cria produto e plano mensal de valor fixo com código único,
// permitindo vários planos no mesmo tenant.
func (a *advEnv) newFlatPlan(t *testing.T, tn fixture.Tenant, amount int64) catalog.PlanDetails {
	t.Helper()
	ctx := context.Background()
	prod, err := a.Catalog.CreateProduct(ctx, tn.ID, tn.Actor, catalog.ProductInput{Name: "Produto de valor fixo"})
	if err != nil {
		t.Fatal(err)
	}
	uid := tn.Owner.ID
	p, err := a.Catalog.CreatePlan(ctx, tn.ID, tn.Actor, &uid, catalog.PlanInput{ProductID: prod.ID, Code: "fixo-" + uniqueSuffix(),
		Name: fmt.Sprintf("Plano de %d centavos", amount), Interval: calendar.Monthly}, catalog.PriceInput{Model: catalog.Flat, FlatAmount: &amount})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (a *advEnv) newPerUnitPlan(t *testing.T, tn fixture.Tenant, unitAmount string) catalog.PlanDetails {
	t.Helper()
	ctx := context.Background()
	prod, err := a.Catalog.CreateProduct(ctx, tn.ID, tn.Actor, catalog.ProductInput{Name: "Licenças"})
	if err != nil {
		t.Fatal(err)
	}
	uid := tn.Owner.ID
	p, err := a.Catalog.CreatePlan(ctx, tn.ID, tn.Actor, &uid, catalog.PlanInput{ProductID: prod.ID, Code: "licencas-" + uniqueSuffix(),
		Name: "Plano por licença", Interval: calendar.Monthly}, catalog.PriceInput{Model: catalog.PerUnit, UnitAmount: unitAmount})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// newMeteredPlan habilita consumo medido no plano da plataforma do tenant e
// cria métrica e plano com o preço informado (a métrica é preenchida aqui).
func (a *advEnv) newMeteredPlan(t *testing.T, tn fixture.Tenant, agg catalog.Aggregation, price catalog.PriceInput) (catalog.Metric, catalog.PlanDetails) {
	t.Helper()
	ctx := context.Background()
	if _, err := a.Platform.ChangePlan(ctx, tn.ID, tn.Actor, "profissional", a.PlatformVersion(t, tn.ID)); err != nil {
		t.Fatal(err)
	}
	m, err := a.Catalog.CreateMetric(ctx, tn.ID, tn.Actor, catalog.MetricInput{Code: "metrica." + uniqueSuffix(), Name: "Métrica de teste",
		Unit: "unidade", Aggregation: agg, MaxQuantityPerEvent: "1000000"})
	if err != nil {
		t.Fatal(err)
	}
	prod, err := a.Catalog.CreateProduct(ctx, tn.ID, tn.Actor, catalog.ProductInput{Name: "Produto medido"})
	if err != nil {
		t.Fatal(err)
	}
	price.MetricID = &m.ID
	uid := tn.Owner.ID
	p, err := a.Catalog.CreatePlan(ctx, tn.ID, tn.Actor, &uid, catalog.PlanInput{ProductID: prod.ID, Code: "medido-" + uniqueSuffix(),
		Name: "Plano medido", Interval: calendar.Monthly}, price)
	if err != nil {
		t.Fatal(err)
	}
	return m, p
}

func (a *advEnv) newSubscriptionQty(t *testing.T, tn fixture.Tenant, planID uuid.UUID, qty int) subscriptions.Subscription {
	t.Helper()
	zero := 0
	s, err := a.Subscriptions.Create(context.Background(), tn.ID, tn.Actor, subscriptions.CreateInput{CustomerID: a.NewCustomer(t, tn).ID,
		PlanID: planID, Quantity: qty, TrialDays: &zero})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (a *advEnv) ingest(t *testing.T, tn fixture.Tenant, events ...metering.EventInput) []metering.Result {
	t.Helper()
	res, err := a.Metering.Ingest(context.Background(), tn.ID, events)
	if err != nil {
		t.Fatalf("ingestão: %v", err)
	}
	return res
}

func wantOutcomes(t *testing.T, res []metering.Result, want ...metering.Outcome) {
	t.Helper()
	if len(res) != len(want) {
		t.Fatalf("resultados = %d, esperado %d", len(res), len(want))
	}
	for i := range want {
		if res[i].Outcome != want[i] {
			t.Errorf("evento %s: resultado %s (%s), esperado %s", res[i].ExternalID, res[i].Outcome, res[i].Error, want[i])
		}
	}
}

type invoiceSummary struct {
	ID             uuid.UUID
	Number         string
	Kind           string
	Status         string
	PeriodStart    time.Time
	PeriodEnd      time.Time
	Subtotal       int64
	DiscountTotal  int64
	Total          int64
	CreditApplied  int64
	AmountCredited int64
	AmountDue      int64
	Version        int
}

// invoicesOf lista as faturas da assinatura pela ordem do período.
func (a *advEnv) invoicesOf(t *testing.T, tenantID, subscriptionID uuid.UUID) []invoiceSummary {
	t.Helper()
	var out []invoiceSummary
	a.inTenant(t, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT id, number, kind, status, billing_period_start, billing_period_end, subtotal,
			discount_total, total, credit_applied, amount_credited, amount_due, version
			FROM invoices WHERE tenant_id = $1 AND subscription_id = $2 ORDER BY billing_period_start, issued_at, id`, tenantID, subscriptionID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[invoiceSummary])
		return err
	})
	return out
}

func findInvoice(t *testing.T, invs []invoiceSummary, kind billing.Kind, periodStart time.Time) invoiceSummary {
	t.Helper()
	for _, inv := range invs {
		if inv.Kind == string(kind) && inv.PeriodStart.Equal(periodStart) {
			return inv
		}
	}
	t.Fatalf("fatura %s do período iniciado em %s não encontrada", kind, periodStart)
	return invoiceSummary{}
}

type itemSummary struct {
	ID               uuid.UUID
	Kind             string
	Description      string
	Amount           int64
	DiscountAmount   int64
	Quantity         string
	PlanID           *uuid.UUID
	PriceVersionID   *uuid.UUID
	UsageAggregateID *uuid.UUID
	Snapshot         string
}

// snap devolve o valor do snapshot de preço como texto ("" se ausente).
func (it itemSummary) snap(t *testing.T, key string) string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(it.Snapshot))
	dec.UseNumber()
	m := map[string]any{}
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	return fmt.Sprint(v)
}

func (a *advEnv) itemsOf(t *testing.T, tenantID, invoiceID uuid.UUID) []itemSummary {
	t.Helper()
	var out []itemSummary
	a.inTenant(t, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT id, kind, description, amount, discount_amount, quantity::text, plan_id,
			price_version_id, usage_aggregate_id, price_snapshot::text
			FROM invoice_items WHERE tenant_id = $1 AND invoice_id = $2 ORDER BY position`, tenantID, invoiceID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[itemSummary])
		return err
	})
	return out
}

type aggregateSummary struct {
	ID                uuid.UUID
	Quantity          string
	EventCount        int
	InvoiceItemID     *uuid.UUID
	PriceVersionID    *uuid.UUID
	CorrectedQuantity *string
	CorrectedAmount   *int64
}

func (a *advEnv) aggregateOf(t *testing.T, tenantID, subscriptionID uuid.UUID, periodStart time.Time) aggregateSummary {
	t.Helper()
	var out aggregateSummary
	a.inTenant(t, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT id, quantity::text, event_count, invoice_item_id, price_version_id,
			corrected_quantity::text, corrected_amount
			FROM usage_aggregates WHERE tenant_id = $1 AND subscription_id = $2 AND period_start = $3`, tenantID, subscriptionID, periodStart)
		if err != nil {
			return err
		}
		out, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[aggregateSummary])
		return err
	})
	return out
}

type pendingSummary struct {
	ID               uuid.UUID
	Kind             string
	Source           string
	Amount           int64
	UsageAggregateID *uuid.UUID
	InvoiceID        *uuid.UUID
}

func (a *advEnv) pendingItemsOf(t *testing.T, tenantID, subscriptionID uuid.UUID) []pendingSummary {
	t.Helper()
	var out []pendingSummary
	a.inTenant(t, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT id, kind, source, amount, usage_aggregate_id, invoice_id
			FROM pending_invoice_items WHERE tenant_id = $1 AND subscription_id = $2 ORDER BY created_at, id`, tenantID, subscriptionID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[pendingSummary])
		return err
	})
	return out
}

type usageEventSummary struct {
	Status           string
	AggregateID      *uuid.UUID
	CorrectedAt      *time.Time
	CorrectionItemID *uuid.UUID
}

func (a *advEnv) usageEvent(t *testing.T, tenantID uuid.UUID, externalID string) usageEventSummary {
	t.Helper()
	var out usageEventSummary
	a.inTenant(t, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT status, aggregate_id, corrected_at, correction_item_id
			FROM usage_events WHERE tenant_id = $1 AND external_id = $2`, tenantID, externalID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[usageEventSummary])
		return err
	})
	return out
}

// creditBalance soma os movimentos de crédito do cliente.
func (a *advEnv) creditBalance(t *testing.T, tenantID, customerID uuid.UUID) int64 {
	t.Helper()
	var n int64
	a.inTenant(t, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT COALESCE(sum(amount), 0)::bigint FROM customer_credit_entries
			WHERE tenant_id = $1 AND customer_id = $2`, tenantID, customerID).Scan(&n)
	})
	return n
}

// lockWaiters conta as sessões que aguardam algum bloqueio no banco.
func lockWaiters(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(DISTINCT pid)::int FROM pg_locks WHERE NOT granted`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// waitLockWaiters aguarda até que ao menos n sessões estejam bloqueadas ou
// até done ser fechado. Informa se o número de sessões foi atingido.
func waitLockWaiters(t *testing.T, pool *pgxpool.Pool, n int, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-done:
			return false
		default:
		}
		if lockWaiters(t, pool) >= n {
			return true
		}
		if time.Now().After(deadline) {
			t.Fatalf("tempo esgotado aguardando %d sessões bloqueadas", n)
		}
	}
}

// As tabelas de condições programadas e notas de crédito têm RLS com a
// política padrão; com o papel da aplicação, um tenant não lê, não altera e
// não grava linhas de outro, nem por SQL direto nem pelos casos de uso, com a
// mesma conexão física reutilizada entre os tenants.
func TestAdvancedTablesIsolateTenantsUnderAppRole(t *testing.T) {
	ctx := context.Background()
	tables := []string{"subscription_scheduled_changes", "subscription_price_adjustments", "credit_notes"}
	owner := pgtest.OwnerPool(t)
	for _, table := range tables {
		var rls bool
		var policies int
		err := owner.QueryRow(ctx, `SELECT c.relrowsecurity,
			(SELECT count(*) FROM pg_policies p WHERE p.schemaname = 'public' AND p.tablename = c.relname AND p.policyname = 'tenant_isolation')::int
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = $1`, table).Scan(&rls, &policies)
		if err != nil {
			t.Fatal(err)
		}
		if !rls || policies != 1 {
			t.Errorf("%s: rls=%v, políticas de isolamento=%d", table, rls, policies)
		}
	}

	a := newAdvEnv(t, 1)
	privileges := []struct {
		table, priv string
		want        bool
	}{
		{"credit_notes", "SELECT", true}, {"credit_notes", "INSERT", true},
		{"credit_notes", "UPDATE", false}, {"credit_notes", "DELETE", false},
		{"subscription_scheduled_changes", "UPDATE", true}, {"subscription_scheduled_changes", "DELETE", false},
		{"subscription_price_adjustments", "UPDATE", true}, {"subscription_price_adjustments", "DELETE", false},
	}
	for _, p := range privileges {
		var has bool
		if err := a.DB.Pool().QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, p.table, p.priv).Scan(&has); err != nil {
			t.Fatal(err)
		}
		if has != p.want {
			t.Errorf("privilégio %s em %s = %v, esperado %v", p.priv, p.table, has, p.want)
		}
	}

	ta, tb := a.NewTenant(t, "adv-iso-a"), a.NewTenant(t, "adv-iso-b")
	basicA, premiumA := a.newFlatPlan(t, ta, 10000), a.newFlatPlan(t, ta, 15000)
	subA := a.NewSubscription(t, ta, a.NewCustomer(t, ta).ID, basicA.ID, nil)
	subB := a.NewSubscription(t, tb, a.NewCustomer(t, tb).ID, a.newFlatPlan(t, tb, 5000).ID, nil)
	a.runEngine(t, ta, 1)
	a.runEngine(t, tb, 1)
	invA := a.invoicesOf(t, ta.ID, subA.ID)[0]
	invB := a.invoicesOf(t, tb.ID, subB.ID)[0]

	change, err := a.Changes.Change(ctx, ta.ID, ta.Actor, &ta.Owner.ID, subA.ID,
		billing.ChangeInput{PlanID: &premiumA.ID, Timing: subscriptions.ChangeNextCycle}, 0)
	if err != nil || change.Scheduled == nil {
		t.Fatalf("agendamento: %+v %v", change.Scheduled, err)
	}
	pct := 800
	adj, err := a.Terms.ScheduleAdjustment(ctx, ta.ID, ta.Actor, &ta.Owner.ID, subA.ID, subscriptions.AdjustmentInput{
		EffectiveDate: calendar.Date{Year: 2026, Month: time.June, Day: 1}, Kind: subscriptions.AdjustPercent, PercentBP: &pct,
		Reason: "Reajuste contratual"})
	if err != nil {
		t.Fatal(err)
	}
	noteA, err := a.CreditNotes.Issue(ctx, ta.ID, ta.Actor, &ta.Owner.ID, invA.ID, brlCents(1000), "Desconto comercial", "iso-nota-1")
	if err != nil {
		t.Fatal(err)
	}
	couponPct := 1000
	couponA, err := a.Coupons.Create(ctx, ta.ID, ta.Actor, catalog.CouponInput{Code: "EXCLUSIVO-A", Name: "Cupom do tenant A",
		Type: catalog.DiscountPercent, PercentOffBP: &couponPct, Duration: catalog.DurationForever})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("SQL direto no contexto de outro tenant", func(t *testing.T) {
		for _, table := range tables {
			if n := countRows(t, a.Env, tb.ID, `SELECT count(*) FROM `+table); n != 0 {
				t.Errorf("%s: tenant B enxerga %d linhas", table, n)
			}
			if n := countRows(t, a.Env, tb.ID, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, ta.ID); n != 0 {
				t.Errorf("%s: tenant B enxerga %d linhas filtrando pelo tenant A", table, n)
			}
			if n := countRows(t, a.Env, ta.ID, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, ta.ID); n != 1 {
				t.Errorf("%s: tenant A enxerga %d linhas próprias, esperado 1", table, n)
			}
		}
		updates := []struct {
			name, stmt string
			id         uuid.UUID
		}{
			{"mudança agendada", `UPDATE subscription_scheduled_changes SET status = 'canceled', resolved_at = now() WHERE id = $1`, change.Scheduled.ID},
			{"reajuste", `UPDATE subscription_price_adjustments SET status = 'canceled', canceled_at = now() WHERE id = $1`, adj.ID},
		}
		for _, u := range updates {
			var affected int64
			a.inTenant(t, tb.ID, func(tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, u.stmt, u.id)
				affected = tag.RowsAffected()
				return err
			})
			if affected != 0 {
				t.Errorf("%s: tenant B alterou %d linhas do tenant A", u.name, affected)
			}
		}
		inserts := []struct {
			name, stmt string
			ref        uuid.UUID
		}{
			{"nota de crédito", `INSERT INTO credit_notes (id, tenant_id, invoice_id, number, amount, reason, created_at)
				VALUES ($1, $2, $3, 'INVASORA-NC01', 100, 'Tentativa indevida', now())`, invA.ID},
			{"mudança agendada", `INSERT INTO subscription_scheduled_changes (id, tenant_id, subscription_id, kind, new_quantity, effective_at, created_at)
				VALUES ($1, $2, $3, 'quantity_change', 2, now(), now())`, subA.ID},
			{"reajuste", `INSERT INTO subscription_price_adjustments (id, tenant_id, subscription_id, effective_date, kind, percent_bp, reason, created_at)
				VALUES ($1, $2, $3, '2026-07-01', 'percent', 100, 'Tentativa indevida', now())`, subA.ID},
		}
		for _, in := range inserts {
			// Com tenant_id de A no contexto de B, a política (WITH CHECK) rejeita.
			err := a.DB.InTenantTx(ctx, tb.ID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, in.stmt, ids.New(), ta.ID, in.ref)
				return err
			})
			if !postgres.IsInsufficientPrivilege(err) {
				t.Errorf("%s com tenant_id de A no contexto de B: %v", in.name, err)
			}
			// Com tenant_id de B referenciando entidade de A, a FK composta rejeita.
			err = a.DB.InTenantTx(ctx, tb.ID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, in.stmt, ids.New(), tb.ID, in.ref)
				return err
			})
			if !postgres.IsForeignKeyViolation(err) {
				t.Errorf("%s de B referenciando entidade de A: %v", in.name, err)
			}
		}
	})

	t.Run("casos de uso no contexto de outro tenant", func(t *testing.T) {
		if _, err := a.Terms.Adjustments(ctx, tb.ID, subA.ID, 25, ""); !apperr.IsKind(err, apperr.KindNotFound) {
			t.Errorf("reajustes de A listados por B: %v", err)
		}
		if err := a.Terms.CancelAdjustment(ctx, tb.ID, tb.Actor, subA.ID, adj.ID); !apperr.IsKind(err, apperr.KindNotFound) {
			t.Errorf("reajuste de A cancelado por B: %v", err)
		}
		if _, err := a.Changes.Change(ctx, tb.ID, tb.Actor, &tb.Owner.ID, subA.ID,
			billing.ChangeInput{PlanID: &premiumA.ID, Timing: subscriptions.ChangeImmediately}, 0); !apperr.IsKind(err, apperr.KindNotFound) {
			t.Errorf("assinatura de A alterada por B: %v", err)
		}
		if _, err := a.CreditNotes.Issue(ctx, tb.ID, tb.Actor, &tb.Owner.ID, invA.ID, brlCents(100), "Tentativa indevida", ""); !apperr.IsKind(err, apperr.KindNotFound) {
			t.Errorf("nota de crédito emitida por B na fatura de A: %v", err)
		}
		notes, err := a.Store.Billing().CreditNotes(ctx, tb.ID, invA.ID)
		if err != nil || len(notes) != 0 {
			t.Errorf("notas de A listadas por B: %d %v", len(notes), err)
		}
		terms, err := a.Terms.Terms(ctx, tb.ID, subA.ID)
		if err != nil || len(terms.ScheduledChanges) != 0 || terms.Discount != nil {
			t.Errorf("condições de A vistas por B: %+v %v", terms, err)
		}
		if _, err := a.Terms.ApplyCoupon(ctx, tb.ID, tb.Actor, &tb.Owner.ID, subB.ID, couponA.Code); errCode(err) != "coupon_not_found" {
			t.Errorf("cupom de A aplicado por B: %v", err)
		}
		// A chave do pedido é única por tenant: B reutiliza a chave de A.
		noteB, err := a.CreditNotes.Issue(ctx, tb.ID, tb.Actor, &tb.Owner.ID, invB.ID, brlCents(500), "Desconto comercial", "iso-nota-1")
		if err != nil {
			t.Fatal(err)
		}
		if noteB.ID == noteA.ID || noteB.InvoiceID != invB.ID {
			t.Errorf("chave do pedido de B devolveu a nota de A: %+v", noteB)
		}
	})

	t.Run("dados do tenant A preservados", func(t *testing.T) {
		terms, err := a.Terms.Terms(ctx, ta.ID, subA.ID)
		if err != nil || len(terms.ScheduledChanges) != 1 || terms.ScheduledChanges[0].ID != change.Scheduled.ID ||
			terms.ScheduledChanges[0].Status != subscriptions.ChangePending {
			t.Errorf("mudança agendada de A: %+v %v", terms.ScheduledChanges, err)
		}
		adjPage, err := a.Terms.Adjustments(ctx, ta.ID, subA.ID, 100, "")
		adjs := adjPage.Items
		if err != nil || len(adjs) != 1 || adjs[0].Status != subscriptions.AdjustmentScheduled {
			t.Errorf("reajustes de A: %+v %v", adjs, err)
		}
		notes, err := a.Store.Billing().CreditNotes(ctx, ta.ID, invA.ID)
		if err != nil || len(notes) != 1 || notes[0].ID != noteA.ID {
			t.Errorf("notas de crédito de A: %+v %v", notes, err)
		}
	})
}

// Mudança agendada para o próximo ciclo não altera o período corrente; o
// motor a aplica no início do ciclo seguinte, com a versão de preço fixada no
// agendamento, e a fatura do ciclo já reflete o novo plano ou quantidade.
func TestScheduledChangeAppliedByEngineAtCycleStart(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		setup      func(t *testing.T, a *advEnv, tn fixture.Tenant) (sub subscriptions.Subscription, in billing.ChangeInput, target catalog.PlanDetails)
		wantAmount int64
		wantQty    string
	}{
		{"troca de plano", func(t *testing.T, a *advEnv, tn fixture.Tenant) (subscriptions.Subscription, billing.ChangeInput, catalog.PlanDetails) {
			basic, premium := a.newFlatPlan(t, tn, 10000), a.newFlatPlan(t, tn, 16000)
			sub := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, basic.ID, nil)
			return sub, billing.ChangeInput{PlanID: &premium.ID, Timing: subscriptions.ChangeNextCycle}, premium
		}, 16000, "1"},
		{"troca de quantidade", func(t *testing.T, a *advEnv, tn fixture.Tenant) (subscriptions.Subscription, billing.ChangeInput, catalog.PlanDetails) {
			plan := a.newPerUnitPlan(t, tn, "2500")
			sub := a.newSubscriptionQty(t, tn, plan.ID, 4)
			qty := 6
			return sub, billing.ChangeInput{Quantity: &qty, Timing: subscriptions.ChangeNextCycle}, plan
		}, 15000, "6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdvEnv(t, 4)
			tn := a.NewTenant(t, "adv-agenda")
			sub, in, target := tc.setup(t, a, tn)
			a.runEngine(t, tn, 1)
			first, err := a.Changes.Change(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, in, 0)
			if err != nil {
				t.Fatal(err)
			}
			// Um novo pedido substitui o agendamento anterior.
			res, err := a.Changes.Change(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, in, 0)
			if err != nil {
				t.Fatal(err)
			}
			cycleStart := spTime(t, 2026, time.April, 10, 0)
			if res.Scheduled == nil || res.InvoiceID != nil || res.Proration != nil {
				t.Fatalf("resultado do agendamento: %+v", res)
			}
			if !res.Scheduled.EffectiveAt.Equal(cycleStart) {
				t.Errorf("vigência %s, esperado %s", res.Scheduled.EffectiveAt, cycleStart)
			}
			if res.Subscription.PlanID != sub.PlanID || res.Subscription.Quantity != sub.Quantity || res.Subscription.PriceVersionID != sub.PriceVersionID {
				t.Errorf("o agendamento alterou o período corrente: %+v", res.Subscription)
			}
			if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM subscription_scheduled_changes WHERE tenant_id = $1 AND id = $2 AND status = 'canceled'`,
				tn.ID, first.Scheduled.ID); n != 1 {
				t.Errorf("agendamento substituído não foi cancelado")
			}
			if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM subscription_scheduled_changes WHERE tenant_id = $1 AND subscription_id = $2 AND status = 'pending'`,
				tn.ID, sub.ID); n != 1 {
				t.Errorf("agendamentos pendentes = %d, esperado 1", n)
			}

			a.runEngine(t, tn, 0)
			a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
			a.runEngine(t, tn, 1)
			a.runEngine(t, tn, 0)

			invs := a.invoicesOf(t, tn.ID, sub.ID)
			if len(invs) != 2 || invs[0].Total != 10000 {
				t.Fatalf("faturas: %+v", invs)
			}
			next := findInvoice(t, invs, billing.KindSubscriptionCycle, cycleStart)
			if next.Total != tc.wantAmount {
				t.Errorf("total do ciclo com a mudança = %d, esperado %d", next.Total, tc.wantAmount)
			}
			items := a.itemsOf(t, tn.ID, next.ID)
			if len(items) != 1 {
				t.Fatalf("itens: %+v", items)
			}
			fee := items[0]
			wantPrice := *target.CurrentPriceVersionID
			if fee.Kind != string(billing.ItemSubscriptionFee) || fee.Amount != tc.wantAmount || fee.PlanID == nil || *fee.PlanID != target.ID ||
				fee.PriceVersionID == nil || *fee.PriceVersionID != wantPrice || !ratEqual(t, fee.Quantity, tc.wantQty) {
				t.Errorf("item do ciclo: %+v", fee)
			}
			if got := fee.snap(t, "plan_code"); got != target.Code {
				t.Errorf("snapshot plan_code = %q, esperado %q", got, target.Code)
			}

			var status string
			var resolvedAt *time.Time
			a.inTenant(t, tn.ID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT status, resolved_at FROM subscription_scheduled_changes WHERE tenant_id = $1 AND id = $2`,
					tn.ID, res.Scheduled.ID).Scan(&status, &resolvedAt)
			})
			if status != string(subscriptions.ChangeApplied) || resolvedAt == nil || !resolvedAt.Equal(a.Clock.Now()) {
				t.Errorf("mudança agendada: status=%s resolvida=%v", status, resolvedAt)
			}
			got, err := a.Subscriptions.Get(ctx, tn.ID, sub.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.PlanID != target.ID || got.PriceVersionID != wantPrice {
				t.Errorf("assinatura após o ciclo: plano %s versão %s", got.PlanID, got.PriceVersionID)
			}
			if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM subscription_history WHERE tenant_id = $1 AND subscription_id = $2
				AND event IN ('plan_changed', 'quantity_changed') AND details->>'scheduled_change_id' = $3`, tn.ID, sub.ID, res.Scheduled.ID.String()); n != 1 {
				t.Errorf("histórico da mudança aplicada = %d, esperado 1", n)
			}
		})
	}
}

// Mudança imediata com o período corrente já faturado: prorrata por dias
// civis no fuso do contrato, contando o dia da mudança. Período de 10/03 a
// 10/04 (31 dias), troca em 31/03: fração 10/31. De R$ 100,00 para R$ 160,00,
// crédito de 3.226 (3.225,81 arredondado) e cobrança de 5.161 (5.161,29),
// diferença de 1.935. Diferença negativa vira crédito do cliente, consumido
// na fatura seguinte. O crédito incide sobre o valor faturado no período: com
// cupom de 10% aplicado antes da fatura de ciclo (válido nos dois planos),
// 9.000 × 10/31 = 2.903,23 → 2.903 e a cobrança 16.000 × 10/31 × 0,9 =
// 4.645,16 → 4.645, diferença de 1.742; com o cupom aplicado depois da
// fatura de ciclo, o crédito é o do valor cheio (3.226) e só a cobrança tem
// o desconto, diferença de 1.419.
func TestImmediateChangeProratesCurrentPeriod(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name                          string
		from, to                      int64
		couponBP                      int
		couponBeforeBilling           bool
		wantCredit, wantCharge        int64
		wantNet                       int64
		wantTotal                     int64
		wantCustomerCredit            int64
		wantNextTotal, wantNextCredit int64
		wantNextDue                   int64
	}{
		{name: "upgrade gera fatura de prorrata", from: 10000, to: 16000, wantCredit: 3226, wantCharge: 5161, wantNet: 1935,
			wantTotal: 1935, wantNextTotal: 16000, wantNextDue: 16000},
		{name: "upgrade com cupom percentual aplicado antes da fatura do período", from: 10000, to: 16000, couponBP: 1000,
			couponBeforeBilling: true, wantCredit: 2903, wantCharge: 4645, wantNet: 1742, wantTotal: 1742, wantNextTotal: 14400, wantNextDue: 14400},
		{name: "upgrade com cupom percentual aplicado no meio do período", from: 10000, to: 16000, couponBP: 1000, wantCredit: 3226,
			wantCharge: 4645, wantNet: 1419, wantTotal: 1419, wantNextTotal: 14400, wantNextDue: 14400},
		{name: "downgrade gera crédito do cliente", from: 16000, to: 10000, wantCredit: 5161, wantCharge: 3226, wantNet: -1935,
			wantCustomerCredit: 1935, wantNextTotal: 10000, wantNextCredit: 1935, wantNextDue: 8065},
		{name: "downgrade com cupom percentual aplicado antes da fatura do período", from: 16000, to: 10000, couponBP: 1000,
			couponBeforeBilling: true, wantCredit: 4645, wantCharge: 2903, wantNet: -1742, wantCustomerCredit: 1742, wantNextTotal: 9000,
			wantNextCredit: 1742, wantNextDue: 7258},
		{name: "downgrade com cupom percentual aplicado no meio do período", from: 16000, to: 10000, couponBP: 1000, wantCredit: 5161,
			wantCharge: 2903, wantNet: -2258, wantCustomerCredit: 2258, wantNextTotal: 9000, wantNextCredit: 2258, wantNextDue: 6742},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdvEnv(t, 4)
			tn := a.NewTenant(t, "adv-prorrata")
			fromPlan, toPlan := a.newFlatPlan(t, tn, tc.from), a.newFlatPlan(t, tn, tc.to)
			cust := a.NewCustomer(t, tn)
			sub := a.NewSubscription(t, tn, cust.ID, fromPlan.ID, nil)
			applyCoupon := func() {
				t.Helper()
				bp := tc.couponBP
				c, err := a.Coupons.Create(ctx, tn.ID, tn.Actor, catalog.CouponInput{Code: "FIDELIDADE", Name: "Fidelidade",
					Type: catalog.DiscountPercent, PercentOffBP: &bp, Duration: catalog.DurationForever})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := a.Terms.ApplyCoupon(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, c.Code); err != nil {
					t.Fatal(err)
				}
			}
			if tc.couponBP > 0 && tc.couponBeforeBilling {
				applyCoupon()
			}
			a.runEngine(t, tn, 1)
			if tc.couponBP > 0 && !tc.couponBeforeBilling {
				applyCoupon()
			}
			a.Clock.Set(spTime(t, 2026, time.March, 31, 12))
			res, err := a.Changes.Change(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID,
				billing.ChangeInput{PlanID: &toPlan.ID, Timing: subscriptions.ChangeImmediately}, 0)
			if err != nil {
				t.Fatal(err)
			}
			pr := res.Proration
			if pr == nil {
				t.Fatal("mudança imediata sem prorrata")
			}
			if pr.Fraction.Cmp(big.NewRat(10, 31)) != 0 || pr.RemainingDays != 10 || pr.PeriodDays != 31 {
				t.Errorf("fração %s (%d de %d dias), esperado 10/31", pr.Fraction.RatString(), pr.RemainingDays, pr.PeriodDays)
			}
			if pr.Credit.Amount() != tc.wantCredit || pr.Charge.Amount() != tc.wantCharge || pr.Net.Amount() != tc.wantNet {
				t.Errorf("prorrata: crédito %d cobrança %d diferença %d", pr.Credit.Amount(), pr.Charge.Amount(), pr.Net.Amount())
			}
			if res.Subscription.PlanID != toPlan.ID {
				t.Errorf("plano após a mudança imediata = %s", res.Subscription.PlanID)
			}
			changeDay := spTime(t, 2026, time.March, 31, 0)
			periodEnd := spTime(t, 2026, time.April, 10, 0)
			invs := a.invoicesOf(t, tn.ID, sub.ID)
			if tc.wantNet > 0 {
				if res.InvoiceID == nil || res.CreditGranted != nil {
					t.Fatalf("upgrade: fatura %v crédito %v", res.InvoiceID, res.CreditGranted)
				}
				inv := findInvoice(t, invs, billing.KindProration, changeDay)
				if inv.ID != *res.InvoiceID || !inv.PeriodEnd.Equal(periodEnd) || inv.Status != string(billing.Open) {
					t.Errorf("fatura de prorrata: %+v", inv)
				}
				if inv.Subtotal != tc.wantNet || inv.DiscountTotal != 0 || inv.Total != tc.wantTotal || inv.AmountDue != tc.wantTotal {
					t.Errorf("totais da prorrata: subtotal %d desconto %d total %d saldo %d", inv.Subtotal, inv.DiscountTotal, inv.Total, inv.AmountDue)
				}
				// A cobrança vem antes do crédito; o percentual do cupom já está nos itens.
				items := a.itemsOf(t, tn.ID, inv.ID)
				if len(items) != 2 || items[0].Kind != string(billing.ItemProration) || items[0].Amount != tc.wantCharge ||
					items[1].Kind != string(billing.ItemProration) || items[1].Amount != -tc.wantCredit {
					t.Fatalf("itens da prorrata: %+v", items)
				}
				if *items[0].PlanID != toPlan.ID || *items[1].PlanID != fromPlan.ID || items[0].snap(t, "remaining_days") != "10" ||
					items[0].snap(t, "period_days") != "31" {
					t.Errorf("snapshot da prorrata: %s | %s", items[0].Snapshot, items[1].Snapshot)
				}
				wantBP := ""
				if tc.couponBP > 0 {
					wantBP = fmt.Sprint(tc.couponBP)
				}
				// O percentual vale só para a cobrança; o crédito registra a fatura
				// de ciclo que serviu de base.
				if items[0].snap(t, "discount_percent_bp") != wantBP || items[1].snap(t, "discount_percent_bp") != "" ||
					items[1].snap(t, "credit_basis") != "cycle_invoice" || items[1].snap(t, "billed_invoice_id") != invs[0].ID.String() {
					t.Errorf("snapshot da prorrata: %s | %s", items[0].Snapshot, items[1].Snapshot)
				}
			} else {
				if res.InvoiceID != nil || res.CreditGranted == nil || res.CreditGranted.Amount() != tc.wantCustomerCredit {
					t.Fatalf("downgrade: fatura %v crédito %v", res.InvoiceID, res.CreditGranted)
				}
				if len(invs) != 1 {
					t.Errorf("downgrade emitiu fatura: %+v", invs)
				}
				if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM customer_credit_entries WHERE tenant_id = $1 AND customer_id = $2
					AND kind = 'proration' AND amount = $3`, tn.ID, cust.ID, tc.wantCustomerCredit); n != 1 {
					t.Errorf("movimento de crédito de prorrata = %d, esperado 1", n)
				}
				if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND kind = 'customer_credit.granted'`,
					tn.ID); n != 1 {
					t.Errorf("lançamentos de crédito concedido = %d, esperado 1", n)
				}
			}
			wantCycleTotal := tc.from
			if tc.couponBeforeBilling {
				wantCycleTotal = tc.from * int64(10000-tc.couponBP) / 10000
			}
			if invs[0].Total != wantCycleTotal || invs[0].Version != 1 {
				t.Errorf("fatura do período corrente foi alterada: %+v", invs[0])
			}
			if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM subscription_prorations WHERE tenant_id = $1 AND subscription_id = $2
				AND period_start = $3 AND credit_amount = $4 AND charge_amount = $5 AND price_version_id = $6`, tn.ID, sub.ID,
				spTime(t, 2026, time.March, 10, 0), tc.wantCredit, tc.wantCharge, *toPlan.CurrentPriceVersionID); n != 1 {
				t.Errorf("prorrata registrada = %d, esperado 1", n)
			}

			a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
			a.runEngine(t, tn, 1)
			next := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, periodEnd)
			if next.Total != tc.wantNextTotal || next.CreditApplied != tc.wantNextCredit || next.AmountDue != tc.wantNextDue {
				t.Errorf("ciclo seguinte: total %d crédito %d saldo %d", next.Total, next.CreditApplied, next.AmountDue)
			}
			if bal := a.creditBalance(t, tn.ID, cust.ID); bal != 0 {
				t.Errorf("saldo de crédito restante = %d", bal)
			}
		})
	}
}

// Reajustes valem a partir do primeiro ciclo iniciado na data efetiva ou
// depois. Na retomada após indisponibilidade, cada período atrasado usa o
// preço vigente no seu início. O snapshot do item registra o reajuste e o
// arredondamento é half-even (10.010 centavos + 5% = 10.510,5, arredondado
// para 10.510).
func TestPriceAdjustmentsAppliedAtCorrectCycle(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-reajuste")
	plan := a.newFlatPlan(t, tn, 10010)
	sub := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
	a.runEngine(t, tn, 1)

	pct := 500
	ipca, err := a.Terms.ScheduleAdjustment(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, subscriptions.AdjustmentInput{
		EffectiveDate: calendar.Date{Year: 2026, Month: time.April, Day: 15}, Kind: subscriptions.AdjustPercent, PercentBP: &pct,
		IndexName: "IPCA", Reason: "Reajuste anual pelo IPCA"})
	if err != nil {
		t.Fatal(err)
	}
	newAmount := int64(12000)
	renegotiated, err := a.Terms.ScheduleAdjustment(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, subscriptions.AdjustmentInput{
		EffectiveDate: calendar.Date{Year: 2026, Month: time.June, Day: 10}, Kind: subscriptions.AdjustFixedAmount, NewAmount: &newAmount,
		Reason: "Renegociação comercial"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Terms.ScheduleAdjustment(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, subscriptions.AdjustmentInput{
		EffectiveDate: calendar.Date{Year: 2026, Month: time.June, Day: 10}, Kind: subscriptions.AdjustPercent, PercentBP: &pct,
		Reason: "Reajuste duplicado"}); errCode(err) != "adjustment_date_taken" {
		t.Errorf("reajuste duplicado na mesma data: %v", err)
	}
	canceled, err := a.Terms.ScheduleAdjustment(ctx, tn.ID, tn.Actor, &tn.Owner.ID, sub.ID, subscriptions.AdjustmentInput{
		EffectiveDate: calendar.Date{Year: 2026, Month: time.July, Day: 1}, Kind: subscriptions.AdjustPercent, PercentBP: &pct,
		Reason: "Reajuste desistido"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Terms.CancelAdjustment(ctx, tn.ID, tn.Actor, sub.ID, canceled.ID); err != nil {
		t.Fatal(err)
	}

	// Retomada: 10/04 (antes de 15/04, sem reajuste) e 10/05 (com reajuste).
	a.Clock.Set(spTime(t, 2026, time.May, 10, 9))
	a.runEngine(t, tn, 2)
	// 10/06: o reajuste por valor vale no próprio dia de início do ciclo.
	a.Clock.Set(spTime(t, 2026, time.June, 10, 9))
	a.runEngine(t, tn, 1)
	// 10/07: o reajuste cancelado não é aplicado.
	a.Clock.Set(spTime(t, 2026, time.July, 10, 9))
	a.runEngine(t, tn, 1)

	invs := a.invoicesOf(t, tn.ID, sub.ID)
	type want struct {
		start        time.Time
		amount       int64
		adjustmentID *uuid.UUID
	}
	wants := []want{
		{spTime(t, 2026, time.March, 10, 0), 10010, nil},
		{spTime(t, 2026, time.April, 10, 0), 10010, nil},
		{spTime(t, 2026, time.May, 10, 0), 10510, &ipca.ID},
		{spTime(t, 2026, time.June, 10, 0), 12000, &renegotiated.ID},
		{spTime(t, 2026, time.July, 10, 0), 12000, &renegotiated.ID},
	}
	if len(invs) != len(wants) {
		t.Fatalf("faturas = %d, esperado %d", len(invs), len(wants))
	}
	for i, w := range wants {
		inv := invs[i]
		if !inv.PeriodStart.Equal(w.start) || inv.Total != w.amount || inv.Version != 1 {
			t.Errorf("fatura %d: período %s total %d versão %d, esperado %s %d", i, inv.PeriodStart, inv.Total, inv.Version, w.start, w.amount)
			continue
		}
		items := a.itemsOf(t, tn.ID, inv.ID)
		if len(items) != 1 {
			t.Fatalf("itens da fatura %d: %+v", i, items)
		}
		fee := items[0]
		if fee.PriceVersionID == nil || *fee.PriceVersionID != *plan.CurrentPriceVersionID {
			t.Errorf("fatura %d: versão de preço %v, esperado a original", i, fee.PriceVersionID)
		}
		if got := fee.snap(t, "flat_amount"); got != fmt.Sprint(w.amount) {
			t.Errorf("fatura %d: snapshot flat_amount = %s, esperado %d", i, got, w.amount)
		}
		gotAdj := fee.snap(t, "price_adjustment_id")
		switch {
		case w.adjustmentID == nil && gotAdj != "":
			t.Errorf("fatura %d: snapshot com reajuste %s antes da vigência", i, gotAdj)
		case w.adjustmentID != nil && (gotAdj != w.adjustmentID.String() || fee.snap(t, "adjusted_fixed_amount") != fmt.Sprint(w.amount)):
			t.Errorf("fatura %d: snapshot do reajuste %s", i, fee.Snapshot)
		}
	}

	adjPage, err := a.Terms.Adjustments(ctx, tn.ID, sub.ID, 100, "")
	adjs := adjPage.Items
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uuid.UUID]subscriptions.PriceAdjustment{}
	for _, adj := range adjs {
		byID[adj.ID] = adj
	}
	checks := []struct {
		id               uuid.UUID
		status           subscriptions.AdjustmentStatus
		previous, result int64
		periodStart      time.Time
	}{
		{ipca.ID, subscriptions.AdjustmentApplied, 10010, 10510, spTime(t, 2026, time.May, 10, 0)},
		{renegotiated.ID, subscriptions.AdjustmentApplied, 10510, 12000, spTime(t, 2026, time.June, 10, 0)},
		{canceled.ID, subscriptions.AdjustmentCanceled, 0, 0, time.Time{}},
	}
	for _, c := range checks {
		adj, ok := byID[c.id]
		if !ok || adj.Status != c.status {
			t.Errorf("reajuste %s: %+v", c.id, adj)
			continue
		}
		if c.status != subscriptions.AdjustmentApplied {
			if adj.AppliedAt != nil || adj.CanceledAt == nil || adj.CancelReason != subscriptions.AdjustmentCanceledManually {
				t.Errorf("reajuste cancelado com aplicação registrada ou sem motivo: %+v", adj)
			}
			continue
		}
		if adj.PreviousAmount == nil || adj.PreviousAmount.Cmp(big.NewRat(c.previous, 1)) != 0 || adj.ResultingAmount == nil ||
			adj.ResultingAmount.Cmp(big.NewRat(c.result, 1)) != 0 || adj.AppliedPeriodStart == nil || !adj.AppliedPeriodStart.Equal(c.periodStart) {
			t.Errorf("reajuste aplicado %s: anterior %v resultante %v período %v", c.id, adj.PreviousAmount, adj.ResultingAmount, adj.AppliedPeriodStart)
		}
	}
	got, err := a.Subscriptions.Get(ctx, tn.ID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PriceOverride == nil || got.PriceOverride.Cmp(big.NewRat(12000, 1)) != 0 || got.PriceOverrideAdjustmentID == nil ||
		*got.PriceOverrideAdjustmentID != renegotiated.ID {
		t.Errorf("valor vigente da assinatura: %v %v", got.PriceOverride, got.PriceOverrideAdjustmentID)
	}
}

// Resgates concorrentes de um cupom com limite de usos: somente N
// assinaturas recebem o desconto; as demais recebem conflito, e o contador
// nunca ultrapassa o limite (condição na consulta e constraint no banco).
func TestConcurrentCouponRedemptionStopsAtLimit(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 16)
	tn := a.NewTenant(t, "adv-cupom")
	plan := a.newFlatPlan(t, tn, 10000)
	const limit, contenders = 3, 10
	maxRedemptions, pct := limit, 1500
	coupon, err := a.Coupons.Create(ctx, tn.ID, tn.Actor, catalog.CouponInput{Code: "LIMITE3", Name: "Cupom com limite de usos",
		Type: catalog.DiscountPercent, PercentOffBP: &pct, Duration: catalog.DurationForever, MaxRedemptions: &maxRedemptions})
	if err != nil {
		t.Fatal(err)
	}
	subs := make([]subscriptions.Subscription, contenders)
	for i := range subs {
		subs[i] = a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
	}
	start := make(chan struct{})
	errs := make([]error, contenders)
	var wg sync.WaitGroup
	for i, s := range subs {
		wg.Go(func() {
			<-start
			_, errs[i] = a.Terms.ApplyCoupon(ctx, tn.ID, tn.Actor, &tn.Owner.ID, s.ID, " limite3 ")
		})
	}
	close(start)
	wg.Wait()
	redeemed, exhausted := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			redeemed++
		case errCode(err) == "coupon_exhausted" && apperr.IsKind(err, apperr.KindConflict):
			exhausted++
		default:
			t.Errorf("assinatura %d: erro inesperado %v", i, err)
		}
	}
	if redeemed != limit || exhausted != contenders-limit {
		t.Fatalf("resgates = %d, esgotados = %d; esperado %d e %d", redeemed, exhausted, limit, contenders-limit)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT redemptions_count FROM coupons WHERE tenant_id = $1 AND id = $2`, tn.ID, coupon.ID); n != limit {
		t.Errorf("contador de resgates = %d, esperado %d", n, limit)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM subscription_discounts WHERE tenant_id = $1 AND coupon_id = $2 AND status = 'active'`,
		tn.ID, coupon.ID); n != limit {
		t.Errorf("descontos ativos = %d, esperado %d", n, limit)
	}
	err = a.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE coupons SET redemptions_count = max_redemptions + 1 WHERE tenant_id = $1 AND id = $2`, tn.ID, coupon.ID)
		return err
	})
	if !postgres.IsCheckViolation(err) {
		t.Errorf("o banco aceitou resgates acima do limite: %v", err)
	}

	// O desconto vale somente nas faturas das assinaturas contempladas.
	a.runEngine(t, tn, contenders)
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM invoices WHERE tenant_id = $1 AND discount_total = 1500 AND total = 8500`, tn.ID); n != limit {
		t.Errorf("faturas com desconto = %d, esperado %d", n, limit)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM invoices WHERE tenant_id = $1 AND discount_total = 0 AND total = 10000`, tn.ID); n != contenders-limit {
		t.Errorf("faturas sem desconto = %d, esperado %d", n, contenders-limit)
	}
}

// Nota de crédito: a mesma chave (inclusive em pedidos concorrentes) devolve
// a mesma nota; pedidos distintos nunca ultrapassam o saldo em aberto; a
// fatura não é reescrita (somente amount_credited e o saldo mudam) e a nota
// é imutável no banco.
func TestCreditNotesIdempotentAndBoundedByBalance(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 12)
	tn := a.NewTenant(t, "adv-nota")
	plan := a.newFlatPlan(t, tn, 10000)
	sub := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
	other := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
	a.runEngine(t, tn, 2)
	inv := a.invoicesOf(t, tn.ID, sub.ID)[0]
	otherInv := a.invoicesOf(t, tn.ID, other.ID)[0]
	issue := func(invoiceID uuid.UUID, amount int64, key string) (billing.CreditNote, error) {
		return a.CreditNotes.Issue(ctx, tn.ID, tn.Actor, &tn.Owner.ID, invoiceID, brlCents(amount), "Ajuste comercial acordado", key)
	}
	due := func() invoiceSummary {
		t.Helper()
		for _, i := range a.invoicesOf(t, tn.ID, sub.ID) {
			if i.ID == inv.ID {
				return i
			}
		}
		t.Fatal("fatura não encontrada")
		return invoiceSummary{}
	}

	// Pedidos concorrentes com a mesma chave: uma única nota.
	const retries = 6
	notes := make([]billing.CreditNote, retries)
	errs := make([]error, retries)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range retries {
		wg.Go(func() {
			<-start
			notes[i], errs[i] = issue(inv.ID, 3000, "nota-repetida")
		})
	}
	close(start)
	wg.Wait()
	for i := range retries {
		if errs[i] != nil || notes[i].ID != notes[0].ID {
			t.Fatalf("pedido %d: nota %s erro %v (esperado %s)", i, notes[i].ID, errs[i], notes[0].ID)
		}
	}
	if notes[0].Number != inv.Number+"-NC01" || notes[0].Amount.Amount() != 3000 {
		t.Errorf("nota emitida: %+v", notes[0])
	}
	again, err := issue(inv.ID, 3000, "nota-repetida")
	if err != nil || again.ID != notes[0].ID {
		t.Errorf("repetição sequencial: %s %v", again.ID, err)
	}
	if d := due(); d.AmountDue != 7000 || d.AmountCredited != 3000 {
		t.Errorf("após a nota idempotente: saldo %d creditado %d", d.AmountDue, d.AmountCredited)
	}
	if _, err := issue(otherInv.ID, 3000, "nota-repetida"); errCode(err) != "request_key_reused" {
		t.Errorf("chave reutilizada em outra fatura: %v", err)
	}

	// Pedidos concorrentes com chaves distintas: limitados ao saldo (7.000).
	const parallel = 4
	errs = make([]error, parallel)
	start = make(chan struct{})
	for i := range parallel {
		wg.Go(func() {
			<-start
			_, errs[i] = issue(inv.ID, 3000, fmt.Sprintf("nota-saldo-%d", i))
		})
	}
	close(start)
	wg.Wait()
	issued, exceeded := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			issued++
		case errCode(err) == "credit_note_exceeds_due":
			exceeded++
		default:
			t.Errorf("pedido %d: erro inesperado %v", i, err)
		}
	}
	if issued != 2 || exceeded != 2 {
		t.Fatalf("notas emitidas = %d, recusadas = %d; esperado 2 e 2", issued, exceeded)
	}
	if d := due(); d.AmountDue != 1000 {
		t.Errorf("saldo após as notas concorrentes = %d, esperado 1000", d.AmountDue)
	}
	if _, err := issue(inv.ID, 1001, "nota-excedente"); errCode(err) != "credit_note_exceeds_due" {
		t.Errorf("nota acima do saldo: %v", err)
	}
	final, err := issue(inv.ID, 1000, "nota-final")
	if err != nil {
		t.Fatal(err)
	}
	if final.Number != inv.Number+"-NC04" {
		t.Errorf("numeração da última nota = %s", final.Number)
	}
	if _, err := issue(inv.ID, 1, "nota-apos-quitacao"); errCode(err) != "invoice_not_open" {
		t.Errorf("nota em fatura quitada: %v", err)
	}

	d := due()
	if d.Status != string(billing.Paid) || d.AmountDue != 0 || d.AmountCredited != 10000 || d.Subtotal != 10000 || d.Total != 10000 {
		t.Errorf("fatura após as notas: %+v", d)
	}
	list, err := a.Store.Billing().CreditNotes(ctx, tn.ID, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	numbers := map[string]bool{}
	for _, n := range list {
		sum += n.Amount.Amount()
		numbers[n.Number] = true
	}
	if len(list) != 4 || len(numbers) != 4 || sum != 10000 {
		t.Errorf("notas: %d, números distintos %d, soma %d", len(list), len(numbers), sum)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM credit_notes WHERE tenant_id = $1 AND request_key IN ('nota-excedente', 'nota-apos-quitacao')`,
		tn.ID); n != 0 {
		t.Errorf("notas recusadas persistidas: %d", n)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND kind = 'credit_note.issued'`, tn.ID); n != 4 {
		t.Errorf("lançamentos de nota de crédito = %d, esperado 4", n)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT COALESCE(sum(CASE WHEN e.direction = 'debit' THEN e.amount ELSE -e.amount END), 0)::int
		FROM ledger_entries e JOIN ledger_accounts acc ON acc.id = e.account_id WHERE e.tenant_id = $1 AND acc.code = 'accounts_receivable'`, tn.ID); n != 10000 {
		t.Errorf("contas a receber = %d, esperado 10000 (somente a outra fatura)", n)
	}
	for _, ev := range []struct {
		eventType string
		want      int
	}{{outbox.EventInvoiceCredited, 4}, {outbox.EventInvoicePaid, 1}} {
		if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2 AND aggregate_id = $3`,
			tn.ID, ev.eventType, inv.ID); n != ev.want {
			t.Errorf("eventos %s = %d, esperado %d", ev.eventType, n, ev.want)
		}
	}

	err = a.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE credit_notes SET amount = 1 WHERE tenant_id = $1`, tn.ID)
		return err
	})
	if !postgres.IsInsufficientPrivilege(err) {
		t.Errorf("papel da aplicação alterou nota de crédito: %v", err)
	}
	for _, stmt := range []string{`UPDATE credit_notes SET amount = 1 WHERE tenant_id = $1`, `DELETE FROM credit_notes WHERE tenant_id = $1`} {
		if _, err := pgtest.OwnerPool(t).Exec(ctx, stmt, tn.ID); !postgres.IsRaisedException(err) {
			t.Errorf("gatilho de imutabilidade não rejeitou %q: %v", stmt, err)
		}
	}
}

// Consumo: deduplicação na ingestão, fechamento do período pelo motor com
// vínculo até o item faturado, eventos atrasados dentro da tolerância e
// correção pelo UsageCorrector como item pendente da próxima fatura, sem
// alterar a fatura emitida (protegida por gatilho). Preço de 2,5 centavos por
// unidade: 1.234 unidades = 3.085; com 99,8 atrasadas, 1.333,8 unidades =
// 3.334,5, arredondado half-even para 3.334 (diferença de 249).
func TestLateUsageCorrectedWithoutRewritingIssuedInvoice(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-consumo")
	metric, plan := a.newMeteredPlan(t, tn, catalog.AggSum, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "2.5"})
	sub := a.newSubscriptionQty(t, tn, plan.ID, 1)
	// Período 0 sem componente fixo: nada a faturar no início.
	a.runEngine(t, tn, 0)
	event := func(id, qty string, at time.Time) metering.EventInput {
		return metering.EventInput{ExternalID: id, SubscriptionID: sub.ID, MetricCode: metric.Code, Unit: metric.Unit, Quantity: qty, OccurredAt: at}
	}
	period0 := spTime(t, 2026, time.March, 10, 0)
	period1 := spTime(t, 2026, time.April, 10, 0)
	period2 := spTime(t, 2026, time.May, 10, 0)

	a.Clock.Set(spTime(t, 2026, time.April, 9, 20))
	wantOutcomes(t, a.ingest(t, tn,
		event("evt-1", "1000", spTime(t, 2026, time.March, 15, 10)),
		event("evt-2", "234", spTime(t, 2026, time.April, 9, 18)),
		event("evt-1", "1000", spTime(t, 2026, time.March, 15, 10)),
	), metering.OutcomeAccepted, metering.OutcomeAccepted, metering.OutcomeDuplicate)
	wantOutcomes(t, a.ingest(t, tn, event("evt-1", "1000", spTime(t, 2026, time.March, 15, 10))), metering.OutcomeDuplicate)
	wantOutcomes(t, a.ingest(t, tn, event("evt-1", "999", spTime(t, 2026, time.March, 15, 10))), metering.OutcomeRejected)

	// Fechamento do período 0 no início do período 1.
	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	a.runEngine(t, tn, 1)
	inv := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, period1)
	items := a.itemsOf(t, tn.ID, inv.ID)
	if len(items) != 1 || items[0].Kind != string(billing.ItemUsage) || items[0].Amount != 3085 || !ratEqual(t, items[0].Quantity, "1234") {
		t.Fatalf("itens do fechamento: %+v", items)
	}
	agg := a.aggregateOf(t, tn.ID, sub.ID, period0)
	if !ratEqual(t, agg.Quantity, "1234") || agg.EventCount != 2 || agg.InvoiceItemID == nil || *agg.InvoiceItemID != items[0].ID ||
		agg.PriceVersionID == nil || *agg.PriceVersionID != *plan.CurrentPriceVersionID || agg.CorrectedAmount != nil {
		t.Fatalf("agregado fechado: %+v", agg)
	}
	if items[0].UsageAggregateID == nil || *items[0].UsageAggregateID != agg.ID {
		t.Errorf("item sem vínculo com o agregado: %+v", items[0])
	}
	for _, id := range []string{"evt-1", "evt-2"} {
		if ev := a.usageEvent(t, tn.ID, id); ev.Status != string(metering.Accepted) || ev.AggregateID == nil || *ev.AggregateID != agg.ID {
			t.Errorf("%s: %+v", id, ev)
		}
	}

	// Eventos atrasados do período 0, dentro da tolerância de 72 horas.
	wantOutcomes(t, a.ingest(t, tn,
		event("evt-3", "99", spTime(t, 2026, time.April, 9, 23)),
		event("evt-4", "0.8", spTime(t, 2026, time.April, 1, 8)),
	), metering.OutcomeLate, metering.OutcomeLate)
	before := a.invoicesOf(t, tn.ID, sub.ID)

	n, err := a.Corrector.RunAll(ctx)
	if err != nil || n < 1 {
		t.Fatalf("correção: %d grupos, %v", n, err)
	}
	pending := a.pendingItemsOf(t, tn.ID, sub.ID)
	if len(pending) != 1 || pending[0].Kind != string(billing.ItemAdjustment) || pending[0].Source != "usage_correction" || pending[0].Amount != 249 ||
		pending[0].UsageAggregateID == nil || *pending[0].UsageAggregateID != agg.ID || pending[0].InvoiceID != nil {
		t.Fatalf("itens pendentes: %+v", pending)
	}
	for _, id := range []string{"evt-3", "evt-4"} {
		ev := a.usageEvent(t, tn.ID, id)
		if ev.Status != string(metering.Late) || ev.CorrectedAt == nil || ev.CorrectionItemID == nil || *ev.CorrectionItemID != pending[0].ID ||
			ev.AggregateID == nil || *ev.AggregateID != agg.ID {
			t.Errorf("%s após a correção: %+v", id, ev)
		}
	}
	corrected := a.aggregateOf(t, tn.ID, sub.ID, period0)
	if !ratEqual(t, corrected.Quantity, "1234") || corrected.InvoiceItemID == nil || *corrected.InvoiceItemID != items[0].ID ||
		corrected.CorrectedQuantity == nil || !ratEqual(t, *corrected.CorrectedQuantity, "1333.8") || corrected.CorrectedAmount == nil ||
		*corrected.CorrectedAmount != 3334 {
		t.Errorf("agregado corrigido: %+v", corrected)
	}
	if after := a.invoicesOf(t, tn.ID, sub.ID); len(after) != len(before) || after[0] != before[0] {
		t.Errorf("a correção alterou a fatura emitida: antes %+v depois %+v", before, after)
	}
	if got := a.itemsOf(t, tn.ID, inv.ID); len(got) != 1 || got[0].Amount != 3085 {
		t.Errorf("itens da fatura emitida após a correção: %+v", got)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'usage.corrected'
		AND details->>'outcome' = 'next_invoice' AND (details->>'delta')::int = 249`, tn.ID); n != 1 {
		t.Errorf("auditoria da correção = %d, esperado 1", n)
	}
	// Nova execução sem eventos pendentes não gera ajuste.
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := a.pendingItemsOf(t, tn.ID, sub.ID); len(got) != 1 {
		t.Errorf("itens pendentes após nova execução = %d, esperado 1", len(got))
	}

	// Gatilho de proteção: a fatura emitida e seus itens não são reescritos.
	err = a.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE invoices SET subtotal = subtotal + 249, total = total + 249, amount_due = amount_due + 249
			WHERE tenant_id = $1 AND id = $2`, tn.ID, inv.ID)
		return err
	})
	if !postgres.IsRaisedException(err) {
		t.Errorf("o banco aceitou reescrever a fatura emitida: %v", err)
	}
	err = a.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE invoice_items SET amount = amount + 249 WHERE tenant_id = $1 AND invoice_id = $2`, tn.ID, inv.ID)
		return err
	})
	if !postgres.IsInsufficientPrivilege(err) {
		t.Errorf("o papel da aplicação alterou item faturado: %v", err)
	}
	if _, err := pgtest.OwnerPool(t).Exec(ctx, `UPDATE invoice_items SET amount = amount + 249 WHERE tenant_id = $1 AND invoice_id = $2`,
		tn.ID, inv.ID); !postgres.IsRaisedException(err) {
		t.Errorf("gatilho de imutabilidade dos itens não rejeitou a alteração: %v", err)
	}

	// Segunda correção acumula sobre a anterior: 1.335,8 unidades = 3.339,5,
	// arredondado para 3.340; diferença de 6 sobre os 3.334 já corrigidos.
	wantOutcomes(t, a.ingest(t, tn, event("evt-5", "2", spTime(t, 2026, time.April, 8, 12))), metering.OutcomeLate)
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	pending = a.pendingItemsOf(t, tn.ID, sub.ID)
	if len(pending) != 2 || pending[1].Amount != 6 {
		t.Fatalf("itens pendentes após a segunda correção: %+v", pending)
	}
	if c := a.aggregateOf(t, tn.ID, sub.ID, period0); c.CorrectedAmount == nil || *c.CorrectedAmount != 3340 || !ratEqual(t, *c.CorrectedQuantity, "1335.8") {
		t.Errorf("agregado após a segunda correção: %+v", c)
	}

	// Depois da tolerância, o evento atrasado é rejeitado.
	a.Clock.Set(spTime(t, 2026, time.April, 13, 9))
	wantOutcomes(t, a.ingest(t, tn, event("evt-6", "5", spTime(t, 2026, time.April, 9, 12))), metering.OutcomeRejected)

	// Consumo do período 1 e fechamento: a próxima fatura inclui os ajustes.
	a.Clock.Set(spTime(t, 2026, time.April, 25, 9))
	wantOutcomes(t, a.ingest(t, tn, event("evt-7", "10", spTime(t, 2026, time.April, 20, 15))), metering.OutcomeAccepted)
	a.Clock.Set(spTime(t, 2026, time.May, 10, 9))
	a.runEngine(t, tn, 1)
	next := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, period2)
	if next.Total != 25+249+6 {
		t.Errorf("total da fatura seguinte = %d, esperado %d", next.Total, 25+249+6)
	}
	var adjustments, usage int64
	for _, it := range a.itemsOf(t, tn.ID, next.ID) {
		switch billing.ItemKind(it.Kind) {
		case billing.ItemAdjustment:
			adjustments += it.Amount
			if !strings.Contains(it.Description, "Correção de consumo") {
				t.Errorf("descrição do ajuste: %q", it.Description)
			}
		case billing.ItemUsage:
			usage += it.Amount
		}
	}
	if adjustments != 255 || usage != 25 {
		t.Errorf("ajustes = %d, consumo = %d; esperado 255 e 25", adjustments, usage)
	}
	for _, p := range a.pendingItemsOf(t, tn.ID, sub.ID) {
		if p.InvoiceID == nil || *p.InvoiceID != next.ID {
			t.Errorf("item pendente não vinculado à fatura seguinte: %+v", p)
		}
	}
	if a.invoicesOf(t, tn.ID, sub.ID)[0] != before[0] {
		t.Errorf("fatura do fechamento alterada após o ciclo seguinte")
	}
}

// Métrica "last": evento atrasado mais recente com quantidade menor reduz o
// valor do período; a diferença vira crédito do cliente (sem item pendente),
// consumido na fatura seguinte.
func TestLateUsageReducingAmountBecomesCustomerCredit(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "adv-consumo-credito")
	metric, plan := a.newMeteredPlan(t, tn, catalog.AggLast, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "100"})
	sub := a.newSubscriptionQty(t, tn, plan.ID, 1)
	a.runEngine(t, tn, 0)
	event := func(id, qty string, at time.Time) metering.EventInput {
		return metering.EventInput{ExternalID: id, SubscriptionID: sub.ID, MetricCode: metric.Code, Quantity: qty, OccurredAt: at}
	}
	a.Clock.Set(spTime(t, 2026, time.April, 6, 9))
	wantOutcomes(t, a.ingest(t, tn,
		event("arm-1", "50", spTime(t, 2026, time.March, 20, 10)),
		event("arm-2", "40", spTime(t, 2026, time.April, 5, 10)),
	), metering.OutcomeAccepted, metering.OutcomeAccepted)
	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))
	a.runEngine(t, tn, 1)
	closing := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, spTime(t, 2026, time.April, 10, 0))
	if closing.Total != 4000 {
		t.Fatalf("fechamento com o último valor (40) = %d, esperado 4000", closing.Total)
	}
	wantOutcomes(t, a.ingest(t, tn, event("arm-3", "30", spTime(t, 2026, time.April, 9, 22))), metering.OutcomeLate)
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := a.pendingItemsOf(t, tn.ID, sub.ID); len(got) != 0 {
		t.Errorf("correção negativa gerou item pendente: %+v", got)
	}
	if bal := a.creditBalance(t, tn.ID, sub.CustomerID); bal != 1000 {
		t.Errorf("crédito do cliente = %d, esperado 1000", bal)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM customer_credit_entries WHERE tenant_id = $1 AND customer_id = $2
		AND kind = 'usage_correction' AND amount = 1000`, tn.ID, sub.CustomerID); n != 1 {
		t.Errorf("movimento de crédito da correção = %d, esperado 1", n)
	}
	if n := countRows(t, a.Env, tn.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND kind = 'customer_credit.granted'`, tn.ID); n != 1 {
		t.Errorf("lançamentos de crédito concedido = %d, esperado 1", n)
	}
	if c := a.aggregateOf(t, tn.ID, sub.ID, spTime(t, 2026, time.March, 10, 0)); c.CorrectedAmount == nil || *c.CorrectedAmount != 3000 ||
		!ratEqual(t, c.Quantity, "40") {
		t.Errorf("agregado corrigido: %+v", c)
	}
	if after := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, closing.PeriodStart); after != closing {
		t.Errorf("a correção alterou a fatura emitida: %+v", after)
	}

	a.Clock.Set(spTime(t, 2026, time.April, 25, 9))
	wantOutcomes(t, a.ingest(t, tn, event("arm-4", "25", spTime(t, 2026, time.April, 20, 10))), metering.OutcomeAccepted)
	a.Clock.Set(spTime(t, 2026, time.May, 10, 9))
	a.runEngine(t, tn, 1)
	next := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle, spTime(t, 2026, time.May, 10, 0))
	if next.Total != 2500 || next.CreditApplied != 1000 || next.AmountDue != 1500 {
		t.Errorf("fatura seguinte: total %d crédito %d saldo %d", next.Total, next.CreditApplied, next.AmountDue)
	}
}

// Evento de consumo recebido enquanto o motor fecha o período a que ele
// pertence: o motor reivindica a assinatura, agrega o consumo e fica
// aguardando a numeração da fatura (bloqueada pelo teste, como ocorre quando
// outra instância do motor emite fatura no mesmo tenant); a ingestão do
// evento começa nesse intervalo. Depois de tudo confirmado e da correção de
// atrasados, o evento precisa estar faturado (no agregado ou corrigido).
func TestUsageIngestedWhileEngineClosesPeriodIsBilled(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 8)
	tn := a.NewTenant(t, "adv-consumo-corrida")
	metric, plan := a.newMeteredPlan(t, tn, catalog.AggSum, catalog.PriceInput{Model: catalog.FlatPlusOverage, FlatAmount: new(int64(5000)),
		UnitAmount: "2", IncludedQuantity: "0"})
	sub := a.newSubscriptionQty(t, tn, plan.ID, 1)
	a.runEngine(t, tn, 1)
	event := func(id, qty string, at time.Time) metering.EventInput {
		return metering.EventInput{ExternalID: id, SubscriptionID: sub.ID, MetricCode: metric.Code, Quantity: qty, OccurredAt: at}
	}
	a.Clock.Set(spTime(t, 2026, time.April, 5, 9))
	wantOutcomes(t, a.ingest(t, tn, event("corrida-1", "100", spTime(t, 2026, time.March, 20, 10))), metering.OutcomeAccepted)
	a.Clock.Set(spTime(t, 2026, time.April, 10, 9))

	owner := pgtest.OwnerPool(t)
	blocker, err := a.DB.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
	if _, err := blocker.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tn.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SELECT next_number FROM invoice_number_sequences WHERE tenant_id = $1 FOR UPDATE`, tn.ID); err != nil {
		t.Fatal(err)
	}
	engineDone := make(chan error, 1)
	go func() {
		_, err := a.Engine.RunTenant(ctx, tn.ID, tn.Actor)
		engineDone <- err
	}()
	waitLockWaiters(t, owner, 1, nil)

	type ingestOutcome struct {
		res []metering.Result
		err error
	}
	concurrent := event("corrida-2", "50", spTime(t, 2026, time.April, 9, 20))
	ingestResult := make(chan ingestOutcome, 1)
	ingestFinished := make(chan struct{})
	go func() {
		defer close(ingestFinished)
		res, err := a.Metering.Ingest(ctx, tn.ID, []metering.EventInput{concurrent})
		ingestResult <- ingestOutcome{res, err}
	}()
	// A ingestão termina sozinha ou fica aguardando a transação do motor.
	waitLockWaiters(t, owner, 2, ingestFinished)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-engineDone; err != nil {
		t.Fatalf("motor: %v", err)
	}
	out := <-ingestResult
	if out.err != nil {
		t.Fatalf("ingestão: %v", out.err)
	}
	// A leitura da assinatura com bloqueio compartilhado espera o commit do
	// motor; a verificação seguinte já enxerga o período fechado.
	if len(out.res) != 1 || out.res[0].Outcome != metering.OutcomeLate {
		t.Fatalf("evento concorrente ao fechamento: %+v, esperado late", out.res)
	}
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatal(err)
	}

	period0 := spTime(t, 2026, time.March, 10, 0)
	agg := a.aggregateOf(t, tn.ID, sub.ID, period0)
	billed := int64(0)
	if agg.InvoiceItemID != nil {
		for _, it := range a.itemsOf(t, tn.ID, findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindSubscriptionCycle,
			spTime(t, 2026, time.April, 10, 0)).ID) {
			if it.UsageAggregateID != nil && *it.UsageAggregateID == agg.ID {
				billed += it.Amount
			}
		}
	}
	for _, p := range a.pendingItemsOf(t, tn.ID, sub.ID) {
		if p.UsageAggregateID != nil && *p.UsageAggregateID == agg.ID {
			billed += p.Amount
		}
	}
	ev := a.usageEvent(t, tn.ID, "corrida-2")
	if billed != 300 || ev.AggregateID == nil {
		t.Errorf("consumo do período faturado = %d (esperado 300); evento %s (%s): %+v", billed, out.res[0].Outcome, out.res[0].Error, ev)
	}
}
