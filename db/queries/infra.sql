-- Usuários, outbox, inbox, auditoria, idempotência, agendamentos e dead letters.

-- name: UpsertUserFromClaims :one
INSERT INTO users (id, oidc_issuer, oidc_subject, email, display_name, status, created_at, updated_at, last_login_at)
VALUES (@id, @oidc_issuer, @oidc_subject, @email, @display_name, 'active', @now, @now, @now)
ON CONFLICT (oidc_issuer, oidc_subject) DO UPDATE
   SET email = EXCLUDED.email, display_name = EXCLUDED.display_name, last_login_at = EXCLUDED.last_login_at
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = @id;

-- name: IsPlatformStaff :one
SELECT EXISTS (SELECT 1 FROM platform_staff WHERE user_id = @user_id)::boolean;

-- name: InsertOutboxEvent :exec
INSERT INTO outbox_events (id, tenant_id, event_type, aggregate_type, aggregate_id, payload, headers, occurred_at, available_at)
VALUES (@id, @tenant_id, @event_type, @aggregate_type, @aggregate_id, @payload, @headers, @occurred_at, @available_at);

-- name: OutboxClaim :many
SELECT * FROM app_outbox_claim(@worker, @batch_size, @lease_seconds);

-- name: OutboxMarkPublished :one
SELECT app_outbox_mark_published(@worker, @ids::uuid[])::int AS marked;

-- name: OutboxRelease :exec
SELECT app_outbox_release(@worker, @id, @delay_seconds, @error);

-- name: OutboxStats :one
SELECT pending::bigint AS pending, oldest_pending_seconds::double precision AS oldest_pending_seconds, max_attempts::int AS max_attempts
  FROM app_outbox_stats();

-- name: OutboxPurge :one
SELECT app_outbox_purge(@keep_days)::int AS purged;

-- name: InsertInbox :execrows
INSERT INTO inbox_messages (tenant_id, consumer, message_id, processed_at)
VALUES (@tenant_id, @consumer, @message_id, @processed_at)
ON CONFLICT DO NOTHING;

-- name: InsertAuditEntry :exec
INSERT INTO audit_log (id, tenant_id, occurred_at, actor_type, actor_id, actor_label, action, resource_type, resource_id, request_id, details)
VALUES (@id, @tenant_id, @occurred_at, @actor_type, sqlc.narg(actor_id), sqlc.narg(actor_label), @action, @resource_type, sqlc.narg(resource_id), sqlc.narg(request_id), @details);

-- name: ListAuditEvents :many
SELECT * FROM audit_log
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(resource_type)::text IS NULL OR resource_type = sqlc.narg(resource_type))
   AND (sqlc.narg(resource_id)::text IS NULL OR resource_id = sqlc.narg(resource_id))
   AND (sqlc.narg(action)::text IS NULL OR action = sqlc.narg(action))
   AND (sqlc.narg(actor_id)::text IS NULL OR actor_id = sqlc.narg(actor_id))
   AND (sqlc.narg(from_ts)::timestamptz IS NULL OR occurred_at >= sqlc.narg(from_ts))
   AND (sqlc.narg(to_ts)::timestamptz IS NULL OR occurred_at < sqlc.narg(to_ts))
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (occurred_at, id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY occurred_at DESC, id DESC
 LIMIT @row_limit;

-- name: InsertIdempotencyKey :execrows
INSERT INTO idempotency_keys (tenant_id, client_id, operation, idempotency_key, request_hash, status, locked_until, created_at, expires_at)
VALUES (@tenant_id, @client_id, @operation, @idempotency_key, @request_hash, 'in_progress', @locked_until, @now, @expires_at)
ON CONFLICT DO NOTHING;

-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys
 WHERE tenant_id = @tenant_id AND client_id = @client_id AND operation = @operation AND idempotency_key = @idempotency_key;

-- name: TakeOverIdempotencyKey :execrows
-- Recuperação de processamento interrompido: assume a chave apenas se o
-- lease anterior expirou (um único processo vence a disputa).
UPDATE idempotency_keys SET locked_until = @locked_until
 WHERE tenant_id = @tenant_id AND client_id = @client_id AND operation = @operation AND idempotency_key = @idempotency_key
   AND status = 'in_progress' AND locked_until < @now;

-- name: CompleteIdempotencyKey :exec
UPDATE idempotency_keys
   SET status = 'completed', response_status = @response_status, response_body = @response_body,
       response_content_type = @response_content_type, response_location = sqlc.narg(response_location), completed_at = @now,
       locked_until = NULL
 WHERE tenant_id = @tenant_id AND client_id = @client_id AND operation = @operation AND idempotency_key = @idempotency_key;

-- name: ReleaseIdempotencyKey :exec
DELETE FROM idempotency_keys
 WHERE tenant_id = @tenant_id AND client_id = @client_id AND operation = @operation AND idempotency_key = @idempotency_key
   AND status = 'in_progress';

-- name: PurgeExpiredIdempotencyKeys :one
SELECT app_idempotency_purge_expired()::int AS purged;

-- name: EnsureScheduledJob :exec
INSERT INTO scheduled_jobs (name, interval_seconds, next_run_at) VALUES (@name, @interval_seconds, @next_run_at)
ON CONFLICT (name) DO UPDATE SET interval_seconds = EXCLUDED.interval_seconds;

-- name: ClaimScheduledJob :one
UPDATE scheduled_jobs
   SET locked_by = @worker, locked_until = @locked_until, last_started_at = @now
 WHERE name = (
   SELECT j.name FROM scheduled_jobs j
    WHERE j.next_run_at <= @now AND (j.locked_until IS NULL OR j.locked_until < @now)
    ORDER BY j.next_run_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: CompleteScheduledJob :exec
UPDATE scheduled_jobs
   SET locked_by = NULL, locked_until = NULL, last_finished_at = @now, next_run_at = @next_run_at,
       last_error = sqlc.narg(last_error), run_count = run_count + 1
 WHERE name = @name AND locked_by = @worker;

-- name: ListScheduledJobs :many
SELECT * FROM scheduled_jobs ORDER BY name;

-- name: InsertDeadLetter :exec
INSERT INTO dead_letters (id, tenant_id, consumer, message_id, event_type, payload, headers, error, attempts, status, failed_at)
VALUES (@id, @tenant_id, @consumer, @message_id, sqlc.narg(event_type), @payload, @headers, @error, @attempts, 'open', @failed_at)
ON CONFLICT (consumer, message_id) DO NOTHING;

-- name: RecordUntenantedDeadLetter :exec
SELECT app_dead_letter_record_untenanted(@id, @consumer, @message_id, @event_type, @headers, @error, @attempts);

-- name: ListDeadLetters :many
SELECT * FROM dead_letters
 WHERE tenant_id = @tenant_id AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
 ORDER BY failed_at DESC, id DESC LIMIT 200;

-- name: GetDeadLetterForUpdate :one
SELECT * FROM dead_letters WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ResolveDeadLetter :exec
UPDATE dead_letters SET status = @status, resolved_at = @resolved_at, resolved_by = @resolved_by, resolution_note = @resolution_note
 WHERE tenant_id = @tenant_id AND id = @id;

-- name: RetentionTenants :many
-- Tenants com dados do tipo informado vencidos no corte (somente IDs).
SELECT t::uuid AS tenant_id FROM app_retention_tenants(@kind, @cutoff, sqlc.narg(after_tenant)::uuid, @max_tenants) AS t;

-- name: RetentionPurge :one
-- Remove um lote vencido do tipo informado, na transação do tenant.
SELECT purged::int AS purged, more::boolean AS more
  FROM app_retention_purge(@kind, @tenant_id, @cutoff, @now, @batch_size);
