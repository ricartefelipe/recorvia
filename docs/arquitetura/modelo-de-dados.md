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
  subscriptions ||--o{ subscription_prorations : prorrateia
  subscriptions ||--o{ invoices : gera
  invoices ||--o{ invoice_items : discrimina
  invoices ||--o{ invoice_status_history : registra
  invoices ||--o{ charges : cobra
  charges ||--o{ payment_operations : executa
  charges ||--o| payments : confirma
  payment_provider_accounts ||--o{ charges : processa
  payment_provider_accounts ||--o{ provider_webhook_events : recebe
  charges ||--o{ refunds : reembolsa
  refunds ||--o| payment_operations : executa
  charges ||--o{ disputes : contesta
  disputes ||--o{ dispute_events : registra
  payment_provider_accounts ||--o{ reconciliation_runs : concilia
  reconciliation_runs ||--o{ reconciliation_discrepancies : encontra
  charges ||--o{ reconciliation_discrepancies : diverge
  subscriptions ||--o{ usage_events : consome
  usage_aggregates ||--o{ usage_events : fecha
  usage_aggregates |o--|| invoice_items : fatura
  ledger_transactions ||--o{ ledger_entries : contem
  ledger_accounts ||--o{ ledger_entries : movimenta
  tenants ||--o{ outbox_events : publica
  tenants ||--o{ audit_log : audita
  tenants ||--o{ api_credentials : emite
  users ||--o{ api_credentials : responde
  customers ||--o{ customer_portal_links : acessa
  api_credentials |o--o{ customer_portal_links : gera
```

Cobrança, notificações e webhooks enviados (migration `00011`):

```mermaid
erDiagram
  tenants ||--o{ collection_policies : define
  tenants ||--o{ notification_templates : personaliza
  collection_policies ||--o{ notifications : agenda_lembrete
  invoices ||--o{ notifications : avisa
  customers ||--o{ notifications : recebe
  subscriptions ||--o{ notifications : suspende
  invitations ||--o{ notifications : convida
  notifications ||--o{ notification_events : registra
  invoices ||--o| invoice_delinquencies : vence
  subscriptions ||--o{ collection_suspensions : suspensa
  collection_policies ||--o{ collection_suspensions : aplica
  invoices ||--o{ collection_suspensions : motiva
  tenants ||--o{ webhook_endpoints : cadastra
  webhook_endpoints ||--o{ webhook_endpoint_secrets : assina
  tenants ||--o{ webhook_events : emite
  webhook_events ||--o{ webhook_deliveries : distribui
  webhook_endpoints ||--o{ webhook_deliveries : recebe
  webhook_deliveries ||--o{ webhook_delivery_attempts : tenta
```

- `notifications`: identidade de negócio (`business_key`), template, destinatários e assunto em
  claro; conteúdo (texto e HTML) cifrado com o keyring (`content_ciphertext`, `content_key_id`,
  AAD com tenant e notificação); situação, motivo, tentativas, lease, `message_id` e envio.
- `notification_events`: histórico imutável de cada mudança de situação, com motivo e tentativa.
- `webhook_endpoint_secrets`: segredo cifrado (AAD com tenant, endpoint e segredo) e `expires_at`
  (nulo no segredo corrente).
- `webhook_events`: o evento de domínio entregue (`id` = id da mensagem da outbox, usado em
  `webhook-id`), imutável, para que todas as tentativas enviem o mesmo corpo.
- `webhook_delivery_attempts`: uma linha por tentativa (status HTTP, erro, duração e quantidade de
  assinaturas enviadas).

## Tabelas por módulo

| Módulo | Tabelas |
|---|---|
| Organizações e identidade | `tenants`, `users`, `memberships`, `invitations`, `platform_staff`, `api_credentials` |
| Plataforma | `platform_plans`, `platform_addons`, `tenant_platform_subscriptions`, `platform_addon_orders`, `platform_statements` |
| Clientes | `customers`, `customer_contacts`, `customer_billing_addresses`, `customer_provider_refs`, `customer_credit_entries`, `customer_portal_links` |
| Catálogo | `products`, `billable_metrics`, `plans`, `plan_price_versions`, `coupons` |
| Assinaturas | `subscriptions`, `subscription_history`, `subscription_discounts`, `pending_invoice_items`, `subscription_scheduled_changes`, `subscription_price_adjustments`, `subscription_prorations` |
| Consumo | `usage_events`, `usage_aggregates` |
| Faturamento | `invoice_number_sequences`, `invoices`, `invoice_items`, `invoice_status_history`, `credit_notes` |
| Pagamentos | `payment_provider_accounts`, `charges`, `payment_operations`, `payments`, `provider_webhook_events` |
| Recebíveis | `refunds`, `disputes`, `dispute_events`, `reconciliation_runs`, `reconciliation_discrepancies` |
| Ledger | `ledger_accounts`, `ledger_transactions`, `ledger_entries` |
| Cobrança e notificações | `collection_policies`, `notification_templates`, `notifications`, `notification_events`, `invoice_delinquencies`, `collection_suspensions` |
| Webhooks enviados | `webhook_endpoints`, `webhook_endpoint_secrets`, `webhook_events`, `webhook_deliveries`, `webhook_delivery_attempts` |
| Infraestrutura | `outbox_events`, `inbox_messages`, `dead_letters`, `idempotency_keys`, `scheduled_jobs`, `audit_log` |

## Recebíveis

| Tabela ou coluna | Conteúdo |
|---|---|
| `charges.amount_refund_pending` | valor reservado por reembolsos ainda não confirmados; a confirmação o move para `amount_refunded` e a recusa o libera. O saldo reembolsável é `amount_paid - amount_refunded - amount_charged_back - amount_refund_pending` |
| `refunds` | reembolso total ou parcial (`requested`, `succeeded`, `failed`, `unknown`); `origin` distingue pedido do Recorvia (`recorvia`, com `request_key` e `requested_by`) de reembolso feito no provedor (`provider`); `credit_amount` é a parte que devolveu crédito do cliente gerado pelo excedente do próprio pagamento; `provider_refund_id` é único por cobrança |
| `payment_operations` (`kind = 'refund'`, `'cancel_charge'`) | chamada externa do reembolso (vinculada por `refund_id`, referência externa = ID do reembolso) e do cancelamento no provedor (uma por cobrança) |
| `disputes`, `dispute_events` | disputa por cobrança (`opened`, `under_review`, `won`, `lost`) com o valor contestado e o histórico imutável de transições com a situação informada pelo provedor. Na disputa perdida de pagamento com excedente, `credit_amount` é o crédito do cliente consumido pelo chargeback e `recovery_amount` é a parte do excedente que o cliente já havia usado e passa a ser valor a recuperar (ADR-070); as duas ficam em zero fora da disputa perdida e somam, no máximo, o valor contestado |
| `reconciliation_runs` | execução manual ou agendada (`trigger_type`) sobre a janela `[window_start, window_end)` da conta do provedor, com `request_key` derivada do `Idempotency-Key`. A execução manual é registrada em `running` com `lease_until` nulo (aguardando um worker) e `attempts = 0`; o worker que a assume grava o lease e soma uma tentativa. Com o lease expirado (worker interrompido) ou devolvido (lease nulo de novo), a execução continua em `running` e é retomada; ao ser assumida depois de três tentativas interrompidas, é encerrada como `failed` (ADR-071). Há no máximo uma execução em `running` por conta do provedor |
| `reconciliation_charge_checks` | última verificação de cada cobrança por uma execução de conciliação concluída; orienta o rodízio das cobranças fora da janela que ainda podem mudar no provedor (nunca verificadas e verificadas há mais tempo primeiro) |
| `reconciliation_discrepancies` | divergência deduplicada por `dedupe_key` (tipo, objeto e valores) com primeira e última execução, ocorrências, valores inteiros usados na resolução (`local_amount`, `provider_amount`), retratos exibidos (`local_value`, `provider_value`), resolução, justificativa, responsável e lançamento de ajuste |

Funções `SECURITY DEFINER` da etapa: `app_tenants_due_reconciliation` (tenants elegíveis à conciliação
agendada, somente IDs), `app_claimable_reconciliation_runs` (execuções em `running` sem lease ou com o lease
expirado, que um worker pode assumir: somente os IDs do tenant e da execução), `app_open_discrepancy_stats`,
`app_refund_stats` e `app_reconciliation_run_stats` (contagens agregadas para métricas, sem identificar
tenants; a última conta as execuções em andamento por estado, `queued`, `executing` e `stalled`, com a espera
mais longa de cada um). A reivindicação de operações de reembolso usa um bloqueio consultivo transacional por
cobrança (`pg_advisory_xact_lock`), sem bloquear a linha da cobrança.

`idempotency_keys.response_location` guarda o header `Location` da resposta registrada pela
`Idempotency-Key` (nulo quando a resposta não o traz), reproduzido nas repetições com status, corpo e
`Content-Type` (ADR-072).

## Invariantes no banco

| Invariante | Mecanismo |
|---|---|
| Fatura única por assinatura e período | índice único parcial `invoices_subscription_period_key` |
| Numeração única por tenant | `UNIQUE (tenant_id, number)` + sequência bloqueada |
| Evento de consumo único | `UNIQUE (tenant_id, external_id)` |
| Webhook único por provedor | `UNIQUE (tenant_id, provider, provider_event_id)` |
| Cobrança do provedor vinculada uma vez | `UNIQUE (tenant_id, provider, provider_charge_id)` |
| Um pagamento por cobrança; pedido manual único | `payments_charge_key`, `payments_request_key` |
| Reembolso e chargeback nunca maiores que o pago | `charges_refund_limit` (reembolsado + estornado) e `charges_refund_reservation_limit` (reembolsado + reservado + estornado) |
| Pedido de reembolso único; reembolso do provedor registrado uma vez | `refunds_request_key`, `refunds_provider_refund_key` |
| Operação de reembolso vinculada ao reembolso; um cancelamento no provedor por cobrança | `payment_operations_refund_link`, `payment_operations_refund_fk`, `payment_operations_cancel_key` |
| Uma disputa aberta por cobrança; histórico imutável | `disputes_open_key`, `app_reject_mutation` em `dispute_events` |
| Uma conciliação em andamento por conta; divergência única por chave | `reconciliation_runs_running_key`, `reconciliation_discrepancies_dedupe_key` |
| Divergência resolvida com justificativa; descartada somente por `dismiss` | `reconciliation_discrepancies_resolution`, `reconciliation_discrepancies_dismissed` |
| Reembolso, disputa e execução concluídos com data | `refunds_completion`, `disputes_closed`, `reconciliation_runs_finished` |
| Reembolso do provedor sem pedido nem solicitante | `refunds_origin_request` |
| Ledger balanceado e imutável | gatilho de restrição adiado + `app_reject_mutation` |
| Fatura emitida não reescrita | gatilho `app_invoice_guard` |
| Versão de preço imutável | `app_reject_mutation` |
| Nota de crédito imutável, número única por tenant e pedido único | `app_reject_mutation`, `credit_notes_number_key`, `credit_notes_request_key` |
| Uma mudança pendente por assinatura; um reajuste programado por data | `subscription_scheduled_changes_pending_key`, `subscription_price_adjustments_date_key` |
| Item pendente sempre positivo (créditos viram crédito do cliente) | `pending_invoice_items_positive` |
| Valor reajustado sempre ligado ao reajuste que o originou | `subscriptions_override_consistency` |
| Prorrata registrada imutável, dentro do período de cobrança | `app_reject_mutation`, `subscription_prorations_period` |
| Valores na escala do preço unitário comportam `money.MaxAmount` | colunas `numeric(28,10)` (`unit_amount`, reajustes, bases de prorrata) |
| Uma política de cobrança padrão por tenant; nome único | `collection_policies_default_key`, `collection_policies_name_key` |
| Notificação única por identidade de negócio | `notifications_business_key` (`UNIQUE (tenant_id, business_key)`) |
| Inadimplência registrada uma vez por fatura | chave primária de `invoice_delinquencies` |
| Uma suspensão da política ativa por assinatura | `collection_suspensions_active_key` |
| Um segredo corrente por endpoint; aplicação só altera a validade | `webhook_endpoint_secrets_current_key`, `GRANT UPDATE (expires_at)` |
| Uma entrega por endpoint e evento | `webhook_deliveries_event_key` |
| Históricos de notificação e de entrega imutáveis | `app_reject_mutation` em `notification_events`, `webhook_events` e `webhook_delivery_attempts` |
| Total da fatura coerente | `total = subtotal - desconto`; `saldo = total - crédito - pago - creditado - baixado` |
| Credencial guarda só o hash do segredo; identificador único global | `api_credentials_key_id_key`, `secret_hash` de 32 bytes |
| Escopos, nome, autoria e validade da credencial imutáveis; revogada não muda | gatilho `app_api_credentials_guard` |
| Segredo anterior só existe com janela de validade e após rotação | `api_credentials_previous_secret` |
| Um link do portal não revogado por cliente; uso único dentro da validade | `customer_portal_links_active_key`, `customer_portal_links_use_window`, gatilho `app_portal_links_guard` |
| Link do portal com exatamente um autor (membro ou credencial do mesmo tenant) | `customer_portal_links_creator`, FKs compostas |

## Dados do portal do cliente

O portal não tem tabelas próprias de conteúdo: lê e altera dados do tenant sempre no escopo do
cliente da sessão (consultas em `db/queries/portal.sql`).

| Operação | Tabelas | Filtro |
|---|---|---|
| Sessão e resumo | `customer_portal_links`, `customers`, `customer_contacts`, `tenants` | tenant da sessão e cliente do link |
| Faturas e documento | `invoices`, `invoice_items`, `charges` (`checkout_url` da tentativa mais recente aguardando pagamento) | `tenant_id`, `customer_id` e `status <> 'draft'` (índice `invoices_customer_idx`) |
| Preferências de contato | `customer_contacts.receives_billing_email`, `audit_log` | `tenant_id`, `customer_id` e contato do próprio cliente |
