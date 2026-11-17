-- Créditos de clientes, cupons e descontos, ajustes pendentes (prorrata e
-- correções) e medição de consumo.

-- +goose Up

-- Movimentos de crédito do cliente (imutáveis). Saldo = soma dos movimentos.
CREATE TABLE customer_credit_entries (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL,
  customer_id uuid NOT NULL,
  amount      bigint NOT NULL CHECK (amount <> 0),
  currency    char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  kind        text NOT NULL CHECK (kind IN ('grant', 'consumption', 'return', 'overpayment')),
  invoice_id  uuid,
  reason      text NOT NULL DEFAULT '' CHECK (length(reason) <= 500),
  created_by  uuid REFERENCES users (id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT customer_credit_entries_customer_fk FOREIGN KEY (tenant_id, customer_id) REFERENCES customers (tenant_id, id),
  CONSTRAINT customer_credit_entries_invoice_fk FOREIGN KEY (tenant_id, invoice_id) REFERENCES invoices (tenant_id, id),
  CONSTRAINT customer_credit_entries_sign CHECK ((kind = 'consumption') = (amount < 0))
);
CREATE INDEX customer_credit_entries_customer_idx ON customer_credit_entries (tenant_id, customer_id, created_at);
CREATE TRIGGER customer_credit_entries_immutable BEFORE UPDATE OR DELETE ON customer_credit_entries FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('customer_credit_entries');
GRANT SELECT, INSERT ON customer_credit_entries TO recorvia_app;

CREATE TABLE coupons (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL REFERENCES tenants (id),
  code                text NOT NULL CHECK (code ~ '^[A-Z0-9_-]{3,40}$'),
  name                text NOT NULL CHECK (length(name) BETWEEN 2 AND 200),
  discount_type       text NOT NULL CHECK (discount_type IN ('percent', 'amount')),
  percent_off_bp      integer CHECK (percent_off_bp IS NULL OR percent_off_bp BETWEEN 1 AND 10000),
  amount_off          bigint CHECK (amount_off IS NULL OR amount_off > 0),
  currency            char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  duration            text NOT NULL CHECK (duration IN ('once', 'repeating', 'forever')),
  duration_cycles     integer CHECK (duration_cycles IS NULL OR duration_cycles BETWEEN 1 AND 120),
  max_redemptions     integer CHECK (max_redemptions IS NULL OR max_redemptions > 0),
  redemptions_count   integer NOT NULL DEFAULT 0 CHECK (redemptions_count >= 0),
  valid_from          timestamptz,
  valid_until         timestamptz,
  applies_to_plan_ids uuid[] NOT NULL DEFAULT '{}',
  status              text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  version             integer NOT NULL DEFAULT 1,
  CONSTRAINT coupons_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT coupons_code_key UNIQUE (tenant_id, code),
  CONSTRAINT coupons_type_consistency CHECK (
    (discount_type = 'percent' AND percent_off_bp IS NOT NULL AND amount_off IS NULL) OR
    (discount_type = 'amount' AND amount_off IS NOT NULL AND percent_off_bp IS NULL)),
  CONSTRAINT coupons_duration_consistency CHECK ((duration = 'repeating') = (duration_cycles IS NOT NULL)),
  CONSTRAINT coupons_redemption_limit CHECK (max_redemptions IS NULL OR redemptions_count <= max_redemptions),
  CONSTRAINT coupons_validity CHECK (valid_from IS NULL OR valid_until IS NULL OR valid_from < valid_until)
);
CREATE TRIGGER coupons_touch BEFORE UPDATE ON coupons FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('coupons');
GRANT SELECT, INSERT, UPDATE ON coupons TO recorvia_app;

CREATE TABLE subscription_discounts (
  id               uuid PRIMARY KEY,
  tenant_id        uuid NOT NULL,
  subscription_id  uuid NOT NULL,
  coupon_id        uuid NOT NULL,
  remaining_cycles integer CHECK (remaining_cycles IS NULL OR remaining_cycles >= 0),
  status           text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'ended')),
  applied_by       uuid REFERENCES users (id),
  created_at       timestamptz NOT NULL DEFAULT now(),
  ended_at         timestamptz,
  CONSTRAINT subscription_discounts_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT subscription_discounts_subscription_fk FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id),
  CONSTRAINT subscription_discounts_coupon_fk FOREIGN KEY (tenant_id, coupon_id) REFERENCES coupons (tenant_id, id)
);
CREATE UNIQUE INDEX subscription_discounts_active_key ON subscription_discounts (tenant_id, subscription_id) WHERE status = 'active';
SELECT app_enable_tenant_rls('subscription_discounts');
GRANT SELECT, INSERT, UPDATE ON subscription_discounts TO recorvia_app;

-- Ajustes aguardando a próxima fatura (prorrata, correção de consumo após o
-- fechamento, ajuste manual). Uma vez faturado, invoice_id é preenchido.
CREATE TABLE pending_invoice_items (
  id              uuid PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  customer_id     uuid NOT NULL,
  subscription_id uuid NOT NULL,
  kind            text NOT NULL CHECK (kind IN ('proration', 'adjustment')),
  source          text NOT NULL CHECK (source IN ('proration', 'usage_correction', 'manual')),
  description     text NOT NULL CHECK (length(description) BETWEEN 1 AND 500),
  amount          bigint NOT NULL,
  quantity        numeric(20, 6),
  unit_amount     numeric(24, 10),
  period_start    timestamptz,
  period_end      timestamptz,
  details         jsonb NOT NULL DEFAULT '{}',
  invoice_id      uuid,
  created_by      uuid REFERENCES users (id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT pending_invoice_items_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT pending_invoice_items_customer_fk FOREIGN KEY (tenant_id, customer_id) REFERENCES customers (tenant_id, id),
  CONSTRAINT pending_invoice_items_subscription_fk FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id),
  CONSTRAINT pending_invoice_items_invoice_fk FOREIGN KEY (tenant_id, invoice_id) REFERENCES invoices (tenant_id, id)
);
CREATE INDEX pending_invoice_items_open_idx ON pending_invoice_items (tenant_id, subscription_id) WHERE invoice_id IS NULL;
SELECT app_enable_tenant_rls('pending_invoice_items');
GRANT SELECT, INSERT, UPDATE ON pending_invoice_items TO recorvia_app;

-- Eventos de consumo. external_id deduplica reenvios; period_start indica
-- o período de cobrança ao qual o evento pertence (pela data de ocorrência).
CREATE TABLE usage_events (
  id                 uuid PRIMARY KEY,
  tenant_id          uuid NOT NULL,
  external_id        text NOT NULL CHECK (length(external_id) BETWEEN 1 AND 128),
  customer_id        uuid NOT NULL,
  subscription_id    uuid NOT NULL,
  metric_id          uuid NOT NULL,
  quantity           numeric(20, 6) NOT NULL CHECK (quantity >= 0),
  unique_key         text CHECK (unique_key IS NULL OR length(unique_key) BETWEEN 1 AND 128),
  occurred_at        timestamptz NOT NULL,
  received_at        timestamptz NOT NULL,
  period_start       timestamptz NOT NULL,
  status             text NOT NULL DEFAULT 'accepted' CHECK (status IN ('accepted', 'late')),
  aggregate_id       uuid,
  correction_item_id uuid,
  properties         jsonb NOT NULL DEFAULT '{}',
  CONSTRAINT usage_events_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT usage_events_external_key UNIQUE (tenant_id, external_id),
  CONSTRAINT usage_events_customer_fk FOREIGN KEY (tenant_id, customer_id) REFERENCES customers (tenant_id, id),
  CONSTRAINT usage_events_subscription_fk FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id),
  CONSTRAINT usage_events_metric_fk FOREIGN KEY (tenant_id, metric_id) REFERENCES billable_metrics (tenant_id, id),
  CONSTRAINT usage_events_correction_fk FOREIGN KEY (tenant_id, correction_item_id) REFERENCES pending_invoice_items (tenant_id, id)
);
CREATE INDEX usage_events_period_idx ON usage_events (tenant_id, subscription_id, metric_id, period_start, occurred_at);
CREATE INDEX usage_events_late_idx ON usage_events (tenant_id, received_at) WHERE status = 'late' AND correction_item_id IS NULL;
SELECT app_enable_tenant_rls('usage_events');
GRANT SELECT, INSERT, UPDATE ON usage_events TO recorvia_app;

-- Agregado fechado de uma métrica num período, vinculado ao item faturado.
CREATE TABLE usage_aggregates (
  id              uuid PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  subscription_id uuid NOT NULL,
  metric_id       uuid NOT NULL,
  period_start    timestamptz NOT NULL,
  period_end      timestamptz NOT NULL,
  aggregation     text NOT NULL CHECK (aggregation IN ('sum', 'max', 'unique_count', 'last')),
  quantity        numeric(20, 6) NOT NULL CHECK (quantity >= 0),
  event_count     integer NOT NULL CHECK (event_count >= 0),
  invoice_item_id uuid,
  closed_at       timestamptz NOT NULL,
  CONSTRAINT usage_aggregates_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT usage_aggregates_period_key UNIQUE (tenant_id, subscription_id, metric_id, period_start),
  CONSTRAINT usage_aggregates_subscription_fk FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id),
  CONSTRAINT usage_aggregates_metric_fk FOREIGN KEY (tenant_id, metric_id) REFERENCES billable_metrics (tenant_id, id),
  CONSTRAINT usage_aggregates_item_fk FOREIGN KEY (tenant_id, invoice_item_id) REFERENCES invoice_items (tenant_id, id)
);
SELECT app_enable_tenant_rls('usage_aggregates');
GRANT SELECT, INSERT, UPDATE ON usage_aggregates TO recorvia_app;

ALTER TABLE usage_events ADD CONSTRAINT usage_events_aggregate_fk
  FOREIGN KEY (tenant_id, aggregate_id) REFERENCES usage_aggregates (tenant_id, id);

-- +goose StatementBegin
CREATE FUNCTION app_tenants_with_late_usage()
RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$ SELECT DISTINCT tenant_id FROM usage_events WHERE status = 'late' AND correction_item_id IS NULL $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_tenants_with_late_usage() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_tenants_with_late_usage() TO recorvia_app;

-- +goose Down
DROP FUNCTION IF EXISTS app_tenants_with_late_usage();
ALTER TABLE usage_events DROP CONSTRAINT IF EXISTS usage_events_aggregate_fk;
DROP TABLE IF EXISTS usage_aggregates;
DROP TABLE IF EXISTS usage_events;
DROP TABLE IF EXISTS pending_invoice_items;
DROP TABLE IF EXISTS subscription_discounts;
DROP TABLE IF EXISTS coupons;
DROP TABLE IF EXISTS customer_credit_entries;
