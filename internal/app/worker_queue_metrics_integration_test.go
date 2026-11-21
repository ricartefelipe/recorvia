//go:build integration

package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// A tarefa metrics.collect publica, com o relógio da aplicação, as dead
// letters abertas por consumidor e as filas de notificações e de entregas de
// webhooks de todos os tenants, sem rótulos de tenant, e os medidores voltam
// ao valor anterior quando o trabalho é resolvido.
//
// O banco é compartilhado pelos testes do pacote, que usam datas atuais: o
// relógio deste teste fica em 2001, de modo que nada de outro teste está
// vencido e as idades medidas são as das linhas criadas aqui; os totais são
// comparados com a leitura anterior.
func TestWorkerCollectsQueueMetrics(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ref := time.Date(2001, 8, 14, 9, 30, 0, 0, time.UTC)
	env := fixture.New(t, 4, ref)
	a, b := env.NewTenant(t, "fila-metricas-a"), env.NewTenant(t, "fila-metricas-b")
	metrics := telemetry.NewMetrics("worker")
	w := &Worker{Core: &Core{Store: env.Store, Clock: env.Clock, Log: log}, Metrics: metrics, Conn: rabbit.NewConn("amqp://indisponivel", log)}
	collect := func() {
		t.Helper()
		if err := w.job(t, "metrics.collect")(ctx); err != nil {
			t.Fatal(err)
		}
	}
	gauge := func(name, label, value string) float64 {
		t.Helper()
		v, ok := metricValue(t, metrics, name, label, value)
		if !ok {
			t.Fatalf("série %s{%s=%q} ausente", name, label, value)
		}
		return v
	}
	exec := func(tenantID uuid.UUID, sql string, args ...any) {
		t.Helper()
		err := env.DB.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Encerra o trabalho em fila: as tarefas de envio de outros testes
	// percorrem todos os tenants com trabalho vencido.
	closeQueued := func() {
		for _, id := range []uuid.UUID{a.ID, b.ID} {
			exec(id, `UPDATE notifications SET status = 'canceled', status_reason = 'teste encerrado', lease_until = NULL
				WHERE tenant_id = $1 AND status IN ('scheduled', 'sending')`, id)
			exec(id, `UPDATE webhook_deliveries SET status = 'succeeded', delivered_at = $2, next_attempt_at = NULL
				WHERE tenant_id = $1 AND status IN ('pending', 'retrying')`, id, env.Clock.Now())
			exec(id, `UPDATE dead_letters SET status = 'discarded', resolved_at = $2, resolution_note = 'teste encerrado'
				WHERE tenant_id = $1 AND status = 'open'`, id, env.Clock.Now())
		}
	}
	t.Cleanup(closeQueued)

	collect()
	// Todas as séries existem desde a primeira coleta, com o conjunto
	// fechado de rótulos.
	type reading struct{ open, other, due, sending, failed, pending, retrying float64 }
	read := func() reading {
		t.Helper()
		return reading{
			open:     gauge("recorvia_dead_letters_open", "consumer", payments.ConsumerWebhooks),
			other:    gauge("recorvia_dead_letters_open", "consumer", telemetry.OtherConsumer),
			due:      gauge("recorvia_notifications_backlog", "state", "due"),
			sending:  gauge("recorvia_notifications_backlog", "state", "sending"),
			failed:   gauge("recorvia_notifications_backlog", "state", "failed"),
			pending:  gauge("recorvia_webhook_deliveries_backlog", "state", "pending"),
			retrying: gauge("recorvia_webhook_deliveries_backlog", "state", "retrying"),
		}
	}
	before := read()
	for _, spec := range consumerSpecs {
		gauge("recorvia_dead_letters_open", "consumer", spec.Name)
		gauge("recorvia_dead_letters_oldest_open_seconds", "consumer", spec.Name)
	}
	if before.due != 0 || before.failed != 0 {
		t.Fatalf("o relógio do teste deve ser anterior aos dados dos demais testes: %+v", before)
	}

	// Dead letters de dois tenants no mesmo consumidor e uma de um consumidor
	// que o worker não tem mais.
	removed := "payments.removido." + uuid.NewString()[:8]
	for _, d := range []pgstore.DeadLetter{
		{TenantID: &a.ID, Consumer: payments.ConsumerWebhooks, FailedAt: ref.Add(-2 * time.Hour)},
		{TenantID: &b.ID, Consumer: payments.ConsumerWebhooks, FailedAt: ref.Add(-5 * time.Minute)},
		{TenantID: &b.ID, Consumer: removed, FailedAt: ref.Add(-26 * time.Hour)},
	} {
		d.ID, d.MessageID, d.Error, d.Attempts, d.Payload = ids.New(), uuid.NewString(), "falha de teste", 6, []byte(`{}`)
		if err := env.Store.RecordDeadLetter(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	notification := func(tenantID uuid.UUID, status string, sendAfter, updatedAt time.Time) {
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
		exec(tenantID, `INSERT INTO notifications (id, tenant_id, business_key, template_key, recipients, subject, content_ciphertext,
				content_key_id, status, send_after, attempts, max_attempts, lease_until, created_at, updated_at)
			VALUES ($1, $2, $3, 'invoice_issued', '{financeiro@cliente.test}', 'Fatura', $4, $5, $6, $7, 1, 5, $8, $9, $9)`,
			ids.New(), tenantID, "fila-"+uuid.NewString(), content, keyID, status, sendAfter, lease, updatedAt)
	}
	notification(a.ID, "scheduled", ref.Add(-25*time.Minute), ref.Add(-time.Hour))
	notification(b.ID, "scheduled", ref.Add(-time.Minute), ref.Add(-time.Hour))
	notification(b.ID, "scheduled", ref.Add(48*time.Hour), ref) // lembrete futuro: fora da fila
	notification(a.ID, "sending", ref.Add(-time.Hour), ref.Add(-40*time.Second))
	notification(b.ID, "failed", ref.Add(-3*time.Hour), ref.Add(-2*time.Hour))
	notification(a.ID, "failed", ref.Add(-80*time.Hour), ref.Add(-30*time.Hour)) // fora da janela de 24 horas
	delivery := func(tenantID uuid.UUID, status string, next time.Time) {
		t.Helper()
		endpointID, eventID := ids.New(), ids.New()
		exec(tenantID, `INSERT INTO webhook_endpoints (id, tenant_id, url, event_types, status, created_at, updated_at)
			VALUES ($1, $2, 'https://integracao.cliente.test/webhooks', '{invoice.paid}', 'active', $3, $3)`, endpointID, tenantID, ref.Add(-72*time.Hour))
		exec(tenantID, `INSERT INTO webhook_events (id, tenant_id, event_type, payload, occurred_at, created_at)
			VALUES ($1, $2, 'invoice.paid', '{}', $3, $3)`, eventID, tenantID, ref.Add(-48*time.Hour))
		exec(tenantID, `INSERT INTO webhook_deliveries (id, tenant_id, endpoint_id, event_id, event_type, status, attempts, max_attempts,
				next_attempt_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'invoice.paid', $5, 0, 10, $6, $7, $7)`, ids.New(), tenantID, endpointID, eventID, status, next, ref.Add(-48*time.Hour))
	}
	delivery(a.ID, "pending", ref.Add(-18*time.Minute))
	delivery(b.ID, "pending", ref.Add(-30*time.Second))
	delivery(a.ID, "retrying", ref.Add(-75*time.Second))
	delivery(b.ID, "retrying", ref.Add(5*time.Hour)) // em backoff: conta no total, não no atraso

	collect()
	after := read()
	deltas := []struct {
		name      string
		got, want float64
	}{
		{"dead letters de " + payments.ConsumerWebhooks, after.open - before.open, 2},
		{"dead letters de consumidor removido (other)", after.other - before.other, 1},
		{"notificações vencidas", after.due - before.due, 2},
		{"notificações em envio", after.sending - before.sending, 1},
		{"notificações com falha na janela", after.failed - before.failed, 1},
		{"entregas pendentes", after.pending - before.pending, 2},
		{"entregas em retentativa", after.retrying - before.retrying, 2},
	}
	for _, d := range deltas {
		if d.got != d.want {
			t.Errorf("%s: variação %v, esperado %v", d.name, d.got, d.want)
		}
	}
	ages := []struct {
		name, label, value string
		want               time.Duration
	}{
		{"recorvia_dead_letters_oldest_open_seconds", "consumer", payments.ConsumerWebhooks, 2 * time.Hour},
		{"recorvia_dead_letters_oldest_open_seconds", "consumer", telemetry.OtherConsumer, 26 * time.Hour},
		{"recorvia_notifications_backlog_oldest_seconds", "state", "due", 25 * time.Minute},
		{"recorvia_notifications_backlog_oldest_seconds", "state", "sending", 40 * time.Second},
		{"recorvia_notifications_backlog_oldest_seconds", "state", "failed", 2 * time.Hour},
		{"recorvia_webhook_deliveries_backlog_oldest_due_seconds", "state", "pending", 18 * time.Minute},
		{"recorvia_webhook_deliveries_backlog_oldest_due_seconds", "state", "retrying", 75 * time.Second},
	}
	for _, c := range ages {
		if got := gauge(c.name, c.label, c.value); got != c.want.Seconds() {
			t.Errorf("%s{%s=%q} = %v s, esperado %v s", c.name, c.label, c.value, got, c.want.Seconds())
		}
	}

	// As idades acompanham o relógio da aplicação, não o do banco.
	env.Clock.Advance(10 * time.Minute)
	collect()
	if got := gauge("recorvia_notifications_backlog_oldest_seconds", "state", "due"); got != (35 * time.Minute).Seconds() {
		t.Errorf("atraso das vencidas dez minutos depois = %v s", got)
	}
	if got := gauge("recorvia_webhook_deliveries_backlog_oldest_due_seconds", "state", "pending"); got != (28 * time.Minute).Seconds() {
		t.Errorf("atraso das entregas pendentes dez minutos depois = %v s", got)
	}

	// Nenhuma série identifica o tenant nem o consumidor fora do conjunto
	// fechado.
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, s := range f.GetMetric() {
			for _, l := range s.GetLabel() {
				if l.GetValue() == a.ID.String() || l.GetValue() == b.ID.String() || strings.Contains(l.GetName(), "tenant") || l.GetValue() == removed {
					t.Fatalf("métrica %s com rótulo %s=%q", f.GetName(), l.GetName(), l.GetValue())
				}
			}
		}
	}

	// Resolvido o trabalho, os medidores voltam à leitura inicial.
	closeQueued()
	collect()
	resolved := read()
	resolved.failed, before.failed = 0, 0 // a falha definitiva permanece na janela
	if resolved != before {
		t.Errorf("depois de resolver: %+v, esperado %+v", resolved, before)
	}
	if got := gauge("recorvia_notifications_backlog_oldest_seconds", "state", "due"); got != 0 {
		t.Errorf("atraso das vencidas com a fila vazia = %v s", got)
	}
}
