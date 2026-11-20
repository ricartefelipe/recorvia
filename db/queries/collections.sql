-- Políticas de cobrança, inadimplência de faturas e suspensões pela política.

-- name: ListCollectionPolicies :many
SELECT * FROM collection_policies WHERE tenant_id = @tenant_id ORDER BY is_default DESC, lower(name), id;

-- name: GetCollectionPolicy :one
SELECT * FROM collection_policies WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetCollectionPolicyForUpdate :one
SELECT * FROM collection_policies WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: GetDefaultCollectionPolicy :one
SELECT * FROM collection_policies WHERE tenant_id = @tenant_id AND is_default;

-- name: InsertCollectionPolicy :exec
INSERT INTO collection_policies (id, tenant_id, name, is_default, reminder_offsets_days, send_window_start_hour, send_window_end_hour,
                                 max_messages_per_day, suspend_after_days, reactivate_on_payment, created_by, created_at, updated_at, version)
VALUES (@id, @tenant_id, @name, @is_default, @reminder_offsets_days, @send_window_start_hour, @send_window_end_hour, @max_messages_per_day,
        sqlc.narg(suspend_after_days), @reactivate_on_payment, sqlc.narg(created_by), @created_at, @created_at, 1);

-- name: UpdateCollectionPolicy :execrows
UPDATE collection_policies
   SET name = @name, is_default = @is_default, reminder_offsets_days = @reminder_offsets_days,
       send_window_start_hour = @send_window_start_hour, send_window_end_hour = @send_window_end_hour,
       max_messages_per_day = @max_messages_per_day, suspend_after_days = sqlc.narg(suspend_after_days),
       reactivate_on_payment = @reactivate_on_payment, updated_at = @updated_at, version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version;

-- name: LockCollectionPolicies :exec
-- Serializa, por tenant, as gravações que leem ou trocam a política padrão.
SELECT pg_advisory_xact_lock(hashtextextended(@lock_key::text, 0));

-- name: ClearDefaultCollectionPolicy :exec
UPDATE collection_policies SET is_default = false, updated_at = @updated_at, version = version + 1
 WHERE tenant_id = @tenant_id AND is_default AND id <> @keep_id;

-- name: ReminderCandidates :many
SELECT i.id, i.number, i.status, i.customer_id, i.subscription_id, i.total, i.amount_due, i.amount_paid, i.amount_credited,
       i.issued_at, i.due_at, c.name AS customer_name
  FROM invoices i JOIN customers c ON c.tenant_id = i.tenant_id AND c.id = i.customer_id
 WHERE i.tenant_id = @tenant_id AND i.status IN ('open', 'partially_paid') AND i.amount_due > 0 AND i.due_at <= @horizon
   AND (sqlc.narg(after_due_at)::timestamptz IS NULL OR (i.due_at, i.id) > (sqlc.narg(after_due_at)::timestamptz, sqlc.narg(after_id)::uuid))
 ORDER BY i.due_at, i.id
 LIMIT @row_limit;

-- name: LockOverdueSubscriptionInvoices :many
SELECT i.id, i.number, i.status, i.customer_id, i.subscription_id, i.total, i.amount_due, i.amount_paid, i.amount_credited,
       i.issued_at, i.due_at, c.name AS customer_name
  FROM invoices i JOIN customers c ON c.tenant_id = i.tenant_id AND c.id = i.customer_id
 WHERE i.tenant_id = @tenant_id AND i.subscription_id = @subscription_id AND i.status IN ('open', 'partially_paid')
   AND i.amount_due > 0 AND i.due_at <= @now
 ORDER BY i.id
   FOR SHARE OF i;

-- name: SubscriptionsToReview :many
-- Assinaturas a reavaliar: ativas ou em past_due com fatura vencida a marcar
-- ou a caminho da suspensão, e em past_due sem nenhuma fatura vencida em
-- aberto (voltam a active).
SELECT t.subscription_id::uuid AS subscription_id FROM (
  SELECT i.subscription_id
    FROM invoices i JOIN subscriptions s ON s.tenant_id = i.tenant_id AND s.id = i.subscription_id
   WHERE i.tenant_id = @tenant_id AND i.subscription_id IS NOT NULL AND i.status IN ('open', 'partially_paid')
     AND i.amount_due > 0 AND i.due_at <= @now AND s.status IN ('active', 'past_due')
     AND (s.status = 'active'
          OR NOT EXISTS (SELECT 1 FROM invoice_delinquencies d WHERE d.tenant_id = i.tenant_id AND d.invoice_id = i.id)
          OR (sqlc.narg(suspend_before)::timestamptz IS NOT NULL AND i.due_at <= sqlc.narg(suspend_before)::timestamptz))
  UNION
  SELECT s.id
    FROM subscriptions s
   WHERE s.tenant_id = @tenant_id AND s.status = 'past_due'
     AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.tenant_id = s.tenant_id AND i.subscription_id = s.id
                       AND i.status IN ('open', 'partially_paid') AND i.amount_due > 0 AND i.due_at <= @now)
) t
LIMIT @row_limit;

-- name: UnmarkedOverdueInvoices :many
-- Faturas vencidas e em aberto ainda sem registro de inadimplência que não
-- passam pela revisão de assinaturas: avulsas ou de assinaturas fora de
-- active e past_due (canceladas, suspensas ou em avaliação).
SELECT i.id FROM invoices i
  LEFT JOIN subscriptions s ON s.tenant_id = i.tenant_id AND s.id = i.subscription_id
 WHERE i.tenant_id = @tenant_id AND i.status IN ('open', 'partially_paid') AND i.amount_due > 0 AND i.due_at <= @now
   AND (i.subscription_id IS NULL OR s.status NOT IN ('active', 'past_due'))
   AND NOT EXISTS (SELECT 1 FROM invoice_delinquencies d WHERE d.tenant_id = i.tenant_id AND d.invoice_id = i.id)
 ORDER BY i.due_at, i.id
 LIMIT @row_limit;

-- name: SuspendedByPolicy :many
SELECT subscription_id FROM collection_suspensions WHERE tenant_id = @tenant_id AND status = 'active' ORDER BY suspended_at, id LIMIT @row_limit;

-- name: InsertInvoiceDelinquency :execrows
INSERT INTO invoice_delinquencies (tenant_id, invoice_id, subscription_id, due_at, detected_at)
VALUES (@tenant_id, @invoice_id, sqlc.narg(subscription_id), @due_at, @detected_at)
ON CONFLICT (tenant_id, invoice_id) DO NOTHING;

-- name: ResolveInvoiceDelinquencies :execrows
UPDATE invoice_delinquencies d
   SET resolved_at = @now,
       resolution = CASE
         WHEN i.status = 'void' THEN 'void'
         WHEN i.status = 'uncollectible' THEN 'uncollectible'
         WHEN i.amount_paid = 0 AND i.amount_credited > 0 THEN 'credited'
         ELSE 'paid' END
  FROM invoices i
 WHERE d.tenant_id = @tenant_id AND i.tenant_id = d.tenant_id AND i.id = d.invoice_id AND d.resolved_at IS NULL
   AND (i.status IN ('paid', 'void', 'uncollectible') OR i.amount_due = 0);

-- name: GetActiveCollectionSuspension :one
SELECT * FROM collection_suspensions WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND status = 'active';

-- name: InsertCollectionSuspension :exec
INSERT INTO collection_suspensions (id, tenant_id, subscription_id, policy_id, invoice_id, days_overdue, status, suspended_at)
VALUES (@id, @tenant_id, @subscription_id, @policy_id, @invoice_id, @days_overdue, 'active', @suspended_at);

-- name: EndCollectionSuspension :execrows
UPDATE collection_suspensions SET status = @status, ended_at = @ended_at, end_reason = @end_reason
 WHERE tenant_id = @tenant_id AND id = @id AND status = 'active';

-- name: SuspensionReleasedForInvoice :one
SELECT EXISTS (SELECT 1 FROM collection_suspensions
                WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND invoice_id = @invoice_id
                  AND status = 'released' AND end_reason = 'manual_reactivation')::boolean;

-- name: ListCollectionSuspensions :many
SELECT * FROM collection_suspensions WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id ORDER BY suspended_at, id;

-- name: TenantsForCollections :many
SELECT t::uuid AS tenant_id FROM app_tenants_for_collections(@horizon, sqlc.narg(after_tenant)::uuid, @max_tenants) AS t;
