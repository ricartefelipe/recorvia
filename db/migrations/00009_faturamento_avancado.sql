-- Faturamento avançado: mudanças agendadas de plano e quantidade, reajustes
-- programados de preço, correção de consumo atrasado e notas de crédito.

-- +goose Up

-- Mudanças de plano ou quantidade agendadas para o próximo ciclo. A versão de
-- preço é fixada no agendamento: o cliente contratou aquele preço.
CREATE TABLE subscription_scheduled_changes (
  id                   uuid PRIMARY KEY,
  tenant_id            uuid NOT NULL,
  subscription_id      uuid NOT NULL,
  kind                 text NOT NULL CHECK (kind IN ('plan_change', 'quantity_change')),
  new_plan_id          uuid,
  new_price_version_id uuid,
  new_quantity         integer CHECK (new_quantity IS NULL OR new_quantity BETWEEN 1 AND 1000000),
  effective_at         timestamptz NOT NULL,
  status               text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'applied', 'canceled')),
  created_by           uuid REFERENCES users (id),
  created_at           timestamptz NOT NULL,
  resolved_at          timestamptz,
  CONSTRAINT subscription_scheduled_changes_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT subscription_scheduled_changes_subscription_fk FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id),
  CONSTRAINT subscription_scheduled_changes_plan_fk FOREIGN KEY (tenant_id, new_plan_id) REFERENCES plans (tenant_id, id),
  CONSTRAINT subscription_scheduled_changes_price_fk FOREIGN KEY (tenant_id, new_price_version_id) REFERENCES plan_price_versions (tenant_id, id),
  CONSTRAINT subscription_scheduled_changes_plan_consistency CHECK ((kind = 'plan_change') = (new_plan_id IS NOT NULL AND new_price_version_id IS NOT NULL)),
  CONSTRAINT subscription_scheduled_changes_quantity_consistency CHECK (kind = 'plan_change' OR new_quantity IS NOT NULL),
  CONSTRAINT subscription_scheduled_changes_resolution CHECK ((status = 'pending') = (resolved_at IS NULL))
);
CREATE UNIQUE INDEX subscription_scheduled_changes_pending_key ON subscription_scheduled_changes (tenant_id, subscription_id) WHERE status = 'pending';
CREATE INDEX subscription_scheduled_changes_sub_idx ON subscription_scheduled_changes (tenant_id, subscription_id, created_at);
SELECT app_enable_tenant_rls('subscription_scheduled_changes');
GRANT SELECT, INSERT, UPDATE ON subscription_scheduled_changes TO recorvia_app;

-- Reajustes programados do componente fixo da assinatura (valor fixo ou
-- preço por unidade). Aplicados no primeiro ciclo iniciado na data efetiva
-- ou depois; ciclos já faturados nunca são alterados. Os valores anterior e
-- resultante usam a escala do preço unitário (10 casas em centavos): no
-- preço por unidade o reajuste percentual mantém a fração de centavo e o
-- arredondamento a centavos acontece uma única vez, no total do item.
-- cancel_reason registra por que um reajuste programado deixou de valer.
CREATE TABLE subscription_price_adjustments (
  id                   uuid PRIMARY KEY,
  tenant_id            uuid NOT NULL,
  subscription_id      uuid NOT NULL,
  effective_date       date NOT NULL,
  kind                 text NOT NULL CHECK (kind IN ('percent', 'fixed_amount')),
  percent_bp           integer CHECK (percent_bp IS NULL OR percent_bp BETWEEN -10000 AND 100000),
  new_amount           bigint CHECK (new_amount IS NULL OR new_amount >= 0),
  index_name           text NOT NULL DEFAULT '' CHECK (length(index_name) <= 50),
  reason               text NOT NULL CHECK (length(reason) BETWEEN 3 AND 500),
  status               text NOT NULL DEFAULT 'scheduled' CHECK (status IN ('scheduled', 'applied', 'canceled')),
  previous_amount      numeric(24, 10) CHECK (previous_amount IS NULL OR previous_amount >= 0),
  resulting_amount     numeric(24, 10) CHECK (resulting_amount IS NULL OR resulting_amount >= 0),
  applied_period_start timestamptz,
  applied_at           timestamptz,
  canceled_at          timestamptz,
  cancel_reason        text CHECK (cancel_reason IS NULL OR cancel_reason IN ('manual', 'subscription_ended', 'price_version_changed',
                                                                            'price_without_fixed_component', 'adjustment_amount_limit')),
  created_by           uuid REFERENCES users (id),
  created_at           timestamptz NOT NULL,
  CONSTRAINT subscription_price_adjustments_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT subscription_price_adjustments_subscription_fk FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id),
  CONSTRAINT subscription_price_adjustments_percent CHECK ((kind = 'percent') = (percent_bp IS NOT NULL)),
  CONSTRAINT subscription_price_adjustments_amount CHECK ((kind = 'fixed_amount') = (new_amount IS NOT NULL)),
  CONSTRAINT subscription_price_adjustments_applied CHECK (status <> 'applied' OR
    (previous_amount IS NOT NULL AND resulting_amount IS NOT NULL AND applied_at IS NOT NULL AND applied_period_start IS NOT NULL)),
  CONSTRAINT subscription_price_adjustments_canceled CHECK ((status = 'canceled') = (canceled_at IS NOT NULL)),
  CONSTRAINT subscription_price_adjustments_cancel_reason CHECK ((status = 'canceled') = (cancel_reason IS NOT NULL))
);
CREATE UNIQUE INDEX subscription_price_adjustments_date_key ON subscription_price_adjustments (tenant_id, subscription_id, effective_date)
  WHERE status = 'scheduled';
CREATE INDEX subscription_price_adjustments_sub_idx ON subscription_price_adjustments (tenant_id, subscription_id, effective_date, created_at);
SELECT app_enable_tenant_rls('subscription_price_adjustments');
GRANT SELECT, INSERT, UPDATE ON subscription_price_adjustments TO recorvia_app;

-- Valor do componente fixo após reajuste (substitui o valor fixo ou o preço
-- por unidade da versão de preço apenas nesta assinatura), na escala do preço
-- unitário. Mudança de versão de preço remove o reajuste vigente e cancela os
-- reajustes programados.
ALTER TABLE subscriptions
  ADD COLUMN price_override_amount numeric(24, 10) CHECK (price_override_amount IS NULL OR price_override_amount >= 0),
  ADD COLUMN price_override_adjustment_id uuid,
  ADD CONSTRAINT subscriptions_override_adjustment_fk FOREIGN KEY (tenant_id, price_override_adjustment_id)
    REFERENCES subscription_price_adjustments (tenant_id, id),
  ADD CONSTRAINT subscriptions_override_consistency CHECK ((price_override_amount IS NULL) = (price_override_adjustment_id IS NULL));

-- Correção de consumo atrasado: o agregado registra a versão de preço usada
-- no fechamento; a correção vira item pendente (valor positivo) ou crédito do
-- cliente (valor negativo), sem alterar a fatura emitida.
-- corrected_quantity/corrected_amount guardam o resultado acumulado das
-- correções (a quantidade e o valor faturados originalmente não mudam).
ALTER TABLE usage_aggregates
  ADD COLUMN price_version_id uuid,
  ADD COLUMN corrected_quantity numeric(20, 6) CHECK (corrected_quantity IS NULL OR corrected_quantity >= 0),
  ADD COLUMN corrected_amount bigint CHECK (corrected_amount IS NULL OR corrected_amount >= 0),
  ADD CONSTRAINT usage_aggregates_price_fk FOREIGN KEY (tenant_id, price_version_id) REFERENCES plan_price_versions (tenant_id, id);

ALTER TABLE pending_invoice_items
  ADD COLUMN usage_aggregate_id uuid,
  ADD CONSTRAINT pending_invoice_items_aggregate_fk FOREIGN KEY (tenant_id, usage_aggregate_id) REFERENCES usage_aggregates (tenant_id, id),
  ADD CONSTRAINT pending_invoice_items_positive CHECK (amount > 0);
CREATE INDEX pending_invoice_items_aggregate_idx ON pending_invoice_items (tenant_id, usage_aggregate_id) WHERE usage_aggregate_id IS NOT NULL;

ALTER TABLE usage_events ADD COLUMN corrected_at timestamptz;
DROP INDEX usage_events_late_idx;
CREATE INDEX usage_events_late_idx ON usage_events (tenant_id, subscription_id, metric_id, period_start) WHERE status = 'late' AND corrected_at IS NULL;
CREATE INDEX usage_events_received_idx ON usage_events (tenant_id, received_at DESC, id DESC);
-- Eventos aceitos ainda sem agregado (períodos abertos). A correção procura
-- aqui os que ficaram fora de um período já fechado.
CREATE INDEX usage_events_unattached_idx ON usage_events (tenant_id, subscription_id, metric_id, period_start)
  WHERE status = 'accepted' AND aggregate_id IS NULL;

ALTER TABLE customer_credit_entries DROP CONSTRAINT customer_credit_entries_kind_check;
ALTER TABLE customer_credit_entries ADD CONSTRAINT customer_credit_entries_kind_check
  CHECK (kind IN ('grant', 'consumption', 'return', 'overpayment', 'proration', 'usage_correction'));

-- Tenants com consumo a corrigir: eventos atrasados não corrigidos ou
-- eventos aceitos sem agregado num período já fechado.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_tenants_with_late_usage()
RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT tenant_id FROM usage_events WHERE status = 'late' AND corrected_at IS NULL
  UNION
  SELECT a.tenant_id FROM usage_aggregates a
   WHERE EXISTS (SELECT 1 FROM usage_events e
                  WHERE e.tenant_id = a.tenant_id AND e.subscription_id = a.subscription_id AND e.metric_id = a.metric_id
                    AND e.period_start = a.period_start AND e.status = 'accepted' AND e.aggregate_id IS NULL)
$$;
-- +goose StatementEnd

-- Notas de crédito: reduzem o saldo devedor de uma fatura emitida sem
-- reescrevê-la. Numeração derivada do número da fatura.
CREATE TABLE credit_notes (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL,
  invoice_id  uuid NOT NULL,
  number      text NOT NULL CHECK (length(number) BETWEEN 1 AND 80),
  amount      bigint NOT NULL CHECK (amount > 0),
  currency    char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  reason      text NOT NULL CHECK (length(reason) BETWEEN 3 AND 500),
  created_by  uuid REFERENCES users (id),
  request_key text CHECK (request_key IS NULL OR length(request_key) <= 300),
  created_at  timestamptz NOT NULL,
  CONSTRAINT credit_notes_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT credit_notes_number_key UNIQUE (tenant_id, number),
  CONSTRAINT credit_notes_request_key UNIQUE (tenant_id, request_key),
  CONSTRAINT credit_notes_invoice_fk FOREIGN KEY (tenant_id, invoice_id) REFERENCES invoices (tenant_id, id)
);
CREATE INDEX credit_notes_invoice_idx ON credit_notes (tenant_id, invoice_id, created_at);
CREATE TRIGGER credit_notes_immutable BEFORE UPDATE OR DELETE ON credit_notes FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('credit_notes');
GRANT SELECT, INSERT ON credit_notes TO recorvia_app;

-- +goose Down
DROP TABLE IF EXISTS credit_notes;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_tenants_with_late_usage()
RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$ SELECT DISTINCT tenant_id FROM usage_events WHERE status = 'late' AND correction_item_id IS NULL $$;
-- +goose StatementEnd
ALTER TABLE customer_credit_entries DROP CONSTRAINT customer_credit_entries_kind_check;
ALTER TABLE customer_credit_entries ADD CONSTRAINT customer_credit_entries_kind_check
  CHECK (kind IN ('grant', 'consumption', 'return', 'overpayment'));
DROP INDEX IF EXISTS usage_events_unattached_idx;
DROP INDEX IF EXISTS usage_events_received_idx;
DROP INDEX IF EXISTS usage_events_late_idx;
CREATE INDEX usage_events_late_idx ON usage_events (tenant_id, received_at) WHERE status = 'late' AND correction_item_id IS NULL;
ALTER TABLE usage_events DROP COLUMN IF EXISTS corrected_at;
DROP INDEX IF EXISTS pending_invoice_items_aggregate_idx;
ALTER TABLE pending_invoice_items DROP CONSTRAINT IF EXISTS pending_invoice_items_positive;
ALTER TABLE pending_invoice_items DROP COLUMN IF EXISTS usage_aggregate_id;
ALTER TABLE usage_aggregates DROP COLUMN IF EXISTS corrected_amount;
ALTER TABLE usage_aggregates DROP COLUMN IF EXISTS corrected_quantity;
ALTER TABLE usage_aggregates DROP COLUMN IF EXISTS price_version_id;
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_override_consistency;
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_override_adjustment_fk;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS price_override_adjustment_id;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS price_override_amount;
DROP TABLE IF EXISTS subscription_price_adjustments;
DROP TABLE IF EXISTS subscription_scheduled_changes;
