//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	amqp "github.com/rabbitmq/amqp091-go"
	tcrabbit "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/httpapi"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/infra/redisstore"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/paysim"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// lockedBuffer recebe os logs JSON de várias goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines devolve os registros de log em que todos os campos de match
// coincidem.
func (b *lockedBuffer) lines(t *testing.T, match map[string]string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	raw := b.buf.String()
	b.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(raw), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log não é JSON: %v (%s)", err, l)
		}
		ok := true
		for k, v := range match {
			ok = ok && m[k] == v
		}
		if ok {
			out = append(out, m)
		}
	}
	return out
}

// providerGate retém, quando ativado, a criação de cobrança no provedor
// (POST /v1/charges, feita pelo consumidor) até ser liberada e a listagem
// da conciliação agendada (GET /v1/charges?limit=, feita pela tarefa) até o
// cliente desistir, sinalizando cada etapa.
type providerGate struct {
	next http.Handler

	mu      sync.Mutex
	active  bool
	charge  *http.Request
	listing bool

	chargeHeld   chan struct{}
	release      chan struct{}
	listHeld     chan struct{}
	listCanceled chan struct{}
}

func newProviderGate(next http.Handler) *providerGate {
	return &providerGate{next: next, chargeHeld: make(chan struct{}), release: make(chan struct{}), listHeld: make(chan struct{}),
		listCanceled: make(chan struct{})}
}

func (g *providerGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	holdCharge := g.active && g.charge == nil && r.Method == http.MethodPost && r.URL.Path == "/v1/charges"
	holdList := g.active && !g.listing && r.Method == http.MethodGet && r.URL.Path == "/v1/charges" && r.URL.Query().Has("limit")
	if holdCharge {
		g.charge = r
	}
	g.listing = g.listing || holdList
	g.mu.Unlock()
	switch {
	case holdCharge:
		close(g.chargeHeld)
		<-g.release
	case holdList:
		close(g.listHeld)
		<-r.Context().Done() // só termina quando o cliente (a tarefa) desiste
		close(g.listCanceled)
		return
	}
	g.next.ServeHTTP(w, r)
}

// workerEnv é o worker real sobre PostgreSQL e RabbitMQ (Testcontainers),
// com o simulador de pagamentos em processo.
type workerEnv struct {
	env     *fixture.Env
	core    *Core
	amqpURL string
	gate    *providerGate
	logs    *lockedBuffer
}

func newWorkerEnv(t *testing.T) *workerEnv {
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
	logs := &lockedBuffer{}
	log := logging.New(logs, "debug", "worker")
	env := fixture.New(t, 16, time.Now().UTC())
	// O worker grava em scheduled_jobs (tabela global, compartilhada pelos
	// testes do pacote) o próximo horário de cada tarefa pelo relógio deste
	// teste; sem a limpeza, as tarefas vencidas seriam reivindicadas pelos
	// agendadores de outros testes. Executa depois do encerramento do worker.
	owner := pgtest.OwnerPool(t)
	t.Cleanup(func() {
		if _, err := owner.Exec(context.Background(), `DELETE FROM scheduled_jobs`); err != nil {
			t.Errorf("limpeza do agendamento: %v", err)
		}
	})
	st, err := paysim.Load("")
	if err != nil {
		t.Fatal(err)
	}
	sim := paysim.New(st, "http://checkout.test", func() time.Time { return env.Clock.Now() }, log)
	gate := newProviderGate(sim.Handler())
	simSrv := httptest.NewServer(gate)
	t.Cleanup(simSrv.Close)
	vars := map[string]string{
		"RECORVIA_ENV": string(config.Test), "RECORVIA_WORKER_ID": "worker-ciclo-de-vida", "RECORVIA_SCHEDULER_TICK": "50ms",
		"RECORVIA_WORKER_CONCURRENCY": "2", "RECORVIA_OUTBOX_LEASE": "10s", "RECORVIA_AMQP_URL": amqpURL, "RECORVIA_PAYSIM_URL": simSrv.URL,
		"RECORVIA_PUBLIC_URL": "https://app.recorvia.test",
	}
	cfg, err := config.Load(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	core, err := Assemble(cfg, log, env.DB, env.Keyring, env.Clock)
	if err != nil {
		t.Fatal(err)
	}
	return &workerEnv{env: env, core: core, amqpURL: amqpURL, gate: gate, logs: logs}
}

// tenantWithInvoice cria um tenant com o simulador como provedor padrão e
// uma fatura emitida de 150,00 (evento invoice.issued na outbox).
func (w *workerEnv) tenantWithInvoice(t *testing.T, name string) (fixture.Tenant, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	env := w.env
	tn := env.NewTenant(t, name)
	// Plano com conciliação automática: a tarefa payments.reconcile consulta
	// o provedor.
	if _, err := env.Platform.ChangePlan(ctx, tn.ID, tn.Actor, "escala"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.core.Payments.ConfigureAccount(ctx, tn.ID, tn.Actor, payments.AccountInput{Provider: "simulator", Environment: "local",
		APIKey: "sim_" + uuid.NewString()[:8], WebhookSecret: strings.Repeat("s", 40), MakeDefault: true}); err != nil {
		t.Fatal(err)
	}
	plan := env.NewFlatPlan(t, tn, 15000)
	cust := env.NewCustomer(t, tn)
	sub := env.NewSubscription(t, tn, cust.ID, plan.ID, nil)
	if _, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	invs, err := env.Billing.List(ctx, tn.ID, billing.ListFilter{SubscriptionID: &sub.ID, Limit: 10})
	if err != nil || len(invs.Items) != 1 {
		t.Fatalf("fatura: %v", err)
	}
	return tn, invs.Items[0].ID
}

// count executa uma contagem no escopo do tenant.
func (w *workerEnv) count(t *testing.T, tenantID uuid.UUID, query string, args ...any) int {
	t.Helper()
	var n int
	ctx := context.Background()
	if err := w.env.DB.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, append([]any{tenantID}, args...)...).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// queueDepth devolve as mensagens prontas na fila (as não confirmadas voltam
// a ela quando a conexão do worker fecha).
func (w *workerEnv) queueDepth(t *testing.T, queue string) int {
	t.Helper()
	conn, err := amqp.Dial(w.amqpURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return q.Messages
}

// eventually verifica a condição em intervalos curtos até o prazo.
func eventually(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condição não atingida no prazo: %s", desc)
		}
		<-tick.C
	}
}

// RNF-11: com SIGTERM, o cancelamento chega às tarefas agendadas (a consulta
// ao provedor da conciliação em andamento é abandonada) e o worker só
// retorna depois que a mensagem em processamento termina: a cobrança é
// criada, o inbox é registrado e a mensagem é confirmada (não volta à fila).
func TestWorkerWaitsInFlightMessageAndCancelsTasksOnSIGTERM(t *testing.T) {
	w := newWorkerEnv(t)
	tn, invoiceID := w.tenantWithInvoice(t, "encerramento")
	w.gate.mu.Lock()
	w.gate.active = true
	w.gate.mu.Unlock()

	ctx, stop := SignalContext(context.Background())
	defer stop()
	worker := &Worker{Core: w.core, Metrics: telemetry.NewMetrics("worker"), Conn: rabbit.NewConn(w.amqpURL, w.core.Log)}
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	t.Cleanup(func() {
		stop()
		select {
		case <-w.gate.release:
		default:
			close(w.gate.release)
		}
	})
	wait(t, w.gate.chargeHeld, "consumidor criando a cobrança no provedor")
	wait(t, w.gate.listHeld, "conciliação agendada consultando o provedor")

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	wait(t, ctx.Done(), "cancelamento pelo SIGTERM")
	wait(t, w.gate.listCanceled, "cancelamento da tarefa em andamento")
	select {
	case err := <-done:
		t.Fatalf("o worker saiu com a mensagem em processamento: %v", err)
	default:
	}
	w.gate.mu.Lock()
	held := w.gate.charge
	w.gate.mu.Unlock()
	if err := held.Context().Err(); err != nil {
		t.Fatalf("o processamento da mensagem não pode ser cancelado pelo encerramento: %v", err)
	}

	close(w.gate.release)
	if err := wait(t, done, "retorno do worker"); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if n := w.count(t, tn.ID, `SELECT count(*) FROM charges WHERE tenant_id = $1 AND invoice_id = $2 AND status = 'awaiting_payment'
		AND provider_charge_id IS NOT NULL`, invoiceID); n != 1 {
		t.Fatalf("cobranças criadas no provedor: %d", n)
	}
	if n := w.count(t, tn.ID, `SELECT count(*) FROM inbox_messages WHERE tenant_id = $1 AND consumer = $2`, payments.ConsumerInvoiceIssued); n != 1 {
		t.Fatalf("inbox do consumidor: %d", n)
	}
	if n := w.queueDepth(t, rabbit.QueueName(payments.ConsumerInvoiceIssued)); n != 0 {
		t.Fatalf("a mensagem processada voltou à fila: %d", n)
	}
	if lines := w.logs.lines(t, map[string]string{"msg": "tarefa falhou", "job": "payments.reconcile"}); len(lines) == 0 {
		t.Fatalf("a tarefa interrompida deve registrar a falha com o nome: %v", lines)
	}
}

// RNF-07: o request_id de uma requisição HTTP percorre a outbox, o RabbitMQ
// e o consumidor: a nota de crédito emitida pela API gera invoice.credited
// com o request_id; o consumidor payments.invoice_credited cancela a
// cobrança divergente e solicita outra, e os logs e as entradas de auditoria
// desse processamento levam o mesmo request_id.
func TestRequestIDReachesConsumerLogsAndAudit(t *testing.T) {
	w := newWorkerEnv(t)
	ctx := context.Background()
	env := w.env
	tn, invoiceID := w.tenantWithInvoice(t, "correlacao")
	if c, err := w.core.Payments.RequestCharge(ctx, tn.ID, invoiceID, tn.Actor); err != nil || c == nil || c.Status != payments.ChargeAwaitingPayment {
		t.Fatalf("cobrança inicial: %+v %v", c, err)
	}

	// API com sessões no Redis, como em cmd/api.
	rctr, err := tcredis.Run(ctx, "redis:8.8.4-alpine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rctr.Terminate(context.Background()) })
	redisURL, err := rctr.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rdb, err := redisstore.Open(ctx, redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	sessions := redisstore.New(rdb, env.Keyring, "web")
	c := w.core
	_, router := httpapi.NewRouter(httpapi.Deps{Log: c.Log, Config: c.Config, Clock: c.Clock, Store: c.Store, Sessions: sessions,
		Identity: identity.NewService(nil, sessions, c.Store.Users(), c.Clock, identity.DefaultPolicy()), Tenancy: c.Tenancy,
		Platform: c.Platform, Customers: c.Customers, Catalog: c.Catalog, Subscriptions: c.Subscriptions, Billing: c.Billing,
		Changes: c.Changes, CreditNotes: c.CreditNotes, Coupons: c.Coupons, Terms: c.Terms, Metering: c.Metering, Payments: c.Payments,
		Reporting: c.Reporting, Notifications: c.Notifications, Collections: c.Collections, Webhooks: c.Webhooks})
	api := httptest.NewServer(router)
	t.Cleanup(api.Close)
	now := env.Clock.Now()
	sess := identity.Session{ID: identity.RandomToken(32), UserID: tn.Owner.ID, ActiveTenantID: &tn.ID, CSRFToken: identity.RandomToken(16),
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := sessions.Save(ctx, sess, time.Hour); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	worker := &Worker{Core: c, Metrics: telemetry.NewMetrics("worker"), Conn: rabbit.NewConn(w.amqpURL, c.Log)}
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := wait(t, done, "encerramento do worker"); err != nil {
			t.Errorf("worker: %v", err)
		}
	})

	// O evento invoice.issued (sem request_id) é processado antes da nota de
	// crédito: processado depois, o consumidor payments.invoice_issued
	// poderia solicitar a nova cobrança no lugar do consumidor da nota.
	eventually(t, "processamento de invoice.issued", func() bool {
		return w.count(t, tn.ID, `SELECT count(*) FROM inbox_messages WHERE tenant_id = $1 AND consumer = $2`, payments.ConsumerInvoiceIssued) == 1
	})
	const requestID = "req-correlacao-ponta-0001"
	req, err := http.NewRequest(http.MethodPost, api.URL+"/api/v1/invoices/"+invoiceID.String()+"/credit-notes",
		strings.NewReader(`{"amount": 5000, "reason": "Desconto negociado"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: c.Config.CookieName, Value: sess.ID})
	req.Header.Set("X-CSRF-Token", sess.CSRFToken)
	req.Header.Set("X-Recorvia-Tenant", tn.ID.String())
	req.Header.Set("Idempotency-Key", uuid.NewString())
	req.Header.Set("X-Request-ID", requestID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 || resp.Header.Get("X-Request-ID") != requestID {
		t.Fatalf("nota de crédito: %d %q", resp.StatusCode, resp.Header.Get("X-Request-ID"))
	}
	// Elo 1: o evento gravado na outbox carrega o request_id.
	if n := w.count(t, tn.ID, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'invoice.credited'
		AND headers->>'request_id' = $2`, requestID); n != 1 {
		t.Fatalf("eventos invoice.credited com o request_id: %d", n)
	}
	// Elo 2: a auditoria gravada pelo consumidor recebe o request_id.
	eventually(t, "auditoria do consumidor com o request_id", func() bool {
		return w.count(t, tn.ID, `SELECT count(DISTINCT action) FROM audit_log WHERE tenant_id = $1 AND request_id = $2
			AND action IN ('charge.canceled_after_credit_note', 'charge.requested')`, requestID) == 2
	})
	// Elo 3: os logs do consumidor levam o request_id, o tenant e o rastro.
	eventually(t, "log do consumidor com o request_id", func() bool {
		return len(w.logs.lines(t, map[string]string{"msg": "mensagem processada", "consumer": payments.ConsumerInvoiceCredited,
			"request_id": requestID, "tenant_id": tn.ID.String()})) == 1
	})
	// O log de acesso da requisição tem o mesmo request_id, o tenant e o
	// usuário.
	access := w.logs.lines(t, map[string]string{"msg": "requisição", "request_id": requestID, "tenant_id": tn.ID.String(),
		"user_id": tn.Owner.ID.String()})
	if len(access) != 1 {
		t.Fatalf("log de acesso: %v", access)
	}
}
