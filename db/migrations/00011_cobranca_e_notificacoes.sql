-- Cobrança e notificações: régua de cobrança por tenant, templates de e-mail,
-- notificações com identidade de negócio, inadimplência de faturas,
-- suspensões pela política e webhooks enviados com segredos cifrados,
-- entregas e histórico de tentativas.
--
-- Ordem de bloqueio (ADR-031): transações que alteram faturas e assinaturas
-- bloqueiam a fatura antes da assinatura e do cliente. Inserções com chave
-- estrangeira para faturas, assinaturas ou clientes contam como bloqueio
-- compartilhado dessas linhas.

-- +goose Up

-- Políticas de cobrança. A política padrão (no máximo uma por tenant) rege os
-- lembretes de todas as faturas em aberto. Os passos são dias relativos ao
-- vencimento (negativo = antes); o template de cada passo decorre do sinal.
-- A janela de envio é [início, fim) em horas do fuso do tenant.
CREATE TABLE collection_policies (
  id                     uuid PRIMARY KEY,
  tenant_id              uuid NOT NULL REFERENCES tenants (id),
  name                   text NOT NULL CHECK (length(name) BETWEEN 2 AND 100),
  is_default             boolean NOT NULL DEFAULT false,
  reminder_offsets_days  integer[] NOT NULL DEFAULT '{}',
  send_window_start_hour integer NOT NULL CHECK (send_window_start_hour BETWEEN 0 AND 23),
  send_window_end_hour   integer NOT NULL CHECK (send_window_end_hour BETWEEN 1 AND 24),
  max_messages_per_day   integer NOT NULL CHECK (max_messages_per_day BETWEEN 1 AND 5),
  suspend_after_days     integer CHECK (suspend_after_days IS NULL OR suspend_after_days BETWEEN 1 AND 180),
  reactivate_on_payment  boolean NOT NULL DEFAULT false,
  created_by             uuid REFERENCES users (id),
  created_at             timestamptz NOT NULL,
  updated_at             timestamptz NOT NULL,
  version                integer NOT NULL DEFAULT 1,
  CONSTRAINT collection_policies_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT collection_policies_window CHECK (send_window_start_hour < send_window_end_hour),
  CONSTRAINT collection_policies_offsets CHECK (
    cardinality(reminder_offsets_days) <= 10 AND array_position(reminder_offsets_days, NULL) IS NULL
    AND -30 <= ALL (reminder_offsets_days) AND 90 >= ALL (reminder_offsets_days))
);
CREATE UNIQUE INDEX collection_policies_default_key ON collection_policies (tenant_id) WHERE is_default;
CREATE UNIQUE INDEX collection_policies_name_key ON collection_policies (tenant_id, lower(name));
SELECT app_enable_tenant_rls('collection_policies');
GRANT SELECT, INSERT, UPDATE ON collection_policies TO recorvia_app;

-- Templates personalizados pelo tenant. Sem linha, vale o padrão do sistema.
CREATE TABLE notification_templates (
  tenant_id    uuid NOT NULL REFERENCES tenants (id),
  template_key text NOT NULL CHECK (template_key IN ('invoice_issued', 'reminder_before_due', 'reminder_after_due', 'payment_confirmed',
                                                     'subscription_suspended')),
  subject      text NOT NULL CHECK (length(subject) BETWEEN 3 AND 200),
  body         text NOT NULL CHECK (length(body) BETWEEN 10 AND 10000),
  updated_by   uuid REFERENCES users (id),
  created_at   timestamptz NOT NULL,
  updated_at   timestamptz NOT NULL,
  version      integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, template_key)
);
SELECT app_enable_tenant_rls('notification_templates');
GRANT SELECT, INSERT, UPDATE ON notification_templates TO recorvia_app;

-- Notificações por e-mail. business_key é a identidade de negócio (por
-- exemplo, lembrete de uma fatura num passo da régua): única por tenant, de
-- modo que reprocessamentos e execuções concorrentes não duplicam o envio.
-- O conteúdo renderizado fica cifrado (contém dados pessoais e, no convite,
-- o link com o token); assunto e destinatários ficam em claro para o
-- histórico. attempts funciona como token de exclusão mútua do registro do
-- resultado: só grava quem reivindicou a tentativa corrente.
CREATE TABLE notifications (
  id                 uuid PRIMARY KEY,
  tenant_id          uuid NOT NULL REFERENCES tenants (id),
  business_key       text NOT NULL CHECK (length(business_key) BETWEEN 3 AND 200),
  template_key       text NOT NULL CHECK (template_key ~ '^[a-z_]{3,50}$'),
  channel            text NOT NULL DEFAULT 'email' CHECK (channel = 'email'),
  recipients         text[] NOT NULL DEFAULT '{}',
  subject            text NOT NULL CHECK (length(subject) <= 300),
  content_ciphertext bytea,
  content_key_id     text,
  customer_id        uuid,
  invoice_id         uuid,
  subscription_id    uuid,
  invitation_id      uuid,
  policy_id          uuid,
  step_offset_days   integer CHECK (step_offset_days IS NULL OR step_offset_days BETWEEN -30 AND 90),
  status             text NOT NULL CHECK (status IN ('scheduled', 'sending', 'sent', 'failed', 'canceled', 'suppressed')),
  status_reason      text CHECK (status_reason IS NULL OR length(status_reason) <= 100),
  send_after         timestamptz NOT NULL,
  attempts           integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  max_attempts       integer NOT NULL CHECK (max_attempts BETWEEN 1 AND 20),
  lease_until        timestamptz,
  last_error         text CHECK (last_error IS NULL OR length(last_error) <= 1000),
  message_id         text CHECK (message_id IS NULL OR length(message_id) <= 300),
  sent_at            timestamptz,
  created_at         timestamptz NOT NULL,
  updated_at         timestamptz NOT NULL,
  CONSTRAINT notifications_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT notifications_business_key UNIQUE (tenant_id, business_key),
  CONSTRAINT notifications_invoice_fk FOREIGN KEY (tenant_id, invoice_id) REFERENCES invoices (tenant_id, id),
  CONSTRAINT notifications_subscription_fk FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id),
  CONSTRAINT notifications_customer_fk FOREIGN KEY (tenant_id, customer_id) REFERENCES customers (tenant_id, id),
  CONSTRAINT notifications_invitation_fk FOREIGN KEY (tenant_id, invitation_id) REFERENCES invitations (tenant_id, id),
  CONSTRAINT notifications_policy_fk FOREIGN KEY (tenant_id, policy_id) REFERENCES collection_policies (tenant_id, id),
  CONSTRAINT notifications_content CHECK ((content_ciphertext IS NULL) = (content_key_id IS NULL)),
  CONSTRAINT notifications_sent CHECK ((status = 'sent') = (sent_at IS NOT NULL)),
  CONSTRAINT notifications_sending_lease CHECK (status <> 'sending' OR lease_until IS NOT NULL),
  CONSTRAINT notifications_deliverable CHECK (status NOT IN ('scheduled', 'sending', 'sent')
    OR (content_ciphertext IS NOT NULL AND cardinality(recipients) > 0)),
  CONSTRAINT notifications_reason CHECK (status NOT IN ('canceled', 'suppressed') OR status_reason IS NOT NULL),
  CONSTRAINT notifications_step CHECK ((step_offset_days IS NULL) = (policy_id IS NULL)),
  CONSTRAINT notifications_recipients CHECK (cardinality(recipients) <= 20 AND array_position(recipients, NULL) IS NULL)
);
CREATE INDEX notifications_due_idx ON notifications (send_after) WHERE status = 'scheduled';
CREATE INDEX notifications_lease_idx ON notifications (lease_until) WHERE status = 'sending';
CREATE INDEX notifications_tenant_created_idx ON notifications (tenant_id, created_at DESC, id DESC);
CREATE INDEX notifications_invoice_idx ON notifications (tenant_id, invoice_id, created_at) WHERE invoice_id IS NOT NULL;
CREATE INDEX notifications_customer_sent_idx ON notifications (tenant_id, customer_id, sent_at)
  WHERE customer_id IS NOT NULL AND policy_id IS NOT NULL AND status IN ('sending', 'sent');
SELECT app_enable_tenant_rls('notifications');
GRANT SELECT, INSERT, UPDATE ON notifications TO recorvia_app;

-- Histórico imutável de cada notificação (agendamento, adiamentos, tentativas,
-- envio, falha, cancelamento e supressão, com o motivo).
CREATE TABLE notification_events (
  id              uuid PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  notification_id uuid NOT NULL,
  status          text NOT NULL CHECK (status IN ('scheduled', 'sending', 'sent', 'failed', 'canceled', 'suppressed')),
  reason          text NOT NULL CHECK (length(reason) BETWEEN 1 AND 100),
  attempt         integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
  detail          text CHECK (detail IS NULL OR length(detail) <= 1000),
  occurred_at     timestamptz NOT NULL,
  CONSTRAINT notification_events_notification_fk FOREIGN KEY (tenant_id, notification_id) REFERENCES notifications (tenant_id, id)
);
CREATE INDEX notification_events_notification_idx ON notification_events (tenant_id, notification_id, occurred_at, id);
CREATE TRIGGER notification_events_immutable BEFORE UPDATE OR DELETE ON notification_events
  FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('notification_events');
GRANT SELECT, INSERT ON notification_events TO recorvia_app;

-- Inadimplência por fatura: registra quando a fatura passou a vencida (uma
-- única vez, o que torna o evento invoice.overdue idempotente) e quando foi
-- resolvida.
CREATE TABLE invoice_delinquencies (
  tenant_id       uuid NOT NULL,
  invoice_id      uuid NOT NULL,
  subscription_id uuid,
  due_at          timestamptz NOT NULL,
  detected_at     timestamptz NOT NULL,
  resolved_at     timestamptz,
  resolution      text CHECK (resolution IS NULL OR resolution IN ('paid', 'credited', 'void', 'uncollectible')),
  PRIMARY KEY (tenant_id, invoice_id),
  CONSTRAINT invoice_delinquencies_invoice_fk FOREIGN KEY (tenant_id, invoice_id) REFERENCES invoices (tenant_id, id),
  CONSTRAINT invoice_delinquencies_subscription_fk FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id),
  CONSTRAINT invoice_delinquencies_resolution CHECK ((resolved_at IS NULL) = (resolution IS NULL)),
  CONSTRAINT invoice_delinquencies_order CHECK (detected_at >= due_at AND (resolved_at IS NULL OR resolved_at >= detected_at))
);
CREATE INDEX invoice_delinquencies_open_idx ON invoice_delinquencies (tenant_id, invoice_id) WHERE resolved_at IS NULL;
SELECT app_enable_tenant_rls('invoice_delinquencies');
GRANT SELECT, INSERT, UPDATE ON invoice_delinquencies TO recorvia_app;

-- Suspensões de assinatura aplicadas pela política de cobrança. Somente estas
-- são reativadas automaticamente; uma suspensão manual não tem registro aqui.
-- Uma reativação manual (inclusive seguida de nova suspensão manual) ou o
-- cancelamento encerram o registro (released) na revisão seguinte da
-- assinatura pela régua.
CREATE TABLE collection_suspensions (
  id              uuid PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  subscription_id uuid NOT NULL,
  policy_id       uuid NOT NULL,
  invoice_id      uuid NOT NULL,
  days_overdue    integer NOT NULL CHECK (days_overdue >= 1),
  status          text NOT NULL CHECK (status IN ('active', 'reactivated', 'released')),
  suspended_at    timestamptz NOT NULL,
  ended_at        timestamptz,
  end_reason      text CHECK (end_reason IS NULL OR end_reason IN ('payment_regularized', 'manual_reactivation', 'subscription_canceled')),
  CONSTRAINT collection_suspensions_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT collection_suspensions_subscription_fk FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id),
  CONSTRAINT collection_suspensions_policy_fk FOREIGN KEY (tenant_id, policy_id) REFERENCES collection_policies (tenant_id, id),
  CONSTRAINT collection_suspensions_invoice_fk FOREIGN KEY (tenant_id, invoice_id) REFERENCES invoices (tenant_id, id),
  CONSTRAINT collection_suspensions_end CHECK ((status = 'active') = (ended_at IS NULL) AND (ended_at IS NULL) = (end_reason IS NULL)),
  CONSTRAINT collection_suspensions_reactivated CHECK (status <> 'reactivated' OR end_reason = 'payment_regularized')
);
CREATE UNIQUE INDEX collection_suspensions_active_key ON collection_suspensions (tenant_id, subscription_id) WHERE status = 'active';
CREATE INDEX collection_suspensions_subscription_idx ON collection_suspensions (tenant_id, subscription_id, suspended_at);
SELECT app_enable_tenant_rls('collection_suspensions');
GRANT SELECT, INSERT, UPDATE ON collection_suspensions TO recorvia_app;

-- Webhooks enviados: endpoints do tenant. A remoção desativa o endpoint e o
-- retira da listagem (deleted_at), preservando o histórico de entregas.
CREATE TABLE webhook_endpoints (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL REFERENCES tenants (id),
  url         text NOT NULL CHECK (length(url) BETWEEN 8 AND 2000),
  description text NOT NULL DEFAULT '' CHECK (length(description) <= 200),
  event_types text[] NOT NULL,
  status      text NOT NULL CHECK (status IN ('active', 'disabled')),
  deleted_at  timestamptz,
  created_by  uuid REFERENCES users (id),
  created_at  timestamptz NOT NULL,
  updated_at  timestamptz NOT NULL,
  version     integer NOT NULL DEFAULT 1,
  CONSTRAINT webhook_endpoints_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT webhook_endpoints_event_types CHECK (cardinality(event_types) BETWEEN 1 AND 50 AND array_position(event_types, NULL) IS NULL),
  CONSTRAINT webhook_endpoints_deleted CHECK (deleted_at IS NULL OR status = 'disabled')
);
CREATE INDEX webhook_endpoints_tenant_idx ON webhook_endpoints (tenant_id, created_at, id) WHERE deleted_at IS NULL;
SELECT app_enable_tenant_rls('webhook_endpoints');
GRANT SELECT, INSERT, UPDATE ON webhook_endpoints TO recorvia_app;

-- Segredos de assinatura, cifrados com o keyring (AAD: tenant, endpoint e
-- segredo). expires_at nulo indica o segredo corrente; na rotação, os
-- anteriores recebem a validade da janela, durante a qual as entregas são
-- assinadas com todos os segredos válidos. A aplicação só altera a validade.
CREATE TABLE webhook_endpoint_secrets (
  id                uuid PRIMARY KEY,
  tenant_id         uuid NOT NULL,
  endpoint_id       uuid NOT NULL,
  secret_ciphertext bytea NOT NULL,
  secret_key_id     text NOT NULL,
  created_at        timestamptz NOT NULL,
  expires_at        timestamptz,
  CONSTRAINT webhook_endpoint_secrets_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT webhook_endpoint_secrets_endpoint_fk FOREIGN KEY (tenant_id, endpoint_id) REFERENCES webhook_endpoints (tenant_id, id),
  CONSTRAINT webhook_endpoint_secrets_validity CHECK (expires_at IS NULL OR expires_at >= created_at)
);
CREATE UNIQUE INDEX webhook_endpoint_secrets_current_key ON webhook_endpoint_secrets (tenant_id, endpoint_id) WHERE expires_at IS NULL;
CREATE INDEX webhook_endpoint_secrets_endpoint_idx ON webhook_endpoint_secrets (tenant_id, endpoint_id, created_at DESC);
SELECT app_enable_tenant_rls('webhook_endpoint_secrets');
GRANT SELECT, INSERT ON webhook_endpoint_secrets TO recorvia_app;
GRANT UPDATE (expires_at) ON webhook_endpoint_secrets TO recorvia_app;

-- Eventos de domínio entregues por webhook. O id é o do evento da outbox,
-- estável entre retentativas e reenvios (cabeçalho webhook-id).
CREATE TABLE webhook_events (
  id          uuid NOT NULL,
  tenant_id   uuid NOT NULL REFERENCES tenants (id),
  event_type  text NOT NULL CHECK (event_type ~ '^[a-z_]+\.[a-z_.]+$'),
  payload     jsonb NOT NULL,
  occurred_at timestamptz NOT NULL,
  created_at  timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, id)
);
CREATE TRIGGER webhook_events_immutable BEFORE UPDATE OR DELETE ON webhook_events FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('webhook_events');
GRANT SELECT, INSERT ON webhook_events TO recorvia_app;

-- Entregas: uma por endpoint e evento. attempts funciona como token de
-- exclusão mútua do registro do resultado (ver notifications).
CREATE TABLE webhook_deliveries (
  id               uuid PRIMARY KEY,
  tenant_id        uuid NOT NULL,
  endpoint_id      uuid NOT NULL,
  event_id         uuid NOT NULL,
  event_type       text NOT NULL,
  status           text NOT NULL CHECK (status IN ('pending', 'delivering', 'succeeded', 'retrying', 'exhausted')),
  attempts         integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  max_attempts     integer NOT NULL CHECK (max_attempts BETWEEN 1 AND 100),
  next_attempt_at  timestamptz,
  lease_until      timestamptz,
  last_status_code integer CHECK (last_status_code IS NULL OR last_status_code BETWEEN 100 AND 599),
  last_error       text CHECK (last_error IS NULL OR length(last_error) <= 1000),
  delivered_at     timestamptz,
  created_at       timestamptz NOT NULL,
  updated_at       timestamptz NOT NULL,
  CONSTRAINT webhook_deliveries_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT webhook_deliveries_event_key UNIQUE (tenant_id, endpoint_id, event_id),
  CONSTRAINT webhook_deliveries_endpoint_fk FOREIGN KEY (tenant_id, endpoint_id) REFERENCES webhook_endpoints (tenant_id, id),
  CONSTRAINT webhook_deliveries_event_fk FOREIGN KEY (tenant_id, event_id) REFERENCES webhook_events (tenant_id, id),
  CONSTRAINT webhook_deliveries_delivered CHECK ((status = 'succeeded') = (delivered_at IS NOT NULL)),
  CONSTRAINT webhook_deliveries_scheduled CHECK (status NOT IN ('pending', 'retrying') OR next_attempt_at IS NOT NULL),
  CONSTRAINT webhook_deliveries_lease CHECK (status <> 'delivering' OR lease_until IS NOT NULL)
);
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE status IN ('pending', 'retrying');
CREATE INDEX webhook_deliveries_lease_idx ON webhook_deliveries (lease_until) WHERE status = 'delivering';
CREATE INDEX webhook_deliveries_endpoint_idx ON webhook_deliveries (tenant_id, endpoint_id, created_at DESC, id DESC);
SELECT app_enable_tenant_rls('webhook_deliveries');
GRANT SELECT, INSERT, UPDATE ON webhook_deliveries TO recorvia_app;

-- Histórico imutável das tentativas de entrega.
CREATE TABLE webhook_delivery_attempts (
  id              uuid PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  delivery_id     uuid NOT NULL,
  attempt         integer NOT NULL CHECK (attempt >= 1),
  started_at      timestamptz NOT NULL,
  duration_ms     integer NOT NULL CHECK (duration_ms >= 0),
  status_code     integer CHECK (status_code IS NULL OR status_code BETWEEN 100 AND 599),
  error           text CHECK (error IS NULL OR length(error) <= 1000),
  signature_count integer NOT NULL CHECK (signature_count >= 0),
  CONSTRAINT webhook_delivery_attempts_delivery_fk FOREIGN KEY (tenant_id, delivery_id) REFERENCES webhook_deliveries (tenant_id, id),
  CONSTRAINT webhook_delivery_attempts_key UNIQUE (tenant_id, delivery_id, attempt)
);
CREATE TRIGGER webhook_delivery_attempts_immutable BEFORE UPDATE OR DELETE ON webhook_delivery_attempts
  FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
SELECT app_enable_tenant_rls('webhook_delivery_attempts');
GRANT SELECT, INSERT ON webhook_delivery_attempts TO recorvia_app;

-- Descoberta de trabalho (somente IDs de tenant; o processamento ocorre na
-- transação de cada tenant). As funções paginam por ID de tenant (p_after
-- exclusivo, ordem crescente), para que nenhum limite fixo deixe tenants de
-- fora.

-- +goose StatementBegin
CREATE FUNCTION app_tenants_with_due_notifications(p_now timestamptz, p_after uuid, p_limit integer)
RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT tenant_id FROM (
    SELECT tenant_id FROM notifications WHERE status = 'scheduled' AND send_after <= p_now
    UNION
    SELECT tenant_id FROM notifications WHERE status = 'sending' AND lease_until < p_now
  ) t
  WHERE p_after IS NULL OR tenant_id > p_after
  ORDER BY tenant_id
  LIMIT p_limit
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_tenants_with_due_webhook_deliveries(p_now timestamptz, p_after uuid, p_limit integer)
RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT tenant_id FROM (
    SELECT tenant_id FROM webhook_deliveries WHERE status IN ('pending', 'retrying') AND next_attempt_at <= p_now
    UNION
    SELECT tenant_id FROM webhook_deliveries WHERE status = 'delivering' AND lease_until < p_now
  ) t
  WHERE p_after IS NULL OR tenant_id > p_after
  ORDER BY tenant_id
  LIMIT p_limit
$$;
-- +goose StatementEnd

-- Assinaturas em past_due por tenant: a régua as reavalia para voltar a
-- active quando não houver mais faturas vencidas em aberto (por exemplo,
-- depois de um cancelamento ou de uma baixa da fatura).
CREATE INDEX subscriptions_past_due_idx ON subscriptions (tenant_id) WHERE status = 'past_due';

-- +goose StatementBegin
-- Tenants com trabalho de cobrança: faturas em aberto com vencimento até
-- p_horizon (lembretes antes do vencimento e faturas vencidas), suspensões
-- ativas da política, inadimplências ainda não resolvidas e assinaturas em
-- past_due.
CREATE FUNCTION app_tenants_for_collections(p_horizon timestamptz, p_after uuid, p_limit integer)
RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT tenant_id FROM (
    SELECT tenant_id FROM invoices WHERE status IN ('open', 'partially_paid') AND due_at <= p_horizon
    UNION
    SELECT tenant_id FROM collection_suspensions WHERE status = 'active'
    UNION
    SELECT tenant_id FROM invoice_delinquencies WHERE resolved_at IS NULL
    UNION
    SELECT tenant_id FROM subscriptions WHERE status = 'past_due'
  ) t
  WHERE p_after IS NULL OR tenant_id > p_after
  ORDER BY tenant_id
  LIMIT p_limit
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION app_tenants_with_due_notifications(timestamptz, uuid, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION app_tenants_with_due_webhook_deliveries(timestamptz, uuid, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION app_tenants_for_collections(timestamptz, uuid, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_tenants_with_due_notifications(timestamptz, uuid, integer) TO recorvia_app;
GRANT EXECUTE ON FUNCTION app_tenants_with_due_webhook_deliveries(timestamptz, uuid, integer) TO recorvia_app;
GRANT EXECUTE ON FUNCTION app_tenants_for_collections(timestamptz, uuid, integer) TO recorvia_app;

-- +goose Down
DROP FUNCTION IF EXISTS app_tenants_for_collections(timestamptz, uuid, integer);
DROP FUNCTION IF EXISTS app_tenants_with_due_webhook_deliveries(timestamptz, uuid, integer);
DROP FUNCTION IF EXISTS app_tenants_with_due_notifications(timestamptz, uuid, integer);
DROP INDEX IF EXISTS subscriptions_past_due_idx;
DROP TABLE IF EXISTS webhook_delivery_attempts;
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhook_events;
DROP TABLE IF EXISTS webhook_endpoint_secrets;
DROP TABLE IF EXISTS webhook_endpoints;
DROP TABLE IF EXISTS collection_suspensions;
DROP TABLE IF EXISTS invoice_delinquencies;
DROP TABLE IF EXISTS notification_events;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS notification_templates;
DROP TABLE IF EXISTS collection_policies;
