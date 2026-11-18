# Observabilidade

Como o Recorvia expõe métricas, rastros e sondas de saúde, como subir a stack local de
observabilidade e o que cada alerta significa. Decisões: ADR-052 (stack e configurações nas
imagens), ADR-053 (métricas e alertas de baixa cardinalidade) e ADR-054 (liveness e readiness).
Procedimentos de resposta aos alertas: [runbooks](runbooks/README.md).

## Subir a stack local

```bash
make up-obs          # ambiente completo + OpenTelemetry Collector, Jaeger, Prometheus e Grafana
make obs-stack-check # confere alvos, regras, séries, dashboards e rastros da stack em execução
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
| Jaeger | http://localhost:16686 | `JAEGER_UI_PORT` | rastros da API e do worker |

As portas de métricas (`api:9091`, `worker:9091`, `rabbitmq:15692`, `otel-collector:8888`) não
são publicadas no host: só o Prometheus, na rede do compose, as acessa.

| Componente | Imagem (versão fixa) | Configuração |
|---|---|---|
| OpenTelemetry Collector | `otel/opentelemetry-collector-contrib:0.165.0` | `deploy/observability/otel-collector/config.yaml` |
| Jaeger v2 | `jaegertracing/jaeger:2.23.0` | `deploy/observability/jaeger/config.yaml` (armazenamento em memória) |
| Prometheus | `prom/prometheus:v3.16.0` | `deploy/observability/prometheus/prometheus.yml` e `rules/recorvia.yml` |
| Grafana | `grafana/grafana:13.3.1` | `deploy/observability/grafana/provisioning` e `dashboards/*.json` |

## Liveness e readiness

As sondas têm semânticas distintas (RNF-09, ADR-054) e respondem JSON com `Cache-Control: no-store`:

| Processo | Porta | `/livez` | `/readyz` |
|---|---|---|---|
| API | HTTP (`RECORVIA_HTTP_ADDR`, 8081 no compose) | 200 enquanto o processo atende; não consulta dependências | 200 com PostgreSQL e Redis acessíveis; 503 caso contrário, com `checks` por dependência; atualiza `recorvia_dependency_up` |
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

## Métricas

Todas as métricas da aplicação têm o prefixo `recorvia_` e o rótulo constante `service` (`api` ou
`worker`). Nenhuma tem rótulo de tenant, cliente, fatura, usuário ou mensagem (RNF-08); o teste
`TestMetricsHaveNoHighCardinalityLabels` falha se um rótulo proibido ou reservado (`job`,
`instance`) for declarado. Os contadores de consumidores, tarefas e falhas de cobrança são criados
com zero na partida, para que `increase()` detecte a primeira ocorrência.

| Métrica | Tipo | Rótulos | Origem |
|---|---|---|---|
| `recorvia_http_requests_total` | contador | `route`, `method`, `status_class` | API, padrão de rota (nunca a URL concreta) |
| `recorvia_http_request_duration_seconds` | histograma | `route`, `method` | API |
| `recorvia_provider_webhooks_total` | contador | `provider`, `outcome` | API, recebimento de webhooks (`accepted`, `duplicate`, `unauthenticated`, `unknown_endpoint`, `invalid`, `error`) |
| `recorvia_dependency_up` | gauge | `dependency` | API (`postgres`, `redis`, a cada `/readyz`) e worker (`rabbitmq`) |
| `recorvia_consumer_messages_total` | contador | `consumer`, `outcome` | worker (`ack`, `retry`, `dead_letter`, `requeue`) |
| `recorvia_consumer_processing_seconds` | histograma | `consumer` | worker |
| `recorvia_consumer_in_flight` / `recorvia_consumer_concurrency` | gauge | `consumer` | worker (saturação) |
| `recorvia_outbox_published_total` / `recorvia_outbox_publish_failures_total` | contador | — | worker, relay da outbox |
| `recorvia_outbox_pending` / `recorvia_outbox_oldest_pending_seconds` | gauge | — | worker, tarefa `metrics.collect` (15 s) |
| `recorvia_job_runs_total` | contador | `task`, `outcome` | worker, agendador (`success`, `failure`) |
| `recorvia_job_duration_seconds` | histograma | `task` | worker |
| `recorvia_job_last_success_timestamp_seconds` | gauge | `task` | worker; só existe depois de um sucesso real |
| `recorvia_invoices_issued_total` | contador | — | worker, motor de faturamento |
| `recorvia_charge_failures_total` | contador | `provider`, `kind` | API e worker, falhas de cadastro do cliente e criação de cobrança no provedor (`permanent`, `retryable`, `ambiguous`, `not_found`) |
| `recorvia_payment_operations` | gauge | `status` | worker, `metrics.collect` (`pending`, `in_flight`, `unknown`) |
| `recorvia_reconciliation_open_discrepancies` | gauge | `kind` | declarada no catálogo; alimentada pelo módulo de conciliação |
| `recorvia_db_pool_connections` | gauge | `state` | API e worker, pool pgx lido a cada coleta (`acquired`, `idle`, `constructing`) |
| `recorvia_db_pool_max_connections` | gauge | — | API e worker |
| `recorvia_db_pool_acquires_total`, `_empty_acquires_total`, `_canceled_acquires_total`, `_empty_acquire_wait_seconds_total` | contador | — | API e worker |
| `recorvia_db_server_connections` / `recorvia_db_server_max_connections` | gauge | — | worker, `metrics.collect`: sessões de clientes de todo o servidor e limite para papéis comuns (`max_connections` menos as reservadas) |

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
pontos de partida para o ambiente local e devem ser calibrados com volume real. Não há
Alertmanager no perfil local: os alertas aparecem em http://localhost:9090/alerts e no Grafana. A
anotação `runbook` de cada regra aponta para uma seção existente (conferido por
`TestAlertRulesLinkToExistingRunbookSections`).

| Alerta | Condição | Severidade |
|---|---|---|
| `RecorviaTargetDown` | coleta da API, do worker ou do RabbitMQ falhando por 2 min | critical |
| `RecorviaOutboxBacklog` | mais de 1000 eventos pendentes por 10 min | warning |
| `RecorviaOutboxStalled` | evento pendente mais antigo com mais de 5 min, por 5 min | critical |
| `RecorviaOutboxPublishFailures` | falhas de publicação contínuas por 10 min | warning |
| `RecorviaQueueBacklog` | fila principal com mais de 500 mensagens prontas por 10 min | warning |
| `RecorviaQueueLag` | mensagem na cabeça da fila principal com mais de 5 min, por 5 min | warning |
| `RecorviaQueueWithoutConsumer` | fila principal sem consumidor por 5 min | critical |
| `RecorviaDeadLetterQueueNotDrained` | DLQ com mensagens não registradas em `dead_letters` por 10 min | warning |
| `RecorviaMessagesDeadLettered` | mensagens enviadas à DLQ nos últimos 15 min | warning |
| `RecorviaConsumerRetryRate` | mais de 10% de retentativas por 15 min | warning |
| `RecorviaMessagesRequeued` | mensagens devolvidas à fila principal nos últimos 10 min | warning |
| `RecorviaWorkerSaturated` | consumidor com 90% ou mais das vagas ocupadas por 15 min | warning |
| `RecorviaChargesDeclined` | mais de 5 recusas definitivas de cobrança em 30 min | warning |
| `RecorviaPaymentProviderUnavailable` | mais de 3 falhas repetíveis ou ambíguas em 10 min, por 5 min | critical |
| `RecorviaPaymentOperationsUnknown` | operação com resultado desconhecido por 30 min | warning |
| `RecorviaPaymentOperationsPending` | mais de 50 operações pendentes por 15 min | warning |
| `RecorviaWebhooksRejected` | mais de 5 webhooks recusados em 15 min | warning |
| `RecorviaReconciliationDiscrepanciesOpen` | divergência aberta por 4 h | warning |
| `RecorviaReconciliationDiscrepanciesHigh` | mais de 25 divergências abertas por 30 min | critical |
| `RecorviaBillingCycleStale` | `billing.run_cycle` sem sucesso há mais de 15 min | critical |
| `RecorviaScheduledTaskFailing` | 3 ou mais falhas de uma tarefa em 30 min | warning |
| `RecorviaMetricsCollectionStale` | `metrics.collect` sem sucesso há mais de 2 min | warning |
| `RecorviaDBPoolSaturated` | pool de um processo com 90% ou mais em uso por 5 min | warning |
| `RecorviaDBPoolWaiting` | espera por conexão acima de 0,2 s por segundo, por 10 min | warning |
| `RecorviaDBServerConnectionsHigh` | servidor com mais de 80% das conexões para papéis comuns, por 10 min | critical |
| `RecorviaDependencyUnavailable` | dependência indisponível por 2 min | critical |
| `RecorviaHTTPErrorRate` | mais de 5% de respostas 5xx por 10 min | warning |

A quantidade de dead letters registradas e ainda não resolvidas por tenant é consultada pela API
(`GET /api/v1/dead-letters`); não há métrica global para ela (ADR-053).

## Dashboards

Provisionados na pasta *Recorvia* do Grafana, somente leitura (alterações feitas na interface não
são salvas; edite o JSON e reconstrua a imagem). A fonte de dados é o Prometheus (uid
`prometheus`).

| Dashboard | Conteúdo |
|---|---|
| Recorvia: visão geral (página inicial) | indicadores dos alertas (outbox, operações desconhecidas, divergências, faturamento, DLQs), API, faturamento e cobrança, conciliação e dependências |
| Recorvia: mensageria | filas principais (prontas, atraso, não confirmadas, consumidores), retentativas, DLQs, resultado e duração por consumidor, saturação e outbox |
| Recorvia: banco e processos | pool de conexões por processo, conexões do servidor, tarefas agendadas (resultado, duração, último sucesso) e recursos dos processos |

Os rastros são consultados na interface do Jaeger: a fonte de dados Jaeger do Grafana usa a API
HTTP v1, que o Jaeger 2.23 não expõe mais.

## Rastreamento

- **API**: span de servidor por requisição (`otelhttp`), nomeado pelo padrão de rota
  (`GET /api/v1/customers/{id}`) e com os atributos `http.route` e `recorvia.request_id` (o mesmo
  `X-Request-ID` devolvido na resposta e registrado nos logs).
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
| Dashboards e regras | `go test -run 'TestDashboardsAreValidAndUseProvisionedDatasource\|TestAlertRulesAndDashboardsUseDeclaredMetrics\|TestAlertRulesLinkToExistingRunbookSections' ./internal/infra/telemetry/`: JSON válido, uid único, fonte de dados provisionada, somente métricas declaradas no código e runbook existente para cada alerta |

Os testes Go da última linha não dependem de Docker e também rodam em `make test` (job
`go-quality`).

Testes de integração com as mesmas imagens (Testcontainers, portas aleatórias, sem o compose),
executados por `make test-integration` (job `go-integration` da CI):

| Teste | O que confere |
|---|---|
| `TestCollectorRedactsSensitiveSpanAttributes` | aplicação → Collector → Jaeger, com redação |
| `TestGrafanaProvisionsDatasourceAndDashboards` | fonte de dados padrão somente leitura, os três dashboards provisionados na pasta *Recorvia* e a página inicial com acesso anônimo de leitura |
| `TestPrometheusLoadsAlertRulesAndScrapeTargets` | regras carregadas sem erro e os seis alvos de coleta |
| `TestPrometheusPluginExposesQueueSeriesUsedByAlerts` | séries do plugin do RabbitMQ usadas pelos alertas de filas |

### Exercício da stack com o compose (job `observability-stack`)

O job `observability-stack` da CI executa `make up-obs` (ambiente completo, perfil
`observability` e dados de demonstração) e `make obs-stack-check`
(`deploy/observability/stack-check.sh`), que consulta as portas publicadas em `127.0.0.1` e
confere, com prazo por verificação:

- Prometheus: os seis alvos (`recorvia-api`, `recorvia-worker`, `rabbitmq`, `rabbitmq-queues`,
  `otel-collector`, `prometheus`) com `health=up`; todas as regras do arquivo de regras carregadas
  e avaliadas com `health=ok`; e as séries das quais os alertas dependem (pool e conexões do
  banco, outbox, operações de pagamento, dead letters, concorrência dos consumidores, falhas de
  cobrança, requisições HTTP por padrão de rota, último sucesso de `billing.run_cycle` e as séries
  por fila do plugin do RabbitMQ, inclusive consumidores conectados à fila principal).
- Grafana: banco interno saudável, fonte de dados `prometheus` consultando o Prometheus
  (`/api/datasources/uid/prometheus/health`) e os três dashboards na pasta *Recorvia*.
- Jaeger: rastros dos serviços `recorvia-api` e `recorvia-worker` recebidos pelo Collector
  (`/api/v3/services`).

Ao final, o script lista os alertas em andamento, apenas para registro. O mesmo alvo pode ser
usado localmente depois de `make up-obs`.

### Verificação local nesta etapa

No ambiente de desenvolvimento desta etapa o Docker não estava disponível, portanto nem as
ferramentas oficiais (que rodam nas imagens) nem o perfil `observability` foram executados
localmente; o resultado de referência é o dos jobs `observability-check`, `observability-stack` e
`go-integration` da CI. Localmente, sem containers, foram conferidos:

- JSON dos dashboards com `python3 -m json.tool deploy/observability/grafana/dashboards/*.json`;
- YAML das configurações, regras, testes das regras, provisionamento, `compose.yaml` e
  `.github/workflows/ci.yml` com um parser YAML (`yaml.safe_load`);
- sintaxe dos scripts com `bash -n`;
- os testes Go de dashboards, métricas declaradas, rótulos proibidos e links de runbook
  (`make test`).

## Limitações

- O perfil `observability` não foi iniciado localmente nesta etapa (Docker indisponível no
  ambiente de desenvolvimento): as imagens, as configurações e o perfil do compose são
  exercitados pelos jobs `observability-check`, `observability-stack` e `go-integration` da CI.
- `recorvia_reconciliation_open_discrepancies` é alimentada pelo módulo de conciliação; enquanto
  ele não publicar a métrica, os painéis e alertas de divergências ficam sem dados.
- Rastros ficam apenas em memória no Jaeger (até 50.000 rastros); métricas ficam 7 dias no volume
  do Prometheus. Não há encaminhamento de alertas (e-mail, plantão).
- A taxa de amostragem dos rastros é de 100% quando a exportação está habilitada.
