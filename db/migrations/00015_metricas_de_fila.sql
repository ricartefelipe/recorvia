-- Métricas globais de fila (ADR-066): dead letters abertas por consumidor,
-- fila de notificações e fila de entregas de webhooks enviados. As funções
-- agregam todos os tenants sem identificá-los (SECURITY DEFINER, como
-- app_outbox_stats e app_payment_operation_stats) e recebem o instante de
-- referência do chamador, que usa o relógio da aplicação: a idade não
-- depende do relógio do servidor de banco.
--
-- Os índices parciais mantêm as consultas da tarefa metrics.collect (a cada
-- 15 s) proporcionais ao trabalho em aberto, e não ao histórico: dead letters
-- resolvidas e notificações encerradas há mais de um dia ficam de fora.

-- +goose Up

CREATE INDEX dead_letters_open_idx ON dead_letters (consumer, failed_at) WHERE status = 'open';
CREATE INDEX notifications_failed_idx ON notifications (updated_at) WHERE status = 'failed';

-- +goose StatementBegin
-- Dead letters abertas por consumidor, inclusive as registradas sem tenant
-- (invisíveis ao papel da aplicação pela RLS), com a idade da mais antiga.
CREATE FUNCTION app_dead_letter_stats(p_now timestamptz)
RETURNS TABLE (consumer text, open_total bigint, oldest_open_seconds double precision)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT d.consumer, count(*),
         GREATEST(EXTRACT(EPOCH FROM p_now - min(d.failed_at)), 0)::double precision
    FROM dead_letters d
   WHERE d.status = 'open'
   GROUP BY d.consumer
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Fila de notificações por situação:
--   due      agendadas com o horário de envio vencido; a idade é o atraso da
--            mais antiga em relação ao horário de envio;
--   sending  em envio (reivindicadas com lease); a idade é o tempo desde a
--            reivindicação mais antiga;
--   failed   encerradas com falha definitiva em (p_failed_since, p_now]; a
--            idade é o tempo desde a falha mais antiga da janela.
-- Agendamentos futuros (lembretes da régua, novas tentativas em espera) não
-- são fila acumulada e ficam de fora.
CREATE FUNCTION app_notification_backlog_stats(p_now timestamptz, p_failed_since timestamptz)
RETURNS TABLE (state text, total bigint, oldest_seconds double precision)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT 'due', count(*),
         COALESCE(GREATEST(EXTRACT(EPOCH FROM p_now - min(n.send_after)), 0), 0)::double precision
    FROM notifications n
   WHERE n.status = 'scheduled' AND n.send_after <= p_now
  UNION ALL
  SELECT 'sending', count(*),
         COALESCE(GREATEST(EXTRACT(EPOCH FROM p_now - min(n.updated_at)), 0), 0)::double precision
    FROM notifications n
   WHERE n.status = 'sending'
  UNION ALL
  SELECT 'failed', count(*),
         COALESCE(GREATEST(EXTRACT(EPOCH FROM p_now - min(n.updated_at)), 0), 0)::double precision
    FROM notifications n
   WHERE n.status = 'failed' AND n.updated_at > p_failed_since AND n.updated_at <= p_now
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Fila de entregas de webhooks enviados por situação (pending e retrying),
-- com o atraso da entrega mais antiga já devida (horário da próxima
-- tentativa vencido). Entregas em espera de backoff contam no total e não
-- no atraso: a espera é o comportamento previsto para um destino fora do ar.
CREATE FUNCTION app_webhook_delivery_backlog_stats(p_now timestamptz)
RETURNS TABLE (state text, total bigint, oldest_due_seconds double precision)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
  SELECT d.status, count(*),
         COALESCE(GREATEST(EXTRACT(EPOCH FROM p_now - min(d.next_attempt_at) FILTER (WHERE d.next_attempt_at <= p_now)), 0), 0)::double precision
    FROM webhook_deliveries d
   WHERE d.status IN ('pending', 'retrying')
   GROUP BY d.status
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION app_dead_letter_stats(timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION app_notification_backlog_stats(timestamptz, timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION app_webhook_delivery_backlog_stats(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_dead_letter_stats(timestamptz) TO recorvia_app;
GRANT EXECUTE ON FUNCTION app_notification_backlog_stats(timestamptz, timestamptz) TO recorvia_app;
GRANT EXECUTE ON FUNCTION app_webhook_delivery_backlog_stats(timestamptz) TO recorvia_app;

-- +goose Down
DROP FUNCTION IF EXISTS app_webhook_delivery_backlog_stats(timestamptz);
DROP FUNCTION IF EXISTS app_notification_backlog_stats(timestamptz, timestamptz);
DROP FUNCTION IF EXISTS app_dead_letter_stats(timestamptz);
DROP INDEX IF EXISTS notifications_failed_idx;
DROP INDEX IF EXISTS dead_letters_open_idx;
