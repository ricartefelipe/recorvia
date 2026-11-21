-- Faturas, itens, histórico e numeração.

-- name: NextInvoiceNumber :one
-- O bloqueio da linha da sequência serializa a numeração por tenant: sem
-- lacunas nem repetição (a sequência só avança junto com a emissão).
UPDATE invoice_number_sequences SET next_number = next_number + 1
 WHERE tenant_id = @tenant_id
RETURNING (next_number - 1)::bigint AS number;

-- name: CycleInvoiceExists :one
SELECT EXISTS (SELECT 1 FROM invoices WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id
                AND kind = @kind AND billing_period_start = @billing_period_start)::boolean;

-- name: GetCycleFeeItem :one
-- Item do componente fixo da fatura de ciclo do período, com a situação da
-- fatura (há no máximo uma fatura de ciclo por assinatura e período).
SELECT i.id AS invoice_id, i.status AS invoice_status, it.id AS item_id, it.amount, it.discount_amount
  FROM invoices i
  JOIN invoice_items it ON it.tenant_id = i.tenant_id AND it.invoice_id = i.id
 WHERE i.tenant_id = @tenant_id AND i.subscription_id = @subscription_id AND i.kind = 'subscription_cycle'
   AND i.billing_period_start = @period_start AND it.tenant_id = @tenant_id AND it.kind = 'subscription_fee'
 ORDER BY it.position
 LIMIT 1;

-- name: InsertSubscriptionProration :exec
INSERT INTO subscription_prorations (id, tenant_id, subscription_id, period_start, period_end, effective_at, price_version_id, quantity,
                                     remaining_days, period_days, credit_base, credit_amount, charge_base, charge_amount, invoice_id, created_at)
VALUES (@id, @tenant_id, @subscription_id, @period_start, @period_end, @effective_at, @price_version_id, @quantity, @remaining_days,
        @period_days, @credit_base, @credit_amount, @charge_base, @charge_amount, sqlc.narg(invoice_id), @created_at);

-- name: ListPeriodProrations :many
-- Prorratas do período em ordem cronológica, com a situação da fatura de
-- prorrata (nula quando a mudança não emitiu fatura).
SELECT p.id, p.subscription_id, p.period_start, p.period_end, p.effective_at, p.price_version_id, p.quantity, p.remaining_days,
       p.period_days, p.credit_base, p.credit_amount, p.charge_base, p.charge_amount, p.invoice_id, p.created_at,
       i.status AS invoice_status
  FROM subscription_prorations p
  LEFT JOIN invoices i ON i.tenant_id = p.tenant_id AND i.id = p.invoice_id
 WHERE p.tenant_id = @tenant_id AND p.subscription_id = @subscription_id AND p.period_start = @period_start
 ORDER BY p.effective_at, p.created_at, p.id;

-- name: InsertInvoice :exec
INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, number, status, kind, currency, billing_period_start, billing_period_end,
                      issued_at, due_at, subtotal, discount_total, total, credit_applied, amount_paid, amount_credited, amount_written_off,
                      amount_due, customer_snapshot, issuer_snapshot, memo, paid_at, created_at, updated_at, version)
VALUES (@id, @tenant_id, @customer_id, sqlc.narg(subscription_id), sqlc.narg(number), @status, @kind, @currency,
        sqlc.narg(billing_period_start), sqlc.narg(billing_period_end), sqlc.narg(issued_at), sqlc.narg(due_at), @subtotal, @discount_total,
        @total, @credit_applied, @amount_paid, @amount_credited, @amount_written_off, @amount_due, @customer_snapshot,
        sqlc.narg(issuer_snapshot), @memo,
        sqlc.narg(paid_at), @created_at, @created_at, 1);

-- name: InsertInvoiceItem :exec
INSERT INTO invoice_items (id, tenant_id, invoice_id, position, kind, description, quantity, unit_amount, amount, discount_amount,
                           period_start, period_end, plan_id, price_version_id, price_snapshot, usage_aggregate_id, created_at)
VALUES (@id, @tenant_id, @invoice_id, @position, @kind, @description, @quantity, @unit_amount, @amount, @discount_amount,
        sqlc.narg(period_start), sqlc.narg(period_end), sqlc.narg(plan_id), sqlc.narg(price_version_id), @price_snapshot,
        sqlc.narg(usage_aggregate_id), @created_at);

-- name: GetInvoice :one
SELECT * FROM invoices WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetInvoiceForUpdate :one
SELECT * FROM invoices WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListInvoiceItems :many
SELECT * FROM invoice_items WHERE tenant_id = @tenant_id AND invoice_id = @invoice_id ORDER BY position;

-- name: UpdateInvoiceProgress :execrows
-- Somente campos de andamento; o gatilho do banco rejeita alterações nos
-- campos de emissão de faturas emitidas.
UPDATE invoices
   SET status = @status, amount_paid = @amount_paid, amount_credited = @amount_credited, amount_written_off = @amount_written_off,
       amount_due = @amount_due, paid_at = sqlc.narg(paid_at), voided_at = sqlc.narg(voided_at), void_reason = sqlc.narg(void_reason),
       uncollectible_at = sqlc.narg(uncollectible_at), version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version;

-- name: ListInvoices :many
SELECT i.*, c.name AS customer_name
  FROM invoices i JOIN customers c ON c.tenant_id = i.tenant_id AND c.id = i.customer_id
 WHERE i.tenant_id = @tenant_id
   AND (sqlc.narg(status)::text IS NULL OR i.status = sqlc.narg(status))
   AND (sqlc.narg(customer_id)::uuid IS NULL OR i.customer_id = sqlc.narg(customer_id))
   AND (sqlc.narg(subscription_id)::uuid IS NULL OR i.subscription_id = sqlc.narg(subscription_id))
   AND (sqlc.narg(issued_from)::timestamptz IS NULL OR i.issued_at >= sqlc.narg(issued_from))
   AND (sqlc.narg(issued_to)::timestamptz IS NULL OR i.issued_at < sqlc.narg(issued_to))
   AND (NOT @only_overdue::boolean OR (i.status IN ('open', 'partially_paid') AND i.due_at <= @now))
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (i.created_at, i.id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY i.created_at DESC, i.id DESC
 LIMIT @row_limit;

-- name: InvoiceListPosition :many
-- Chave de ordenação da fatura na posição row_offset (base zero) da mesma
-- listagem de ListInvoices, e da seguinte, se houver: usada para limitar a
-- exportação e informar onde começa a parte seguinte.
SELECT i.created_at, i.id
  FROM invoices i
 WHERE i.tenant_id = @tenant_id
   AND (sqlc.narg(status)::text IS NULL OR i.status = sqlc.narg(status))
   AND (sqlc.narg(customer_id)::uuid IS NULL OR i.customer_id = sqlc.narg(customer_id))
   AND (sqlc.narg(subscription_id)::uuid IS NULL OR i.subscription_id = sqlc.narg(subscription_id))
   AND (sqlc.narg(issued_from)::timestamptz IS NULL OR i.issued_at >= sqlc.narg(issued_from))
   AND (sqlc.narg(issued_to)::timestamptz IS NULL OR i.issued_at < sqlc.narg(issued_to))
   AND (NOT @only_overdue::boolean OR (i.status IN ('open', 'partially_paid') AND i.due_at <= @now))
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (i.created_at, i.id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY i.created_at DESC, i.id DESC
 OFFSET @row_offset LIMIT 2;

-- name: InsertInvoiceHistory :exec
INSERT INTO invoice_status_history (id, tenant_id, invoice_id, from_status, to_status, occurred_at, actor_type, actor_id, reason, details)
VALUES (@id, @tenant_id, @invoice_id, sqlc.narg(from_status), @to_status, @occurred_at, @actor_type, sqlc.narg(actor_id), @reason, @details);

-- name: ListInvoiceHistory :many
SELECT * FROM invoice_status_history WHERE tenant_id = @tenant_id AND invoice_id = @invoice_id ORDER BY occurred_at, id;

-- name: ListInvoiceCharges :many
SELECT * FROM charges WHERE tenant_id = @tenant_id AND invoice_id = @invoice_id ORDER BY attempt;

-- name: InsertCreditNote :execrows
INSERT INTO credit_notes (id, tenant_id, invoice_id, number, amount, currency, reason, created_by, request_key, created_at)
VALUES (@id, @tenant_id, @invoice_id, @number, @amount, @currency, @reason, sqlc.narg(created_by), sqlc.narg(request_key), @created_at)
ON CONFLICT (tenant_id, request_key) DO NOTHING;

-- name: GetCreditNoteByRequestKey :one
SELECT * FROM credit_notes WHERE tenant_id = @tenant_id AND request_key = @request_key;

-- name: CountInvoiceCreditNotes :one
SELECT count(*)::int FROM credit_notes WHERE tenant_id = @tenant_id AND invoice_id = @invoice_id;

-- name: ListInvoiceCreditNotes :many
SELECT * FROM credit_notes WHERE tenant_id = @tenant_id AND invoice_id = @invoice_id ORDER BY created_at, id;
