# Runbooks

Procedimentos de diagnóstico e ação para os alertas do Prometheus
(`deploy/observability/prometheus/rules/recorvia.yml`, anotação `runbook`). Cada runbook segue a
mesma estrutura: sintomas, métricas e alertas, diagnóstico (comandos e consultas), ações e
verificação.

| Runbook | Alertas |
|---|---|
| [Faturamento e cobrança](faturamento.md) | `RecorviaBillingCycleStale`, `RecorviaScheduledTaskFailing`, `RecorviaScheduledTaskStale`, `RecorviaMetricsCollectionStale`, `RecorviaChargesDeclined`, `RecorviaPaymentProviderUnavailable`, `RecorviaPaymentOperationsPending`, `RecorviaWebhooksRejected`, `RecorviaDBPoolSaturated`, `RecorviaDBPoolWaiting`, `RecorviaDBServerConnectionsHigh`, `RecorviaHTTPErrorRate` |
| [Filas e DLQ](filas-e-dlq.md) | `RecorviaTargetDown`, `RecorviaOutboxBacklog`, `RecorviaOutboxStalled`, `RecorviaOutboxPublishFailures`, `RecorviaQueueBacklog`, `RecorviaQueueLag`, `RecorviaQueueWithoutConsumer`, `RecorviaDeadLetterQueueNotDrained`, `RecorviaMessagesDeadLettered`, `RecorviaDeadLettersOpen`, `RecorviaConsumerRetryRate`, `RecorviaMessagesRequeued`, `RecorviaWorkerSaturated`, `RecorviaDependencyUnavailable` |
| [Conciliação](conciliacao.md) | `RecorviaPaymentOperationsUnknown`, `RecorviaPaymentOperationsExhausted`, `RecorviaRefundsUnknown`, `RecorviaRefundsFailed`, `RecorviaReconciliationDiscrepanciesOpen`, `RecorviaReconciliationDiscrepanciesHigh`, `RecorviaReconciliationRunsWaiting` |
| [Notificações e webhooks enviados](notificacoes-e-webhooks.md) | `RecorviaEmailDeliveryStalled`, `RecorviaNotificationsFailed`, `RecorviaNotificationsBacklog`, `RecorviaWebhookDeliveriesFailing`, `RecorviaWebhookDeliveriesBacklog` |
| [Expurgo por retenção](retencao.md) | `RecorviaRetentionPurgeStale`, `RecorviaRetentionPurgeBacklog` |
| [Encaminhamento de alertas](alertas.md) | `RecorviaWatchdog`, `RecorviaAlertDeliveryFailing`, `RecorviaAlertNotificationsFailing` |
| [Isolamento de dados](isolamento-de-dados.md) | sem alerta automático (incidente aberto por suspeita ou relato) |
| [Equipe da plataforma](equipe-da-plataforma.md) | sem alerta automático (inclusão e remoção de integrantes da equipe) |
| [Recuperação de desastre e chaves de cifragem](recuperacao-de-desastre.md) | sem alerta próprio (`RecorviaDependencyUnavailable` e `RecorviaTargetDown` acompanham a perda do banco e a recusa de inicialização por chave errada) |

## Convenções dos comandos

Os exemplos usam o ambiente local (`docker compose`, projeto `recorvia`). Em outros ambientes,
substitua pelo acesso equivalente ao banco e ao broker. Na produção com o pacote de
`deploy/producao` ([implantação](../implantacao.md)), os mesmos comandos `docker compose` valem
executados dentro de `deploy/producao`; a API fica em `https://RECORVIA_DOMAIN` e as interfaces
de observabilidade, no loopback do servidor (túnel SSH).

- **Consultas globais** (sem dados de tenant: outbox agregada, agendamentos, catálogo do banco):
  `docker compose exec postgres psql -U postgres -d recorvia`.
- **O superusuário ignora a RLS.** Use-o somente para leitura e diagnóstico, sempre filtrando
  `tenant_id` explicitamente. Correções passam pela API (com permissão, justificativa e auditoria)
  ou, em último caso, por transação revisada por duas pessoas e registrada no incidente.
- **Consultas como a aplicação** (respeitam a RLS):
  `docker compose exec postgres psql -U recorvia_app -d recorvia` e, na transação,
  `SELECT set_config('app.tenant_id', '<tenant>', true);`.
- **API**: `curl` com uma sessão de usuário com a permissão indicada (o roteiro de login está em
  `docs/roteiro-demo.md`) ou, onde o comando usa `Authorization: Bearer $TOKEN`, com uma credencial
  de API da organização (`rcv_...`) que tenha a permissão; `$API` é o endereço da API. A equipe da
  plataforma não tem acesso aos dados de um tenant: a credencial é sempre da organização. Os
  endpoints citados estão em `api/openapi.yaml`.
- **Métricas**: Prometheus em http://localhost:9090 (consultas PromQL citadas) e dashboards do
  Grafana em http://localhost:3000 (pasta *Recorvia*).
- **Alertas**: as notificações (e-mail e webhook) trazem o link da seção do runbook. No ambiente
  local, os e-mails ficam no Mailpit (http://localhost:8025) e os alertas ativos, silêncios e
  inibições, no Alertmanager (http://localhost:9093).
- Ao abrir um incidente, registre horário, alerta, consultas executadas e ações tomadas. Não copie
  dados pessoais de clientes para o registro; use identificadores.
