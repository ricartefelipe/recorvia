//go:build integration

package pgstore_test

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

const anonymizationRequest = "Pedido de eliminação do titular, protocolo 2026/0107"

// anonymize pede a anonimização e devolve o código do erro ("" se aceita).
func (a *advEnv) anonymize(t *testing.T, tn fixture.Tenant, customerID uuid.UUID) string {
	t.Helper()
	_, err := a.Customers.Anonymize(context.Background(), tn.ID, tn.Actor, customerID, anonymizationRequest)
	if err == nil {
		return ""
	}
	code := errCode(err)
	if code == "" {
		t.Fatalf("anonimização: erro inesperado: %v", err)
	}
	return code
}

// voidOpenInvoices cancela as faturas em aberto da assinatura.
func (a *advEnv) voidOpenInvoices(t *testing.T, tn fixture.Tenant, subscriptionID uuid.UUID) {
	t.Helper()
	for _, inv := range a.invoicesOf(t, tn.ID, subscriptionID) {
		if inv.Status != string(billing.Open) {
			continue
		}
		if _, err := a.Billing.Void(context.Background(), tn.ID, tn.Actor, inv.ID, "encerramento do contrato", 0); err != nil {
			t.Fatal(err)
		}
	}
}

func (a *advEnv) customerName(t *testing.T, tn fixture.Tenant, customerID uuid.UUID) string {
	t.Helper()
	d, err := a.Customers.Get(context.Background(), tn.ID, customerID)
	if err != nil {
		t.Fatal(err)
	}
	return d.Name
}

// Assinatura por consumo cancelada de imediato: o consumo final ainda vai ser
// faturado, sem assinatura ativa nem fatura em aberto. A anonimização é
// recusada até o fechamento, a resolução da fatura e o fim da tolerância de
// consumo atrasado; a fatura final nasce com o cadastro real, e nenhuma fatura
// nem evento de consumo é gravado para o cliente depois de anonimizado.
func TestAnonymizationWaitsForFinalUsageBilling(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	s := seeder{t: t, pool: pgtest.OwnerPool(t)}
	tn := a.NewTenant(t, "anon-consumo-final")
	metric, plan := a.newMeteredPlan(t, tn, catalog.AggSum, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "25"})
	sub := a.newSubscriptionQty(t, tn, plan.ID, 1)
	original := a.customerName(t, tn, sub.CustomerID)
	if _, err := a.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	event := func(id string, at time.Time) metering.EventInput {
		return metering.EventInput{ExternalID: id, SubscriptionID: sub.ID, MetricCode: metric.Code, Quantity: "100", OccurredAt: at,
			Properties: map[string]any{"origem": "integração do cliente"}}
	}
	a.Clock.Set(spTime(t, 2026, time.March, 20, 10))
	wantOutcomes(t, a.ingest(t, tn, event("final-1", spTime(t, 2026, time.March, 20, 9))), metering.OutcomeAccepted)
	canceled, err := a.Subscriptions.Cancel(ctx, tn.ID, tn.Actor, sub.ID, subscriptions.CancelImmediately, "encerramento do contrato", 0)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Status != subscriptions.Canceled || canceled.NextBillingAt == nil || canceled.EndedAt == nil {
		t.Fatalf("assinatura cancelada com consumo final a faturar: %+v", canceled)
	}
	a.voidOpenInvoices(t, tn, sub.ID)

	// Sem assinatura ativa nem fatura em aberto, mas com o consumo final a faturar.
	if code := a.anonymize(t, tn, sub.CustomerID); code != "customer_has_obligations" {
		t.Fatalf("anonimização com consumo final pendente: %q", code)
	}
	if name := a.customerName(t, tn, sub.CustomerID); name != original {
		t.Fatalf("cliente alterado por anonimização recusada: %q", name)
	}

	// O fechamento emite a fatura do consumo final com o cadastro real.
	a.runEngine(t, tn, 1)
	final := findInvoice(t, a.invoicesOf(t, tn.ID, sub.ID), billing.KindFinalUsage, canceled.CurrentPeriod.Start)
	if final.Status != string(billing.Open) || final.Total != 2500 {
		t.Fatalf("fatura do consumo final: %+v", final)
	}
	if n := s.count(`SELECT count(*) FROM invoices WHERE id = $1 AND customer_snapshot->>'name' = $2`, final.ID, original); n != 1 {
		t.Fatal("a fatura do consumo final deveria trazer o cadastro do cliente no momento da emissão")
	}
	if code := a.anonymize(t, tn, sub.CustomerID); code != "customer_has_obligations" {
		t.Fatalf("anonimização com a fatura final em aberto: %q", code)
	}
	a.voidOpenInvoices(t, tn, sub.ID)

	// Fatura resolvida, mas a métrica ainda aceita consumo atrasado do período
	// fechado (72 h depois do encerramento), o que geraria correção e fatura.
	graceEnd := canceled.EndedAt.Add(time.Duration(metric.LateGraceHours) * time.Hour)
	a.Clock.Set(graceEnd)
	if code := a.anonymize(t, tn, sub.CustomerID); code != "customer_has_obligations" {
		t.Fatalf("anonimização no último instante da tolerância de atraso: %q", code)
	}
	a.Clock.Set(graceEnd.Add(time.Microsecond))
	if code := a.anonymize(t, tn, sub.CustomerID); code != "" {
		t.Fatalf("anonimização depois da tolerância de atraso: %q", code)
	}
	if name := a.customerName(t, tn, sub.CustomerID); name != customers.AnonymizedName {
		t.Fatalf("cliente depois da anonimização: %q", name)
	}

	// Depois da anonimização nenhum consumo é gravado para o cliente, mesmo
	// que a métrica volte a aceitar atraso (tolerância ampliada diretamente no
	// banco): sem a recusa, o evento seria "late" e viraria fatura.
	invoicesBefore := s.count(`SELECT count(*) FROM invoices WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, sub.CustomerID)
	eventsBefore := s.count(`SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, sub.CustomerID)
	s.exec(`UPDATE billable_metrics SET late_grace_hours = 720 WHERE tenant_id = $1 AND id = $2`, tn.ID, metric.ID)
	res := a.ingest(t, tn, event("atrasado-1", spTime(t, 2026, time.March, 19, 15)), event("final-1", spTime(t, 2026, time.March, 20, 9)))
	wantOutcomes(t, res, metering.OutcomeRejected, metering.OutcomeDuplicate)
	if res[0].Error != "Cliente anonimizado: consumo não é mais aceito." {
		t.Fatalf("mensagem da rejeição: %q", res[0].Error)
	}
	if n := s.count(`SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, sub.CustomerID); n != eventsBefore {
		t.Fatalf("eventos de consumo do cliente anonimizado: %d, antes %d", n, eventsBefore)
	}
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	a.runEngine(t, tn, 0)
	if n := s.count(`SELECT count(*) FROM invoices WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, sub.CustomerID); n != invoicesBefore {
		t.Fatalf("faturas do cliente anonimizado: %d, antes %d", n, invoicesBefore)
	}
	if n := s.count(`SELECT count(*) FROM invoices WHERE tenant_id = $1 AND customer_id = $2
		AND customer_snapshot->>'name' = $3`, tn.ID, sub.CustomerID, customers.AnonymizedName); n != 0 {
		t.Fatalf("faturas emitidas com o cadastro anonimizado: %d", n)
	}
}

// pendingBilling consulta a pré-condição da anonimização no instante at.
func (a *advEnv) pendingBilling(t *testing.T, tn fixture.Tenant, customerID uuid.UUID, at time.Time) bool {
	t.Helper()
	var pending bool
	err := a.Store.Customers().InTenant(context.Background(), tn.ID, func(tx customers.Tx) error {
		var err error
		pending, err = tx.HasPendingBilling(context.Background(), customerID, at)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

// Cada forma de faturamento ainda por acontecer impede a anonimização de um
// cliente sem assinatura ativa nem fatura em aberto; o que já foi faturado,
// corrigido ou saiu da tolerância não impede. Nada disso alcança outro cliente.
func TestAnonymizationPendingBillingConditions(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	s := seeder{t: t, pool: pgtest.OwnerPool(t)}
	tn := a.NewTenant(t, "anon-pendencias")
	metric, _ := a.newMeteredPlan(t, tn, catalog.AggSum, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "1"})
	plan := a.newFlatPlan(t, tn, 9000)
	at := a.Clock.Now()
	grace := time.Duration(metric.LateGraceHours) * time.Hour
	periodStart := at.Add(-40 * 24 * time.Hour)

	aggregate := func(sub subscriptions.Subscription, start, end time.Time) uuid.UUID {
		id := ids.New()
		s.exec(`INSERT INTO usage_aggregates (id, tenant_id, subscription_id, metric_id, period_start, period_end, aggregation, quantity,
			event_count, closed_at) VALUES ($1, $2, $3, $4, $5, $6, 'sum', 10, 1, $6)`, id, tn.ID, sub.ID, metric.ID, start, end)
		return id
	}
	usage := func(sub subscriptions.Subscription, status string, start time.Time, aggregateID *uuid.UUID, correctedAt *time.Time) {
		s.exec(`INSERT INTO usage_events (id, tenant_id, external_id, customer_id, subscription_id, metric_id, quantity, occurred_at, received_at,
			period_start, status, aggregate_id, corrected_at) VALUES ($1, $2, $3, $4, $5, $6, 5, $7, $8, $7, $9, $10, $11)`,
			ids.New(), tn.ID, "pendencia-"+uniqueSuffix(), sub.CustomerID, sub.ID, metric.ID, start, at, status, aggregateID, correctedAt)
	}
	pendingItem := func(sub subscriptions.Subscription, invoiceID *uuid.UUID) {
		s.exec(`INSERT INTO pending_invoice_items (id, tenant_id, customer_id, subscription_id, kind, source, description, amount, invoice_id,
			created_at) VALUES ($1, $2, $3, $4, 'adjustment', 'manual', 'Ajuste de teste', 500, $5, $6)`,
			ids.New(), tn.ID, sub.CustomerID, sub.ID, invoiceID, at)
	}
	interrupted := func(sub subscriptions.Subscription, _ uuid.UUID) {
		aggregate(sub, periodStart, at.Add(20*24*time.Hour))
	}
	cases := []struct {
		name string
		seed func(sub subscriptions.Subscription, invoiceID uuid.UUID)
		// after desloca o relógio na verificação (a assinatura é encerrada em at).
		after time.Duration
		want  bool
	}{
		{"assinatura cancelada sem nada a faturar", func(subscriptions.Subscription, uuid.UUID) {}, 0, false},
		{"assinatura cancelada com fechamento agendado", func(sub subscriptions.Subscription, _ uuid.UUID) {
			s.exec(`UPDATE subscriptions SET next_billing_at = $3 WHERE tenant_id = $1 AND id = $2`, tn.ID, sub.ID, at.Add(time.Hour))
		}, 0, true},
		{"item pendente sem fatura", func(sub subscriptions.Subscription, _ uuid.UUID) { pendingItem(sub, nil) }, 0, true},
		{"item pendente já faturado", func(sub subscriptions.Subscription, invoiceID uuid.UUID) { pendingItem(sub, &invoiceID) }, 0, false},
		{"consumo atrasado por corrigir", func(sub subscriptions.Subscription, _ uuid.UUID) {
			agg := aggregate(sub, periodStart, periodStart.Add(24*time.Hour))
			usage(sub, "late", periodStart, &agg, nil)
		}, 0, true},
		{"consumo atrasado já corrigido", func(sub subscriptions.Subscription, _ uuid.UUID) {
			agg := aggregate(sub, periodStart, periodStart.Add(24*time.Hour))
			usage(sub, "late", periodStart, &agg, &at)
		}, 0, false},
		{"consumo aceito sem agregado em período fechado", func(sub subscriptions.Subscription, _ uuid.UUID) {
			aggregate(sub, periodStart, periodStart.Add(24*time.Hour))
			usage(sub, "accepted", periodStart, nil, nil)
		}, 0, true},
		{"consumo aceito sem agregado em período que não será fechado", func(sub subscriptions.Subscription, _ uuid.UUID) {
			usage(sub, "accepted", periodStart, nil, nil)
		}, 0, false},
		{"consumo faturado fora da tolerância", func(sub subscriptions.Subscription, _ uuid.UUID) {
			agg := aggregate(sub, periodStart, periodStart.Add(24*time.Hour))
			usage(sub, "accepted", periodStart, &agg, nil)
		}, 0, false},
		{"período fechado no último instante da tolerância", func(sub subscriptions.Subscription, _ uuid.UUID) {
			aggregate(sub, periodStart, at.Add(-grace))
		}, 0, true},
		{"período fechado logo depois da tolerância", func(sub subscriptions.Subscription, _ uuid.UUID) {
			aggregate(sub, periodStart, at.Add(-grace-time.Microsecond))
		}, 0, false},
		// O fim do período é limitado pelo encerramento da assinatura, como na
		// ingestão: o período iria até vinte dias depois, mas a assinatura
		// terminou em at e a tolerância conta do encerramento.
		{"período interrompido pelo encerramento, no último instante da tolerância", interrupted, grace, true},
		{"período interrompido pelo encerramento, logo depois da tolerância", interrupted, grace + time.Microsecond, false},
	}
	// Cliente de controle no mesmo tenant, com assinatura cancelada e sem
	// nenhuma pendência.
	control := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
	a.runEngine(t, tn, 1)
	if _, err := a.Subscriptions.Cancel(ctx, tn.ID, tn.Actor, control.ID, subscriptions.CancelImmediately, "encerramento", 0); err != nil {
		t.Fatal(err)
	}
	a.voidOpenInvoices(t, tn, control.ID)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer a.Clock.Set(at)
			sub := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
			a.runEngine(t, tn, 1)
			invoices := a.invoicesOf(t, tn.ID, sub.ID)
			if len(invoices) != 1 {
				t.Fatalf("faturas da assinatura: %d", len(invoices))
			}
			if _, err := a.Subscriptions.Cancel(ctx, tn.ID, tn.Actor, sub.ID, subscriptions.CancelImmediately, "encerramento", 0); err != nil {
				t.Fatal(err)
			}
			a.voidOpenInvoices(t, tn, sub.ID)
			tc.seed(sub, invoices[0].ID)
			a.Clock.Set(at.Add(tc.after))
			if got := a.pendingBilling(t, tn, sub.CustomerID, a.Clock.Now()); got != tc.want {
				t.Fatalf("faturamento pendente = %v, esperado %v", got, tc.want)
			}
			if a.pendingBilling(t, tn, control.CustomerID, a.Clock.Now()) {
				t.Fatal("a pendência de um cliente alcançou outro")
			}
			code := a.anonymize(t, tn, sub.CustomerID)
			if want := map[bool]string{true: "customer_has_obligations", false: ""}[tc.want]; code != want {
				t.Fatalf("anonimização: código %q, esperado %q", code, want)
			}
			if anonymized := a.customerName(t, tn, sub.CustomerID) == customers.AnonymizedName; anonymized == tc.want {
				t.Fatalf("cliente anonimizado = %v com faturamento pendente = %v", anonymized, tc.want)
			}
		})
	}
}

// closedUsagePeriod prepara um cliente com assinatura por consumo cancelada,
// consumo final já faturado e fatura resolvida, com o relógio no último
// instante da tolerância de atraso da métrica: o período fechado ainda aceita
// consumo atrasado, e um microssegundo depois a anonimização passa a ser aceita.
func (a *advEnv) closedUsagePeriod(t *testing.T, name string) (fixture.Tenant, catalog.Metric, subscriptions.Subscription, time.Time) {
	t.Helper()
	ctx := context.Background()
	tn := a.NewTenant(t, name)
	metric, plan := a.newMeteredPlan(t, tn, catalog.AggSum, catalog.PriceInput{Model: catalog.Usage, UnitAmount: "25"})
	sub := a.newSubscriptionQty(t, tn, plan.ID, 1)
	if _, err := a.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	a.Clock.Set(spTime(t, 2026, time.March, 20, 10))
	wantOutcomes(t, a.ingest(t, tn, metering.EventInput{ExternalID: "final-1", SubscriptionID: sub.ID, MetricCode: metric.Code, Quantity: "100",
		OccurredAt: spTime(t, 2026, time.March, 20, 9)}), metering.OutcomeAccepted)
	canceled, err := a.Subscriptions.Cancel(ctx, tn.ID, tn.Actor, sub.ID, subscriptions.CancelImmediately, "encerramento do contrato", 0)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.EndedAt == nil {
		t.Fatalf("assinatura cancelada sem data de encerramento: %+v", canceled)
	}
	a.voidOpenInvoices(t, tn, sub.ID)
	a.runEngine(t, tn, 1)
	a.voidOpenInvoices(t, tn, sub.ID)
	graceEnd := canceled.EndedAt.Add(time.Duration(metric.LateGraceHours) * time.Hour)
	a.Clock.Set(graceEnd)
	return tn, metric, canceled, graceEnd
}

// Ingestão concorrente com a anonimização em andamento: o lote lê o cliente
// com bloqueio compartilhado, espera a confirmação e rejeita o evento em vez de
// gravá-lo com a situação lida antes. Sem o bloqueio da leitura, o lote veria o
// cliente como não anonimizado, a gravação esperaria só na chave estrangeira e
// o evento entraria como atrasado para um cliente já anonimizado: a correção de
// consumo tentaria faturá-lo e seria recusada pelo banco a cada execução.
func TestUsageIngestionWaitsForAnonymizationInProgress(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 6)
	owner := pgtest.OwnerPool(t)
	s := seeder{t: t, pool: owner}
	tn, metric, sub, _ := a.closedUsagePeriod(t, "anon-ingestao-concorrente")
	event := func(id string, at time.Time) metering.EventInput {
		return metering.EventInput{ExternalID: id, SubscriptionID: sub.ID, MetricCode: metric.Code, Quantity: "100", OccurredAt: at}
	}

	// O período fechado ainda aceita consumo atrasado. O primeiro evento prova
	// isso e é corrigido e faturado antes da anonimização, para não restar nada
	// por corrigir.
	wantOutcomes(t, a.ingest(t, tn, event("atrasado-1", spTime(t, 2026, time.March, 19, 15))), metering.OutcomeLate)
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	a.voidOpenInvoices(t, tn, sub.ID)
	if n := s.count(`SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND customer_id = $2 AND status = 'late'
		AND corrected_at IS NULL`, tn.ID, sub.CustomerID); n != 0 {
		t.Fatalf("consumo atrasado por corrigir antes da anonimização: %d", n)
	}
	invoicesBefore := s.count(`SELECT count(*) FROM invoices WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, sub.CustomerID)
	eventsBefore := s.count(`SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, sub.CustomerID)

	// Anonimização em andamento, com o relógio do lote ainda dentro da
	// tolerância: o mesmo evento, um instante antes, seria aceito como atrasado.
	blockedBefore := lockWaiters(t, owner)
	confirm := a.holdAnonymization(t, tn, sub.CustomerID)
	type ingestion struct {
		res []metering.Result
		err error
	}
	ingested := make(chan ingestion, 1)
	done := make(chan struct{})
	batch := []metering.EventInput{event("atrasado-2", spTime(t, 2026, time.March, 19, 16))}
	go func() {
		defer close(done)
		res, err := a.Metering.Ingest(ctx, tn.ID, batch)
		ingested <- ingestion{res, err}
	}()
	if !waitLockWaiters(t, owner, blockedBefore+1, done) {
		t.Fatalf("a ingestão não esperou a anonimização em andamento: %+v", <-ingested)
	}
	// A espera é na leitura do cliente, antes de qualquer gravação do evento
	// (consulta feita com o papel da aplicação, que enxerga o texto das
	// consultas das próprias sessões).
	var waitingOnCustomer int
	err := a.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*)::int FROM pg_locks l JOIN pg_stat_activity act ON act.pid = l.pid
			WHERE NOT l.granted AND act.query LIKE '%FROM customers%FOR KEY SHARE%'`).Scan(&waitingOnCustomer)
	})
	if err != nil {
		t.Fatal(err)
	}
	if waitingOnCustomer != 1 {
		t.Fatalf("sessões esperando na leitura do cliente com bloqueio compartilhado: %d", waitingOnCustomer)
	}
	if err := confirm(); err != nil {
		t.Fatalf("anonimização: %v", err)
	}
	got := <-ingested
	if got.err != nil {
		t.Fatalf("ingestão concorrente com a anonimização: %v", got.err)
	}
	wantOutcomes(t, got.res, metering.OutcomeRejected)
	if got.res[0].Error != "Cliente anonimizado: consumo não é mais aceito." {
		t.Fatalf("mensagem da rejeição: %q", got.res[0].Error)
	}
	if name := a.customerName(t, tn, sub.CustomerID); name != customers.AnonymizedName {
		t.Fatalf("cliente depois da anonimização: %q", name)
	}
	if n := s.count(`SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, sub.CustomerID); n != eventsBefore {
		t.Fatalf("eventos de consumo do cliente anonimizado: %d, antes %d", n, eventsBefore)
	}

	// Nada ficou por corrigir: a correção de consumo e o motor continuam
	// rodando sem erro e sem emitir fatura para o cliente anonimizado.
	if _, err := a.Corrector.RunAll(ctx); err != nil {
		t.Fatalf("correção de consumo depois da anonimização: %v", err)
	}
	a.runEngine(t, tn, 0)
	if n := s.count(`SELECT count(*) FROM invoices WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, sub.CustomerID); n != invoicesBefore {
		t.Fatalf("faturas do cliente anonimizado: %d, antes %d", n, invoicesBefore)
	}
}

// O sentido inverso: a anonimização pedida enquanto um lote de ingestão está em
// andamento espera o lote já na leitura do cliente (antes de o evento ser
// gravado) e, depois da confirmação dele, avalia o faturamento pendente com o
// evento atrasado gravado: é recusada, e o cliente continua como estava.
func TestAnonymizationWaitsForUsageIngestionInProgress(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 6)
	owner := pgtest.OwnerPool(t)
	s := seeder{t: t, pool: owner}
	tn, metric, sub, graceEnd := a.closedUsagePeriod(t, "anon-espera-ingestao")
	original := a.customerName(t, tn, sub.CustomerID)
	eventsBefore := s.count(`SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, sub.CustomerID)

	// Lote em andamento, direto na transação: a assinatura e o cliente já foram
	// lidos com bloqueio compartilhado e o evento atrasado ainda não foi gravado.
	late := metering.Event{ID: ids.New(), TenantID: tn.ID, ExternalID: "atrasado-1", CustomerID: sub.CustomerID, SubscriptionID: sub.ID,
		MetricID: metric.ID, Quantity: big.NewRat(100, 1), OccurredAt: spTime(t, 2026, time.March, 19, 15), ReceivedAt: graceEnd,
		PeriodStart: sub.CurrentPeriod.Start, Status: metering.Late}
	blockedBefore := lockWaiters(t, owner)
	read, release, ingested := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	t.Cleanup(resume)
	go func() {
		ingested <- a.Store.Metering().InTenant(ctx, tn.ID, func(tx metering.Tx) error {
			if _, err := tx.SubscriptionForShare(ctx, sub.ID); err != nil {
				return err
			}
			anonymized, err := tx.CustomerAnonymized(ctx, sub.CustomerID)
			if err != nil {
				return err
			}
			if anonymized {
				return errors.New("cliente lido como anonimizado antes da anonimização")
			}
			close(read)
			<-release
			inserted, err := tx.InsertEvent(ctx, late)
			if err == nil && !inserted {
				err = errors.New("evento atrasado não gravado")
			}
			return err
		})
	}()
	select {
	case <-read:
	case err := <-ingested:
		t.Fatalf("lote em andamento terminou antes da hora: %v", err)
	}

	// Um microssegundo depois da tolerância a anonimização seria aceita, se o
	// lote em andamento não gravasse mais nada.
	a.Clock.Set(graceEnd.Add(time.Microsecond))
	anonymized := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := a.Customers.Anonymize(ctx, tn.ID, tn.Actor, sub.CustomerID, anonymizationRequest)
		anonymized <- err
	}()
	if !waitLockWaiters(t, owner, blockedBefore+1, done) {
		t.Fatalf("a anonimização não esperou a ingestão em andamento: %v", <-anonymized)
	}
	resume()
	if err := <-ingested; err != nil {
		t.Fatalf("ingestão: %v", err)
	}
	if err := <-anonymized; errCode(err) != "customer_has_obligations" {
		t.Fatalf("anonimização depois do lote com consumo atrasado: %v", err)
	}
	if name := a.customerName(t, tn, sub.CustomerID); name != original {
		t.Fatalf("cliente alterado por anonimização recusada: %q", name)
	}
	if n := s.count(`SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, sub.CustomerID); n != eventsBefore+1 {
		t.Fatalf("eventos de consumo do cliente: %d, antes %d", n, eventsBefore)
	}
}

// holdAnonymization executa a anonimização do cliente direto na transação (sem
// as pré-condições do caso de uso) e a mantém em andamento: o cadastro já foi
// substituído e o cliente continua bloqueado, mas nada foi confirmado. A função
// devolvida confirma a transação e devolve o erro dela.
func (a *advEnv) holdAnonymization(t *testing.T, tn fixture.Tenant, customerID uuid.UUID) func() error {
	t.Helper()
	ctx := context.Background()
	locked, release, confirmed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		confirmed <- a.Store.Customers().InTenant(ctx, tn.ID, func(tx customers.Tx) error {
			c, err := tx.Get(ctx, customerID, true)
			if err != nil {
				return err
			}
			at := a.Clock.Now()
			c.Anonymize(at)
			if _, err := tx.Anonymize(ctx, c); err != nil {
				return err
			}
			if _, err := tx.ErasePersonalData(ctx, customerID, at); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	var once sync.Once
	confirm := func() error {
		once.Do(func() { close(release) })
		err := <-confirmed
		// Chamadas seguintes (a limpeza do teste) recebem o mesmo resultado.
		confirmed <- err
		return err
	}
	select {
	case <-locked:
	case err := <-confirmed:
		t.Fatalf("anonimização em andamento terminou antes da hora: %v", err)
	}
	t.Cleanup(func() { _ = confirm() })
	return confirm
}

// Defesa no banco: nenhuma fatura é gravada para um cliente anonimizado, nem
// pelo dono das tabelas; e a fatura montada antes da anonimização, gravada
// enquanto ela está em andamento, espera o resultado e é recusada em vez de
// entrar depois com o cadastro lido antes.
func TestInvoiceForAnonymizedCustomerIsRejectedByDatabase(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 6)
	owner := pgtest.OwnerPool(t)
	s := seeder{t: t, pool: owner}
	tn := a.NewTenant(t, "anon-fatura-banco")
	plan := a.newFlatPlan(t, tn, 7000)
	const copyInvoice = `INSERT INTO invoices SELECT (jsonb_populate_record(i, jsonb_build_object('id', $2::uuid, 'number', $3::text,
		'billing_period_start', i.billing_period_start + interval '1 year', 'billing_period_end', i.billing_period_end + interval '1 year'))).*
		FROM invoices i WHERE i.id = $1`
	issued := func() (subscriptions.Subscription, invoiceSummary) {
		sub := a.NewSubscription(t, tn, a.NewCustomer(t, tn).ID, plan.ID, nil)
		a.runEngine(t, tn, 1)
		if _, err := a.Subscriptions.Cancel(ctx, tn.ID, tn.Actor, sub.ID, subscriptions.CancelImmediately, "encerramento", 0); err != nil {
			t.Fatal(err)
		}
		a.voidOpenInvoices(t, tn, sub.ID)
		return sub, a.invoicesOf(t, tn.ID, sub.ID)[0]
	}

	// Cliente não anonimizado: a mesma gravação é aceita (o gatilho só recusa
	// cliente anonimizado).
	keptSub, keptInv := issued()
	if _, err := owner.Exec(ctx, copyInvoice, keptInv.ID, ids.New(), "COPIA-"+uniqueSuffix()); err != nil {
		t.Fatalf("fatura para cliente não anonimizado: %v", err)
	}
	if n := s.count(`SELECT count(*) FROM invoices WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, keptSub.CustomerID); n != 2 {
		t.Fatalf("faturas do cliente não anonimizado: %d", n)
	}

	// Cliente já anonimizado.
	sub, inv := issued()
	if code := a.anonymize(t, tn, sub.CustomerID); code != "" {
		t.Fatalf("anonimização: %q", code)
	}
	if _, err := owner.Exec(ctx, copyInvoice, inv.ID, ids.New(), "COPIA-"+uniqueSuffix()); !postgres.IsRaisedException(err) {
		t.Fatalf("fatura para cliente anonimizado gravada no banco: %v", err)
	}
	err := a.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, copyInvoice, inv.ID, ids.New(), "COPIA-"+uniqueSuffix())
		return err
	})
	if !postgres.IsRaisedException(err) {
		t.Fatalf("fatura para cliente anonimizado gravada pela aplicação: %v", err)
	}

	// Anonimização em andamento (cliente bloqueado, ainda não confirmada): a
	// gravação concorrente da fatura espera e é recusada depois da confirmação.
	racing, racingInv := issued()
	blockedBefore := lockWaiters(t, owner)
	confirm := a.holdAnonymization(t, tn, racing.CustomerID)
	inserted := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := owner.Exec(ctx, copyInvoice, racingInv.ID, ids.New(), "COPIA-"+uniqueSuffix())
		inserted <- err
	}()
	if !waitLockWaiters(t, owner, blockedBefore+1, done) {
		t.Fatalf("a gravação da fatura não esperou a anonimização em andamento: %v", <-inserted)
	}
	if err := confirm(); err != nil {
		t.Fatalf("anonimização: %v", err)
	}
	if err := <-inserted; !postgres.IsRaisedException(err) {
		t.Fatalf("fatura gravada depois da anonimização concorrente: %v", err)
	}
	if n := s.count(`SELECT count(*) FROM invoices WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, racing.CustomerID); n != 1 {
		t.Fatalf("faturas do cliente anonimizado: %d", n)
	}
}
