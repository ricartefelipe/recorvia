# Runbook: faturamento e cobrança

Cobre o motor de recorrência (`billing.run_cycle`), as demais tarefas agendadas, a criação de
cobranças no provedor, os webhooks recebidos, as conexões do banco e os erros internos da API.
Fluxos de referência: `docs/arquitetura/fluxos.md` (faturamento recorrente, cobrança no provedor e
webhook).

## Motor de faturamento parado

### Sintomas
- Assinaturas com período vencido sem fatura emitida; `GET /api/v1/invoices` sem faturas novas
  no horário esperado.
- Dashboard *Recorvia: visão geral*, painel "Tempo desde o último faturamento" acima de 15 minutos.

### Métricas e alertas
- `RecorviaBillingCycleStale` (crítico): `time() - max(recorvia_job_last_success_timestamp_seconds{task="billing.run_cycle"}) > 900`.
- `recorvia_job_runs_total{task="billing.run_cycle", outcome="failure"}` e
  `recorvia_job_duration_seconds` (dashboard *banco e processos*).
- Sem nenhuma série do worker: `RecorviaTargetDown` (o worker está parado).

### Diagnóstico
```bash
docker compose ps worker
docker compose logs --since 30m worker | grep -E 'billing.run_cycle|tarefa|error'
```
```sql
-- Estado da tarefa no agendador (lease, último erro, próxima execução).
SELECT name, next_run_at, locked_by, locked_until, last_started_at, last_finished_at, last_error, run_count
  FROM scheduled_jobs ORDER BY name;
-- Assinaturas vencidas por tenant (superusuário; somente contagens).
SELECT tenant_id, count(*), min(next_billing_at) AS vencida_desde
  FROM subscriptions WHERE next_billing_at <= now() GROUP BY 1 ORDER BY 3;
```
- `locked_until` no futuro com `last_finished_at` antigo: uma instância reivindicou e está presa
  (ou caiu; o lease expira sozinho em até 11 minutos, o maior timeout das tarefas mais 1 minuto).
- `last_error` preenchido: a execução falha. A mensagem (também no log `tarefa falhou` do worker,
  campo `error`) identifica o tenant e a assinatura: `billing: tenant <id>: assinatura <id>: ...`.
- **Atenção:** dentro de um tenant, a falha de uma assinatura não impede as demais; entre tenants,
  a execução para no primeiro tenant com erro (`Engine.RunAll`). Uma assinatura com falha
  persistente atrasa, a cada minuto, os tenants descobertos depois dela. Assinaturas vencidas há
  muito tempo em vários tenants, com `last_error` sempre apontando o mesmo tenant, indicam esse
  caso.
- Duração p95 próxima do timeout (10 minutos): volume alto ou bloqueio; ver
  [Conexões do banco](#conexoes-do-banco).

### Ações
1. Worker parado: `docker compose up -d worker` (ou reinício pelo orquestrador). O catch-up emite os
   períodos atrasados (até 24 por transação), sem duplicar faturas.
2. Falha restrita a uma assinatura (dado inválido, regra de negócio): corrija o cadastro pela API
   (ou suspenda a assinatura, `POST /api/v1/subscriptions/{id}/suspend`, até a correção) e aguarde o
   próximo ciclo (1 minuto). Enquanto a falha persistir, os tenants descobertos depois do tenant
   afetado podem ser faturados com atraso (ver diagnóstico).
3. Execução manual para um tenant, com a permissão `billing.run`:
   `POST /api/v1/billing-runs` com `Idempotency-Key` (auditado). Útil para faturar um tenant
   atrasado pela falha de outro.
4. Lease preso após queda abrupta: aguarde a expiração (`locked_until`). Não apague linhas de
   `scheduled_jobs`.

### Verificação
- `recorvia_job_last_success_timestamp_seconds{task="billing.run_cycle"}` volta a avançar a cada
  minuto e o alerta é resolvido.
- A consulta de assinaturas vencidas não retorna linhas (ou só assinaturas cujo vencimento é o
  minuto corrente).
- `recorvia_invoices_issued_total` cresce conforme as faturas atrasadas são emitidas.

## Tarefas agendadas falhando

### Sintomas
- Correção de consumo, recuperação de operações, fechamento de extratos da plataforma ou expurgos
  sem efeito; métricas da outbox e do banco congeladas.

### Métricas e alertas
- `RecorviaScheduledTaskFailing`: três ou mais falhas de uma tarefa (`task`) em 30 minutos.
- `RecorviaMetricsCollectionStale`: `metrics.collect` sem sucesso há 2 minutos (os gauges da
  outbox, das operações e das conexões do servidor ficam desatualizados).

### Diagnóstico
```sql
SELECT name, last_error, last_started_at, last_finished_at, next_run_at FROM scheduled_jobs
 WHERE last_error IS NOT NULL ORDER BY last_finished_at DESC;
```
```bash
docker compose logs --since 30m worker | grep -E '"job"|tarefa'
```

### Ações
- Erro de banco (`connection refused`, `too many clients`): ver [Conexões do banco](#conexoes-do-banco).
- Erro de um tenant específico: o identificador está no log; trate o dado pela API.
- Tarefas são idempotentes: depois da correção, a próxima execução recupera o atraso.

### Verificação
`increase(recorvia_job_runs_total{outcome="failure"}[30m])` volta a zero e
`recorvia_job_last_success_timestamp_seconds` avança para a tarefa.

<a id="falhas-de-cobranca"></a>

## Falhas de cobrança

### Sintomas
- Faturas emitidas sem cobrança `awaiting_payment`; cobranças `failed`; operações `pending` em
  acúmulo.

### Métricas e alertas
- `RecorviaChargesDeclined`: mais de 5 recusas definitivas (`kind="permanent"`) por provedor em
  30 minutos (validação, credencial ou cadastro do cliente).
- `RecorviaPaymentProviderUnavailable` (crítico): falhas `retryable` ou `ambiguous` repetidas em
  10 minutos.
- `RecorviaPaymentOperationsPending`: mais de 50 operações aguardando nova tentativa.
- `recorvia_charge_failures_total{provider, kind}` e `recorvia_payment_operations{status}`
  (dashboard *visão geral*).

### Diagnóstico
```sql
-- Agregado por situação e erro (superusuário, sem expor dados de clientes).
SELECT tenant_id, kind, status, count(*), max(attempts), left(max(last_error), 200) AS exemplo_erro
  FROM payment_operations
 WHERE status IN ('pending', 'in_flight', 'unknown', 'failed') AND updated_at > now() - interval '1 day'
 GROUP BY 1, 2, 3 ORDER BY 4 DESC;
-- Cobranças recusadas recentemente por tenant.
SELECT tenant_id, provider, count(*) FROM charges
 WHERE status = 'failed' AND updated_at > now() - interval '1 day' GROUP BY 1, 2;
```
- API (permissão `payments.read`): `GET /api/v1/payment-operations` e `GET /api/v1/charges?status=failed`.
- Provedor: no ambiente local, `docker compose logs paysim`; no Asaas, o painel e a página de
  status do provedor.
- `kind="permanent"` concentrado num tenant: credencial inválida ou revogada
  (`GET /api/v1/payment-providers`). Espalhado entre tenants: mudança de contrato do provedor.

### Ações
1. Provedor indisponível: nenhuma ação de dados. As operações `pending` são retomadas pela
   tarefa `payments.recover_operations` (backoff exponencial até 30 minutos, 6 tentativas); as
   `unknown` são consultadas pela referência externa antes de qualquer nova criação (ADR-008).
2. Credencial inválida: o tenant reconfigura o provedor
   (`PUT /api/v1/payment-providers/{provider}`, permissão `integrations.write`); depois, nova
   tentativa por fatura com `POST /api/v1/invoices/{id}/charges` (permissão `payments.charge`,
   com `Idempotency-Key`).
3. Operação específica dentro do limite de tentativas: `POST /api/v1/payment-operations/{id}/retry`.
   Operações que esgotaram as tentativas não são reexecutadas por esse endpoint: seguem para a
   conciliação ([runbook](conciliacao.md#operacoes-com-resultado-desconhecido)).
4. Nunca recrie uma cobrança diretamente no provedor nem altere `payment_operations` à mão: uma
   cobrança duplicada é o risco que o fluxo evita.

### Verificação
- `increase(recorvia_charge_failures_total[10m])` volta a zero; `recorvia_payment_operations{status="pending"}`
  diminui.
- As faturas afetadas têm cobrança `awaiting_payment` (ou `paid`) e o evento `charge.created` na
  trilha (`GET /api/v1/audit-events`).

## Webhooks recusados

### Sintomas
- Pagamentos confirmados no provedor sem reflexo na fatura.

### Métricas e alertas
- `RecorviaWebhooksRejected`: mais de 5 recusas (`unauthenticated`, `unknown_endpoint` ou
  `error`) por provedor e resultado em 15 minutos.

### Diagnóstico
- `unauthenticated`: token (Asaas) ou HMAC (simulador) divergente; segredo rotacionado no
  provedor sem reconfigurar o Recorvia, ou tentativa de envio falso.
- `unknown_endpoint`: URL com token antigo (conta reconfigurada) ou varredura.
- `error`: falha ao persistir o evento (banco): ver [Conexões do banco](#conexoes-do-banco).
```bash
docker compose logs --since 1h api | grep -E 'webhook' | tail -50
```

### Ações
- Reconfigure a conta do provedor pela API (gera novo token de endpoint e registra o webhook). O
  provedor reenvia os eventos pendentes conforme a política dele; a conciliação cobre os que não
  forem reenviados.
- Recusas por varredura não exigem ação além de acompanhar o volume.

### Verificação
`recorvia_provider_webhooks_total{outcome="accepted"}` volta a crescer e as faturas afetadas são
quitadas.

<a id="conexoes-do-banco"></a>

## Conexões do banco

### Sintomas
- Latência alta na API, timeouts, erros `too many clients already` ou
  `context deadline exceeded` na aquisição de conexão.

### Métricas e alertas
- `RecorviaDBPoolSaturated`: conexões em uso acima de 90% do pool de um processo por 5 minutos.
- `RecorviaDBPoolWaiting`: espera por conexão acima de 0,2 s por segundo.
- `RecorviaDBServerConnectionsHigh` (crítico): conexões de clientes acima de 80% do limite para
  papéis comuns (`max_connections` menos as reservadas).
- Dashboard *Recorvia: banco e processos*.

### Diagnóstico
```sql
-- Conexões por aplicação e estado.
SELECT application_name, usename, state, count(*), max(now() - state_change) AS maior_tempo_no_estado
  FROM pg_stat_activity WHERE backend_type = 'client backend' GROUP BY 1, 2, 3 ORDER BY 4 DESC;
-- Transações longas e bloqueios.
SELECT pid, application_name, state, now() - xact_start AS duracao, wait_event_type, wait_event, left(query, 120)
  FROM pg_stat_activity WHERE xact_start IS NOT NULL ORDER BY xact_start LIMIT 20;
SELECT blocked.pid AS bloqueado, blocking.pid AS bloqueador, left(blocking.query, 120)
  FROM pg_stat_activity blocked
  JOIN pg_stat_activity blocking ON blocking.pid = ANY (pg_blocking_pids(blocked.pid));
```
- `application_name` distingue `recorvia-api` e `recorvia-worker` (definido no pool).
- `idle in transaction` prolongado indica transação aberta durante chamada externa (não deveria
  ocorrer: as chamadas ao provedor acontecem fora de transação).

### Ações
1. Pico momentâneo de tráfego: aumentar `RECORVIA_DATABASE_MAX_CONNS` só se o servidor tiver folga
   (`recorvia_db_server_connections` bem abaixo do limite); caso contrário, reduzir concorrência
   (`RECORVIA_WORKER_CONCURRENCY`) ou escalar o banco.
2. Bloqueio por transação longa: identificar a origem pelo log e encerrar a sessão com
   `SELECT pg_cancel_backend(<pid>);` (cancela a consulta) antes de `pg_terminate_backend`.
3. Ordem de bloqueio inesperada (impasses detectados pelo PostgreSQL): registrar a consulta e
   comparar com a ordem da ADR-031.

### Verificação
`recorvia_db_pool_connections{state="acquired"}` abaixo de 70% do máximo e
`rate(recorvia_db_pool_empty_acquire_wait_seconds_total[5m])` próximo de zero.

## Erros internos da API

### Sintomas
- Respostas 5xx na interface ou nas integrações.

### Métricas e alertas
- `RecorviaHTTPErrorRate`: mais de 5% de respostas 5xx por 10 minutos.
- `recorvia_http_requests_total{status_class="5xx"}` por `route` (dashboard *visão geral*).

### Diagnóstico
```bash
curl -s http://localhost:18081/readyz   # PostgreSQL e Redis
docker compose logs --since 30m api | grep -E '"level":"ERROR"' | tail -50
```
- O corpo de erro (RFC 9457) traz `request_id`; procure-o nos logs (`request_id`) e no rastro do
  Jaeger (atributo do span HTTP).
- `/readyz` com `redis` indisponível: sessões e limites de taxa falham (ADR-012).

### Ações
- Dependência indisponível: restabelecer o serviço; a API reconecta sozinha.
- Erro de código: abrir correção com o `request_id`, a rota e o rastro.

### Verificação
`sum(rate(recorvia_http_requests_total{status_class="5xx"}[5m]))` volta a zero e `/readyz`
responde 200.
