-- Portal do cliente: links temporários de uso único.

-- name: LockCustomerForPortalLink :one
-- Serializa a geração de links do mesmo cliente. FOR NO KEY UPDATE não
-- conflita com as inserções que apenas referenciam o cliente (KEY SHARE).
SELECT * FROM customers WHERE tenant_id = @tenant_id AND id = @id FOR NO KEY UPDATE;

-- name: InsertPortalLink :exec
INSERT INTO customer_portal_links (id, tenant_id, customer_id, token_hash, created_by_user_id, created_by_credential_id, created_at, expires_at)
VALUES (@id, @tenant_id, @customer_id, @token_hash, sqlc.narg(created_by_user_id), sqlc.narg(created_by_credential_id), @created_at, @expires_at);

-- name: RevokeActivePortalLinks :many
UPDATE customer_portal_links SET revoked_at = @revoked_at, revoke_reason = @revoke_reason
 WHERE tenant_id = @tenant_id AND customer_id = @customer_id AND revoked_at IS NULL
RETURNING id;

-- name: GetPortalLink :one
SELECT * FROM customer_portal_links WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetPortalLinkForUpdate :one
SELECT * FROM customer_portal_links WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: MarkPortalLinkUsed :execrows
UPDATE customer_portal_links SET used_at = @used_at
 WHERE tenant_id = @tenant_id AND id = @id AND used_at IS NULL AND revoked_at IS NULL AND expires_at > @used_at;

-- name: FindPortalLink :one
SELECT link_id::uuid AS link_id, tenant_id::uuid AS tenant_id FROM app_find_portal_link(@token_hash);

-- Faturas exibidas no portal: somente as emitidas (rascunhos nunca aparecem)
-- e sempre filtradas pelo cliente da sessão, além do tenant.

-- name: ListPortalInvoices :many
SELECT * FROM invoices
 WHERE tenant_id = @tenant_id AND customer_id = @customer_id AND status <> 'draft'
 ORDER BY issued_at DESC NULLS LAST, created_at DESC, id DESC
 LIMIT @row_limit;

-- name: GetPortalInvoice :one
SELECT * FROM invoices WHERE tenant_id = @tenant_id AND customer_id = @customer_id AND id = @id AND status <> 'draft';

-- name: ListPortalInvoiceItems :many
SELECT * FROM invoice_items
 WHERE tenant_id = @tenant_id AND invoice_id = ANY(@invoice_ids::uuid[])
 ORDER BY invoice_id, position;

-- name: ListPortalCheckoutURLs :many
-- Link do pagamento hospedado da cobrança mais recente aguardando pagamento
-- de cada fatura.
SELECT DISTINCT ON (invoice_id) invoice_id, checkout_url::text AS checkout_url
  FROM charges
 WHERE tenant_id = @tenant_id AND invoice_id = ANY(@invoice_ids::uuid[]) AND status = 'awaiting_payment' AND checkout_url IS NOT NULL
 ORDER BY invoice_id, attempt DESC;
