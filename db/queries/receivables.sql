-- Reembolsos, disputas, cancelamento no provedor e conciliação.

-- name: InsertRefund :exec
INSERT INTO refunds (id, tenant_id, charge_id, provider, origin, status, amount, credit_amount, currency, reason, provider_refund_id,
                     failure_reason, requested_by, request_key, created_at, updated_at, completed_at, version)
VALUES (@id, @tenant_id, @charge_id, @provider, @origin, @status, @amount, @credit_amount, @currency, @reason, sqlc.narg(provider_refund_id),
        sqlc.narg(failure_reason), sqlc.narg(requested_by), sqlc.narg(request_key), @created_at, @created_at, sqlc.narg(completed_at), 1);

-- name: GetRefund :one
SELECT * FROM refunds WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetRefundForUpdate :one
SELECT * FROM refunds WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: GetRefundByRequestKey :one
SELECT * FROM refunds WHERE tenant_id = @tenant_id AND request_key = @request_key;

-- name: UpdateRefund :execrows
UPDATE refunds
   SET status = @status, credit_amount = @credit_amount, provider_refund_id = sqlc.narg(provider_refund_id),
       failure_reason = sqlc.narg(failure_reason), completed_at = sqlc.narg(completed_at), version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version;

-- name: ListChargeRefunds :many
SELECT * FROM refunds WHERE tenant_id = @tenant_id AND charge_id = @charge_id ORDER BY created_at, id;

-- name: ListRefundsForCharges :many
SELECT * FROM refunds WHERE tenant_id = @tenant_id AND charge_id = ANY (@charge_ids::uuid[]) ORDER BY charge_id, created_at, id;

-- name: ListRefunds :many
SELECT * FROM refunds
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(charge_id)::uuid IS NULL OR charge_id = sqlc.narg(charge_id))
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY created_at DESC, id DESC
 LIMIT @row_limit;

-- name: ChargeOverpaymentAmount :one
-- Excedente do pagamento da cobrança convertido em crédito do cliente
-- (lançamento payment.overpayment do pagamento).
SELECT COALESCE(sum(e.amount), 0)::bigint AS amount
  FROM payments p
  JOIN ledger_transactions t ON t.tenant_id = p.tenant_id AND t.idempotency_key = 'payment.overpayment:' || p.id::text
  JOIN ledger_entries e ON e.tenant_id = t.tenant_id AND e.transaction_id = t.id AND e.direction = 'credit'
 WHERE p.tenant_id = @tenant_id AND p.charge_id = @charge_id;

-- name: ChargeRefundedCreditAmount :one
SELECT COALESCE(sum(credit_amount), 0)::bigint AS amount FROM refunds
 WHERE tenant_id = @tenant_id AND charge_id = @charge_id AND status = 'succeeded';

-- name: ChargeChargebackCreditAmount :one
-- Excedente do pagamento da cobrança já estornado por chargeback: crédito
-- consumido do saldo do cliente e valor a recuperar das disputas perdidas.
SELECT COALESCE(sum(credit_amount + recovery_amount), 0)::bigint AS amount FROM disputes
 WHERE tenant_id = @tenant_id AND charge_id = @charge_id AND status = 'lost';

-- Bloqueio consultivo até o fim da transação: serializa a reivindicação das
-- operações de reembolso de uma cobrança sem bloquear a linha da cobrança.
-- name: LockChargeRefundClaims :exec
SELECT pg_advisory_xact_lock(hashtextextended(concat('refund_claim:', sqlc.arg(tenant_id)::uuid, ':', sqlc.arg(charge_id)::uuid), 0));

-- name: ListChargeOperations :many
SELECT * FROM payment_operations WHERE tenant_id = @tenant_id AND charge_id = @charge_id AND kind = @kind ORDER BY created_at, id;

-- name: InsertOperationIfAbsent :execrows
INSERT INTO payment_operations (id, tenant_id, charge_id, refund_id, kind, status, external_reference, attempts, next_attempt_at,
                                created_at, updated_at)
VALUES (@id, @tenant_id, @charge_id, sqlc.narg(refund_id), @kind, @status, @external_reference, 0, @next_attempt_at, @created_at, @created_at)
ON CONFLICT DO NOTHING;

-- name: ListUnknownOperations :many
-- Operações com resultado desconhecido: situação unknown ou em execução com o
-- lease expirado (processo interrompido durante a chamada ao provedor).
SELECT * FROM payment_operations
 WHERE tenant_id = @tenant_id
   AND (status = 'unknown' OR (status = 'in_flight' AND (lease_until IS NULL OR lease_until < @now)))
 ORDER BY created_at, id LIMIT @row_limit;

-- name: RefundStats :many
SELECT status::text AS status, total::bigint AS total FROM app_refund_stats();

-- name: InsertDispute :exec
INSERT INTO disputes (id, tenant_id, charge_id, provider, status, amount, currency, provider_status, opened_at, closed_at, created_at, updated_at, version)
VALUES (@id, @tenant_id, @charge_id, @provider, @status, @amount, @currency, sqlc.narg(provider_status), @opened_at, sqlc.narg(closed_at),
        @created_at, @created_at, 1);

-- name: GetLatestDisputeForCharge :one
SELECT * FROM disputes WHERE tenant_id = @tenant_id AND charge_id = @charge_id ORDER BY opened_at DESC, id DESC LIMIT 1;

-- name: UpdateDispute :execrows
UPDATE disputes
   SET status = @status, provider_status = sqlc.narg(provider_status), closed_at = sqlc.narg(closed_at), credit_amount = @credit_amount,
       recovery_amount = @recovery_amount, version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version;

-- name: GetDispute :one
SELECT * FROM disputes WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetDisputeForUpdate :one
-- Bloqueia a disputa até o fim da transação: serializa as baixas do valor a
-- recuperar e a repetição do pedido com a mesma chave.
SELECT * FROM disputes WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: InsertRecoverySettlement :exec
-- O gatilho dispute_recovery_settlements_apply acumula a baixa nos totais da
-- disputa; a constraint disputes_recovery_settled_within recusa a baixa
-- acima do valor a recuperar.
INSERT INTO dispute_recovery_settlements (id, tenant_id, dispute_id, outcome, amount, currency, effective_date, justification, settled_by,
                                          request_key, ledger_transaction_id, created_at)
VALUES (@id, @tenant_id, @dispute_id, @outcome, @amount, @currency, @effective_date, @justification, sqlc.narg(settled_by),
        sqlc.narg(request_key), @ledger_transaction_id, @created_at);

-- name: GetRecoverySettlementByRequestKey :one
SELECT * FROM dispute_recovery_settlements WHERE tenant_id = @tenant_id AND request_key = @request_key;

-- name: ListDisputeRecoverySettlements :many
SELECT * FROM dispute_recovery_settlements WHERE tenant_id = @tenant_id AND dispute_id = @dispute_id ORDER BY created_at, id;

-- name: InsertDisputeEvent :exec
INSERT INTO dispute_events (id, tenant_id, dispute_id, from_status, to_status, provider_status, occurred_at, recorded_at, details)
VALUES (@id, @tenant_id, @dispute_id, sqlc.narg(from_status), @to_status, sqlc.narg(provider_status), @occurred_at, @recorded_at, @details);

-- name: ListDisputes :many
SELECT * FROM disputes
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(charge_id)::uuid IS NULL OR charge_id = sqlc.narg(charge_id))
 ORDER BY opened_at DESC, id DESC
 LIMIT @row_limit;

-- name: ListDisputeEvents :many
SELECT * FROM dispute_events WHERE tenant_id = @tenant_id AND dispute_id = ANY (@dispute_ids::uuid[]) ORDER BY dispute_id, recorded_at, id;

-- name: InsertReconciliationRun :exec
INSERT INTO reconciliation_runs (id, tenant_id, provider_account_id, provider, trigger_type, status, window_start, window_end, checked_count,
                                 discrepancy_count, error, requested_by, request_key, lease_until, attempts, started_at, finished_at, updated_at)
VALUES (@id, @tenant_id, @provider_account_id, @provider, @trigger_type, @status, @window_start, @window_end, 0, 0, NULL, sqlc.narg(requested_by),
        sqlc.narg(request_key), sqlc.narg(lease_until), @attempts, @started_at, NULL, @started_at);

-- name: GetReconciliationRun :one
SELECT * FROM reconciliation_runs WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetReconciliationRunForUpdate :one
SELECT * FROM reconciliation_runs WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: GetReconciliationRunByRequestKey :one
SELECT * FROM reconciliation_runs WHERE tenant_id = @tenant_id AND request_key = @request_key;

-- name: SetReconciliationRunLease :execrows
-- Assume a execução em andamento (novo lease e mais uma tentativa) ou a
-- devolve para ser retomada (lease vencido, mesma contagem de tentativas).
UPDATE reconciliation_runs
   SET lease_until = sqlc.narg(lease_until), attempts = @attempts
 WHERE tenant_id = @tenant_id AND id = @id AND status = 'running';

-- name: ClaimableReconciliationRuns :many
SELECT r.tenant_id::uuid AS tenant_id, r.run_id::uuid AS run_id FROM app_claimable_reconciliation_runs(@now, @row_limit) AS r;

-- name: ReconciliationRunStats :many
SELECT state::text AS state, total::bigint AS total, oldest_seconds::double precision AS oldest_seconds
  FROM app_reconciliation_run_stats(@now);

-- name: FinishReconciliationRun :exec
UPDATE reconciliation_runs
   SET status = @status, checked_count = @checked_count, discrepancy_count = @discrepancy_count, error = sqlc.narg(error),
       finished_at = @finished_at, lease_until = NULL
 WHERE tenant_id = @tenant_id AND id = @id;

-- name: ListReconciliationRuns :many
SELECT * FROM reconciliation_runs WHERE tenant_id = @tenant_id ORDER BY started_at DESC, id DESC LIMIT @row_limit;

-- name: TenantsDueReconciliation :many
SELECT t::uuid AS tenant_id FROM app_tenants_due_reconciliation(@now, @interval_seconds) AS t;

-- name: ListChargesForReconciliation :many
-- Cobranças locais da conta no escopo da execução: criadas ou atualizadas
-- na janela, ou presentes na listagem do provedor (pelo ID do provedor ou
-- pela referência externa, que é o ID local). As presentes na listagem vêm
-- primeiro: o limite de linhas só corta cobranças da janela ausentes da
-- listagem, e nenhuma cobrança listada pelo provedor fica sem a
-- correspondência local.
SELECT * FROM charges
 WHERE tenant_id = @tenant_id AND provider_account_id = @provider_account_id
   AND ((created_at >= @window_start AND created_at < @window_end)
        OR (updated_at >= @window_start AND updated_at < @window_end)
        OR provider_charge_id = ANY (@provider_charge_ids::text[])
        OR id = ANY (@reference_ids::uuid[]))
 ORDER BY (provider_charge_id = ANY (@provider_charge_ids::text[]) OR id = ANY (@reference_ids::uuid[])) IS TRUE DESC, created_at, id
 LIMIT @row_limit;

-- name: ListChargesForRotation :many
-- Rodízio da conciliação: cobranças da conta vinculadas ao provedor que ainda
-- podem mudar nele (aguardando pagamento, pagas, parcialmente reembolsadas ou
-- em disputa), criadas desde @since e fora da janela da execução, a começar
-- pelas nunca verificadas e pelas verificadas há mais tempo.
SELECT c.* FROM charges c
  LEFT JOIN reconciliation_charge_checks k ON k.tenant_id = c.tenant_id AND k.charge_id = c.id
 WHERE c.tenant_id = @tenant_id AND c.provider_account_id = @provider_account_id
   AND c.provider_charge_id IS NOT NULL
   AND c.status IN ('awaiting_payment', 'paid', 'partially_refunded', 'disputed')
   AND c.created_at >= @since
   AND NOT (c.created_at >= @window_start AND c.created_at < @window_end)
   AND NOT (c.updated_at >= @window_start AND c.updated_at < @window_end)
 ORDER BY k.last_checked_at NULLS FIRST, c.created_at, c.id
 LIMIT @row_limit;

-- name: MarkChargesReconciled :exec
-- Registra a verificação das cobranças comparadas numa execução concluída.
INSERT INTO reconciliation_charge_checks (tenant_id, charge_id, last_checked_at)
SELECT @tenant_id::uuid, id, @checked_at::timestamptz FROM unnest(@charge_ids::uuid[]) AS id
ON CONFLICT (tenant_id, charge_id) DO UPDATE SET last_checked_at = excluded.last_checked_at;

-- name: InsertDiscrepancy :exec
INSERT INTO reconciliation_discrepancies (id, tenant_id, run_id, last_run_id, kind, status, dedupe_key, provider, charge_id, refund_id,
                                          dispute_id, operation_id, provider_charge_id, local_amount, provider_amount, local_value,
                                          provider_value, occurrences, first_detected_at, last_detected_at, created_at, updated_at, version)
VALUES (@id, @tenant_id, @run_id, @run_id, @kind, 'open', @dedupe_key, @provider, sqlc.narg(charge_id), sqlc.narg(refund_id),
        sqlc.narg(dispute_id), sqlc.narg(operation_id), sqlc.narg(provider_charge_id), sqlc.narg(local_amount), sqlc.narg(provider_amount),
        @local_value, @provider_value, 1, @detected_at, @detected_at, @detected_at, @detected_at, 1);

-- name: GetDiscrepancy :one
SELECT * FROM reconciliation_discrepancies WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetDiscrepancyForUpdate :one
SELECT * FROM reconciliation_discrepancies WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: GetDiscrepancyByKeyForUpdate :one
SELECT * FROM reconciliation_discrepancies WHERE tenant_id = @tenant_id AND dedupe_key = @dedupe_key FOR UPDATE;

-- name: UpdateDiscrepancy :execrows
UPDATE reconciliation_discrepancies
   SET last_run_id = @last_run_id, status = @status, local_amount = sqlc.narg(local_amount), provider_amount = sqlc.narg(provider_amount),
       local_value = @local_value, provider_value = @provider_value, occurrences = @occurrences, last_detected_at = @last_detected_at,
       resolution = sqlc.narg(resolution), justification = sqlc.narg(justification), resolved_by = sqlc.narg(resolved_by),
       resolved_at = sqlc.narg(resolved_at), ledger_transaction_id = sqlc.narg(ledger_transaction_id), version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version;

-- name: ListOpenDiscrepanciesForSubjects :many
-- Divergências abertas ligadas às cobranças verificadas na execução, às
-- cobranças do provedor listadas ou a operações (reavaliadas a cada execução).
SELECT * FROM reconciliation_discrepancies
 WHERE tenant_id = @tenant_id AND status = 'open' AND provider = @provider
   AND (charge_id = ANY (@charge_ids::uuid[]) OR provider_charge_id = ANY (@provider_charge_ids::text[]) OR kind = 'unknown_operation')
 ORDER BY created_at, id
 FOR UPDATE;

-- name: ListDiscrepancies :many
SELECT * FROM reconciliation_discrepancies
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
   AND (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind))
 ORDER BY created_at DESC, id DESC
 LIMIT @row_limit;

-- name: OpenDiscrepancyStats :many
SELECT kind::text AS kind, total::bigint AS total FROM app_open_discrepancy_stats();
