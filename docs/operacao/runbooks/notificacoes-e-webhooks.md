# Runbook: notificações por e-mail e webhooks enviados

Cobre o envio de e-mails pela tarefa `notifications.dispatch` (a cada 15 s; ADR-042) e as entregas
de webhooks aos endpoints dos tenants pela tarefa `webhooks.deliver` (a cada 10 s; ADR-045). As
métricas não têm rótulos de tenant: os alertas apontam falhas da plataforma (servidor SMTP, saída
de rede do worker), e não problemas de um endpoint ou destinatário isolado, que cada tenant
acompanha pelo histórico na API.

## Envio de e-mails parado

### Sintomas
- Faturas, lembretes da régua e convites sem e-mail; notificações acumuladas em `scheduled` com
  `attempts` crescendo.

### Métricas e alertas
- `RecorviaEmailDeliveryStalled`: tentativas falhando (`retried` ou `failed`) e nenhum envio
  aceito (`sent`) em 15 minutos, por 15 minutos.
- `recorvia_notifications_dispatched_total{outcome}` (dashboard *visão geral*, painel
  "Notificações por resultado").

### Diagnóstico
```sql
-- Notificações aguardando nova tentativa, por tenant (superusuário; sem conteúdo).
SELECT tenant_id, template_key, count(*), max(attempts) AS tentativas, min(send_after) AS proxima,
       left(max(last_error), 200) AS erro
  FROM notifications WHERE status IN ('scheduled', 'sending') AND attempts > 0 GROUP BY 1, 2 ORDER BY 3 DESC;
```
```bash
docker compose logs --since 30m worker | grep -E 'falha no envio de notificação|conteúdo da notificação indisponível'
docker compose ps mailpit   # SMTP do ambiente local
```
- Erro de conexão ou tempo esgotado (`RECORVIA_SMTP_TIMEOUT`): servidor SMTP indisponível ou rede.
- `starttls` sem suporte do servidor ou falha de autenticação: configuração (`RECORVIA_SMTP_TLS`,
  `RECORVIA_SMTP_USERNAME`, `RECORVIA_SMTP_PASSWORD`).

### Ações
1. Restabelecer o servidor SMTP ou corrigir a configuração do worker e reiniciá-lo. As falhas
   transitórias voltam a `scheduled` com espera crescente (1 min, 5 min, 15 min e 1 h, até 5
   tentativas); restabelecido o envio, a fila é retomada sozinha.
2. Notificações que se encerraram em `failed` durante a indisponibilidade não são reenviadas
   automaticamente: ver [Notificações com falha definitiva](#notificacoes-com-falha-definitiva).

### Verificação
`increase(recorvia_notifications_dispatched_total{outcome="sent"}[15m])` volta a crescer e a
consulta do diagnóstico não retorna notificações com tentativas recentes.

<a id="notificacoes-com-falha-definitiva"></a>

## Notificações com falha definitiva

### Sintomas
- Notificações `failed` no histórico (`GET /api/v1/notifications`, permissão `collections.read`):
  o destinatário não foi avisado.

### Métricas e alertas
- `RecorviaNotificationsFailed`: mais de 5 falhas definitivas em 30 minutos.

### Diagnóstico
```sql
-- Falhas definitivas da última hora por tenant, modelo e motivo (superusuário).
SELECT tenant_id, template_key, status_reason, count(*), left(max(last_error), 200) AS erro
  FROM notifications WHERE status = 'failed' AND updated_at > now() - interval '1 hour'
 GROUP BY 1, 2, 3 ORDER BY 4 DESC;
```
- Resposta SMTP 5xx para todos os destinatários: endereço inválido no cadastro do cliente
  (concentrado num tenant) ou recusa do servidor (remetente `RECORVIA_SMTP_FROM` não autorizado,
  espalhado entre tenants).
- `content_unavailable`: o conteúdo cifrado não decifra (chave retirada de
  `RECORVIA_ENCRYPTION_KEYS` ou dado corrompido).
- Fim das 5 tentativas: indisponibilidade prolongada do SMTP
  ([seção anterior](#envio-de-e-mails-parado)).

### Ações
- Endereço inválido: o tenant corrige o contato do cliente; os próximos lembretes da régua usam o
  cadastro atualizado.
- Remetente recusado ou chave ausente: corrigir a configuração do worker (a chave precisa voltar ao
  keyring para que as notificações ainda agendadas sejam enviadas).
- Não altere `notifications` à mão: o histórico em `notification_events` é imutável (só sai pelo
  expurgo por idade, junto com a notificação).

### Verificação
`increase(recorvia_notifications_dispatched_total{outcome="failed"}[30m])` volta a zero.

<a id="webhooks-enviados-sem-sucesso"></a>

## Webhooks enviados sem sucesso

### Sintomas
- Integrações dos tenants sem eventos; entregas em `retrying` ou `exhausted` no histórico
  (`GET /api/v1/webhook-endpoints/{id}/deliveries`, permissão `webhooks.read`) de vários tenants.

### Métricas e alertas
- `RecorviaWebhookDeliveriesFailing`: 10 ou mais tentativas falhando (`retrying` ou `exhausted`) e
  nenhuma entrega bem-sucedida em 30 minutos, por 15 minutos. Enquanto algum destino recebe, a
  falha de um endpoint isolado não dispara o alerta.
- `recorvia_webhook_deliveries_total{outcome}` (dashboard *visão geral*, painel "Entregas de
  webhooks por resultado").

### Diagnóstico
```sql
-- Entregas falhando por tenant e último código de resposta (superusuário).
SELECT tenant_id, status, last_status_code, count(*), max(attempts) AS tentativas, left(max(last_error), 200) AS erro
  FROM webhook_deliveries WHERE status IN ('retrying', 'exhausted') AND updated_at > now() - interval '1 hour'
 GROUP BY 1, 2, 3 ORDER BY 4 DESC;
```
```bash
docker compose logs --since 30m worker | grep -E 'entrega de webhook sem sucesso'
```
- `status_code` 0 (sem resposta) em todos os tenants: o worker não alcança os destinos (DNS, rota de
  saída ou firewall).
- `permanent` verdadeiro: a proteção contra SSRF recusou o endereço resolvido (endereço interno;
  exceções somente em `RECORVIA_WEBHOOK_ALLOWED_PRIVATE_CIDRS`, ADR-045) e a entrega foi encerrada.
- Códigos 4xx ou 5xx de um único tenant: problema do endpoint do tenant, sem ação da plataforma.

### Ações
1. Restabelecer a saída de rede ou a resolução de nomes do worker; as entregas em `retrying` são
   retomadas no horário da próxima tentativa (backoff de 30 s até 6 h, até 10 tentativas).
2. Entregas que chegaram a `exhausted` durante a falha: o tenant reenvia pela API
   (`POST /api/v1/webhook-deliveries/{id}/retry`, permissão `webhooks.write`; uma tentativa
   adicional imediata, auditada).
3. Não amplie `RECORVIA_WEBHOOK_ALLOWED_PRIVATE_CIDRS` para contornar o bloqueio sem avaliar o
   destino.

### Verificação
`increase(recorvia_webhook_deliveries_total{outcome="succeeded"}[30m])` volta a crescer.
