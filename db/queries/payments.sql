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

-- name: UpsertProviderAccount :exec
INSERT INTO payment_provider_accounts (id, tenant_id, provider, environment, status, is_default, api_key_ciphertext, api_key_key_id,
                                       api_key_hint, webhook_secret_ciphertext, webhook_secret_key_id, webhook_endpoint_token_hash,
                                       created_at, updated_at, version)
VALUES (@id, @tenant_id, @provider, @environment, 'active', @is_default, @api_key_ciphertext, @api_key_key_id, @api_key_hint,
        @webhook_secret_ciphertext, @webhook_secret_key_id, @webhook_endpoint_token_hash, @created_at, @created_at, 1)
ON CONFLICT (tenant_id, provider) DO UPDATE
   SET environment = EXCLUDED.environment, status = 'active', is_default = EXCLUDED.is_default,
       api_key_ciphertext = EXCLUDED.api_key_ciphertext, api_key_key_id = EXCLUDED.api_key_key_id, api_key_hint = EXCLUDED.api_key_hint,
       webhook_secret_ciphertext = EXCLUDED.webhook_secret_ciphertext, webhook_secret_key_id = EXCLUDED.webhook_secret_key_id,
       webhook_endpoint_token_hash = EXCLUDED.webhook_endpoint_token_hash, version = payment_provider_accounts.version + 1;

-- name: ResolveWebhookEndpoint :one
SELECT tenant_id::uuid AS tenant_id, account_id::uuid AS account_id FROM app_resolve_webhook_endpoint(@provider, @token_hash);

-- name: LatestCharge :one
SELECT * FROM charges WHERE tenant_id = @tenant_id AND invoice_id = @invoice_id ORDER BY attempt DESC LIMIT 1;

-- name: InsertCharge :exec
INSERT INTO charges (id, tenant_id, invoice_id, customer_id, provider_account_id, provider, attempt, provider_charge_id, status, amount, currency,
                     amount_paid, fee_amount, amount_refunded, amount_charged_back, checkout_url, due_date, paid_at, failure_reason,
                     provider_status, provider_status_at, created_at, updated_at, version)
VALUES (@id, @tenant_id, @invoice_id, @customer_id, @provider_account_id, @provider, @attempt, sqlc.narg(provider_charge_id), @status, @amount,
        @currency, @amount_paid, @fee_amount, @amount_refunded, @amount_charged_back, sqlc.narg(checkout_url), @due_date, sqlc.narg(paid_at),
        sqlc.narg(failure_reason), sqlc.narg(provider_status), sqlc.narg(provider_status_at), @created_at, @created_at, 1);

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
       amount_refunded = @amount_refunded, amount_charged_back = @amount_charged_back, checkout_url = sqlc.narg(checkout_url),
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

-- name: ListOperations :many
SELECT * FROM payment_operations WHERE tenant_id = @tenant_id AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
 ORDER BY updated_at DESC, id DESC LIMIT 200;

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
