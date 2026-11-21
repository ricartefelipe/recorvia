-- Ingestão, consulta e correção de eventos de consumo.

-- name: InsertUsageEvent :execrows
INSERT INTO usage_events (id, tenant_id, external_id, customer_id, subscription_id, metric_id, quantity, unique_key, occurred_at,
                          received_at, period_start, status, properties)
VALUES (@id, @tenant_id, @external_id, @customer_id, @subscription_id, @metric_id, @quantity, sqlc.narg(unique_key), @occurred_at,
        @received_at, @period_start, @status, @properties)
ON CONFLICT (tenant_id, external_id) DO NOTHING;

-- name: GetUsageEventByExternalID :one
SELECT * FROM usage_events WHERE tenant_id = @tenant_id AND external_id = @external_id;

-- name: GetSubscriptionForShare :one
-- Leitura da assinatura pela ingestão de consumo. O bloqueio compartilhado
-- espera o faturamento, que reivindica a assinatura com FOR UPDATE SKIP
-- LOCKED e fecha o período na mesma transação; a consulta seguinte de
-- fechamento (UsageAggregateExists) já enxerga o agregado confirmado.
SELECT * FROM subscriptions WHERE tenant_id = @tenant_id AND id = @id FOR SHARE;

-- name: GetCustomerAnonymizedAtForKeyShare :one
-- Situação do cliente lida pela ingestão de consumo. O bloqueio compartilhado
-- (o mesmo da chave estrangeira do evento) espera uma anonimização em
-- andamento, que mantém o cliente com bloqueio exclusivo, e lê o resultado
-- dela: nenhum consumo é gravado para um cliente anonimizado.
SELECT anonymized_at FROM customers WHERE tenant_id = @tenant_id AND id = @id FOR KEY SHARE;

-- name: ListSubscriptionSuspensions :many
-- Suspensões e reativações da assinatura em ordem cronológica: consumo
-- ocorrido durante uma suspensão é rejeitado mesmo depois da reativação.
SELECT event, occurred_at FROM subscription_history
 WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND event IN ('suspended', 'reactivated')
 ORDER BY occurred_at, id;

-- name: GetPendingScheduledChangeForUsage :one
-- Mudança agendada pendente lida pela ingestão de consumo, sem bloqueio: o
-- agendamento e a aplicação da mudança bloqueiam a assinatura com FOR UPDATE
-- antes de alterá-la, e a ingestão já mantém a assinatura em FOR SHARE.
SELECT * FROM subscription_scheduled_changes
 WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND status = 'pending';

-- name: UsageAggregateExists :one
SELECT EXISTS (SELECT 1 FROM usage_aggregates WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id
                AND metric_id = @metric_id AND period_start = @period_start)::boolean;

-- name: ListUsageEvents :many
SELECT e.*, a.invoice_item_id AS aggregate_item_id
  FROM usage_events e
  LEFT JOIN usage_aggregates a ON a.tenant_id = e.tenant_id AND a.id = e.aggregate_id
 WHERE e.tenant_id = @tenant_id
   AND (sqlc.narg(subscription_id)::uuid IS NULL OR e.subscription_id = sqlc.narg(subscription_id))
   AND (sqlc.narg(metric_id)::uuid IS NULL OR e.metric_id = sqlc.narg(metric_id))
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (e.received_at, e.id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY e.received_at DESC, e.id DESC
 LIMIT @row_limit;

-- name: LateUsageGroups :many
-- Grupos (assinatura, métrica, período) a corrigir: eventos atrasados ainda
-- não corrigidos e eventos aceitos que ficaram sem agregado num período já
-- fechado (gravados enquanto o fechamento era confirmado).
SELECT g.subscription_id, g.metric_id, g.period_start FROM (
  SELECT e.subscription_id, e.metric_id, e.period_start FROM usage_events e
   WHERE e.tenant_id = @tenant_id AND e.status = 'late' AND e.corrected_at IS NULL
  UNION
  SELECT a.subscription_id, a.metric_id, a.period_start FROM usage_aggregates a
   WHERE a.tenant_id = @tenant_id
     AND EXISTS (SELECT 1 FROM usage_events e
                  WHERE e.tenant_id = a.tenant_id AND e.subscription_id = a.subscription_id AND e.metric_id = a.metric_id
                    AND e.period_start = a.period_start AND e.status = 'accepted' AND e.aggregate_id IS NULL)
) g
 ORDER BY g.subscription_id, g.metric_id, g.period_start
 LIMIT @row_limit;

-- name: GetUsageAggregateForUpdate :one
SELECT * FROM usage_aggregates
 WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND metric_id = @metric_id AND period_start = @period_start
   FOR UPDATE;

-- name: SetUsageAggregateCorrection :exec
UPDATE usage_aggregates SET corrected_quantity = @corrected_quantity, corrected_amount = @corrected_amount
 WHERE tenant_id = @tenant_id AND id = @id;

-- name: MarkLateUsageCorrected :execrows
-- Marca os eventos do grupo considerados na correção: atrasados e aceitos
-- sem agregado. A situação do evento não muda.
UPDATE usage_events SET aggregate_id = @aggregate_id, corrected_at = @corrected_at, correction_item_id = sqlc.narg(correction_item_id)
 WHERE tenant_id = @tenant_id AND subscription_id = @subscription_id AND metric_id = @metric_id AND period_start = @period_start
   AND corrected_at IS NULL AND (status = 'late' OR (status = 'accepted' AND aggregate_id IS NULL));

-- name: GetInvoiceItemAmounts :one
-- Valor bruto, desconto distribuído e o tipo do desconto registrado no
-- snapshot na emissão ('' em itens faturados antes do registro).
SELECT amount, discount_amount, COALESCE(price_snapshot->>'discount_type', '')::text AS discount_type
  FROM invoice_items WHERE tenant_id = @tenant_id AND id = @id;

-- name: TenantsWithLateUsage :many
SELECT app_tenants_with_late_usage()::uuid AS tenant_id;
