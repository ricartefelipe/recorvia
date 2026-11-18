-- Recebíveis: reembolsos, disputas e chargebacks, cancelamento da cobrança no
-- provedor e conciliação com o provedor (execuções e divergências).

-- +goose Up

-- Reservas de reembolso ainda não confirmadas pelo provedor. O pedido de
-- reembolso reserva o valor na mesma transação que bloqueia a cobrança; a
-- confirmação move a reserva para amount_refunded. Assim, pedidos
-- concorrentes nunca somam mais que o valor pago.
ALTER TABLE charges ADD COLUMN amount_refund_pending bigint NOT NULL DEFAULT 0;
ALTER TABLE charges ADD CONSTRAINT charges_refund_pending_nonnegative CHECK (amount_refund_pending >= 0);
ALTER TABLE charges ADD CONSTRAINT charges_refund_reservation_limit CHECK (amount_refunded + amount_refund_pending <= amount_paid);

-- Reembolsos de cobranças pagas. origin = 'recorvia' para pedidos feitos pelo
-- Recorvia (com operação externa persistida) e 'provider' para reembolsos
-- feitos diretamente no provedor e conhecidos por webhook ou consulta.
-- credit_amount é a parte do reembolso que consumiu crédito do cliente gerado
-- pelo excedente do próprio pagamento (o restante reduz a receita).
CREATE TABLE refunds (
  id                 uuid PRIMARY KEY,
  tenant_id          uuid NOT NULL,
  charge_id          uuid NOT NULL,
  provider           text NOT NULL CHECK (provider IN ('simulator', 'asaas')),
  origin             text NOT NULL CHECK (origin IN ('recorvia', 'provider')),
  status             text NOT NULL CHECK (status IN ('requested', 'succeeded', 'failed', 'unknown')),
  amount             bigint NOT NULL CHECK (amount > 0),
  credit_amount      bigint NOT NULL DEFAULT 0 CHECK (credit_amount >= 0),
  currency           char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  reason             text NOT NULL CHECK (length(reason) BETWEEN 3 AND 500),
  provider_refund_id text CHECK (provider_refund_id IS NULL OR length(provider_refund_id) BETWEEN 1 AND 255),
  failure_reason     text CHECK (failure_reason IS NULL OR length(failure_reason) <= 500),
  requested_by       uuid REFERENCES users (id),
  -- Chave de negócio derivada do Idempotency-Key (ADR-022).
  request_key        text CHECK (request_key IS NULL OR length(request_key) <= 300),
  created_at         timestamptz NOT NULL,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  completed_at       timestamptz,
  version            integer NOT NULL DEFAULT 1,
  CONSTRAINT refunds_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT refunds_request_key UNIQUE (tenant_id, request_key),
  CONSTRAINT refunds_provider_refund_key UNIQUE (tenant_id, charge_id, provider_refund_id),
  CONSTRAINT refunds_charge_fk FOREIGN KEY (tenant_id, charge_id) REFERENCES charges (tenant_id, id),
  CONSTRAINT refunds_credit_within_amount CHECK (credit_amount <= amount),
  CONSTRAINT refunds_completion CHECK ((status IN ('succeeded', 'failed')) = (completed_at IS NOT NULL)),
  CONSTRAINT refunds_origin_request CHECK (origin = 'recorvia' OR (request_key IS NULL AND requested_by IS NULL))
);
CREATE INDEX refunds_charge_idx ON refunds (tenant_id, charge_id, created_at, id);
CREATE INDEX refunds_tenant_created_idx ON refunds (tenant_id, created_at DESC, id DESC);
CREATE TRIGGER refunds_touch BEFORE UPDATE ON refunds FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('refunds');
GRANT SELECT, INSERT, UPDATE ON refunds TO recorvia_app;

-- Operações externas: reembolso vinculado ao registro de reembolso e
-- cancelamento da cobrança no provedor (uma operação por cobrança).
ALTER TABLE payment_operations DROP CONSTRAINT payment_operations_kind_check;
ALTER TABLE payment_operations ADD CONSTRAINT payment_operations_kind_check
  CHECK (kind IN ('create_charge', 'refund', 'cancel_charge'));
ALTER TABLE payment_operations ADD CONSTRAINT payment_operations_refund_link CHECK ((kind = 'refund') = (refund_id IS NOT NULL));
ALTER TABLE payment_operations ADD CONSTRAINT payment_operations_refund_fk
  FOREIGN KEY (tenant_id, refund_id) REFERENCES refunds (tenant_id, id);
CREATE UNIQUE INDEX payment_operations_cancel_key ON payment_operations (tenant_id, charge_id) WHERE kind = 'cancel_charge';
CREATE INDEX payment_operations_charge_idx ON payment_operations (tenant_id, charge_id, kind);
CREATE INDEX payment_operations_unknown_idx ON payment_operations (tenant_id) WHERE status = 'unknown';

-- Disputas e chargebacks informados pelo provedor. Há no máximo uma disputa
-- aberta por cobrança.
CREATE TABLE disputes (
  id              uuid PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  charge_id       uuid NOT NULL,
  provider        text NOT NULL CHECK (provider IN ('simulator', 'asaas')),
  status          text NOT NULL CHECK (status IN ('opened', 'under_review', 'won', 'lost')),
  amount          bigint NOT NULL CHECK (amount > 0),
  currency        char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  provider_status text CHECK (provider_status IS NULL OR length(provider_status) <= 100),
  opened_at       timestamptz NOT NULL,
  closed_at       timestamptz,
  created_at      timestamptz NOT NULL,
  updated_at      timestamptz NOT NULL DEFAULT now(),
  version         integer NOT NULL DEFAULT 1,
  CONSTRAINT disputes_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT disputes_charge_fk FOREIGN KEY (tenant_id, charge_id) REFERENCES charges (tenant_id, id),
  CONSTRAINT disputes_closed CHECK ((status IN ('won', 'lost')) = (closed_at IS NOT NULL)),
  CONSTRAINT disputes_closed_after_open CHECK (closed_at IS NULL OR closed_at >= opened_at)
);
CREATE UNIQUE INDEX disputes_open_key ON disputes (tenant_id, charge_id) WHERE status IN ('opened', 'under_review');
CREATE INDEX disputes_charge_idx ON disputes (tenant_id, charge_id, opened_at DESC, id DESC);
CREATE INDEX disputes_tenant_opened_idx ON disputes (tenant_id, opened_at DESC, id DESC);
CREATE TRIGGER disputes_touch BEFORE UPDATE ON disputes FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('disputes');
GRANT SELECT, INSERT, UPDATE ON disputes TO recorvia_app;

-- Histórico imutável das transições de disputa.
CREATE TABLE dispute_events (
  id              uuid PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  dispute_id      uuid NOT NULL,
  from_status     text CHECK (from_status IS NULL OR from_status IN ('opened', 'under_review', 'won', 'lost')),
  to_status       text NOT NULL CHECK (to_status IN ('opened', 'under_review', 'won', 'lost')),
  provider_status text CHECK (provider_status IS NULL OR length(provider_status) <= 100),
  occurred_at     timestamptz NOT NULL,
  recorded_at     timestamptz NOT NULL,
  details         jsonb NOT NULL DEFAULT '{}',
  CONSTRAINT dispute_events_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT dispute_events_dispute_fk FOREIGN KEY (tenant_id, dispute_id) REFERENCES disputes (tenant_id, id)
);
CREATE INDEX dispute_events_dispute_idx ON dispute_events (tenant_id, dispute_id, recorded_at, id);
CREATE TRIGGER dispute_events_immutable BEFORE UPDATE OR DELETE ON dispute_events FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('dispute_events');
GRANT SELECT, INSERT ON dispute_events TO recorvia_app;

-- Execuções de conciliação. Há no máximo uma execução em andamento por conta
-- do provedor; lease_until permite encerrar execuções interrompidas.
CREATE TABLE reconciliation_runs (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  provider_account_id uuid NOT NULL,
  provider            text NOT NULL CHECK (provider IN ('simulator', 'asaas')),
  trigger_type        text NOT NULL CHECK (trigger_type IN ('manual', 'scheduled')),
  status              text NOT NULL CHECK (status IN ('running', 'completed', 'failed')),
  window_start        timestamptz NOT NULL,
  window_end          timestamptz NOT NULL,
  checked_count       integer NOT NULL DEFAULT 0 CHECK (checked_count >= 0),
  discrepancy_count   integer NOT NULL DEFAULT 0 CHECK (discrepancy_count >= 0),
  error               text CHECK (error IS NULL OR length(error) <= 1000),
  requested_by        uuid REFERENCES users (id),
  request_key         text CHECK (request_key IS NULL OR length(request_key) <= 300),
  lease_until         timestamptz,
  started_at          timestamptz NOT NULL,
  finished_at         timestamptz,
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT reconciliation_runs_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT reconciliation_runs_request_key UNIQUE (tenant_id, request_key),
  CONSTRAINT reconciliation_runs_account_fk FOREIGN KEY (tenant_id, provider_account_id) REFERENCES payment_provider_accounts (tenant_id, id),
  CONSTRAINT reconciliation_runs_window CHECK (window_start < window_end),
  CONSTRAINT reconciliation_runs_finished CHECK ((status = 'running') = (finished_at IS NULL))
);
CREATE UNIQUE INDEX reconciliation_runs_running_key ON reconciliation_runs (tenant_id, provider_account_id) WHERE status = 'running';
CREATE INDEX reconciliation_runs_tenant_started_idx ON reconciliation_runs (tenant_id, started_at DESC, id DESC);
CREATE TRIGGER reconciliation_runs_touch BEFORE UPDATE ON reconciliation_runs FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('reconciliation_runs');
GRANT SELECT, INSERT, UPDATE ON reconciliation_runs TO recorvia_app;

-- Divergências encontradas pela conciliação. dedupe_key identifica a mesma
-- divergência (tipo, objeto e valores) entre execuções: uma divergência
-- repetida atualiza o registro existente em vez de criar outro. Valores
-- monetários usados na resolução ficam em colunas inteiras; local_value e
-- provider_value são os retratos exibidos.
CREATE TABLE reconciliation_discrepancies (
  id                    uuid PRIMARY KEY,
  tenant_id             uuid NOT NULL,
  run_id                uuid NOT NULL,
  last_run_id           uuid NOT NULL,
  kind                  text NOT NULL CHECK (kind IN ('missing_local', 'missing_provider', 'amount_mismatch', 'fee_mismatch',
                                                      'payment_not_reflected', 'refund_pending', 'chargeback_pending', 'unknown_operation')),
  status                text NOT NULL CHECK (status IN ('open', 'resolved', 'dismissed')),
  dedupe_key            text NOT NULL CHECK (length(dedupe_key) BETWEEN 1 AND 300),
  provider              text NOT NULL CHECK (provider IN ('simulator', 'asaas')),
  charge_id             uuid,
  refund_id             uuid,
  dispute_id            uuid,
  operation_id          uuid,
  provider_charge_id    text CHECK (provider_charge_id IS NULL OR length(provider_charge_id) BETWEEN 1 AND 255),
  local_amount          bigint,
  provider_amount       bigint,
  local_value           jsonb NOT NULL DEFAULT '{}',
  provider_value        jsonb NOT NULL DEFAULT '{}',
  occurrences           integer NOT NULL DEFAULT 1 CHECK (occurrences > 0),
  first_detected_at     timestamptz NOT NULL,
  last_detected_at      timestamptz NOT NULL,
  resolution            text CHECK (resolution IS NULL OR resolution IN ('accept_provider', 'keep_local', 'adjust_ledger', 'dismiss', 'auto_resolved')),
  justification         text CHECK (justification IS NULL OR length(justification) BETWEEN 10 AND 2000),
  resolved_by           uuid REFERENCES users (id),
  resolved_at           timestamptz,
  ledger_transaction_id uuid,
  created_at            timestamptz NOT NULL,
  updated_at            timestamptz NOT NULL DEFAULT now(),
  version               integer NOT NULL DEFAULT 1,
  CONSTRAINT reconciliation_discrepancies_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT reconciliation_discrepancies_dedupe_key UNIQUE (tenant_id, dedupe_key),
  CONSTRAINT reconciliation_discrepancies_run_fk FOREIGN KEY (tenant_id, run_id) REFERENCES reconciliation_runs (tenant_id, id),
  CONSTRAINT reconciliation_discrepancies_last_run_fk FOREIGN KEY (tenant_id, last_run_id) REFERENCES reconciliation_runs (tenant_id, id),
  CONSTRAINT reconciliation_discrepancies_charge_fk FOREIGN KEY (tenant_id, charge_id) REFERENCES charges (tenant_id, id),
  CONSTRAINT reconciliation_discrepancies_refund_fk FOREIGN KEY (tenant_id, refund_id) REFERENCES refunds (tenant_id, id),
  CONSTRAINT reconciliation_discrepancies_dispute_fk FOREIGN KEY (tenant_id, dispute_id) REFERENCES disputes (tenant_id, id),
  CONSTRAINT reconciliation_discrepancies_operation_fk FOREIGN KEY (tenant_id, operation_id) REFERENCES payment_operations (tenant_id, id),
  CONSTRAINT reconciliation_discrepancies_ledger_fk FOREIGN KEY (tenant_id, ledger_transaction_id) REFERENCES ledger_transactions (tenant_id, id),
  CONSTRAINT reconciliation_discrepancies_resolution CHECK (
    (status = 'open') = (resolution IS NULL) AND (status = 'open') = (resolved_at IS NULL) AND (status = 'open') = (justification IS NULL)),
  CONSTRAINT reconciliation_discrepancies_dismissed CHECK ((status = 'dismissed') = (resolution IS NOT DISTINCT FROM 'dismiss')),
  CONSTRAINT reconciliation_discrepancies_detection CHECK (last_detected_at >= first_detected_at)
);
CREATE INDEX reconciliation_discrepancies_list_idx ON reconciliation_discrepancies (tenant_id, status, kind, created_at DESC, id DESC);
CREATE INDEX reconciliation_discrepancies_charge_idx ON reconciliation_discrepancies (tenant_id, charge_id) WHERE charge_id IS NOT NULL;
CREATE INDEX reconciliation_discrepancies_open_idx ON reconciliation_discrepancies (tenant_id, kind) WHERE status = 'open';
CREATE TRIGGER reconciliation_discrepancies_touch BEFORE UPDATE ON reconciliation_discrepancies FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('reconciliation_discrepancies');
GRANT SELECT, INSERT, UPDATE ON reconciliation_discrepancies TO recorvia_app;

-- +goose StatementBegin
-- Tenants elegíveis à conciliação agendada: plano da plataforma com o recurso
-- reconciliation_automation, provedor padrão ativo e nenhuma execução
-- agendada bem-sucedida ou em andamento no intervalo. Retorna apenas IDs.
CREATE FUNCTION app_tenants_due_reconciliation(p_now timestamptz, p_interval_seconds integer)
RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT s.tenant_id
    FROM tenant_platform_subscriptions s
    JOIN platform_plans p ON p.code = s.plan_code
    JOIN tenants t ON t.id = s.tenant_id
   WHERE 'reconciliation_automation' = ANY (p.features)
     AND s.status IN ('trialing', 'active', 'past_due')
     AND t.status = 'active'
     AND EXISTS (SELECT 1 FROM payment_provider_accounts a
                  WHERE a.tenant_id = s.tenant_id AND a.is_default AND a.status = 'active')
     AND NOT EXISTS (SELECT 1 FROM reconciliation_runs r
                      WHERE r.tenant_id = s.tenant_id AND r.trigger_type = 'scheduled' AND r.status <> 'failed'
                        AND r.started_at > p_now - make_interval(secs => p_interval_seconds))
   ORDER BY s.tenant_id
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_tenants_due_reconciliation(timestamptz, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_tenants_due_reconciliation(timestamptz, integer) TO recorvia_app;

-- +goose StatementBegin
-- Contagem agregada de divergências abertas por tipo (métrica sem rótulo de tenant).
CREATE FUNCTION app_open_discrepancy_stats()
RETURNS TABLE (kind text, total bigint)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$ SELECT kind, count(*) FROM reconciliation_discrepancies WHERE status = 'open' GROUP BY kind $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_open_discrepancy_stats() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_open_discrepancy_stats() TO recorvia_app;

-- +goose StatementBegin
-- Contagem agregada de reembolsos por situação (métrica sem rótulo de tenant).
CREATE FUNCTION app_refund_stats()
RETURNS TABLE (status text, total bigint)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$ SELECT status, count(*) FROM refunds GROUP BY status $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_refund_stats() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_refund_stats() TO recorvia_app;

-- +goose Down
DROP FUNCTION IF EXISTS app_refund_stats();
DROP FUNCTION IF EXISTS app_open_discrepancy_stats();
DROP FUNCTION IF EXISTS app_tenants_due_reconciliation(timestamptz, integer);
DROP TABLE IF EXISTS reconciliation_discrepancies;
DROP TABLE IF EXISTS reconciliation_runs;
DROP TABLE IF EXISTS dispute_events;
DROP TABLE IF EXISTS disputes;
DROP INDEX IF EXISTS payment_operations_unknown_idx;
DROP INDEX IF EXISTS payment_operations_charge_idx;
DROP INDEX IF EXISTS payment_operations_cancel_key;
ALTER TABLE payment_operations DROP CONSTRAINT IF EXISTS payment_operations_refund_fk;
ALTER TABLE payment_operations DROP CONSTRAINT IF EXISTS payment_operations_refund_link;
-- Operações sem correspondência no esquema anterior (reembolsos vinculados à
-- tabela removida e cancelamentos no provedor) são descartadas na reversão.
DELETE FROM payment_operations WHERE kind IN ('refund', 'cancel_charge');
ALTER TABLE payment_operations DROP CONSTRAINT IF EXISTS payment_operations_kind_check;
ALTER TABLE payment_operations ADD CONSTRAINT payment_operations_kind_check CHECK (kind IN ('create_charge', 'refund'));
DROP TABLE IF EXISTS refunds;
ALTER TABLE charges DROP CONSTRAINT IF EXISTS charges_refund_reservation_limit;
ALTER TABLE charges DROP CONSTRAINT IF EXISTS charges_refund_pending_nonnegative;
ALTER TABLE charges DROP COLUMN IF EXISTS amount_refund_pending;
