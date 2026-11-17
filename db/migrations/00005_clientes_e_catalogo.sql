-- Clientes dos tenants e catálogo comercial (produtos, métricas, planos e
-- versões de preço imutáveis).

-- +goose Up

CREATE TABLE customers (
  id                 uuid PRIMARY KEY,
  tenant_id          uuid NOT NULL REFERENCES tenants (id),
  kind               text NOT NULL CHECK (kind IN ('individual', 'company')),
  name               text NOT NULL CHECK (length(name) BETWEEN 2 AND 200),
  legal_name         text CHECK (legal_name IS NULL OR length(legal_name) <= 200),
  document_type      text NOT NULL CHECK (document_type IN ('cpf', 'cnpj')),
  -- CNPJ alfanumérico: 12 caracteres [0-9A-Z] e 2 dígitos verificadores.
  document_number    text NOT NULL CHECK (
    (document_type = 'cpf' AND document_number ~ '^[0-9]{11}$') OR
    (document_type = 'cnpj' AND document_number ~ '^[0-9A-Z]{12}[0-9]{2}$')),
  email              text CHECK (email IS NULL OR length(email) BETWEEN 3 AND 320),
  phone              text CHECK (phone IS NULL OR phone ~ '^\+?[0-9]{8,15}$'),
  external_id        text CHECK (external_id IS NULL OR length(external_id) BETWEEN 1 AND 128),
  status             text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
  payment_terms_days integer CHECK (payment_terms_days IS NULL OR payment_terms_days BETWEEN 0 AND 120),
  anonymized_at      timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  version            integer NOT NULL DEFAULT 1,
  CONSTRAINT customers_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT customers_external_id_key UNIQUE (tenant_id, external_id)
);
CREATE UNIQUE INDEX customers_active_document_key ON customers (tenant_id, document_type, document_number) WHERE status = 'active';
CREATE INDEX customers_tenant_name_idx ON customers (tenant_id, lower(name), id);
CREATE INDEX customers_tenant_created_idx ON customers (tenant_id, created_at DESC, id DESC);
CREATE TRIGGER customers_touch BEFORE UPDATE ON customers FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('customers');
GRANT SELECT, INSERT, UPDATE ON customers TO recorvia_app;

CREATE TABLE customer_contacts (
  id                     uuid PRIMARY KEY,
  tenant_id              uuid NOT NULL,
  customer_id            uuid NOT NULL,
  name                   text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
  email                  text NOT NULL CHECK (length(email) BETWEEN 3 AND 320),
  phone                  text CHECK (phone IS NULL OR phone ~ '^\+?[0-9]{8,15}$'),
  role                   text NOT NULL DEFAULT '' CHECK (length(role) <= 100),
  receives_billing_email boolean NOT NULL DEFAULT true,
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT customer_contacts_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT customer_contacts_customer_fk FOREIGN KEY (tenant_id, customer_id) REFERENCES customers (tenant_id, id)
);
CREATE INDEX customer_contacts_customer_idx ON customer_contacts (tenant_id, customer_id);
CREATE TRIGGER customer_contacts_touch BEFORE UPDATE ON customer_contacts FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('customer_contacts');
GRANT SELECT, INSERT, UPDATE, DELETE ON customer_contacts TO recorvia_app;

CREATE TABLE customer_billing_addresses (
  tenant_id   uuid NOT NULL,
  customer_id uuid NOT NULL,
  street      text NOT NULL CHECK (length(street) BETWEEN 1 AND 200),
  number      text NOT NULL CHECK (length(number) BETWEEN 1 AND 20),
  complement  text NOT NULL DEFAULT '' CHECK (length(complement) <= 100),
  district    text NOT NULL CHECK (length(district) BETWEEN 1 AND 100),
  city        text NOT NULL CHECK (length(city) BETWEEN 1 AND 100),
  state       char(2) NOT NULL CHECK (state ~ '^[A-Z]{2}$'),
  postal_code text NOT NULL CHECK (postal_code ~ '^[0-9]{8}$'),
  country     char(2) NOT NULL DEFAULT 'BR' CHECK (country ~ '^[A-Z]{2}$'),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, customer_id),
  CONSTRAINT customer_billing_addresses_customer_fk FOREIGN KEY (tenant_id, customer_id) REFERENCES customers (tenant_id, id)
);
SELECT app_enable_tenant_rls('customer_billing_addresses');
GRANT SELECT, INSERT, UPDATE ON customer_billing_addresses TO recorvia_app;

-- Identificador do cliente em cada provedor de pagamento.
CREATE TABLE customer_provider_refs (
  tenant_id            uuid NOT NULL,
  customer_id          uuid NOT NULL,
  provider             text NOT NULL CHECK (provider IN ('simulator', 'asaas')),
  provider_customer_id text NOT NULL,
  created_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, customer_id, provider),
  CONSTRAINT customer_provider_refs_provider_key UNIQUE (tenant_id, provider, provider_customer_id),
  CONSTRAINT customer_provider_refs_customer_fk FOREIGN KEY (tenant_id, customer_id) REFERENCES customers (tenant_id, id)
);
SELECT app_enable_tenant_rls('customer_provider_refs');
GRANT SELECT, INSERT ON customer_provider_refs TO recorvia_app;

CREATE TABLE products (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL REFERENCES tenants (id),
  name        text NOT NULL CHECK (length(name) BETWEEN 2 AND 200),
  description text NOT NULL DEFAULT '' CHECK (length(description) <= 2000),
  status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  version     integer NOT NULL DEFAULT 1,
  CONSTRAINT products_tenant_id_key UNIQUE (tenant_id, id)
);
CREATE TRIGGER products_touch BEFORE UPDATE ON products FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('products');
GRANT SELECT, INSERT, UPDATE ON products TO recorvia_app;

CREATE TABLE billable_metrics (
  id                     uuid PRIMARY KEY,
  tenant_id              uuid NOT NULL REFERENCES tenants (id),
  code                   text NOT NULL CHECK (code ~ '^[a-z0-9][a-z0-9_.-]{1,63}$'),
  name                   text NOT NULL CHECK (length(name) BETWEEN 2 AND 200),
  unit                   text NOT NULL CHECK (unit ~ '^[a-z0-9_]{1,32}$'),
  aggregation            text NOT NULL CHECK (aggregation IN ('sum', 'max', 'unique_count', 'last')),
  max_quantity_per_event numeric(20, 6) NOT NULL CHECK (max_quantity_per_event > 0),
  late_grace_hours       integer NOT NULL DEFAULT 72 CHECK (late_grace_hours BETWEEN 0 AND 720),
  status                 text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
  created_at             timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT billable_metrics_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT billable_metrics_code_key UNIQUE (tenant_id, code)
);
SELECT app_enable_tenant_rls('billable_metrics');
GRANT SELECT, INSERT, UPDATE ON billable_metrics TO recorvia_app;

CREATE TABLE plans (
  id                       uuid PRIMARY KEY,
  tenant_id                uuid NOT NULL REFERENCES tenants (id),
  product_id               uuid NOT NULL,
  code                     text NOT NULL CHECK (code ~ '^[a-z0-9][a-z0-9_.-]{1,63}$'),
  name                     text NOT NULL CHECK (length(name) BETWEEN 2 AND 200),
  description              text NOT NULL DEFAULT '' CHECK (length(description) <= 2000),
  billing_interval         text NOT NULL CHECK (billing_interval IN ('month', 'year')),
  currency                 char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  trial_days               integer NOT NULL DEFAULT 0 CHECK (trial_days BETWEEN 0 AND 365),
  status                   text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
  current_price_version_id uuid,
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now(),
  version                  integer NOT NULL DEFAULT 1,
  CONSTRAINT plans_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT plans_code_key UNIQUE (tenant_id, code),
  CONSTRAINT plans_product_fk FOREIGN KEY (tenant_id, product_id) REFERENCES products (tenant_id, id)
);
CREATE TRIGGER plans_touch BEFORE UPDATE ON plans FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('plans');
GRANT SELECT, INSERT, UPDATE ON plans TO recorvia_app;

-- Versões de preço são imutáveis: alterar o preço cria uma nova versão.
-- Valores unitários fracionários ficam em unidades mínimas (centavos) com
-- até 10 casas decimais.
CREATE TABLE plan_price_versions (
  id                uuid PRIMARY KEY,
  tenant_id         uuid NOT NULL,
  plan_id           uuid NOT NULL,
  version           integer NOT NULL CHECK (version > 0),
  pricing_model     text NOT NULL CHECK (pricing_model IN ('flat', 'per_unit', 'usage', 'flat_plus_overage')),
  flat_amount       bigint CHECK (flat_amount IS NULL OR flat_amount >= 0),
  unit_amount       numeric(24, 10) CHECK (unit_amount IS NULL OR unit_amount >= 0),
  metric_id         uuid,
  included_quantity numeric(20, 6) CHECK (included_quantity IS NULL OR included_quantity >= 0),
  currency          char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  created_by        uuid REFERENCES users (id),
  created_at        timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT plan_price_versions_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT plan_price_versions_plan_version_key UNIQUE (tenant_id, plan_id, version),
  CONSTRAINT plan_price_versions_plan_fk FOREIGN KEY (tenant_id, plan_id) REFERENCES plans (tenant_id, id),
  CONSTRAINT plan_price_versions_metric_fk FOREIGN KEY (tenant_id, metric_id) REFERENCES billable_metrics (tenant_id, id),
  CONSTRAINT plan_price_versions_model_consistency CHECK (
    (pricing_model = 'flat' AND flat_amount IS NOT NULL AND unit_amount IS NULL AND metric_id IS NULL) OR
    (pricing_model = 'per_unit' AND unit_amount IS NOT NULL AND flat_amount IS NULL AND metric_id IS NULL) OR
    (pricing_model = 'usage' AND unit_amount IS NOT NULL AND metric_id IS NOT NULL AND flat_amount IS NULL) OR
    (pricing_model = 'flat_plus_overage' AND flat_amount IS NOT NULL AND unit_amount IS NOT NULL
       AND metric_id IS NOT NULL AND included_quantity IS NOT NULL))
);
CREATE TRIGGER plan_price_versions_immutable BEFORE UPDATE OR DELETE ON plan_price_versions FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('plan_price_versions');
GRANT SELECT, INSERT ON plan_price_versions TO recorvia_app;

ALTER TABLE plans ADD CONSTRAINT plans_current_price_fk
  FOREIGN KEY (tenant_id, current_price_version_id) REFERENCES plan_price_versions (tenant_id, id)
  DEFERRABLE INITIALLY DEFERRED;

-- +goose Down
ALTER TABLE plans DROP CONSTRAINT IF EXISTS plans_current_price_fk;
DROP TABLE IF EXISTS plan_price_versions;
DROP TABLE IF EXISTS plans;
DROP TABLE IF EXISTS billable_metrics;
DROP TABLE IF EXISTS products;
DROP TABLE IF EXISTS customer_provider_refs;
DROP TABLE IF EXISTS customer_billing_addresses;
DROP TABLE IF EXISTS customer_contacts;
DROP TABLE IF EXISTS customers;
