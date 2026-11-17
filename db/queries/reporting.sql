-- Indicadores do dashboard. Valores em centavos; períodos [início, fim).

-- name: DashboardMRRComponents :many
-- Componente recorrente fixo das assinaturas ativas ou em atraso, por
-- modelo e periodicidade. O consumo e o excedente não entram no MRR.
SELECT s.billing_interval, pv.pricing_model,
       COALESCE(sum(pv.flat_amount), 0)::bigint AS flat_total,
       COALESCE(sum(pv.unit_amount * s.quantity), 0)::numeric AS per_unit_total
  FROM subscriptions s
  JOIN plan_price_versions pv ON pv.tenant_id = s.tenant_id AND pv.id = s.price_version_id
 WHERE s.tenant_id = @tenant_id AND s.status IN ('active', 'past_due')
 GROUP BY s.billing_interval, pv.pricing_model;

-- name: DashboardBilled :one
SELECT COALESCE(sum(i.total), 0)::bigint AS billed,
       COALESCE(sum(it.recurring), 0)::bigint AS billed_recurring,
       COALESCE(sum(it.usage), 0)::bigint AS billed_usage
  FROM invoices i
  LEFT JOIN LATERAL (
    SELECT sum(amount - discount_amount) FILTER (WHERE kind = 'subscription_fee') AS recurring,
           sum(amount - discount_amount) FILTER (WHERE kind IN ('usage', 'overage')) AS usage
      FROM invoice_items x WHERE x.tenant_id = i.tenant_id AND x.invoice_id = i.id) it ON true
 WHERE i.tenant_id = @tenant_id AND i.status <> 'void' AND i.status <> 'draft'
   AND i.issued_at >= @from_ts AND i.issued_at < @to_ts;

-- name: DashboardReceived :one
SELECT COALESCE(sum(amount), 0)::bigint FROM payments
 WHERE tenant_id = @tenant_id AND paid_at >= @from_ts AND paid_at < @to_ts;

-- name: DashboardReceivables :one
SELECT COALESCE(sum(amount_due), 0)::bigint AS open_total,
       COALESCE(sum(amount_due) FILTER (WHERE due_at <= @now), 0)::bigint AS overdue_total
  FROM invoices WHERE tenant_id = @tenant_id AND status IN ('open', 'partially_paid');

-- name: DashboardDelinquencyBase :one
-- Base da inadimplência: total das faturas com vencimento nos últimos 90 dias.
SELECT COALESCE(sum(total), 0)::bigint FROM invoices
 WHERE tenant_id = @tenant_id AND status NOT IN ('void', 'draft') AND due_at > @since AND due_at <= @now;

-- name: DashboardDelinquencyOpen :one
SELECT COALESCE(sum(amount_due), 0)::bigint FROM invoices
 WHERE tenant_id = @tenant_id AND status IN ('open', 'partially_paid') AND due_at > @since AND due_at <= @now;

-- name: DashboardSubscriptionCounts :one
SELECT count(*) FILTER (WHERE status IN ('trialing', 'active', 'past_due'))::int AS active_now,
       count(*) FILTER (WHERE created_at >= @from_ts AND created_at < @to_ts)::int AS created_in_period,
       count(*) FILTER (WHERE ended_at >= @from_ts AND ended_at < @to_ts)::int AS ended_in_period,
       count(*) FILTER (WHERE started_at < @from_ts AND status <> 'trialing' AND (ended_at IS NULL OR ended_at >= @from_ts))::int AS active_at_start
  FROM subscriptions WHERE tenant_id = @tenant_id;

-- name: DashboardContraRevenue :one
SELECT COALESCE(sum(e.amount) FILTER (WHERE a.code = 'refunds' AND e.direction = 'debit'), 0)::bigint AS refunds,
       COALESCE(sum(e.amount) FILTER (WHERE a.code = 'chargebacks' AND e.direction = 'debit'), 0)::bigint AS chargebacks
  FROM ledger_entries e
  JOIN ledger_accounts a ON a.tenant_id = e.tenant_id AND a.id = e.account_id
  JOIN ledger_transactions t ON t.tenant_id = e.tenant_id AND t.id = e.transaction_id
 WHERE e.tenant_id = @tenant_id AND t.occurred_at >= @from_ts AND t.occurred_at < @to_ts;

-- name: DashboardUnknownOperations :one
SELECT count(*)::int FROM payment_operations WHERE tenant_id = @tenant_id AND status = 'unknown';
