-- Conferência do keyring contra os dados cifrados de um tenant (ADR-063):
-- ciphertexts e identificadores de chave, sem decifrar nada no banco.

-- name: KeyringProviderAccounts :many
SELECT id, provider, status, api_key_ciphertext, api_key_key_id, webhook_secret_ciphertext, webhook_secret_key_id
  FROM payment_provider_accounts
 WHERE tenant_id = @tenant_id
 ORDER BY provider;

-- name: KeyringWebhookSecrets :many
-- in_use: segredo ainda válido de um endpoint não removido.
SELECT s.id, s.endpoint_id, s.secret_ciphertext, s.secret_key_id,
       ((s.expires_at IS NULL OR s.expires_at > @now) AND e.deleted_at IS NULL)::boolean AS in_use
  FROM webhook_endpoint_secrets s
  JOIN webhook_endpoints e ON e.tenant_id = s.tenant_id AND e.id = s.endpoint_id
 WHERE s.tenant_id = @tenant_id
 ORDER BY s.created_at, s.id;

-- name: KeyringPendingNotifications :many
-- Uma notificação ainda não encerrada por chave: basta uma para provar que a
-- chave decifra, e a fila pendente pode ser grande.
SELECT DISTINCT ON (content_key_id) id, content_ciphertext, content_key_id
  FROM notifications
 WHERE tenant_id = @tenant_id AND status IN ('scheduled', 'sending') AND content_ciphertext IS NOT NULL
 ORDER BY content_key_id, created_at DESC, id DESC;
