-- Faturamento da própria plataforma (relacionamento tenant -> Recorvia).
-- Separado do faturamento que os tenants fazem a seus clientes: tabelas com
-- prefixo platform_ e assinatura do tenant em tenant_platform_subscriptions.
-- Os valores dos planos são hipóteses comerciais configuráveis.

-- +goose Up

CREATE TABLE platform_plans (
  code                     text PRIMARY KEY CHECK (code ~ '^[a-z0-9_]{2,40}$'),
  name                     text NOT NULL,
  description              text NOT NULL DEFAULT '',
  monthly_price_amount     bigint NOT NULL CHECK (monthly_price_amount >= 0),
  currency                 char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  max_users                integer CHECK (max_users IS NULL OR max_users > 0),
  max_active_subscriptions integer CHECK (max_active_subscriptions IS NULL OR max_active_subscriptions > 0),
  features                 text[] NOT NULL DEFAULT '{}',
  trial_days               integer NOT NULL DEFAULT 14 CHECK (trial_days BETWEEN 0 AND 90),
  is_default               boolean NOT NULL DEFAULT false,
  active                   boolean NOT NULL DEFAULT true,
  sort_order               integer NOT NULL DEFAULT 0,
  created_at               timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX platform_plans_single_default ON platform_plans (is_default) WHERE is_default;
GRANT SELECT ON platform_plans TO recorvia_app;

CREATE TABLE platform_addons (
  code         text PRIMARY KEY CHECK (code ~ '^[a-z0-9_]{2,40}$'),
  name         text NOT NULL,
  description  text NOT NULL DEFAULT '',
  price_amount bigint NOT NULL CHECK (price_amount >= 0),
  currency     char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  kind         text NOT NULL CHECK (kind IN ('one_time')),
  active       boolean NOT NULL DEFAULT true
);
GRANT SELECT ON platform_addons TO recorvia_app;

-- Hipóteses comerciais iniciais (ver docs/negocio/modelo-comercial.md).
INSERT INTO platform_plans (code, name, description, monthly_price_amount, max_users, max_active_subscriptions, features, trial_days, is_default, sort_order) VALUES
  ('essencial', 'Essencial', 'Até 3 usuários e 100 contratos ativos.', 19900, 3, 100,
   ARRAY['recurring_billing', 'email_dunning'], 14, true, 1),
  ('profissional', 'Profissional', 'Até 10 usuários e 1.000 contratos ativos, consumo medido e webhooks.', 49900, 10, 1000,
   ARRAY['recurring_billing', 'email_dunning', 'usage_billing', 'outbound_webhooks', 'api_access', 'asaas_integration'], 14, false, 2),
  ('escala', 'Escala', 'Usuários e contratos sem limite, com todos os recursos.', 129900, NULL, NULL,
   ARRAY['recurring_billing', 'email_dunning', 'usage_billing', 'outbound_webhooks', 'api_access', 'asaas_integration', 'reconciliation_automation'], 14, false, 3);

INSERT INTO platform_addons (code, name, description, price_amount, kind) VALUES
  ('implantacao_assistida', 'Implantação assistida',
   'Configuração do catálogo, migração de contratos e treinamento da equipe.', 250000, 'one_time');

CREATE TABLE tenant_platform_subscriptions (
  tenant_id            uuid PRIMARY KEY REFERENCES tenants (id),
  plan_code            text NOT NULL REFERENCES platform_plans (code),
  status               text NOT NULL CHECK (status IN ('trialing', 'active', 'past_due', 'canceled')),
  trial_ends_at        timestamptz,
  current_period_start timestamptz NOT NULL,
  current_period_end   timestamptz NOT NULL,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  version              integer NOT NULL DEFAULT 1,
  CHECK (current_period_start < current_period_end)
);
CREATE TRIGGER tenant_platform_subscriptions_touch BEFORE UPDATE ON tenant_platform_subscriptions FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('tenant_platform_subscriptions');
GRANT SELECT, INSERT, UPDATE ON tenant_platform_subscriptions TO recorvia_app;

CREATE TABLE platform_addon_orders (
  id           uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES tenants (id),
  addon_code   text NOT NULL REFERENCES platform_addons (code),
  status       text NOT NULL DEFAULT 'requested' CHECK (status IN ('requested', 'scheduled', 'completed', 'canceled')),
  notes        text NOT NULL DEFAULT '' CHECK (length(notes) <= 2000),
  price_amount bigint NOT NULL CHECK (price_amount >= 0),
  requested_by uuid NOT NULL REFERENCES users (id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT platform_addon_orders_tenant_id_key UNIQUE (tenant_id, id)
);
SELECT app_enable_tenant_rls('platform_addon_orders');
GRANT SELECT, INSERT, UPDATE ON platform_addon_orders TO recorvia_app;

CREATE TABLE platform_statements (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL REFERENCES tenants (id),
  period_start        timestamptz NOT NULL,
  period_end          timestamptz NOT NULL,
  plan_code           text NOT NULL REFERENCES platform_plans (code),
  subscription_amount bigint NOT NULL CHECK (subscription_amount >= 0),
  addons_amount       bigint NOT NULL DEFAULT 0 CHECK (addons_amount >= 0),
  total_amount        bigint NOT NULL CHECK (total_amount = subscription_amount + addons_amount),
  currency            char(3) NOT NULL DEFAULT 'BRL',
  status              text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'paid', 'void')),
  created_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT platform_statements_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT platform_statements_period_key UNIQUE (tenant_id, period_start)
);
SELECT app_enable_tenant_rls('platform_statements');
GRANT SELECT, INSERT, UPDATE ON platform_statements TO recorvia_app;

-- +goose StatementBegin
-- app_platform_tenant_catalog alimenta a visão da equipe da plataforma:
-- organização, plano e contagens operacionais, sem valores financeiros dos
-- clientes dos tenants.
CREATE FUNCTION app_platform_tenant_catalog()
RETURNS TABLE (tenant_id uuid, name text, slug text, status text, plan_code text, platform_status text,
               member_count bigint, created_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT t.id, t.name, t.slug, t.status, s.plan_code, s.status,
         (SELECT count(*) FROM memberships m WHERE m.tenant_id = t.id AND m.status = 'active'),
         t.created_at
  FROM tenants t
  LEFT JOIN tenant_platform_subscriptions s ON s.tenant_id = t.id
  ORDER BY t.created_at, t.id
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_platform_tenant_catalog() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_platform_tenant_catalog() TO recorvia_app;

-- +goose StatementBegin
CREATE FUNCTION app_tenants_due_platform_statement(p_now timestamptz)
RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT tenant_id FROM tenant_platform_subscriptions
  WHERE status IN ('trialing', 'active', 'past_due') AND current_period_end <= p_now
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_tenants_due_platform_statement(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_tenants_due_platform_statement(timestamptz) TO recorvia_app;

-- +goose Down
DROP FUNCTION IF EXISTS app_tenants_due_platform_statement(timestamptz);
DROP FUNCTION IF EXISTS app_platform_tenant_catalog();
DROP TABLE IF EXISTS platform_statements;
DROP TABLE IF EXISTS platform_addon_orders;
DROP TABLE IF EXISTS tenant_platform_subscriptions;
DROP TABLE IF EXISTS platform_addons;
DROP TABLE IF EXISTS platform_plans;
