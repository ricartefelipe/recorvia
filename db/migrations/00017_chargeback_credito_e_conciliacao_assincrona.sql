-- Recebíveis: chargeback que consome o crédito do cliente gerado por
-- pagamento excedente (ADR-070) e conciliação executada pelo worker a partir
-- da execução registrada, com lease e retomada (ADR-071). A resposta
-- registrada pela Idempotency-Key passa a guardar o header Location.

-- +goose Up

-- Parte do chargeback da disputa perdida que corresponde ao excedente do
-- pagamento convertido em crédito do cliente. credit_amount é o crédito
-- consumido do saldo disponível; recovery_amount é a parte que o cliente já
-- havia usado e que passa a ser valor a recuperar (o saldo de crédito nunca
-- fica negativo). As duas somam, no máximo, o valor contestado.
ALTER TABLE disputes ADD COLUMN credit_amount bigint NOT NULL DEFAULT 0;
ALTER TABLE disputes ADD COLUMN recovery_amount bigint NOT NULL DEFAULT 0;
ALTER TABLE disputes ADD CONSTRAINT disputes_credit_nonnegative CHECK (credit_amount >= 0 AND recovery_amount >= 0);
ALTER TABLE disputes ADD CONSTRAINT disputes_credit_within_amount CHECK (credit_amount + recovery_amount <= amount);
ALTER TABLE disputes ADD CONSTRAINT disputes_credit_only_lost
  CHECK (status = 'lost' OR (credit_amount = 0 AND recovery_amount = 0));

-- Número de vezes em que um worker assumiu a execução. A execução manual é
-- registrada sem lease (aguardando o worker) e assumida com lease; a
-- execução com lease expirado (worker interrompido) é retomada até o limite
-- de tentativas. As execuções anteriores a esta migration foram assumidas uma
-- vez, dentro da requisição ou da tarefa agendada.
ALTER TABLE reconciliation_runs ADD COLUMN attempts integer NOT NULL DEFAULT 0;
ALTER TABLE reconciliation_runs ADD CONSTRAINT reconciliation_runs_attempts_nonnegative CHECK (attempts >= 0);
UPDATE reconciliation_runs SET attempts = 1;

-- Header Location da resposta registrada pela Idempotency-Key. O pedido de
-- conciliação responde com o endereço da execução; a repetição do resultado
-- registrado reproduz o endereço junto de status e corpo. Nulo nas respostas
-- sem o header e nos registros anteriores a esta migration.
ALTER TABLE idempotency_keys ADD COLUMN response_location text;
ALTER TABLE idempotency_keys ADD CONSTRAINT idempotency_keys_response_location_length
  CHECK (response_location IS NULL OR length(response_location) BETWEEN 1 AND 2048);

-- +goose StatementBegin
-- Execuções de conciliação que um worker pode assumir: em andamento, sem
-- lease (aguardando) ou com o lease expirado (interrompidas), das mais
-- antigas para as mais novas. Retorna apenas identificadores.
CREATE FUNCTION app_claimable_reconciliation_runs(p_now timestamptz, p_limit integer)
RETURNS TABLE (tenant_id uuid, run_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT r.tenant_id, r.id
    FROM reconciliation_runs r
   WHERE r.status = 'running' AND (r.lease_until IS NULL OR r.lease_until < p_now)
   ORDER BY r.started_at, r.id
   LIMIT greatest(p_limit, 0)
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_claimable_reconciliation_runs(timestamptz, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_claimable_reconciliation_runs(timestamptz, integer) TO recorvia_app;

-- +goose StatementBegin
-- Execuções de conciliação em andamento por estado (métrica sem rótulo de
-- tenant): queued aguarda um worker, executing tem lease vigente e stalled
-- teve o lease expirado. oldest_seconds é a espera mais longa do estado:
-- desde o registro (queued e executing) ou desde o fim do lease (stalled).
CREATE FUNCTION app_reconciliation_run_stats(p_now timestamptz)
RETURNS TABLE (state text, total bigint, oldest_seconds double precision)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT s.state, count(*), COALESCE(max(s.waiting), 0)
    FROM (SELECT CASE WHEN r.lease_until IS NULL THEN 'queued' WHEN r.lease_until < p_now THEN 'stalled' ELSE 'executing' END AS state,
                 greatest(extract(epoch FROM p_now - CASE WHEN r.lease_until < p_now THEN r.lease_until ELSE r.started_at END), 0)::double precision AS waiting
            FROM reconciliation_runs r
           WHERE r.status = 'running') s
   GROUP BY s.state
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_reconciliation_run_stats(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_reconciliation_run_stats(timestamptz) TO recorvia_app;

-- +goose Down
DROP FUNCTION IF EXISTS app_reconciliation_run_stats(timestamptz);
DROP FUNCTION IF EXISTS app_claimable_reconciliation_runs(timestamptz, integer);
-- As respostas registradas continuam válidas sem o endereço: a repetição
-- volta a sair sem o header Location.
ALTER TABLE idempotency_keys DROP CONSTRAINT IF EXISTS idempotency_keys_response_location_length;
ALTER TABLE idempotency_keys DROP COLUMN IF EXISTS response_location;
ALTER TABLE reconciliation_runs DROP CONSTRAINT IF EXISTS reconciliation_runs_attempts_nonnegative;
ALTER TABLE reconciliation_runs DROP COLUMN IF EXISTS attempts;
-- O esquema anterior encerra, no início da execução seguinte, as execuções
-- com lease expirado; as que aguardavam um worker (sem lease) nunca seriam
-- executadas nem encerradas e bloqueariam novas execuções da conta.
UPDATE reconciliation_runs
   SET status = 'failed', finished_at = now(), error = 'Execução encerrada na reversão do esquema antes de ser assumida por um worker.'
 WHERE status = 'running' AND lease_until IS NULL;
ALTER TABLE disputes DROP CONSTRAINT IF EXISTS disputes_credit_only_lost;
ALTER TABLE disputes DROP CONSTRAINT IF EXISTS disputes_credit_within_amount;
ALTER TABLE disputes DROP CONSTRAINT IF EXISTS disputes_credit_nonnegative;
ALTER TABLE disputes DROP COLUMN IF EXISTS recovery_amount;
ALTER TABLE disputes DROP COLUMN IF EXISTS credit_amount;
