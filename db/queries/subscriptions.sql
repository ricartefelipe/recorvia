-- Assinaturas, histórico e descontos.

-- name: InsertSubscription :exec
INSERT INTO subscriptions (id, tenant_id, customer_id, plan_id, price_version_id, status, quantity, billing_interval, timezone, anchor_date,
                           trial_ends_at, started_at, current_period_start, current_period_end, next_billing_index, next_billing_at,
                           cancel_at_period_end, canceled_at, cancel_reason, ended_at, suspended_at, suspension_reason, payment_terms_days,
                           created_at, updated_at, version)
VALUES (@id, @tenant_id, @customer_id, @plan_id, @price_version_id, @status, @quantity, @billing_interval, @timezone, @anchor_date,
        sqlc.narg(trial_ends_at), @started_at, @current_period_start, @current_period_end, @next_billing_index, sqlc.narg(next_billing_at),
        @cancel_at_period_end, sqlc.narg(canceled_at), sqlc.narg(cancel_reason), sqlc.narg(ended_at), sqlc.narg(suspended_at),
        sqlc.narg(suspension_reason), sqlc.narg(payment_terms_days), @created_at, @created_at, 1);

-- name: GetSubscription :one
SELECT * FROM subscriptions WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetSubscriptionForUpdate :one
SELECT * FROM subscriptions WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: UpdateSubscription :execrows
UPDATE subscriptions
   SET plan_id = @plan_id, price_version_id = @price_version_id, status = @status, quantity = @quantity,
       trial_ends_at = sqlc.narg(trial_ends_at), current_period_start = @current_period_start, current_period_end = @current_period_end,
       next_billing_index = @next_billing_index, next_billing_at = sqlc.narg(next_billing_at),
       cancel_at_period_end = @cancel_at_period_end, canceled_at = sqlc.narg(canceled_at), cancel_reason = sqlc.narg(cancel_reason),
       ended_at = sqlc.narg(ended_at), suspended_at = sqlc.narg(suspended_at), suspension_reason = sqlc.narg(suspension_reason),
       payment_terms_days = sqlc.narg(payment_terms_days), version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version;

-- name: ClaimDueSubscription :one
-- Reivindica uma assinatura vencida; SKIP LOCKED permite várias instâncias
-- do motor em paralelo sem processar a mesma assinatura.
SELECT * FROM subscriptions
 WHERE tenant_id = @tenant_id AND next_billing_at IS NOT NULL AND next_billing_at <= @now AND status <> 'suspended'
   AND NOT (id = ANY (@exclude::uuid[]))
 ORDER BY next_billing_at, id
 LIMIT 1
 FOR UPDATE SKIP LOCKED;

-- name: TenantsWithDueBilling :many
SELECT t::uuid AS tenant_id FROM app_tenants_with_due_billing(@now, @max_tenants) AS t;

-- name: ListSubscriptions :many
SELECT s.*, c.name AS customer_name, p.name AS plan_name
  FROM subscriptions s
  JOIN customers c ON c.tenant_id = s.tenant_id AND c.id = s.customer_id
  JOIN plans p ON p.tenant_id = s.tenant_id AND p.id = s.plan_id
 WHERE s.tenant_id = @tenant_id
   AND (sqlc.narg(status)::text IS NULL OR s.status = sqlc.narg(status))
   AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id))
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (s.created_at, s.id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY s.created_at DESC, s.id DESC
 LIMIT @row_limit;

-- name: SubscriptionNames :one
SELECT c.name AS customer_name, p.name AS plan_name
  FROM subscriptions s
  JOIN customers c ON c.tenant_id = s.tenant_id AND c.id = s.customer_id
  JOIN plans p ON p.tenant_id = s.tenant_id AND p.id = s.plan_id
 WHERE s.tenant_id = @tenant_id AND s.id = @id;

-- name: InsertSubscriptionHistory :exec
INSERT INTO subscription_history (id, tenant_id, subscription_id, event, from_status, to_status, occurred_at, actor_type, actor_id, details)
VALUES (@id, @tenant_id, @subscription_id, @event, sqlc.narg(from_status), @to_status, @occurred_at, @actor_type, sqlc.narg(actor_id), @details);

-- name: ListSubscriptionHistory :many
SELECT * FROM subscription_history WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id ORDER BY occurred_at, id;

-- name: HasOverdueInvoices :one
SELECT EXISTS (SELECT 1 FROM invoices WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id
                AND status IN ('open', 'partially_paid') AND due_at <= @now)::boolean;

-- name: GetActiveDiscount :one
SELECT d.*, c.code AS coupon_code, c.discount_type, c.percent_off_bp, c.amount_off, c.name AS coupon_name
  FROM subscription_discounts d JOIN coupons c ON c.tenant_id = d.tenant_id AND c.id = d.coupon_id
 WHERE d.tenant_id = @tenant_id AND d.subscription_id = @subscription_id AND d.status = 'active';

-- name: InsertSubscriptionDiscount :exec
INSERT INTO subscription_discounts (id, tenant_id, subscription_id, coupon_id, remaining_cycles, status, applied_by, created_at)
VALUES (@id, @tenant_id, @subscription_id, @coupon_id, sqlc.narg(remaining_cycles), 'active', sqlc.narg(applied_by), @created_at);

-- name: ConsumeDiscountCycle :exec
UPDATE subscription_discounts
   SET remaining_cycles = CASE WHEN remaining_cycles IS NULL THEN NULL ELSE remaining_cycles - 1 END,
       status = CASE WHEN remaining_cycles IS NOT NULL AND remaining_cycles - 1 <= 0 THEN 'ended' ELSE status END,
       ended_at = CASE WHEN remaining_cycles IS NOT NULL AND remaining_cycles - 1 <= 0 THEN @now ELSE ended_at END
 WHERE tenant_id = @tenant_id AND id = @id AND status = 'active';

-- name: EndActiveDiscount :execrows
UPDATE subscription_discounts SET status = 'ended', ended_at = @now
 WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND status = 'active';

-- name: ListOpenPendingItems :many
SELECT * FROM pending_invoice_items WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND invoice_id IS NULL
 ORDER BY created_at, id FOR UPDATE;

-- name: MarkPendingItemsBilled :exec
UPDATE pending_invoice_items SET invoice_id = @invoice_id WHERE tenant_id = @tenant_id AND id = ANY (@ids::uuid[]) AND invoice_id IS NULL;

-- name: InsertPendingItem :exec
INSERT INTO pending_invoice_items (id, tenant_id, customer_id, subscription_id, kind, source, description, amount, quantity, unit_amount,
                                   period_start, period_end, details, created_by, created_at)
VALUES (@id, @tenant_id, @customer_id, @subscription_id, @kind, @source, @description, @amount, sqlc.narg(quantity), sqlc.narg(unit_amount),
        sqlc.narg(period_start), sqlc.narg(period_end), @details, sqlc.narg(created_by), @created_at);
