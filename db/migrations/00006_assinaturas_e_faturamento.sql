-- Assinaturas dos clientes dos tenants e faturas comerciais.
-- Fatura comercial não é documento fiscal.

-- +goose Up

CREATE TABLE subscriptions (
  id                   uuid PRIMARY KEY,
  tenant_id            uuid NOT NULL,
  customer_id          uuid NOT NULL,
  plan_id              uuid NOT NULL,
  price_version_id     uuid NOT NULL,
  status               text NOT NULL CHECK (status IN ('trialing', 'active', 'past_due', 'suspended', 'canceled')),
  quantity             integer NOT NULL DEFAULT 1 CHECK (quantity BETWEEN 1 AND 1000000),
  billing_interval     text NOT NULL CHECK (billing_interval IN ('month', 'year')),
  -- Fuso do tenant na criação: as regras de calendário do contrato não mudam
  -- se o tenant alterar o fuso depois.
  timezone             text NOT NULL,
  anchor_date          date NOT NULL,
  trial_ends_at        timestamptz,
  started_at           timestamptz NOT NULL,
  current_period_start timestamptz NOT NULL,
  current_period_end   timestamptz NOT NULL,
  -- Próximo período a faturar (cobrança antecipada) e seu início. Em
  -- assinaturas canceladas com consumo, aponta para o fechamento do consumo
  -- final; NULL quando não há mais nada a faturar (ou durante a suspensão).
  next_billing_index   integer NOT NULL DEFAULT 0 CHECK (next_billing_index >= 0),
  next_billing_at      timestamptz,
  cancel_at_period_end boolean NOT NULL DEFAULT false,
  canceled_at          timestamptz,
  cancel_reason        text CHECK (cancel_reason IS NULL OR length(cancel_reason) <= 500),
  ended_at             timestamptz,
  suspended_at         timestamptz,
  suspension_reason    text CHECK (suspension_reason IS NULL OR length(suspension_reason) <= 500),
  payment_terms_days   integer CHECK (payment_terms_days IS NULL OR payment_terms_days BETWEEN 0 AND 120),
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  version              integer NOT NULL DEFAULT 1,
  CONSTRAINT subscriptions_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT subscriptions_customer_fk FOREIGN KEY (tenant_id, customer_id) REFERENCES customers (tenant_id, id),
  CONSTRAINT subscriptions_plan_fk FOREIGN KEY (tenant_id, plan_id) REFERENCES plans (tenant_id, id),
  CONSTRAINT subscriptions_price_fk FOREIGN KEY (tenant_id, price_version_id) REFERENCES plan_price_versions (tenant_id, id),
  CONSTRAINT subscriptions_period_order CHECK (current_period_start < current_period_end),
  CONSTRAINT subscriptions_canceled_ended CHECK (status <> 'canceled' OR ended_at IS NOT NULL)
);
CREATE INDEX subscriptions_due_idx ON subscriptions (next_billing_at) WHERE next_billing_at IS NOT NULL;
CREATE INDEX subscriptions_customer_idx ON subscriptions (tenant_id, customer_id);
CREATE INDEX subscriptions_tenant_created_idx ON subscriptions (tenant_id, created_at DESC, id DESC);
CREATE TRIGGER subscriptions_touch BEFORE UPDATE ON subscriptions FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('subscriptions');
GRANT SELECT, INSERT, UPDATE ON subscriptions TO recorvia_app;

CREATE TABLE subscription_history (
  id              uuid PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  subscription_id uuid NOT NULL,
  event           text NOT NULL,
  from_status     text,
  to_status       text NOT NULL,
  occurred_at     timestamptz NOT NULL,
  actor_type      text NOT NULL,
  actor_id        text,
  details         jsonb NOT NULL DEFAULT '{}',
  CONSTRAINT subscription_history_subscription_fk FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id)
);
CREATE INDEX subscription_history_sub_idx ON subscription_history (tenant_id, subscription_id, occurred_at, id);
CREATE TRIGGER subscription_history_immutable BEFORE UPDATE OR DELETE ON subscription_history FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('subscription_history');
GRANT SELECT, INSERT ON subscription_history TO recorvia_app;

-- +goose StatementBegin
-- Descoberta de tenants com assinaturas a faturar. Retorna apenas IDs; o
-- faturamento ocorre depois, na transação de cada tenant.
CREATE FUNCTION app_tenants_with_due_billing(p_now timestamptz, p_limit integer)
RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT DISTINCT tenant_id FROM subscriptions
   WHERE next_billing_at IS NOT NULL AND next_billing_at <= p_now
   LIMIT p_limit
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_tenants_with_due_billing(timestamptz, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_tenants_with_due_billing(timestamptz, integer) TO recorvia_app;

CREATE TABLE invoice_number_sequences (
  tenant_id   uuid PRIMARY KEY REFERENCES tenants (id),
  next_number bigint NOT NULL DEFAULT 1 CHECK (next_number > 0)
);
SELECT app_enable_tenant_rls('invoice_number_sequences');
GRANT SELECT, INSERT, UPDATE ON invoice_number_sequences TO recorvia_app;

CREATE TABLE invoices (
  id                   uuid PRIMARY KEY,
  tenant_id            uuid NOT NULL,
  customer_id          uuid NOT NULL,
  subscription_id      uuid,
  number               text,
  status               text NOT NULL CHECK (status IN ('draft', 'open', 'partially_paid', 'paid', 'void', 'uncollectible')),
  kind                 text NOT NULL CHECK (kind IN ('subscription_cycle', 'final_usage', 'proration', 'manual')),
  currency             char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  billing_period_start timestamptz,
  billing_period_end   timestamptz,
  issued_at            timestamptz,
  due_at               timestamptz,
  subtotal             bigint NOT NULL,
  discount_total       bigint NOT NULL DEFAULT 0 CHECK (discount_total >= 0),
  total                bigint NOT NULL CHECK (total >= 0),
  credit_applied       bigint NOT NULL DEFAULT 0 CHECK (credit_applied >= 0),
  amount_paid          bigint NOT NULL DEFAULT 0 CHECK (amount_paid >= 0),
  amount_credited      bigint NOT NULL DEFAULT 0 CHECK (amount_credited >= 0),
  amount_written_off   bigint NOT NULL DEFAULT 0 CHECK (amount_written_off >= 0),
  amount_due           bigint NOT NULL CHECK (amount_due >= 0),
  customer_snapshot    jsonb NOT NULL,
  memo                 text NOT NULL DEFAULT '' CHECK (length(memo) <= 2000),
  paid_at              timestamptz,
  voided_at            timestamptz,
  void_reason          text CHECK (void_reason IS NULL OR length(void_reason) <= 500),
  uncollectible_at     timestamptz,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  version              integer NOT NULL DEFAULT 1,
  CONSTRAINT invoices_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT invoices_number_key UNIQUE (tenant_id, number),
  CONSTRAINT invoices_customer_fk FOREIGN KEY (tenant_id, customer_id) REFERENCES customers (tenant_id, id),
  CONSTRAINT invoices_subscription_fk FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id),
  CONSTRAINT invoices_issued_fields CHECK (status = 'draft' OR (number IS NOT NULL AND issued_at IS NOT NULL AND due_at IS NOT NULL)),
  CONSTRAINT invoices_total_consistency CHECK (total = subtotal - discount_total),
  CONSTRAINT invoices_due_consistency CHECK (amount_due = total - credit_applied - amount_paid - amount_credited - amount_written_off)
);
-- Uma fatura de ciclo por assinatura e período (protege a retomada do agendador).
CREATE UNIQUE INDEX invoices_subscription_period_key ON invoices (tenant_id, subscription_id, kind, billing_period_start)
  WHERE subscription_id IS NOT NULL AND kind IN ('subscription_cycle', 'final_usage');
CREATE INDEX invoices_tenant_issued_idx ON invoices (tenant_id, issued_at DESC, id DESC);
CREATE INDEX invoices_customer_idx ON invoices (tenant_id, customer_id, issued_at DESC);
CREATE INDEX invoices_open_due_idx ON invoices (tenant_id, due_at) WHERE status IN ('open', 'partially_paid');

-- +goose StatementBegin
-- Fatura emitida não pode ser reescrita: campos de emissão e valores
-- faturados ficam congelados; somente o andamento (pagamentos, créditos,
-- cancelamento) evolui, sempre com histórico.
CREATE FUNCTION app_invoice_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.status <> 'draft' THEN
      RAISE EXCEPTION 'fatura emitida % não pode ser excluída', OLD.id USING ERRCODE = 'P0001';
    END IF;
    RETURN OLD;
  END IF;
  IF OLD.status <> 'draft' AND (
       NEW.number IS DISTINCT FROM OLD.number OR
       NEW.issued_at IS DISTINCT FROM OLD.issued_at OR
       NEW.due_at IS DISTINCT FROM OLD.due_at OR
       NEW.customer_id IS DISTINCT FROM OLD.customer_id OR
       NEW.subscription_id IS DISTINCT FROM OLD.subscription_id OR
       NEW.kind IS DISTINCT FROM OLD.kind OR
       NEW.currency IS DISTINCT FROM OLD.currency OR
       NEW.billing_period_start IS DISTINCT FROM OLD.billing_period_start OR
       NEW.billing_period_end IS DISTINCT FROM OLD.billing_period_end OR
       NEW.subtotal IS DISTINCT FROM OLD.subtotal OR
       NEW.discount_total IS DISTINCT FROM OLD.discount_total OR
       NEW.total IS DISTINCT FROM OLD.total OR
       NEW.credit_applied IS DISTINCT FROM OLD.credit_applied OR
       NEW.customer_snapshot IS DISTINCT FROM OLD.customer_snapshot) THEN
    RAISE EXCEPTION 'fatura emitida % não pode ser reescrita', OLD.id USING ERRCODE = 'P0001';
  END IF;
  IF OLD.status IN ('void') AND NEW.status <> OLD.status THEN
    RAISE EXCEPTION 'fatura cancelada % não pode mudar de situação', OLD.id USING ERRCODE = 'P0001';
  END IF;
  NEW.updated_at := now();
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER invoices_guard BEFORE UPDATE OR DELETE ON invoices FOR EACH ROW EXECUTE FUNCTION app_invoice_guard();
SELECT app_enable_tenant_rls('invoices');
GRANT SELECT, INSERT, UPDATE ON invoices TO recorvia_app;

CREATE TABLE invoice_items (
  id                 uuid PRIMARY KEY,
  tenant_id          uuid NOT NULL,
  invoice_id         uuid NOT NULL,
  position           integer NOT NULL CHECK (position >= 0),
  kind               text NOT NULL CHECK (kind IN ('subscription_fee', 'usage', 'overage', 'proration', 'adjustment')),
  description        text NOT NULL CHECK (length(description) BETWEEN 1 AND 500),
  quantity           numeric(20, 6) NOT NULL,
  unit_amount        numeric(24, 10) NOT NULL,
  amount             bigint NOT NULL,
  discount_amount    bigint NOT NULL DEFAULT 0 CHECK (discount_amount >= 0),
  period_start       timestamptz,
  period_end         timestamptz,
  plan_id            uuid,
  price_version_id   uuid,
  price_snapshot     jsonb NOT NULL DEFAULT '{}',
  usage_aggregate_id uuid,
  created_at         timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT invoice_items_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT invoice_items_position_key UNIQUE (tenant_id, invoice_id, position),
  CONSTRAINT invoice_items_invoice_fk FOREIGN KEY (tenant_id, invoice_id) REFERENCES invoices (tenant_id, id),
  CONSTRAINT invoice_items_price_fk FOREIGN KEY (tenant_id, price_version_id) REFERENCES plan_price_versions (tenant_id, id)
);
CREATE TRIGGER invoice_items_immutable BEFORE UPDATE OR DELETE ON invoice_items FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('invoice_items');
GRANT SELECT, INSERT ON invoice_items TO recorvia_app;

CREATE TABLE invoice_status_history (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL,
  invoice_id  uuid NOT NULL,
  from_status text,
  to_status   text NOT NULL,
  occurred_at timestamptz NOT NULL,
  actor_type  text NOT NULL,
  actor_id    text,
  reason      text NOT NULL DEFAULT '',
  details     jsonb NOT NULL DEFAULT '{}',
  CONSTRAINT invoice_status_history_invoice_fk FOREIGN KEY (tenant_id, invoice_id) REFERENCES invoices (tenant_id, id)
);
CREATE INDEX invoice_status_history_invoice_idx ON invoice_status_history (tenant_id, invoice_id, occurred_at, id);
CREATE TRIGGER invoice_status_history_immutable BEFORE UPDATE OR DELETE ON invoice_status_history FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('invoice_status_history');
GRANT SELECT, INSERT ON invoice_status_history TO recorvia_app;

-- +goose Down
DROP TABLE IF EXISTS invoice_status_history;
DROP TABLE IF EXISTS invoice_items;
DROP TABLE IF EXISTS invoices;
DROP FUNCTION IF EXISTS app_invoice_guard();
DROP TABLE IF EXISTS invoice_number_sequences;
DROP FUNCTION IF EXISTS app_tenants_with_due_billing(timestamptz, integer);
DROP TABLE IF EXISTS subscription_history;
DROP TABLE IF EXISTS subscriptions;
