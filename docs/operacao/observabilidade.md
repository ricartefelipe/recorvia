# Observabilidade

Como o Recorvia expõe métricas, rastros e sondas de saúde, como subir a stack local de
observabilidade, como os alertas são encaminhados e o que cada um significa. Decisões: ADR-052
(stack e configurações nas imagens), ADR-053 (métricas e alertas de baixa cardinalidade), ADR-054
(liveness e readiness), ADR-065 (Alertmanager), ADR-066 (métricas globais de fila) e ADR-067
(alertas das filas e do caminho de alertas).
Procedimentos de resposta aos alertas: [runbooks](runbooks/README.md).

## Subir a stack local

```bash
make up-obs          # ambiente completo + OpenTelemetry Collector, Jaeger, Prometheus, Alertmanager e Grafana
make obs-stack-check # confere alvos, regras, séries, um alerta até o e-mail, dashboards e rastros da stack em execução
make obs-check       # valida as configurações com as ferramentas oficiais (sem subir o ambiente)
```

`make up-obs` sobe o perfil `observability` do `compose.yaml` e define
`OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317` para a API e o worker; com `make up`, a
exportação de rastros fica desligada (apenas a propagação de contexto). As configurações entram
nas imagens por `COPY` em Dockerfiles próprios em `deploy/observability/` (ADR-020: sem bind
mounts); alterar uma configuração exige reconstruir a imagem, o que `make up-obs` faz (`--build`).

| Serviço | Endereço (somente `127.0.0.1`) | Variável da porta | Observação |
|---|---|---|---|
| Grafana | http://localhost:3000 | `GRAFANA_PORT` | leitura anônima; administrador `admin` com `GRAFANA_ADMIN_PASSWORD` do `.env` |
| Prometheus | http://localhost:9090 | `PROMETHEUS_PORT` | consultas, alvos (`/targets`) e alertas (`/alerts`) |
| Alertmanager | http://localhost:9093 | `ALERTMANAGER_PORT` | alertas recebidos, silêncios e inibições; as notificações por e-mail ficam no Mailpit (http://localhost:8025) |
| Jaeger | http://localhost:16686 | `JAEGER_UI_PORT` | rastros da API e do worker |

As portas de métricas (`api:9091`, `worker:9091`, `rabbitmq:15692`, `otel-collector:8888`) não
são publicadas no host: só o Prometheus, na rede do compose, as acessa. O Alertmanager é coletado
em `alertmanager:9093`.

| Componente | Imagem (versão fixa) | Configuração |
|---|---|---|
| OpenTelemetry Collector | `otel/opentelemetry-collector-contrib:0.165.0` | `deploy/observability/otel-collector/config.yaml` |
| Jaeger v2 | `jaegertracing/jaeger:2.23.0` | `deploy/observability/jaeger/config.yaml` (armazenamento em memória) |
| Prometheus | `prom/prometheus:v3.16.0` | `deploy/observability/prometheus/prometheus.yml` e `rules/recorvia.yml` |
| Alertmanager | `prom/alertmanager:v0.35.0` | `deploy/observability/alertmanager/alertmanager.yml` (roteamento), `templates/recorvia.tmpl` (notificações) e `entrypoint.sh` (receptores gerados na partida) |
| Grafana | `grafana/grafana:13.3.1` | `deploy/observability/grafana/provisioning` e `dashboards/*.json` |

## Liveness e readiness

As sondas têm semânticas distintas (RNF-09, ADR-054) e respondem JSON com `Cache-Control: no-store`:

| Processo | Porta | `/livez` | `/readyz` |
|---|---|---|---|
| API | HTTP (`RECORVIA_HTTP_ADDR`, 8081 no compose) | 200 enquanto o processo atende; não consulta dependências | 200 com PostgreSQL e Redis acessíveis; 503 caso contrário, com `checks` por dependência |
| Worker | métricas (`RECORVIA_METRICS_ADDR`, 9091 no compose) | 200 enquanto o processo atende | 200 com PostgreSQL acessível e conexão ativa com o RabbitMQ; 503 caso contrário, com `checks` |

```bash
curl -s http://localhost:18081/livez    # {"status":"ok"}
curl -s http://localhost:18081/readyz   # {"checks":{"postgres":"ok","redis":"ok"},"status":"ok"}
docker compose exec worker /app healthcheck   # consulta /livez do próprio worker
```

O Keycloak fica fora da prontidão da API (ADR-021): a sessão já autenticada não depende dele. O
healthcheck dos containers (`/app healthcheck`) usa `/livez`, para que uma dependência fora do ar
não reinicie a API ou o worker (ambos reconectam sozinhos). Em orquestradores, use `/livez` como
sonda de vida e `/readyz` como sonda de prontidão. Testes: `TestLivenessAndReadinessHaveDistinctSemantics`
(`internal/httpapi`), `TestWorkerHealthMuxSeparatesLivenessFromReadiness` e
`TestHealthcheckQueriesLiveness` (`internal/app`).

Como nada consulta `/readyz` da API no compose, `recorvia_dependency_up` da API não depende dele:
as mesmas verificações de PostgreSQL e Redis rodam a cada coleta do Prometheus e o valor acompanha
o estado atual, inclusive na recuperação (`TestDependencyUpIsProbedOnEachGather`,
`internal/infra/telemetry`). As verificações rodam em paralelo, cada uma com prazo próprio de 2 s:
um PostgreSQL lento não faz o Redis aparecer como indisponível
(`TestDependencyChecksHaveIndependentDeadlines`). `/readyz` também atualiza o valor.

## Métricas

Todas as métricas da aplicação têm o prefixo `recorvia_` e o rótulo constante `service` (`api` ou
`worker`). Nenhuma tem rótulo de tenant, cliente, fatura, usuário ou mensagem (RNF-08); o teste
`TestMetricsHaveNoHighCardinalityLabels` falha se um rótulo proibido ou reservado (`job`,
`instance`) for declarado, e os rótulos alimentados por dados externos têm conjunto fechado: o
método HTTP fora da lista padronizada vira `_OTHER` e o provedor de webhook fora da lista vira
`other` (`TestArbitraryHTTPMethodsDoNotCreateSeries`). Os contadores de consumidores, tarefas,
falhas de cobrança, webhooks recebidos, envios de notificações, entregas de webhooks, ações da
régua e do expurgo por retenção são criados com zero na partida, para que `increase()` detecte a
primeira ocorrência.
`TestMetricsCatalogDocumentsEveryMetricAndLabelValue` confere que toda métrica declarada está na
tabela abaixo, com os valores dos rótulos de conjunto fechado.

| Métrica | Tipo | Rótulos | Origem |
|---|---|---|---|
| `recorvia_http_requests_total` | contador | `route`, `method`, `status_class` | API, padrão de rota (nunca a URL concreta); `method` é `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `CONNECT`, `OPTIONS`, `TRACE` ou `_OTHER` (qualquer outro token, também no nome do span e no log de acesso) |
| `recorvia_http_request_duration_seconds` | histograma | `route`, `method` | API, mesmo método normalizado |
| `recorvia_provider_webhooks_total` | contador | `provider`, `outcome` | API, recebimento de webhooks: `accepted`, `duplicate`, `unauthenticated`, `unknown_endpoint`, `invalid` (corpo malformado ou recusado na validação), `too_large` (corpo acima de 256 KiB) e `error` (falha de persistência); `provider` é `simulator`, `asaas` ou `other` (nome fora da lista, vindo da URL) |
| `recorvia_dependency_up` | gauge | `dependency` | API: `postgres` e `redis`, verificados em paralelo a cada coleta do Prometheus (prazo de 2 s para cada um) e a cada `/readyz`; worker: `rabbitmq`, a cada `metrics.collect` |
| `recorvia_consumer_messages_total` | contador | `consumer`, `outcome` | worker (`ack`, `retry`, `dead_letter`, `requeue`) |
| `recorvia_consumer_processing_seconds` | histograma | `consumer` | worker |
| `recorvia_consumer_in_flight` / `recorvia_consumer_concurrency` | gauge | `consumer` | worker (saturação) |
| `recorvia_outbox_published_total` / `recorvia_outbox_publish_failures_total` | contador | — | worker, relay da outbox |
| `recorvia_outbox_pending` / `recorvia_outbox_oldest_pending_seconds` | gauge | — | worker, tarefa `metrics.collect` (15 s) |
| `recorvia_job_runs_total` | contador | `task`, `outcome` | worker, agendador (`success`, `failure`); a falha da reivindicação (banco) não conta: aparece no log `reivindicação de tarefa falhou` e nos alertas de tarefa sem sucesso |
| `recorvia_job_duration_seconds` | histograma | `task` | worker |
| `recorvia_job_last_success_timestamp_seconds` | gauge | `task` | worker; só existe depois de um sucesso real (os alertas tratam a ausência pela idade do processo, `process_start_time_seconds`) |
| `recorvia_job_interval_seconds` | gauge | `task` | worker, intervalo configurado de cada tarefa (alerta proporcional `RecorviaScheduledTaskStale`) |
| `recorvia_invoices_issued_total` | contador | — | worker, motor de faturamento |
| `recorvia_charge_failures_total` | contador | `provider`, `kind` | API e worker, falhas de cadastro do cliente e criação de cobrança no provedor (`permanent`, `retryable`, `ambiguous`, `not_found`) |
| `recorvia_payment_operations` | gauge | `status` | worker, `metrics.collect` (`pending`, `in_flight`, `unknown`), inclusive as esgotadas |
| `recorvia_payment_operations_exhausted` | gauge | `status` | worker, tarefa `payments.recover_operations` (a cada minuto): operações paradas no limite de 6 tentativas, que a recuperação não retoma mais (`pending` e `unknown` depois do horário da próxima tentativa, `in_flight` com lease vencido), sem as criações `pending` cuja cobrança deixou de estar pendente (fatura anulada); uma execução parcial (tenant sem consulta) mantém o valor anterior |
| `recorvia_notifications_dispatched_total` | contador | `outcome` | worker, tarefa `notifications.dispatch`: `sent` (aceita pelo SMTP), `retried` (falha transitória, nova tentativa agendada), `failed` (encerrada sem envio: recusa permanente, fim das tentativas ou conteúdo indisponível), `deferred` (fora da janela de envio ou do limite de frequência), `skipped` (cancelada ou suprimida antes do envio) e `lost_lease` (outra execução assumiu a tentativa) |
| `recorvia_webhook_deliveries_total` | contador | `outcome` | worker, tarefa `webhooks.deliver`: `succeeded` (resposta 2xx), `retrying` (falha, nova tentativa agendada), `exhausted` (tentativas esgotadas ou destino bloqueado pela proteção SSRF), `lost_lease` e `deferred` (devolvida à fila sem consumir tentativa: orçamento da execução ou endpoint sem resposta) |
| `recorvia_collection_actions_total` | contador | `action` | worker, tarefa `collections.run`: `reminder_scheduled`, `invoice_overdue`, `subscription_suspended` e `subscription_reactivated` |
| `recorvia_retention_purged_total` | contador | `kind` | worker, tarefa `retention.purge` (a cada hora): registros removidos ou esvaziados pelo expurgo por idade, por tipo de dado: `inbox_messages`, `dead_letters`, `webhook_deliveries`, `webhook_events`, `webhook_endpoint_secrets`, `provider_webhook_payloads`, `notification_contents`, `notifications`, `portal_links`, `usage_events` e `invitations` |
| `recorvia_retention_purge_failures_total` | contador | `kind` | worker, tarefa `retention.purge`: execuções em que o expurgo do tipo falhou em algum tenant ou na descoberta (`inbox_messages`, `dead_letters`, `webhook_deliveries`, `webhook_events`, `webhook_endpoint_secrets`, `provider_webhook_payloads`, `notification_contents`, `notifications`, `portal_links`, `usage_events` e `invitations`); a tarefa termina sem sucesso |
| `recorvia_retention_purge_incomplete_total` | contador | `kind` | worker, tarefa `retention.purge`: execuções interrompidas pelo orçamento de tempo ou pelo limite de lotes com registros vencidos restantes do tipo (`inbox_messages`, `dead_letters`, `webhook_deliveries`, `webhook_events`, `webhook_endpoint_secrets`, `provider_webhook_payloads`, `notification_contents`, `notifications`, `portal_links`, `usage_events` e `invitations`) |
| `recorvia_dead_letters_open` / `recorvia_dead_letters_oldest_open_seconds` | gauge | `consumer` | worker, `metrics.collect`: dead letters registradas em `dead_letters` e ainda não resolvidas, em todos os tenants e as sem tenant, e a idade da mais antiga (tempo desde a falha; zero sem dead letters abertas); `consumer` é um consumidor do worker ou `other` (registros de consumidores removidos ou renomeados) |
| `recorvia_notifications_backlog` / `recorvia_notifications_backlog_oldest_seconds` | gauge | `state` | worker, `metrics.collect`: fila de notificações de todos os tenants. `due`: agendadas com o horário de envio vencido (idade: atraso em relação a esse horário); `sending`: reivindicadas para envio (idade: tempo desde a reivindicação); `failed`: falha definitiva nas últimas 24 horas (idade: tempo desde a mais antiga da janela). Agendamentos futuros não contam |
| `recorvia_webhook_deliveries_backlog` / `recorvia_webhook_deliveries_backlog_oldest_due_seconds` | gauge | `state` | worker, `metrics.collect`: fila de entregas de webhooks enviados de todos os tenants, em `pending` (aguardando a primeira tentativa ou um reenvio) e `retrying` (aguardando nova tentativa, inclusive em backoff); o atraso considera somente as entregas com o horário da tentativa vencido, a partir desse horário |
| `recorvia_reconciliation_open_discrepancies` | gauge | `kind` | worker, `metrics.collect`: divergências de conciliação abertas por tipo |
| `recorvia_reconciliation_runs_in_progress` | gauge | `state` | worker (`metrics.collect`) e API (lida do banco a cada coleta do Prometheus, com prazo de 2 s; se a leitura falhar, mantém o último valor): execuções de conciliação em andamento, de todos os tenants: `queued` (registrada, aguardando um worker), `executing` (lease vigente) e `stalled` (lease expirado, aguardando a retomada) |
| `recorvia_reconciliation_oldest_waiting_seconds` | gauge | — | worker (`metrics.collect`) e API (a cada coleta, como a métrica anterior): espera mais longa entre as execuções sem worker, desde o registro (`queued`) ou desde o fim do lease (`stalled`); zero quando não há nenhuma. A API a exporta porque registra os pedidos e continua em execução quando o worker para |
| `recorvia_db_pool_connections` | gauge | `state` | API e worker, pool pgx lido a cada coleta (`acquired`, `idle`, `constructing`) |
| `recorvia_db_pool_max_connections` | gauge | — | API e worker |
| `recorvia_db_pool_acquires_total`, `recorvia_db_pool_empty_acquires_total`, `recorvia_db_pool_canceled_acquires_total`, `recorvia_db_pool_empty_acquire_wait_seconds_total` | contador | — | API e worker |
| `recorvia_db_server_connections` / `recorvia_db_server_max_connections` | gauge | — | worker, `metrics.collect`: sessões de clientes de todo o servidor e limite para papéis comuns (`max_connections` menos as reservadas) |

Métricas de reembolsos, usadas pelos alertas `RecorviaRefundsUnknown` e `RecorviaRefundsFailed`:

| Métrica | Tipo | Rótulos | Origem |
|---|---|---|---|
| `recorvia_refunds_total` | contador | `outcome` | worker, `metrics.collect`: reembolsos concluídos (`succeeded`, `failed`), com totais mantidos no banco |
| `recorvia_refunds_in_progress` | gauge | `status` | worker, `metrics.collect`: reembolsos sem conclusão (`requested`, `unknown`) |

Avaliação das métricas da régua de cobrança, das notificações e dos webhooks enviados:

- `recorvia_notifications_dispatched_total`: alertas `RecorviaEmailDeliveryStalled` (tentativas
  falhando sem nenhum envio aceito, sinal de SMTP indisponível) e `RecorviaNotificationsFailed`
  (mais de 5 falhas definitivas em 30 minutos); painel "Notificações por resultado".
- `recorvia_webhook_deliveries_total`: alerta `RecorviaWebhookDeliveriesFailing` somente quando
  nenhum destino recebe (falha da saída do worker); a falha de um endpoint de tenant isolado não
  alerta a plataforma, porque as métricas não distinguem tenants e o reenvio é responsabilidade do
  tenant (histórico em `GET /api/v1/webhook-endpoints/{id}/deliveries`); painel "Entregas de
  webhooks por resultado".
- `recorvia_collection_actions_total`: sem alerta. Lembretes, vencimentos, suspensões e
  reativações dependem da carteira de cada tenant e não têm limite que indique falha; a falha da
  tarefa `collections.run` é coberta por `RecorviaScheduledTaskFailing`. Painel "Ações da régua de
  cobrança".

Métricas globais de fila (ADR-066). As três famílias acima são lidas do banco por funções
agregadas `SECURITY DEFINER` (`app_dead_letter_stats`, `app_notification_backlog_stats` e
`app_webhook_delivery_backlog_stats`, migration 00015), que somam todos os tenants sem
identificá-los e recebem o instante de referência do relógio da aplicação. Os rótulos têm conjunto
fechado (`TestDeadLetterStatsFeedGaugesWithClosedConsumerSet`,
`TestQueueStatsFeedGaugesWithDeclaredStates`), e os medidores só existem depois da primeira
coleta. Ao contrário dos contadores (`recorvia_consumer_messages_total{outcome="dead_letter"}`,
`recorvia_notifications_dispatched_total`, `recorvia_webhook_deliveries_total`), que contam o que
aconteceu desde a partida de cada worker, os medidores mostram o que está parado agora e não
zeram num reinício:

- dead letters abertas: alerta `RecorviaDeadLettersOpen` (aviso com 30 minutos sem triagem;
  crítico com a mais antiga aberta há mais de 24 horas); painéis "Dead letters abertas por
  consumidor" e "Idade da dead letter aberta mais antiga" (*mensageria*) e indicador na *visão
  geral*;
- fila de notificações: alerta `RecorviaNotificationsBacklog` (atraso de `due` ou `sending` acima
  de 15 minutos; crítico acima de uma hora); `failed` tem só painel, porque a falha definitiva já
  alerta por `RecorviaNotificationsFailed`;
- fila de entregas de webhooks: alerta `RecorviaWebhookDeliveriesBacklog` (entrega devida há mais
  de 15 minutos; crítico acima de uma hora). O total em `retrying` não alerta: a espera do backoff
  é o comportamento previsto para o destino de um tenant fora do ar.

O tipo de falha de cobrança vem da classificação do adaptador (`payments.GatewayErrorKind`): erro
não classificado conta como `ambiguous`. A contagem é feita por um decorador do adaptador na
composição (`gateways.Resolver.Observe`), sem alterar os casos de uso.

As conexões do servidor são contadas com o papel da aplicação, sem privilégio de leitura das
estatísticas: as sessões de outros papéis aparecem em `pg_stat_activity` com `backend_type` nulo,
mas com banco e papel, e entram na contagem (conferido por
`TestDatabaseConnectionsCountsAllClientsWithAppRole`).

Filas, pelo plugin `rabbitmq_prometheus` (habilitado na imagem `rabbitmq:4.2.9-management-alpine`),
no endpoint `/metrics/detailed` com as famílias `queue_coarse_metrics`, `queue_consumer_count` e
`queue_metrics` (coleta `rabbitmq-queues`). Cada consumidor tem a fila `recorvia.<consumidor>`,
as filas de espera `recorvia.<consumidor>.retry.N` e a DLQ `recorvia.<consumidor>.dlq`; os
rótulos das séries por fila são `vhost` e `queue` (e o tipo da fila em
`rabbitmq_detailed_queue_info`), sem dados de tenant.

| Série | Uso |
|---|---|
| `rabbitmq_detailed_queue_messages_ready`, `_messages_unacked`, `_messages` | profundidade, mensagens em processamento, retentativas e DLQs |
| `rabbitmq_detailed_queue_consumers` | fila principal sem consumidor |
| `rabbitmq_detailed_queue_head_message_timestamp` | atraso: `time()` menos o carimbo da mensagem na cabeça (em segundos), que o publicador preenche com o instante do evento |

`TestPrometheusPluginExposesQueueSeriesUsedByAlerts` (`internal/infra/rabbit`, tag `integration`)
publica mensagens com o publicador real e confere nomes, rótulos e valores dessas séries no
RabbitMQ 4.2.9.

## Alertas

Regras em `deploy/observability/prometheus/rules/recorvia.yml`, com testes em
`deploy/observability/prometheus/tests/recorvia_test.yml` (`promtool test rules`). Os limites são
pontos de partida para o ambiente local e devem ser calibrados com volume real. Os alertas
aparecem em http://localhost:9090/alerts e no Grafana e são enviados ao Alertmanager, que os
notifica ([Encaminhamento de alertas](#encaminhamento-de-alertas)). A anotação `runbook` de cada
regra aponta para uma seção existente (conferido por `TestAlertRulesLinkToExistingRunbookSections`);
a imagem do Prometheus acrescenta a cada regra a anotação `runbook_url`, com o endereço completo.

Severidades: `critical` exige ação imediata, `warning` exige análise no mesmo dia e `info` não
pede ação (somente `RecorviaWatchdog`). Um alerta com duas severidades (`RecorviaDeadLettersOpen`,
`RecorviaNotificationsBacklog`, `RecorviaWebhookDeliveriesBacklog`) tem duas regras com o mesmo
nome e os mesmos rótulos; enquanto a crítica dispara, o Alertmanager inibe a de aviso.

| Alerta | Condição | Severidade |
|---|---|---|
| `RecorviaTargetDown` | coleta da API, do worker, do RabbitMQ ou do Alertmanager falhando por 2 min | critical |
| `RecorviaOutboxBacklog` | mais de 1000 eventos pendentes por 10 min | warning |
| `RecorviaOutboxStalled` | evento pendente mais antigo com mais de 5 min, por 5 min | critical |
| `RecorviaOutboxPublishFailures` | falhas de publicação contínuas por 10 min | warning |
| `RecorviaQueueBacklog` | fila principal com mais de 500 mensagens prontas por 10 min | warning |
| `RecorviaQueueLag` | mensagem na cabeça da fila principal com mais de 5 min, por 5 min | warning |
| `RecorviaQueueWithoutConsumer` | fila principal sem consumidor por 5 min | critical |
| `RecorviaDeadLetterQueueNotDrained` | DLQ com mensagens não registradas em `dead_letters` por 10 min | warning |
| `RecorviaMessagesDeadLettered` | mensagens enviadas à DLQ nos últimos 15 min | warning |
| `RecorviaDeadLettersOpen` | dead letters abertas de um consumidor (todos os tenants) por 30 min | warning |
| `RecorviaDeadLettersOpen` | dead letter mais antiga de um consumidor aberta há mais de 24 h, por 15 min | critical |
| `RecorviaConsumerRetryRate` | mais de 10% de retentativas por 15 min | warning |
| `RecorviaMessagesRequeued` | mensagens devolvidas à fila principal nos últimos 10 min | warning |
| `RecorviaWorkerSaturated` | consumidor com 90% ou mais das vagas ocupadas por 15 min | warning |
| `RecorviaChargesDeclined` | mais de 5 recusas definitivas de cobrança em 30 min | warning |
| `RecorviaPaymentProviderUnavailable` | mais de 3 falhas repetíveis ou ambíguas em 10 min, por 5 min | critical |
| `RecorviaPaymentOperationsUnknown` | operação com resultado desconhecido por 30 min | warning |
| `RecorviaPaymentOperationsExhausted` | operação `pending` ou `in_flight` parada no limite de tentativas por 15 min | warning |
| `RecorviaPaymentOperationsPending` | mais de 50 operações pendentes por 15 min | warning |
| `RecorviaWebhooksRejected` | mais de 5 webhooks recusados (`unauthenticated`, `unknown_endpoint`, `too_large` ou `error`) em 15 min | warning |
| `RecorviaReconciliationDiscrepanciesOpen` | divergência aberta por 4 h | warning |
| `RecorviaReconciliationDiscrepanciesHigh` | mais de 25 divergências abertas por 30 min | critical |
| `RecorviaReconciliationRunsWaiting` | execução de conciliação sem worker (aguardando ou interrompida) há mais de 5 min, por 5 min; a métrica sai do worker e da API, e o alerta dispara mesmo sem nenhum worker em execução | warning |
| `RecorviaRefundsUnknown` | reembolso com resultado desconhecido por 30 min | warning |
| `RecorviaRefundsFailed` | reembolso recusado pelo provedor na última hora | warning |
| `RecorviaBillingCycleStale` | `billing.run_cycle` sem sucesso há mais de 15 min, ou sem nenhum sucesso desde a partida do worker (há mais de 15 min) | critical |
| `RecorviaScheduledTaskFailing` | 3 ou mais falhas de uma tarefa em 30 min (tarefas frequentes) | warning |
| `RecorviaScheduledTaskStale` | tarefa sem sucesso há mais de três intervalos (mínimo de 15 min), inclusive desde a partida do worker; cobre as tarefas horárias e a diária | warning |
| `RecorviaMetricsCollectionStale` | `metrics.collect` sem sucesso há mais de 2 min, ou sem nenhum sucesso desde a partida do worker | warning |
| `RecorviaRetentionPurgeStale` | `retention.purge` sem sucesso há mais de 3 h, ou sem nenhum sucesso desde a partida do worker (há mais de 3 h) | warning |
| `RecorviaRetentionPurgeBacklog` | 20 ou mais execuções do expurgo em 24 h interrompidas com registros vencidos restantes do mesmo tipo | warning |
| `RecorviaEmailDeliveryStalled` | tentativas de envio falhando e nenhum e-mail aceito em 15 min, por 15 min | warning |
| `RecorviaNotificationsFailed` | mais de 5 notificações com falha definitiva em 30 min | warning |
| `RecorviaWebhookDeliveriesFailing` | 10 ou mais tentativas de entrega falhando e nenhuma bem-sucedida em 30 min, por 15 min | warning |
| `RecorviaNotificationsBacklog` | notificação vencida (`due`) ou em envio (`sending`) há mais de 15 min, por 10 min | warning |
| `RecorviaNotificationsBacklog` | o mesmo, há mais de 1 h, por 10 min | critical |
| `RecorviaWebhookDeliveriesBacklog` | entrega de webhook devida (`pending` ou `retrying`) há mais de 15 min, por 10 min | warning |
| `RecorviaWebhookDeliveriesBacklog` | o mesmo, há mais de 1 h, por 10 min | critical |
| `RecorviaDBPoolSaturated` | pool de um processo com 90% ou mais em uso por 5 min | warning |
| `RecorviaDBPoolWaiting` | espera por conexão acima de 0,2 s por segundo, por 10 min | warning |
| `RecorviaDBServerConnectionsHigh` | servidor com mais de 80% das conexões para papéis comuns, por 10 min | critical |
| `RecorviaDependencyUnavailable` | dependência indisponível por 2 min (estado verificado a cada coleta) | critical |
| `RecorviaHTTPErrorRate` | mais de 5% de respostas 5xx por 10 min | warning |
| `RecorviaWatchdog` | sempre ativo: verificação do caminho de alertas | info |
| `RecorviaAlertDeliveryFailing` | envios do Prometheus ao Alertmanager falhando ou descartados em 10 min, por 5 min | critical |
| `RecorviaAlertNotificationsFailing` | notificações do Alertmanager falhando numa integração (`email`, `webhook`) em 15 min, por 5 min | critical |

O último sucesso de uma tarefa só existe depois de um sucesso real; num reinício do worker com a
tarefa falhando, ou com a reivindicação falhando a cada ciclo (sem execução registrada), a série
não existe. `RecorviaBillingCycleStale`, `RecorviaMetricsCollectionStale`,
`RecorviaRetentionPurgeStale` e `RecorviaScheduledTaskStale` cobrem esse caso pela idade do
processo mais antigo do worker (`process_start_time_seconds`), que é o mínimo do tempo sem sucesso.
A próxima execução de uma tarefa é agendada um intervalo depois, mesmo após falha:
`RecorviaScheduledTaskFailing` só alcança tarefas de até 10 minutos de intervalo, e as horárias
(`platform.close_statements`, `idempotency.purge`) e a diária (`outbox.purge`) ficam com o alerta
proporcional ao intervalo. O expurgo por retenção (`retention.purge`, horário) tem alerta próprio,
com runbook de retenção, e fica fora do alerta genérico.

A quantidade de dead letters registradas e ainda não resolvidas de todos os tenants é exportada
por consumidor (`recorvia_dead_letters_open`, ADR-066); o detalhe por tenant continua na API
(`GET /api/v1/dead-letters`), porque as métricas não identificam tenants.

<a id="encaminhamento-de-alertas"></a>

## Encaminhamento de alertas

O Prometheus envia os alertas ao Alertmanager (`alerting` em `prometheus.yml`), que agrupa,
inibe, roteia por severidade e notifica (ADR-065). O roteamento fica na imagem
(`deploy/observability/alertmanager/alertmanager.yml`); os receptores são gerados na partida do
container pelo `entrypoint.sh`, a partir de variáveis de ambiente, e a configuração resultante é
validada com `amtool check-config` antes de o Alertmanager iniciar. Procedimentos:
[runbook de encaminhamento de alertas](runbooks/alertas.md).

| Rota | Receptor | Agrupamento | Espera inicial | Intervalo do grupo | Repetição |
|---|---|---|---|---|---|
| `alertname="RecorviaWatchdog"` | `recorvia-watchdog` (somente e-mail) | `alertname` | 10 s | 5 min | 24 h |
| `severity="critical"` | `recorvia-critical` (e-mail dos críticos e webhook) | `alertname`, `area`, `severity` | 10 s | 2 min | 1 h |
| `severity="warning"` e demais | `recorvia-warning` (e-mail e webhook) | `alertname`, `area`, `severity` | 30 s | 5 min | 12 h |

- **Agrupamento**: uma notificação por alerta e severidade reúne os consumidores, filas ou
  instâncias afetados; alertas novos do mesmo grupo entram na notificação seguinte, depois do
  intervalo do grupo.
- **Inibição**: enquanto um alerta crítico está ativo, o aviso de mesmo nome e mesmos rótulos de
  agregação (`consumer`, `state`, `queue`, `provider`, `service`, `instance`, `task`, `kind`,
  `status`, `dependency`, `outcome`, `integration`) não é notificado; o aviso de outro consumidor
  ou de outra situação continua. `TestAlertRoutingCoversRuleSeveritiesAndInhibitionLabels` falha
  se uma regra agregar por um rótulo fora dessa lista.
- **Repetição**: um alerta que continua ativo é notificado de novo a cada intervalo de repetição;
  a resolução é notificada por e-mail e pelo webhook (exceto a do `RecorviaWatchdog`).
- **Link do runbook**: cada alerta da notificação traz o endereço completo da seção do runbook.
  A imagem do Prometheus deriva a anotação `runbook_url` de `runbook` com o argumento de
  construção `RUNBOOK_BASE_URL` (padrão `https://github.com/ricartefelipe/recorvia/blob/main/`;
  no compose, a variável de mesmo nome). O e-mail (texto e HTML) mostra o link, e o webhook
  recebe as duas anotações em `alerts[].annotations`.

Receptores, resolvidos na partida do container (alterar uma variável exige recriá-lo):

| Variável | Padrão no compose local | Uso |
|---|---|---|
| `ALERTMANAGER_SMTP_SMARTHOST` | `mailpit:1025` | servidor SMTP (`host:porta`) |
| `ALERTMANAGER_SMTP_FROM` | `alertas@recorvia.local` | remetente |
| `ALERTMANAGER_SMTP_REQUIRE_TLS` | `false` (Mailpit sem TLS) | `true` (padrão da imagem) exige STARTTLS; `false` só é aceito sem autenticação |
| `ALERTMANAGER_SMTP_USERNAME` / `ALERTMANAGER_SMTP_PASSWORD` | vazios | autenticação SMTP: as duas juntas e somente com `ALERTMANAGER_SMTP_REQUIRE_TLS=true` (no compose, é preciso defini-la); a senha é gravada num arquivo de acesso restrito e referenciada por `smtp_auth_password_file` |
| `ALERTMANAGER_EMAIL_TO` | `plantao@recorvia.local` | destinatários dos avisos e do `RecorviaWatchdog` (separados por vírgula) |
| `ALERTMANAGER_EMAIL_TO_CRITICAL` | vazio (usa `ALERTMANAGER_EMAIL_TO`) | destinatários dos críticos |
| `ALERTMANAGER_WEBHOOK_URL` | vazio (webhook desligado) | endpoint HTTP(S) que recebe o `POST` no formato padrão do Alertmanager (versão 4), para críticos e avisos; gravada em arquivo (`url_file`), porque pode conter token |
| `ALERTMANAGER_WEBHOOK_BEARER_TOKEN` | vazio | enviado em `Authorization: Bearer`; gravado em arquivo (`credentials_file`) |
| `ALERTMANAGER_EXTERNAL_URL` | `http://localhost:${ALERTMANAGER_PORT}` | endereço da interface nos links das notificações |
| `ALERTMANAGER_CLUSTER_LISTEN_ADDRESS` | vazio (instância única) | endereço do cluster de alta disponibilidade |

Sem `ALERTMANAGER_EMAIL_TO` e sem `ALERTMANAGER_WEBHOOK_URL`, o Alertmanager inicia com os
receptores vazios e os alertas ficam visíveis apenas na interface (a partida registra o aviso).
Combinações incoerentes impedem a partida, com a variável indicada na mensagem: destinatário sem
servidor SMTP, senha sem usuário, usuário sem senha (todo envio falharia num servidor que anuncia
autenticação), senha com `ALERTMANAGER_SMTP_REQUIRE_TLS=false` (sem TLS exigido, o Alertmanager
não negocia STARTTLS e a senha trafegaria em claro), token sem URL, URL sem `http://` ou
`https://`, valor com quebra de linha ou destinatário com `{{`. A imagem não contém endereços nem
segredos; os arquivos de segredo ficam em `/etc/alertmanager/runtime` (modo 600, diretório 700),
a API do Alertmanager expõe somente os caminhos deles, e nada disso é escrito nos logs
(`TestAlertmanagerEntrypointValidatesEnvironment`,
`TestAlertmanagerKeepsSMTPPasswordInRestrictedFileAndRequiresTLS`,
`TestAlertmanagerRoutesBySeverityInhibitsAndNotifiesWebhook`).

No ambiente local, `make up-obs` já entrega os e-mails no Mailpit: a notificação
"[Recorvia][INFORMATIVO] RecorviaWatchdog" chega cerca de meio minuto depois da partida. Para
receber o webhook num serviço local, defina `ALERTMANAGER_WEBHOOK_URL` no `.env` e recrie o
container (`docker compose --profile observability up -d alertmanager`).

O estado do Alertmanager (silêncios e registro de notificações, em `/alertmanager`) fica em
`tmpfs` no compose local e dura o mesmo que a caixa do Mailpit, que também não tem volume: num
container novo (`make down` seguido de `make up-obs`, ou a recriação do container para trocar um
receptor), os alertas ativos são notificados de novo e os silêncios criados na interface deixam
de existir. Com o registro num volume, o Alertmanager consideraria o grupo já notificado e o
`RecorviaWatchdog` só voltaria a ser enviado depois do intervalo de repetição (24 horas), com o
Mailpit vazio (`TestComposeAlertmanagerStateDoesNotOutliveContainer`,
`TestAlertmanagerNotifiesWatchdogAgainAfterEnvironmentRestart`). Fora do compose local, com
`/alertmanager` num volume persistente, os silêncios sobrevivem à partida e o reenvio de um
alerta já notificado só ocorre depois do intervalo de repetição da rota.

## Dashboards

Provisionados na pasta *Recorvia* do Grafana, somente leitura (alterações feitas na interface não
são salvas; edite o JSON e reconstrua a imagem). A fonte de dados é o Prometheus (uid
`prometheus`).

| Dashboard | Conteúdo |
|---|---|
| Recorvia: visão geral (página inicial) | indicadores dos alertas (outbox, operações desconhecidas, divergências, faturamento, DLQs, dead letters abertas, atraso das notificações e das entregas de webhooks), API, faturamento e cobrança (inclusive operações esgotadas), conciliação e dependências, régua de cobrança, notificações e entregas de webhooks (resultados, filas e idades) |
| Recorvia: mensageria | filas principais (prontas, atraso, não confirmadas, consumidores), retentativas, DLQs, resultado e duração por consumidor, saturação, dead letters abertas por consumidor (total e idade) e outbox |
| Recorvia: banco e processos | pool de conexões por processo, conexões do servidor, tarefas agendadas (resultado, duração, último sucesso), registros expurgados por tipo e recursos dos processos |

Os rastros são consultados na interface do Jaeger: a fonte de dados Jaeger do Grafana usa a API
HTTP v1, que o Jaeger 2.23 não expõe mais.

## Rastreamento

- **API**: span de servidor por requisição (`otelhttp`), nomeado pelo método normalizado e pelo
  padrão de rota (`GET /api/v1/customers/{id}`; método fora da lista vira `_OTHER`, e o valor
  recebido fica em `http.request.method_original`, limitado a 32 bytes) e com os atributos
  `http.route` e `recorvia.request_id` (o mesmo `X-Request-ID` devolvido na resposta e registrado
  nos logs).
- **Eventos**: o contexto de rastreamento viaja nos headers da outbox e das mensagens; o worker
  abre um span de consumo (`consume <consumidor>`) filho do contexto propagado, com fila,
  identificador da mensagem, tipo do evento e tentativa.
- **Tarefas agendadas**: um span por execução (`job <tarefa>`), exceto `metrics.collect`.
- **Banco**: spans das consultas (`otelpgx`) com o texto SQL parametrizado, sem os valores.
- **Redação no Collector** (ADR-052), antes de qualquer exportação: o token do caminho dos
  webhooks recebidos vira `{token}` em `url.path` e `url.full`, a consulta é removida de
  `url.full`, e `url.query`, `client.address` e `network.peer.address` são apagados (o `otelhttp`
  registra o endereço do par da conexão nos spans de servidor).
  `TestCollectorRedactsSensitiveSpanAttributes` (`internal/infra/telemetry`, tag `integration`)
  exporta um span pelo Collector real e confere, no Jaeger, que nada disso chega ao
  armazenamento.

## Correlação de logs

Os logs são JSON (`slog`) e levam, quando existem no contexto (ADR-082):

- `trace_id` e `span_id` do span ativo: do log se chega ao rastro no Jaeger;
- na API, `request_id` em todos os logs da requisição; o log de acesso (`msg=requisição`) leva também
  `tenant_id`, `user_id` ou `principal` identificados pela autenticação, além de método, rota, status
  e duração. Rota inexistente e método não permitido respondem `application/problem+json`
  (`route_not_found`, `method_not_allowed`, com `Allow`) e também têm `request_id`;
- no worker, `consumer`, `message_id`, `tenant_id` e o `request_id` da requisição que originou o
  evento (gravado na outbox e propagado nas mensagens); cada mensagem gera, em nível debug,
  `mensagem processada` ou `falha no processamento da mensagem`, e a falha tem um único aviso, do
  consumidor, com a decisão (retentativa agendada, DLQ ou devolução à fila) e os mesmos campos. A
  auditoria gravada pelo consumidor tem o mesmo `request_id`;
- nas tarefas agendadas, `job` com o nome da tarefa em todos os logs da execução; o executor de
  conciliações registradas usa `job=payments.registered_reconciliations`.

Senhas de URLs (Redis, RabbitMQ, PostgreSQL) e credenciais SMTP são removidas das mensagens de
erro de configuração e conexão antes do registro, e o handler de log mascara senhas de URLs em
qualquer campo.

No encerramento (SIGTERM), o worker responde `/readyz` 503 (`worker=shutting_down`) enquanto drena,
mantém `/livez` e `/metrics` no ar e só fecha o servidor depois da última mensagem em andamento; um
segundo sinal força a saída imediata.

## Validação das configurações

`make obs-check` (`deploy/observability/validate.sh`) constrói as imagens e executa as ferramentas
oficiais dentro delas; os Dockerfiles também validam a configuração no build, de modo que uma
configuração inválida não gera imagem. O job `observability-check` da CI executa `make obs-check`
em cada push e pull request para `develop` e `main`, com as imagens de versões fixas.

| Verificação | Comando executado |
|---|---|
| Collector | `docker run --rm recorvia-otel-collector:local validate --config=/etc/otelcol-contrib/config.yaml` |
| Jaeger | `docker run --rm recorvia-jaeger:local validate --config=file:/etc/jaeger/config.yaml` |
| Configuração do Prometheus | `docker run --rm --entrypoint promtool recorvia-prometheus:local check config /etc/prometheus/prometheus.yml` |
| Regras | `docker run --rm --entrypoint promtool recorvia-prometheus:local check rules /etc/prometheus/rules/recorvia.yml` |
| Testes das regras | `promtool test rules recorvia_test.yml` no estágio `rules-test` da imagem do Prometheus |
| Link dos runbooks | toda regra da imagem final do Prometheus com a anotação `runbook_url` |
| Configuração do Alertmanager | `docker run --rm [-e ALERTMANAGER_...] recorvia-alertmanager:local check-config`: gera a configuração e executa `amtool check-config` sem receptores, com os valores do compose local e com todos os receptores (valores fictícios com caracteres especiais) |
| Roteamento por severidade | `docker run --rm ... recorvia-alertmanager:local route-test rótulo=valor...` (`amtool config routes test`): crítico em `recorvia-critical`, aviso e alerta sem severidade em `recorvia-warning`, `RecorviaWatchdog` em `recorvia-watchdog` |
| Variáveis incoerentes | a geração da configuração precisa falhar para destinatário sem SMTP, senha sem usuário, usuário sem senha, senha com TLS desligado, token sem URL, URL sem esquema, valor com quebra de linha e TLS inválido |
| Dashboards, regras e roteamento | `go test -run 'TestDashboardsAreValidAndUseProvisionedDatasource\|TestAlertRulesAndDashboardsUseDeclaredMetrics\|TestAlertRulesLinkToExistingRunbookSections\|TestAlertRoutingCoversRuleSeveritiesAndInhibitionLabels' ./internal/infra/telemetry/`: JSON válido, uid único, fonte de dados provisionada, somente métricas declaradas no código, runbook existente para cada alerta, rota para cada severidade e rótulos de agregação na inibição |

Os testes Go da última linha não dependem de Docker e também rodam em `make test` (job
`go-quality`). No mesmo alvo, `TestValidateScriptExplainsEveryFailure` executa o `validate.sh` com
um `docker` simulado e confere que cada verificação reprovada termina com a mensagem do erro
(regras sem `runbook_url`, imagem sem regras, falha do `route-test`, combinação incoerente aceita
pela imagem), e `TestComposeAlertmanagerStateDoesNotOutliveContainer` confere que o compose não
guarda o estado do Alertmanager em volume.

Testes de integração com as mesmas imagens (Testcontainers, portas aleatórias, sem o compose),
executados por `make test-integration` (job `go-integration` da CI):

| Teste | O que confere |
|---|---|
| `TestCollectorRedactsSensitiveSpanAttributes` | aplicação → Collector → Jaeger, com redação |
| `TestGrafanaProvisionsDatasourceAndDashboards` | fonte de dados padrão somente leitura, os três dashboards provisionados na pasta *Recorvia* e a página inicial com acesso anônimo de leitura |
| `TestPrometheusLoadsAlertRulesAndScrapeTargets` | regras carregadas sem erro e os sete alvos de coleta |
| `TestAlertPipelineFromPrometheusRuleToEmail` | caminho inteiro de um alerta: `RecorviaWatchdog` dispara no Prometheus, chega ao Alertmanager descoberto pela configuração e o e-mail aparece no Mailpit com o link do runbook; toda regra carregada tem `runbook_url` |
| `TestAlertmanagerRoutesBySeverityInhibitsAndNotifiesWebhook` | críticos e avisos para destinatários distintos, aviso inibido pelo crítico do mesmo consumidor (e não o de outro), webhook com token e link do runbook, segredos do webhook somente em arquivos de acesso restrito |
| `TestAlertmanagerKeepsSMTPPasswordInRestrictedFileAndRequiresTLS` | senha do SMTP com TLS exigido: arquivo de acesso restrito com o conteúdo exato, fora da configuração gerada e da exposta pela API |
| `TestAlertmanagerNotifiesWatchdogAgainAfterEnvironmentRestart` | com o armazenamento declarado no compose, `RecorviaWatchdog` é notificado de novo depois de derrubar e subir o ambiente (Mailpit vazio) e depois de recriar o Alertmanager com outro destinatário |
| `TestAlertmanagerEntrypointValidatesEnvironment` | geração e validação da configuração para cada combinação de variáveis, recusa das incoerentes (inclusive usuário sem senha e senha com TLS desligado) e ausência de segredos na saída |
| `TestWorkerCollectsQueueMetrics` (`internal/app`) | `metrics.collect` publica dead letters abertas e as filas de notificações e de webhooks com o relógio da aplicação, sem rótulos de tenant |
| `TestDeadLetterStatsAggregatesOpenByConsumerAcrossTenants`, `TestNotificationBacklogStatsByState`, `TestWebhookDeliveryBacklogStatsByState`, `TestQueueStatsFunctionsPrivilegesAndIndexes`, `TestQueueMetricsMigrationDownAndUpAgain` (`internal/pgstore`) e `TestQueueMetricsMigrationDownAndUp` (`internal/infra/migrate`) | funções agregadas da migration 00015, privilégios, índices e seção Down (inclusive pelo Goose, com reaplicação) |
| `TestPrometheusPluginExposesQueueSeriesUsedByAlerts` | séries do plugin do RabbitMQ usadas pelos alertas de filas |

### Exercício da stack com o compose (job `observability-stack`)

O job `observability-stack` da CI executa `make up-obs` (ambiente completo, perfil
`observability` e dados de demonstração) e `make obs-stack-check`
(`deploy/observability/stack-check.sh`), que consulta as portas publicadas em `127.0.0.1` e
confere, com prazo por verificação:

- Prometheus: os sete alvos (`recorvia-api`, `recorvia-worker`, `rabbitmq`, `rabbitmq-queues`,
  `otel-collector`, `alertmanager`, `prometheus`) com `health=up`; todas as regras do arquivo de
  regras carregadas e avaliadas com `health=ok`, cada uma com `runbook_url`; e as séries das quais
  os alertas dependem (pool e conexões do
  banco, outbox, operações de pagamento, inclusive as esgotadas, dead letters, concorrência dos
  consumidores, falhas de cobrança, requisições HTTP por padrão de rota, último sucesso de
  `billing.run_cycle`, intervalo das tarefas, idade do processo do worker, disponibilidade do
  PostgreSQL e do Redis vista pela API sem consulta a `/readyz`, webhooks recebidos, envios de
  notificações, entregas de webhooks, ações da régua, dead letters abertas, filas de notificações
  e de entregas de webhooks, envios de alertas do Prometheus e notificações do Alertmanager, e as
  séries por fila do plugin do RabbitMQ, inclusive consumidores conectados à fila principal).
- Caminho dos alertas, de ponta a ponta: o Alertmanager está pronto e foi descoberto pelo
  Prometheus; o alerta de teste `RecorviaWatchdog` (sempre ativo) está disparando; o Alertmanager
  o recebeu e roteou para `recorvia-watchdog`; e o e-mail da notificação está no Mailpit
  (`/api/v1/search`), com o link do runbook da regra no texto e no HTML.
- Grafana: banco interno saudável, fonte de dados `prometheus` consultando o Prometheus
  (`/api/datasources/uid/prometheus/health`) e os três dashboards na pasta *Recorvia*.
- Jaeger: rastros dos serviços `recorvia-api` e `recorvia-worker` recebidos pelo Collector
  (`/api/v3/services`).

Ao final, o script lista os alertas em andamento no Prometheus e no Alertmanager e os e-mails de
alerta no Mailpit, apenas para registro. O mesmo alvo pode ser
usado localmente depois de `make up-obs`.

### Verificação local nesta etapa

Na revisão desta etapa, com Docker disponível, foram executados localmente:

- `promtool check rules` e `promtool test rules` (Prometheus 3.16.0, imagem
  `recorvia-prometheus:local`), com as regras e os testes enviados ao container pela entrada
  padrão, sem bind mount;
- `make test` (dashboards, métricas declaradas, catálogo, rótulos e links de runbook);
- `make test-integration`, que constrói as imagens de `deploy/observability` e executa
  `TestCollectorRedactsSensitiveSpanAttributes`, `TestGrafanaProvisionsDatasourceAndDashboards`,
  `TestPrometheusLoadsAlertRulesAndScrapeTargets` e
  `TestPrometheusPluginExposesQueueSeriesUsedByAlerts`.

O perfil `observability` do compose (`make up-obs` seguido de `make obs-stack-check`) não foi
iniciado localmente, porque as portas publicadas são compartilhadas com outros projetos; a
referência para ele é o job `observability-stack` da CI.

## Limitações

- O perfil `observability` do compose não foi iniciado localmente nesta etapa (portas
  compartilhadas com outros projetos): o perfil completo é exercitado pelo job
  `observability-stack` da CI; as imagens e as regras, também localmente
  (`make test-integration` e `promtool`).
- As contagens globais (`recorvia_payment_operations`, `recorvia_payment_operations_exhausted`,
  outbox) são gravadas pela instância do worker que executou a tarefa; com várias instâncias, os
  alertas usam `max` e o valor de uma instância que deixou de executar a tarefa só muda na próxima
  execução dela.
- Rastros ficam apenas em memória no Jaeger (até 50.000 rastros); métricas ficam 7 dias no volume
  do Prometheus.
- O Alertmanager roda em instância única, sem cluster. `RecorviaWatchdog` é notificado por e-mail
  uma vez por dia; não há serviço externo que alerte pela ausência dessa notificação, e a queda
  simultânea do Prometheus e do Alertmanager só é percebida por quem a acompanha.
- No compose local, os silêncios e o registro de notificações do Alertmanager não sobrevivem ao
  container: recriá-lo repete as notificações dos alertas ativos e descarta os silêncios.
- O envio de e-mail com autenticação e STARTTLS não é exercitado de ponta a ponta (o Mailpit dos
  testes não usa TLS): os testes cobrem a configuração gerada, o arquivo da senha e a recusa de
  senha sem TLS.
- As métricas globais de fila (`recorvia_dead_letters_open`, `recorvia_notifications_backlog`,
  `recorvia_webhook_deliveries_backlog` e as idades) são atualizadas a cada `metrics.collect`
  (15 s) pela instância do worker que executou a tarefa, como as demais contagens globais; os
  limites dos alertas novos são pontos de partida a calibrar com volume real.
- O link dos runbooks nas notificações aponta para o endereço definido na construção da imagem do
  Prometheus (`RUNBOOK_BASE_URL`), por padrão o ramo `main` do repositório, que pode estar à frente
  da versão implantada.
- A taxa de amostragem dos rastros é de 100% quando a exportação está habilitada.
