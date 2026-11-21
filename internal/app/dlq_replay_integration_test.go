//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/ricartefelipe/recorvia/internal/app"
	"github.com/ricartefelipe/recorvia/internal/httpapi"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/infra/redisstore"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

type replayIDP struct{ identity.IdentityProvider }

// opsAPI é a API HTTP real sobre os serviços do worker, com sessões no Redis.
type opsAPI struct {
	t        *testing.T
	core     *app.Core
	sessions *redisstore.Store
	srv      *httptest.Server
}

func newOpsAPI(t *testing.T, core *app.Core) *opsAPI {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcredis.Run(ctx, "redis:8.8.4-alpine")
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	url, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rdb, err := redisstore.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	sessions := redisstore.New(rdb, core.Keyring, "web")
	idsvc := identity.NewService(replayIDP{}, sessions, core.Store.Users(), core.Clock, identity.DefaultPolicy())
	_, router := httpapi.NewRouter(httpapi.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Config: core.Config, Clock: core.Clock,
		Store: core.Store, Sessions: sessions, Identity: idsvc, Tenancy: core.Tenancy, Platform: core.Platform, Customers: core.Customers,
		Catalog: core.Catalog, Subscriptions: core.Subscriptions, Billing: core.Billing, Payments: core.Payments, Metering: core.Metering,
		Reporting: core.Reporting, Notifications: core.Notifications, Collections: core.Collections, Webhooks: core.Webhooks})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &opsAPI{t: t, core: core, sessions: sessions, srv: srv}
}

// post envia o pedido com a sessão do usuário na organização informada.
func (a *opsAPI) post(user identity.User, tenantID uuid.UUID, path string, body any) (int, map[string]any) {
	a.t.Helper()
	now := a.core.Clock.Now()
	// A sessão nasce com as gerações correntes do usuário (ADR-078): uma
	// sessão sem elas é recusada como revogada.
	gen, tenantGen, err := a.sessions.Generations(context.Background(), user.ID, &tenantID)
	if err != nil {
		a.t.Fatal(err)
	}
	s := identity.Session{ID: identity.RandomToken(32), UserID: user.ID, ActiveTenantID: &tenantID, CSRFToken: identity.RandomToken(16),
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(24 * time.Hour), Generation: gen, TenantGeneration: tenantGen}
	if err := a.sessions.Create(context.Background(), s, time.Hour); err != nil {
		a.t.Fatal(err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		a.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, a.srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.CSRFToken)
	req.Header.Set("X-Recorvia-Tenant", tenantID.String())
	req.AddCookie(&http.Cookie{Name: a.core.Config.CookieName, Value: s.ID})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		a.t.Fatalf("resposta de %s: %v", path, err)
	}
	return resp.StatusCode, out
}

// addMember cria um usuário e o vincula à organização com o papel informado.
func addMember(t *testing.T, env *fixture.Env, tn fixture.Tenant, role tenancy.Role) identity.User {
	t.Helper()
	ctx := context.Background()
	uid := ids.New()
	u, err := env.Store.Users().UpsertFromClaims(ctx, uid, identity.Claims{Issuer: "https://idp.test", Subject: uid.String(),
		Email: uid.String()[24:] + "@membro.test", EmailVerified: true, Name: "Membro"}, env.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := env.Tenancy.Invite(ctx, tn.ID, tenancy.Access{TenantID: tn.ID, UserID: tn.Owner.ID, Role: tenancy.RoleOwner},
		tenancy.UserRef{ID: tn.Owner.ID, Email: tn.Owner.Email, DisplayName: tn.Owner.DisplayName}, u.Email, role)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Tenancy.AcceptInvitation(ctx, tenancy.UserRef{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName}, token); err != nil {
		t.Fatal(err)
	}
	return u
}

// failInboxFor faz o registro do inbox dos consumidores falhar para a
// mensagem informada (gatilho criado pelo dono das tabelas), simulando uma
// falha do consumidor que leva a mensagem à DLQ. O gatilho dispara antes da
// verificação de conflito, então também falha a reentrega de uma mensagem
// já registrada. A função devolvida remove a falha.
func failInboxFor(t *testing.T, messageID uuid.UUID, consumers ...string) func() {
	t.Helper()
	quoted := make([]string, len(consumers))
	for i, c := range consumers {
		quoted[i] = "'" + c + "'"
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgtest.Start(t).OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	name := "test_falha_inbox_" + strings.ReplaceAll(ids.New().String(), "-", "")
	drop := func() {
		if _, err := conn.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON inbox_messages; DROP FUNCTION IF EXISTS %[1]s()`, name)); err != nil {
			t.Errorf("remoção da falha simulada: %v", err)
		}
	}
	t.Cleanup(drop)
	_, err = conn.Exec(ctx, fmt.Sprintf(`
CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.consumer IN (%[2]s) AND NEW.message_id = '%[3]s' THEN
    RAISE EXCEPTION 'falha simulada do consumidor';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER %[1]s BEFORE INSERT ON inbox_messages FOR EACH ROW EXECUTE FUNCTION %[1]s();`, name, strings.Join(quoted, ", "), messageID))
	if err != nil {
		t.Fatal(err)
	}
	return drop
}

// logInboxAttempts registra, numa tabela própria do teste, cada tentativa de
// gravar o inbox da mensagem informada, por consumidor (inclusive a
// reentrega de mensagem já registrada, que o ON CONFLICT descarta: o gatilho
// roda antes da verificação de conflito). Devolve a contagem por consumidor.
func logInboxAttempts(t *testing.T, messageID uuid.UUID) func(consumer string) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgtest.Start(t).OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	name := "test_tentativas_inbox_" + strings.ReplaceAll(ids.New().String(), "-", "")
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON inbox_messages; DROP FUNCTION IF EXISTS %[1]s();
			DROP TABLE IF EXISTS %[1]s`, name)); err != nil {
			t.Errorf("remoção do registro de tentativas: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, fmt.Sprintf(`
CREATE TABLE %[1]s (consumer text NOT NULL, message_id uuid NOT NULL);
CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
  IF NEW.message_id = '%[2]s' THEN
    INSERT INTO %[1]s (consumer, message_id) VALUES (NEW.consumer, NEW.message_id);
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER %[1]s BEFORE INSERT ON inbox_messages FOR EACH ROW EXECUTE FUNCTION %[1]s();`, name, messageID)); err != nil {
		t.Fatal(err)
	}
	return func(consumer string) int {
		var n int
		if err := conn.QueryRow(context.Background(), fmt.Sprintf(`SELECT count(*) FROM %s WHERE consumer = $1`, name), consumer).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
}

// Ponta a ponta do reprocessamento de DLQ: a mensagem invoice.issued falha
// só no consumidor de notificações e vai à DLQ (registrada em dead_letters);
// o reprocessamento pela API (permissão operations.reprocess, negado ao
// auditor) grava na mesma transação a resolução, a auditoria e a reentrega
// direcionada na outbox; o relay a publica somente na fila daquele
// consumidor, que a processa com o id original da mensagem e registra o
// inbox com esse id. Os demais consumidores do mesmo evento não a recebem.
func TestEndToEndDeadLetterReplayReachesOnlyTheFailedConsumer(t *testing.T) {
	ctx := context.Background()
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	d0 := calendar.DateOf(time.Now(), loc).AddDays(-20)
	// Uma única tentativa: a falha vai direto à DLQ, sem esperar as
	// retentativas com atraso do broker. Um processamento por vez em cada
	// consumidor: as mensagens de uma fila são tratadas na ordem em que
	// chegaram, o que torna a mensagem sentinela (passo 5) um sinal de que as
	// anteriores daquela fila já foram processadas.
	e := startWorkerWith(t, d0.StartIn(loc).Add(12*time.Hour), map[string]string{"RECORVIA_CONSUMER_MAX_ATTEMPTS": "1",
		"RECORVIA_WORKER_CONCURRENCY": "1"})
	env := e.env
	tn := env.NewTenant(t, "dlq-reprocessamento")
	// Plano com webhooks enviados e um endpoint assinando invoice.issued: o
	// consumidor de webhooks também registra a mensagem no inbox.
	if _, err := env.Platform.ChangePlan(ctx, tn.ID, tn.Actor, "profissional", env.PlatformVersion(t, tn.ID)); err != nil {
		t.Fatal(err)
	}
	rcv := newHookReceiver(t)
	if _, _, err := e.core.Webhooks.CreateEndpoint(ctx, tn.ID, tn.Actor, &tn.Owner.ID, webhooks.Input{URL: rcv.srv.URL + "/recorvia",
		EventTypes: []string{outbox.EventInvoiceIssued}}); err != nil {
		t.Fatal(err)
	}
	cust := env.NewCustomer(t, tn)
	plan := env.NewFlatPlan(t, tn, 45000)
	env.NewSubscription(t, tn, cust.ID, plan.ID, nil)
	if _, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	var invoiceID, messageID uuid.UUID
	if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT aggregate_id, id FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, tn.ID,
			outbox.EventInvoiceIssued).Scan(&invoiceID, &messageID)
	}); err != nil {
		t.Fatal(err)
	}
	inbox := func(consumer string) int {
		var n int
		if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE tenant_id = $1 AND consumer = $2 AND message_id = $3`,
				tn.ID, consumer, messageID).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	restore := failInboxFor(t, messageID, notifications.ConsumerInvoiceIssued)

	// 1. A mensagem chega a todos os consumidores do evento; só o de
	// notificações falha e ela é registrada como dead letter.
	e.start()
	var deadLetterID uuid.UUID
	e.eventually("dead letter registrada", func() bool {
		return env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM dead_letters WHERE tenant_id = $1 AND consumer = $2 AND message_id = $3 AND status = 'open'`,
				tn.ID, notifications.ConsumerInvoiceIssued, messageID.String()).Scan(&deadLetterID)
		}) == nil
	})
	e.eventually("demais consumidores processaram", func() bool {
		return inbox(payments.ConsumerInvoiceIssued) == 1 && inbox(webhooks.ConsumerFanout) == 1
	})
	if n := inbox(notifications.ConsumerInvoiceIssued); n != 0 {
		t.Fatalf("inbox do consumidor que falhou: %d", n)
	}
	if _, ok := e.notifications(tn.ID, invoiceID)[notifications.InvoiceIssuedKey(invoiceID)]; ok {
		t.Fatal("notificação criada apesar da falha do consumidor")
	}
	restore()
	// Daqui em diante, toda tentativa de registrar a mensagem no inbox fica
	// anotada por consumidor.
	attempts := logInboxAttempts(t, messageID)
	notifAcked := e.acked(notifications.ConsumerInvoiceIssued)

	// 2. Reprocessamento pela API: o auditor não tem operations.reprocess.
	api := newOpsAPI(t, e.core)
	path := "/api/v1/dead-letters/" + deadLetterID.String() + "/replay"
	auditor := addMember(t, env, tn, tenancy.RoleAuditor)
	if st, body := api.post(auditor, tn.ID, path, map[string]any{"reason": "Tentativa sem permissão"}); st != http.StatusForbidden || body["code"] != "forbidden" {
		t.Fatalf("auditor reprocessando: %d %v", st, body)
	}
	st, body := api.post(tn.Owner, tn.ID, path, map[string]any{"reason": "Falha do inbox corrigida"})
	if st != http.StatusOK || body["status"] != pgstore.DeadLetterReplayed {
		t.Fatalf("reprocessamento: %d %v", st, body)
	}
	if st, body := api.post(tn.Owner, tn.ID, path, map[string]any{"reason": "Segunda tentativa"}); st != http.StatusConflict || body["code"] != "dead_letter_resolved" {
		t.Fatalf("reprocessamento repetido: %d %v", st, body)
	}

	// 3. Reentrega direcionada na outbox, com a fila do consumidor e o id
	// original, e auditoria com a justificativa.
	var headers map[string]string
	var replayID uuid.UUID
	if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT id, headers FROM outbox_events WHERE tenant_id = $1 AND aggregate_type = 'dead_letter_replay'
			AND event_type = $2`, tn.ID, outbox.EventInvoiceIssued).Scan(&replayID, &raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &headers)
	}); err != nil {
		t.Fatal(err)
	}
	if headers[pgstore.HeaderTargetQueue] != rabbit.QueueName(notifications.ConsumerInvoiceIssued) ||
		headers[pgstore.HeaderOriginalMessageID] != messageID.String() || replayID == messageID {
		t.Fatalf("reentrega direcionada: %s %v", replayID, headers)
	}
	var reason string
	if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT details->>'reason' FROM audit_log WHERE tenant_id = $1 AND action = 'operations.dead_letter_replayed'
			AND resource_id = $2 AND actor_id = $3`, tn.ID, deadLetterID.String(), tn.Owner.ID.String()).Scan(&reason)
	}); err != nil || reason != "Falha do inbox corrigida" {
		t.Fatalf("auditoria do reprocessamento: %q %v", reason, err)
	}

	// 4. O consumidor certo processa a mensagem com o id original.
	e.eventually("reentrega processada pelo consumidor de notificações", func() bool {
		return inbox(notifications.ConsumerInvoiceIssued) == 1 && e.acked(notifications.ConsumerInvoiceIssued) > notifAcked
	})
	if _, ok := e.notifications(tn.ID, invoiceID)[notifications.InvoiceIssuedKey(invoiceID)]; !ok {
		t.Fatal("notificação da fatura emitida ausente após o reprocessamento")
	}
	var replayInbox int
	if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE tenant_id = $1 AND message_id = $2`, tn.ID, replayID).Scan(&replayInbox)
	}); err != nil || replayInbox != 0 {
		t.Fatalf("o inbox usa o id original, não o da reentrega: %d %v", replayInbox, err)
	}
	if n := attempts(notifications.ConsumerInvoiceIssued); n != 1 {
		t.Fatalf("tentativas do consumidor de notificações: %d", n)
	}

	// 5. Sinal positivo de que os demais consumidores do evento não
	// receberam a reentrega: uma mensagem sentinela publicada depois dela na
	// fila de cada um. Com um processamento por vez, quando a sentinela é
	// registrada no inbox, tudo o que estava antes na fila já foi tratado; se
	// a reentrega tivesse chegado ali, a tentativa de inbox estaria anotada.
	var payload []byte
	if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE tenant_id = $1 AND id = $2`, tn.ID, messageID).Scan(&payload)
	}); err != nil {
		t.Fatal(err)
	}
	sentinel := ids.New()
	for _, consumer := range []string{payments.ConsumerInvoiceIssued, webhooks.ConsumerFanout} {
		if err := e.pub.Publish(ctx, rabbit.QueueName(consumer), rabbit.Message{ID: sentinel.String(), Type: outbox.EventInvoiceIssued,
			TenantID: tn.ID.String(), Body: payload, OccurredAt: env.Clock.Now()}, 0); err != nil {
			t.Fatal(err)
		}
	}
	sentinelInbox := func(consumer string) int {
		var n int
		if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE tenant_id = $1 AND consumer = $2 AND message_id = $3`,
				tn.ID, consumer, sentinel).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	e.eventually("sentinela processada pelos demais consumidores", func() bool {
		return sentinelInbox(payments.ConsumerInvoiceIssued) == 1 && sentinelInbox(webhooks.ConsumerFanout) == 1
	})
	for _, consumer := range []string{payments.ConsumerInvoiceIssued, webhooks.ConsumerFanout} {
		if n := attempts(consumer); n != 0 {
			t.Fatalf("reentrega chegou ao consumidor %s: %d tentativas de inbox", consumer, n)
		}
	}
	if p, f := inbox(payments.ConsumerInvoiceIssued), inbox(webhooks.ConsumerFanout); p != 1 || f != 1 {
		t.Fatalf("inbox dos demais consumidores: pagamentos %d, webhooks %d", p, f)
	}
}
