-- Credenciais de API (máquina a máquina) e links do portal do cliente.
--
-- Somente hashes SHA-256 são persistidos: do segredo da credencial (o token
-- rcv_<identificador>_<segredo> é exibido uma única vez) e do token do link
-- do portal. Funções SECURITY DEFINER localizam o tenant pelo identificador
-- público da credencial ou pelo hash do link, sem expor outros dados; a
-- verificação completa ocorre depois, dentro da transação do tenant.

-- +goose Up

-- Credenciais de API vinculadas a uma organização. Os escopos são um
-- subconjunto das permissões de quem emite (validado no caso de uso). A
-- rotação troca o segredo e mantém o anterior válido até
-- previous_secret_expires_at (janela de transição); a revogação invalida os
-- dois imediatamente.
CREATE TABLE api_credentials (
  id                         uuid PRIMARY KEY,
  tenant_id                  uuid NOT NULL REFERENCES tenants (id),
  name                       text NOT NULL CHECK (length(name) BETWEEN 3 AND 100),
  key_id                     text NOT NULL CHECK (key_id ~ '^[a-z2-7]{16}$'),
  secret_hash                bytea NOT NULL CHECK (length(secret_hash) = 32),
  previous_secret_hash       bytea CHECK (previous_secret_hash IS NULL OR length(previous_secret_hash) = 32),
  previous_secret_expires_at timestamptz,
  scopes                     text[] NOT NULL CHECK (
    cardinality(scopes) BETWEEN 1 AND 64 AND
    array_to_string(scopes, ',') ~ '^[a-z_]+\.[a-z_]+(,[a-z_]+\.[a-z_]+)*$'),
  created_by                 uuid NOT NULL REFERENCES users (id),
  created_at                 timestamptz NOT NULL,
  expires_at                 timestamptz,
  rotated_by                 uuid REFERENCES users (id),
  rotated_at                 timestamptz,
  last_used_at               timestamptz,
  revoked_by                 uuid REFERENCES users (id),
  revoked_at                 timestamptz,
  version                    integer NOT NULL DEFAULT 1,
  CONSTRAINT api_credentials_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT api_credentials_key_id_key UNIQUE (key_id),
  CONSTRAINT api_credentials_expiry CHECK (expires_at IS NULL OR expires_at > created_at),
  CONSTRAINT api_credentials_rotation CHECK ((rotated_at IS NULL) = (rotated_by IS NULL)),
  CONSTRAINT api_credentials_previous_secret CHECK (
    (previous_secret_hash IS NULL) = (previous_secret_expires_at IS NULL) AND
    (previous_secret_hash IS NULL OR (rotated_at IS NOT NULL AND previous_secret_hash <> secret_hash))),
  CONSTRAINT api_credentials_revocation CHECK (revoked_by IS NULL OR revoked_at IS NOT NULL)
);
CREATE INDEX api_credentials_tenant_created_idx ON api_credentials (tenant_id, created_at DESC, id DESC);

-- +goose StatementBegin
-- app_api_credentials_guard mantém imutáveis a identificação, os escopos, a
-- autoria e a validade da credencial, e impede alterar credencial revogada
-- (inclusive desfazer a revogação).
CREATE FUNCTION app_api_credentials_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'api_credentials: credenciais não são excluídas, apenas revogadas' USING ERRCODE = 'P0001';
  END IF;
  IF OLD.revoked_at IS NOT NULL THEN
    RAISE EXCEPTION 'api_credentials: credencial revogada não pode ser alterada' USING ERRCODE = 'P0001';
  END IF;
  IF NEW.tenant_id <> OLD.tenant_id OR NEW.key_id <> OLD.key_id OR NEW.name <> OLD.name OR NEW.scopes <> OLD.scopes
     OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at
     OR NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
    RAISE EXCEPTION 'api_credentials: identificação, escopos, autoria e validade são imutáveis' USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER api_credentials_guard BEFORE UPDATE OR DELETE ON api_credentials FOR EACH ROW EXECUTE FUNCTION app_api_credentials_guard();
SELECT app_enable_tenant_rls('api_credentials');
GRANT SELECT, INSERT, UPDATE ON api_credentials TO recorvia_app;

-- +goose StatementBegin
-- app_find_api_credential localiza a credencial e o tenant pelo
-- identificador público do token. Não devolve o hash do segredo nem outros
-- dados: a verificação ocorre na transação do tenant.
CREATE FUNCTION app_find_api_credential(p_key_id text)
RETURNS TABLE (credential_id uuid, tenant_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$ SELECT id, tenant_id FROM api_credentials WHERE key_id = p_key_id $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_find_api_credential(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_find_api_credential(text) TO recorvia_app;

-- Links temporários de uso único do portal do cliente. Cada cliente tem no
-- máximo um link não revogado: gerar um novo link revoga o anterior
-- (pendente ou já trocado por sessão), o que também encerra a sessão aberta
-- com ele. O autor é um membro ou uma credencial de API do próprio tenant.
CREATE TABLE customer_portal_links (
  id                       uuid PRIMARY KEY,
  tenant_id                uuid NOT NULL REFERENCES tenants (id),
  customer_id              uuid NOT NULL,
  token_hash               bytea NOT NULL CHECK (length(token_hash) = 32),
  created_by_user_id       uuid REFERENCES users (id),
  created_by_credential_id uuid,
  created_at               timestamptz NOT NULL,
  expires_at               timestamptz NOT NULL,
  used_at                  timestamptz,
  revoked_at               timestamptz,
  revoke_reason            text CHECK (revoke_reason IS NULL OR revoke_reason IN ('superseded', 'revoked')),
  CONSTRAINT customer_portal_links_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT customer_portal_links_token_hash_key UNIQUE (token_hash),
  CONSTRAINT customer_portal_links_customer_fk FOREIGN KEY (tenant_id, customer_id) REFERENCES customers (tenant_id, id),
  CONSTRAINT customer_portal_links_credential_fk FOREIGN KEY (tenant_id, created_by_credential_id) REFERENCES api_credentials (tenant_id, id),
  CONSTRAINT customer_portal_links_creator CHECK (num_nonnulls(created_by_user_id, created_by_credential_id) = 1),
  CONSTRAINT customer_portal_links_expiry CHECK (expires_at > created_at),
  CONSTRAINT customer_portal_links_use_window CHECK (used_at IS NULL OR (used_at >= created_at AND used_at < expires_at)),
  CONSTRAINT customer_portal_links_revocation CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL))
);
CREATE UNIQUE INDEX customer_portal_links_active_key ON customer_portal_links (tenant_id, customer_id) WHERE revoked_at IS NULL;
CREATE INDEX customer_portal_links_customer_idx ON customer_portal_links (tenant_id, customer_id, created_at DESC, id DESC);

-- +goose StatementBegin
-- app_portal_links_guard permite apenas registrar o uso (uma vez) e a
-- revogação (uma vez); token, cliente, autoria e validade são imutáveis.
CREATE FUNCTION app_portal_links_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'customer_portal_links: links não são excluídos, apenas revogados' USING ERRCODE = 'P0001';
  END IF;
  IF NEW.tenant_id <> OLD.tenant_id OR NEW.customer_id <> OLD.customer_id OR NEW.token_hash <> OLD.token_hash
     OR NEW.created_by_user_id IS DISTINCT FROM OLD.created_by_user_id
     OR NEW.created_by_credential_id IS DISTINCT FROM OLD.created_by_credential_id
     OR NEW.created_at <> OLD.created_at OR NEW.expires_at <> OLD.expires_at
     OR (OLD.used_at IS NOT NULL AND NEW.used_at IS DISTINCT FROM OLD.used_at)
     OR (OLD.revoked_at IS NOT NULL AND (NEW.revoked_at IS DISTINCT FROM OLD.revoked_at
                                         OR NEW.revoke_reason IS DISTINCT FROM OLD.revoke_reason
                                         OR NEW.used_at IS DISTINCT FROM OLD.used_at)) THEN
    RAISE EXCEPTION 'customer_portal_links: alteração não permitida' USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER customer_portal_links_guard BEFORE UPDATE OR DELETE ON customer_portal_links FOR EACH ROW EXECUTE FUNCTION app_portal_links_guard();
SELECT app_enable_tenant_rls('customer_portal_links');
GRANT SELECT, INSERT, UPDATE ON customer_portal_links TO recorvia_app;

-- +goose StatementBegin
-- app_find_portal_link localiza o link e o tenant pelo hash do token, sem
-- expor outros dados; situação, cliente e validade são lidos no contexto do
-- tenant.
CREATE FUNCTION app_find_portal_link(p_token_hash bytea)
RETURNS TABLE (link_id uuid, tenant_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$ SELECT id, tenant_id FROM customer_portal_links WHERE token_hash = p_token_hash $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_find_portal_link(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_find_portal_link(bytea) TO recorvia_app;

-- +goose Down
DROP FUNCTION IF EXISTS app_find_portal_link(bytea);
DROP TABLE IF EXISTS customer_portal_links;
DROP FUNCTION IF EXISTS app_portal_links_guard();
DROP FUNCTION IF EXISTS app_find_api_credential(text);
DROP TABLE IF EXISTS api_credentials;
DROP FUNCTION IF EXISTS app_api_credentials_guard();
