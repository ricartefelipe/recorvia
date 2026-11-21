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

-- name: CustomerHasPendingBilling :one
-- Faturamento do cliente que ainda vai gerar fatura, crédito ou item, mesmo
-- sem assinatura ativa nem fatura em aberto: assinatura com fechamento
-- agendado (inclusive o consumo final de uma assinatura cancelada), item
-- pendente ainda sem fatura, consumo atrasado ou sem agregado ainda por
-- corrigir e período fechado dentro da tolerância de atraso da métrica (até
-- lá a ingestão aceita consumo atrasado, que corrige o faturamento). O fim
-- do período é limitado pelo encerramento da assinatura, como na ingestão.
SELECT (EXISTS (SELECT 1 FROM subscriptions s WHERE s.tenant_id = @tenant_id AND s.customer_id = @customer_id
                 AND s.next_billing_at IS NOT NULL)
     OR EXISTS (SELECT 1 FROM subscriptions s
                  JOIN pending_invoice_items p ON p.tenant_id = s.tenant_id AND p.subscription_id = s.id
                 WHERE s.tenant_id = @tenant_id AND s.customer_id = @customer_id AND p.invoice_id IS NULL)
     OR EXISTS (SELECT 1 FROM subscriptions s
                  JOIN usage_events e ON e.tenant_id = s.tenant_id AND e.subscription_id = s.id
                 WHERE s.tenant_id = @tenant_id AND s.customer_id = @customer_id AND e.status = 'late' AND e.corrected_at IS NULL)
     OR EXISTS (SELECT 1 FROM subscriptions s
                  JOIN usage_aggregates a ON a.tenant_id = s.tenant_id AND a.subscription_id = s.id
                  JOIN billable_metrics bm ON bm.tenant_id = a.tenant_id AND bm.id = a.metric_id
                 WHERE s.tenant_id = @tenant_id AND s.customer_id = @customer_id
                   AND (LEAST(a.period_end, COALESCE(s.ended_at, a.period_end)) + make_interval(hours => bm.late_grace_hours) >= @now::timestamptz
                        OR EXISTS (SELECT 1 FROM usage_events e
                                    WHERE e.tenant_id = a.tenant_id AND e.subscription_id = a.subscription_id AND e.metric_id = a.metric_id
                                      AND e.period_start = a.period_start AND e.status = 'accepted' AND e.aggregate_id IS NULL))))::boolean;

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

-- name: AnonymizeCustomer :one
-- Substitui os dados pessoais por valores neutros, arquiva e marca a
-- anonimização. O gatilho customers_anonymized_guard impede alterações
-- posteriores. Devolve a versão e o instante de atualização gravados (o
-- gatilho customers_touch define updated_at).
UPDATE customers
   SET name = @name, legal_name = NULL, document_number = @document_number, email = NULL, phone = NULL, external_id = NULL,
       status = 'archived', anonymized_at = @anonymized_at, version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND anonymized_at IS NULL
RETURNING version, updated_at;

-- name: CustomerAnonymizationDetails :one
-- Detalhes registrados na auditoria da anonimização do cliente, na mesma
-- transação: é de onde a repetição do pedido relê os identificadores do
-- cliente nos provedores, já excluídos do cadastro.
SELECT details FROM audit_log
 WHERE tenant_id = @tenant_id AND resource_type = 'customer' AND resource_id = @resource_id AND action = 'customer.anonymized'
 ORDER BY occurred_at DESC, id DESC
 LIMIT 1;

-- name: DeleteCustomerContacts :execrows
DELETE FROM customer_contacts WHERE tenant_id = @tenant_id AND customer_id = @customer_id;

-- name: DeleteCustomerAddress :execrows
DELETE FROM customer_billing_addresses WHERE tenant_id = @tenant_id AND customer_id = @customer_id;

-- name: DeleteCustomerProviderRefs :many
DELETE FROM customer_provider_refs WHERE tenant_id = @tenant_id AND customer_id = @customer_id
RETURNING provider, provider_customer_id;

-- name: RevokeCustomerPortalLinks :execrows
UPDATE customer_portal_links SET revoked_at = @revoked_at, revoke_reason = 'revoked'
 WHERE tenant_id = @tenant_id AND customer_id = @customer_id AND revoked_at IS NULL;

-- name: DeleteCustomerNotifications :one
SELECT app_customer_delete_notifications(@tenant_id, @customer_id)::int AS deleted;

-- name: PurgeCustomerProviderWebhookPayloads :execrows
-- Esvazia o corpo dos webhooks já processados das cobranças do cliente (o
-- corpo recebido do provedor pode trazer dados do pagador).
UPDATE provider_webhook_events w
   SET payload = '{}'::jsonb, payload_purged_at = @now
  FROM charges c
 WHERE w.tenant_id = @tenant_id AND c.tenant_id = w.tenant_id AND c.customer_id = @customer_id
   AND c.provider = w.provider AND c.provider_charge_id = w.provider_charge_id
   AND w.status IN ('processed', 'ignored') AND w.payload_purged_at IS NULL;
