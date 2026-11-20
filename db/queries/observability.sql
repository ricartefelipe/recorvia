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
