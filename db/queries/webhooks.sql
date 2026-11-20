-- Webhooks enviados: endpoints, segredos, eventos, entregas e tentativas.

-- name: ListWebhookEndpoints :many
SELECT * FROM webhook_endpoints WHERE tenant_id = @tenant_id AND deleted_at IS NULL ORDER BY created_at, id;

-- name: GetWebhookEndpoint :one
SELECT * FROM webhook_endpoints WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetWebhookEndpointForUpdate :one
SELECT * FROM webhook_endpoints WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: CountWebhookEndpoints :one
SELECT count(*)::int FROM webhook_endpoints WHERE tenant_id = @tenant_id AND deleted_at IS NULL;

-- name: LockWebhookEndpoints :exec
-- Serializa, por tenant, a criação de endpoints (limite por tenant).
SELECT pg_advisory_xact_lock(hashtextextended(@lock_key::text, 0));

-- name: InsertWebhookEndpoint :exec
INSERT INTO webhook_endpoints (id, tenant_id, url, description, event_types, status, created_by, created_at, updated_at, version)
VALUES (@id, @tenant_id, @url, @description, @event_types, @status, sqlc.narg(created_by), @created_at, @created_at, 1);

-- name: UpdateWebhookEndpoint :execrows
UPDATE webhook_endpoints
   SET url = @url, description = @description, event_types = @event_types, status = @status, deleted_at = sqlc.narg(deleted_at),
       updated_at = @updated_at, version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version;

-- name: InsertWebhookSecret :exec
INSERT INTO webhook_endpoint_secrets (id, tenant_id, endpoint_id, secret_ciphertext, secret_key_id, created_at)
VALUES (@id, @tenant_id, @endpoint_id, @secret_ciphertext, @secret_key_id, @created_at);

-- name: ExpireWebhookSecrets :exec
UPDATE webhook_endpoint_secrets SET expires_at = @expires_at
 WHERE tenant_id = @tenant_id AND endpoint_id = @endpoint_id AND (expires_at IS NULL OR expires_at > @expires_at);

-- name: RetainWebhookSecrets :exec
-- Encerra no instante now os segredos válidos além dos keep mais recentes (na
-- ordem de assinatura), para que a listagem coincida com os segredos que
-- assinam as entregas.
UPDATE webhook_endpoint_secrets s SET expires_at = @now::timestamptz
 WHERE s.tenant_id = @tenant_id::uuid AND s.endpoint_id = @endpoint_id::uuid AND (s.expires_at IS NULL OR s.expires_at > @now::timestamptz)
   AND s.id NOT IN (SELECT k.id FROM webhook_endpoint_secrets k
                     WHERE k.tenant_id = @tenant_id::uuid AND k.endpoint_id = @endpoint_id::uuid
                       AND (k.expires_at IS NULL OR k.expires_at > @now::timestamptz)
                     ORDER BY (k.expires_at IS NULL) DESC, k.created_at DESC, k.id DESC
                     LIMIT @keep::int);

-- name: ListValidWebhookSecretInfos :many
SELECT id, endpoint_id, created_at, expires_at FROM webhook_endpoint_secrets
 WHERE tenant_id = @tenant_id AND endpoint_id = ANY (@endpoint_ids::uuid[]) AND (expires_at IS NULL OR expires_at > @now)
 ORDER BY (expires_at IS NULL) DESC, created_at DESC, id DESC;

-- name: ListValidWebhookSecrets :many
SELECT * FROM webhook_endpoint_secrets
 WHERE tenant_id = @tenant_id AND endpoint_id = @endpoint_id AND (expires_at IS NULL OR expires_at > @now)
 ORDER BY (expires_at IS NULL) DESC, created_at DESC, id DESC
 LIMIT 5;

-- name: SubscribedWebhookEndpoints :many
SELECT id FROM webhook_endpoints
 WHERE tenant_id = @tenant_id AND status = 'active' AND deleted_at IS NULL AND @event_type::text = ANY (event_types)
 ORDER BY created_at, id;

-- name: InsertOutboundWebhookEvent :execrows
INSERT INTO webhook_events (id, tenant_id, event_type, payload, occurred_at, created_at)
VALUES (@id, @tenant_id, @event_type, @payload, @occurred_at, @created_at)
ON CONFLICT (tenant_id, id) DO NOTHING;

-- name: GetOutboundWebhookEvent :one
SELECT * FROM webhook_events WHERE tenant_id = @tenant_id AND id = @id;

-- name: InsertWebhookDelivery :execrows
INSERT INTO webhook_deliveries (id, tenant_id, endpoint_id, event_id, event_type, status, attempts, max_attempts, next_attempt_at,
                                created_at, updated_at)
VALUES (@id, @tenant_id, @endpoint_id, @event_id, @event_type, @status, 0, @max_attempts, sqlc.narg(next_attempt_at), @created_at, @created_at)
ON CONFLICT (tenant_id, endpoint_id, event_id) DO NOTHING;

-- name: GetWebhookDelivery :one
SELECT * FROM webhook_deliveries WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetWebhookDeliveryForUpdate :one
SELECT * FROM webhook_deliveries WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListWebhookDeliveries :many
SELECT * FROM webhook_deliveries
 WHERE tenant_id = @tenant_id AND endpoint_id = @endpoint_id
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY created_at DESC, id DESC
 LIMIT @row_limit;

-- name: DueWebhookDeliveries :many
SELECT * FROM webhook_deliveries
 WHERE tenant_id = @tenant_id
   AND ((status IN ('pending', 'retrying') AND next_attempt_at <= @now) OR (status = 'delivering' AND lease_until < @now))
 ORDER BY next_attempt_at NULLS FIRST, id
 LIMIT @row_limit
 FOR UPDATE SKIP LOCKED;

-- name: UpdateWebhookDelivery :execrows
UPDATE webhook_deliveries
   SET status = @status, attempts = @attempts, max_attempts = @max_attempts, next_attempt_at = sqlc.narg(next_attempt_at),
       lease_until = sqlc.narg(lease_until), last_status_code = sqlc.narg(last_status_code), last_error = sqlc.narg(last_error),
       delivered_at = sqlc.narg(delivered_at), updated_at = @updated_at
 WHERE tenant_id = @tenant_id AND id = @id AND attempts = @expected_attempts;

-- name: InsertWebhookDeliveryAttempt :exec
INSERT INTO webhook_delivery_attempts (id, tenant_id, delivery_id, attempt, started_at, duration_ms, status_code, error, signature_count)
VALUES (@id, @tenant_id, @delivery_id, @attempt, @started_at, @duration_ms, sqlc.narg(status_code), sqlc.narg(error), @signature_count);

-- name: ListWebhookDeliveryAttempts :many
SELECT * FROM webhook_delivery_attempts WHERE tenant_id = @tenant_id AND delivery_id = @delivery_id ORDER BY attempt;

-- name: TenantsWithDueWebhookDeliveries :many
SELECT t::uuid AS tenant_id FROM app_tenants_with_due_webhook_deliveries(@now, sqlc.narg(after_tenant)::uuid, @max_tenants) AS t;
