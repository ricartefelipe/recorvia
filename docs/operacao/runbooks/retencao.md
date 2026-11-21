# Runbook: expurgo por retenção

Cobre a tarefa `retention.purge` do worker (a cada hora; ADR-060), que remove por idade os dados
técnicos listados em [retenção e anonimização](../retencao-e-anonimizacao.md), e as tarefas
`outbox.purge` e `idempotency.purge`. As métricas não têm rótulos de tenant: os alertas apontam que
o expurgo parou ou não dá vazão, e as consultas abaixo localizam o tenant e o tipo de dado.

O expurgo nunca toca em registros financeiros nem na auditoria, e nenhuma ação deste runbook
remove dados manualmente: a correção é fazer a tarefa voltar a concluir.

## Expurgo parado ou falhando

### Sintomas
- Tabelas técnicas (inbox, entregas de webhooks, notificações, eventos de consumo) crescendo sem
  queda; dados vencidos pelos prazos configurados continuam consultáveis.

### Métricas e alertas
- `RecorviaRetentionPurgeStale`: `retention.purge` sem sucesso há mais de 3 horas, ou sem nenhum
  sucesso desde a partida do worker.
- `RecorviaScheduledTaskStale` com `task` igual a `outbox.purge` ou `idempotency.purge`.
- `recorvia_retention_purge_failures_total{kind}`: tipo de dado cujo expurgo falhou;
  `recorvia_retention_purged_total{kind}`: registros removidos por tipo;
  `recorvia_job_runs_total{task="retention.purge"}` e
  `recorvia_job_last_success_timestamp_seconds{task="retention.purge"}`.

### Diagnóstico
```sql
-- Situação das tarefas de expurgo: última execução, erro registrado e lease.
SELECT name, last_started_at, last_finished_at, next_run_at, locked_by, locked_until, left(last_error, 300) AS erro
  FROM scheduled_jobs WHERE name IN ('retention.purge', 'outbox.purge', 'idempotency.purge') ORDER BY name;
```
```bash
docker compose logs --since 3h worker | grep -E 'tarefa falhou|expurgo por retenção|reivindicação de tarefa falhou'
```
- `last_error` traz o tipo de dado e o tenant da falha (por exemplo,
  `notifications: tenant <id>: ...`). A execução continua nos demais tipos e tenants: um erro
  isolado impede o sucesso da tarefa, mas não o expurgo do restante.
- `locked_until` no futuro com `last_finished_at` antigo: execução em andamento ou interrompida
  (o lease expira sozinho e outra instância assume).
- Worker reiniciando com `RECORVIA_RETENTION_*` fora dos limites: a partida falha com o nome da
  variável (ver os mínimos em [retenção e anonimização](../retencao-e-anonimizacao.md)).
- Erro de privilégio ou de função inexistente (`app_retention_purge`): a migration `00014` não foi
  aplicada (`cmd/migrate status`).

### Ações
1. Corrigir a causa indicada pelo erro (migration pendente, configuração, banco indisponível) e
   aguardar a próxima execução horária; não é preciso reiniciar o worker para retomar.
2. Falha restrita a um tenant e a um tipo: investigar o registro que impede a remoção (por
   exemplo, uma chave estrangeira nova que referencia a tabela). O expurgo dos outros tenants
   continua; não remova linhas manualmente.
3. Para suspender o expurgo de um tipo durante uma investigação, aumente o prazo correspondente
   (`RECORVIA_RETENTION_*_DAYS`, até 3650 dias) e reinicie o worker.

### Verificação
`recorvia_job_last_success_timestamp_seconds{task="retention.purge"}` volta a avançar a cada hora,
`increase(recorvia_retention_purge_failures_total[3h])` zera e `last_error` fica nulo na consulta
do diagnóstico.

<a id="expurgo-sem-vazao"></a>

## Expurgo sem vazão

### Sintomas
- A tarefa conclui com sucesso, mas o volume vencido de um tipo não diminui.

### Métricas e alertas
- `RecorviaRetentionPurgeBacklog`: 20 ou mais execuções das últimas 24 horas pararam com
  registros vencidos restantes do mesmo tipo (`kind`).
- `recorvia_retention_purge_incomplete_total{kind}` e `recorvia_retention_purged_total{kind}`;
  `recorvia_job_duration_seconds{task="retention.purge"}`.

### Diagnóstico
```sql
-- Volume vencido por tipo, com os prazos padrão (superusuário; ajuste os intervalos aos prazos
-- configurados em RECORVIA_RETENTION_*_DAYS).
SELECT 'inbox_messages' AS tipo, count(*) AS vencidos FROM inbox_messages WHERE processed_at < now() - interval '90 days'
UNION ALL
SELECT 'dead_letters', count(*) FROM dead_letters WHERE status IN ('replayed', 'discarded') AND resolved_at < now() - interval '180 days'
UNION ALL
SELECT 'webhook_deliveries', count(*) FROM webhook_deliveries WHERE status IN ('succeeded', 'exhausted') AND updated_at < now() - interval '90 days'
UNION ALL
SELECT 'provider_webhook_payloads', count(*) FROM provider_webhook_events
 WHERE status IN ('processed', 'ignored') AND payload_purged_at IS NULL AND processed_at < now() - interval '90 days'
UNION ALL
SELECT 'notification_contents', count(*) FROM notifications
 WHERE status IN ('sent', 'failed', 'canceled', 'suppressed') AND content_ciphertext IS NOT NULL AND updated_at < now() - interval '90 days'
UNION ALL
SELECT 'notifications', count(*) FROM notifications
 WHERE status IN ('sent', 'failed', 'canceled', 'suppressed') AND updated_at < now() - interval '365 days'
UNION ALL
SELECT 'portal_links', count(*) FROM customer_portal_links WHERE expires_at < now() - interval '90 days'
UNION ALL
SELECT 'usage_aggregates_com_eventos', count(*) FROM usage_aggregates WHERE events_purged_at IS NULL AND period_end < now() - interval '430 days'
UNION ALL
SELECT 'invitations', count(*) FROM invitations WHERE accepted_at IS NULL AND expires_at < now() - interval '90 days';
```
```sql
-- Tenants com mais registros vencidos de um tipo (exemplo: inbox; superusuário).
SELECT tenant_id, count(*) AS vencidos, min(processed_at) AS mais_antigo
  FROM inbox_messages WHERE processed_at < now() - interval '90 days' GROUP BY 1 ORDER BY 2 DESC LIMIT 20;
```
- Acúmulo inicial (primeira execução depois de ativar a versão com o expurgo, ou depois de reduzir
  um prazo): o volume cai a cada hora; cada execução dura até 2 minutos e cada tenant recebe até
  20 lotes por tipo.
- Volume vencido crescendo mais rápido do que a remoção: lote pequeno para o volume.
- Registros vencidos que permanecem de propósito e não contam como acúmulo: dead letters abertas,
  entregas pendentes, notificações de faturas em aberto, eventos de consumo sem agregado fechado
  ou de períodos com correção pendente e convites com e-mail ainda por enviar.

### Ações
1. Aumentar `RECORVIA_RETENTION_BATCH_SIZE` (padrão 500, máximo 5000) e reiniciar o worker; a
   execução seguinte remove lotes maiores dentro do mesmo orçamento de tempo.
2. Conferir se há transações longas bloqueando as linhas (o expurgo pula linhas bloqueadas e
   tenta de novo na execução seguinte): ver [conexões do banco](faturamento.md#conexoes-do-banco).
3. Eventos de consumo com correção pendente há muito tempo: tratar a tarefa
   `billing.correct_late_usage` ([tarefas agendadas falhando](faturamento.md#tarefas-agendadas-falhando)).

### Verificação
`increase(recorvia_retention_purge_incomplete_total[24h])` volta a zero para o tipo e a consulta de
volume vencido diminui entre execuções.
