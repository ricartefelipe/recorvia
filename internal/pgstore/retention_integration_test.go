//go:build integration

package pgstore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ricartefelipe/recorvia/db"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/retention"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// scopedRetention limita a descoberta aos tenants do teste: o banco é
// compartilhado com os demais testes do pacote.
type scopedRetention struct {
	retention.Store
	only []uuid.UUID
}

func (s scopedRetention) Tenants(ctx context.Context, k retention.Kind, cutoff time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	all, err := s.Store.Tenants(ctx, k, cutoff, nil, 100000)
	if err != nil {
		return nil, err
	}
	var out []uuid.UUID
	for _, id := range all {
		if slices.Contains(s.only, id) && (after == nil || bytes.Compare(id[:], after[:]) > 0) && len(out) < limit {
			out = append(out, id)
		}
	}
	return out, nil
}

func retentionService(t *testing.T, env *fixture.Env, tenants ...uuid.UUID) *retention.Service {
	t.Helper()
	p := retention.DefaultPolicy()
	p.BatchSize = retention.MinBatchSize
	svc, err := retention.NewService(scopedRetention{Store: env.Store.Retention(), only: tenants}, env.Clock, p, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// seeder grava, como dono das tabelas, os cenários que a aplicação só produz
// ao longo de meses de operação.
type seeder struct {
	t    *testing.T
	pool *pgxpool.Pool
}

func (s seeder) exec(query string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(context.Background(), query, args...); err != nil {
		s.t.Fatalf("%v\n%s", err, query)
	}
}

func (s seeder) count(query string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		s.t.Fatalf("%v\n%s", err, query)
	}
	return n
}

func randomHash(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func daysAgo(n int) time.Time { return now.AddDate(0, 0, -n) }

func (s seeder) inbox(tenant uuid.UUID, processedAt time.Time) uuid.UUID {
	id := ids.New()
	s.exec(`INSERT INTO inbox_messages (tenant_id, consumer, message_id, processed_at) VALUES ($1, 'retencao.teste', $2, $3)`, tenant, id, processedAt)
	return id
}

func (s seeder) deadLetter(tenant *uuid.UUID, status string, failedAt time.Time, resolvedAt *time.Time) uuid.UUID {
	id := ids.New()
	s.exec(`INSERT INTO dead_letters (id, tenant_id, consumer, message_id, event_type, payload, error, attempts, status, failed_at, resolved_at, resolution_note)
		VALUES ($1, $2, 'retencao.teste', $3, 'invoice.paid', '{"invoice_id":"x"}', 'falha', 5, $4, $5, $6, $7)`,
		id, tenant, id.String(), status, failedAt, resolvedAt, map[bool]*string{true: nil, false: new("tratada")}[resolvedAt == nil])
	return id
}

func (s seeder) webhookEndpoint(tenant uuid.UUID) uuid.UUID {
	id := ids.New()
	s.exec(`INSERT INTO webhook_endpoints (id, tenant_id, url, event_types, status, created_at, updated_at)
		VALUES ($1, $2, $3, '{invoice.paid}', 'active', $4, $4)`, id, tenant, "https://hooks.cliente.test/"+id.String(), daysAgo(700))
	return id
}

func (s seeder) webhookEvent(tenant uuid.UUID, createdAt time.Time) uuid.UUID {
	id := ids.New()
	s.exec(`INSERT INTO webhook_events (id, tenant_id, event_type, payload, occurred_at, created_at)
		VALUES ($1, $2, 'invoice.paid', '{"invoice_id":"x"}', $3, $3)`, id, tenant, createdAt)
	return id
}

// webhookDelivery grava a entrega na situação pedida, com as tentativas.
func (s seeder) webhookDelivery(tenant, endpoint, event uuid.UUID, status string, updatedAt time.Time, attempts int) uuid.UUID {
	id := ids.New()
	var next, lease, delivered *time.Time
	switch status {
	case "pending", "retrying":
		next = &updatedAt
	case "delivering":
		lease = &updatedAt
	case "succeeded":
		delivered = &updatedAt
	}
	s.exec(`INSERT INTO webhook_deliveries (id, tenant_id, endpoint_id, event_id, event_type, status, attempts, max_attempts, next_attempt_at,
			lease_until, delivered_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'invoice.paid', $5, $6, 8, $7, $8, $9, $10, $10)`,
		id, tenant, endpoint, event, status, attempts, next, lease, delivered, updatedAt)
	for i := 1; i <= attempts; i++ {
		s.exec(`INSERT INTO webhook_delivery_attempts (id, tenant_id, delivery_id, attempt, started_at, duration_ms, status_code, signature_count)
			VALUES ($1, $2, $3, $4, $5, 12, 503, 1)`, ids.New(), tenant, id, i, updatedAt)
	}
	return id
}

func (s seeder) webhookSecret(tenant, endpoint uuid.UUID, createdAt time.Time, expiresAt *time.Time) uuid.UUID {
	id := ids.New()
	s.exec(`INSERT INTO webhook_endpoint_secrets (id, tenant_id, endpoint_id, secret_ciphertext, secret_key_id, created_at, expires_at)
		VALUES ($1, $2, $3, '\x01', 't1', $4, $5)`, id, tenant, endpoint, createdAt, expiresAt)
	return id
}

func (s seeder) providerAccount(tenant uuid.UUID) uuid.UUID {
	id := ids.New()
	s.exec(`INSERT INTO payment_provider_accounts (id, tenant_id, provider, environment, webhook_endpoint_token_hash)
		VALUES ($1, $2, 'simulator', 'local', $3)`, id, tenant, randomHash(s.t))
	return id
}

func (s seeder) providerWebhook(tenant, account uuid.UUID, status string, receivedAt time.Time, processedAt *time.Time) uuid.UUID {
	id := ids.New()
	s.exec(`INSERT INTO provider_webhook_events (id, tenant_id, provider_account_id, provider, provider_event_id, event_type, provider_charge_id,
			received_at, payload, status, processed_at)
		VALUES ($1, $2, $3, 'simulator', $4, 'charge.paid', 'ch_teste', $5, '{"payer":{"name":"Maria Pagadora","email":"maria@pagador.test"}}', $6, $7)`,
		id, tenant, account, "evt_"+id.String(), receivedAt, status, processedAt)
	return id
}

type seededNotification struct {
	status   string
	at       time.Time
	content  bool
	customer *uuid.UUID
	invoice  *uuid.UUID
	invite   *uuid.UUID
	events   int
}

func (s seeder) notification(tenant uuid.UUID, n seededNotification) uuid.UUID {
	id := ids.New()
	var content []byte
	var keyID, reason *string
	var sentAt *time.Time
	recipients := []string{"financeiro@cliente.test"}
	if n.content {
		content, keyID = []byte{1, 2, 3}, new("t1")
	}
	switch n.status {
	case "sent":
		sentAt = &n.at
	case "canceled", "suppressed":
		reason = new("no_recipient")
	}
	if n.status == "suppressed" {
		recipients = []string{}
	}
	s.exec(`INSERT INTO notifications (id, tenant_id, business_key, template_key, recipients, subject, content_ciphertext, content_key_id,
			customer_id, invoice_id, invitation_id, status, status_reason, send_after, attempts, max_attempts, last_error, sent_at, created_at, updated_at)
		VALUES ($1, $2, $3, 'invoice_issued', $4, 'Fatura de Maria Pagadora', $5, $6, $7, $8, $9, $10, $11, $12, 1, 5, NULL, $13, $12, $12)`,
		id, tenant, "retencao:"+id.String(), recipients, content, keyID, n.customer, n.invoice, n.invite, n.status, reason, n.at, sentAt)
	for range n.events {
		s.exec(`INSERT INTO notification_events (id, tenant_id, notification_id, status, reason, attempt, detail, occurred_at)
			VALUES ($1, $2, $3, $4, 'registro', 1, 'Destinatários recusados: maria@pagador.test', $5)`, ids.New(), tenant, id, n.status, n.at)
	}
	return id
}

func (s seeder) portalLink(tenant, customer, author uuid.UUID, createdAt, expiresAt time.Time, usedAt, revokedAt *time.Time) uuid.UUID {
	id := ids.New()
	var reason *string
	if revokedAt != nil {
		reason = new("superseded")
	}
	s.exec(`INSERT INTO customer_portal_links (id, tenant_id, customer_id, token_hash, created_by_user_id, created_at, expires_at, used_at,
			revoked_at, revoke_reason) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id, tenant, customer, randomHash(s.t), author, createdAt, expiresAt, usedAt, revokedAt, reason)
	return id
}

func (s seeder) metric(tenant uuid.UUID, graceHours int) uuid.UUID {
	id := ids.New()
	s.exec(`INSERT INTO billable_metrics (id, tenant_id, code, name, unit, aggregation, max_quantity_per_event, late_grace_hours)
		VALUES ($1, $2, $3, 'Chamadas de API', 'chamada', 'sum', 1000000, $4)`, id, tenant, "api-"+id.String()[24:], graceHours)
	return id
}

func (s seeder) usageAggregate(tenant, sub, metric uuid.UUID, start, end time.Time) uuid.UUID {
	id := ids.New()
	s.exec(`INSERT INTO usage_aggregates (id, tenant_id, subscription_id, metric_id, period_start, period_end, aggregation, quantity,
			event_count, closed_at) VALUES ($1, $2, $3, $4, $5, $6, 'sum', 42, 3, $6)`, id, tenant, sub, metric, start, end)
	return id
}

// usageEvent grava um evento do período; aggregate nulo com status accepted é
// consumo ainda não faturado e late sem correctedAt é correção pendente.
func (s seeder) usageEvent(tenant, customer, sub, metric uuid.UUID, periodStart time.Time, status string, aggregate *uuid.UUID, correctedAt *time.Time) uuid.UUID {
	id := ids.New()
	s.exec(`INSERT INTO usage_events (id, tenant_id, external_id, customer_id, subscription_id, metric_id, quantity, occurred_at, received_at,
			period_start, status, aggregate_id, corrected_at, properties)
		VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $7, $8, $9, $10, $11, '{"origem":"teste"}')`,
		id, tenant, "uso-"+id.String(), customer, sub, metric, periodStart.Add(time.Hour), periodStart, status, aggregate, correctedAt)
	return id
}

func (s seeder) invitation(tenant, author uuid.UUID, expiresAt time.Time, acceptedAt, revokedAt *time.Time) uuid.UUID {
	id := ids.New()
	var acceptedBy, revokedBy *uuid.UUID
	if acceptedAt != nil {
		acceptedBy = &author
	}
	if revokedAt != nil {
		revokedBy = &author
	}
	s.exec(`INSERT INTO invitations (id, tenant_id, email, role, token_hash, invited_by, expires_at, accepted_at, accepted_by, revoked_at,
			revoked_by, created_at) VALUES ($1, $2, $3, 'operator', $4, $5, $6, $7, $8, $9, $10, $11)`,
		id, tenant, "convidada-"+id.String()[24:]+"@empresa.test", randomHash(s.t), author, expiresAt, acceptedAt, acceptedBy, revokedAt,
		revokedBy, expiresAt.AddDate(0, 0, -7))
	return id
}

func (s seeder) exists(table string, id uuid.UUID) bool {
	s.t.Helper()
	return s.count(`SELECT count(*) FROM `+table+` WHERE id = $1`, id) == 1
}

func (s seeder) want(table string, gone, kept []uuid.UUID) {
	s.t.Helper()
	for i, id := range gone {
		if s.exists(table, id) {
			s.t.Errorf("%s: o registro vencido %d deveria ter sido removido", table, i)
		}
	}
	for i, id := range kept {
		if !s.exists(table, id) {
			s.t.Errorf("%s: o registro %d ainda é necessário e foi removido", table, i)
		}
	}
}

// O expurgo remove, tipo a tipo e em lotes, somente o que venceu e não é mais
// necessário, sem tocar em outro tenant enquanto ele não é processado.
func TestRetentionPurgesExpiredRecordsAndKeepsWhatIsStillNeeded(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	s := seeder{t: t, pool: pgtest.OwnerPool(t)}
	ta, tb := env.NewTenant(t, "retencao-a"), env.NewTenant(t, "retencao-b")
	custA, openInvoice := issuedInvoice(t, env, ta, "")
	custA2 := env.NewCustomer(t, ta)
	subA := env.NewSubscription(t, ta, custA2.ID, env.NewFlatPlan(t, ta, 5000).ID, nil)
	custB := env.NewCustomer(t, tb)
	at := func(d int) *time.Time { v := daysAgo(d); return &v }

	// Inbox: 90 dias a partir do processamento.
	var inboxOld []uuid.UUID
	for range 13 {
		inboxOld = append(inboxOld, s.inbox(ta.ID, daysAgo(91)))
	}
	inboxRecent := s.inbox(ta.ID, daysAgo(89))
	inboxB := s.inbox(tb.ID, daysAgo(200))

	// Dead letters: 180 dias a partir do tratamento; abertas nunca saem.
	dlReplayed := s.deadLetter(&ta.ID, "replayed", daysAgo(400), at(181))
	dlDiscarded := s.deadLetter(&ta.ID, "discarded", daysAgo(400), at(181))
	dlOpen := s.deadLetter(&ta.ID, "open", daysAgo(900), nil)
	dlRecent := s.deadLetter(&ta.ID, "replayed", daysAgo(400), at(179))
	dlUntenanted := s.deadLetter(nil, "open", daysAgo(900), nil)
	dlB := s.deadLetter(&tb.ID, "discarded", daysAgo(400), at(300))

	// Webhooks enviados: 90 dias a partir da conclusão da entrega.
	ep1, ep2 := s.webhookEndpoint(ta.ID), s.webhookEndpoint(ta.ID)
	ev1, ev2, ev3 := s.webhookEvent(ta.ID, daysAgo(200)), s.webhookEvent(ta.ID, daysAgo(200)), s.webhookEvent(ta.ID, daysAgo(200))
	ev4, ev5, ev6 := s.webhookEvent(ta.ID, daysAgo(50)), s.webhookEvent(ta.ID, daysAgo(200)), s.webhookEvent(ta.ID, daysAgo(200))
	dSucceeded := s.webhookDelivery(ta.ID, ep1, ev1, "succeeded", daysAgo(100), 2)
	dRetrying := s.webhookDelivery(ta.ID, ep1, ev2, "retrying", daysAgo(199), 3)
	dExhausted := s.webhookDelivery(ta.ID, ep1, ev3, "exhausted", daysAgo(95), 8)
	dRecent := s.webhookDelivery(ta.ID, ep1, ev4, "succeeded", daysAgo(50), 1)
	dSiblingDone := s.webhookDelivery(ta.ID, ep1, ev6, "succeeded", daysAgo(100), 1)
	dSiblingBusy := s.webhookDelivery(ta.ID, ep2, ev6, "delivering", daysAgo(199), 1)
	dPending := s.webhookDelivery(ta.ID, ep2, ev2, "pending", daysAgo(199), 0)
	secretCurrent := s.webhookSecret(ta.ID, ep1, daysAgo(10), nil)
	secretExpired := s.webhookSecret(ta.ID, ep1, daysAgo(300), at(100))
	secretInWindow := s.webhookSecret(ta.ID, ep1, daysAgo(40), at(10))

	// Webhooks recebidos: o corpo sai 90 dias depois do processamento.
	acct := s.providerAccount(ta.ID)
	pwProcessed := s.providerWebhook(ta.ID, acct, "processed", daysAgo(91), at(91))
	pwIgnored := s.providerWebhook(ta.ID, acct, "ignored", daysAgo(91), at(91))
	pwReceived := s.providerWebhook(ta.ID, acct, "received", daysAgo(300), nil)
	pwRecent := s.providerWebhook(ta.ID, acct, "processed", daysAgo(30), at(30))
	pwFailed := s.providerWebhook(ta.ID, acct, "failed", daysAgo(300), at(300))

	// Notificações: conteúdo sai em 90 dias; a linha, em 365.
	nContentOnly := s.notification(ta.ID, seededNotification{status: "sent", at: daysAgo(100), content: true, customer: &custA.ID, events: 2})
	nOld := s.notification(ta.ID, seededNotification{status: "sent", at: daysAgo(400), content: true, customer: &custA2.ID, events: 2})
	nScheduled := s.notification(ta.ID, seededNotification{status: "scheduled", at: daysAgo(400), content: true, customer: &custA.ID, events: 1})
	nOpenInvoice := s.notification(ta.ID, seededNotification{status: "sent", at: daysAgo(400), content: true, customer: &custA.ID,
		invoice: &openInvoice, events: 1})
	nFailed := s.notification(ta.ID, seededNotification{status: "failed", at: daysAgo(400), content: true, events: 3})
	nRecent := s.notification(ta.ID, seededNotification{status: "sent", at: daysAgo(10), content: true, events: 1})
	nSuppressed := s.notification(ta.ID, seededNotification{status: "suppressed", at: daysAgo(400), events: 1})

	// Links do portal: 90 dias depois do fim da validade.
	linkOld := s.portalLink(ta.ID, custA.ID, ta.Owner.ID, daysAgo(105), daysAgo(100), nil, at(104))
	linkValid := s.portalLink(ta.ID, custA.ID, ta.Owner.ID, daysAgo(1), now.AddDate(0, 0, 2), nil, nil)
	linkRecent := s.portalLink(ta.ID, custA.ID, ta.Owner.ID, daysAgo(13), daysAgo(10), at(12), at(11))
	linkUsed := s.portalLink(ta.ID, custA2.ID, ta.Owner.ID, daysAgo(98), daysAgo(95), at(97), nil)
	linkB := s.portalLink(tb.ID, custB.ID, tb.Owner.ID, daysAgo(205), daysAgo(200), nil, nil)

	// Consumo: 400 dias depois do fim da tolerância de atraso do período.
	m72, m720 := s.metric(ta.ID, 72), s.metric(ta.ID, 720)
	billedStart, graceStart, pendingStart, unbilledStart, emptyStart := daysAgo(470), daysAgo(440), daysAgo(530), daysAgo(600), daysAgo(560)
	aggBilled := s.usageAggregate(ta.ID, subA.ID, m72, billedStart, billedStart.AddDate(0, 0, 30))
	aggGrace := s.usageAggregate(ta.ID, subA.ID, m720, graceStart, graceStart.AddDate(0, 0, 30)) // fim há 410 dias + 30 de tolerância
	aggPending := s.usageAggregate(ta.ID, subA.ID, m72, pendingStart, pendingStart.AddDate(0, 0, 30))
	aggEmpty := s.usageAggregate(ta.ID, subA.ID, m720, emptyStart, emptyStart.AddDate(0, 0, 30))
	var usageBilled []uuid.UUID
	for range 12 {
		usageBilled = append(usageBilled, s.usageEvent(ta.ID, custA2.ID, subA.ID, m72, billedStart, "accepted", &aggBilled, nil))
	}
	usageBilled = append(usageBilled, s.usageEvent(ta.ID, custA2.ID, subA.ID, m72, billedStart, "late", &aggBilled, at(430)))
	usageGrace := s.usageEvent(ta.ID, custA2.ID, subA.ID, m720, graceStart, "accepted", &aggGrace, nil)
	usagePendingBilled := s.usageEvent(ta.ID, custA2.ID, subA.ID, m72, pendingStart, "accepted", &aggPending, nil)
	usagePendingLate := s.usageEvent(ta.ID, custA2.ID, subA.ID, m72, pendingStart, "late", nil, nil)
	usageUnbilled := s.usageEvent(ta.ID, custA2.ID, subA.ID, m72, unbilledStart, "accepted", nil, nil)

	// Convites: 90 dias depois do fim da validade, se não aceitos.
	invExpired := s.invitation(ta.ID, ta.Owner.ID, daysAgo(100), nil, nil)
	invExpiredMail := s.notification(ta.ID, seededNotification{status: "sent", at: daysAgo(107), content: true, invite: &invExpired, events: 2})
	invAccepted := s.invitation(ta.ID, ta.Owner.ID, daysAgo(290), at(295), nil)
	invUnsent := s.invitation(ta.ID, ta.Owner.ID, daysAgo(100), nil, nil)
	invUnsentMail := s.notification(ta.ID, seededNotification{status: "scheduled", at: daysAgo(107), content: true, invite: &invUnsent, events: 1})
	invValid := s.invitation(ta.ID, ta.Owner.ID, now.AddDate(0, 0, 3), nil, nil)
	invRevoked := s.invitation(ta.ID, ta.Owner.ID, daysAgo(95), nil, at(101))

	// Descoberta: os dois tenants têm inbox vencido; a página seguinte ao
	// tenant A não o repete.
	cutoff := now.Add(-retention.Days(90))
	found, err := env.Store.Retention().Tenants(ctx, retention.KindInboxMessages, cutoff, nil, 100000)
	if err != nil || !slices.Contains(found, ta.ID) || !slices.Contains(found, tb.ID) {
		t.Fatalf("descoberta: %v %v", found, err)
	}
	if !slices.IsSortedFunc(found, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }) {
		t.Fatal("descoberta fora da ordem de ID")
	}
	if next, err := env.Store.Retention().Tenants(ctx, retention.KindInboxMessages, cutoff, &ta.ID, 100000); err != nil || slices.Contains(next, ta.ID) {
		t.Fatalf("página seguinte repete o tenant: %v", err)
	}

	res, err := retentionService(t, env, ta.ID).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[retention.Kind]int{
		retention.KindInboxMessages: 13, retention.KindDeadLetters: 2, retention.KindWebhookDeliveries: 3, retention.KindWebhookEvents: 3,
		retention.KindWebhookEndpointSecrets: 1, retention.KindProviderWebhookPayloads: 2,
		// conteúdos: nContentOnly, nOld, nOpenInvoice, nFailed e o e-mail do convite vencido
		retention.KindNotificationContents: 5,
		retention.KindNotifications:        3, retention.KindPortalLinks: 2, retention.KindUsageEvents: 13, retention.KindInvitations: 2,
	}
	for _, k := range retention.Kinds() {
		if res.Purged[k] != want[k] {
			t.Errorf("%s: %d removidos, esperado %d", k, res.Purged[k], want[k])
		}
	}
	if len(res.Incomplete) != 0 || len(res.Failed) != 0 {
		t.Fatalf("execução incompleta: %+v", res)
	}

	if n := s.count(`SELECT count(*) FROM inbox_messages WHERE tenant_id = $1 AND message_id = ANY ($2)`, ta.ID, inboxOld); n != 0 {
		t.Errorf("inbox vencido restante: %d", n)
	}
	if n := s.count(`SELECT count(*) FROM inbox_messages WHERE tenant_id = $1 AND message_id = $2`, ta.ID, inboxRecent); n != 1 {
		t.Error("o registro recente do inbox foi removido")
	}
	s.want("dead_letters", []uuid.UUID{dlReplayed, dlDiscarded}, []uuid.UUID{dlOpen, dlRecent, dlUntenanted, dlB})
	s.want("webhook_deliveries", []uuid.UUID{dSucceeded, dExhausted, dSiblingDone}, []uuid.UUID{dRetrying, dRecent, dSiblingBusy, dPending})
	s.want("webhook_events", []uuid.UUID{ev1, ev3, ev5}, []uuid.UUID{ev2, ev4, ev6})
	s.want("webhook_endpoint_secrets", []uuid.UUID{secretExpired}, []uuid.UUID{secretCurrent, secretInWindow})
	s.want("notifications", []uuid.UUID{nOld, nFailed, nSuppressed, invExpiredMail},
		[]uuid.UUID{nContentOnly, nScheduled, nOpenInvoice, nRecent, invUnsentMail})
	s.want("customer_portal_links", []uuid.UUID{linkOld, linkUsed}, []uuid.UUID{linkValid, linkRecent, linkB})
	s.want("usage_events", usageBilled, []uuid.UUID{usageGrace, usagePendingBilled, usagePendingLate, usageUnbilled})
	s.want("usage_aggregates", nil, []uuid.UUID{aggBilled, aggGrace, aggPending, aggEmpty})
	s.want("invitations", []uuid.UUID{invExpired, invRevoked}, []uuid.UUID{invAccepted, invUnsent, invValid})
	if s.count(`SELECT count(*) FROM inbox_messages WHERE tenant_id = $1 AND message_id = $2`, tb.ID, inboxB) != 1 {
		t.Error("o inbox de outro tenant foi removido")
	}

	// Históricos acompanham o registro principal: nenhuma tentativa ou evento órfão.
	if n := s.count(`SELECT count(*) FROM webhook_delivery_attempts WHERE delivery_id = ANY ($1)`, []uuid.UUID{dSucceeded, dExhausted, dSiblingDone}); n != 0 {
		t.Errorf("tentativas de entregas removidas: %d", n)
	}
	if n := s.count(`SELECT count(*) FROM webhook_delivery_attempts WHERE delivery_id = ANY ($1)`, []uuid.UUID{dRetrying, dRecent, dSiblingBusy}); n != 5 {
		t.Errorf("tentativas de entregas mantidas: %d", n)
	}
	if n := s.count(`SELECT count(*) FROM notification_events WHERE notification_id = ANY ($1)`, []uuid.UUID{nOld, nFailed, nSuppressed, invExpiredMail}); n != 0 {
		t.Errorf("histórico de notificações removidas: %d", n)
	}
	if n := s.count(`SELECT count(*) FROM notification_events WHERE notification_id = ANY ($1)`, []uuid.UUID{nContentOnly, nScheduled, nOpenInvoice, nRecent}); n != 5 {
		t.Errorf("histórico de notificações mantidas: %d", n)
	}

	// Conteúdos: esvaziados e marcados com o relógio da aplicação; a linha
	// mantém destinatários e situação.
	for name, id := range map[string]uuid.UUID{"enviada há 100 dias": nContentOnly, "de fatura em aberto": nOpenInvoice} {
		if n := s.count(`SELECT count(*) FROM notifications WHERE id = $1 AND content_ciphertext IS NULL AND content_key_id IS NULL
			AND content_purged_at = $2 AND status = 'sent' AND cardinality(recipients) = 1`, id, now); n != 1 {
			t.Errorf("conteúdo da notificação %s não foi expurgado como esperado", name)
		}
	}
	for name, id := range map[string]uuid.UUID{"agendada": nScheduled, "recente": nRecent, "de convite por enviar": invUnsentMail} {
		if n := s.count(`SELECT count(*) FROM notifications WHERE id = $1 AND content_ciphertext IS NOT NULL AND content_purged_at IS NULL`, id); n != 1 {
			t.Errorf("conteúdo da notificação %s não deveria ter sido removido", name)
		}
	}
	for name, id := range map[string]uuid.UUID{"processado": pwProcessed, "ignorado": pwIgnored} {
		if n := s.count(`SELECT count(*) FROM provider_webhook_events WHERE id = $1 AND payload = '{}'::jsonb AND payload_purged_at = $2`, id, now); n != 1 {
			t.Errorf("corpo do webhook %s não foi expurgado", name)
		}
	}
	for name, id := range map[string]uuid.UUID{"não processado": pwReceived, "recente": pwRecent, "com falha": pwFailed} {
		if n := s.count(`SELECT count(*) FROM provider_webhook_events WHERE id = $1 AND payload->'payer'->>'name' = 'Maria Pagadora'
			AND payload_purged_at IS NULL`, id); n != 1 {
			t.Errorf("corpo do webhook %s não deveria ter sido removido", name)
		}
	}
	// A linha sem corpo continua deduplicando o reenvio do provedor.
	tag, err := s.pool.Exec(ctx, `INSERT INTO provider_webhook_events (id, tenant_id, provider_account_id, provider, provider_event_id, event_type,
			received_at, payload) SELECT $1, tenant_id, provider_account_id, provider, provider_event_id, event_type, $2, '{"reenvio":true}'
		  FROM provider_webhook_events WHERE id = $3 ON CONFLICT (tenant_id, provider, provider_event_id) DO NOTHING`, ids.New(), now, pwProcessed)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("reenvio do provedor não foi deduplicado: %v %v", tag, err)
	}
	// Agregados: quantidade faturada preservada; marcados quando sem eventos.
	if n := s.count(`SELECT count(*) FROM usage_aggregates WHERE id = ANY ($1) AND events_purged_at = $2 AND quantity = 42`,
		[]uuid.UUID{aggBilled, aggEmpty}, now); n != 2 {
		t.Errorf("agregados com eventos removidos marcados: %d", n)
	}
	if n := s.count(`SELECT count(*) FROM usage_aggregates WHERE id = ANY ($1) AND events_purged_at IS NULL`, []uuid.UUID{aggGrace, aggPending}); n != 2 {
		t.Errorf("agregados dentro da tolerância ou com correção pendente não podem ser marcados: %d", n)
	}

	// Depois do expurgo o tenant deixa de ser descoberto, exceto onde restam
	// registros vencidos que ainda são necessários (correção de consumo
	// pendente e convite com e-mail por enviar).
	stillDiscovered := map[retention.Kind]bool{retention.KindUsageEvents: true, retention.KindInvitations: true}
	policy := retention.DefaultPolicy()
	for _, k := range retention.Kinds() {
		found, err := env.Store.Retention().Tenants(ctx, k, now.Add(-policy.For(k)), nil, 100000)
		if err != nil {
			t.Fatal(err)
		}
		if got := slices.Contains(found, ta.ID); got != stillDiscovered[k] {
			t.Errorf("%s: tenant descoberto depois do expurgo = %v, esperado %v", k, got, stillDiscovered[k])
		}
	}
	// Uma nova execução não encontra mais nada.
	again, err := retentionService(t, env, ta.ID).Run(ctx)
	if err != nil || again.Total() != 0 {
		t.Fatalf("segunda execução: %+v %v", again, err)
	}

	// O outro tenant só é tratado na própria vez.
	resB, err := retentionService(t, env, tb.ID).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resB.Purged[retention.KindInboxMessages] != 1 || resB.Purged[retention.KindDeadLetters] != 1 || resB.Purged[retention.KindPortalLinks] != 1 ||
		resB.Total() != 3 {
		t.Fatalf("tenant B: %+v", resB.Purged)
	}
	s.want("dead_letters", []uuid.UUID{dlB}, []uuid.UUID{dlOpen, dlUntenanted})
	s.want("customer_portal_links", []uuid.UUID{linkB}, []uuid.UUID{linkValid})
}

// Cada lote é uma transação limitada; more indica se resta algo vencido.
func TestRetentionPurgeIsBatched(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	s := seeder{t: t, pool: pgtest.OwnerPool(t)}
	tn := env.NewTenant(t, "retencao-lotes")
	for range 25 {
		s.inbox(tn.ID, daysAgo(120))
	}
	cutoff := now.Add(-retention.Days(90))
	for i, want := range []struct {
		n    int
		more bool
	}{{10, true}, {10, true}, {5, false}, {0, false}} {
		n, more, err := env.Store.Retention().Purge(ctx, tn.ID, retention.KindInboxMessages, cutoff, now, 10)
		if err != nil || n != want.n || more != want.more {
			t.Fatalf("lote %d: %d %v %v, esperado %d %v", i+1, n, more, err, want.n, want.more)
		}
	}

	// Consumo: o limite vale para os eventos; agregados sem eventos são
	// marcados e more continua verdadeiro enquanto houver agregados a visitar.
	cust := env.NewCustomer(t, tn)
	sub := env.NewSubscription(t, tn, cust.ID, env.NewFlatPlan(t, tn, 5000).ID, nil)
	metric := s.metric(tn.ID, 0)
	var aggs []uuid.UUID
	for i := range 3 {
		start := daysAgo(600 + 30*i)
		agg := s.usageAggregate(tn.ID, sub.ID, metric, start, start.AddDate(0, 0, 30))
		aggs = append(aggs, agg)
		for range 6 {
			s.usageEvent(tn.ID, cust.ID, sub.ID, metric, start, "accepted", &agg, nil)
		}
	}
	usageCutoff := now.Add(-retention.Days(400))
	total := 0
	for i := range 10 {
		n, more, err := env.Store.Retention().Purge(ctx, tn.ID, retention.KindUsageEvents, usageCutoff, now, 10)
		if err != nil || n > 10 {
			t.Fatalf("lote de consumo %d: %d %v", i+1, n, err)
		}
		total += n
		if !more {
			break
		}
	}
	if total != 18 {
		t.Fatalf("eventos de consumo removidos: %d", total)
	}
	if n := s.count(`SELECT count(*) FROM usage_aggregates WHERE id = ANY ($1) AND events_purged_at IS NOT NULL`, aggs); n != 3 {
		t.Fatalf("agregados marcados: %d", n)
	}
}

// A remoção só acontece na transação do próprio tenant e com parâmetros
// válidos; tipos fora da lista (como a auditoria) não existem.
func TestRetentionPurgeRequiresTenantContextAndKnownKind(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	s := seeder{t: t, pool: pgtest.OwnerPool(t)}
	ta, tb := env.NewTenant(t, "retencao-ctx-a"), env.NewTenant(t, "retencao-ctx-b")
	victim := s.inbox(tb.ID, daysAgo(500))
	cutoff := now.Add(-retention.Days(90))
	call := `SELECT purged FROM app_retention_purge($1, $2, $3, $4, $5)`

	// Na transação do tenant A, pedir o expurgo do tenant B é recusado.
	err := env.DB.InTenantTx(ctx, ta.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, call, "inbox_messages", tb.ID, cutoff, now, 10)
		return err
	})
	if !postgres.IsInsufficientPrivilege(err) {
		t.Fatalf("expurgo de outro tenant: %v", err)
	}
	// Sem contexto de tenant também.
	err = env.DB.InTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, call, "inbox_messages", tb.ID, cutoff, now, 10)
		return err
	})
	if !postgres.IsInsufficientPrivilege(err) {
		t.Fatalf("expurgo sem contexto de tenant: %v", err)
	}
	if s.count(`SELECT count(*) FROM inbox_messages WHERE tenant_id = $1 AND message_id = $2`, tb.ID, victim) != 1 {
		t.Fatal("registro de outro tenant removido")
	}
	for name, args := range map[string][]any{
		"tipo desconhecido":       {"audit_log", tb.ID, cutoff, now, 10},
		"ledger":                  {"ledger_entries", tb.ID, cutoff, now, 10},
		"corte no futuro":         {"inbox_messages", tb.ID, now.Add(time.Hour), now, 10},
		"lote zero":               {"inbox_messages", tb.ID, cutoff, now, 0},
		"lote acima do permitido": {"inbox_messages", tb.ID, cutoff, now, 10001},
	} {
		err := env.DB.InTenantTx(ctx, tb.ID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, call, args...)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "22023") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := env.Store.Retention().Tenants(ctx, "audit_log", cutoff, nil, 10); err == nil || !strings.Contains(err.Error(), "desconhecido") {
		t.Fatalf("descoberta de tipo desconhecido: %v", err)
	}
	if _, _, err := env.Store.Retention().Purge(ctx, tb.ID, "payments", cutoff, now, 10); err == nil {
		t.Fatal("tipo financeiro não pode ter expurgo")
	}
}

// A exceção dos gatilhos vale somente dentro das funções de retenção: a
// aplicação continua sem privilégio de exclusão nos históricos e o dono das
// tabelas, fora da função, continua barrado pelo gatilho.
func TestRetentionPurgeFlagDoesNotOpenDirectDeletes(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	owner := pgtest.OwnerPool(t)
	s := seeder{t: t, pool: owner}
	tn := env.NewTenant(t, "retencao-gatilho")
	cust := env.NewCustomer(t, tn)
	ev := s.webhookEvent(tn.ID, daysAgo(500))
	nt := s.notification(tn.ID, seededNotification{status: "sent", at: daysAgo(500), content: true, events: 1})
	link := s.portalLink(tn.ID, cust.ID, tn.Owner.ID, daysAgo(505), daysAgo(500), nil, nil)

	for _, table := range []string{"notification_events", "webhook_events", "webhook_delivery_attempts", "customer_portal_links", "notifications",
		"webhook_deliveries", "dead_letters", "usage_events", "invitations", "provider_webhook_events"} {
		var has bool
		if err := env.DB.Pool().QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, 'DELETE')`, table).Scan(&has); err != nil {
			t.Fatal(err)
		}
		if has {
			t.Errorf("o papel da aplicação tem DELETE em %s", table)
		}
	}
	// A aplicação ligar a variável não muda nada: falta o privilégio.
	err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.retention_purge', 'on', true)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM webhook_events WHERE tenant_id = $1 AND id = $2`, tn.ID, ev)
		return err
	})
	if !postgres.IsInsufficientPrivilege(err) {
		t.Fatalf("exclusão direta pela aplicação: %v", err)
	}
	// O dono, fora da função, é barrado pelos gatilhos.
	for name, q := range map[string]string{
		"evento de webhook":        `DELETE FROM webhook_events WHERE id = '` + ev.String() + `'`,
		"histórico de notificação": `DELETE FROM notification_events WHERE notification_id = '` + nt.String() + `'`,
		"link do portal":           `DELETE FROM customer_portal_links WHERE id = '` + link.String() + `'`,
	} {
		if _, err := owner.Exec(ctx, q); !postgres.IsRaisedException(err) {
			t.Errorf("%s excluído fora da função de retenção: %v", name, err)
		}
	}
	// Mesmo com a variável ligada, UPDATE continua recusado.
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.retention_purge', 'on', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE webhook_events SET payload = '{}' WHERE id = $1`, ev); !postgres.IsRaisedException(err) {
		t.Fatalf("UPDATE em histórico imutável: %v", err)
	}
	_ = tx.Rollback(ctx)

	// Depois da função, a variável volta ao estado anterior na mesma transação.
	err = env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT purged FROM app_retention_purge('webhook_events', $1, $2, $3, 10)`, tn.ID, now.Add(-retention.Days(90)), now); err != nil {
			return err
		}
		var flag *string
		if err := tx.QueryRow(ctx, `SELECT NULLIF(current_setting('app.retention_purge', true), '')`).Scan(&flag); err != nil {
			return err
		}
		if flag != nil {
			t.Errorf("app.retention_purge permaneceu %q depois da função", *flag)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.exists("webhook_events", ev) {
		t.Fatal("a função de retenção deveria ter removido o evento vencido")
	}
}

// Registros financeiros e de auditoria ficam de fora da retenção: os gatilhos
// de imutabilidade continuam recusando UPDATE e DELETE, inclusive com a
// variável das funções de retenção ligada, e a aplicação não tem o privilégio.
func TestFinancialAndAuditRecordsAreNeverPurged(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	owner := pgtest.OwnerPool(t)
	s := seeder{t: t, pool: owner}
	tn := env.NewTenant(t, "retencao-financeiro")
	_, invoiceID := issuedInvoice(t, env, tn, "")

	immutable := []string{"ledger_transactions", "ledger_entries", "payments", "invoice_items", "invoice_status_history", "credit_notes",
		"customer_credit_entries", "subscription_history", "subscription_prorations", "audit_log", "plan_price_versions", "dispute_events",
		"dispute_recovery_settlements"}
	for _, table := range immutable {
		// O gatilho da tabela é o de imutabilidade sem exceção.
		if n := s.count(`SELECT count(*) FROM pg_trigger tg JOIN pg_proc p ON p.oid = tg.tgfoid
			WHERE tg.tgrelid = $1::regclass AND NOT tg.tgisinternal AND p.proname = 'app_reject_mutation'
			  AND (tg.tgtype & 8) = 8 AND (tg.tgtype & 16) = 16`, table); n != 1 {
			t.Errorf("%s sem o gatilho de imutabilidade para UPDATE e DELETE", table)
		}
		for _, priv := range []string{"UPDATE", "DELETE"} {
			var has bool
			if err := env.DB.Pool().QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, table, priv).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has {
				t.Errorf("o papel da aplicação tem %s em %s", priv, table)
			}
		}
	}
	// Com dados reais e a variável de retenção ligada, o dono não remove.
	for _, table := range []string{"audit_log", "ledger_entries", "ledger_transactions", "invoice_items", "invoice_status_history", "subscription_history"} {
		if n := s.count(`SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, tn.ID); n == 0 {
			t.Fatalf("%s sem linhas do cenário", table)
		}
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.retention_purge', 'on', true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id = $1`, tn.ID); !postgres.IsRaisedException(err) {
			t.Errorf("DELETE em %s: %v", table, err)
		}
		_ = tx.Rollback(ctx)
	}
	// Fatura emitida: nem exclusão nem reescrita do snapshot do cliente.
	if _, err := owner.Exec(ctx, `DELETE FROM invoices WHERE id = $1`, invoiceID); !postgres.IsRaisedException(err) {
		t.Errorf("DELETE de fatura emitida: %v", err)
	}
	if _, err := owner.Exec(ctx, `UPDATE invoices SET customer_snapshot = '{}' WHERE id = $1`, invoiceID); !postgres.IsRaisedException(err) {
		t.Errorf("reescrita do snapshot da fatura emitida: %v", err)
	}
	// Uma execução do expurgo com dados muito antigos não altera as contagens.
	before := map[string]int{}
	tables := []string{"audit_log", "ledger_entries", "ledger_transactions", "invoices", "invoice_items", "invoice_status_history",
		"subscription_history", "subscriptions", "customers"}
	for _, table := range tables {
		before[table] = s.count(`SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, tn.ID)
	}
	env.Clock.Advance(retention.Days(3000))
	defer env.Clock.Set(now)
	if _, err := retentionService(t, env, tn.ID).Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		if after := s.count(`SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, tn.ID); after != before[table] {
			t.Errorf("%s: %d linhas antes e %d depois do expurgo", table, before[table], after)
		}
	}
}

// A seção Down da migration de retenção desfaz o que a Up criou, mesmo com
// conteúdos já expurgados, e a Up pode ser reaplicada (numa transação
// desfeita ao fim, como dono das tabelas).
func TestRetentionMigrationDownAndUpAgain(t *testing.T) {
	ctx := context.Background()
	raw, err := db.Migrations.ReadFile("migrations/00014_retencao_e_anonimizacao.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("migration sem seção Down")
	}
	env := fixture.New(t, 2, now)
	owner := pgtest.OwnerPool(t)
	s := seeder{t: t, pool: owner}
	tn := env.NewTenant(t, "retencao-down")
	// Uma notificação enviada com conteúdo já expurgado não satisfaz a regra
	// anterior à migration.
	purged := s.notification(tn.ID, seededNotification{status: "sent", at: daysAgo(200), content: true})
	if _, _, err := env.Store.Retention().Purge(ctx, tn.ID, retention.KindNotificationContents, now.Add(-retention.Days(90)), now, 10); err != nil {
		t.Fatal(err)
	}
	cust := env.NewCustomer(t, tn)
	link := s.portalLink(tn.ID, cust.ID, tn.Owner.ID, daysAgo(505), daysAgo(500), nil, nil)

	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	state := func() (functions, columns, indexes, guards int, appDeletes bool) {
		t.Helper()
		err := tx.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM pg_proc WHERE proname IN ('app_retention_tenants', 'app_retention_purge', 'app_customer_delete_notifications',
			                                                 'app_reject_mutation_unless_purge', 'app_customers_anonymized_guard',
			                                                 'app_subscriptions_customer_guard', 'app_invoices_customer_guard')),
			(SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'
			    AND (table_name, column_name) IN (('notifications', 'content_purged_at'), ('provider_webhook_events', 'payload_purged_at'),
			                                      ('usage_aggregates', 'events_purged_at'))),
			(SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname IN ('inbox_messages_processed_idx', 'dead_letters_resolved_idx',
			    'webhook_deliveries_final_idx', 'webhook_deliveries_event_idx', 'webhook_events_created_idx', 'webhook_endpoint_secrets_expired_idx',
			    'provider_webhook_events_payload_idx', 'notifications_content_idx', 'notifications_final_idx', 'notifications_customer_idx',
			    'notifications_invitation_idx', 'customer_portal_links_expiry_idx', 'usage_aggregates_events_idx', 'invitations_unaccepted_idx',
			    'charges_customer_idx')),
			(SELECT count(*) FROM pg_trigger tg JOIN pg_proc p ON p.oid = tg.tgfoid
			  WHERE NOT tg.tgisinternal AND p.proname = 'app_reject_mutation'
			    AND tg.tgrelid IN ('notification_events'::regclass, 'webhook_events'::regclass, 'webhook_delivery_attempts'::regclass)),
			has_table_privilege('recorvia_app', 'customer_billing_addresses', 'DELETE')`).Scan(&functions, &columns, &indexes, &guards, &appDeletes)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if f, c, i, g, d := state(); f != 7 || c != 3 || i != 15 || g != 0 || !d {
		t.Fatalf("antes do Down: %d funções, %d colunas, %d índices, %d gatilhos originais, DELETE=%v", f, c, i, g, d)
	}
	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if f, c, i, g, d := state(); f != 0 || c != 0 || i != 0 || g != 3 || d {
		t.Fatalf("depois do Down: %d funções, %d colunas, %d índices, %d gatilhos originais, DELETE=%v", f, c, i, g, d)
	}
	// A regra anterior volta a valer para novas gravações; a linha já
	// expurgada permanece.
	var def string
	var validated bool
	if err := tx.QueryRow(ctx, `SELECT pg_get_constraintdef(oid), convalidated FROM pg_constraint WHERE conname = 'notifications_deliverable'`).
		Scan(&def, &validated); err != nil || strings.Contains(def, "content_purged_at") || validated {
		t.Fatalf("constraint restaurada: %q validada=%v %v", def, validated, err)
	}
	var kept int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE id = $1 AND content_ciphertext IS NULL`, purged).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("notificação expurgada depois do Down: %d %v", kept, err)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT regra`); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO notifications (id, tenant_id, business_key, template_key, recipients, subject, status, send_after,
		max_attempts, sent_at, created_at, updated_at) VALUES ($1, $2, 'down:regra', 'invoice_issued', '{a@b.test}', 'x', 'sent', now(), 1, now(), now(), now())`,
		ids.New(), tn.ID)
	if !postgres.IsNamedCheckViolation(err, "notifications_deliverable") {
		t.Fatalf("notificação enviada sem conteúdo depois do Down: %v", err)
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT regra`); err != nil {
		t.Fatal(err)
	}
	// O gatilho original do link do portal volta a recusar a exclusão.
	if _, err := tx.Exec(ctx, `SAVEPOINT link`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.retention_purge', 'on', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM customer_portal_links WHERE id = $1`, link); !postgres.IsRaisedException(err) {
		t.Fatalf("exclusão de link depois do Down: %v", err)
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT link`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, strings.TrimPrefix(up, "-- +goose Up")); err != nil {
		t.Fatalf("Up após Down: %v", err)
	}
	if f, c, i, g, d := state(); f != 7 || c != 3 || i != 15 || g != 0 || !d {
		t.Fatalf("depois do Up: %d funções, %d colunas, %d índices, %d gatilhos originais, DELETE=%v", f, c, i, g, d)
	}
}

// Pedidos concorrentes de anonimização do mesmo cliente: um deles executa e
// os demais encontram o cliente já anonimizado; há uma única auditoria.
func TestCustomerAnonymizationIsIdempotentUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 12, now)
	s := seeder{t: t, pool: pgtest.OwnerPool(t)}
	tn := env.NewTenant(t, "anonimizacao-concorrente")
	cust := env.NewCustomer(t, tn)
	for range 3 {
		s.notification(tn.ID, seededNotification{status: "sent", at: daysAgo(3), content: true, customer: &cust.ID, events: 2})
	}
	const workers = 8
	var wg sync.WaitGroup
	results := make(chan customers.Anonymization, workers)
	errs := make(chan error, workers)
	for range workers {
		wg.Go(func() {
			res, err := env.Customers.Anonymize(ctx, tn.ID, tn.Actor, cust.ID, "Pedido de eliminação, protocolo 77")
			if err != nil {
				errs <- err
				return
			}
			results <- res
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("anonimização concorrente falhou: %v", err)
	}
	performed, removed := 0, 0
	for res := range results {
		if !res.Customer.Anonymized() || res.Customer.Name != customers.AnonymizedName {
			t.Fatalf("resultado sem anonimização: %+v", res.Customer)
		}
		if !res.AlreadyAnonymized {
			performed++
			removed = res.Erasure.Notifications
		}
	}
	if performed != 1 || removed != 3 {
		t.Fatalf("execuções efetivas: %d (notificações removidas: %d)", performed, removed)
	}
	if n := s.count(`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'customer.anonymized'`, tn.ID); n != 1 {
		t.Fatalf("entradas de auditoria: %d", n)
	}
	if n := s.count(`SELECT count(*) FROM notifications WHERE tenant_id = $1`, tn.ID) + s.count(`SELECT count(*) FROM notification_events WHERE tenant_id = $1`, tn.ID); n != 0 {
		t.Fatalf("notificações e histórico restantes: %d", n)
	}
}

// A exclusão das notificações de um cliente só existe para a anonimização:
// exige a transação do tenant e o cliente já anonimizado.
func TestCustomerNotificationDeletionRequiresAnonymizedCustomer(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, now)
	s := seeder{t: t, pool: pgtest.OwnerPool(t)}
	ta, tb := env.NewTenant(t, "notif-anon-a"), env.NewTenant(t, "notif-anon-b")
	cust := env.NewCustomer(t, ta)
	kept := s.notification(ta.ID, seededNotification{status: "sent", at: daysAgo(3), content: true, customer: &cust.ID, events: 1})
	call := `SELECT app_customer_delete_notifications($1, $2)`
	// Cliente não anonimizado.
	err := env.DB.InTenantTx(ctx, ta.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, call, ta.ID, cust.ID)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "22023") {
		t.Fatalf("exclusão para cliente não anonimizado: %v", err)
	}
	// Transação de outro tenant e ausência de contexto.
	err = env.DB.InTenantTx(ctx, tb.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, call, ta.ID, cust.ID)
		return err
	})
	if !postgres.IsInsufficientPrivilege(err) {
		t.Fatalf("exclusão a partir de outro tenant: %v", err)
	}
	err = env.DB.InTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, call, ta.ID, cust.ID)
		return err
	})
	if !postgres.IsInsufficientPrivilege(err) {
		t.Fatalf("exclusão sem contexto de tenant: %v", err)
	}
	if !s.exists("notifications", kept) {
		t.Fatal("notificação removida por chamada recusada")
	}
}
