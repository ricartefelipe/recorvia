-- name: DatabaseConnectionStats :one
-- Conexões de clientes abertas no servidor (todas as aplicações e papéis) e
-- o limite disponível para papéis comuns. Sem pg_read_all_stats (caso do
-- papel da aplicação), as linhas de sessões de outros papéis trazem
-- backend_type nulo, mas mantêm o banco e o papel: uma linha com banco e
-- papel definidos é uma sessão de cliente (ou um trabalhador paralelo de uma
-- delas), enquanto os processos auxiliares do servidor não têm banco ou papel.
SELECT (SELECT count(*)
          FROM pg_catalog.pg_stat_activity
         WHERE backend_type = 'client backend'
            OR (backend_type IS NULL AND datid IS NOT NULL AND usesysid IS NOT NULL))::bigint AS connections,
       (current_setting('max_connections')::int
        - current_setting('superuser_reserved_connections')::int
        - current_setting('reserved_connections')::int)::bigint AS available_connections;

-- name: DeadLetterStats :many
-- Dead letters abertas por consumidor (todos os tenants e as registradas sem
-- tenant, sem identificá-los) e a idade da mais antiga.
SELECT consumer::text AS consumer, open_total::bigint AS open_total, oldest_open_seconds::double precision AS oldest_open_seconds
  FROM app_dead_letter_stats(@now);

-- name: NotificationBacklogStats :many
-- Fila de notificações por situação (due, sending, failed), sem identificação
-- de tenant; failed considera as falhas definitivas desde failed_since.
SELECT state::text AS state, total::bigint AS total, oldest_seconds::double precision AS oldest_seconds
  FROM app_notification_backlog_stats(@now, @failed_since);

-- name: WebhookDeliveryBacklogStats :many
-- Fila de entregas de webhooks enviados por situação (pending, retrying), sem
-- identificação de tenant, com o atraso da entrega devida mais antiga.
SELECT state::text AS state, total::bigint AS total, oldest_due_seconds::double precision AS oldest_due_seconds
  FROM app_webhook_delivery_backlog_stats(@now);
