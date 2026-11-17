-- Clientes, contatos, endereços e créditos.

-- name: InsertCustomer :exec
INSERT INTO customers (id, tenant_id, kind, name, legal_name, document_type, document_number, email, phone, external_id, status,
                       payment_terms_days, created_at, updated_at, version)
VALUES (@id, @tenant_id, @kind, @name, sqlc.narg(legal_name), @document_type, @document_number, sqlc.narg(email), sqlc.narg(phone),
        sqlc.narg(external_id), @status, sqlc.narg(payment_terms_days), @created_at, @created_at, 1);

-- name: GetCustomer :one
SELECT * FROM customers WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetCustomerForUpdate :one
SELECT * FROM customers WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: UpdateCustomer :execrows
UPDATE customers
   SET kind = @kind, name = @name, legal_name = sqlc.narg(legal_name), document_type = @document_type, document_number = @document_number,
       email = sqlc.narg(email), phone = sqlc.narg(phone), external_id = sqlc.narg(external_id), status = @status,
       payment_terms_days = sqlc.narg(payment_terms_days), version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version;

-- name: ListCustomers :many
-- Ordenação por allowlist; desempate por id garante ordem determinística.
-- O cursor é um deslocamento opaco (limite máximo de 100 por página).
SELECT * FROM customers
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
   AND (sqlc.narg(search)::text IS NULL
        OR lower(name) LIKE '%' || sqlc.narg(search) || '%' ESCAPE '\'
        OR document_number LIKE sqlc.narg(search) || '%' ESCAPE '\'
        OR lower(external_id) = sqlc.narg(search))
 ORDER BY
   CASE WHEN @sort::text = 'name' THEN lower(name) END ASC,
   CASE WHEN @sort::text = '-name' THEN lower(name) END DESC,
   CASE WHEN @sort::text = 'created_at' THEN created_at END ASC,
   CASE WHEN @sort::text = '-created_at' THEN created_at END DESC,
   id
 LIMIT @row_limit OFFSET @row_offset;

-- name: ListContacts :many
SELECT * FROM customer_contacts WHERE tenant_id = @tenant_id AND customer_id = @customer_id ORDER BY created_at, id;

-- name: InsertContact :exec
INSERT INTO customer_contacts (id, tenant_id, customer_id, name, email, phone, role, receives_billing_email, created_at, updated_at)
VALUES (@id, @tenant_id, @customer_id, @name, @email, sqlc.narg(phone), @role, @receives_billing_email, @created_at, @created_at);

-- name: DeleteContact :execrows
DELETE FROM customer_contacts WHERE tenant_id = @tenant_id AND customer_id = @customer_id AND id = @id;

-- name: SetContactBillingPreference :execrows
UPDATE customer_contacts SET receives_billing_email = @receives_billing_email
 WHERE tenant_id = @tenant_id AND customer_id = @customer_id AND id = @id;

-- name: GetAddress :one
SELECT * FROM customer_billing_addresses WHERE tenant_id = @tenant_id AND customer_id = @customer_id;

-- name: UpsertAddress :exec
INSERT INTO customer_billing_addresses (tenant_id, customer_id, street, number, complement, district, city, state, postal_code, country, updated_at)
VALUES (@tenant_id, @customer_id, @street, @number, @complement, @district, @city, @state, @postal_code, @country, @updated_at)
ON CONFLICT (tenant_id, customer_id) DO UPDATE
   SET street = EXCLUDED.street, number = EXCLUDED.number, complement = EXCLUDED.complement, district = EXCLUDED.district,
       city = EXCLUDED.city, state = EXCLUDED.state, postal_code = EXCLUDED.postal_code, country = EXCLUDED.country,
       updated_at = EXCLUDED.updated_at;

-- name: CustomerHasOpenObligations :one
SELECT (EXISTS (SELECT 1 FROM subscriptions s WHERE s.tenant_id = @tenant_id AND s.customer_id = @customer_id
                 AND s.status IN ('trialing', 'active', 'past_due', 'suspended'))
     OR EXISTS (SELECT 1 FROM invoices i WHERE i.tenant_id = @tenant_id AND i.customer_id = @customer_id
                 AND i.status IN ('open', 'partially_paid')))::boolean;

-- name: GetProviderCustomerRef :one
SELECT provider_customer_id FROM customer_provider_refs WHERE tenant_id = @tenant_id AND customer_id = @customer_id AND provider = @provider;

-- name: InsertProviderCustomerRef :exec
INSERT INTO customer_provider_refs (tenant_id, customer_id, provider, provider_customer_id)
VALUES (@tenant_id, @customer_id, @provider, @provider_customer_id)
ON CONFLICT (tenant_id, customer_id, provider) DO NOTHING;

-- name: LockCustomerCredits :one
-- Serializa movimentações de crédito do mesmo cliente.
SELECT id FROM customers WHERE tenant_id = @tenant_id AND id = @customer_id FOR UPDATE;

-- name: CustomerCreditBalance :one
SELECT COALESCE(sum(amount), 0)::bigint FROM customer_credit_entries WHERE tenant_id = @tenant_id AND customer_id = @customer_id;

-- name: InsertCreditEntry :execrows
INSERT INTO customer_credit_entries (id, tenant_id, customer_id, amount, currency, kind, invoice_id, reason, created_by, request_key, created_at)
VALUES (@id, @tenant_id, @customer_id, @amount, @currency, @kind, sqlc.narg(invoice_id), @reason, sqlc.narg(created_by), sqlc.narg(request_key), @created_at)
ON CONFLICT (tenant_id, request_key) DO NOTHING;

-- name: ListCreditEntries :many
SELECT * FROM customer_credit_entries WHERE tenant_id = @tenant_id AND customer_id = @customer_id ORDER BY created_at DESC, id DESC LIMIT 200;
