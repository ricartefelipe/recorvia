# Runbook: filas, outbox e DLQ

Cobre a publicação de eventos (Transactional Outbox, ADR-009), as filas do RabbitMQ com
retentativas escalonadas e DLQ (ADR-010), o registro e o reprocessamento de dead letters
(ADR-024) e a capacidade do worker. Topologia: exchange `recorvia.events`; para cada consumidor,
a fila `recorvia.<consumidor>`, as filas de espera `recorvia.<consumidor>.retry.1` a `.retry.5` e a
DLQ `recorvia.<consumidor>.dlq`. Consumidores atuais: `payments.invoice_issued`,
`payments.webhooks`, `payments.invoice_voided` e `payments.invoice_credited`.

## Outbox acumulada ou parada

### Sintomas
- Faturas emitidas sem cobrança, webhooks recebidos sem efeito, notas de crédito sem ajuste da
  cobrança: os eventos foram gravados, mas não chegaram ao broker.

### Métricas e alertas
- `RecorviaOutboxStalled` (crítico): evento pendente mais antigo com mais de 5 minutos.
- `RecorviaOutboxBacklog`: mais de 1000 eventos pendentes por 10 minutos.
- `RecorviaOutboxPublishFailures`: falhas de publicação contínuas por 10 minutos.
- `recorvia_outbox_pending`, `recorvia_outbox_oldest_pending_seconds`,
  `recorvia_outbox_published_total`, `recorvia_outbox_publish_failures_total` (dashboard
  *Recorvia: mensageria*).

### Diagnóstico
```sql
-- Pendentes por tipo, tentativas e último erro (superusuário).
SELECT event_type, count(*), min(occurred_at) AS mais_antigo, max(attempts) AS tentativas, left(max(last_error), 200) AS erro
  FROM outbox_events WHERE published_at IS NULL GROUP BY 1 ORDER BY 3;
-- Reivindicações (lease) em andamento.
SELECT claimed_by, count(*), min(claim_expires_at), max(claim_expires_at)
  FROM outbox_events WHERE published_at IS NULL GROUP BY 1;
```
```bash
docker compose logs --since 30m worker | grep -E 'outbox|broker|publica'
docker compose exec rabbitmq rabbitmq-diagnostics -q status | head -40
docker compose exec rabbitmq rabbitmq-diagnostics -q alarms   # memória ou disco
```
- `last_error` com `sem conexão com o broker` ou `nack`: broker indisponível ou em alarme de
  recursos (ver [Processo ou broker indisponível](#processo-ou-broker-indisponivel)).
- Pendentes sem `claimed_by` e sem erro: o relay não está rodando (worker parado ou sem conexão
  inicial com o broker; o relay só começa depois de declarar a topologia).
- `recorvia_outbox_published_total` crescendo e pendentes também: volume acima da vazão; o lote
  (`RECORVIA_OUTBOX_BATCH_SIZE`, padrão 100) e o intervalo do relay limitam a taxa.

### Ações
1. Restabelecer o broker ou o worker; o relay retoma sozinho, e eventos com lease vencido voltam a
   ser reivindicados (publicação pelo menos uma vez; os consumidores deduplicam pelo inbox).
2. Alarme de disco ou memória do RabbitMQ: liberar espaço ou aumentar o limite; o broker bloqueia
   publicadores enquanto o alarme durar.
3. Não marque eventos como publicados nem os apague à mão: um evento não publicado é uma cobrança
   ou um pagamento que não será processado.

### Verificação
`recorvia_outbox_oldest_pending_seconds` volta a poucos segundos, `recorvia_outbox_pending` cai e
as filas de destino recebem as mensagens.

## Fila acumulada ou atrasada

### Sintomas
- Eventos publicados, mas processados com atraso (cobranças criadas minutos depois da emissão).

### Métricas e alertas
- `RecorviaQueueBacklog`: mais de 500 mensagens prontas numa fila principal por 10 minutos.
- `RecorviaQueueLag`: mensagem na cabeça de uma fila principal ocorrida há mais de 5 minutos.
- `rabbitmq_detailed_queue_messages_ready`, `rabbitmq_detailed_queue_messages_unacked`,
  `rabbitmq_detailed_queue_head_message_timestamp` e `recorvia_consumer_processing_seconds`.

### Diagnóstico
```bash
docker compose exec rabbitmq rabbitmqctl list_queues name messages_ready messages_unacknowledged consumers
docker compose exec rabbitmq rabbitmqctl list_consumers
```
- `messages_unacknowledged` igual a `prefetch × instâncias` e duração p95 alta: o handler está
  lento (provedor de pagamento lento ou banco saturado).
- Prontas crescendo com consumidores conectados e `recorvia_consumer_in_flight` no máximo: falta
  capacidade (ver [Worker saturado](#worker-saturado)).
- O carimbo da mensagem é o instante do evento: uma mensagem que volta de uma fila de espera
  `.retry.N` carrega o carimbo original e pode parecer atrasada por alguns instantes; o alerta
  exige 5 minutos contínuos.

### Ações
- Handler lento por dependência externa: ver o runbook de [faturamento](faturamento.md#falhas-de-cobranca)
  ou [conexões do banco](faturamento.md#conexoes-do-banco).
- Capacidade: aumentar `RECORVIA_WORKER_CONCURRENCY` (e `RECORVIA_WORKER_PREFETCH`, que precisa ser
  maior ou igual) ou o número de instâncias do worker, observando as conexões do banco.

### Verificação
`rabbitmq_detailed_queue_messages_ready` volta a zero entre picos e o atraso da cabeça da fila
fica abaixo de alguns segundos.

<a id="processo-ou-broker-indisponivel"></a>

## Processo ou broker indisponível

### Sintomas
- Nenhuma métrica do worker ou da API; filas sem consumidores; `/readyz` com 503.

### Métricas e alertas
- `RecorviaTargetDown` (crítico): coleta falhando para `recorvia-api`, `recorvia-worker` ou
  `rabbitmq`.
- `RecorviaQueueWithoutConsumer` (crítico): fila principal sem consumidor por 5 minutos.
- `RecorviaDependencyUnavailable` (crítico): `recorvia_dependency_up == 0` por 2 minutos (RabbitMQ
  no worker, a cada `metrics.collect`; PostgreSQL e Redis na API, verificados em paralelo a cada
  coleta do Prometheus, cada um com prazo próprio de 2 s, sem depender de consultas a `/readyz`).
  A lentidão de uma dependência não marca a outra como indisponível: com o pool do banco esgotado
  (`RecorviaDBPoolSaturated`), só `postgres` pode cair para 0. O alerta acompanha o estado atual:
  resolve-se sozinho quando a dependência volta.

### Diagnóstico
```bash
docker compose ps
curl -s http://localhost:18081/livez; curl -s http://localhost:18081/readyz
docker compose exec worker /app healthcheck && echo worker-vivo
docker compose logs --since 15m worker | grep -E 'broker|conectado|consumo'
docker compose exec rabbitmq rabbitmq-diagnostics -q check_running
```
- `/livez` responde e `/readyz` não: o processo está vivo e espera a dependência (ADR-054); não é
  necessário reiniciá-lo.
- Worker sem `conectado ao broker` nos logs: credenciais (`RECORVIA_AMQP_URL`) ou rede.
- Worker parado: as conciliações pedidas pela API ficam registradas em `running`, sem quem as
  execute, e a conta do provedor responde `409 reconciliation_running` a novos pedidos. A API
  exporta essa espera (`recorvia_reconciliation_oldest_waiting_seconds`), e
  `RecorviaReconciliationRunsWaiting` dispara quando ela passa de 5 minutos e assim permanece por
  mais 5 (`> 300` com `for: 5m`): o alerta aparece pouco mais de 10 minutos depois do registro da
  execução, não aos 5. Antes disso, a ausência do alerta não indica que não haja execução presa; a
  consulta das execuções em `running` está em [Execução parada](conciliacao.md#execucoes-paradas).

### Ações
1. Restabelecer a dependência; a API e o worker reconectam sozinhos (backoff de até 30 s no broker).
2. Processo parado (`/livez` sem resposta): reiniciar o container. No encerramento, o worker
   aguarda as mensagens em andamento (até 2 minutos). Na partida, o worker assume sozinho as
   conciliações que ficaram aguardando.
3. Fila sem consumidor com o worker conectado: o canal do consumidor caiu; o worker reabre o canal a
   cada 2 segundos. Persistindo, reiniciar o worker e registrar o erro do log.

### Verificação
`up` volta a 1 para os alvos, `rabbitmq_detailed_queue_consumers` maior que zero em todas as filas
principais e `/readyz` com 200.

## Mensagens na DLQ

### Sintomas
- Evento que não produziu efeito (por exemplo, webhook de pagamento sem quitar a fatura) e
  registro em *Operações* na interface.

### Métricas e alertas
- `RecorviaMessagesDeadLettered`: mensagens enviadas à DLQ por um consumidor nos últimos
  15 minutos (o contador começa em zero na partida do worker, então a primeira ocorrência alerta).
- `recorvia_consumer_messages_total{outcome="dead_letter"}`.

### Diagnóstico
```sql
-- Dead letters abertas por consumidor e erro (superusuário; registros sem tenant incluídos).
SELECT consumer, event_type, tenant_id IS NULL AS sem_tenant, count(*), max(failed_at), left(max(error), 200)
  FROM dead_letters WHERE status = 'open' GROUP BY 1, 2, 3 ORDER BY 5 DESC;
```
- API, com a permissão `operations.read`: `GET /api/v1/dead-letters?status=open` (somente as do
  tenant da sessão).
- `permanente:` no erro: mensagem inválida ou regra de negócio definitiva, sem retentativa.
  Demais erros: as 5 retentativas escalonadas (2 s, 10 s, 1 min, 5 min e 15 min, com jitter) se
  esgotaram.
- Registros sem tenant: mensagem sem tenant válido; visíveis apenas para a operação (consulta acima).

### Ações
1. Corrigir a causa (dado, configuração ou código) antes de reprocessar.
2. Reprocessar, com a permissão `operations.reprocess` e justificativa:
   `POST /api/v1/dead-letters/{id}/replay` com `{"reason": "..."}`. A reentrega vai somente para a
   fila do consumidor que falhou e preserva o id original, de modo que o inbox continua
   deduplicando (ADR-024). A ação é auditada.
3. Mensagem sem efeito a aplicar (duplicada, obsoleta): `POST /api/v1/dead-letters/{id}/discard`
   com justificativa (auditado).

### Verificação
A dead letter passa a `replayed` (ou `discarded`), o efeito esperado aparece (por exemplo, fatura
paga) e `increase(recorvia_consumer_messages_total{outcome="dead_letter"}[15m])` volta a zero.

<a id="dlq-nao-esvaziada"></a>

## DLQ não esvaziada

### Sintomas
- Mensagens paradas nas filas `.dlq` do RabbitMQ (o registrador do worker deveria movê-las para
  a tabela `dead_letters` imediatamente).

### Métricas e alertas
- `RecorviaDeadLetterQueueNotDrained`: mensagens numa `.dlq` por 10 minutos.

### Diagnóstico
```bash
docker compose exec rabbitmq rabbitmqctl list_queues name messages consumers | grep '\.dlq'
docker compose logs --since 30m worker | grep 'registro de DLQ falhou'
```
- Sem consumidor na `.dlq`: worker parado ou canal fechado.
- `registro de DLQ falhou`: o banco recusou a gravação (indisponível, conexões esgotadas).

### Ações
Restabelecer o banco ou o worker. O registrador devolve a mensagem à DLQ a cada falha e tenta de
novo (sem perda). Não remova mensagens das filas `.dlq` pela interface do RabbitMQ.

### Verificação
`rabbitmq_detailed_queue_messages{queue=~".+\\.dlq"}` volta a zero e os registros aparecem em
`dead_letters`.

## Retentativas elevadas

### Sintomas
- Processamento lento ou intermitente; muitas mensagens nas filas `.retry.N`.

### Métricas e alertas
- `RecorviaConsumerRetryRate`: mais de 10% de retentativas por consumidor por 15 minutos.
- `RecorviaMessagesRequeued`: retentativa ou DLQ não publicadas e mensagem devolvida à fila
  principal (broker com problema de publicação).
- Painel "Mensagens aguardando retentativa" (dashboard *mensageria*).

### Diagnóstico
```bash
docker compose logs --since 30m worker | grep 'retentativa agendada' | tail -20
```
O log traz `consumer`, `message_id`, `attempt` e o erro. O tenant e o evento são obtidos pelo
`message_id` (id do evento da outbox: `SELECT tenant_id, event_type FROM outbox_events WHERE id = '<message_id>'`,
enquanto o evento não for expurgado).

### Ações
- Erro transitório de dependência: ação na dependência; as retentativas absorvem a falha.
- Erro determinístico (sempre o mesmo para a mesma mensagem): a mensagem chegará à DLQ; corrigir a
  causa e seguir [Mensagens na DLQ](#mensagens-na-dlq).
- `requeue`: tratar como broker indisponível ou em alarme.

### Verificação
A razão `retry/total` volta abaixo de 10% e as filas `.retry.N` esvaziam.

## Worker saturado

### Sintomas
- Filas crescendo com consumidores ativos; processamento contínuo no limite.

### Métricas e alertas
- `RecorviaWorkerSaturated`: `recorvia_consumer_in_flight / recorvia_consumer_concurrency >= 0.9`
  por 15 minutos num consumidor.

### Diagnóstico
- Comparar a duração p95 (`recorvia_consumer_processing_seconds`) com a taxa de chegada
  (`rate(rabbitmq_queue_messages_published_total[5m])` no nó ou o crescimento das filas).
- Duração alta: dependência lenta. Duração normal com saturação: volume acima da capacidade.

### Ações
- Aumentar a concorrência (`RECORVIA_WORKER_CONCURRENCY`, padrão 8; no compose local, 4) ou o
  número de instâncias do worker, conferindo antes as conexões do banco
  (`recorvia_db_pool_max_connections` de todas as instâncias abaixo do limite do servidor).

### Verificação
A saturação cai abaixo de 0,9 e as filas principais esvaziam.
