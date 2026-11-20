//go:build integration

package pgstore_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

func suspendAfter(d int) *int { return &d }

func defaultPolicyInput() collections.PolicyInput {
	return collections.PolicyInput{Name: "Régua padrão", OffsetsDays: []int{-3, 0, 3, 7}, WindowStartHour: 8, WindowEndHour: 20,
		MaxMessagesPerDay: 2, SuspendAfterDays: suspendAfter(10), ReactivateOnPayment: true}
}

func subscriptionStatus(t *testing.T, env *fixture.Env, tenantID, subID uuid.UUID) string {
	t.Helper()
	var s string
	err := env.DB.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT status FROM subscriptions WHERE tenant_id = $1 AND id = $2`, tenantID, subID).Scan(&s)
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Régua completa: lembrete antes do vencimento, marcação de inadimplência
// (past_due e invoice.overdue), suspensão após N dias com aviso, e
// reativação após o pagamento, com interrupção dos lembretes.
func TestCollectionsLifecycleFromReminderToReactivation(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := env.NewTenant(t, "regua")
	svc := collections.NewService(env.Store.Collections(), env.Clock, nil)
	policy, err := svc.CreatePolicy(ctx, tn.ID, tn.Actor, &tn.Owner.ID, defaultPolicyInput())
	if err != nil {
		t.Fatal(err)
	}
	if !policy.IsDefault {
		t.Fatal("a primeira política é a padrão")
	}
	c, invoiceID := issuedInvoice(t, env, tn, "regua@cliente.test") // emitida em 10/03, vence em 15/03
	var subID uuid.UUID
	if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT subscription_id FROM invoices WHERE tenant_id = $1 AND id = $2`, tn.ID, invoiceID).Scan(&subID)
	}); err != nil {
		t.Fatal(err)
	}

	env.Clock.Set(time.Date(2026, 3, 12, 15, 0, 0, 0, time.UTC)) // passo -3
	if _, err := svc.RunTenant(ctx, tn.ID); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM notifications WHERE tenant_id = $1 AND business_key = $2 AND template_key = 'reminder_before_due'`,
		tn.ID, notifications.ReminderKey(invoiceID, -3)); n != 1 {
		t.Fatal("lembrete antes do vencimento não agendado")
	}

	env.Clock.Set(time.Date(2026, 3, 16, 15, 0, 0, 0, time.UTC)) // vencida desde o início de 16/03
	if _, err := svc.RunTenant(ctx, tn.ID); err != nil {
		t.Fatal(err)
	}
	if s := subscriptionStatus(t, env, tn.ID, subID); s != "past_due" {
		t.Fatalf("assinatura deveria estar past_due: %s", s)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM invoice_delinquencies WHERE tenant_id = $1 AND invoice_id = $2 AND resolved_at IS NULL`,
		tn.ID, invoiceID); n != 1 {
		t.Fatal("inadimplência não registrada")
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, tn.ID, outbox.EventInvoiceOverdue); n != 1 {
		t.Fatalf("invoice.overdue emitido %d vezes", n)
	}

	env.Clock.Set(time.Date(2026, 3, 26, 15, 0, 0, 0, time.UTC)) // 11 dias de atraso
	res, err := svc.RunTenant(ctx, tn.ID)
	if err != nil || res.Suspended != 1 {
		t.Fatalf("suspensão: %+v %v", res, err)
	}
	if s := subscriptionStatus(t, env, tn.ID, subID); s != "suspended" {
		t.Fatalf("assinatura deveria estar suspensa: %s", s)
	}
	susp, err := env.Store.Collections().Suspensions(ctx, tn.ID, subID)
	if err != nil || len(susp) != 1 || susp[0].Status != collections.SuspensionActive || susp[0].DaysOverdue != 11 || susp[0].PolicyID != policy.ID {
		t.Fatalf("registro da suspensão: %+v %v", susp, err)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM subscription_history WHERE tenant_id = $1 AND subscription_id = $2 AND event = 'suspended'
		AND details->>'origin' = 'collection_policy'`, tn.ID, subID); n != 1 {
		t.Fatal("histórico da suspensão sem origem")
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM notifications WHERE tenant_id = $1 AND template_key = 'subscription_suspended'`, tn.ID); n != 1 {
		t.Fatal("aviso de suspensão não agendado")
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'subscription.suspended' AND actor_type = 'system'`, tn.ID); n != 1 {
		t.Fatal("suspensão automática auditada")
	}

	// Pagamento integral: o consumidor da fatura paga reativa e interrompe a régua.
	pay := env.Payments(nil, payments.Config{})
	if _, err := pay.RecordManualPayment(ctx, tn.ID, tn.Actor, &tn.Owner.ID, payments.ManualPaymentInput{InvoiceID: invoiceID,
		Amount: money.New(99000, money.BRL), PaidAt: env.Clock.Now(), Method: "pix", RequestKey: "regua-" + ids.New().String()}); err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleInvoiceResolved(ctx, tn.ID, invoiceID, ids.New()); err != nil {
		t.Fatal(err)
	}
	if s := subscriptionStatus(t, env, tn.ID, subID); s != "active" {
		t.Fatalf("assinatura deveria ser reativada: %s", s)
	}
	susp, _ = env.Store.Collections().Suspensions(ctx, tn.ID, subID)
	if susp[0].Status != collections.SuspensionReactivated || susp[0].EndReason != collections.EndPaymentRegularized {
		t.Fatalf("suspensão encerrada: %+v", susp[0])
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM notifications WHERE tenant_id = $1 AND invoice_id = $2 AND policy_id IS NOT NULL
		AND status = 'scheduled'`, tn.ID, invoiceID); n != 0 {
		t.Fatalf("lembretes agendados após o pagamento: %d", n)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM invoice_delinquencies WHERE tenant_id = $1 AND resolution = 'paid'`, tn.ID); n != 1 {
		t.Fatal("inadimplência resolvida como paga")
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, tn.ID,
		outbox.EventSubscriptionReactivate); n != 1 {
		t.Fatal("subscription.reactivated emitido")
	}
	_ = c
}

// Execuções concorrentes da régua (várias instâncias do worker) não
// duplicam lembretes, inadimplências nem suspensões e não entram em impasse.
func TestCollectionsConcurrentRunsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 16, now)
	tn := env.NewTenant(t, "regua-conc")
	svc := collections.NewService(env.Store.Collections(), env.Clock, nil)
	if _, err := svc.CreatePolicy(ctx, tn.ID, tn.Actor, nil, defaultPolicyInput()); err != nil {
		t.Fatal(err)
	}
	const customers = 4
	plan := env.NewFlatPlan(t, tn, 99000)
	for range customers {
		issuedInvoiceOnPlan(t, env, tn, plan.ID, "")
	}
	env.Clock.Set(time.Date(2026, 3, 27, 15, 0, 0, 0, time.UTC))
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			if _, err := svc.RunTenant(ctx, tn.ID); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("execução concorrente falhou: %v", err)
	}
	checks := map[string]int{
		`SELECT count(*) FROM collection_suspensions WHERE tenant_id = $1 AND status = 'active'`:                              customers,
		`SELECT count(*) FROM subscriptions WHERE tenant_id = $1 AND status = 'suspended'`:                                    customers,
		`SELECT count(*) FROM invoice_delinquencies WHERE tenant_id = $1`:                                                     customers,
		`SELECT count(*) FROM notifications WHERE tenant_id = $1 AND template_key = 'reminder_after_due'`:                     customers,
		`SELECT count(*) FROM notifications WHERE tenant_id = $1 AND template_key = 'subscription_suspended'`:                 customers,
		`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'invoice.overdue'`:                          customers,
		`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'subscription.suspended'`:                   customers,
		`SELECT count(*) FROM subscription_history WHERE tenant_id = $1 AND event = 'suspended'`:                              customers,
		`SELECT count(DISTINCT subscription_id) FROM collection_suspensions WHERE tenant_id = $1`:                             customers,
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'subscription.suspended'`:                           customers,
		`SELECT count(*) FROM subscription_history WHERE tenant_id = $1 AND event = 'past_due'`:                               customers,
		`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'subscription.updated'`:                     customers,
		`SELECT count(*) FROM notification_events WHERE tenant_id = $1 AND reason = 'scheduled'`:                              2 * customers,
		`SELECT count(*) FROM notifications WHERE tenant_id = $1 AND business_key LIKE 'reminder:%' AND status = 'scheduled'`: customers,
	}
	for q, want := range checks {
		if n := countRows(t, env, tn.ID, q, tn.ID); n != want {
			t.Errorf("%s = %d, esperado %d", q, n, want)
		}
	}
	// A restrição de suspensão ativa única protege também gravações diretas.
	err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO collection_suspensions (id, tenant_id, subscription_id, policy_id, invoice_id, days_overdue, status, suspended_at)
			SELECT $1, tenant_id, subscription_id, policy_id, invoice_id, days_overdue, 'active', now() FROM collection_suspensions WHERE tenant_id = $2 LIMIT 1`,
			ids.New(), tn.ID)
		return err
	})
	if !postgres.IsUniqueViolation(err, "collection_suspensions_active_key") {
		t.Fatalf("esperava violação da suspensão ativa única: %v", err)
	}
}

// Políticas: nome único por tenant, uma padrão por tenant e If-Match.
func TestCollectionPolicyConstraints(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "regua-pol")
	svc := collections.NewService(env.Store.Collections(), env.Clock, nil)
	first, err := svc.CreatePolicy(ctx, tn.ID, tn.Actor, nil, defaultPolicyInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreatePolicy(ctx, tn.ID, tn.Actor, nil, defaultPolicyInput()); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("nome repetido: %v", err)
	}
	in := defaultPolicyInput()
	in.Name, in.IsDefault = "Agressiva", true
	second, err := svc.CreatePolicy(ctx, tn.ID, tn.Actor, nil, in)
	if err != nil || !second.IsDefault {
		t.Fatalf("nova padrão: %+v %v", second, err)
	}
	list, err := svc.Policies(ctx, tn.ID)
	if err != nil || len(list) != 2 || !list[0].IsDefault || list[0].ID != second.ID {
		t.Fatalf("listagem (padrão primeiro): %+v %v", list, err)
	}
	if _, err := svc.UpdatePolicy(ctx, tn.ID, tn.Actor, first.ID, in, first.Version); !apperr.IsKind(err, apperr.KindPreconditionFailed) {
		t.Fatalf("a política anterior mudou de versão ao perder o padrão: %v", err)
	}
	upd := defaultPolicyInput()
	upd.Name = "Agressiva"
	if _, err := svc.UpdatePolicy(ctx, tn.ID, tn.Actor, list[1].ID, upd, list[1].Version); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("renomear para nome existente: %v", err)
	}
	other := env.NewTenant(t, "regua-outra")
	if _, err := svc.UpdatePolicy(ctx, other.ID, other.Actor, first.ID, upd, 0); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("política de outro tenant: %v", err)
	}
	if _, err := svc.CreatePolicy(ctx, other.ID, other.Actor, nil, defaultPolicyInput()); err != nil {
		t.Fatalf("mesmo nome em outro tenant é permitido: %v", err)
	}
	err = env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE collection_policies SET is_default = true WHERE tenant_id = $1`, tn.ID)
		return err
	})
	if !postgres.IsUniqueViolation(err, "collection_policies_default_key") {
		t.Fatalf("duas políticas padrão: %v", err)
	}
	err = env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE collection_policies SET send_window_start_hour = 20, send_window_end_hour = 8 WHERE tenant_id = $1`, tn.ID)
		return err
	})
	if !postgres.IsCheckViolation(err) {
		t.Fatalf("janela invertida no banco: %v", err)
	}
}

func invoiceSubscription(t *testing.T, env *fixture.Env, tenantID, invoiceID uuid.UUID) uuid.UUID {
	t.Helper()
	var subID uuid.UUID
	if err := env.DB.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT subscription_id FROM invoices WHERE tenant_id = $1 AND id = $2`, tenantID, invoiceID).Scan(&subID)
	}); err != nil {
		t.Fatal(err)
	}
	return subID
}

// walkTenants percorre a descoberta paginada e confere a ordem crescente.
func walkTenants(t *testing.T, page func(after *uuid.UUID, limit int) ([]uuid.UUID, error)) []uuid.UUID {
	t.Helper()
	var all []uuid.UUID
	var after *uuid.UUID
	for {
		ids, err := page(after, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if n := len(all); n > 0 && bytes.Compare(all[n-1][:], id[:]) >= 0 {
				t.Fatalf("descoberta fora de ordem: %s depois de %s", id, all[n-1])
			}
			all = append(all, id)
		}
		if len(ids) < 2 {
			return all
		}
		after = &ids[len(ids)-1]
	}
}

// Fatura vencida de assinatura cancelada (com a última fatura em aberto) é
// marcada como inadimplente e gera invoice.overdue uma única vez.
func TestCollectionsMarksOverdueInvoiceOfCanceledSubscription(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := env.NewTenant(t, "regua-cancelada")
	svc := collections.NewService(env.Store.Collections(), env.Clock, nil)
	if _, err := svc.CreatePolicy(ctx, tn.ID, tn.Actor, nil, defaultPolicyInput()); err != nil {
		t.Fatal(err)
	}
	_, invoiceID := issuedInvoice(t, env, tn, "cancelada@cliente.test") // vence em 15/03
	subID := invoiceSubscription(t, env, tn.ID, invoiceID)
	env.Clock.Set(time.Date(2026, 3, 12, 15, 0, 0, 0, time.UTC))
	if _, err := env.Subscriptions.Cancel(ctx, tn.ID, tn.Actor, subID, subscriptions.CancelImmediately, "encerramento do contrato", 0); err != nil {
		t.Fatal(err)
	}
	for _, day := range []int{16, 20, 30} {
		env.Clock.Set(time.Date(2026, 3, day, 15, 0, 0, 0, time.UTC))
		if _, err := svc.RunTenant(ctx, tn.ID); err != nil {
			t.Fatal(err)
		}
	}
	if s := subscriptionStatus(t, env, tn.ID, subID); s != "canceled" {
		t.Fatalf("a assinatura cancelada não muda: %s", s)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM invoice_delinquencies WHERE tenant_id = $1 AND invoice_id = $2 AND subscription_id = $3`,
		tn.ID, invoiceID, subID); n != 1 {
		t.Fatalf("inadimplências registradas: %d", n)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, tn.ID, outbox.EventInvoiceOverdue); n != 1 {
		t.Fatalf("invoice.overdue emitido %d vezes", n)
	}
}

// Fatura vencida cancelada ou baixada: a assinatura em past_due volta a
// active pelo consumidor da fatura resolvida ou, sem ele, pela execução
// seguinte do job (a descoberta inclui o tenant com assinatura em past_due).
func TestCollectionsPastDueReturnsToActiveAfterVoidOrWriteOff(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	svc := collections.NewService(env.Store.Collections(), env.Clock, nil)
	cases := []struct {
		name     string
		close    func(tenant fixture.Tenant, invoiceID uuid.UUID) error
		consumer bool
	}{
		{"cancelada, pelo consumidor", func(tn fixture.Tenant, id uuid.UUID) error {
			_, err := env.Billing.Void(ctx, tn.ID, tn.Actor, id, "emitida por engano", 0)
			return err
		}, true},
		{"baixada, pelo job", func(tn fixture.Tenant, id uuid.UUID) error {
			_, err := env.Billing.MarkUncollectible(ctx, tn.ID, tn.Actor, id, "cliente encerrou as atividades", 0)
			return err
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env.Clock.Set(now)
			tn := env.NewTenant(t, "regua-pastdue")
			_, invoiceID := issuedInvoice(t, env, tn, "")
			subID := invoiceSubscription(t, env, tn.ID, invoiceID)
			env.Clock.Set(time.Date(2026, 3, 16, 15, 0, 0, 0, time.UTC))
			if _, err := svc.RunTenant(ctx, tn.ID); err != nil {
				t.Fatal(err)
			}
			if s := subscriptionStatus(t, env, tn.ID, subID); s != "past_due" {
				t.Fatalf("assinatura deveria estar past_due: %s", s)
			}
			if err := c.close(tn, invoiceID); err != nil {
				t.Fatal(err)
			}
			if c.consumer {
				if err := svc.HandleInvoiceResolved(ctx, tn.ID, invoiceID, ids.New()); err != nil {
					t.Fatal(err)
				}
			} else {
				horizon := env.Clock.Now().AddDate(0, 0, collections.ReminderHorizonDays)
				found := walkTenants(t, func(after *uuid.UUID, limit int) ([]uuid.UUID, error) {
					return env.Store.Collections().TenantsForCollections(ctx, horizon, after, limit)
				})
				if !slices.Contains(found, tn.ID) {
					t.Fatal("a descoberta deve incluir o tenant com assinatura em past_due")
				}
				if _, err := svc.RunTenant(ctx, tn.ID); err != nil {
					t.Fatal(err)
				}
			}
			if s := subscriptionStatus(t, env, tn.ID, subID); s != "active" {
				t.Fatalf("sem fatura vencida a assinatura volta a active: %s", s)
			}
			if n := countRows(t, env, tn.ID, `SELECT count(*) FROM subscription_history WHERE tenant_id = $1 AND subscription_id = $2
				AND event = 'payment_recovered' AND to_status = 'active'`, tn.ID, subID); n != 1 {
				t.Fatalf("histórico da regularização: %d", n)
			}
		})
	}
}

// advisoryWaiters conta as sessões do banco atual que aguardam um bloqueio
// consultivo.
func advisoryWaiters(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT count(DISTINCT pid)::int FROM pg_locks WHERE NOT granted AND locktype = 'advisory'
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n)
	return n, err
}

// pauseUntilPeer pausa a primeira transação num ponto da operação até a
// segunda chegar ao mesmo ponto (sem serialização) ou ficar aguardando o
// bloqueio consultivo (com serialização). Sem time.Sleep: a espera termina
// por um desses dois eventos.
type pauseUntilPeer struct {
	pool    *pgxpool.Pool
	mu      sync.Mutex
	arrived int
	both    chan struct{}
}

func newPauseUntilPeer(pool *pgxpool.Pool) *pauseUntilPeer {
	return &pauseUntilPeer{pool: pool, both: make(chan struct{})}
}

func (p *pauseUntilPeer) arrive(ctx context.Context) error {
	p.mu.Lock()
	p.arrived++
	n := p.arrived
	p.mu.Unlock()
	switch {
	case n == 2:
		close(p.both)
		return nil
	case n > 2:
		return nil
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-p.both:
			return nil
		default:
		}
		w, err := advisoryWaiters(ctx, p.pool)
		if err != nil {
			return err
		}
		if w > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("tempo esgotado aguardando a outra transação")
		}
	}
}

type pausedPolicies struct {
	*pgstore.Collections
	pause *pauseUntilPeer
}

func (s pausedPolicies) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(collections.Tx) error) error {
	return s.Collections.InTenant(ctx, tenantID, func(tx collections.Tx) error { return fn(pausedPoliciesTx{tx, s.pause}) })
}

type pausedPoliciesTx struct {
	collections.Tx
	pause *pauseUntilPeer
}

func (tx pausedPoliciesTx) DefaultPolicy(ctx context.Context) (*collections.Policy, error) {
	p, err := tx.Tx.DefaultPolicy(ctx)
	if err != nil {
		return nil, err
	}
	return p, tx.pause.arrive(ctx)
}

// Duas criações simultâneas sem política padrão definida: o bloqueio por
// tenant serializa a leitura da padrão, e ambas terminam com exatamente uma
// política padrão (sem violar o índice único nem responder 500).
func TestCollectionPolicyConcurrentFirstCreation(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := env.NewTenant(t, "regua-corrida")
	svc := collections.NewService(pausedPolicies{env.Store.Collections(), newPauseUntilPeer(pgtest.OwnerPool(t))}, env.Clock, nil)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, name := range []string{"Régua Alfa", "Régua Beta"} {
		wg.Go(func() {
			in := defaultPolicyInput()
			in.Name = name
			_, errs[i] = svc.CreatePolicy(ctx, tn.ID, tn.Actor, nil, in)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("criação concorrente: %v", err)
		}
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM collection_policies WHERE tenant_id = $1 AND is_default`, tn.ID); n != 1 {
		t.Fatalf("políticas padrão: %d", n)
	}
}

// A descoberta de tenants da régua, do envio de notificações e das entregas
// de webhooks é paginada por ID (ordem crescente, sem repetição).
func TestWorkDiscoveryIsPaginatedByTenant(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	notif := notifications.NewService(env.Store.Notifications(), nil, env.Keyring, env.Clock, notifications.Config{})
	hooks := webhooks.NewService(env.Store.Webhooks(), nil, nil, env.Clock, webhooks.Config{})
	var created []uuid.UUID
	for range 3 {
		tn := webhookTenant(t, env, "descoberta")
		_, invoiceID := issuedInvoice(t, env, tn, "descoberta@cliente.test")
		if err := notif.HandleInvoiceIssued(ctx, tn.ID, invoiceID, ids.New()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := hooks.CreateEndpoint(ctx, tn.ID, tn.Actor, nil, webhooks.Input{URL: "https://hooks.cliente.test/x",
			EventTypes: []string{outbox.EventInvoicePaid}}); err != nil {
			t.Fatal(err)
		}
		if err := hooks.HandleEvent(ctx, tn.ID, ids.New(), outbox.EventInvoicePaid, now, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		created = append(created, tn.ID)
	}
	at := env.Clock.Now()
	pages := map[string]func(after *uuid.UUID, limit int) ([]uuid.UUID, error){
		"régua": func(after *uuid.UUID, limit int) ([]uuid.UUID, error) {
			return env.Store.Collections().TenantsForCollections(ctx, at.AddDate(0, 0, collections.ReminderHorizonDays), after, limit)
		},
		"notificações": func(after *uuid.UUID, limit int) ([]uuid.UUID, error) {
			return env.Store.Notifications().TenantsWithDueNotifications(ctx, at, after, limit)
		},
		"webhooks": func(after *uuid.UUID, limit int) ([]uuid.UUID, error) {
			return env.Store.Webhooks().TenantsWithDueDeliveries(ctx, at, after, limit)
		},
	}
	for name, page := range pages {
		found := walkTenants(t, page)
		for _, id := range created {
			if !slices.Contains(found, id) {
				t.Errorf("%s: tenant %s fora da descoberta paginada", name, id)
			}
		}
		all, err := page(nil, len(found)+10)
		if err != nil || !slices.Equal(all, found) {
			t.Errorf("%s: página única difere da paginada (%d x %d): %v", name, len(all), len(found), err)
		}
	}
}
