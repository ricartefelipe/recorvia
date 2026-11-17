-- Integração com provedores de pagamento, cobranças, operações externas,
-- webhooks recebidos e ledger operacional de partidas dobradas.

-- +goose Up

CREATE TABLE payment_provider_accounts (
  id                         uuid PRIMARY KEY,
  tenant_id                  uuid NOT NULL REFERENCES tenants (id),
  provider                   text NOT NULL CHECK (provider IN ('simulator', 'asaas')),
  environment                text NOT NULL CHECK (environment IN ('local', 'sandbox', 'production')),
  status                     text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
  is_default                 boolean NOT NULL DEFAULT false,
  -- Credenciais cifradas com AES-256-GCM (chave fora do banco).
  api_key_ciphertext         bytea,
  api_key_key_id             text,
  api_key_hint               text CHECK (api_key_hint IS NULL OR length(api_key_hint) <= 8),
  webhook_secret_ciphertext  bytea,
  webhook_secret_key_id      text,
  -- Token opaco do endpoint de recepção (somente o hash é guardado).
  webhook_endpoint_token_hash bytea NOT NULL,
  created_at                 timestamptz NOT NULL DEFAULT now(),
  updated_at                 timestamptz NOT NULL DEFAULT now(),
  version                    integer NOT NULL DEFAULT 1,
  CONSTRAINT payment_provider_accounts_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT payment_provider_accounts_provider_key UNIQUE (tenant_id, provider),
  CONSTRAINT payment_provider_accounts_endpoint_key UNIQUE (webhook_endpoint_token_hash)
);
CREATE UNIQUE INDEX payment_provider_accounts_default_key ON payment_provider_accounts (tenant_id) WHERE is_default;
CREATE TRIGGER payment_provider_accounts_touch BEFORE UPDATE ON payment_provider_accounts FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('payment_provider_accounts');
GRANT SELECT, INSERT, UPDATE ON payment_provider_accounts TO recorvia_app;

-- +goose StatementBegin
-- Vincula o webhook recebido ao tenant pelo hash do token do endpoint.
CREATE FUNCTION app_resolve_webhook_endpoint(p_provider text, p_token_hash bytea)
RETURNS TABLE (tenant_id uuid, account_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT tenant_id, id FROM payment_provider_accounts
   WHERE provider = p_provider AND webhook_endpoint_token_hash = p_token_hash AND status = 'active'
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_resolve_webhook_endpoint(text, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_resolve_webhook_endpoint(text, bytea) TO recorvia_app;

CREATE TABLE charges (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  invoice_id          uuid NOT NULL,
  customer_id         uuid NOT NULL,
  provider_account_id uuid NOT NULL,
  provider            text NOT NULL CHECK (provider IN ('simulator', 'asaas')),
  attempt             integer NOT NULL DEFAULT 1 CHECK (attempt > 0),
  provider_charge_id  text,
  status              text NOT NULL CHECK (status IN ('pending', 'awaiting_payment', 'paid', 'failed', 'canceled',
                                                       'partially_refunded', 'refunded', 'disputed', 'charged_back')),
  amount              bigint NOT NULL CHECK (amount > 0),
  currency            char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  amount_paid         bigint NOT NULL DEFAULT 0 CHECK (amount_paid >= 0),
  fee_amount          bigint NOT NULL DEFAULT 0 CHECK (fee_amount >= 0),
  amount_refunded     bigint NOT NULL DEFAULT 0 CHECK (amount_refunded >= 0),
  amount_charged_back bigint NOT NULL DEFAULT 0 CHECK (amount_charged_back >= 0),
  checkout_url        text,
  due_date            date NOT NULL,
  paid_at             timestamptz,
  failure_reason      text,
  provider_status     text,
  provider_status_at  timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  version             integer NOT NULL DEFAULT 1,
  CONSTRAINT charges_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT charges_invoice_attempt_key UNIQUE (tenant_id, invoice_id, attempt),
  CONSTRAINT charges_provider_charge_key UNIQUE (tenant_id, provider, provider_charge_id),
  CONSTRAINT charges_invoice_fk FOREIGN KEY (tenant_id, invoice_id) REFERENCES invoices (tenant_id, id),
  CONSTRAINT charges_customer_fk FOREIGN KEY (tenant_id, customer_id) REFERENCES customers (tenant_id, id),
  CONSTRAINT charges_account_fk FOREIGN KEY (tenant_id, provider_account_id) REFERENCES payment_provider_accounts (tenant_id, id),
  CONSTRAINT charges_refund_limit CHECK (amount_refunded + amount_charged_back <= amount_paid)
);
CREATE INDEX charges_tenant_created_idx ON charges (tenant_id, created_at DESC, id DESC);
CREATE TRIGGER charges_touch BEFORE UPDATE ON charges FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('charges');
GRANT SELECT, INSERT, UPDATE ON charges TO recorvia_app;

-- Operações externas persistidas: permitem recuperação após falha e o estado
-- "unknown" quando não há confirmação segura do resultado no provedor.
CREATE TABLE payment_operations (
  id                 uuid PRIMARY KEY,
  tenant_id          uuid NOT NULL,
  charge_id          uuid NOT NULL,
  refund_id          uuid,
  kind               text NOT NULL CHECK (kind IN ('create_charge', 'refund')),
  status             text NOT NULL CHECK (status IN ('pending', 'in_flight', 'succeeded', 'failed', 'unknown')),
  external_reference text NOT NULL,
  attempts           integer NOT NULL DEFAULT 0,
  lease_until        timestamptz,
  next_attempt_at    timestamptz NOT NULL DEFAULT now(),
  last_error         text,
  provider_response  jsonb,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  completed_at       timestamptz,
  CONSTRAINT payment_operations_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT payment_operations_charge_fk FOREIGN KEY (tenant_id, charge_id) REFERENCES charges (tenant_id, id)
);
CREATE UNIQUE INDEX payment_operations_create_key ON payment_operations (tenant_id, charge_id) WHERE kind = 'create_charge';
CREATE UNIQUE INDEX payment_operations_refund_key ON payment_operations (tenant_id, refund_id) WHERE kind = 'refund';
CREATE INDEX payment_operations_pending_idx ON payment_operations (next_attempt_at) WHERE status IN ('pending', 'in_flight', 'unknown');
CREATE TRIGGER payment_operations_touch BEFORE UPDATE ON payment_operations FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('payment_operations');
GRANT SELECT, INSERT, UPDATE ON payment_operations TO recorvia_app;

-- +goose StatementBegin
CREATE FUNCTION app_tenants_with_pending_payment_operations(p_now timestamptz)
RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT DISTINCT tenant_id FROM payment_operations
   WHERE next_attempt_at <= p_now
     AND (status IN ('pending', 'unknown') OR (status = 'in_flight' AND lease_until < p_now))
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_tenants_with_pending_payment_operations(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_tenants_with_pending_payment_operations(timestamptz) TO recorvia_app;

-- +goose StatementBegin
-- Contagem agregada de operações por situação (métrica sem rótulo de tenant).
CREATE FUNCTION app_payment_operation_stats()
RETURNS TABLE (status text, total bigint)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$ SELECT status, count(*) FROM payment_operations WHERE status IN ('pending', 'in_flight', 'unknown') GROUP BY status $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_payment_operation_stats() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_payment_operation_stats() TO recorvia_app;

-- Recebimentos aplicados a faturas (do provedor ou registrados manualmente).
CREATE TABLE payments (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL,
  invoice_id  uuid NOT NULL,
  charge_id   uuid,
  customer_id uuid NOT NULL,
  amount      bigint NOT NULL CHECK (amount > 0),
  fee_amount  bigint NOT NULL DEFAULT 0 CHECK (fee_amount >= 0),
  currency    char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  method      text NOT NULL DEFAULT 'unknown',
  source      text NOT NULL CHECK (source IN ('provider', 'manual')),
  paid_at     timestamptz NOT NULL,
  note        text NOT NULL DEFAULT '' CHECK (length(note) <= 500),
  recorded_by uuid REFERENCES users (id),
  -- Chave de negócio de registros manuais (derivada do Idempotency-Key):
  -- protege contra duplicidade mesmo após a expiração da chave HTTP.
  request_key text CHECK (request_key IS NULL OR length(request_key) <= 300),
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT payments_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT payments_charge_key UNIQUE (tenant_id, charge_id),
  CONSTRAINT payments_request_key UNIQUE (tenant_id, request_key),
  CONSTRAINT payments_invoice_fk FOREIGN KEY (tenant_id, invoice_id) REFERENCES invoices (tenant_id, id),
  CONSTRAINT payments_charge_fk FOREIGN KEY (tenant_id, charge_id) REFERENCES charges (tenant_id, id),
  CONSTRAINT payments_source_charge CHECK ((source = 'provider') = (charge_id IS NOT NULL))
);
CREATE INDEX payments_tenant_paid_idx ON payments (tenant_id, paid_at DESC, id DESC);
CREATE TRIGGER payments_immutable BEFORE UPDATE OR DELETE ON payments FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('payments');
GRANT SELECT, INSERT ON payments TO recorvia_app;

-- Webhooks recebidos: persistidos antes do 200 e processados de forma
-- assíncrona. A unicidade por evento do provedor deduplica entregas repetidas.
CREATE TABLE provider_webhook_events (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  provider_account_id uuid NOT NULL,
  provider            text NOT NULL,
  provider_event_id   text NOT NULL CHECK (length(provider_event_id) BETWEEN 1 AND 255),
  event_type          text NOT NULL,
  provider_charge_id  text,
  occurred_at         timestamptz,
  received_at         timestamptz NOT NULL,
  payload             jsonb NOT NULL,
  status              text NOT NULL DEFAULT 'received' CHECK (status IN ('received', 'processed', 'ignored', 'failed')),
  processed_at        timestamptz,
  error               text,
  CONSTRAINT provider_webhook_events_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT provider_webhook_events_dedupe_key UNIQUE (tenant_id, provider, provider_event_id),
  CONSTRAINT provider_webhook_events_account_fk FOREIGN KEY (tenant_id, provider_account_id) REFERENCES payment_provider_accounts (tenant_id, id)
);
CREATE INDEX provider_webhook_events_charge_idx ON provider_webhook_events (tenant_id, provider, provider_charge_id, received_at);
SELECT app_enable_tenant_rls('provider_webhook_events');
GRANT SELECT, INSERT, UPDATE ON provider_webhook_events TO recorvia_app;

-- Ledger operacional imutável. Não substitui contabilidade fiscal.
CREATE TABLE ledger_accounts (
  id         uuid PRIMARY KEY,
  tenant_id  uuid NOT NULL REFERENCES tenants (id),
  code       text NOT NULL CHECK (code ~ '^[a-z_]{2,64}$'),
  name       text NOT NULL,
  kind       text NOT NULL CHECK (kind IN ('asset', 'liability', 'revenue', 'contra_revenue', 'expense')),
  currency   char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ledger_accounts_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT ledger_accounts_code_key UNIQUE (tenant_id, code, currency)
);
SELECT app_enable_tenant_rls('ledger_accounts');
GRANT SELECT, INSERT ON ledger_accounts TO recorvia_app;

CREATE TABLE ledger_transactions (
  id                      uuid PRIMARY KEY,
  tenant_id               uuid NOT NULL REFERENCES tenants (id),
  kind                    text NOT NULL,
  description             text NOT NULL CHECK (length(description) <= 500),
  occurred_at             timestamptz NOT NULL,
  reference_type          text NOT NULL,
  reference_id            uuid NOT NULL,
  idempotency_key         text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255),
  reverses_transaction_id uuid,
  created_at              timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ledger_transactions_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT ledger_transactions_idempotency_key UNIQUE (tenant_id, idempotency_key),
  CONSTRAINT ledger_transactions_reversal_key UNIQUE (tenant_id, reverses_transaction_id),
  CONSTRAINT ledger_transactions_reversal_fk FOREIGN KEY (tenant_id, reverses_transaction_id) REFERENCES ledger_transactions (tenant_id, id)
);
CREATE INDEX ledger_transactions_tenant_time_idx ON ledger_transactions (tenant_id, occurred_at DESC, id DESC);
CREATE INDEX ledger_transactions_reference_idx ON ledger_transactions (tenant_id, reference_type, reference_id);
CREATE TRIGGER ledger_transactions_immutable BEFORE UPDATE OR DELETE ON ledger_transactions FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('ledger_transactions');
GRANT SELECT, INSERT ON ledger_transactions TO recorvia_app;

CREATE TABLE ledger_entries (
  id             uuid PRIMARY KEY,
  tenant_id      uuid NOT NULL,
  transaction_id uuid NOT NULL,
  account_id     uuid NOT NULL,
  direction      text NOT NULL CHECK (direction IN ('debit', 'credit')),
  amount         bigint NOT NULL CHECK (amount > 0),
  currency       char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  CONSTRAINT ledger_entries_transaction_fk FOREIGN KEY (tenant_id, transaction_id) REFERENCES ledger_transactions (tenant_id, id),
  CONSTRAINT ledger_entries_account_fk FOREIGN KEY (tenant_id, account_id) REFERENCES ledger_accounts (tenant_id, id)
);
CREATE INDEX ledger_entries_transaction_idx ON ledger_entries (tenant_id, transaction_id);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (tenant_id, account_id);
CREATE TRIGGER ledger_entries_immutable BEFORE UPDATE OR DELETE ON ledger_entries FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();

-- +goose StatementBegin
-- Verificação adiada para o commit: cada transação precisa de ao menos duas
-- partidas e soma zero (débitos = créditos) em cada moeda.
CREATE FUNCTION app_ledger_check_balanced() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  v_tx uuid;
  v_count integer;
  v_unbalanced text;
BEGIN
  IF TG_TABLE_NAME = 'ledger_entries' THEN
    v_tx := NEW.transaction_id;
  ELSE
    v_tx := NEW.id;
  END IF;
  SELECT count(*) INTO v_count FROM ledger_entries WHERE transaction_id = v_tx;
  IF v_count < 2 THEN
    RAISE EXCEPTION 'lançamento % possui menos de duas partidas', v_tx USING ERRCODE = 'P0001';
  END IF;
  SELECT string_agg(currency, ',') INTO v_unbalanced FROM (
    SELECT currency FROM ledger_entries WHERE transaction_id = v_tx
     GROUP BY currency
    HAVING sum(CASE WHEN direction = 'debit' THEN amount ELSE -amount END) <> 0) u;
  IF v_unbalanced IS NOT NULL THEN
    RAISE EXCEPTION 'lançamento % desbalanceado em %', v_tx, v_unbalanced USING ERRCODE = 'P0001';
  END IF;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER ledger_entries_balanced AFTER INSERT ON ledger_entries
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION app_ledger_check_balanced();
CREATE CONSTRAINT TRIGGER ledger_transactions_has_entries AFTER INSERT ON ledger_transactions
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION app_ledger_check_balanced();
SELECT app_enable_tenant_rls('ledger_entries');
GRANT SELECT, INSERT ON ledger_entries TO recorvia_app;

-- +goose Down
DROP TABLE IF EXISTS ledger_entries;
DROP FUNCTION IF EXISTS app_ledger_check_balanced();
DROP TABLE IF EXISTS ledger_transactions;
DROP TABLE IF EXISTS ledger_accounts;
DROP TABLE IF EXISTS provider_webhook_events;
DROP TABLE IF EXISTS payments;
DROP FUNCTION IF EXISTS app_payment_operation_stats();
DROP FUNCTION IF EXISTS app_tenants_with_pending_payment_operations(timestamptz);
DROP TABLE IF EXISTS payment_operations;
DROP TABLE IF EXISTS charges;
DROP FUNCTION IF EXISTS app_resolve_webhook_endpoint(text, bytea);
DROP TABLE IF EXISTS payment_provider_accounts;
