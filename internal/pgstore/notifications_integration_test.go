//go:build integration

package pgstore_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/infra/mailer"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/mailpit"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// mailSender adapta o cliente SMTP à porta de envio (como em internal/app).
type mailSender struct{ m *mailer.Mailer }

func (s mailSender) Send(ctx context.Context, e notifications.Email) (notifications.Receipt, error) {
	r, err := s.m.Send(ctx, mailer.Message{ID: e.ID.String(), FromName: e.FromName, To: e.To, Subject: e.Subject, Text: e.Text, HTML: e.HTML, Date: e.Date})
	return notifications.Receipt{MessageID: r.MessageID, Refused: r.Refused}, err
}

// notifier cria o serviço de notificações com envio SMTP real (Mailpit).
func notifier(t *testing.T, env *fixture.Env) (*notifications.Service, mailpit.Server) {
	t.Helper()
	mp := mailpit.Start(t)
	m, err := mailer.New(mailer.Config{Addr: mp.SMTPAddr, From: "cobranca@recorvia.test", TLS: mailer.TLSNone, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return notifications.NewService(env.Store.Notifications(), mailSender{m}, env.Keyring, env.Clock, notifications.Config{
		Lease: time.Minute, SendTimeout: 15 * time.Second, PublicURL: "https://app.recorvia.test", IsPermanent: mailer.IsPermanent}), mp
}

// issuedInvoice cria cliente com contato de cobrança, plano, assinatura e a
// fatura do período corrente; devolve o cliente e a fatura.
func issuedInvoice(t *testing.T, env *fixture.Env, tn fixture.Tenant, contact string) (customers.Customer, uuid.UUID) {
	t.Helper()
	return issuedInvoiceOnPlan(t, env, tn, env.NewFlatPlan(t, tn, 99000).ID, contact)
}

// issuedInvoiceOnPlan é issuedInvoice com um plano já existente.
func issuedInvoiceOnPlan(t *testing.T, env *fixture.Env, tn fixture.Tenant, planID uuid.UUID, contact string) (customers.Customer, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	c := env.NewCustomer(t, tn)
	if contact != "" {
		if _, err := env.Customers.AddContact(ctx, tn.ID, tn.Actor, c.ID, customers.ContactInput{Name: "Financeiro", Email: contact,
			ReceivesBillingEmail: true}); err != nil {
			t.Fatal(err)
		}
	}
	env.NewSubscription(t, tn, c.ID, planID, nil)
	if _, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
		t.Fatal(err)
	}
	var invoiceID uuid.UUID
	err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM invoices WHERE tenant_id = $1 AND customer_id = $2`, tn.ID, c.ID).Scan(&invoiceID)
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, invoiceID
}

// Execuções concorrentes do mesmo evento (reentregas com ids diferentes)
// gravam uma única notificação para a identidade de negócio.
func TestNotificationBusinessKeyIsDeduplicatedConcurrently(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 16, now)
	tn := env.NewTenant(t, "notif-dedup")
	svc := notifications.NewService(env.Store.Notifications(), nil, env.Keyring, env.Clock, notifications.Config{})
	_, invoiceID := issuedInvoice(t, env, tn, "dedup@cliente.test")
	const workers = 12
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Go(func() {
			if err := svc.HandleInvoiceIssued(ctx, tn.ID, invoiceID, ids.New()); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("consumidor concorrente falhou: %v", err)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM notifications WHERE tenant_id = $1 AND business_key = $2`, tn.ID,
		notifications.InvoiceIssuedKey(invoiceID)); n != 1 {
		t.Fatalf("notificações gravadas = %d, esperado 1", n)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM notification_events WHERE tenant_id = $1`, tn.ID); n != 1 {
		t.Fatalf("histórico deve ter uma única entrada de agendamento: %d", n)
	}
	// A restrição única protege também gravações diretas.
	err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO notifications (id, tenant_id, business_key, template_key, subject, status, status_reason, send_after,
			max_attempts, created_at, updated_at) VALUES ($1, $2, $3, 'invoice_issued', 'x', 'suppressed', 'no_recipient', now(), 1, now(), now())`,
			ids.New(), tn.ID, notifications.InvoiceIssuedKey(invoiceID))
		return err
	})
	if !postgres.IsUniqueViolation(err, "notifications_business_key") {
		t.Fatalf("esperava violação da identidade de negócio: %v", err)
	}
}

// Envio real por SMTP (Mailpit): a mensagem chega com assunto, destinatário,
// texto e HTML escapado; a notificação fica sent com o Message-ID; envios
// concorrentes não duplicam o e-mail.
func TestNotificationDispatchDeliversThroughSMTP(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := env.NewTenant(t, "notif-smtp")
	svc, mp := notifier(t, env)
	recipient := "smtp-" + ids.New().String()[24:] + "@cliente.test"
	c, invoiceID := issuedInvoice(t, env, tn, recipient)
	if _, err := env.Customers.Update(ctx, tn.ID, tn.Actor, c.ID, customers.Input{Kind: c.Kind, Name: `Cliente <b>Negrito</b> & Cia`,
		DocumentNumber: c.DocumentNumber, Email: c.Email}, c.Version); err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleInvoiceIssued(ctx, tn.ID, invoiceID, ids.New()); err != nil {
		t.Fatal(err)
	}
	// O envio percorre todos os tenants com notificações vencidas (inclusive
	// os de outros testes); a verificação usa o destinatário e o histórico
	// desta notificação.
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, err := svc.Dispatch(ctx); err != nil {
				t.Errorf("envio: %v", err)
			}
		})
	}
	wg.Wait()
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM notification_events WHERE tenant_id = $1 AND reason = 'attempt'`, tn.ID); n != 1 {
		t.Fatalf("tentativas de envio = %d, esperado 1", n)
	}
	msgs := mp.MessagesTo(t, recipient)
	if len(msgs) != 1 {
		t.Fatalf("mensagens recebidas no Mailpit = %d, esperado 1", len(msgs))
	}
	m := msgs[0]
	if !strings.HasPrefix(m.Subject, "Fatura ") || !strings.Contains(m.Subject, "emitida") || m.To[0].Address != recipient ||
		m.From.Name != "Org notif-smtp via Recorvia" || m.From.Address != "cobranca@recorvia.test" {
		t.Fatalf("cabeçalhos recebidos: %+v", m.Summary)
	}
	if !strings.Contains(m.Text, "R$ 990,00") || !strings.Contains(m.Text, "Cliente <b>Negrito</b> & Cia") {
		t.Fatalf("texto recebido: %q", m.Text)
	}
	if strings.Contains(m.HTML, "<b>Negrito</b>") || !strings.Contains(m.HTML, "&lt;b&gt;Negrito&lt;/b&gt; &amp; Cia") {
		t.Fatalf("HTML sem escape: %s", m.HTML)
	}
	var status, messageID string
	err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, message_id FROM notifications WHERE tenant_id = $1 AND invoice_id = $2`, tn.ID, invoiceID).
			Scan(&status, &messageID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != "sent" || "<"+m.MessageID+">" != messageID {
		t.Fatalf("situação gravada: %s %s (Mailpit: %s)", status, messageID, m.MessageID)
	}
	page, err := svc.List(ctx, tn.ID, notifications.ListFilter{InvoiceID: &invoiceID})
	if err != nil || len(page.Items) != 1 || page.Items[0].Status != notifications.Sent {
		t.Fatalf("histórico: %+v %v", page, err)
	}
	events, err := env.Store.Notifications().NotificationEvents(ctx, tn.ID, page.Items[0].ID)
	if err != nil || len(events) != 3 || events[2].Reason != notifications.ReasonSent {
		t.Fatalf("histórico de situações: %+v %v", events, err)
	}
	// Outro tenant não enxerga as notificações.
	other := env.NewTenant(t, "notif-outro")
	if p, err := svc.List(ctx, other.ID, notifications.ListFilter{}); err != nil || len(p.Items) != 0 {
		t.Fatalf("isolamento entre tenants: %+v %v", p, err)
	}
}

// Contatos que não recebem e-mails de cobrança são respeitados e o conteúdo
// fica cifrado em repouso.
func TestNotificationRespectsContactPreferencesAndEncryptsContent(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "notif-pref")
	svc := notifications.NewService(env.Store.Notifications(), nil, env.Keyring, env.Clock, notifications.Config{})
	c, invoiceID := issuedInvoice(t, env, tn, "")
	if _, err := env.Customers.AddContact(ctx, tn.ID, tn.Actor, c.ID, customers.ContactInput{Name: "Comercial", Email: "comercial@cliente.test",
		ReceivesBillingEmail: false}); err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleInvoiceIssued(ctx, tn.ID, invoiceID, ids.New()); err != nil {
		t.Fatal(err)
	}
	var status, reason, keyID string
	var id uuid.UUID
	var recipients []string
	var content []byte
	err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id, status, status_reason, recipients, content_ciphertext, content_key_id FROM notifications WHERE tenant_id = $1`, tn.ID).
			Scan(&id, &status, &reason, &recipients, &content, &keyID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != "suppressed" || reason != notifications.ReasonNoRecipient || len(recipients) != 0 {
		t.Fatalf("contato sem preferência de cobrança deveria suprimir: %s %s %v", status, reason, recipients)
	}
	// O texto cifrado são bytes aleatórios: procurar nele um marcador curto
	// (como "R$") acusa coincidências. A conferência usa o nome do cliente,
	// longo o bastante para não ocorrer por acaso, e prova pela decifragem
	// que o conteúdo guardado é o texto com os dados do cliente.
	if len(content) == 0 || strings.Contains(string(content), c.Name) {
		t.Fatal("conteúdo deve ficar cifrado em repouso")
	}
	plain, err := env.Keyring.Decrypt(content, keyID, []byte(tn.ID.String()+"/notification/"+id.String()+"/content"))
	if err != nil {
		t.Fatalf("o conteúdo deve decifrar com a chave e o contexto da notificação: %v", err)
	}
	if !strings.Contains(string(plain), c.Name) || !strings.Contains(string(plain), "R$") {
		t.Fatalf("conteúdo decifrado sem os dados da fatura: %s", plain)
	}
}

// Convite de membro: o evento leva o token cifrado; o worker envia o e-mail
// com o link de aceite. O token não fica em claro no banco.
func TestInvitationEmailUsesSealedTokenFromEvent(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	env.Tenancy.WithTokenSealer(env.Keyring)
	tn := env.NewTenant(t, "notif-convite")
	svc, mp := notifier(t, env)
	invitee := "convidada-" + ids.New().String()[24:] + "@empresa.test"
	owner := tenancy.UserRef{ID: tn.Owner.ID, Email: tn.Owner.Email, DisplayName: tn.Owner.DisplayName}
	inv, token, err := env.Tenancy.Invite(ctx, tn.ID, tenancy.Access{TenantID: tn.ID, UserID: tn.Owner.ID, Role: tenancy.RoleOwner}, owner,
		invitee, tenancy.RoleFinance)
	if err != nil {
		t.Fatal(err)
	}
	var msgID uuid.UUID
	var payload map[string]any
	err = env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id, payload FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, tn.ID, outbox.EventMemberInvited).
			Scan(&msgID, &payload)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND payload::text LIKE '%' || $2 || '%'`, tn.ID, token); n != 0 {
		t.Fatal("o token não pode ficar em claro na outbox")
	}
	ev := notifications.MemberInvited{InvitationID: inv.ID, TokenCiphertext: payload["token_ciphertext"].(string), TokenKeyID: payload["token_key_id"].(string)}
	if err := svc.HandleMemberInvited(ctx, tn.ID, msgID, ev); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM notifications WHERE tenant_id = $1 AND encode(content_ciphertext, 'escape') LIKE '%' || $2 || '%'`,
		tn.ID, token); n != 0 {
		t.Fatal("o token não pode ficar em claro na notificação")
	}
	if _, err := svc.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	msgs := mp.MessagesTo(t, invitee)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "https://app.recorvia.test/convite?token="+token) ||
		!strings.Contains(msgs[0].Subject, "Org notif-convite") || !strings.Contains(msgs[0].Text, "financeiro") {
		t.Fatalf("e-mail de convite: %+v", msgs)
	}
	// Convite revogado antes do envio: o e-mail é cancelado.
	second := "revogada-" + ids.New().String()[24:] + "@empresa.test"
	inv2, _, err := env.Tenancy.Invite(ctx, tn.ID, tenancy.Access{TenantID: tn.ID, UserID: tn.Owner.ID, Role: tenancy.RoleOwner}, owner,
		second, tenancy.RoleAuditor)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Tenancy.RevokeInvitation(ctx, tn.ID, owner, inv2.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleMemberInvited(ctx, tn.ID, ids.New(), notifications.MemberInvited{InvitationID: inv2.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if len(mp.MessagesTo(t, second)) != 0 {
		t.Fatal("convite revogado não deve ser enviado")
	}
}

// Falha de conexão SMTP: a notificação volta a scheduled com backoff e o
// histórico registra a tentativa.
func TestNotificationRetriesWhenSMTPIsUnavailable(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "notif-retry")
	m, err := mailer.New(mailer.Config{Addr: "127.0.0.1:1", From: "cobranca@recorvia.test", TLS: mailer.TLSNone, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	svc := notifications.NewService(env.Store.Notifications(), mailSender{m}, env.Keyring, env.Clock, notifications.Config{Lease: time.Minute,
		IsPermanent: mailer.IsPermanent, Jitter: func() float64 { return 0.5 }})
	_, invoiceID := issuedInvoice(t, env, tn, "retry@cliente.test")
	if err := svc.HandleInvoiceIssued(ctx, tn.ID, invoiceID, ids.New()); err != nil {
		t.Fatal(err)
	}
	// O envio percorre também tenants de outros testes; as verificações usam
	// o estado desta notificação.
	if res, err := svc.Dispatch(ctx); err != nil || res.Retried < 1 {
		t.Fatalf("esperava retentativa: %+v %v", res, err)
	}
	var status, lastErr string
	var sendAfter time.Time
	var attempts int
	err = env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, last_error, send_after, attempts FROM notifications WHERE tenant_id = $1`, tn.ID).
			Scan(&status, &lastErr, &sendAfter, &attempts)
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != "scheduled" || attempts != 1 || lastErr == "" || !sendAfter.Equal(now.Add(time.Minute)) {
		t.Fatalf("retentativa gravada: %s %d %q %v", status, attempts, lastErr, sendAfter)
	}
	if _, err := svc.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, env, tn.ID, `SELECT attempts FROM notifications WHERE tenant_id = $1`, tn.ID); n != 1 {
		t.Fatalf("antes do backoff não há nova tentativa: %d tentativas", n)
	}
}

func notificationState(t *testing.T, env *fixture.Env, tenantID uuid.UUID, businessKey string) (status, reason string, attempts int) {
	t.Helper()
	err := env.DB.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT status, COALESCE(status_reason, ''), attempts FROM notifications
			WHERE tenant_id = $1 AND business_key = $2`, tenantID, businessKey).Scan(&status, &reason, &attempts)
	})
	if err != nil {
		t.Fatal(err)
	}
	return status, reason, attempts
}

// Uma notificação cujo conteúdo não decifra (chave retirada do keyring) é
// encerrada como failed, e as demais do tenant são enviadas.
func TestNotificationWithUnreadableContentDoesNotBlockTheTenant(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := env.NewTenant(t, "notif-ilegivel")
	svc, mp := notifier(t, env)
	suffix := ids.New().String()[24:]
	broken, healthy := "ilegivel-"+suffix+"@cliente.test", "legivel-"+suffix+"@cliente.test"
	plan := env.NewFlatPlan(t, tn, 99000)
	_, brokenInvoice := issuedInvoiceOnPlan(t, env, tn, plan.ID, broken)
	_, healthyInvoice := issuedInvoiceOnPlan(t, env, tn, plan.ID, healthy)
	for _, id := range []uuid.UUID{brokenInvoice, healthyInvoice} {
		if err := svc.HandleInvoiceIssued(ctx, tn.ID, id, ids.New()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pgtest.OwnerPool(t).Exec(ctx, `UPDATE notifications SET content_key_id = 'chave-retirada' WHERE tenant_id = $1 AND business_key = $2`,
		tn.ID, notifications.InvoiceIssuedKey(brokenInvoice)); err != nil {
		t.Fatal(err)
	}
	// O envio percorre também tenants de outros testes; as verificações usam
	// as notificações deste tenant.
	if _, err := svc.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if status, reason, attempts := notificationState(t, env, tn.ID, notifications.InvoiceIssuedKey(brokenInvoice)); status != "failed" ||
		reason != notifications.ReasonContentUnavailable || attempts != 0 {
		t.Fatalf("notificação ilegível: %s %s %d", status, reason, attempts)
	}
	if status, _, _ := notificationState(t, env, tn.ID, notifications.InvoiceIssuedKey(healthyInvoice)); status != "sent" {
		t.Fatalf("notificação legível: %s", status)
	}
	if got := len(mp.MessagesTo(t, healthy)); got != 1 {
		t.Fatalf("mensagens para o destinatário da notificação legível: %d", got)
	}
	if got := len(mp.MessagesTo(t, broken)); got != 0 {
		t.Fatalf("a notificação ilegível não pode ser enviada: %d", got)
	}
}

// Um lembrete reivindicado de novo após o lease vencer (worker interrompido
// durante o envio) não conta para o próprio limite diário.
func TestReclaimedReminderDoesNotCountAgainstItsOwnDailyLimit(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := env.NewTenant(t, "notif-limite")
	regua := collections.NewService(env.Store.Collections(), env.Clock, nil)
	in := defaultPolicyInput()
	in.MaxMessagesPerDay = 1
	if _, err := regua.CreatePolicy(ctx, tn.ID, tn.Actor, nil, in); err != nil {
		t.Fatal(err)
	}
	svc, mp := notifier(t, env)
	recipient := "limite-" + ids.New().String()[24:] + "@cliente.test"
	_, invoiceID := issuedInvoice(t, env, tn, recipient)         // vence em 15/03
	env.Clock.Set(time.Date(2026, 3, 12, 15, 0, 0, 0, time.UTC)) // passo -3, dentro da janela
	if _, err := regua.RunTenant(ctx, tn.ID); err != nil {
		t.Fatal(err)
	}
	key := notifications.ReminderKey(invoiceID, -3)
	if _, err := pgtest.OwnerPool(t).Exec(ctx, `UPDATE notifications SET status = 'sending', attempts = 1, lease_until = $3
		WHERE tenant_id = $1 AND business_key = $2`, tn.ID, key, env.Clock.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if status, reason, attempts := notificationState(t, env, tn.ID, key); status != "sent" || attempts != 2 {
		t.Fatalf("lembrete retomado deveria ser enviado: %s %s %d", status, reason, attempts)
	}
	if got := len(mp.MessagesTo(t, recipient)); got != 1 {
		t.Fatalf("mensagens recebidas: %d", got)
	}
}
