//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	amqp "github.com/rabbitmq/amqp091-go"
	tcrabbit "github.com/testcontainers/testcontainers-go/modules/rabbitmq"

	"github.com/ricartefelipe/recorvia/internal/app"
	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/mailpit"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

// hookRequest é uma entrega recebida pelo sistema do tenant.
type hookRequest struct {
	header http.Header
	body   []byte
}

// hookReceiver simula o sistema do tenant que recebe os webhooks.
type hookReceiver struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []hookRequest
}

func newHookReceiver(t *testing.T) *hookReceiver {
	t.Helper()
	r := &hookReceiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, hookRequest{header: req.Header.Clone(), body: body})
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// ofType devolve as entregas recebidas de um tipo de evento.
func (r *hookReceiver) ofType(eventType string) []hookRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []hookRequest
	for _, q := range r.reqs {
		if q.header.Get(webhooks.HeaderEventType) == eventType {
			out = append(out, q)
		}
	}
	return out
}

// verify confere a assinatura como o destinatário deve fazer. O relógio do
// teste é manual e avança em saltos enquanto entregas podem estar em
// trânsito, então a hora de recebimento nele não representa o atraso real:
// a conferência usa o horário assinado, e a tolerância de horário é coberta
// pelos testes de internal/webhooks.
func (q hookRequest) verify(secret string) error {
	ts := q.header.Get(webhooks.HeaderTimestamp)
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return err
	}
	return webhooks.Verify(secret, q.header.Get(webhooks.HeaderID), ts, q.header.Get(webhooks.HeaderSignature),
		q.body, time.Unix(sec, 0), 5*time.Minute)
}

// e2e reúne o worker real (relay, RabbitMQ, consumidores com inbox e
// agendador com lease) sobre PostgreSQL, Mailpit e um relógio manual.
type e2e struct {
	t       *testing.T
	env     *fixture.Env
	core    *app.Core
	metrics *telemetry.Metrics
	mail    mailpit.Server
	pub     *rabbit.Publisher
	loc     *time.Location
	// start inicia o worker (relay, consumidores e agendador).
	start func()
}

// eventually avança o relógio manual em passos curtos (tornando as tarefas
// do agendador vencidas) até a condição ser atendida ou o prazo acabar.
func (e *e2e) eventually(desc string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("condição não atingida no prazo: %s", desc)
		}
		e.env.Clock.Advance(20 * time.Second)
		<-tick.C
	}
}

// at posiciona o relógio no dia e na hora locais do tenant.
func (e *e2e) at(d calendar.Date, hour int) {
	e.env.Clock.Set(d.StartIn(e.loc).Add(time.Duration(hour) * time.Hour))
}

func (e *e2e) jobRuns(job string) float64 {
	return testutil.ToFloat64(e.metrics.JobRuns.WithLabelValues(job, "success")) + testutil.ToFloat64(e.metrics.JobRuns.WithLabelValues(job, "failure"))
}

func (e *e2e) acked(consumer string) float64 {
	return testutil.ToFloat64(e.metrics.ConsumerProcessed.WithLabelValues(consumer, "ack"))
}

// afterJobRuns avança o relógio até a tarefa executar mais n vezes.
func (e *e2e) afterJobRuns(job string, n float64) {
	e.t.Helper()
	start := e.jobRuns(job)
	e.eventually("execuções de "+job, func() bool { return e.jobRuns(job) >= start+n })
}

func (e *e2e) notifications(tenantID, invoiceID uuid.UUID) map[string]notifications.Notification {
	e.t.Helper()
	page, err := e.core.Notifications.List(context.Background(), tenantID, notifications.ListFilter{InvoiceID: &invoiceID, Limit: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]notifications.Notification{}
	for _, n := range page.Items {
		out[n.BusinessKey] = n
	}
	return out
}

func (e *e2e) mailsWith(recipient, subject string) []mailpit.Message {
	var out []mailpit.Message
	for _, m := range e.mail.MessagesTo(e.t, recipient) {
		if strings.Contains(m.Subject, subject) {
			out = append(out, m)
		}
	}
	return out
}

func startWorker(t *testing.T, start time.Time) *e2e {
	t.Helper()
	return startWorkerWith(t, start, nil)
}

// startWorkerWith inicia o worker com variáveis de configuração adicionais
// (substituem as padrão do teste).
func startWorkerWith(t *testing.T, start time.Time, extra map[string]string) *e2e {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcrabbit.Run(ctx, "rabbitmq:4.2.9-management-alpine")
	if err != nil {
		t.Fatalf("rabbitmq: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("encerramento do broker: %v", err)
		}
	})
	amqpURL, err := ctr.AmqpURL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mp := mailpit.Start(t)
	env := fixture.New(t, 32, start)
	vars := map[string]string{
		"RECORVIA_ENV": string(config.Test), "RECORVIA_SMTP_ADDR": mp.SMTPAddr, "RECORVIA_SMTP_FROM": "cobranca@recorvia.test",
		"RECORVIA_SMTP_TLS": "none", "RECORVIA_SMTP_TIMEOUT": "10s", "RECORVIA_PUBLIC_URL": "https://app.recorvia.test",
		"RECORVIA_WEBHOOK_ALLOWED_PRIVATE_CIDRS": "127.0.0.1/32", "RECORVIA_WORKER_ID": "worker-e2e", "RECORVIA_SCHEDULER_TICK": "50ms",
		"RECORVIA_WORKER_CONCURRENCY": "2", "RECORVIA_OUTBOX_LEASE": "10s", "RECORVIA_AMQP_URL": amqpURL,
	}
	maps.Copy(vars, extra)
	cfg, err := config.Load(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	core, err := app.Assemble(cfg, log, env.DB, env.Keyring, env.Clock)
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	e := &e2e{t: t, env: env, core: core, metrics: telemetry.NewMetrics("worker"), mail: mp, loc: loc}

	// Publicador próprio do teste, para simular reentregas do broker.
	pubConn := rabbit.NewConn(amqpURL, log)
	runCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	connected := make(chan struct{})
	var once sync.Once
	wg.Go(func() {
		pubConn.Run(runCtx, func(*amqp.Connection) error {
			once.Do(func() { close(connected) })
			return nil
		})
	})
	<-connected
	e.pub = rabbit.NewPublisher(pubConn)
	e.start = func() {
		w := &app.Worker{Core: core, Metrics: e.metrics, Conn: rabbit.NewConn(amqpURL, log)}
		wg.Go(func() {
			if err := w.Run(runCtx); err != nil {
				t.Errorf("worker encerrado com erro: %v", err)
			}
		})
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return e
}

// Ponta a ponta com o worker real: a fatura emitida gera e-mail e webhook
// assinado; a fatura vencida dispara o lembrete por e-mail (Mailpit) e o
// webhook invoice.overdue com as duas assinaturas da rotação; reentregas do
// evento não duplicam envios; o pagamento interrompe a régua (o lembrete
// agendado é cancelado e não é enviado) e o webhook invoice.paid só confere
// com o segredo corrente; destinos privados são recusados.
func TestEndToEndDunningNotificationsAndWebhooks(t *testing.T) {
	ctx := context.Background()
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	// O relógio fica no passado: os eventos da outbox ficam disponíveis pelo
	// relógio do banco e o teste avança o tempo de negócio livremente.
	d0 := calendar.DateOf(time.Now(), loc).AddDays(-20)
	e := startWorker(t, d0.StartIn(loc).Add(12*time.Hour))
	env := e.env

	tn := env.NewTenant(t, "ponta-a-ponta")
	if _, err := env.Platform.ChangePlan(ctx, tn.ID, tn.Actor, "profissional"); err != nil {
		t.Fatal(err)
	}
	uid := tn.Owner.ID
	recipient := "e2e-" + ids.New().String()[24:] + "@cliente.test"
	cust := env.NewCustomer(t, tn)
	if _, err := env.Customers.AddContact(ctx, tn.ID, tn.Actor, cust.ID, customers.ContactInput{Name: "Contas a pagar", Email: recipient,
		ReceivesBillingEmail: true}); err != nil {
		t.Fatal(err)
	}
	plan := env.NewFlatPlan(t, tn, 99000)
	env.NewSubscription(t, tn, cust.ID, plan.ID, nil)
	if _, err := e.core.Collections.CreatePolicy(ctx, tn.ID, tn.Actor, &uid, collections.PolicyInput{Name: "Régua", OffsetsDays: []int{1, 3},
		WindowStartHour: 8, WindowEndHour: 20, MaxMessagesPerDay: 2, ReactivateOnPayment: true}); err != nil {
		t.Fatal(err)
	}
	rcv := newHookReceiver(t)
	ep, secret1, err := e.core.Webhooks.CreateEndpoint(ctx, tn.ID, tn.Actor, &uid, webhooks.Input{URL: rcv.srv.URL + "/recorvia",
		EventTypes: []string{outbox.EventInvoiceIssued, outbox.EventInvoiceOverdue, outbox.EventInvoicePaid}})
	if err != nil {
		t.Fatal(err)
	}
	// Destinos privados e de metadados de nuvem são recusados (somente o
	// loopback foi liberado explicitamente na configuração).
	for _, u := range []string{"http://10.1.2.3/hook", "http://169.254.169.254/latest/meta-data", "http://192.168.0.10:8080/hook"} {
		_, _, err := e.core.Webhooks.CreateEndpoint(ctx, tn.ID, tn.Actor, &uid, webhooks.Input{URL: u, EventTypes: []string{outbox.EventInvoicePaid}})
		if ae, ok := apperr.As(err); !ok || ae.Code != "webhook_destination_not_allowed" {
			t.Fatalf("destino %s deveria ser recusado: %v", u, err)
		}
	}

	// 1. Emissão pelo job de faturamento: e-mail e webhook assinado.
	e.start()
	var invoiceID uuid.UUID
	e.eventually("fatura emitida", func() bool {
		return env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM invoices WHERE tenant_id = $1 AND customer_id = $2 AND status = 'open'`, tn.ID, cust.ID).Scan(&invoiceID)
		}) == nil
	})
	e.eventually("e-mail de fatura emitida", func() bool { return len(e.mailsWith(recipient, "emitida")) == 1 })
	e.eventually("webhook invoice.issued", func() bool { return len(rcv.ofType(outbox.EventInvoiceIssued)) == 1 })
	issued := rcv.ofType(outbox.EventInvoiceIssued)[0]
	if err := issued.verify(secret1); err != nil {
		t.Fatalf("assinatura do webhook invoice.issued: %v", err)
	}
	var issuedEvent struct {
		ID         uuid.UUID
		Payload    []byte
		OccurredAt time.Time
	}
	if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id, payload, occurred_at FROM outbox_events WHERE tenant_id = $1 AND event_type = $2 AND aggregate_id = $3`,
			tn.ID, outbox.EventInvoiceIssued, invoiceID).Scan(&issuedEvent.ID, &issuedEvent.Payload, &issuedEvent.OccurredAt)
	}); err != nil {
		t.Fatal(err)
	}
	var body struct {
		ID   string         `json:"id"`
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(issued.body, &body); err != nil || body.ID != issuedEvent.ID.String() || body.Type != outbox.EventInvoiceIssued ||
		body.Data["invoice_id"] != invoiceID.String() || issued.header.Get(webhooks.HeaderID) != issuedEvent.ID.String() {
		t.Fatalf("corpo e identificador estável do webhook: %s %v (%v)", issued.body, issued.header, err)
	}

	// 2. Reentregas do evento: mesmo ID (inbox) e novo ID somente para o
	// consumidor de notificações (identidade de negócio).
	before := e.acked(notifications.ConsumerInvoiceIssued)
	dup := rabbit.Message{ID: issuedEvent.ID.String(), Type: outbox.EventInvoiceIssued, TenantID: tn.ID.String(), Body: issuedEvent.Payload,
		OccurredAt: issuedEvent.OccurredAt}
	if err := e.pub.Publish(ctx, "", dup, 0); err != nil {
		t.Fatal(err)
	}
	replay := dup
	replay.ID = ids.New().String()
	if err := e.pub.Publish(ctx, rabbit.QueueName(notifications.ConsumerInvoiceIssued), replay, 0); err != nil {
		t.Fatal(err)
	}
	e.eventually("reentregas processadas", func() bool { return e.acked(notifications.ConsumerInvoiceIssued) >= before+2 })
	e.afterJobRuns(app.JobNotifications, 2)
	e.afterJobRuns(app.JobWebhooks, 2)
	if n := len(e.mailsWith(recipient, "emitida")); n != 1 {
		t.Fatalf("e-mails de fatura emitida após reentregas = %d, esperado 1", n)
	}
	if n := len(rcv.ofType(outbox.EventInvoiceIssued)); n != 1 {
		t.Fatalf("webhooks invoice.issued após reentrega = %d, esperado 1", n)
	}

	// 3. Rotação com janela: as entregas seguintes levam as duas assinaturas.
	_, secret2, err := e.core.Webhooks.RotateSecret(ctx, tn.ID, tn.Actor, ep.ID, new(int(webhooks.MaxRotationWindow/time.Hour)))
	if err != nil {
		t.Fatal(err)
	}

	// 4. Fatura vencida há 1 dia: lembrete por e-mail e webhook invoice.overdue.
	due := d0.AddDays(5)
	e.at(due.AddDays(1), 12)
	e.eventually("e-mail de fatura vencida", func() bool { return len(e.mailsWith(recipient, "vencida")) == 1 })
	reminder := e.mailsWith(recipient, "vencida")[0]
	if !strings.Contains(reminder.Text, "há 1 dia(s)") || !strings.Contains(reminder.Text, "R$ 990,00") || reminder.To[0].Address != recipient {
		t.Fatalf("lembrete recebido: %+v %q", reminder.Summary, reminder.Text)
	}
	e.eventually("webhook invoice.overdue", func() bool { return len(rcv.ofType(outbox.EventInvoiceOverdue)) == 1 })
	overdue := rcv.ofType(outbox.EventInvoiceOverdue)[0]
	if sigs := strings.Fields(overdue.header.Get(webhooks.HeaderSignature)); len(sigs) != 2 {
		t.Fatalf("durante a rotação a entrega leva duas assinaturas: %v", sigs)
	}
	for name, s := range map[string]string{"novo": secret2, "anterior": secret1} {
		if err := overdue.verify(s); err != nil {
			t.Fatalf("assinatura com o segredo %s: %v", name, err)
		}
	}

	// 5. Passo de 3 dias agendado fora da janela (06:00): fica aguardando.
	e.at(due.AddDays(3), 6)
	step3 := notifications.ReminderKey(invoiceID, 3)
	e.eventually("lembrete do 3º dia agendado", func() bool {
		n, ok := e.notifications(tn.ID, invoiceID)[step3]
		return ok && n.Status == notifications.Scheduled
	})

	// 6. Rotação sem janela e pagamento: o consumidor interrompe a régua.
	_, secret3, err := e.core.Webhooks.RotateSecret(ctx, tn.ID, tn.Actor, ep.ID, new(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.core.Payments.RecordManualPayment(ctx, tn.ID, tn.Actor, &uid, payments.ManualPaymentInput{InvoiceID: invoiceID,
		Amount: money.New(99000, money.BRL), PaidAt: env.Clock.Now(), Method: "pix", RequestKey: "e2e-" + ids.New().String()}); err != nil {
		t.Fatal(err)
	}
	e.eventually("lembrete cancelado pelo pagamento", func() bool {
		n := e.notifications(tn.ID, invoiceID)[step3]
		return n.Status == notifications.Canceled && n.StatusReason == notifications.ReasonInvoicePaid
	})
	if hour := env.Clock.Now().In(loc).Hour(); hour >= 8 {
		t.Fatalf("o cancelamento deveria ocorrer antes da abertura da janela: %d h", hour)
	}

	// 7. Depois da abertura da janela nada mais é enviado pela régua.
	e.at(due.AddDays(3), 9)
	e.eventually("e-mail de pagamento recebido", func() bool { return len(e.mailsWith(recipient, "Pagamento recebido")) == 1 })
	e.eventually("webhook invoice.paid", func() bool { return len(rcv.ofType(outbox.EventInvoicePaid)) == 1 })
	e.afterJobRuns(app.JobCollections, 1)
	e.afterJobRuns(app.JobNotifications, 2)
	if msgs := e.mail.MessagesTo(t, recipient); len(msgs) != 3 {
		subjects := make([]string, 0, len(msgs))
		for _, m := range msgs {
			subjects = append(subjects, m.Subject)
		}
		t.Fatalf("e-mails recebidos: %v (esperado: emitida, vencida e pagamento)", subjects)
	}
	if n := e.notifications(tn.ID, invoiceID)[step3]; n.Status != notifications.Canceled || n.Attempts != 0 {
		t.Fatalf("lembrete após o pagamento: %+v", n)
	}
	paid := rcv.ofType(outbox.EventInvoicePaid)[0]
	if sigs := strings.Fields(paid.header.Get(webhooks.HeaderSignature)); len(sigs) != 1 || paid.verify(secret3) != nil {
		t.Fatalf("após a rotação sem janela, somente o segredo corrente assina: %v", sigs)
	}
	for _, old := range []string{secret1, secret2} {
		if err := paid.verify(old); err == nil {
			t.Fatal("segredo encerrado não pode validar a entrega")
		}
	}

	// Identidade de negócio: uma notificação por fatura e passo.
	keys := e.notifications(tn.ID, invoiceID)
	for _, k := range []string{notifications.InvoiceIssuedKey(invoiceID), notifications.ReminderKey(invoiceID, 1), step3} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("notificação ausente: %s (%v)", k, keys)
		}
	}
	if len(keys) != 4 { // emitida, passo 1, passo 3 e pagamento recebido
		t.Fatalf("notificações da fatura: %v", keys)
	}
	page, err := e.core.Webhooks.Deliveries(ctx, tn.ID, ep.ID, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range page.Items {
		if d.Status != webhooks.Succeeded || d.Attempts != 1 {
			t.Fatalf("entrega %s: %+v", d.EventType, d)
		}
	}
	if len(page.Items) != 3 {
		t.Fatalf("entregas do endpoint: %d, esperado 3", len(page.Items))
	}

	// Métricas por resultado, sem rótulos de tenant.
	for _, m := range []struct {
		name      string
		got, want float64
	}{
		{"e-mails enviados", testutil.ToFloat64(e.metrics.NotificationsDispatched.WithLabelValues("sent")), 3},
		{"webhooks entregues", testutil.ToFloat64(e.metrics.WebhookDeliveries.WithLabelValues("succeeded")), 3},
		{"lembretes agendados", testutil.ToFloat64(e.metrics.CollectionActions.WithLabelValues("reminder_scheduled")), 2},
		{"faturas vencidas", testutil.ToFloat64(e.metrics.CollectionActions.WithLabelValues("invoice_overdue")), 1},
	} {
		if m.got < m.want {
			t.Errorf("métrica %s = %v, esperado ao menos %v", m.name, m.got, m.want)
		}
	}
}
