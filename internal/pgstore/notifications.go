package pgstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// Notifications implementa notifications.Store.
type Notifications struct{ s *Store }

// Notifications retorna o adaptador de notificações.
func (s *Store) Notifications() *Notifications { return &Notifications{s: s} }

type notificationsTx struct{ base }

// InTenant abre a transação do tenant.
func (a *Notifications) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(notifications.Tx) error) error {
	return a.s.inTenant(ctx, tenantID, func(b base) error { return fn(notificationsTx{b}) })
}

// TenantsWithDueNotifications descobre tenants com notificações a enviar.
func (a *Notifications) TenantsWithDueNotifications(ctx context.Context, now time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := a.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		out, err = q.TenantsWithDueNotifications(ctx, dbgen.TenantsWithDueNotificationsParams{Now: now, AfterTenant: after, MaxTenants: int32(limit)})
		return err
	})
	return out, err
}

// NotificationEvents devolve o histórico de uma notificação (consulta e testes).
func (a *Notifications) NotificationEvents(ctx context.Context, tenantID, id uuid.UUID) ([]notifications.Event, error) {
	var out []notifications.Event
	err := a.s.inTenant(ctx, tenantID, func(b base) error {
		var err error
		out, err = notificationsTx{b}.NotificationEvents(ctx, id)
		return err
	})
	return out, err
}

// --- templates ---

func templateFromRow(r dbgen.NotificationTemplate) notifications.Template {
	at := r.UpdatedAt
	return notifications.Template{Key: notifications.TemplateKey(r.TemplateKey), Subject: r.Subject, Body: r.Body, UpdatedAt: &at,
		Version: int(r.Version)}
}

// customTemplate devolve o template personalizado, se houver.
func (b base) customTemplate(ctx context.Context, key notifications.TemplateKey) (*notifications.Template, error) {
	r, err := b.q.GetNotificationTemplate(ctx, dbgen.GetNotificationTemplateParams{TenantID: b.tenantID, TemplateKey: string(key)})
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t := templateFromRow(r)
	return &t, nil
}

func (tx notificationsTx) CustomTemplates(ctx context.Context) ([]notifications.Template, error) {
	rows, err := tx.q.ListNotificationTemplates(ctx, tx.tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]notifications.Template, 0, len(rows))
	for _, r := range rows {
		out = append(out, templateFromRow(r))
	}
	return out, nil
}

func (tx notificationsTx) SaveTemplate(ctx context.Context, t notifications.Template, updatedBy *uuid.UUID, now time.Time,
	expectedVersion int) (notifications.Template, error) {
	var rows []dbgen.NotificationTemplate
	var err error
	if expectedVersion == 0 {
		rows, err = tx.q.InsertNotificationTemplate(ctx, dbgen.InsertNotificationTemplateParams{TenantID: tx.tenantID, TemplateKey: string(t.Key),
			Subject: t.Subject, Body: t.Body, UpdatedBy: updatedBy, Now: now})
	} else {
		rows, err = tx.q.UpdateNotificationTemplate(ctx, dbgen.UpdateNotificationTemplateParams{TenantID: tx.tenantID, TemplateKey: string(t.Key),
			Subject: t.Subject, Body: t.Body, UpdatedBy: updatedBy, Now: now, Version: int32(expectedVersion)})
	}
	if err != nil {
		return notifications.Template{}, err
	}
	if err := versionConflict(int64(len(rows)), "O template foi alterado por outra pessoa."); err != nil {
		return notifications.Template{}, err
	}
	return templateFromRow(rows[0]), nil
}

// --- notificações ---

func contentAAD(tenantID, id uuid.UUID) []byte {
	return []byte(tenantID.String() + "/notification/" + id.String() + "/content")
}

// notificationFromRow converte a linha e decifra o conteúdo. Conteúdo que não
// decifra (chave retirada do keyring, AAD divergente ou dado corrompido) não
// interrompe a leitura: fica registrado em ContentError e o serviço encerra
// somente essa notificação.
func (b base) notificationFromRow(r dbgen.Notification) notifications.Notification {
	n := notificationFromList(dbgen.ListNotificationsRow{ID: r.ID, TenantID: r.TenantID, BusinessKey: r.BusinessKey, TemplateKey: r.TemplateKey,
		Channel: r.Channel, Recipients: r.Recipients, Subject: r.Subject, CustomerID: r.CustomerID, InvoiceID: r.InvoiceID,
		SubscriptionID: r.SubscriptionID, InvitationID: r.InvitationID, PolicyID: r.PolicyID, StepOffsetDays: r.StepOffsetDays,
		Status: r.Status, StatusReason: r.StatusReason, SendAfter: r.SendAfter, Attempts: r.Attempts, MaxAttempts: r.MaxAttempts,
		LeaseUntil: r.LeaseUntil, LastError: r.LastError, MessageID: r.MessageID, SentAt: r.SentAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
	if r.ContentCiphertext != nil && r.ContentKeyID != nil {
		pt, err := b.store.keyring.Decrypt(r.ContentCiphertext, *r.ContentKeyID, contentAAD(r.TenantID, r.ID))
		if err == nil {
			err = json.Unmarshal(pt, &n.Content)
		}
		if err != nil {
			n.Content, n.ContentError = notifications.Content{}, err.Error()
		}
	}
	return n
}

func notificationFromList(r dbgen.ListNotificationsRow) notifications.Notification {
	n := notifications.Notification{ID: r.ID, TenantID: r.TenantID, BusinessKey: r.BusinessKey, TemplateKey: notifications.TemplateKey(r.TemplateKey),
		Channel: r.Channel, Recipients: r.Recipients, Subject: r.Subject, CustomerID: r.CustomerID, InvoiceID: r.InvoiceID,
		SubscriptionID: r.SubscriptionID, InvitationID: r.InvitationID, PolicyID: r.PolicyID, StepOffsetDays: intFromPtr32(r.StepOffsetDays),
		Status: notifications.Status(r.Status), StatusReason: deref(r.StatusReason), SendAfter: r.SendAfter, Attempts: int(r.Attempts),
		MaxAttempts: int(r.MaxAttempts), LeaseUntil: r.LeaseUntil, LastError: deref(r.LastError), MessageID: deref(r.MessageID),
		SentAt: r.SentAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if n.Recipients == nil {
		n.Recipients = []string{}
	}
	return n
}

func (b base) insertNotificationEvents(ctx context.Context, id uuid.UUID, events []notifications.Event) error {
	for _, e := range events {
		if err := b.q.InsertNotificationEvent(ctx, dbgen.InsertNotificationEventParams{ID: ids.New(), TenantID: b.tenantID, NotificationID: id,
			Status: string(e.Status), Reason: e.Reason, Attempt: int32(e.Attempt), Detail: strPtr(e.Detail), OccurredAt: e.At}); err != nil {
			return err
		}
	}
	return nil
}

// notificationExists informa se a identidade de negócio já foi registrada.
func (b base) notificationExists(ctx context.Context, businessKey string) (bool, error) {
	return b.q.NotificationKeyExists(ctx, dbgen.NotificationKeyExistsParams{TenantID: b.tenantID, BusinessKey: businessKey})
}

// insertNotification grava a notificação (conteúdo cifrado) e o histórico.
// Antes da gravação, as linhas referenciadas são bloqueadas em modo
// compartilhado na ordem fatura, assinatura e cliente (ADR-031), a mesma
// das transações de pagamento; a chave estrangeira bloquearia na ordem de
// criação das restrições.
func (b base) insertNotification(ctx context.Context, n *notifications.Notification) (bool, error) {
	if n.InvoiceID != nil {
		if err := b.q.LockInvoiceKeyShare(ctx, dbgen.LockInvoiceKeyShareParams{TenantID: b.tenantID, ID: *n.InvoiceID}); err != nil {
			return false, err
		}
	}
	if n.SubscriptionID != nil {
		if err := b.q.LockSubscriptionKeyShare(ctx, dbgen.LockSubscriptionKeyShareParams{TenantID: b.tenantID, ID: *n.SubscriptionID}); err != nil {
			return false, err
		}
	}
	if n.CustomerID != nil {
		// Cliente inexistente segue adiante e é recusado pela chave
		// estrangeira, como antes. Cliente anonimizado depois da composição
		// (os dados do conteúdo e os destinatários foram lidos antes): nada
		// é gravado.
		anonymizedAt, err := b.q.LockCustomerKeyShare(ctx, dbgen.LockCustomerKeyShareParams{TenantID: b.tenantID, ID: *n.CustomerID})
		if err != nil && !postgres.IsNoRows(err) {
			return false, err
		}
		if anonymizedAt != nil {
			return false, nil
		}
	}
	p := dbgen.InsertNotificationParams{ID: n.ID, TenantID: b.tenantID, BusinessKey: n.BusinessKey, TemplateKey: string(n.TemplateKey),
		Channel: n.Channel, Recipients: n.Recipients, Subject: n.Subject, CustomerID: n.CustomerID, InvoiceID: n.InvoiceID,
		SubscriptionID: n.SubscriptionID, InvitationID: n.InvitationID, PolicyID: n.PolicyID, StepOffsetDays: intPtr32(n.StepOffsetDays),
		Status: string(n.Status), StatusReason: strPtr(n.StatusReason), SendAfter: n.SendAfter, Attempts: int32(n.Attempts),
		MaxAttempts: int32(n.MaxAttempts), LastError: strPtr(n.LastError), CreatedAt: n.CreatedAt}
	if p.Recipients == nil {
		p.Recipients = []string{}
	}
	if n.Content.Text != "" || n.Content.HTML != "" {
		pt, err := json.Marshal(n.Content)
		if err != nil {
			return false, err
		}
		ct, keyID, err := b.store.keyring.Encrypt(pt, contentAAD(b.tenantID, n.ID))
		if err != nil {
			return false, err
		}
		p.ContentCiphertext, p.ContentKeyID = ct, &keyID
	}
	rows, err := b.q.InsertNotification(ctx, p)
	if err != nil || rows == 0 {
		return false, err
	}
	return true, b.insertNotificationEvents(ctx, n.ID, n.PopEvents())
}

func (tx notificationsTx) DueNotifications(ctx context.Context, now time.Time, limit int) ([]notifications.Notification, error) {
	rows, err := tx.q.DueNotifications(ctx, dbgen.DueNotificationsParams{TenantID: tx.tenantID, Now: now, RowLimit: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]notifications.Notification, 0, len(rows))
	for _, r := range rows {
		out = append(out, tx.notificationFromRow(r))
	}
	return out, nil
}

func (tx notificationsTx) Notification(ctx context.Context, id uuid.UUID, forUpdate bool) (notifications.Notification, error) {
	var r dbgen.Notification
	var err error
	if forUpdate {
		r, err = tx.q.GetNotificationForUpdate(ctx, dbgen.GetNotificationForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetNotification(ctx, dbgen.GetNotificationParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return notifications.Notification{}, notFound(err, "notification_not_found", "Notificação não encontrada.")
	}
	return tx.notificationFromRow(r), nil
}

func (tx notificationsTx) UpdateNotification(ctx context.Context, n *notifications.Notification, expectedAttempts int) (bool, error) {
	recipients := n.Recipients
	if recipients == nil {
		recipients = []string{}
	}
	rows, err := tx.q.UpdateNotification(ctx, dbgen.UpdateNotificationParams{Recipients: recipients, Status: string(n.Status),
		StatusReason: strPtr(n.StatusReason), SendAfter: n.SendAfter, Attempts: int32(n.Attempts), LeaseUntil: n.LeaseUntil,
		LastError: strPtr(n.LastError), MessageID: strPtr(n.MessageID), SentAt: n.SentAt, UpdatedAt: n.UpdatedAt, TenantID: tx.tenantID,
		ID: n.ID, ExpectedAttempts: int32(expectedAttempts)})
	if err != nil || rows == 0 {
		return false, err
	}
	return true, tx.insertNotificationEvents(ctx, n.ID, n.PopEvents())
}

func (tx notificationsTx) ListNotifications(ctx context.Context, f notifications.ListFilter) (notifications.Page, error) {
	cts, cid, err := parseTimeCursor(f.Cursor)
	if err != nil {
		return notifications.Page{}, err
	}
	limit := pageLimit(f.Limit)
	rows, err := tx.q.ListNotifications(ctx, dbgen.ListNotificationsParams{TenantID: tx.tenantID, Status: strPtr(string(f.Status)),
		InvoiceID: f.InvoiceID, CursorTs: cts, CursorID: cid, RowLimit: int32(limit + 1)})
	if err != nil {
		return notifications.Page{}, err
	}
	var p notifications.Page
	for i, r := range rows {
		if i == limit {
			p.NextCursor = timeCursor(rows[i-1].CreatedAt, rows[i-1].ID)
			break
		}
		p.Items = append(p.Items, notificationFromList(r))
	}
	return p, nil
}

func (tx notificationsTx) NewerReminderExists(ctx context.Context, invoiceID uuid.UUID, offsetDays int) (bool, error) {
	off := int32(offsetDays)
	return tx.q.NewerReminderExists(ctx, dbgen.NewerReminderExistsParams{TenantID: tx.tenantID, InvoiceID: &invoiceID, OffsetDays: &off})
}

func (tx notificationsTx) TryLockCustomer(ctx context.Context, customerID uuid.UUID) (bool, error) {
	return tx.q.TryLockCustomerNotifications(ctx, "notifications:"+tx.tenantID.String()+":"+customerID.String())
}

func (tx notificationsTx) RemindersSent(ctx context.Context, customerID, excludeID uuid.UUID, from, to time.Time) (int, error) {
	n, err := tx.q.CountRemindersSent(ctx, dbgen.CountRemindersSentParams{TenantID: tx.tenantID, CustomerID: &customerID, ExcludeID: excludeID,
		FromTs: &from, ToTs: &to})
	return int(n), err
}

func (tx notificationsTx) ReminderRules(ctx context.Context, policyID uuid.UUID) (notifications.ReminderRules, error) {
	r, err := tx.q.GetReminderRules(ctx, dbgen.GetReminderRulesParams{TenantID: tx.tenantID, ID: policyID})
	if err != nil {
		return notifications.ReminderRules{}, notFound(err, "collection_policy_not_found", "Política de cobrança não encontrada.")
	}
	return notifications.ReminderRules{Window: notifications.Window{StartHour: int(r.SendWindowStartHour), EndHour: int(r.SendWindowEndHour)},
		MaxPerDay: int(r.MaxMessagesPerDay)}, nil
}

func (tx notificationsTx) SuspensionWindow(ctx context.Context, suspensionID uuid.UUID) (notifications.Window, bool, error) {
	r, err := tx.q.GetSuspensionSendWindow(ctx, dbgen.GetSuspensionSendWindowParams{TenantID: tx.tenantID, ID: suspensionID})
	if postgres.IsNoRows(err) {
		return notifications.Window{}, false, nil
	}
	if err != nil {
		return notifications.Window{}, false, err
	}
	return notifications.Window{StartHour: int(r.SendWindowStartHour), EndHour: int(r.SendWindowEndHour)}, true, nil
}

// NotificationEvents devolve o histórico da notificação em ordem cronológica.
func (tx notificationsTx) NotificationEvents(ctx context.Context, id uuid.UUID) ([]notifications.Event, error) {
	rows, err := tx.q.ListNotificationEvents(ctx, dbgen.ListNotificationEventsParams{TenantID: tx.tenantID, NotificationID: id})
	if err != nil {
		return nil, err
	}
	out := make([]notifications.Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, notifications.Event{Status: notifications.Status(r.Status), Reason: r.Reason, Attempt: int(r.Attempt),
			Detail: deref(r.Detail), At: r.OccurredAt})
	}
	return out, nil
}

// cancelReminders cancela os lembretes agendados de uma fatura.
func (b base) cancelReminders(ctx context.Context, invoiceID uuid.UUID, reason string, now time.Time) (int, error) {
	rows, err := b.q.CancelScheduledReminders(ctx, dbgen.CancelScheduledRemindersParams{Reason: &reason, Now: now, TenantID: b.tenantID,
		InvoiceID: &invoiceID})
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if err := b.insertNotificationEvents(ctx, r.ID, []notifications.Event{{Status: notifications.Canceled, Reason: reason,
			Attempt: int(r.Attempts), At: now}}); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// --- dados de contexto ---

// organization devolve nome e fuso do tenant.
func (b base) organization(ctx context.Context) (notifications.Organization, error) {
	t, err := b.q.GetTenant(ctx)
	if err != nil {
		return notifications.Organization{}, notFound(err, "organization_not_found", "Organização não encontrada.")
	}
	loc, err := time.LoadLocation(t.Timezone)
	if err != nil {
		return notifications.Organization{}, err
	}
	return notifications.Organization{Name: t.Name, Location: loc}, nil
}

// customerRecipients devolve os contatos que recebem e-mails de cobrança; sem
// contatos cadastrados, o e-mail principal do cliente. Contatos que optaram
// por não receber e clientes anonimizados não recebem mensagens.
func (b base) customerRecipients(ctx context.Context, customerID uuid.UUID) ([]string, error) {
	r, err := b.q.CustomerRecipientData(ctx, dbgen.CustomerRecipientDataParams{TenantID: b.tenantID, CustomerID: customerID})
	if err != nil {
		return nil, notFound(err, "customer_not_found", "Cliente não encontrado.")
	}
	switch {
	case r.AnonymizedAt != nil:
		return nil, nil
	case r.ContactCount > 0:
		return notifications.NormalizeRecipients(r.BillingEmails), nil
	case r.CustomerEmail != nil:
		return notifications.NormalizeRecipients([]string{*r.CustomerEmail}), nil
	}
	return nil, nil
}

func invoiceInfoFromRow(r dbgen.GetInvoiceNotificationInfoRow) notifications.InvoiceInfo {
	return notifications.InvoiceInfo{ID: r.ID, Number: deref(r.Number), Status: billing.Status(r.Status), CustomerID: r.CustomerID,
		CustomerName: r.CustomerName, SubscriptionID: r.SubscriptionID, Total: r.Total, AmountDue: r.AmountDue, AmountPaid: r.AmountPaid,
		AmountCredited: r.AmountCredited, IssuedAt: r.IssuedAt, DueAt: r.DueAt}
}

// invoiceInfo devolve a visão da fatura para notificações.
func (b base) invoiceInfo(ctx context.Context, id uuid.UUID) (notifications.InvoiceInfo, error) {
	r, err := b.q.GetInvoiceNotificationInfo(ctx, dbgen.GetInvoiceNotificationInfoParams{TenantID: b.tenantID, ID: id})
	if err != nil {
		return notifications.InvoiceInfo{}, notFound(err, "invoice_not_found", "Fatura não encontrada.")
	}
	return invoiceInfoFromRow(r), nil
}

// lockInvoiceInfo bloqueia a fatura em modo compartilhado e devolve a visão.
func (b base) lockInvoiceInfo(ctx context.Context, id uuid.UUID) (notifications.InvoiceInfo, error) {
	r, err := b.q.LockInvoiceNotificationInfo(ctx, dbgen.LockInvoiceNotificationInfoParams{TenantID: b.tenantID, ID: id})
	if err != nil {
		return notifications.InvoiceInfo{}, notFound(err, "invoice_not_found", "Fatura não encontrada.")
	}
	return invoiceInfoFromRow(dbgen.GetInvoiceNotificationInfoRow(r)), nil
}

func (tx notificationsTx) Payment(ctx context.Context, id uuid.UUID) (notifications.PaymentInfo, error) {
	r, err := tx.q.GetPaymentNotificationInfo(ctx, dbgen.GetPaymentNotificationInfoParams{TenantID: tx.tenantID, ID: id})
	if err != nil {
		return notifications.PaymentInfo{}, notFound(err, "payment_not_found", "Pagamento não encontrado.")
	}
	return notifications.PaymentInfo{ID: r.ID, InvoiceID: r.InvoiceID, Amount: r.Amount, PaidAt: r.PaidAt}, nil
}

func (tx notificationsTx) Invitation(ctx context.Context, id uuid.UUID, now time.Time) (notifications.InvitationInfo, error) {
	r, err := tx.q.GetInvitation(ctx, dbgen.GetInvitationParams{TenantID: tx.tenantID, ID: id})
	if err != nil {
		return notifications.InvitationInfo{}, notFound(err, "invitation_not_found", "Convite não encontrado.")
	}
	inv := tenancy.Invitation{ID: r.ID, ExpiresAt: r.ExpiresAt, AcceptedAt: r.AcceptedAt, RevokedAt: r.RevokedAt}
	return notifications.InvitationInfo{ID: r.ID, Email: r.Email, Role: tenancy.Role(r.Role), ExpiresAt: r.ExpiresAt,
		Pending: inv.State(now) == tenancy.InvitationPending}, nil
}

// SuspensionActive informa se a suspensão está ativa; found=false quando
// ela não existe.
func (tx notificationsTx) SuspensionActive(ctx context.Context, suspensionID uuid.UUID) (bool, bool, error) {
	active, err := tx.q.GetSuspensionActive(ctx, dbgen.GetSuspensionActiveParams{TenantID: tx.tenantID, ID: suspensionID})
	if postgres.IsNoRows(err) {
		return false, false, nil
	}
	return active, err == nil, err
}

func (tx notificationsTx) SubscriptionSuspended(ctx context.Context, id uuid.UUID) (bool, error) {
	sub, err := tx.subscription(ctx, id, false)
	if err != nil {
		return false, err
	}
	return sub.Status == subscriptions.Suspended, nil
}

func (tx notificationsTx) PlatformPlan(ctx context.Context) (platformbilling.Plan, error) {
	return tx.platformPlan(ctx)
}

// Métodos da porta notifications.Tx sobre os auxiliares compartilhados.

func (tx notificationsTx) CustomTemplate(ctx context.Context, key notifications.TemplateKey) (*notifications.Template, error) {
	return tx.customTemplate(ctx, key)
}

func (tx notificationsTx) InsertNotification(ctx context.Context, n *notifications.Notification) (bool, error) {
	return tx.insertNotification(ctx, n)
}

func (tx notificationsTx) Organization(ctx context.Context) (notifications.Organization, error) {
	return tx.organization(ctx)
}

func (tx notificationsTx) CustomerRecipients(ctx context.Context, customerID uuid.UUID) ([]string, error) {
	return tx.customerRecipients(ctx, customerID)
}

func (tx notificationsTx) Invoice(ctx context.Context, id uuid.UUID) (notifications.InvoiceInfo, error) {
	return tx.invoiceInfo(ctx, id)
}
