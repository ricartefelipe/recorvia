# Runbook: encaminhamento de alertas

Cobre o caminho dos alertas depois da avaliação das regras (ADR-065): o Prometheus envia os
alertas ao Alertmanager, que agrupa, inibe, roteia por severidade e notifica por e-mail e pelo
webhook genérico. A configuração do roteamento está em
`deploy/observability/alertmanager/alertmanager.yml`; os receptores são gerados na partida do
container a partir das variáveis `ALERTMANAGER_*`
([observabilidade](../observabilidade.md#encaminhamento-de-alertas)). Com o encaminhamento
parado, os alertas continuam visíveis no Prometheus (http://localhost:9090/alerts) e no Grafana:
use-os enquanto o caminho é restabelecido.

<a id="alertas-nao-chegam-aos-destinatarios"></a>

## Alertas não chegam aos destinatários

### Sintomas
- Alerta ativo no Prometheus ou no Grafana sem e-mail nem chamada ao webhook; a notificação
  diária de `RecorviaWatchdog` deixou de chegar.

### Métricas e alertas
- `RecorviaAlertDeliveryFailing` (crítico): o Prometheus não consegue enviar alertas ao
  Alertmanager (`prometheus_notifications_errors_total` ou
  `prometheus_notifications_dropped_total` crescendo) por 5 minutos.
- `RecorviaAlertNotificationsFailing` (crítico): o Alertmanager não consegue notificar por uma
  integração (`alertmanager_notifications_failed_total{integration}`, `email` ou `webhook`) por
  5 minutos. Com as duas integrações configuradas, a que funciona entrega este alerta.
- `RecorviaTargetDown` (crítico) com `job="alertmanager"`: o Alertmanager não responde à coleta.
- Nenhum desses alertas alcança os destinatários quando o próprio caminho está interrompido: a
  ausência da notificação de `RecorviaWatchdog` é o sinal
  ([Verificação do caminho de alertas](#verificacao-do-caminho-de-alertas)).

### Diagnóstico
```bash
docker compose --profile observability ps alertmanager prometheus mailpit
docker compose --profile observability logs --since 30m alertmanager | grep -E 'level=(WARN|ERROR)|alertmanager:'
curl -s http://localhost:9090/api/v1/alertmanagers | jq '.data'               # Alertmanager descoberto pelo Prometheus
curl -s http://localhost:9093/api/v2/alerts | jq -r '.[] | "\(.labels.alertname) \(.labels.severity) \(.status.state) \([.receivers[].name] | join(","))"'
curl -s http://localhost:9093/api/v2/status | jq -r '.config.original' | sed -n '/^receivers:/,$p'   # receptores gerados (sem segredos)
docker compose --profile observability exec alertmanager amtool check-config /etc/alertmanager/runtime/alertmanager.yml
```
- Container do Alertmanager encerrado com `alertmanager: ...` nos logs: as variáveis
  `ALERTMANAGER_*` são incoerentes (por exemplo, destinatário sem servidor SMTP, usuário do SMTP
  sem senha ou senha com `ALERTMANAGER_SMTP_REQUIRE_TLS=false`, que é o padrão do compose local)
  ou a configuração gerada é inválida; a mensagem indica a variável.
- `nenhum receptor configurado` na partida: `ALERTMANAGER_EMAIL_TO` e `ALERTMANAGER_WEBHOOK_URL`
  vazias; os alertas ficam somente na interface.
- Alerta com estado `suppressed`: silenciado por um silêncio criado na interface ou inibido por
  um alerta crítico de mesmo nome e mesmos rótulos (o crítico foi notificado no lugar do aviso).
- `Notify for alerts failed` com `integration=email`: servidor SMTP indisponível, TLS exigido e
  não oferecido (`ALERTMANAGER_SMTP_REQUIRE_TLS`), autenticação recusada ou remetente não
  autorizado. Com `integration=webhook`: endpoint fora do ar, resposta diferente de 2xx ou token
  recusado.
- Alerta ausente do Alertmanager e ativo no Prometheus: falha de envio do Prometheus (nome
  `alertmanager` não resolvido, rede) ou Alertmanager parado.
- Alerta `active` no Alertmanager, com receptor, sem notificação nova e sem falha nos logs: o
  grupo já foi notificado e a próxima notificação aguarda o intervalo de repetição da rota (1 hora
  para críticos, 12 horas para avisos, 24 horas para `RecorviaWatchdog`). O Alertmanager guarda
  esse registro de notificações junto com os silêncios, no diretório de `--storage.path`
  (`/alertmanager`).

### Ações
1. Alertmanager parado: corrigir as variáveis no `.env` (ou no ambiente do
   orquestrador) e recriar o container (`docker compose --profile observability up -d alertmanager`).
   Os receptores são gerados somente na partida: alterar uma variável exige recriar o container.
   No compose local, o estado do Alertmanager fica em memória (`tmpfs`) e não sobrevive ao
   container: depois de recriá-lo, os alertas ativos são notificados de novo (um destinatário
   novo recebe o `RecorviaWatchdog` em cerca de meio minuto) e os silêncios criados na interface
   deixam de existir. Com `/alertmanager` num volume persistente (fora do compose local), o
   registro de notificações sobrevive à partida: um alerta já notificado só chega ao destinatário
   novo depois do intervalo de repetição da rota.
2. Falha do SMTP ou do webhook: restabelecer o destino ou corrigir a variável correspondente. O
   Alertmanager repete a notificação com espera crescente e, depois, a cada intervalo de repetição
   da rota (1 hora para críticos, 12 horas para avisos); alertas que se resolveram durante a falha
   não são reenviados.
3. Enquanto o caminho não volta, acompanhar os alertas ativos pelo Prometheus e pelo Grafana e
   avisar o plantão por outro canal.
4. Não coloque senhas, tokens ou URLs com token em `alertmanager.yml` nem na imagem: os segredos
   entram pelas variáveis e são gravados em arquivos de acesso restrito no container.

### Verificação
`increase(alertmanager_notifications_failed_total[15m])` e
`increase(prometheus_notifications_errors_total[10m])` voltam a zero,
`alertmanager_notifications_total` volta a crescer e um alerta ativo chega ao destinatário (no
ambiente local, o e-mail aparece no Mailpit em http://localhost:8025).

<a id="verificacao-do-caminho-de-alertas"></a>

## Verificação do caminho de alertas

### Sintomas
- Notificação "[Recorvia][INFORMATIVO] RecorviaWatchdog" recebida: é o comportamento esperado
  e não pede ação.
- A notificação deixou de chegar no intervalo esperado: algum trecho do caminho falhou. O
  intervalo é de uma vez por dia, contado da última notificação. No compose local, ela também
  chega cerca de meio minuto depois de cada partida do ambiente (`make up-obs`, inclusive depois
  de `make down`), porque o registro de notificações do Alertmanager não sobrevive ao container;
  com esse registro num volume persistente, a partida não antecipa a notificação.

### Métricas e alertas
- `RecorviaWatchdog` (informativo): alerta sempre ativo (`vector(1)`), roteado para o receptor
  `recorvia-watchdog` (somente e-mail, repetição a cada 24 horas). Ele prova, de ponta a ponta,
  que o Prometheus avalia as regras, o Alertmanager recebe e roteia os alertas e o servidor de
  e-mail entrega a notificação.
- `alertmanager_notifications_total{integration="email"}` cresce a cada notificação enviada.

### Diagnóstico
```bash
make obs-stack-check   # confere o caminho inteiro: regra, Alertmanager, e-mail no Mailpit e link do runbook
curl -s http://localhost:9090/api/v1/alerts | jq -r '.data.alerts[] | select(.labels.alertname == "RecorviaWatchdog") | .state'
curl -s -G http://localhost:9093/api/v2/alerts --data-urlencode 'filter=alertname="RecorviaWatchdog"' | jq -r '.[] | "\(.status.state) \([.receivers[].name] | join(","))"'
curl -s -G http://localhost:8025/api/v1/search --data-urlencode 'query=subject:"RecorviaWatchdog"' | jq -r '.messages[] | "\(.Created) \(.Subject)"'
```
- Alerta ausente no Prometheus: regras não carregadas ou avaliação parada
  (http://localhost:9090/rules).
- Alerta `firing` no Prometheus e ausente no Alertmanager, ou presente no Alertmanager sem
  e-mail: seguir [Alertas não chegam aos destinatários](#alertas-nao-chegam-aos-destinatarios).
- No ambiente local, alerta `active` no Alertmanager e nenhum e-mail no Mailpit, sem falha de
  notificação: a caixa do Mailpit foi esvaziada (o container dele foi recriado) com o Alertmanager
  em execução, que já tinha notificado o grupo e só repete em 24 horas. Recrie o container do
  Alertmanager para descartar o registro de notificações:
  `docker compose --profile observability up -d --force-recreate alertmanager`.

### Ações
- Nenhuma, quando a notificação chega no intervalo esperado. Não silencie `RecorviaWatchdog`: o
  silêncio esconderia a falha do caminho.
- Fora do ambiente local, direcione o receptor `recorvia-watchdog` a um serviço que alerte pela
  ausência do sinal (a caixa de e-mail só mostra que ele deixou de chegar a quem a consulta).

### Verificação
`make obs-stack-check` conclui as verificações do caminho de alertas e a notificação de
`RecorviaWatchdog` volta a chegar.
