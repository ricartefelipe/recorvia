-- Ingestão, consulta e correção de eventos de consumo.

-- name: InsertUsageEvent :execrows
INSERT INTO usage_events (id, tenant_id, external_id, customer_id, subscription_id, metric_id, quantity, unique_key, occurred_at,
                          received_at, period_start, status, properties)
VALUES (@id, @tenant_id, @external_id, @customer_id, @subscription_id, @metric_id, @quantity, sqlc.narg(unique_key), @occurred_at,
        @received_at, @period_start, @status, @properties)
ON CONFLICT (tenant_id, external_id) DO NOTHING;

-- name: GetUsageEventByExternalID :one
SELECT * FROM usage_events WHERE tenant_id = @tenant_id AND external_id = @external_id;

-- name: UsageAggregateExists :one
SELECT EXISTS (SELECT 1 FROM usage_aggregates WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id
                AND metric_id = @metric_id AND period_start = @period_start)::boolean;

-- name: ListUsageEvents :many
SELECT e.*, a.invoice_item_id AS aggregate_item_id
  FROM usage_events e
  LEFT JOIN usage_aggregates a ON a.tenant_id = e.tenant_id AND a.id = e.aggregate_id
 WHERE e.tenant_id = @tenant_id
   AND (sqlc.narg(subscription_id)::uuid IS NULL OR e.subscription_id = sqlc.narg(subscription_id))
   AND (sqlc.narg(metric_id)::uuid IS NULL OR e.metric_id = sqlc.narg(metric_id))
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (e.received_at, e.id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY e.received_at DESC, e.id DESC
 LIMIT @row_limit;

-- name: LateUsageGroups :many
-- Grupos (assinatura, métrica, período) com eventos atrasados ainda não corrigidos.
SELECT DISTINCT subscription_id, metric_id, period_start FROM usage_events
 WHERE tenant_id = @tenant_id AND status = 'late' AND corrected_at IS NULL
 ORDER BY subscription_id, metric_id, period_start
 LIMIT @row_limit;

-- name: GetUsageAggregateForUpdate :one
SELECT * FROM usage_aggregates
 WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND metric_id = @metric_id AND period_start = @period_start
   FOR UPDATE;

-- name: SetUsageAggregateCorrection :exec
UPDATE usage_aggregates SET corrected_quantity = @corrected_quantity, corrected_amount = @corrected_amount
 WHERE tenant_id = @tenant_id AND id = @id;

-- name: MarkLateUsageCorrected :execrows
UPDATE usage_events SET aggregate_id = @aggregate_id, corrected_at = @corrected_at, correction_item_id = sqlc.narg(correction_item_id)
 WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND metric_id = @metric_id AND period_start = @period_start
   AND status = 'late' AND corrected_at IS NULL;

-- name: GetInvoiceItemAmount :one
SELECT amount FROM invoice_items WHERE tenant_id = @tenant_id AND id = @id;

-- name: TenantsWithLateUsage :many
SELECT app_tenants_with_late_usage()::uuid AS tenant_id;
