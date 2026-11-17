-- Infraestrutura assíncrona (outbox, inbox, dead letters, idempotência,
-- agendamentos) e trilha de auditoria imutável.

-- +goose Up

CREATE TABLE outbox_events (
  id               uuid PRIMARY KEY,
  tenant_id        uuid NOT NULL REFERENCES tenants (id),
  event_type       text NOT NULL CHECK (event_type ~ '^[a-z_]+\.[a-z_.]+$'),
  aggregate_type   text NOT NULL,
  aggregate_id     uuid NOT NULL,
  payload          jsonb NOT NULL,
  headers          jsonb NOT NULL DEFAULT '{}',
  occurred_at      timestamptz NOT NULL,
  available_at     timestamptz NOT NULL DEFAULT now(),
  attempts         integer NOT NULL DEFAULT 0,
  claimed_by       text,
  claim_expires_at timestamptz,
  published_at     timestamptz,
  last_error       text
);
CREATE INDEX outbox_events_pending_idx ON outbox_events (available_at, id) WHERE published_at IS NULL;
CREATE INDEX outbox_events_published_idx ON outbox_events (published_at) WHERE published_at IS NOT NULL;
SELECT app_enable_tenant_rls('outbox_events');
GRANT SELECT, INSERT ON outbox_events TO recorvia_app;

-- +goose StatementBegin
-- app_outbox_claim reivindica eventos pendentes com lease. SKIP LOCKED evita
-- que dois publicadores peguem o mesmo evento; um lease expirado (publicador
-- interrompido) torna o evento elegível novamente.
CREATE FUNCTION app_outbox_claim(p_worker text, p_limit integer, p_lease_seconds integer)
RETURNS SETOF outbox_events
LANGUAGE sql VOLATILE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  UPDATE outbox_events o
     SET claimed_by = p_worker,
         claim_expires_at = clock_timestamp() + make_interval(secs => p_lease_seconds),
         attempts = o.attempts + 1
   WHERE o.id IN (
     SELECT id FROM outbox_events
      WHERE published_at IS NULL
        AND available_at <= clock_timestamp()
        AND (claim_expires_at IS NULL OR claim_expires_at < clock_timestamp())
      ORDER BY available_at, id
      LIMIT p_limit
      FOR UPDATE SKIP LOCKED)
  RETURNING o.*
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_outbox_mark_published(p_worker text, p_ids uuid[])
RETURNS integer
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE n integer;
BEGIN
  UPDATE outbox_events
     SET published_at = clock_timestamp(), claimed_by = NULL, claim_expires_at = NULL, last_error = NULL
   WHERE id = ANY (p_ids) AND claimed_by = p_worker AND published_at IS NULL;
  GET DIAGNOSTICS n = ROW_COUNT;
  RETURN n;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_outbox_release(p_worker text, p_id uuid, p_delay_seconds integer, p_error text)
RETURNS void
LANGUAGE sql VOLATILE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  UPDATE outbox_events
     SET claimed_by = NULL, claim_expires_at = NULL,
         available_at = clock_timestamp() + make_interval(secs => p_delay_seconds),
         last_error = left(p_error, 1000)
   WHERE id = p_id AND claimed_by = p_worker AND published_at IS NULL
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Métricas agregadas da outbox (sem identificação de tenant).
CREATE FUNCTION app_outbox_stats()
RETURNS TABLE (pending bigint, oldest_pending_seconds double precision, max_attempts integer)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT count(*),
         COALESCE(EXTRACT(EPOCH FROM clock_timestamp() - min(occurred_at)), 0)::double precision,
         COALESCE(max(attempts), 0)
  FROM outbox_events WHERE published_at IS NULL
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Retenção: remove eventos publicados há mais de p_keep_days dias.
CREATE FUNCTION app_outbox_purge(p_keep_days integer)
RETURNS integer
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE n integer;
BEGIN
  DELETE FROM outbox_events WHERE published_at < clock_timestamp() - make_interval(days => p_keep_days);
  GET DIAGNOSTICS n = ROW_COUNT;
  RETURN n;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION app_outbox_claim(text, integer, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION app_outbox_mark_published(text, uuid[]) FROM PUBLIC;
REVOKE ALL ON FUNCTION app_outbox_release(text, uuid, integer, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION app_outbox_stats() FROM PUBLIC;
REVOKE ALL ON FUNCTION app_outbox_purge(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_outbox_claim(text, integer, integer) TO recorvia_app;
GRANT EXECUTE ON FUNCTION app_outbox_mark_published(text, uuid[]) TO recorvia_app;
GRANT EXECUTE ON FUNCTION app_outbox_release(text, uuid, integer, text) TO recorvia_app;
GRANT EXECUTE ON FUNCTION app_outbox_stats() TO recorvia_app;
GRANT EXECUTE ON FUNCTION app_outbox_purge(integer) TO recorvia_app;

-- Inbox: deduplicação persistente por consumidor. Gravado na mesma transação
-- dos efeitos locais; o ack ao broker ocorre após o commit.
CREATE TABLE inbox_messages (
  tenant_id    uuid NOT NULL REFERENCES tenants (id),
  consumer     text NOT NULL,
  message_id   uuid NOT NULL,
  processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, consumer, message_id)
);
SELECT app_enable_tenant_rls('inbox_messages');
GRANT SELECT, INSERT, DELETE ON inbox_messages TO recorvia_app;

CREATE TABLE dead_letters (
  id              uuid PRIMARY KEY,
  tenant_id       uuid REFERENCES tenants (id),
  consumer        text NOT NULL,
  message_id      text NOT NULL,
  event_type      text,
  payload         jsonb,
  headers         jsonb NOT NULL DEFAULT '{}',
  error           text NOT NULL,
  attempts        integer NOT NULL DEFAULT 0,
  status          text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'replayed', 'discarded')),
  failed_at       timestamptz NOT NULL DEFAULT now(),
  resolved_at     timestamptz,
  resolved_by     uuid REFERENCES users (id),
  resolution_note text,
  CONSTRAINT dead_letters_message_key UNIQUE (consumer, message_id)
);
CREATE INDEX dead_letters_tenant_idx ON dead_letters (tenant_id, failed_at DESC, id DESC);
-- Linhas sem tenant (mensagem sem identificação válida) não são visíveis à
-- aplicação: a política compara com o tenant corrente e NULL nega acesso.
SELECT app_enable_tenant_rls('dead_letters');
GRANT SELECT, INSERT, UPDATE ON dead_letters TO recorvia_app;

-- +goose StatementBegin
CREATE FUNCTION app_dead_letter_record_untenanted(p_id uuid, p_consumer text, p_message_id text,
  p_event_type text, p_headers jsonb, p_error text, p_attempts integer)
RETURNS void
LANGUAGE sql VOLATILE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  INSERT INTO dead_letters (id, tenant_id, consumer, message_id, event_type, headers, error, attempts)
  VALUES (p_id, NULL, p_consumer, p_message_id, p_event_type, p_headers, left(p_error, 2000), p_attempts)
  ON CONFLICT (consumer, message_id) DO NOTHING
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_dead_letter_record_untenanted(uuid, text, text, text, jsonb, text, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_dead_letter_record_untenanted(uuid, text, text, text, jsonb, text, integer) TO recorvia_app;

-- Chaves de idempotência por tenant, cliente da API e operação.
CREATE TABLE idempotency_keys (
  tenant_id             uuid NOT NULL REFERENCES tenants (id),
  client_id             text NOT NULL,
  operation             text NOT NULL,
  idempotency_key       text NOT NULL CHECK (length(idempotency_key) BETWEEN 8 AND 255),
  request_hash          bytea NOT NULL,
  status                text NOT NULL CHECK (status IN ('in_progress', 'completed')),
  response_status       integer,
  response_body         bytea,
  response_content_type text,
  locked_until          timestamptz,
  created_at            timestamptz NOT NULL DEFAULT now(),
  completed_at          timestamptz,
  expires_at            timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, client_id, operation, idempotency_key),
  CHECK (status = 'in_progress' OR response_status IS NOT NULL)
);
CREATE INDEX idempotency_keys_expiry_idx ON idempotency_keys (expires_at);
SELECT app_enable_tenant_rls('idempotency_keys');
GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO recorvia_app;

-- +goose StatementBegin
CREATE FUNCTION app_idempotency_purge_expired()
RETURNS integer
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE n integer;
BEGIN
  DELETE FROM idempotency_keys WHERE expires_at < clock_timestamp() AND status = 'completed';
  GET DIAGNOSTICS n = ROW_COUNT;
  RETURN n;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_idempotency_purge_expired() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_idempotency_purge_expired() TO recorvia_app;

-- Agendamentos globais (sem dados de tenant). Cada instância do worker
-- reivindica tarefas vencidas com lease.
CREATE TABLE scheduled_jobs (
  name             text PRIMARY KEY,
  interval_seconds integer NOT NULL CHECK (interval_seconds > 0),
  next_run_at      timestamptz NOT NULL,
  locked_by        text,
  locked_until     timestamptz,
  last_started_at  timestamptz,
  last_finished_at timestamptz,
  last_error       text,
  run_count        bigint NOT NULL DEFAULT 0
);
GRANT SELECT, INSERT, UPDATE ON scheduled_jobs TO recorvia_app;

-- Trilha de auditoria imutável. Não armazena valores pessoais em details:
-- registra identificadores e nomes de campos alterados.
CREATE TABLE audit_log (
  id            uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL REFERENCES tenants (id),
  occurred_at   timestamptz NOT NULL,
  actor_type    text NOT NULL CHECK (actor_type IN ('user', 'api_credential', 'system', 'customer_portal', 'provider')),
  actor_id      text,
  actor_label   text,
  action        text NOT NULL,
  resource_type text NOT NULL,
  resource_id   text,
  request_id    text,
  details       jsonb NOT NULL DEFAULT '{}',
  CONSTRAINT audit_log_tenant_id_key UNIQUE (tenant_id, id)
);
CREATE INDEX audit_log_tenant_time_idx ON audit_log (tenant_id, occurred_at DESC, id DESC);
CREATE INDEX audit_log_resource_idx ON audit_log (tenant_id, resource_type, resource_id, occurred_at DESC);
CREATE TRIGGER audit_log_immutable BEFORE UPDATE OR DELETE ON audit_log FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('audit_log');
GRANT SELECT, INSERT ON audit_log TO recorvia_app;

-- +goose Down
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS scheduled_jobs;
DROP FUNCTION IF EXISTS app_idempotency_purge_expired();
DROP TABLE IF EXISTS idempotency_keys;
DROP FUNCTION IF EXISTS app_dead_letter_record_untenanted(uuid, text, text, text, jsonb, text, integer);
DROP TABLE IF EXISTS dead_letters;
DROP TABLE IF EXISTS inbox_messages;
DROP FUNCTION IF EXISTS app_outbox_purge(integer);
DROP FUNCTION IF EXISTS app_outbox_stats();
DROP FUNCTION IF EXISTS app_outbox_release(text, uuid, integer, text);
DROP FUNCTION IF EXISTS app_outbox_mark_published(text, uuid[]);
DROP FUNCTION IF EXISTS app_outbox_claim(text, integer, integer);
DROP TABLE IF EXISTS outbox_events;
