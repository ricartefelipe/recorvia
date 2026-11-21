-- Retenção automática e anonimização de clientes (ADR-060 a ADR-062).
--
-- O expurgo por idade é executado pelo worker, tenant a tenant, em lotes
-- limitados: app_retention_tenants descobre os tenants com dados vencidos e
-- app_retention_purge remove um lote dentro da transação do tenant. As regras
-- do que pode ser removido ficam no banco, de modo que nenhum chamador apaga
-- o que ainda é necessário (mensagem não processada, entrega pendente, dead
-- letter aberta, consumo não faturado ou dentro da tolerância de correção).
--
-- Registros financeiros e de auditoria (ledger, faturas, itens, pagamentos,
-- notas de crédito, créditos do cliente, histórico de assinaturas e
-- audit_log) não são tocados por esta migration: continuam imutáveis e sem
-- expurgo.

-- +goose Up

-- +goose StatementBegin
-- Históricos técnicos (eventos de notificação, eventos e tentativas de
-- webhooks enviados) continuam imutáveis para a aplicação, que não tem
-- privilégio de UPDATE nem de DELETE. A exclusão só é aceita dentro das
-- funções de retenção, que ligam app.retention_purge (local à transação)
-- durante a própria execução; UPDATE nunca é aceito.
CREATE FUNCTION app_reject_mutation_unless_purge() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' AND current_setting('app.retention_purge', true) = 'on' THEN
    RETURN OLD;
  END IF;
  RAISE EXCEPTION 'tabela % é imutável: operação % não permitida', TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'P0001';
END;
$$;
-- +goose StatementEnd

DROP TRIGGER notification_events_immutable ON notification_events;
CREATE TRIGGER notification_events_immutable BEFORE UPDATE OR DELETE ON notification_events
  FOR EACH ROW EXECUTE FUNCTION app_reject_mutation_unless_purge();
DROP TRIGGER webhook_events_immutable ON webhook_events;
CREATE TRIGGER webhook_events_immutable BEFORE UPDATE OR DELETE ON webhook_events
  FOR EACH ROW EXECUTE FUNCTION app_reject_mutation_unless_purge();
DROP TRIGGER webhook_delivery_attempts_immutable ON webhook_delivery_attempts;
CREATE TRIGGER webhook_delivery_attempts_immutable BEFORE UPDATE OR DELETE ON webhook_delivery_attempts
  FOR EACH ROW EXECUTE FUNCTION app_reject_mutation_unless_purge();

-- +goose StatementBegin
-- Links do portal: além do uso e da revogação (uma vez cada), a exclusão
-- passa a ser aceita somente dentro das funções de retenção.
CREATE OR REPLACE FUNCTION app_portal_links_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF current_setting('app.retention_purge', true) = 'on' THEN
      RETURN OLD;
    END IF;
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

-- Conteúdo de notificações: o corpo cifrado é removido depois do prazo e a
-- linha guarda quando isso ocorreu. Uma notificação enviada continua exigindo
-- destinatários; o conteúdo só pode faltar se tiver sido expurgado.
ALTER TABLE notifications ADD COLUMN content_purged_at timestamptz;
ALTER TABLE notifications DROP CONSTRAINT notifications_deliverable;
-- Reaplicação depois de um Down: notificações enviadas cujo conteúdo já havia
-- sido expurgado voltam a ser marcadas (a regra anterior não admitia envio sem
-- conteúdo, então numa primeira aplicação nenhuma linha é alterada).
UPDATE notifications SET content_purged_at = updated_at WHERE status = 'sent' AND content_ciphertext IS NULL;
ALTER TABLE notifications ADD CONSTRAINT notifications_deliverable CHECK (
  status NOT IN ('scheduled', 'sending', 'sent')
  OR (cardinality(recipients) > 0
      AND (content_ciphertext IS NOT NULL OR (status = 'sent' AND content_purged_at IS NOT NULL))));
ALTER TABLE notifications ADD CONSTRAINT notifications_content_purged
  CHECK (content_purged_at IS NULL OR content_ciphertext IS NULL);

-- Webhooks recebidos do provedor: o corpo é substituído por um objeto vazio
-- depois do prazo; a linha permanece para deduplicar reenvios do provedor.
ALTER TABLE provider_webhook_events ADD COLUMN payload_purged_at timestamptz;
ALTER TABLE provider_webhook_events ADD CONSTRAINT provider_webhook_events_payload_purged
  CHECK (payload_purged_at IS NULL OR (payload = '{}'::jsonb AND status IN ('processed', 'ignored')));

-- Consumo: o agregado fechado (quantidade, contagem e item faturado) é
-- mantido; events_purged_at registra que os eventos individuais do período
-- foram removidos.
ALTER TABLE usage_aggregates ADD COLUMN events_purged_at timestamptz;

-- Índices das consultas de retenção (por tenant e instante de referência) e
-- das verificações de chave estrangeira nas exclusões.
CREATE INDEX inbox_messages_processed_idx ON inbox_messages (tenant_id, processed_at);
CREATE INDEX dead_letters_resolved_idx ON dead_letters (tenant_id, resolved_at) WHERE status IN ('replayed', 'discarded');
CREATE INDEX webhook_deliveries_final_idx ON webhook_deliveries (tenant_id, updated_at) WHERE status IN ('succeeded', 'exhausted');
CREATE INDEX webhook_deliveries_event_idx ON webhook_deliveries (tenant_id, event_id);
CREATE INDEX webhook_events_created_idx ON webhook_events (tenant_id, created_at);
CREATE INDEX webhook_endpoint_secrets_expired_idx ON webhook_endpoint_secrets (tenant_id, expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX provider_webhook_events_payload_idx ON provider_webhook_events (tenant_id, processed_at)
  WHERE payload_purged_at IS NULL AND status IN ('processed', 'ignored');
CREATE INDEX notifications_content_idx ON notifications (tenant_id, updated_at)
  WHERE content_ciphertext IS NOT NULL AND status IN ('sent', 'failed', 'canceled', 'suppressed');
CREATE INDEX notifications_final_idx ON notifications (tenant_id, updated_at)
  WHERE status IN ('sent', 'failed', 'canceled', 'suppressed');
CREATE INDEX notifications_customer_idx ON notifications (tenant_id, customer_id) WHERE customer_id IS NOT NULL;
CREATE INDEX notifications_invitation_idx ON notifications (tenant_id, invitation_id) WHERE invitation_id IS NOT NULL;
CREATE INDEX customer_portal_links_expiry_idx ON customer_portal_links (tenant_id, expires_at);
CREATE INDEX usage_aggregates_events_idx ON usage_aggregates (tenant_id, period_end) WHERE events_purged_at IS NULL;
CREATE INDEX invitations_unaccepted_idx ON invitations (tenant_id, expires_at) WHERE accepted_at IS NULL;
-- Cobranças por cliente: usada pela anonimização para localizar os webhooks
-- recebidos do provedor.
CREATE INDEX charges_customer_idx ON charges (tenant_id, customer_id);

-- +goose StatementBegin
-- app_retention_tenants descobre, em páginas ordenadas por ID (p_after
-- exclusivo), os tenants com dados do tipo p_kind vencidos em p_cutoff.
-- Devolve somente IDs de tenant; a remoção ocorre na transação de cada um.
CREATE FUNCTION app_retention_tenants(p_kind text, p_cutoff timestamptz, p_after uuid, p_limit integer)
RETURNS SETOF uuid
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
  IF p_kind NOT IN ('inbox_messages', 'dead_letters', 'webhook_deliveries', 'webhook_events', 'webhook_endpoint_secrets',
                    'provider_webhook_payloads', 'notification_contents', 'notifications', 'portal_links', 'usage_events',
                    'invitations') THEN
    RAISE EXCEPTION 'tipo de retenção desconhecido: %', p_kind USING ERRCODE = '22023';
  END IF;
  RETURN QUERY
  SELECT t.id FROM tenants t
   WHERE (p_after IS NULL OR t.id > p_after)
     AND CASE p_kind
       WHEN 'inbox_messages' THEN EXISTS (
         SELECT 1 FROM inbox_messages m WHERE m.tenant_id = t.id AND m.processed_at < p_cutoff)
       WHEN 'dead_letters' THEN EXISTS (
         SELECT 1 FROM dead_letters d WHERE d.tenant_id = t.id AND d.status IN ('replayed', 'discarded') AND d.resolved_at < p_cutoff)
       WHEN 'webhook_deliveries' THEN EXISTS (
         SELECT 1 FROM webhook_deliveries d WHERE d.tenant_id = t.id AND d.status IN ('succeeded', 'exhausted') AND d.updated_at < p_cutoff)
       WHEN 'webhook_events' THEN EXISTS (
         SELECT 1 FROM webhook_events e WHERE e.tenant_id = t.id AND e.created_at < p_cutoff
            AND NOT EXISTS (SELECT 1 FROM webhook_deliveries d WHERE d.tenant_id = e.tenant_id AND d.event_id = e.id))
       WHEN 'webhook_endpoint_secrets' THEN EXISTS (
         SELECT 1 FROM webhook_endpoint_secrets s WHERE s.tenant_id = t.id AND s.expires_at IS NOT NULL AND s.expires_at < p_cutoff)
       WHEN 'provider_webhook_payloads' THEN EXISTS (
         SELECT 1 FROM provider_webhook_events w WHERE w.tenant_id = t.id AND w.status IN ('processed', 'ignored')
            AND w.payload_purged_at IS NULL AND w.processed_at < p_cutoff)
       WHEN 'notification_contents' THEN EXISTS (
         SELECT 1 FROM notifications n WHERE n.tenant_id = t.id AND n.status IN ('sent', 'failed', 'canceled', 'suppressed')
            AND n.content_ciphertext IS NOT NULL AND n.updated_at < p_cutoff)
       WHEN 'notifications' THEN EXISTS (
         SELECT 1 FROM notifications n WHERE n.tenant_id = t.id AND n.status IN ('sent', 'failed', 'canceled', 'suppressed')
            AND n.updated_at < p_cutoff
            AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.tenant_id = n.tenant_id AND i.id = n.invoice_id
                              AND i.status IN ('open', 'partially_paid')))
       WHEN 'portal_links' THEN EXISTS (
         SELECT 1 FROM customer_portal_links l WHERE l.tenant_id = t.id AND l.expires_at < p_cutoff)
       WHEN 'usage_events' THEN EXISTS (
         SELECT 1 FROM usage_aggregates a JOIN billable_metrics bm ON bm.tenant_id = a.tenant_id AND bm.id = a.metric_id
          WHERE a.tenant_id = t.id AND a.events_purged_at IS NULL AND a.period_end < p_cutoff
            AND a.period_end + make_interval(hours => bm.late_grace_hours) < p_cutoff)
       WHEN 'invitations' THEN EXISTS (
         SELECT 1 FROM invitations i WHERE i.tenant_id = t.id AND i.accepted_at IS NULL AND i.expires_at < p_cutoff)
       ELSE false
     END
   ORDER BY t.id
   LIMIT p_limit;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- app_retention_purge remove (ou, para conteúdos, esvazia) até p_limit
-- registros do tipo p_kind do tenant p_tenant vencidos em p_cutoff. Exige a
-- transação do próprio tenant. more indica que pode haver mais registros
-- vencidos (o chamador repete em nova transação). Linhas bloqueadas por
-- outra transação são puladas (SKIP LOCKED) e ficam para a próxima execução.
--
-- O que nunca é removido:
--   inbox_messages             somente por idade do processamento (o prazo
--                              mínimo é validado pela aplicação)
--   dead_letters               abertas (status open)
--   webhook_deliveries         pendentes, em reenvio ou em andamento
--   webhook_events             com alguma entrega ainda registrada
--   webhook_endpoint_secrets   corrente (sem validade) ou ainda válido
--   provider_webhook_payloads  eventos recebidos e ainda não processados
--   notification_contents      notificações agendadas ou em envio
--   notifications              agendadas, em envio ou de fatura em aberto
--                              (o registro do lembrete evita reenvio)
--   portal_links               dentro da validade
--   usage_events               sem agregado fechado (não faturados), dentro
--                              da tolerância de atraso da métrica ou de
--                              período com correção pendente
--   invitations                aceitos, dentro da validade ou com e-mail de
--                              convite ainda por enviar
CREATE FUNCTION app_retention_purge(p_kind text, p_tenant uuid, p_cutoff timestamptz, p_now timestamptz, p_limit integer)
RETURNS TABLE (purged integer, more boolean)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
  v_n       integer := 0;
  v_d       integer;
  v_ids     uuid[];
  v_visited integer := 0;
  v_more    boolean;
  v_agg     record;
BEGIN
  IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM app_current_tenant() THEN
    RAISE EXCEPTION 'expurgo fora da transação do tenant' USING ERRCODE = '42501';
  END IF;
  IF p_cutoff IS NULL OR p_now IS NULL OR p_cutoff > p_now OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 10000 THEN
    RAISE EXCEPTION 'parâmetros de expurgo inválidos' USING ERRCODE = '22023';
  END IF;
  -- Libera a exclusão nos gatilhos somente durante esta chamada: o valor é
  -- local à transação, desligado antes do retorno e desfeito pelo PostgreSQL
  -- se a chamada falhar.
  PERFORM set_config('app.retention_purge', 'on', true);

  CASE p_kind
  WHEN 'inbox_messages' THEN
    DELETE FROM inbox_messages m USING (
      SELECT x.tenant_id, x.consumer, x.message_id FROM inbox_messages x
       WHERE x.tenant_id = p_tenant AND x.processed_at < p_cutoff
       ORDER BY x.processed_at LIMIT p_limit FOR UPDATE SKIP LOCKED) s
     WHERE m.tenant_id = s.tenant_id AND m.consumer = s.consumer AND m.message_id = s.message_id;
    GET DIAGNOSTICS v_n = ROW_COUNT;

  WHEN 'dead_letters' THEN
    DELETE FROM dead_letters dl USING (
      SELECT x.id FROM dead_letters x
       WHERE x.tenant_id = p_tenant AND x.status IN ('replayed', 'discarded') AND x.resolved_at < p_cutoff
       ORDER BY x.resolved_at LIMIT p_limit FOR UPDATE SKIP LOCKED) s
     WHERE dl.tenant_id = p_tenant AND dl.id = s.id;
    GET DIAGNOSTICS v_n = ROW_COUNT;

  WHEN 'webhook_deliveries' THEN
    SELECT array_agg(s.id) INTO v_ids FROM (
      SELECT x.id FROM webhook_deliveries x
       WHERE x.tenant_id = p_tenant AND x.status IN ('succeeded', 'exhausted') AND x.updated_at < p_cutoff
       ORDER BY x.updated_at LIMIT p_limit FOR UPDATE SKIP LOCKED) s;
    DELETE FROM webhook_delivery_attempts a WHERE a.tenant_id = p_tenant AND a.delivery_id = ANY (v_ids);
    DELETE FROM webhook_deliveries wd WHERE wd.tenant_id = p_tenant AND wd.id = ANY (v_ids);
    GET DIAGNOSTICS v_n = ROW_COUNT;

  WHEN 'webhook_events' THEN
    DELETE FROM webhook_events we USING (
      SELECT x.id FROM webhook_events x
       WHERE x.tenant_id = p_tenant AND x.created_at < p_cutoff
         AND NOT EXISTS (SELECT 1 FROM webhook_deliveries wd WHERE wd.tenant_id = x.tenant_id AND wd.event_id = x.id)
       ORDER BY x.created_at LIMIT p_limit FOR UPDATE SKIP LOCKED) s
     WHERE we.tenant_id = p_tenant AND we.id = s.id;
    GET DIAGNOSTICS v_n = ROW_COUNT;

  WHEN 'webhook_endpoint_secrets' THEN
    DELETE FROM webhook_endpoint_secrets ws USING (
      SELECT x.id FROM webhook_endpoint_secrets x
       WHERE x.tenant_id = p_tenant AND x.expires_at IS NOT NULL AND x.expires_at < p_cutoff
       ORDER BY x.expires_at LIMIT p_limit FOR UPDATE SKIP LOCKED) s
     WHERE ws.tenant_id = p_tenant AND ws.id = s.id;
    GET DIAGNOSTICS v_n = ROW_COUNT;

  WHEN 'provider_webhook_payloads' THEN
    UPDATE provider_webhook_events w SET payload = '{}'::jsonb, payload_purged_at = p_now
      FROM (SELECT x.id FROM provider_webhook_events x
             WHERE x.tenant_id = p_tenant AND x.status IN ('processed', 'ignored') AND x.payload_purged_at IS NULL
               AND x.processed_at < p_cutoff
             ORDER BY x.processed_at LIMIT p_limit FOR UPDATE SKIP LOCKED) s
     WHERE w.tenant_id = p_tenant AND w.id = s.id;
    GET DIAGNOSTICS v_n = ROW_COUNT;

  WHEN 'notification_contents' THEN
    UPDATE notifications n SET content_ciphertext = NULL, content_key_id = NULL, content_purged_at = p_now
      FROM (SELECT x.id FROM notifications x
             WHERE x.tenant_id = p_tenant AND x.status IN ('sent', 'failed', 'canceled', 'suppressed')
               AND x.content_ciphertext IS NOT NULL AND x.updated_at < p_cutoff
             ORDER BY x.updated_at LIMIT p_limit FOR UPDATE SKIP LOCKED) s
     WHERE n.tenant_id = p_tenant AND n.id = s.id;
    GET DIAGNOSTICS v_n = ROW_COUNT;

  WHEN 'notifications' THEN
    SELECT array_agg(s.id) INTO v_ids FROM (
      SELECT x.id FROM notifications x
       WHERE x.tenant_id = p_tenant AND x.status IN ('sent', 'failed', 'canceled', 'suppressed') AND x.updated_at < p_cutoff
         AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.tenant_id = x.tenant_id AND i.id = x.invoice_id
                           AND i.status IN ('open', 'partially_paid'))
       ORDER BY x.updated_at LIMIT p_limit FOR UPDATE OF x SKIP LOCKED) s;
    DELETE FROM notification_events ne WHERE ne.tenant_id = p_tenant AND ne.notification_id = ANY (v_ids);
    DELETE FROM notifications n WHERE n.tenant_id = p_tenant AND n.id = ANY (v_ids);
    GET DIAGNOSTICS v_n = ROW_COUNT;

  WHEN 'portal_links' THEN
    DELETE FROM customer_portal_links l USING (
      SELECT x.id FROM customer_portal_links x
       WHERE x.tenant_id = p_tenant AND x.expires_at < p_cutoff
       ORDER BY x.expires_at LIMIT p_limit FOR UPDATE SKIP LOCKED) s
     WHERE l.tenant_id = p_tenant AND l.id = s.id;
    GET DIAGNOSTICS v_n = ROW_COUNT;

  WHEN 'usage_events' THEN
    -- Por agregado fechado (assinatura, métrica e período): os eventos só
    -- saem depois da tolerância de atraso da métrica, quando nenhum evento
    -- novo do período é aceito, e sem correção pendente (a correção recalcula
    -- o período a partir de todos os eventos). O agregado é marcado quando
    -- não resta evento.
    FOR v_agg IN
      SELECT a.id, a.subscription_id, a.metric_id, a.period_start
        FROM usage_aggregates a JOIN billable_metrics bm ON bm.tenant_id = a.tenant_id AND bm.id = a.metric_id
       WHERE a.tenant_id = p_tenant AND a.events_purged_at IS NULL AND a.period_end < p_cutoff
         AND a.period_end + make_interval(hours => bm.late_grace_hours) < p_cutoff
         AND NOT EXISTS (
           SELECT 1 FROM usage_events e
            WHERE e.tenant_id = a.tenant_id AND e.subscription_id = a.subscription_id AND e.metric_id = a.metric_id
              AND e.period_start = a.period_start AND e.corrected_at IS NULL
              AND (e.status = 'late' OR e.aggregate_id IS NULL))
       ORDER BY a.period_end, a.id
       LIMIT p_limit
       FOR UPDATE OF a SKIP LOCKED
    LOOP
      v_visited := v_visited + 1;
      DELETE FROM usage_events ue WHERE ue.tenant_id = p_tenant AND ue.id IN (
        SELECT x.id FROM usage_events x
         WHERE x.tenant_id = p_tenant AND x.subscription_id = v_agg.subscription_id AND x.metric_id = v_agg.metric_id
           AND x.period_start = v_agg.period_start
         LIMIT p_limit - v_n);
      GET DIAGNOSTICS v_d = ROW_COUNT;
      v_n := v_n + v_d;
      IF v_n >= p_limit THEN
        EXIT;
      END IF;
      UPDATE usage_aggregates SET events_purged_at = p_now WHERE tenant_id = p_tenant AND id = v_agg.id;
    END LOOP;
    v_more := v_n >= p_limit OR v_visited >= p_limit;

  WHEN 'invitations' THEN
    SELECT array_agg(s.id) INTO v_ids FROM (
      SELECT x.id FROM invitations x
       WHERE x.tenant_id = p_tenant AND x.accepted_at IS NULL AND x.expires_at < p_cutoff
         AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.tenant_id = x.tenant_id AND n.invitation_id = x.id
                           AND n.status IN ('scheduled', 'sending'))
       ORDER BY x.expires_at LIMIT p_limit FOR UPDATE OF x SKIP LOCKED) s;
    DELETE FROM notification_events ne USING notifications n
     WHERE n.tenant_id = p_tenant AND n.invitation_id = ANY (v_ids) AND ne.tenant_id = n.tenant_id AND ne.notification_id = n.id;
    DELETE FROM notifications n WHERE n.tenant_id = p_tenant AND n.invitation_id = ANY (v_ids);
    DELETE FROM invitations i WHERE i.tenant_id = p_tenant AND i.id = ANY (v_ids);
    GET DIAGNOSTICS v_n = ROW_COUNT;

  ELSE
    RAISE EXCEPTION 'tipo de retenção desconhecido: %', p_kind USING ERRCODE = '22023';
  END CASE;

  PERFORM set_config('app.retention_purge', '', true);
  purged := v_n;
  more := COALESCE(v_more, v_n >= p_limit);
  RETURN NEXT;
END;
$$;
-- +goose StatementEnd

-- Anonimização de clientes (ADR-061). O cadastro anonimizado fica arquivado e
-- não volta a ser alterado; endereço e identificadores no provedor são
-- excluídos pela aplicação, na transação da anonimização.
ALTER TABLE customers ADD CONSTRAINT customers_anonymized_archived CHECK (anonymized_at IS NULL OR status = 'archived');
GRANT DELETE ON customer_billing_addresses TO recorvia_app;
GRANT DELETE ON customer_provider_refs TO recorvia_app;

-- +goose StatementBegin
-- A anonimização é irreversível: depois dela a linha do cliente não aceita
-- nenhuma alteração.
CREATE FUNCTION app_customers_anonymized_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF OLD.anonymized_at IS NOT NULL THEN
    RAISE EXCEPTION 'cliente anonimizado % não pode ser alterado', OLD.id USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER customers_anonymized_guard BEFORE UPDATE ON customers FOR EACH ROW EXECUTE FUNCTION app_customers_anonymized_guard();

-- +goose StatementBegin
-- Nenhuma assinatura nova para cliente anonimizado. O bloqueio compartilhado
-- espera uma anonimização em andamento (que mantém o cliente com bloqueio
-- exclusivo) e lê o resultado dela: a criação de assinatura que conferiu o
-- cliente antes da anonimização é recusada em vez de gravada depois.
CREATE FUNCTION app_subscriptions_customer_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  v_anonymized timestamptz;
BEGIN
  SELECT c.anonymized_at INTO v_anonymized FROM customers c
   WHERE c.tenant_id = NEW.tenant_id AND c.id = NEW.customer_id FOR KEY SHARE;
  IF v_anonymized IS NOT NULL THEN
    RAISE EXCEPTION 'cliente anonimizado % não recebe novas assinaturas', NEW.customer_id USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER subscriptions_customer_guard BEFORE INSERT ON subscriptions FOR EACH ROW EXECUTE FUNCTION app_subscriptions_customer_guard();

-- +goose StatementBegin
-- Nenhuma fatura nova para cliente anonimizado. A anonimização só é aceita
-- sem faturamento pendente; este gatilho é a defesa no banco para a emissão
-- concorrente: o bloqueio compartilhado espera uma anonimização em andamento
-- e lê o resultado dela, de modo que a fatura montada com o cadastro lido
-- antes da anonimização é recusada em vez de gravada depois com os dados
-- pessoais, e nenhuma fatura nasce com o cadastro neutro.
CREATE FUNCTION app_invoices_customer_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  v_anonymized timestamptz;
BEGIN
  SELECT c.anonymized_at INTO v_anonymized FROM customers c
   WHERE c.tenant_id = NEW.tenant_id AND c.id = NEW.customer_id FOR KEY SHARE;
  IF v_anonymized IS NOT NULL THEN
    RAISE EXCEPTION 'cliente anonimizado % não recebe novas faturas', NEW.customer_id USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER invoices_customer_guard BEFORE INSERT ON invoices FOR EACH ROW EXECUTE FUNCTION app_invoices_customer_guard();

-- +goose StatementBegin
-- app_customer_delete_notifications exclui as notificações do cliente
-- (destinatários, assunto, conteúdo cifrado e histórico de envio) na
-- transação da anonimização. Notificações não são registros financeiros; as
-- faturas e os pagamentos a que se referem permanecem.
CREATE FUNCTION app_customer_delete_notifications(p_tenant uuid, p_customer uuid)
RETURNS integer
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
  v_n integer;
BEGIN
  IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM app_current_tenant() THEN
    RAISE EXCEPTION 'exclusão fora da transação do tenant' USING ERRCODE = '42501';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM customers c WHERE c.tenant_id = p_tenant AND c.id = p_customer AND c.anonymized_at IS NOT NULL) THEN
    RAISE EXCEPTION 'cliente % não está anonimizado', p_customer USING ERRCODE = '22023';
  END IF;
  PERFORM set_config('app.retention_purge', 'on', true);
  DELETE FROM notification_events ne USING notifications n
   WHERE n.tenant_id = p_tenant AND n.customer_id = p_customer AND ne.tenant_id = n.tenant_id AND ne.notification_id = n.id;
  DELETE FROM notifications n WHERE n.tenant_id = p_tenant AND n.customer_id = p_customer;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  PERFORM set_config('app.retention_purge', '', true);
  RETURN v_n;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION app_retention_tenants(text, timestamptz, uuid, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION app_retention_purge(text, uuid, timestamptz, timestamptz, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION app_customer_delete_notifications(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_retention_tenants(text, timestamptz, uuid, integer) TO recorvia_app;
GRANT EXECUTE ON FUNCTION app_retention_purge(text, uuid, timestamptz, timestamptz, integer) TO recorvia_app;
GRANT EXECUTE ON FUNCTION app_customer_delete_notifications(uuid, uuid) TO recorvia_app;

-- +goose Down
DROP FUNCTION IF EXISTS app_customer_delete_notifications(uuid, uuid);
DROP TRIGGER IF EXISTS invoices_customer_guard ON invoices;
DROP FUNCTION IF EXISTS app_invoices_customer_guard();
DROP TRIGGER IF EXISTS subscriptions_customer_guard ON subscriptions;
DROP FUNCTION IF EXISTS app_subscriptions_customer_guard();
DROP TRIGGER IF EXISTS customers_anonymized_guard ON customers;
DROP FUNCTION IF EXISTS app_customers_anonymized_guard();
REVOKE DELETE ON customer_provider_refs FROM recorvia_app;
REVOKE DELETE ON customer_billing_addresses FROM recorvia_app;
ALTER TABLE customers DROP CONSTRAINT IF EXISTS customers_anonymized_archived;
DROP FUNCTION IF EXISTS app_retention_purge(text, uuid, timestamptz, timestamptz, integer);
DROP FUNCTION IF EXISTS app_retention_tenants(text, timestamptz, uuid, integer);
DROP INDEX IF EXISTS charges_customer_idx;
DROP INDEX IF EXISTS invitations_unaccepted_idx;
DROP INDEX IF EXISTS usage_aggregates_events_idx;
DROP INDEX IF EXISTS customer_portal_links_expiry_idx;
DROP INDEX IF EXISTS notifications_invitation_idx;
DROP INDEX IF EXISTS notifications_customer_idx;
DROP INDEX IF EXISTS notifications_final_idx;
DROP INDEX IF EXISTS notifications_content_idx;
DROP INDEX IF EXISTS provider_webhook_events_payload_idx;
DROP INDEX IF EXISTS webhook_endpoint_secrets_expired_idx;
DROP INDEX IF EXISTS webhook_events_created_idx;
DROP INDEX IF EXISTS webhook_deliveries_event_idx;
DROP INDEX IF EXISTS webhook_deliveries_final_idx;
DROP INDEX IF EXISTS dead_letters_resolved_idx;
DROP INDEX IF EXISTS inbox_messages_processed_idx;
ALTER TABLE usage_aggregates DROP COLUMN IF EXISTS events_purged_at;
ALTER TABLE provider_webhook_events DROP CONSTRAINT IF EXISTS provider_webhook_events_payload_purged;
ALTER TABLE provider_webhook_events DROP COLUMN IF EXISTS payload_purged_at;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_content_purged;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_deliverable;
-- Notificações enviadas cujo conteúdo já foi expurgado não satisfazem a regra
-- anterior e o conteúdo não pode ser restaurado: a constraint volta sem
-- validar as linhas existentes e continua valendo para as novas gravações.
ALTER TABLE notifications ADD CONSTRAINT notifications_deliverable CHECK (status NOT IN ('scheduled', 'sending', 'sent')
    OR (content_ciphertext IS NOT NULL AND cardinality(recipients) > 0)) NOT VALID;
ALTER TABLE notifications DROP COLUMN IF EXISTS content_purged_at;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_portal_links_guard() RETURNS trigger
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
DROP TRIGGER IF EXISTS webhook_delivery_attempts_immutable ON webhook_delivery_attempts;
CREATE TRIGGER webhook_delivery_attempts_immutable BEFORE UPDATE OR DELETE ON webhook_delivery_attempts
  FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
DROP TRIGGER IF EXISTS webhook_events_immutable ON webhook_events;
CREATE TRIGGER webhook_events_immutable BEFORE UPDATE OR DELETE ON webhook_events FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
DROP TRIGGER IF EXISTS notification_events_immutable ON notification_events;
CREATE TRIGGER notification_events_immutable BEFORE UPDATE OR DELETE ON notification_events
  FOR EACH ROW EXECUTE FUNCTION app_reject_mutation();
DROP FUNCTION IF EXISTS app_reject_mutation_unless_purge();
