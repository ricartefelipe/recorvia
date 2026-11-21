//go:build integration

package pgstore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/db"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// queueRef é o instante de referência dos testes das métricas de fila. O
// banco é compartilhado pelos testes do pacote, que gravam datas de 2026 em
// diante: com a referência em 2001, nenhum dado de outro teste está vencido
// nem dentro da janela de falhas, e as idades medidas são as das linhas
// criadas aqui.
var queueRef = time.Date(2001, 5, 20, 12, 0, 0, 0, time.UTC)

func tenantExec(t *testing.T, env *fixture.Env, tenantID uuid.UUID, sql string, args ...any) {
	t.Helper()
	err := env.DB.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), sql, args...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// As dead letters abertas são contadas por consumidor em todos os tenants,
// inclusive as registradas sem tenant (invisíveis ao papel da aplicação), e
// as resolvidas saem da contagem.
func TestDeadLetterStatsAggregatesOpenByConsumerAcrossTenants(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	a, b := env.NewTenant(t, "dlq-stats-a"), env.NewTenant(t, "dlq-stats-b")
	suffix := uuid.NewString()[:8]
	billingConsumer, webhookConsumer, idle := "teste.cobranca."+suffix, "teste.webhooks."+suffix, "teste.sem-falhas."+suffix
	record := func(tenantID *uuid.UUID, consumer string, age time.Duration) uuid.UUID {
		t.Helper()
		d := pgstore.DeadLetter{ID: ids.New(), TenantID: tenantID, Consumer: consumer, MessageID: uuid.NewString(), EventType: "invoice.issued",
			Payload: []byte(`{}`), Error: "falha de teste", Attempts: 6, FailedAt: queueRef.Add(-age)}
		if err := env.Store.RecordDeadLetter(ctx, d); err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	record(&a.ID, billingConsumer, 10*time.Minute)
	oldest := record(&b.ID, billingConsumer, 3*time.Hour)
	record(&a.ID, billingConsumer, time.Minute)
	record(&b.ID, webhookConsumer, 30*time.Second)
	resolved := record(&a.ID, webhookConsumer, 5*time.Hour)
	tenantExec(t, env, a.ID, `UPDATE dead_letters SET status = 'discarded', resolved_at = $3, resolution_note = 'teste'
		WHERE tenant_id = $1 AND id = $2`, a.ID, resolved, queueRef)

	stats, err := env.Store.DeadLetterStats(ctx, queueRef)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		consumer string
		want     pgstore.BacklogStat
		present  bool
	}{
		{billingConsumer, pgstore.BacklogStat{Total: 3, OldestSeconds: (3 * time.Hour).Seconds()}, true},
		{webhookConsumer, pgstore.BacklogStat{Total: 1, OldestSeconds: 30}, true},
		{idle, pgstore.BacklogStat{}, false},
	}
	for _, c := range cases {
		got, ok := stats[c.consumer]
		if ok != c.present || got != c.want {
			t.Errorf("%s: %+v (presente: %v), esperado %+v (presente: %v)", c.consumer, got, ok, c.want, c.present)
		}
	}

	// Uma dead letter sem tenant é registrada com o relógio do banco e não é
	// visível ao papel da aplicação; a função agregada a conta. Medida no
	// instante de referência (anterior ao registro), a idade não fica
	// negativa.
	untenanted := "teste.sem-tenant." + suffix
	record(nil, untenanted, 0)
	var visible int
	if err := env.DB.InTenantTx(ctx, a.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM dead_letters WHERE consumer = $1`, untenanted).Scan(&visible)
	}); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("dead letter sem tenant visível ao tenant: %d", visible)
	}
	stats, err = env.Store.DeadLetterStats(ctx, queueRef)
	if err != nil {
		t.Fatal(err)
	}
	if got := stats[untenanted]; got.Total != 1 || got.OldestSeconds != 0 {
		t.Fatalf("dead letter sem tenant: %+v", got)
	}
	// Um dia depois, a idade acompanha o instante informado pelo chamador.
	later, err := env.Store.DeadLetterStats(ctx, queueRef.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := later[billingConsumer]; got.Total != 3 || got.OldestSeconds != (27*time.Hour).Seconds() {
		t.Fatalf("idade um dia depois: %+v", got)
	}
	// Resolvida a mais antiga, a idade passa a ser a da seguinte.
	tenantExec(t, env, b.ID, `UPDATE dead_letters SET status = 'replayed', resolved_at = $3, resolution_note = 'teste'
		WHERE tenant_id = $1 AND id = $2`, b.ID, oldest, queueRef)
	stats, err = env.Store.DeadLetterStats(ctx, queueRef)
	if err != nil {
		t.Fatal(err)
	}
	if got := stats[billingConsumer]; got.Total != 2 || got.OldestSeconds != (10*time.Minute).Seconds() {
		t.Fatalf("depois do reprocessamento: %+v", got)
	}
}

// closeQueuedWork encerra, ao fim do teste, as notificações e entregas ainda
// em fila dos tenants informados: o banco é compartilhado, e as tarefas de
// envio exercitadas por outros testes percorrem todos os tenants com
// trabalho vencido.
func closeQueuedWork(t *testing.T, env *fixture.Env, tenantIDs ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		for _, id := range tenantIDs {
			tenantExec(t, env, id, `UPDATE notifications SET status = 'canceled', status_reason = 'teste encerrado', lease_until = NULL
				WHERE tenant_id = $1 AND status IN ('scheduled', 'sending')`, id)
			tenantExec(t, env, id, `UPDATE webhook_deliveries SET status = 'exhausted', next_attempt_at = NULL, lease_until = NULL
				WHERE tenant_id = $1 AND status IN ('pending', 'retrying', 'delivering')`, id)
		}
	})
}

// insertNotification grava uma notificação mínima na situação informada.
func insertNotification(t *testing.T, env *fixture.Env, tenantID uuid.UUID, status string, sendAfter, updatedAt time.Time) {
	t.Helper()
	var lease *time.Time
	if status == "sending" {
		l := updatedAt.Add(2 * time.Minute)
		lease = &l
	}
	var content []byte
	var keyID *string
	if status != "failed" {
		k := "t1"
		content, keyID = []byte{1}, &k
	}
	tenantExec(t, env, tenantID, `INSERT INTO notifications (id, tenant_id, business_key, template_key, recipients, subject, content_ciphertext,
			content_key_id, status, status_reason, send_after, attempts, max_attempts, lease_until, created_at, updated_at)
		VALUES ($1, $2, $3, 'invoice_issued', '{financeiro@cliente.test}', 'Fatura', $4, $5, $6, NULL, $7, 1, 5, $8, $9, $9)`,
		ids.New(), tenantID, "fila-"+uuid.NewString(), content, keyID, status, sendAfter, lease, updatedAt)
}

// A fila de notificações agrega todos os tenants por situação: agendadas
// vencidas (atraso em relação ao horário de envio), em envio (tempo desde a
// reivindicação) e falhas definitivas da janela. Agendamentos futuros e
// falhas anteriores à janela ficam de fora.
func TestNotificationBacklogStatsByState(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	a, b := env.NewTenant(t, "notif-stats-a"), env.NewTenant(t, "notif-stats-b")
	closeQueuedWork(t, env, a.ID, b.ID)
	failedSince := queueRef.Add(-24 * time.Hour)
	before, err := env.Store.NotificationBacklogStats(ctx, queueRef, failedSince)
	if err != nil {
		t.Fatal(err)
	}
	// Sem linhas, as três situações aparecem com zero.
	for _, state := range []string{pgstore.NotificationBacklogDue, pgstore.NotificationBacklogSending, pgstore.NotificationBacklogFailed} {
		if _, ok := before[state]; !ok {
			t.Fatalf("situação %s ausente: %+v", state, before)
		}
	}
	if before[pgstore.NotificationBacklogDue].Total != 0 || before[pgstore.NotificationBacklogFailed].Total != 0 {
		t.Fatalf("a referência dos testes deve ser anterior aos dados dos demais testes: %+v", before)
	}

	insertNotification(t, env, a.ID, "scheduled", queueRef.Add(-20*time.Minute), queueRef.Add(-time.Hour))
	insertNotification(t, env, b.ID, "scheduled", queueRef.Add(-5*time.Minute), queueRef.Add(-time.Hour))
	insertNotification(t, env, b.ID, "scheduled", queueRef, queueRef.Add(-time.Hour))   // vence exatamente agora
	insertNotification(t, env, a.ID, "scheduled", queueRef.Add(time.Second), queueRef)  // ainda não vencida
	insertNotification(t, env, a.ID, "scheduled", queueRef.Add(72*time.Hour), queueRef) // lembrete futuro
	insertNotification(t, env, a.ID, "sending", queueRef.Add(-time.Hour), queueRef.Add(-90*time.Second))
	insertNotification(t, env, b.ID, "sending", queueRef.Add(-time.Hour), queueRef.Add(-10*time.Second))
	insertNotification(t, env, a.ID, "failed", queueRef.Add(-30*time.Hour), queueRef.Add(-6*time.Hour))
	insertNotification(t, env, b.ID, "failed", queueRef.Add(-30*time.Hour), queueRef.Add(-time.Hour))
	insertNotification(t, env, b.ID, "failed", queueRef.Add(-30*time.Hour), failedSince) // limite exclusivo da janela
	insertNotification(t, env, a.ID, "failed", queueRef.Add(-90*time.Hour), queueRef.Add(-72*time.Hour))

	after, err := env.Store.NotificationBacklogStats(ctx, queueRef, failedSince)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		state  string
		added  int64
		oldest time.Duration
	}{
		{pgstore.NotificationBacklogDue, 3, 20 * time.Minute},
		{pgstore.NotificationBacklogSending, 2, 90 * time.Second},
		{pgstore.NotificationBacklogFailed, 2, 6 * time.Hour},
	}
	for _, c := range cases {
		got := after[c.state]
		if added := got.Total - before[c.state].Total; added != c.added || got.OldestSeconds != c.oldest.Seconds() {
			t.Errorf("%s: %d novas e idade %v s; esperado %d e %v s", c.state, added, got.OldestSeconds, c.added, c.oldest.Seconds())
		}
	}
	// Antes de qualquer vencimento, a fila de vencidas está vazia e a idade é
	// zero.
	early, err := env.Store.NotificationBacklogStats(ctx, queueRef.Add(-48*time.Hour), queueRef.Add(-72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := early[pgstore.NotificationBacklogDue]; got != (pgstore.BacklogStat{}) {
		t.Fatalf("vencidas dois dias antes: %+v", got)
	}
}

// insertDelivery grava um endpoint, um evento e a entrega na situação
// informada.
func insertDelivery(t *testing.T, env *fixture.Env, tenantID, endpointID uuid.UUID, status string, nextAttempt time.Time, attempts int) {
	t.Helper()
	eventID := ids.New()
	tenantExec(t, env, tenantID, `INSERT INTO webhook_events (id, tenant_id, event_type, payload, occurred_at, created_at)
		VALUES ($1, $2, 'invoice.paid', '{}', $3, $3)`, eventID, tenantID, queueRef.Add(-48*time.Hour))
	var next, lease, delivered *time.Time
	switch status {
	case "pending", "retrying":
		next = &nextAttempt
	case "delivering":
		lease = &nextAttempt
	case "succeeded":
		delivered = &nextAttempt
	}
	tenantExec(t, env, tenantID, `INSERT INTO webhook_deliveries (id, tenant_id, endpoint_id, event_id, event_type, status, attempts, max_attempts,
			next_attempt_at, lease_until, delivered_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'invoice.paid', $5, $6, 10, $7, $8, $9, $10, $10)`,
		ids.New(), tenantID, endpointID, eventID, status, attempts, next, lease, delivered, queueRef.Add(-48*time.Hour))
}

func insertEndpoint(t *testing.T, env *fixture.Env, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := ids.New()
	tenantExec(t, env, tenantID, `INSERT INTO webhook_endpoints (id, tenant_id, url, event_types, status, created_at, updated_at)
		VALUES ($1, $2, 'https://integracao.cliente.test/webhooks', '{invoice.paid}', 'active', $3, $3)`, id, tenantID, queueRef.Add(-72*time.Hour))
	return id
}

// A fila de entregas de webhooks agrega todos os tenants por situação; o
// atraso considera somente as entregas já devidas (a espera do backoff é o
// comportamento previsto), a partir do horário da tentativa e não da criação
// da entrega.
func TestWebhookDeliveryBacklogStatsByState(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	a, b := env.NewTenant(t, "wh-stats-a"), env.NewTenant(t, "wh-stats-b")
	closeQueuedWork(t, env, a.ID, b.ID)
	epA, epB := insertEndpoint(t, env, a.ID), insertEndpoint(t, env, b.ID)
	before, err := env.Store.WebhookDeliveryBacklogStats(ctx, queueRef)
	if err != nil {
		t.Fatal(err)
	}
	for state, s := range before {
		if s.OldestSeconds != 0 {
			t.Fatalf("a referência dos testes deve ser anterior aos dados dos demais testes: %s %+v", state, s)
		}
	}

	insertDelivery(t, env, a.ID, epA, "pending", queueRef.Add(-12*time.Minute), 0)
	insertDelivery(t, env, b.ID, epB, "pending", queueRef.Add(-time.Minute), 0)
	insertDelivery(t, env, b.ID, epB, "pending", queueRef.Add(30*time.Second), 0) // devolvida à fila, ainda não devida
	insertDelivery(t, env, a.ID, epA, "retrying", queueRef.Add(-45*time.Second), 3)
	insertDelivery(t, env, a.ID, epA, "retrying", queueRef.Add(4*time.Hour), 8) // em backoff
	insertDelivery(t, env, b.ID, epB, "retrying", queueRef.Add(time.Hour), 5)   // em backoff
	insertDelivery(t, env, a.ID, epA, "delivering", queueRef.Add(-time.Hour), 1)
	insertDelivery(t, env, b.ID, epB, "succeeded", queueRef.Add(-time.Hour), 1)
	insertDelivery(t, env, b.ID, epB, "exhausted", queueRef, 10)

	after, err := env.Store.WebhookDeliveryBacklogStats(ctx, queueRef)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		state  string
		added  int64
		oldest time.Duration
	}{
		{"pending", 3, 12 * time.Minute},
		{"retrying", 3, 45 * time.Second},
	}
	for _, c := range cases {
		got := after[c.state]
		if added := got.Total - before[c.state].Total; added != c.added || got.OldestSeconds != c.oldest.Seconds() {
			t.Errorf("%s: %d novas e atraso %v s; esperado %d e %v s", c.state, added, got.OldestSeconds, c.added, c.oldest.Seconds())
		}
	}
	for state := range after {
		if state != "pending" && state != "retrying" {
			t.Errorf("situação fora da fila exportada: %s", state)
		}
	}
	// Antes de qualquer horário de tentativa, as entregas contam no total e
	// o atraso é zero.
	early, err := env.Store.WebhookDeliveryBacklogStats(ctx, queueRef.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if got := early[c.state]; got.Total != after[c.state].Total || got.OldestSeconds != 0 {
			t.Errorf("%s uma hora antes: %+v", c.state, got)
		}
	}
}

// As funções agregadas são executáveis somente pelo papel da aplicação (e
// pelo dono), não por PUBLIC, e as consultas usam os índices parciais do
// trabalho em aberto.
func TestQueueStatsFunctionsPrivilegesAndIndexes(t *testing.T) {
	ctx := context.Background()
	owner := pgtest.OwnerPool(t)
	for _, fn := range []string{"app_dead_letter_stats(timestamptz)", "app_notification_backlog_stats(timestamptz, timestamptz)",
		"app_webhook_delivery_backlog_stats(timestamptz)"} {
		var app, public, definer bool
		err := owner.QueryRow(ctx, `SELECT has_function_privilege('recorvia_app', p.oid, 'EXECUTE'),
				EXISTS (SELECT 1 FROM aclexplode(p.proacl) a WHERE a.grantee = 0 AND a.privilege_type = 'EXECUTE'),
				p.prosecdef
			FROM pg_proc p WHERE p.oid = $1::regprocedure`, fn).Scan(&app, &public, &definer)
		if err != nil {
			t.Fatalf("%s: %v", fn, err)
		}
		if !app || public || !definer {
			t.Errorf("%s: aplicação %v, PUBLIC %v, SECURITY DEFINER %v", fn, app, public, definer)
		}
	}
	for index, predicate := range map[string]string{
		"dead_letters_open_idx":    "status = 'open'",
		"notifications_failed_idx": "status = 'failed'",
	} {
		var def string
		if err := owner.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, index).Scan(&def); err != nil {
			t.Fatalf("%s: %v", index, err)
		}
		if !strings.Contains(def, predicate) {
			t.Errorf("%s sem o predicado %q: %s", index, predicate, def)
		}
	}
}

// A seção Down da migration das métricas de fila remove as funções e os
// índices, e a Up pode ser reaplicada em seguida (numa transação desfeita ao
// fim, como dono das tabelas).
func TestQueueMetricsMigrationDownAndUpAgain(t *testing.T) {
	ctx := context.Background()
	raw, err := db.Migrations.ReadFile("migrations/00015_metricas_de_fila.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("migration sem seção Down")
	}
	_, up, ok = strings.Cut(up, "-- +goose Up")
	if !ok {
		t.Fatal("migration sem seção Up")
	}
	owner := pgtest.OwnerPool(t)
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	count := func() (funcs, indexes int) {
		t.Helper()
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname IN ('app_dead_letter_stats', 'app_notification_backlog_stats',
			'app_webhook_delivery_backlog_stats')`).Scan(&funcs); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public'
			AND indexname IN ('dead_letters_open_idx', 'notifications_failed_idx')`).Scan(&indexes); err != nil {
			t.Fatal(err)
		}
		return funcs, indexes
	}
	if f, i := count(); f != 3 || i != 2 {
		t.Fatalf("antes do Down: %d funções e %d índices", f, i)
	}
	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if f, i := count(); f != 0 || i != 0 {
		t.Fatalf("Down deixou objetos: %d funções e %d índices", f, i)
	}
	// As tabelas e os dados permanecem.
	for _, table := range []string{"dead_letters", "notifications", "webhook_deliveries"} {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
			t.Fatalf("tabela %s depois do Down: %v %v", table, exists, err)
		}
	}
	if _, err := tx.Exec(ctx, up); err != nil {
		t.Fatalf("Up após Down: %v", err)
	}
	if f, i := count(); f != 3 || i != 2 {
		t.Fatalf("Up após Down: %d funções e %d índices", f, i)
	}
}
