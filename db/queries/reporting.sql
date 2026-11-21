-- Indicadores do dashboard. Valores em centavos; períodos [início, fim).
-- Fórmulas documentadas em docs/arquitetura/indicadores.md (ADR-074).
-- @position_at é o instante da posição: o último instante do mês consultado
-- ou, no mês corrente, o momento da consulta.

-- name: DashboardMRRComponents :many
-- Componente recorrente fixo vigente (posição atual) das assinaturas ativas
-- ou em atraso, por periodicidade. gross_total é o componente fixo com o
-- reajuste da assinatura (price_override_amount, ADR-027): valor fixo nos
-- modelos fixo e fixo com excedente, preço por unidade × quantidade no modelo
-- por unidade. discount_total é o desconto recorrente do cupom ativo de
-- duração "forever" ou "repeating" (o de uso único fica fora): percentual
-- sobre o componente fixo ou valor fixo limitado a ele. Valores exatos
-- (numeric), divididos por 12 nos planos anuais e arredondados na aplicação.
-- O consumo e o excedente não entram no MRR.
SELECT m.billing_interval,
       COALESCE(sum(m.fixed), 0)::numeric AS gross_total,
       COALESCE(sum(CASE m.discount_type
                      WHEN 'percent' THEN m.fixed * m.percent_off_bp * 0.0001
                      WHEN 'amount' THEN LEAST(m.amount_off::numeric, m.fixed)
                      ELSE 0 END), 0)::numeric AS discount_total
  FROM (
    SELECT s.billing_interval,
           (CASE pv.pricing_model
              WHEN 'flat' THEN COALESCE(s.price_override_amount, pv.flat_amount::numeric)
              WHEN 'flat_plus_overage' THEN COALESCE(s.price_override_amount, pv.flat_amount::numeric)
              WHEN 'per_unit' THEN COALESCE(s.price_override_amount, pv.unit_amount) * s.quantity
              ELSE 0 END)::numeric AS fixed,
           c.discount_type, c.percent_off_bp, c.amount_off
      FROM subscriptions s
      JOIN plan_price_versions pv ON pv.tenant_id = s.tenant_id AND pv.id = s.price_version_id
      LEFT JOIN subscription_discounts d ON d.tenant_id = s.tenant_id AND d.subscription_id = s.id AND d.status = 'active'
      LEFT JOIN coupons c ON c.tenant_id = d.tenant_id AND c.id = d.coupon_id AND c.duration <> 'once'
     WHERE s.tenant_id = @tenant_id AND s.status IN ('active', 'past_due')) m
 GROUP BY m.billing_interval;

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

-- name: DashboardReceivablesCurrent :one
-- Recebíveis na posição atual (mês corrente): amount_due das faturas em
-- aberto. Equivale ao saldo de contas a receber por fatura no ledger
-- (DashboardReceivablesAt no instante atual), sem ler o histórico.
SELECT COALESCE(sum(amount_due), 0)::bigint AS open_total,
       COALESCE(sum(amount_due) FILTER (WHERE due_at <= @position_at), 0)::bigint AS overdue_total,
       COALESCE(sum(amount_due) FILTER (WHERE due_at > @since AND due_at <= @position_at), 0)::bigint AS delinquency_open
  FROM invoices
 WHERE tenant_id = @tenant_id AND status IN ('open', 'partially_paid');

-- name: DashboardReceivablesAt :one
-- Recebíveis no instante da posição de um mês encerrado, pelo ledger: saldo
-- de contas a receber por fatura (emissão menos crédito aplicado, pagamentos,
-- notas de crédito e baixas como incobrável, com os estornos) até
-- @position_at. Só as faturas que podiam ter saldo na posição entram:
-- emitidas até ela e ainda em aberto, ou pagas, canceladas ou baixadas
-- depois dela (paid_at, voided_at, uncollectible_at); uma fatura encerrada
-- antes da posição tinha saldo zero. Cada ramo usa um índice próprio: com RLS,
-- só comparações simples de coluna (operadores leakproof) viram condição de
-- índice, o que exclui uma expressão como COALESCE. As partidas de cada fatura são achadas pela referência do lançamento
-- (a fatura, os seus pagamentos e as suas notas de crédito), por índice.
WITH candidates AS (
  SELECT i.id FROM invoices i
   WHERE i.tenant_id = @tenant_id AND i.status IN ('open', 'partially_paid') AND i.issued_at <= @position_at
  UNION
  SELECT i.id FROM invoices i WHERE i.tenant_id = @tenant_id AND i.paid_at > @position_at AND i.issued_at <= @position_at
  UNION
  SELECT i.id FROM invoices i WHERE i.tenant_id = @tenant_id AND i.voided_at > @position_at AND i.issued_at <= @position_at
  UNION
  SELECT i.id FROM invoices i WHERE i.tenant_id = @tenant_id AND i.uncollectible_at > @position_at AND i.issued_at <= @position_at
), refs AS (
  SELECT c.id AS invoice_id, 'invoice'::text AS reference_type, c.id AS reference_id FROM candidates c
  UNION ALL
  SELECT c.id, 'payment', p.id FROM candidates c JOIN payments p ON p.tenant_id = @tenant_id AND p.invoice_id = c.id
  UNION ALL
  SELECT c.id, 'credit_note', cn.id FROM candidates c JOIN credit_notes cn ON cn.tenant_id = @tenant_id AND cn.invoice_id = c.id
), balances AS (
  SELECT r.invoice_id, sum(CASE WHEN e.direction = 'debit' THEN e.amount ELSE -e.amount END) AS balance
    FROM refs r
    JOIN ledger_transactions t ON t.tenant_id = @tenant_id AND t.reference_type = r.reference_type AND t.reference_id = r.reference_id
    JOIN ledger_entries e ON e.tenant_id = t.tenant_id AND e.transaction_id = t.id
    JOIN ledger_accounts a ON a.tenant_id = e.tenant_id AND a.id = e.account_id
   WHERE a.code = 'accounts_receivable' AND t.occurred_at <= @position_at
   GROUP BY r.invoice_id
)
SELECT COALESCE(sum(b.balance) FILTER (WHERE b.balance > 0), 0)::bigint AS open_total,
       COALESCE(sum(b.balance) FILTER (WHERE b.balance > 0 AND i.due_at <= @position_at), 0)::bigint AS overdue_total,
       COALESCE(sum(b.balance) FILTER (WHERE b.balance > 0 AND i.due_at > @since AND i.due_at <= @position_at), 0)::bigint AS delinquency_open
  FROM balances b
  JOIN invoices i ON i.tenant_id = @tenant_id AND i.id = b.invoice_id;

-- name: DashboardDelinquencyBase :one
-- Base da inadimplência: total das faturas emitidas com vencimento nos 90
-- dias anteriores à posição, sem as canceladas até o instante da posição.
SELECT COALESCE(sum(total), 0)::bigint FROM invoices
 WHERE tenant_id = @tenant_id AND status <> 'draft' AND due_at > @since AND due_at <= @position_at
   AND (voided_at IS NULL OR voided_at > @position_at);

-- name: DashboardSubscriptionCounts :one
-- Situação de cada assinatura num instante: a do último registro do
-- histórico (subscription_history) até o instante. Sem registro até o
-- instante (início retroativo: o histórico começa na criação), a situação é
-- derivada do contrato: em trial enquanto trial_ends_at for posterior ao
-- instante, ativa depois. O encerramento usa ended_at (o fim do período no
-- cancelamento ao fim do período, registrado depois pelo motor). Assinaturas
-- encerradas antes do mês não entram em nenhum indicador e ficam fora.
WITH s AS (
  SELECT sub.started_at, sub.ended_at,
         COALESCE((SELECT h.to_status FROM subscription_history h
                    WHERE h.tenant_id = sub.tenant_id AND h.subscription_id = sub.id AND h.occurred_at < @from_ts
                    ORDER BY h.occurred_at DESC, h.id DESC LIMIT 1),
                  CASE WHEN sub.trial_ends_at >= @from_ts THEN 'trialing' ELSE 'active' END) AS status_at_start,
         COALESCE((SELECT h.to_status FROM subscription_history h
                    WHERE h.tenant_id = sub.tenant_id AND h.subscription_id = sub.id AND h.occurred_at <= @position_at
                    ORDER BY h.occurred_at DESC, h.id DESC LIMIT 1),
                  CASE WHEN sub.trial_ends_at > @position_at THEN 'trialing' ELSE 'active' END) AS status_at_position
    FROM subscriptions sub
   WHERE sub.tenant_id = @tenant_id AND sub.started_at < @to_ts AND (sub.ended_at IS NULL OR sub.ended_at >= @from_ts)
), c AS (
  SELECT s.*, (s.started_at < @from_ts AND s.status_at_start IN ('active', 'past_due', 'suspended')) AS active_at_start
    FROM s
)
SELECT count(*) FILTER (WHERE status_at_position IN ('trialing', 'active', 'past_due')
                          AND (ended_at IS NULL OR ended_at > @position_at))::int AS active_at_position,
       count(*) FILTER (WHERE started_at >= @from_ts)::int AS started_in_period,
       count(*) FILTER (WHERE ended_at < @to_ts)::int AS ended_in_period,
       count(*) FILTER (WHERE active_at_start)::int AS active_at_start,
       count(*) FILTER (WHERE active_at_start AND ended_at < @to_ts)::int AS churned_in_period
  FROM c;

-- name: DashboardContraRevenue :one
-- Movimento líquido (débitos menos créditos) das contas de reembolsos e de
-- chargebacks no período. Os créditos em chargebacks são a retenção liberada
-- na disputa perdida (dispute.hold_released), o estorno da disputa ganha e a
-- parte do chargeback que era pagamento excedente (dispute.credit_reversed):
-- somar só os débitos contaria duas vezes o valor reembolsado ao pagador, o
-- valor de disputa ganha e o crédito do cliente estornado.
SELECT COALESCE(sum(CASE WHEN e.direction = 'debit' THEN e.amount ELSE -e.amount END) FILTER (WHERE a.code = 'refunds'), 0)::bigint AS refunds,
       COALESCE(sum(CASE WHEN e.direction = 'debit' THEN e.amount ELSE -e.amount END) FILTER (WHERE a.code = 'chargebacks'), 0)::bigint AS chargebacks
  FROM ledger_entries e
  JOIN ledger_accounts a ON a.tenant_id = e.tenant_id AND a.id = e.account_id
  JOIN ledger_transactions t ON t.tenant_id = e.tenant_id AND t.id = e.transaction_id
 WHERE e.tenant_id = @tenant_id AND a.code IN ('refunds', 'chargebacks')
   AND t.occurred_at >= @from_ts AND t.occurred_at < @to_ts;

-- name: DashboardRecoverables :one
-- Saldo da conta de valores a recuperar de clientes (débitos menos créditos)
-- no instante da posição: o excedente já usado estornado por chargeback menos
-- as baixas recebidas e perdidas.
SELECT COALESCE(sum(CASE WHEN e.direction = 'debit' THEN e.amount ELSE -e.amount END), 0)::bigint AS balance
  FROM ledger_entries e
  JOIN ledger_accounts a ON a.tenant_id = e.tenant_id AND a.id = e.account_id
  JOIN ledger_transactions t ON t.tenant_id = e.tenant_id AND t.id = e.transaction_id
 WHERE e.tenant_id = @tenant_id AND a.code = 'customer_recoverables' AND t.occurred_at <= @position_at;

-- name: DashboardUnknownOperations :one
-- Posição atual: operações de pagamento sem histórico de situação.
SELECT count(*)::int FROM payment_operations WHERE tenant_id = @tenant_id AND status = 'unknown';

-- name: DashboardOpenDiscrepancies :one
-- Posição atual: divergências de conciliação abertas (fila de trabalho).
SELECT count(*)::int FROM reconciliation_discrepancies WHERE tenant_id = @tenant_id AND status = 'open';
