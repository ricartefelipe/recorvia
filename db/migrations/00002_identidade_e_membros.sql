-- Identidade, associação a organizações e convites.
--
-- Exceções globais (sem tenant_id): users e platform_staff. Um usuário pode
-- participar de várias organizações; a associação (memberships) é do tenant.

-- +goose Up

-- +goose StatementBegin
-- app_enable_tenant_rls habilita RLS com a política padrão de isolamento.
-- Usada apenas por migrations (executadas por recorvia_owner).
CREATE FUNCTION app_enable_tenant_rls(p_table regclass) RETURNS void
LANGUAGE plpgsql
AS $$
BEGIN
  EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY', p_table);
  EXECUTE format(
    'CREATE POLICY tenant_isolation ON %s USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant())',
    p_table);
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_enable_tenant_rls(regclass) FROM PUBLIC;

CREATE TABLE users (
  id            uuid PRIMARY KEY,
  oidc_issuer   text NOT NULL,
  oidc_subject  text NOT NULL,
  email         text NOT NULL CHECK (length(email) BETWEEN 3 AND 320),
  display_name  text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 200),
  status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  last_login_at timestamptz,
  CONSTRAINT users_oidc_identity_key UNIQUE (oidc_issuer, oidc_subject)
);
CREATE TRIGGER users_touch BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
GRANT SELECT, INSERT, UPDATE ON users TO recorvia_app;

CREATE TABLE memberships (
  tenant_id  uuid NOT NULL REFERENCES tenants (id),
  user_id    uuid NOT NULL REFERENCES users (id),
  role       text NOT NULL CHECK (role IN ('owner', 'admin', 'finance', 'operator', 'auditor')),
  status     text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'removed')),
  invited_by uuid REFERENCES users (id),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  version    integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, user_id)
);
CREATE INDEX memberships_user_idx ON memberships (user_id) WHERE status = 'active';
CREATE TRIGGER memberships_touch BEFORE UPDATE ON memberships FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
SELECT app_enable_tenant_rls('memberships');
GRANT SELECT, INSERT, UPDATE ON memberships TO recorvia_app;

CREATE TABLE invitations (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL REFERENCES tenants (id),
  email       text NOT NULL CHECK (length(email) BETWEEN 3 AND 320),
  role        text NOT NULL CHECK (role IN ('owner', 'admin', 'finance', 'operator', 'auditor')),
  token_hash  bytea NOT NULL,
  invited_by  uuid NOT NULL REFERENCES users (id),
  expires_at  timestamptz NOT NULL,
  accepted_at timestamptz,
  accepted_by uuid REFERENCES users (id),
  revoked_at  timestamptz,
  revoked_by  uuid REFERENCES users (id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT invitations_token_hash_key UNIQUE (token_hash),
  CONSTRAINT invitations_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT invitations_single_outcome CHECK (accepted_at IS NULL OR revoked_at IS NULL)
);
CREATE INDEX invitations_tenant_created_idx ON invitations (tenant_id, created_at DESC, id DESC);
SELECT app_enable_tenant_rls('invitations');
GRANT SELECT, INSERT, UPDATE ON invitations TO recorvia_app;

-- Membros da equipe da plataforma. Não concede acesso a dados de tenants:
-- apenas ao catálogo de organizações sem dados financeiros.
CREATE TABLE platform_staff (
  user_id    uuid PRIMARY KEY REFERENCES users (id),
  role       text NOT NULL CHECK (role IN ('support', 'operations')),
  created_at timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT ON platform_staff TO recorvia_app;

-- +goose StatementBegin
-- app_user_memberships lista as organizações ativas de um usuário. Retorna
-- somente catálogo (nome, slug, papel) e é chamada com o usuário autenticado.
CREATE FUNCTION app_user_memberships(p_user uuid)
RETURNS TABLE (tenant_id uuid, tenant_name text, tenant_slug text, role text, tenant_status text)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT m.tenant_id, t.name, t.slug, m.role, t.status
  FROM memberships m
  JOIN tenants t ON t.id = m.tenant_id
  WHERE m.user_id = p_user AND m.status = 'active'
  ORDER BY t.name, t.id
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_user_memberships(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_user_memberships(uuid) TO recorvia_app;

-- +goose StatementBegin
-- app_find_invitation localiza o tenant de um convite pelo hash do token,
-- sem expor outros dados; o restante é lido dentro do contexto do tenant.
CREATE FUNCTION app_find_invitation(p_token_hash bytea)
RETURNS TABLE (invitation_id uuid, tenant_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$ SELECT id, tenant_id FROM invitations WHERE token_hash = p_token_hash $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_find_invitation(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_find_invitation(bytea) TO recorvia_app;

-- +goose Down
DROP FUNCTION IF EXISTS app_find_invitation(bytea);
DROP FUNCTION IF EXISTS app_user_memberships(uuid);
DROP TABLE IF EXISTS platform_staff;
DROP TABLE IF EXISTS invitations;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS users;
DROP FUNCTION IF EXISTS app_enable_tenant_rls(regclass);
