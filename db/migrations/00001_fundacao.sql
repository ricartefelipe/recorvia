-- Fundação: funções de contexto de tenant e catálogo de organizações.
--
-- Papéis (criados pelo bootstrap, fora das migrations):
--   recorvia_owner  dono das tabelas; executa migrations e funções SECURITY DEFINER.
--   recorvia_app    papel da aplicação: NOSUPERUSER, NOBYPASSRLS, sem posse de tabelas.

-- +goose Up

-- +goose StatementBegin
-- app_current_tenant retorna o tenant definido na transação corrente ou NULL.
-- current_setting(..., true) devolve texto vazio quando a variável já existiu
-- na sessão; NULLIF converte em NULL para que comparações neguem acesso.
CREATE FUNCTION app_current_tenant() RETURNS uuid
LANGUAGE sql STABLE PARALLEL SAFE
AS $$ SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_touch_updated_at() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- app_reject_mutation impede UPDATE e DELETE em tabelas imutáveis.
CREATE FUNCTION app_reject_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'tabela % é imutável: operação % não permitida', TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'P0001';
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION app_current_tenant() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_current_tenant() TO recorvia_app;

CREATE TABLE tenants (
  id                 uuid PRIMARY KEY,
  slug               text NOT NULL,
  name               text NOT NULL CHECK (length(name) BETWEEN 2 AND 200),
  legal_name         text CHECK (legal_name IS NULL OR length(legal_name) <= 200),
  document_number    text CHECK (document_number IS NULL OR document_number ~ '^[0-9]{14}$'),
  timezone           text NOT NULL DEFAULT 'America/Sao_Paulo',
  currency           char(3) NOT NULL DEFAULT 'BRL' CHECK (currency = 'BRL'),
  invoice_prefix     text NOT NULL DEFAULT 'FAT' CHECK (invoice_prefix ~ '^[A-Z0-9]{1,10}$'),
  payment_terms_days integer NOT NULL DEFAULT 5 CHECK (payment_terms_days BETWEEN 0 AND 120),
  status             text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'closed')),
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  version            integer NOT NULL DEFAULT 1,
  CONSTRAINT tenants_slug_key UNIQUE (slug),
  CONSTRAINT tenants_slug_format CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$')
);
CREATE TRIGGER tenants_touch BEFORE UPDATE ON tenants FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();

-- A organização só é visível dentro do próprio contexto. A criação no
-- onboarding define o contexto com o novo identificador antes do INSERT.
ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenants_isolation ON tenants
  USING (id = app_current_tenant())
  WITH CHECK (id = app_current_tenant());

GRANT SELECT, INSERT, UPDATE ON tenants TO recorvia_app;

-- +goose StatementBegin
-- app_tenant_slug_available permite verificar disponibilidade de slug no
-- onboarding sem expor outras organizações.
CREATE FUNCTION app_tenant_slug_available(p_slug text) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$ SELECT NOT EXISTS (SELECT 1 FROM tenants WHERE slug = p_slug) $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_tenant_slug_available(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_tenant_slug_available(text) TO recorvia_app;

-- +goose Down
DROP FUNCTION IF EXISTS app_tenant_slug_available(text);
DROP TABLE IF EXISTS tenants;
DROP FUNCTION IF EXISTS app_reject_mutation();
DROP FUNCTION IF EXISTS app_touch_updated_at();
DROP FUNCTION IF EXISTS app_current_tenant();
