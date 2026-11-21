-- Templates, notificações por e-mail, histórico e descoberta de envio.

-- name: ListNotificationTemplates :many
SELECT * FROM notification_templates WHERE tenant_id = @tenant_id ORDER BY template_key;

-- name: GetNotificationTemplate :one
SELECT * FROM notification_templates WHERE tenant_id = @tenant_id AND template_key = @template_key;

-- name: UpsertNotificationTemplate :one
INSERT INTO notification_templates (tenant_id, template_key, subject, body, updated_by, created_at, updated_at, version)
VALUES (@tenant_id, @template_key, @subject, @body, sqlc.narg(updated_by), @now, @now, 1)
ON CONFLICT (tenant_id, template_key) DO UPDATE
   SET subject = EXCLUDED.subject, body = EXCLUDED.body, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at,
       version = notification_templates.version + 1
RETURNING *;

-- name: NotificationKeyExists :one
SELECT EXISTS (SELECT 1 FROM notifications WHERE tenant_id = @tenant_id AND business_key = @business_key)::boolean;

-- name: InsertNotification :execrows
INSERT INTO notifications (id, tenant_id, business_key, template_key, channel, recipients, subject, content_ciphertext, content_key_id,
                           customer_id, invoice_id, subscription_id, invitation_id, policy_id, step_offset_days, status, status_reason,
                           send_after, attempts, max_attempts, last_error, created_at, updated_at)
VALUES (@id, @tenant_id, @business_key, @template_key, @channel, @recipients, @subject, sqlc.narg(content_ciphertext), sqlc.narg(content_key_id),
        sqlc.narg(customer_id), sqlc.narg(invoice_id), sqlc.narg(subscription_id), sqlc.narg(invitation_id), sqlc.narg(policy_id),
        sqlc.narg(step_offset_days), @status, sqlc.narg(status_reason), @send_after, @attempts, @max_attempts, sqlc.narg(last_error),
        @created_at, @created_at)
ON CONFLICT (tenant_id, business_key) DO NOTHING;

-- name: GetNotification :one
SELECT * FROM notifications WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetNotificationForUpdate :one
SELECT * FROM notifications WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: DueNotifications :many
SELECT * FROM notifications
 WHERE tenant_id = @tenant_id
   AND ((status = 'scheduled' AND send_after <= @now) OR (status = 'sending' AND lease_until < @now))
 ORDER BY send_after, id
 LIMIT @row_limit
 FOR UPDATE SKIP LOCKED;

-- name: UpdateNotification :execrows
UPDATE notifications
   SET recipients = @recipients, status = @status, status_reason = sqlc.narg(status_reason), send_after = @send_after,
       attempts = @attempts, lease_until = sqlc.narg(lease_until), last_error = sqlc.narg(last_error),
       message_id = sqlc.narg(message_id), sent_at = sqlc.narg(sent_at), updated_at = @updated_at
 WHERE tenant_id = @tenant_id AND id = @id AND attempts = @expected_attempts;

-- name: InsertNotificationEvent :exec
INSERT INTO notification_events (id, tenant_id, notification_id, status, reason, attempt, detail, occurred_at)
VALUES (@id, @tenant_id, @notification_id, @status, @reason, @attempt, sqlc.narg(detail), @occurred_at);

-- name: ListNotificationEvents :many
SELECT * FROM notification_events WHERE tenant_id = @tenant_id AND notification_id = @notification_id ORDER BY occurred_at, id;

-- name: ListNotifications :many
SELECT id, tenant_id, business_key, template_key, channel, recipients, subject, customer_id, invoice_id, subscription_id, invitation_id,
       policy_id, step_offset_days, status, status_reason, send_after, attempts, max_attempts, lease_until, last_error, message_id, sent_at,
       created_at, updated_at
  FROM notifications
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
   AND (sqlc.narg(invoice_id)::uuid IS NULL OR invoice_id = sqlc.narg(invoice_id))
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY created_at DESC, id DESC
 LIMIT @row_limit;

-- name: NewerReminderExists :one
SELECT EXISTS (SELECT 1 FROM notifications
                WHERE tenant_id = @tenant_id AND invoice_id = @invoice_id AND policy_id IS NOT NULL
                  AND step_offset_days > @offset_days)::boolean;

-- name: TryLockCustomerNotifications :one
SELECT pg_try_advisory_xact_lock(hashtextextended(@lock_key::text, 0))::boolean;

-- name: CountRemindersSent :one
-- A notificação avaliada (exclude_id) não entra na contagem: reivindicada de
-- novo após o lease vencer, ela ainda está em sending.
SELECT count(*)::int FROM notifications
 WHERE tenant_id = @tenant_id AND customer_id = @customer_id AND policy_id IS NOT NULL AND id <> @exclude_id
   AND (status = 'sending' OR (status = 'sent' AND sent_at >= @from_ts AND sent_at < @to_ts));

-- name: GetReminderRules :one
SELECT send_window_start_hour, send_window_end_hour, max_messages_per_day
  FROM collection_policies WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetSuspensionSendWindow :one
-- Janela de envio da política que aplicou a suspensão (aviso de suspensão).
SELECT p.send_window_start_hour, p.send_window_end_hour
  FROM collection_suspensions s
  JOIN collection_policies p ON p.tenant_id = s.tenant_id AND p.id = s.policy_id
 WHERE s.tenant_id = @tenant_id AND s.id = @id;

-- name: GetSuspensionActive :one
-- Situação da suspensão do aviso de suspensão: o aviso só sai enquanto a
-- própria suspensão estiver ativa.
SELECT (status = 'active')::boolean AS active FROM collection_suspensions WHERE tenant_id = @tenant_id AND id = @id;

-- name: CancelScheduledReminders :many
UPDATE notifications
   SET status = 'canceled', status_reason = @reason, lease_until = NULL, updated_at = @now
 WHERE tenant_id = @tenant_id AND invoice_id = @invoice_id AND policy_id IS NOT NULL AND status = 'scheduled'
RETURNING id, attempts;

-- name: CustomerRecipientData :one
SELECT c.email AS customer_email, c.anonymized_at,
       (SELECT count(*) FROM customer_contacts cc WHERE cc.tenant_id = c.tenant_id AND cc.customer_id = c.id)::int AS contact_count,
       COALESCE((SELECT array_agg(cc.email ORDER BY cc.created_at, cc.id) FROM customer_contacts cc
                  WHERE cc.tenant_id = c.tenant_id AND cc.customer_id = c.id AND cc.receives_billing_email), '{}')::text[] AS billing_emails
  FROM customers c
 WHERE c.tenant_id = @tenant_id AND c.id = @customer_id;

-- name: GetInvoiceNotificationInfo :one
SELECT i.id, i.number, i.status, i.customer_id, i.subscription_id, i.total, i.amount_due, i.amount_paid, i.amount_credited,
       i.issued_at, i.due_at, c.name AS customer_name
  FROM invoices i JOIN customers c ON c.tenant_id = i.tenant_id AND c.id = i.customer_id
 WHERE i.tenant_id = @tenant_id AND i.id = @id;

-- name: LockInvoiceNotificationInfo :one
SELECT i.id, i.number, i.status, i.customer_id, i.subscription_id, i.total, i.amount_due, i.amount_paid, i.amount_credited,
       i.issued_at, i.due_at, c.name AS customer_name
  FROM invoices i JOIN customers c ON c.tenant_id = i.tenant_id AND c.id = i.customer_id
 WHERE i.tenant_id = @tenant_id AND i.id = @id
   FOR SHARE OF i;

-- name: LockInvoiceKeyShare :exec
SELECT id FROM invoices WHERE tenant_id = @tenant_id AND id = @id FOR KEY SHARE;

-- name: LockSubscriptionKeyShare :exec
SELECT id FROM subscriptions WHERE tenant_id = @tenant_id AND id = @id FOR KEY SHARE;

-- name: LockCustomerKeyShare :one
-- Bloqueia o cliente em modo compartilhado e devolve a anonimização: a
-- anonimização mantém o cliente com bloqueio exclusivo, de modo que a
-- gravação de uma notificação composta antes dela espera e é descartada.
SELECT anonymized_at FROM customers WHERE tenant_id = @tenant_id AND id = @id FOR KEY SHARE;

-- name: GetPaymentNotificationInfo :one
SELECT id, invoice_id, amount, paid_at FROM payments WHERE tenant_id = @tenant_id AND id = @id;

-- name: TenantsWithDueNotifications :many
SELECT t::uuid AS tenant_id FROM app_tenants_with_due_notifications(@now, sqlc.narg(after_tenant)::uuid, @max_tenants) AS t;
