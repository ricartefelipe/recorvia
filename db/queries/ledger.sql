-- Ledger operacional e consumo medido.

-- name: GetLedgerAccountID :one
SELECT id FROM ledger_accounts WHERE tenant_id = @tenant_id AND code = @code AND currency = @currency;

-- name: InsertLedgerTransaction :execrows
-- A chave de idempotência evita lançamento duplicado em reprocessamentos.
INSERT INTO ledger_transactions (id, tenant_id, kind, description, occurred_at, reference_type, reference_id, idempotency_key,
                                 reverses_transaction_id, created_at)
VALUES (@id, @tenant_id, @kind, @description, @occurred_at, @reference_type, @reference_id, @idempotency_key,
        sqlc.narg(reverses_transaction_id), @created_at)
ON CONFLICT (tenant_id, idempotency_key) DO NOTHING;

-- name: InsertLedgerEntry :exec
INSERT INTO ledger_entries (id, tenant_id, transaction_id, account_id, direction, amount, currency)
VALUES (@id, @tenant_id, @transaction_id, @account_id, @direction, @amount, @currency);

-- name: GetLedgerTransactionByKey :one
SELECT * FROM ledger_transactions WHERE tenant_id = @tenant_id AND idempotency_key = @idempotency_key;

-- name: ListLedgerEntriesForTransactions :many
SELECT e.transaction_id, e.direction, e.amount, e.currency, a.code AS account_code
  FROM ledger_entries e JOIN ledger_accounts a ON a.tenant_id = e.tenant_id AND a.id = e.account_id
 WHERE e.tenant_id = @tenant_id AND e.transaction_id = ANY (@ids::uuid[])
 ORDER BY e.transaction_id, e.direction DESC, a.code;

-- name: ListLedgerTransactions :many
SELECT * FROM ledger_transactions
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(reference_type)::text IS NULL OR reference_type = sqlc.narg(reference_type))
   AND (sqlc.narg(reference_id)::uuid IS NULL OR reference_id = sqlc.narg(reference_id))
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY created_at DESC, id DESC
 LIMIT @row_limit;

-- name: LedgerBalances :many
SELECT a.code, a.name, a.kind, a.currency,
       COALESCE(sum(e.amount) FILTER (WHERE e.direction = 'debit'), 0)::bigint AS debits,
       COALESCE(sum(e.amount) FILTER (WHERE e.direction = 'credit'), 0)::bigint AS credits
  FROM ledger_accounts a
  LEFT JOIN ledger_entries e ON e.tenant_id = a.tenant_id AND e.account_id = a.id
 WHERE a.tenant_id = @tenant_id
 GROUP BY a.code, a.name, a.kind, a.currency
 ORDER BY a.code;

-- name: UsageAggregateSum :one
SELECT COALESCE(sum(quantity), 0)::numeric AS quantity, count(*)::int AS event_count FROM usage_events
 WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND metric_id = @metric_id AND period_start = @period_start;

-- name: UsageAggregateMax :one
SELECT COALESCE(max(quantity), 0)::numeric AS quantity, count(*)::int AS event_count FROM usage_events
 WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND metric_id = @metric_id AND period_start = @period_start;

-- name: UsageAggregateUniqueCount :one
SELECT count(DISTINCT unique_key)::numeric AS quantity, count(*)::int AS event_count FROM usage_events
 WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND metric_id = @metric_id AND period_start = @period_start;

-- name: UsageAggregateLast :one
WITH ev AS (
  SELECT u.quantity, u.occurred_at, u.received_at, u.id FROM usage_events u
   WHERE u.tenant_id = @tenant_id AND u.subscription_id = @subscription_id AND u.metric_id = @metric_id AND u.period_start = @period_start)
SELECT COALESCE((SELECT ev.quantity FROM ev ORDER BY ev.occurred_at DESC, ev.received_at DESC, ev.id DESC LIMIT 1), 0)::numeric AS quantity,
       (SELECT count(*) FROM ev)::int AS event_count;

-- name: InsertUsageAggregate :exec
INSERT INTO usage_aggregates (id, tenant_id, subscription_id, metric_id, period_start, period_end, aggregation, quantity, event_count, closed_at)
VALUES (@id, @tenant_id, @subscription_id, @metric_id, @period_start, @period_end, @aggregation, @quantity, @event_count, @closed_at);

-- name: AttachUsageEventsToAggregate :exec
UPDATE usage_events SET aggregate_id = @aggregate_id
 WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND metric_id = @metric_id AND period_start = @period_start
   AND aggregate_id IS NULL;

-- name: SetUsageAggregateItem :exec
UPDATE usage_aggregates SET invoice_item_id = @invoice_item_id WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetUsageAggregate :one
SELECT * FROM usage_aggregates WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND metric_id = @metric_id AND period_start = @period_start;
