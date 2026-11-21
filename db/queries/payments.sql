-- Contas de provedores, cobranças, operações externas, recebimentos e webhooks.

-- name: GetDefaultProviderAccount :one
SELECT * FROM payment_provider_accounts WHERE tenant_id = @tenant_id AND is_default AND status = 'active';

-- name: GetProviderAccount :one
SELECT * FROM payment_provider_accounts WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetProviderAccountByProvider :one
SELECT * FROM payment_provider_accounts WHERE tenant_id = @tenant_id AND provider = @provider;

-- name: ListProviderAccounts :many
SELECT * FROM payment_provider_accounts WHERE tenant_id = @tenant_id ORDER BY provider;

-- name: ClearDefaultProviderAccount :exec
UPDATE payment_provider_accounts SET is_default = false WHERE tenant_id = @tenant_id AND is_default AND id <> @keep_id;

-- name: InsertProviderAccount :execrows
-- Primeira configuração do provedor (versão esperada 0): nenhuma linha se
-- outra pessoa configurou o provedor antes.
INSERT INTO payment_provider_accounts (id, tenant_id, provider, environment, status, is_default, api_key_ciphertext, api_key_key_id,
                                       api_key_hint, webhook_secret_ciphertext, webhook_secret_key_id, webhook_endpoint_token_hash,
                                       created_at, updated_at, version)
VALUES (@id, @tenant_id, @provider, @environment, 'active', @is_default, @api_key_ciphertext, @api_key_key_id, @api_key_hint,
        @webhook_secret_ciphertext, @webhook_secret_key_id, @webhook_endpoint_token_hash, @created_at, @created_at, 1)
ON CONFLICT (tenant_id, provider) DO NOTHING;

-- name: UpdateProviderAccount :execrows
-- Nova configuração do provedor, condicionada à versão esperada (If-Match).
UPDATE payment_provider_accounts
   SET environment = @environment, status = 'active', is_default = @is_default,
       api_key_ciphertext = @api_key_ciphertext, api_key_key_id = @api_key_key_id, api_key_hint = @api_key_hint,
       webhook_secret_ciphertext = @webhook_secret_ciphertext, webhook_secret_key_id = @webhook_secret_key_id,
       webhook_endpoint_token_hash = @webhook_endpoint_token_hash, version = version + 1
 WHERE tenant_id = @tenant_id AND provider = @provider AND version = @version;

-- name: ResolveWebhookEndpoint :one
SELECT tenant_id::uuid AS tenant_id, account_id::uuid AS account_id FROM app_resolve_webhook_endpoint(@provider, @token_hash);

-- name: LatestCharge :one
SELECT * FROM charges WHERE tenant_id = @tenant_id AND invoice_id = @invoice_id ORDER BY attempt DESC LIMIT 1;

-- name: InsertCharge :exec
INSERT INTO charges (id, tenant_id, invoice_id, customer_id, provider_account_id, provider, attempt, provider_charge_id, status, amount, currency,
                     amount_paid, fee_amount, amount_refunded, amount_refund_pending, amount_charged_back, checkout_url, due_date, paid_at,
                     failure_reason, provider_status, provider_status_at, created_at, updated_at, version)
VALUES (@id, @tenant_id, @invoice_id, @customer_id, @provider_account_id, @provider, @attempt, sqlc.narg(provider_charge_id), @status, @amount,
        @currency, @amount_paid, @fee_amount, @amount_refunded, @amount_refund_pending, @amount_charged_back, sqlc.narg(checkout_url), @due_date,
        sqlc.narg(paid_at), sqlc.narg(failure_reason), sqlc.narg(provider_status), sqlc.narg(provider_status_at), @created_at, @created_at, 1);

-- name: GetCharge :one
SELECT * FROM charges WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetChargeForUpdate :one
SELECT * FROM charges WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: GetChargeByProviderID :one
SELECT * FROM charges WHERE tenant_id = @tenant_id AND provider = @provider AND provider_charge_id = @provider_charge_id;

-- name: GetChargeByProviderIDForUpdate :one
SELECT * FROM charges WHERE tenant_id = @tenant_id AND provider = @provider AND provider_charge_id = @provider_charge_id FOR UPDATE;

-- name: UpdateCharge :execrows
UPDATE charges
   SET provider_charge_id = sqlc.narg(provider_charge_id), status = @status, amount_paid = @amount_paid, fee_amount = @fee_amount,
       amount_refunded = @amount_refunded, amount_refund_pending = @amount_refund_pending, amount_charged_back = @amount_charged_back,
       checkout_url = sqlc.narg(checkout_url),
       paid_at = sqlc.narg(paid_at), failure_reason = sqlc.narg(failure_reason), provider_status = sqlc.narg(provider_status),
       provider_status_at = sqlc.narg(provider_status_at), version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version;

-- name: OpenChargesForInvoice :many
SELECT * FROM charges WHERE tenant_id = @tenant_id AND invoice_id = @invoice_id AND status IN ('pending', 'awaiting_payment') FOR UPDATE;

-- name: ListCharges :many
SELECT * FROM charges
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
   AND (sqlc.narg(invoice_id)::uuid IS NULL OR invoice_id = sqlc.narg(invoice_id))
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY created_at DESC, id DESC
 LIMIT @row_limit;

-- name: InsertOperation :exec
INSERT INTO payment_operations (id, tenant_id, charge_id, refund_id, kind, status, external_reference, attempts, next_attempt_at,
                                created_at, updated_at)
VALUES (@id, @tenant_id, @charge_id, sqlc.narg(refund_id), @kind, @status, @external_reference, 0, @next_attempt_at, @created_at, @created_at);

-- name: GetOperationForUpdate :one
SELECT * FROM payment_operations WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: GetOperation :one
SELECT * FROM payment_operations WHERE tenant_id = @tenant_id AND id = @id;

-- name: UpdateOperation :exec
UPDATE payment_operations
   SET status = @status, attempts = @attempts, lease_until = sqlc.narg(lease_until), next_attempt_at = @next_attempt_at,
       last_error = sqlc.narg(last_error), provider_response = sqlc.narg(provider_response), completed_at = sqlc.narg(completed_at)
 WHERE tenant_id = @tenant_id AND id = @id;

-- name: DueOperationIDs :many
SELECT id FROM payment_operations
 WHERE tenant_id = @tenant_id AND next_attempt_at <= @now AND attempts < @max_attempts
   AND (status IN ('pending', 'unknown') OR (status = 'in_flight' AND lease_until < @now))
 ORDER BY next_attempt_at, id LIMIT @row_limit;

-- name: ExhaustedOperationCounts :many
-- Operações do tenant paradas depois de esgotar as tentativas automáticas
-- (mesma condição de app_tenants_with_pending_payment_operations, com
-- attempts no limite): a recuperação não as reivindica mais. Uma criação
-- pendente cuja cobrança deixou de estar pendente (cancelada com a fatura)
-- não conta: a cobrança não será mais criada (payments.Operation.Exhausted).
SELECT o.status::text AS status, count(*)::bigint AS total
  FROM payment_operations o
  JOIN charges c ON c.tenant_id = o.tenant_id AND c.id = o.charge_id
 WHERE o.tenant_id = @tenant_id AND o.attempts >= @max_attempts AND o.next_attempt_at <= @now
   AND (o.status IN ('pending', 'unknown') OR (o.status = 'in_flight' AND o.lease_until < @now))
   AND NOT (o.status = 'pending' AND o.kind = 'create_charge' AND c.status <> 'pending')
 GROUP BY o.status;

-- name: ListOperations :many
-- Paginação por keyset em (created_at, id): updated_at muda a cada tentativa
-- e deslocaria a operação entre as páginas.
SELECT * FROM payment_operations
 WHERE tenant_id = @tenant_id AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY created_at DESC, id DESC
 LIMIT @row_limit;

-- name: TenantsWithPendingOperations :many
SELECT t::uuid AS tenant_id FROM app_tenants_with_pending_payment_operations(@now) AS t;

-- name: PaymentOperationStats :many
SELECT status::text AS status, total::bigint AS total FROM app_payment_operation_stats();

-- name: InsertPayment :execrows
INSERT INTO payments (id, tenant_id, invoice_id, charge_id, customer_id, amount, fee_amount, currency, method, source, paid_at, note, recorded_by,
                      request_key, created_at)
VALUES (@id, @tenant_id, @invoice_id, sqlc.narg(charge_id), @customer_id, @amount, @fee_amount, @currency, @method, @source, @paid_at, @note,
        sqlc.narg(recorded_by), sqlc.narg(request_key), @created_at)
ON CONFLICT DO NOTHING;

-- name: GetPaymentByRequestKey :one
SELECT * FROM payments WHERE tenant_id = @tenant_id AND request_key = @request_key;

-- name: GetChargePaymentPendingReceipt :one
-- Pagamento da cobrança e o valor dele ainda em pagamentos confirmados no
-- provedor (saldo de provider_confirmed nos lançamentos que referenciam o
-- pagamento: confirmação, excedente e recebimento), à espera do recebimento.
SELECT sqlc.embed(p),
       COALESCE((SELECT sum(CASE WHEN e.direction = 'debit' THEN e.amount ELSE -e.amount END)
                   FROM ledger_transactions t
                   JOIN ledger_entries e ON e.tenant_id = t.tenant_id AND e.transaction_id = t.id
                   JOIN ledger_accounts a ON a.tenant_id = e.tenant_id AND a.id = e.account_id
                  WHERE t.tenant_id = p.tenant_id AND t.reference_type = 'payment' AND t.reference_id = p.id
                    AND a.code = 'provider_confirmed'), 0)::bigint AS pending_receipt
  FROM payments p
 WHERE p.tenant_id = @tenant_id AND p.charge_id = @charge_id;

-- name: ListChargesPendingReceipt :many
-- Valor ainda em pagamentos confirmados no provedor por cobrança, em lote
-- para a conciliação: só as cobranças com pendência de recebimento.
SELECT p.charge_id::uuid AS charge_id,
       sum(CASE WHEN e.direction = 'debit' THEN e.amount ELSE -e.amount END)::bigint AS pending_receipt
  FROM payments p
  JOIN ledger_transactions t ON t.tenant_id = p.tenant_id AND t.reference_type = 'payment' AND t.reference_id = p.id
  JOIN ledger_entries e ON e.tenant_id = t.tenant_id AND e.transaction_id = t.id
  JOIN ledger_accounts a ON a.tenant_id = e.tenant_id AND a.id = e.account_id
 WHERE p.tenant_id = @tenant_id AND p.charge_id = ANY (@charge_ids::uuid[]) AND a.code = 'provider_confirmed'
 GROUP BY p.charge_id
HAVING sum(CASE WHEN e.direction = 'debit' THEN e.amount ELSE -e.amount END) <> 0;

-- name: ListPayments :many
SELECT * FROM payments
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(invoice_id)::uuid IS NULL OR invoice_id = sqlc.narg(invoice_id))
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (paid_at, id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY paid_at DESC, id DESC
 LIMIT @row_limit;

-- name: InsertWebhookEvent :execrows
INSERT INTO provider_webhook_events (id, tenant_id, provider_account_id, provider, provider_event_id, event_type, provider_charge_id,
                                     occurred_at, received_at, payload, status)
VALUES (@id, @tenant_id, @provider_account_id, @provider, @provider_event_id, @event_type, sqlc.narg(provider_charge_id),
        sqlc.narg(occurred_at), @received_at, @payload, 'received')
ON CONFLICT (tenant_id, provider, provider_event_id) DO NOTHING;

-- name: GetWebhookEvent :one
SELECT * FROM provider_webhook_events WHERE tenant_id = @tenant_id AND id = @id;

-- name: UpdateWebhookEventStatus :exec
UPDATE provider_webhook_events SET status = @status, error = sqlc.narg(error), processed_at = @processed_at
 WHERE tenant_id = @tenant_id AND id = @id;
