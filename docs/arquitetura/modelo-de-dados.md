# Modelo de dados

O schema completo está em `db/migrations` (Goose). Todas as tabelas de tenant têm `tenant_id`,
`UNIQUE (tenant_id, id)`, FKs compostas e RLS (ver `multitenancy.md`).

```mermaid
erDiagram
  tenants ||--o{ memberships : possui
  users ||--o{ memberships : participa
  tenants ||--o{ invitations : emite
  tenants ||--|| tenant_platform_subscriptions : contrata
  platform_plans ||--o{ tenant_platform_subscriptions : define
  tenants ||--o{ customers : atende
  customers ||--o{ customer_contacts : tem
  customers ||--o| customer_billing_addresses : tem
  customers ||--o{ customer_credit_entries : acumula
  products ||--o{ plans : oferece
  plans ||--o{ plan_price_versions : versiona
  billable_metrics ||--o{ plan_price_versions : mede
  customers ||--o{ subscriptions : assina
  plans ||--o{ subscriptions : contratado
  plan_price_versions ||--o{ subscriptions : fixa_preco
  subscriptions ||--o{ subscription_history : registra
  subscriptions ||--o{ invoices : gera
  invoices ||--o{ invoice_items : discrimina
  invoices ||--o{ invoice_status_history : registra
  invoices ||--o{ charges : cobra
  charges ||--o{ payment_operations : executa
  charges ||--o| payments : confirma
  payment_provider_accounts ||--o{ charges : processa
  payment_provider_accounts ||--o{ provider_webhook_events : recebe
  subscriptions ||--o{ usage_events : consome
  usage_aggregates ||--o{ usage_events : fecha
  usage_aggregates |o--|| invoice_items : fatura
  ledger_transactions ||--o{ ledger_entries : contem
  ledger_accounts ||--o{ ledger_entries : movimenta
  tenants ||--o{ outbox_events : publica
  tenants ||--o{ audit_log : audita
```

## Tabelas por módulo

| Módulo | Tabelas |
|---|---|
| Organizações e identidade | `tenants`, `users`, `memberships`, `invitations`, `platform_staff` |
| Plataforma | `platform_plans`, `platform_addons`, `tenant_platform_subscriptions`, `platform_addon_orders`, `platform_statements` |
| Clientes | `customers`, `customer_contacts`, `customer_billing_addresses`, `customer_provider_refs`, `customer_credit_entries` |
| Catálogo | `products`, `billable_metrics`, `plans`, `plan_price_versions`, `coupons` |
| Assinaturas | `subscriptions`, `subscription_history`, `subscription_discounts`, `pending_invoice_items` |
| Consumo | `usage_events`, `usage_aggregates` |
| Faturamento | `invoice_number_sequences`, `invoices`, `invoice_items`, `invoice_status_history` |
| Pagamentos | `payment_provider_accounts`, `charges`, `payment_operations`, `payments`, `provider_webhook_events` |
| Ledger | `ledger_accounts`, `ledger_transactions`, `ledger_entries` |
| Infraestrutura | `outbox_events`, `inbox_messages`, `dead_letters`, `idempotency_keys`, `scheduled_jobs`, `audit_log` |

## Invariantes no banco

| Invariante | Mecanismo |
|---|---|
| Fatura única por assinatura e período | índice único parcial `invoices_subscription_period_key` |
| Numeração única por tenant | `UNIQUE (tenant_id, number)` + sequência bloqueada |
| Evento de consumo único | `UNIQUE (tenant_id, external_id)` |
| Webhook único por provedor | `UNIQUE (tenant_id, provider, provider_event_id)` |
| Cobrança do provedor vinculada uma vez | `UNIQUE (tenant_id, provider, provider_charge_id)` |
| Um pagamento por cobrança; pedido manual único | `payments_charge_key`, `payments_request_key` |
| Reembolso nunca maior que o pago | `charges_refund_limit` |
| Ledger balanceado e imutável | gatilho de restrição adiado + `app_reject_mutation` |
| Fatura emitida não reescrita | gatilho `app_invoice_guard` |
| Versão de preço imutável | `app_reject_mutation` |
| Total da fatura coerente | `total = subtotal - desconto`; `saldo = total - crédito - pago - creditado - baixado` |
