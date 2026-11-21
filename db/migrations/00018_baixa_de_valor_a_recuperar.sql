-- Recebíveis: baixa do valor a recuperar do cliente registrado na disputa
-- perdida de pagamento com excedente (ADR-070 e ADR-073). Cada baixa é
-- recebida (o cliente pagou por fora do provedor) ou perdida (o tenant desiste
-- de cobrar), pode ser parcial e tem lançamento próprio no ledger. A soma das
-- baixas de uma disputa nunca passa do valor a recuperar.

-- +goose Up

-- Totais das baixas da disputa por desfecho. São mantidos somente pelo
-- gatilho da tabela de baixas, na mesma instrução que grava a baixa: o
-- UPDATE bloqueia a linha da disputa, de modo que baixas concorrentes se
-- serializam e a constraint avalia a soma já com a baixa anterior.
ALTER TABLE disputes ADD COLUMN recovery_received_amount bigint NOT NULL DEFAULT 0;
ALTER TABLE disputes ADD COLUMN recovery_written_off_amount bigint NOT NULL DEFAULT 0;
ALTER TABLE disputes ADD CONSTRAINT disputes_recovery_settled_within CHECK (
  recovery_received_amount >= 0 AND recovery_written_off_amount >= 0
  AND recovery_received_amount + recovery_written_off_amount <= recovery_amount);

-- Baixas do valor a recuperar. outcome = 'received' para o valor recebido
-- do cliente fora do provedor (transferência, acerto comercial) e
-- 'written_off' para o valor de que o tenant desistiu. effective_date é a data
-- do recebimento ou da decisão, no fuso do tenant; o lançamento no ledger usa
-- o instante do registro. settled_by é nulo quando a baixa foi feita por
-- credencial de API (autoria na auditoria, ADR-059). Registro financeiro
-- imutável: correções são feitas por nova baixa ou por lançamento
-- compensatório, nunca por alteração.
CREATE TABLE dispute_recovery_settlements (
  id                    uuid PRIMARY KEY,
  tenant_id             uuid NOT NULL,
  dispute_id            uuid NOT NULL,
  outcome               text NOT NULL CHECK (outcome IN ('received', 'written_off')),
  amount                bigint NOT NULL CHECK (amount > 0),
  currency              char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  effective_date        date NOT NULL,
  justification         text NOT NULL CHECK (length(justification) BETWEEN 10 AND 2000),
  settled_by            uuid REFERENCES users (id),
  -- Chave de negócio derivada do Idempotency-Key (ADR-022).
  request_key           text CHECK (request_key IS NULL OR length(request_key) <= 300),
  ledger_transaction_id uuid NOT NULL,
  created_at            timestamptz NOT NULL,
  CONSTRAINT dispute_recovery_settlements_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT dispute_recovery_settlements_request_key UNIQUE (tenant_id, request_key),
  CONSTRAINT dispute_recovery_settlements_ledger_key UNIQUE (tenant_id, ledger_transaction_id),
  CONSTRAINT dispute_recovery_settlements_dispute_fk FOREIGN KEY (tenant_id, dispute_id) REFERENCES disputes (tenant_id, id),
  CONSTRAINT dispute_recovery_settlements_ledger_fk FOREIGN KEY (tenant_id, ledger_transaction_id)
    REFERENCES ledger_transactions (tenant_id, id)
);
CREATE INDEX dispute_recovery_settlements_dispute_idx ON dispute_recovery_settlements (tenant_id, dispute_id, created_at, id);
CREATE TRIGGER dispute_recovery_settlements_immutable BEFORE UPDATE OR DELETE ON dispute_recovery_settlements
  FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('dispute_recovery_settlements');
GRANT SELECT, INSERT ON dispute_recovery_settlements TO recorvia_app;

-- +goose StatementBegin
-- Acumula a baixa gravada no total da disputa. A constraint
-- disputes_recovery_settled_within recusa a baixa que passaria do valor a
-- recuperar (também quando outra baixa da mesma disputa foi confirmada
-- enquanto esta aguardava o bloqueio da linha).
CREATE FUNCTION app_dispute_recovery_settled() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  UPDATE disputes
     SET recovery_received_amount = recovery_received_amount + CASE WHEN NEW.outcome = 'received' THEN NEW.amount ELSE 0 END,
         recovery_written_off_amount = recovery_written_off_amount + CASE WHEN NEW.outcome = 'written_off' THEN NEW.amount ELSE 0 END
   WHERE tenant_id = NEW.tenant_id AND id = NEW.dispute_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'disputa % não encontrada para a baixa do valor a recuperar', NEW.dispute_id USING ERRCODE = 'P0001';
  END IF;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER dispute_recovery_settlements_apply AFTER INSERT ON dispute_recovery_settlements
  FOR EACH ROW EXECUTE FUNCTION app_dispute_recovery_settled();

-- +goose StatementBegin
-- Os totais das baixas só mudam pelo gatilho acima (profundidade de gatilho
-- maior que 1): uma alteração direta deixaria a disputa diferente da soma das
-- baixas registradas. A disputa nasce sem baixas.
CREATE FUNCTION app_disputes_recovery_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.recovery_received_amount <> 0 OR NEW.recovery_written_off_amount <> 0 THEN
      RAISE EXCEPTION 'disputa criada com baixa do valor a recuperar' USING ERRCODE = 'P0001';
    END IF;
  ELSIF (NEW.recovery_received_amount, NEW.recovery_written_off_amount) IS DISTINCT FROM
        (OLD.recovery_received_amount, OLD.recovery_written_off_amount) AND pg_trigger_depth() < 2 THEN
    RAISE EXCEPTION 'os totais das baixas do valor a recuperar só mudam pelo registro da baixa' USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER disputes_recovery_guard BEFORE INSERT OR UPDATE ON disputes
  FOR EACH ROW EXECUTE FUNCTION app_disputes_recovery_guard();

-- +goose Down
-- Os lançamentos das baixas continuam no ledger, que é imutável: o saldo de
-- customer_recoverables continua igual ao valor a recuperar menos as baixas,
-- mas sem o registro que as explica (justificativa, desfecho e data efetiva).
-- Reverter com baixas registradas exige guardar antes o conteúdo da tabela.
DROP TRIGGER IF EXISTS disputes_recovery_guard ON disputes;
DROP FUNCTION IF EXISTS app_disputes_recovery_guard();
DROP TABLE IF EXISTS dispute_recovery_settlements;
DROP FUNCTION IF EXISTS app_dispute_recovery_settled();
ALTER TABLE disputes DROP CONSTRAINT IF EXISTS disputes_recovery_settled_within;
ALTER TABLE disputes DROP COLUMN IF EXISTS recovery_written_off_amount;
ALTER TABLE disputes DROP COLUMN IF EXISTS recovery_received_amount;
