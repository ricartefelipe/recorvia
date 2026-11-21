-- Endereço da organização (RF-06, ADR-084): o emitente do documento da fatura
-- passa a ter endereço, no mesmo formato e com as mesmas regras do endereço de
-- cobrança do cliente (customer_billing_addresses). Esta tabela guarda o
-- endereço vigente, editado no perfil da organização; a emissão copia o
-- endereço para invoices.issuer_snapshot, que não muda depois. Faturas
-- emitidas antes desta migration continuam com o snapshot que têm, sem
-- endereço.

-- +goose Up
CREATE TABLE tenant_addresses (
  tenant_id   uuid PRIMARY KEY,
  street      text NOT NULL CHECK (length(street) BETWEEN 1 AND 200),
  number      text NOT NULL CHECK (length(number) BETWEEN 1 AND 20),
  complement  text NOT NULL DEFAULT '' CHECK (length(complement) <= 100),
  district    text NOT NULL CHECK (length(district) BETWEEN 1 AND 100),
  city        text NOT NULL CHECK (length(city) BETWEEN 1 AND 100),
  state       char(2) NOT NULL CHECK (state ~ '^[A-Z]{2}$'),
  postal_code text NOT NULL CHECK (postal_code ~ '^[0-9]{8}$'),
  country     char(2) NOT NULL DEFAULT 'BR' CHECK (country ~ '^[A-Z]{2}$'),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT tenant_addresses_tenant_fk FOREIGN KEY (tenant_id) REFERENCES tenants (id)
);
SELECT app_enable_tenant_rls('tenant_addresses');
GRANT SELECT, INSERT, UPDATE ON tenant_addresses TO recorvia_app;

-- +goose Down
DROP TABLE IF EXISTS tenant_addresses;
