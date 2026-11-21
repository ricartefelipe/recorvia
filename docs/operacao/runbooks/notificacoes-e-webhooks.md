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
- Com o envio parado, a fila cresce e `RecorviaNotificationsBacklog` também dispara
  ([Fila de notificações acumulada](#fila-de-notificacoes-acumulada)).
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

<a id="fila-de-notificacoes-acumulada"></a>

## Fila de notificações acumulada

### Sintomas
- E-mails de fatura, lembretes e convites saindo com atraso ou não saindo, sem falhas de envio
  registradas: as notificações ficam em `scheduled` com o horário de envio vencido ou presas em
  `sending`.

### Métricas e alertas
- `RecorviaNotificationsBacklog` (aviso): notificação vencida (`due`) ou em envio (`sending`)
  há mais de 15 minutos, por 10 minutos. Crítico acima de uma hora; enquanto o crítico dura, o
  Alertmanager inibe o aviso da mesma situação.
- `recorvia_notifications_backlog{state}` e `recorvia_notifications_backlog_oldest_seconds{state}`
  (dashboard *visão geral*, painéis "Fila de notificações" e "Idade da fila de notificações";
  indicador "Atraso do envio de notificações"). Medidores globais lidos do banco pela tarefa
  `metrics.collect`, sem dados de tenant:
  - `due`: agendadas com `send_after` vencido; a idade é o atraso da mais antiga em relação ao
    horário de envio. Lembretes futuros e novas tentativas ainda em espera não contam.
  - `sending`: reivindicadas para envio (lease de 2 minutos); a idade é o tempo desde a
    reivindicação mais antiga. Depois do lease, a notificação volta a ser reivindicada; uma idade
    de vários minutos indica que ninguém a retoma.
  - `failed`: encerradas com falha definitiva nas últimas 24 horas; não alerta por aqui
    ([Notificações com falha definitiva](#notificacoes-com-falha-definitiva)).

### Diagnóstico
```sql
-- Notificações vencidas ou em envio por tenant e modelo, da mais atrasada para a menos (superusuário; sem conteúdo).
SELECT tenant_id, template_key, status, count(*), min(send_after) AS vencida_desde, min(lease_until) AS lease_mais_antigo,
       max(attempts) AS tentativas
  FROM notifications
 WHERE (status = 'scheduled' AND send_after <= now()) OR status = 'sending'
 GROUP BY 1, 2, 3 ORDER BY 5;
-- O que a métrica exporta (função agregada usada pelo worker; sem dados de tenant).
SELECT state, total, round(oldest_seconds) AS idade_s FROM app_notification_backlog_stats(now(), now() - interval '24 hours');
-- Última execução das tarefas de envio.
SELECT name, last_started_at, last_finished_at, next_run_at, locked_by, left(last_error, 200) AS erro
  FROM scheduled_jobs WHERE name IN ('notifications.dispatch', 'webhooks.deliver', 'metrics.collect');
```
```bash
docker compose logs --since 30m worker | grep -E 'notifications.dispatch|falha no envio de notificação'
```
- `notifications.dispatch` sem execução recente ou com erro: tarefa parada ou falhando
  (`RecorviaScheduledTaskFailing`, `RecorviaScheduledTaskStale`; runbook de
  [faturamento](faturamento.md#tarefas-agendadas-falhando)).
- Tarefa executando e fila crescendo num único tenant: volume acima do lote por execução (50
  notificações por tenant a cada 15 s) ou servidor SMTP lento (`RECORVIA_SMTP_TIMEOUT`).
- Somente `sending` com idade alta: worker interrompido durante o envio; a notificação é retomada
  depois do lease. Persistindo, o envio trava a cada tentativa (ver os logs).

### Ações
1. Tarefa parada: restabelecer o worker ou a dependência que a faz falhar; a fila é retomada
   sozinha, da mais antiga para a mais recente.
2. SMTP lento ou indisponível: seguir [Envio de e-mails parado](#envio-de-e-mails-parado).
3. Volume acima da vazão: acrescentar instâncias do worker (o envio usa lease e `SKIP LOCKED`,
   sem duplicar mensagens).
4. Não altere `send_after` nem a situação das notificações à mão: lembretes vencidos são
   reavaliados no envio e os que perderam o sentido são cancelados com o motivo no histórico.

### Verificação
`recorvia_notifications_backlog_oldest_seconds{state=~"due|sending"}` volta a poucos segundos e
`recorvia_notifications_backlog{state="due"}` esvazia entre as execuções.

<a id="notificacoes-com-falha-definitiva"></a>

## Notificações com falha definitiva

### Sintomas
- Notificações `failed` no histórico (`GET /api/v1/notifications`, permissão `collections.read`):
  o destinatário não foi avisado. `status_reason` indica a causa (`permanent_failure`,
  `attempts_exhausted` ou `content_unavailable`) e `GET /api/v1/notifications/{id}/events` mostra cada
  tentativa com o erro devolvido pelo servidor de e-mail.

### Métricas e alertas
- `RecorviaNotificationsFailed`: mais de 5 falhas definitivas em 30 minutos.
- `recorvia_notifications_backlog{state="failed"}`: falhas definitivas das últimas 24 horas em
  todos os tenants (painel "Fila de notificações"). Ao contrário do contador do alerta, o medidor
  vem do banco e não zera quando o worker reinicia.

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
  `RECORVIA_ENCRYPTION_KEYS` ou dado corrompido). Com a conferência do keyring na inicialização
  (padrão), o worker não inicia com chave ausente ou errada; o motivo só aparece com a chave
  declarada perdida (`RECORVIA_ENCRYPTION_LOST_KEYS`), com `RECORVIA_ENCRYPTION_STARTUP_CHECK=warn`
  ou `off`, ou com um registro isolado ilegível, que não impede a inicialização
  ([chaves de cifragem](recuperacao-de-desastre.md#chaves-de-cifragem)).
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
  (`GET /api/v1/webhook-endpoints/{id}/deliveries`, permissão `webhooks.read`) de vários tenants. As
  tentativas de cada entrega, com código HTTP, duração e erro, estão em
  `GET /api/v1/webhook-deliveries/{id}/attempts` e na ação "Tentativas" do histórico de entregas.

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

<a id="fila-de-entregas-de-webhooks-acumulada"></a>

## Fila de entregas de webhooks acumulada

### Sintomas
- Integrações dos tenants recebendo eventos com atraso ou não recebendo, sem tentativas
  registradas no histórico: as entregas ficam em `pending` ou `retrying` com o horário da tentativa
  vencido.

### Métricas e alertas
- `RecorviaWebhookDeliveriesBacklog` (aviso): entrega devida há mais de 15 minutos, por
  10 minutos. Crítico acima de uma hora; enquanto o crítico dura, o Alertmanager inibe o aviso da
  mesma situação.
- `recorvia_webhook_deliveries_backlog{state}` e
  `recorvia_webhook_deliveries_backlog_oldest_due_seconds{state}` (dashboard *visão geral*,
  painéis "Fila de entregas de webhooks" e "Atraso das entregas de webhooks"). Medidores globais
  lidos do banco pela tarefa `metrics.collect`, sem dados de tenant:
  - `pending`: aguardando a primeira tentativa (ou um reenvio manual);
  - `retrying`: aguardando nova tentativa depois de uma falha.
  O total inclui as entregas em espera de backoff (30 s até 6 h), que é o comportamento previsto
  para um destino fora do ar. O atraso considera somente as entregas com `next_attempt_at`
  vencido e é medido a partir desse horário, não da criação da entrega: um total alto em
  `retrying` com atraso zero indica destinos de tenants falhando, não a plataforma
  ([Webhooks enviados sem sucesso](#webhooks-enviados-sem-sucesso)).

### Diagnóstico
```sql
-- Entregas devidas por tenant e situação, da mais atrasada para a menos (superusuário).
SELECT tenant_id, status, count(*), min(next_attempt_at) AS devida_desde, max(attempts) AS tentativas,
       left(max(last_error), 200) AS erro
  FROM webhook_deliveries
 WHERE status IN ('pending', 'retrying') AND next_attempt_at <= now()
 GROUP BY 1, 2 ORDER BY 4;
-- O que a métrica exporta (função agregada usada pelo worker; sem dados de tenant).
SELECT state, total, round(oldest_due_seconds) AS atraso_s FROM app_webhook_delivery_backlog_stats(now());
```
```bash
docker compose logs --since 30m worker | grep -E 'webhooks.deliver|entrega de webhook sem sucesso'
```
- `webhooks.deliver` sem execução recente (consulta de `scheduled_jobs` da
  [fila de notificações](#fila-de-notificacoes-acumulada)): tarefa parada ou falhando.
- Atraso concentrado num tenant com muitas entregas: destino lento. Cada execução dedica até
  20 s a um tenant e até 1 minuto ao todo; as entregas não tentadas voltam à fila sem consumir
  tentativa (`deferred` em `recorvia_webhook_deliveries_total`).
- Atraso em vários tenants com a tarefa executando: volume acima da vazão do worker.

### Ações
1. Tarefa parada: restabelecer o worker ou a dependência que a faz falhar; as entregas devidas
   são retomadas da mais antiga para a mais recente.
2. Destino lento de um tenant: avisar o tenant; o orçamento por tenant impede que ele atrase os
   demais. O tenant pode desativar o endpoint (`PATCH /api/v1/webhook-endpoints/{id}`), o que
   encerra as entregas pendentes dele.
3. Volume acima da vazão: acrescentar instâncias do worker (as entregas usam lease e
   `SKIP LOCKED`).
4. Não altere `next_attempt_at` nem a situação das entregas à mão.

### Verificação
`recorvia_webhook_deliveries_backlog_oldest_due_seconds` volta a poucos segundos nas duas
situações.
