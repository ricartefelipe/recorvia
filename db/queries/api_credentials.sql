-- Credenciais de API (máquina a máquina).

-- name: InsertAPICredential :exec
INSERT INTO api_credentials (id, tenant_id, name, key_id, secret_hash, scopes, created_by, created_at, expires_at, version)
VALUES (@id, @tenant_id, @name, @key_id, @secret_hash, @scopes, @created_by, @created_at, sqlc.narg(expires_at), 1);

-- name: GetAPICredential :one
SELECT * FROM api_credentials WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetAPICredentialForUpdate :one
SELECT * FROM api_credentials WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: ListAPICredentials :many
-- As credenciais ativas (no máximo 50 por organização) vêm sempre primeiro;
-- revogadas e expiradas, que nunca são excluídas, vêm depois, das mais
-- recentes para as mais antigas. A situação é avaliada no instante da
-- primeira página (@now, levado no cursor), para que uma credencial que
-- expira durante a navegação não mude de posição entre as páginas. O cursor é
-- a posição (ativa, criação, id) da última credencial da página.
SELECT * FROM api_credentials
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR
        ((revoked_at IS NULL AND (expires_at IS NULL OR expires_at > @now))::int, created_at, id)
          < (sqlc.narg(cursor_active)::int, sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY (revoked_at IS NULL AND (expires_at IS NULL OR expires_at > @now)) DESC, created_at DESC, id DESC
 LIMIT @row_limit;

-- name: LockTenantForAPICredentialIssuance :one
-- Serializa a emissão de credenciais da organização, para que a contagem de
-- credenciais ativas continue valendo até o commit. FOR NO KEY UPDATE não
-- conflita com as inserções que apenas referenciam a organização (KEY SHARE).
SELECT id FROM tenants WHERE id = @tenant_id FOR NO KEY UPDATE;

-- name: CountActiveAPICredentials :one
SELECT count(*)::int FROM api_credentials
 WHERE tenant_id = @tenant_id AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > @now);

-- name: RotateAPICredential :execrows
UPDATE api_credentials
   SET secret_hash = @secret_hash, previous_secret_hash = @previous_secret_hash,
       previous_secret_expires_at = @previous_secret_expires_at, rotated_by = @rotated_by, rotated_at = @rotated_at,
       version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version AND revoked_at IS NULL;

-- name: RevokeAPICredential :execrows
UPDATE api_credentials
   SET revoked_at = @revoked_at, revoked_by = @revoked_by, version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version AND revoked_at IS NULL;

-- name: TouchAPICredential :exec
-- Registra o último uso com resolução limitada (no máximo uma escrita por
-- intervalo); credenciais revogadas entre a leitura e a escrita são ignoradas.
UPDATE api_credentials SET last_used_at = @used_at
 WHERE tenant_id = @tenant_id AND id = @id AND revoked_at IS NULL
   AND (last_used_at IS NULL OR last_used_at <= @stale_before);

-- name: GetAPICredentialAuthContext :one
-- Reúne em uma leitura o que a autenticação verifica: a credencial, a
-- situação da organização, a associação de quem emitiu o segredo vigente e
-- os recursos do plano da plataforma.
SELECT sqlc.embed(c), t.status AS tenant_status,
       COALESCE(m.role, '')::text AS issuer_role, COALESCE(m.status, '')::text AS issuer_status,
       p.features AS plan_features
  FROM api_credentials c
  JOIN tenants t ON t.id = c.tenant_id
  JOIN tenant_platform_subscriptions s ON s.tenant_id = c.tenant_id
  JOIN platform_plans p ON p.code = s.plan_code
  LEFT JOIN memberships m ON m.tenant_id = c.tenant_id AND m.user_id = COALESCE(c.rotated_by, c.created_by)
 WHERE c.tenant_id = @tenant_id AND c.id = @id;

-- name: FindAPICredential :one
SELECT credential_id::uuid AS credential_id, tenant_id::uuid AS tenant_id FROM app_find_api_credential(@key_id);
