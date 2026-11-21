#!/usr/bin/env bash
# Conferência da stack de observabilidade em execução (make up-obs), pelas
# portas publicadas em 127.0.0.1. Usada pelo job observability-stack da CI e
# disponível localmente com make obs-stack-check. Confere:
#   - Prometheus: os sete alvos de coleta com health=up, todas as regras de
#     alerta do arquivo de regras carregadas e avaliadas sem erro, com o link
#     do runbook (runbook_url), e as séries da aplicação, do RabbitMQ e do
#     Alertmanager das quais os alertas dependem;
#   - caminho dos alertas: o Prometheus descobre o Alertmanager, o alerta
#     sempre ativo RecorviaWatchdog dispara, chega ao Alertmanager pela rota
#     recorvia-watchdog e o e-mail da notificação aparece no Mailpit com o
#     link do runbook;
#   - Grafana: banco interno saudável, fonte de dados provisionada consultando
#     o Prometheus e os dashboards provisionados na pasta Recorvia;
#   - Jaeger: rastros da API e do worker recebidos pelo Collector.
#
# Variáveis (opcionais): PROMETHEUS_PORT, ALERTMANAGER_PORT, MAILPIT_UI_PORT,
# GRAFANA_PORT, JAEGER_UI_PORT e GRAFANA_ADMIN_PASSWORD (padrão: valores do
# .env, se existir, ou do .env.example) e OBS_CHECK_TIMEOUT (prazo de cada
# verificação, em segundos).
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
obs="$root/deploy/observability"

log() { echo "==> $*"; }
die() {
  echo "erro: $*" >&2
  exit 1
}

command -v curl >/dev/null || die "curl não encontrado"
command -v jq >/dev/null || die "jq não encontrado"

# env_value NOME: último valor de NOME no .env do repositório (vazio se ausente).
env_value() {
  if [[ -f $root/.env ]]; then
    sed -n "s/^$1=//p" "$root/.env" | tail -n 1
  fi
  return 0
}

: "${PROMETHEUS_PORT:=$(env_value PROMETHEUS_PORT)}"
: "${ALERTMANAGER_PORT:=$(env_value ALERTMANAGER_PORT)}"
: "${MAILPIT_UI_PORT:=$(env_value MAILPIT_UI_PORT)}"
: "${GRAFANA_PORT:=$(env_value GRAFANA_PORT)}"
: "${JAEGER_UI_PORT:=$(env_value JAEGER_UI_PORT)}"
: "${GRAFANA_ADMIN_PASSWORD:=$(env_value GRAFANA_ADMIN_PASSWORD)}"
prometheus="http://127.0.0.1:${PROMETHEUS_PORT:-9090}"
alertmanager="http://127.0.0.1:${ALERTMANAGER_PORT:-9093}"
mailpit="http://127.0.0.1:${MAILPIT_UI_PORT:-8025}"
grafana="http://127.0.0.1:${GRAFANA_PORT:-3000}"
jaeger="http://127.0.0.1:${JAEGER_UI_PORT:-16686}"
grafana_auth="admin:${GRAFANA_ADMIN_PASSWORD:-local-grafana-admin}"
timeout=${OBS_CHECK_TIMEOUT:-300}

get() { curl -fsS --max-time 10 "$@"; }

# eventually DESCRIÇÃO COMANDO...: repete o comando a cada 5 s até ter
# sucesso ou o prazo expirar.
eventually() {
  local what=$1 deadline=$((SECONDS + timeout))
  shift
  until "$@" 2>/dev/null; do
    ((SECONDS < deadline)) || die "$what: condição não atingida em $timeout s"
    sleep 5
  done
  log "$what"
}

expected_jobs='["alertmanager","otel-collector","prometheus","rabbitmq","rabbitmq-queues","recorvia-api","recorvia-worker"]'
targets_up() {
  get "$prometheus/api/v1/targets?state=active" | jq -e --argjson want "$expected_jobs" '
    ($want - [.data.activeTargets[] | select(.health == "up") | .labels.job]) | length == 0' >/dev/null
}

expected_rules=$(grep -c '^ *- alert: ' "$obs/prometheus/rules/recorvia.yml")
rules_ok() {
  get "$prometheus/api/v1/rules?type=alert" | jq -e --argjson n "$expected_rules" '
    [.data.groups[].rules[]] | length == $n and all(.[]; .health == "ok")' >/dev/null
}

# Toda regra carregada traz o link completo do runbook, derivado da anotação
# runbook na construção da imagem do Prometheus.
rules_have_runbook_url() {
  get "$prometheus/api/v1/rules?type=alert" | jq -e '
    [.data.groups[].rules[]] | length > 0 and all(.[];
      (.annotations.runbook // "") as $path
      | ($path | startswith("docs/operacao/runbooks/"))
        and ((.annotations.runbook_url // "") | test("^https?://") and endswith($path)))' >/dev/null
}

# Caminho dos alertas, do Prometheus ao e-mail.
watchdog=RecorviaWatchdog
alertmanager_ready() { get "$alertmanager/-/ready" >/dev/null; }
alertmanager_discovered() {
  get "$prometheus/api/v1/alertmanagers" | jq -e '.data.activeAlertmanagers | length == 1' >/dev/null
}
watchdog_firing() {
  get "$prometheus/api/v1/alerts" | jq -e --arg name "$watchdog" '
    .data.alerts | any(.[]; .labels.alertname == $name and .state == "firing")' >/dev/null
}
watchdog_routed() {
  get --get "$alertmanager/api/v2/alerts" --data-urlencode "filter=alertname=\"$watchdog\"" | jq -e '
    any(.[]; .status.state == "active" and (.receivers | any(.name == "recorvia-watchdog")))' >/dev/null
}
watchdog_runbook_url() {
  get "$prometheus/api/v1/rules?type=alert" | jq -er --arg name "$watchdog" '
    [.data.groups[].rules[] | select(.name == $name)][0].annotations.runbook_url'
}
# O e-mail mais recente do alerta no Mailpit traz, no texto e no HTML, o link
# do runbook da regra.
watchdog_email() {
  local link id
  link=$(watchdog_runbook_url) || return 1
  id=$(get --get "$mailpit/api/v1/search" --data-urlencode "query=subject:\"$watchdog\"" | jq -er '.messages[0].ID') || return 1
  get "$mailpit/api/v1/message/$id" | jq -e --arg name "$watchdog" --arg link "$link" '
    (.Subject | contains($name)) and (.To | length > 0) and (.Text | contains($link)) and (.HTML | contains($link))' >/dev/null
}

has_series() {
  get --get "$prometheus/api/v1/query" --data-urlencode "query=$1" | jq -e '.data.result | length > 0' >/dev/null
}

grafana_healthy() { get "$grafana/api/health" | jq -e '.database == "ok"' >/dev/null; }
datasource_ok() {
  get -u "$grafana_auth" "$grafana/api/datasources/uid/prometheus/health" | jq -e '.status == "OK"' >/dev/null
}
expected_dashboards=$(jq -s -c '[.[].uid]' "$obs"/grafana/dashboards/*.json)
dashboards_ok() {
  get -u "$grafana_auth" "$grafana/api/search?type=dash-db" | jq -e --argjson want "$expected_dashboards" '
    ($want - [.[] | select(.folderTitle == "Recorvia") | .uid]) | length == 0' >/dev/null
}

jaeger_services() {
  get "$jaeger/api/v3/services" | jq -e '(["recorvia-api", "recorvia-worker"] - .services) | length == 0' >/dev/null
}

log "Prometheus em $prometheus, Alertmanager em $alertmanager, Mailpit em $mailpit, Grafana em $grafana, Jaeger em $jaeger (prazo de $timeout s por verificação)"
eventually "Prometheus: alvos $expected_jobs com health=up" targets_up
eventually "Prometheus: $expected_rules regras de alerta carregadas e avaliadas sem erro" rules_ok
eventually "Prometheus: todas as regras com o link do runbook (runbook_url)" rules_have_runbook_url
eventually "Alertmanager: pronto" alertmanager_ready
eventually "Prometheus: Alertmanager descoberto para o envio dos alertas" alertmanager_discovered
eventually "Prometheus: alerta de teste $watchdog disparando" watchdog_firing
eventually "Alertmanager: $watchdog recebido e roteado para recorvia-watchdog" watchdog_routed
eventually "Mailpit: e-mail de $watchdog com o link do runbook" watchdog_email
for q in \
  'recorvia_db_pool_max_connections{service="api"}' \
  'recorvia_db_pool_max_connections{service="worker"}' \
  'recorvia_db_server_max_connections' \
  'recorvia_outbox_oldest_pending_seconds' \
  'recorvia_payment_operations{status="unknown"}' \
  'recorvia_consumer_messages_total{outcome="dead_letter"}' \
  'recorvia_consumer_concurrency' \
  'recorvia_charge_failures_total{kind="permanent"}' \
  'recorvia_http_requests_total{route="/livez"}' \
  'recorvia_job_last_success_timestamp_seconds{task="billing.run_cycle"}' \
  'recorvia_job_interval_seconds{task="platform.close_statements"}' \
  'process_start_time_seconds{job="recorvia-worker"}' \
  'recorvia_payment_operations_exhausted{status="pending"}' \
  'recorvia_dependency_up{service="api", dependency="postgres"} == 1' \
  'recorvia_dependency_up{service="api", dependency="redis"} == 1' \
  'recorvia_provider_webhooks_total{outcome="too_large"}' \
  'recorvia_notifications_dispatched_total{outcome="sent"}' \
  'recorvia_webhook_deliveries_total{outcome="succeeded"}' \
  'recorvia_collection_actions_total{action="reminder_scheduled"}' \
  'recorvia_retention_purged_total{kind="inbox_messages"}' \
  'recorvia_job_interval_seconds{task="retention.purge"}' \
  'recorvia_dead_letters_open{consumer="payments.invoice_issued"}' \
  'recorvia_dead_letters_oldest_open_seconds{consumer="other"}' \
  'recorvia_notifications_backlog{state="due"}' \
  'recorvia_notifications_backlog_oldest_seconds{state="sending"}' \
  'recorvia_webhook_deliveries_backlog{state="pending"}' \
  'recorvia_webhook_deliveries_backlog_oldest_due_seconds{state="retrying"}' \
  'prometheus_notifications_errors_total' \
  'prometheus_notifications_dropped_total' \
  'alertmanager_notifications_total{integration="email"} > 0' \
  'alertmanager_notifications_failed_total{integration="email"}' \
  'rabbitmq_detailed_queue_consumers{queue="recorvia.payments.invoice_issued"} > 0' \
  'rabbitmq_detailed_queue_messages{queue=~"recorvia\\..+\\.dlq"}'; do
  eventually "Prometheus: série $q" has_series "$q"
done
eventually "Grafana: banco interno saudável" grafana_healthy
eventually "Grafana: fonte de dados prometheus consultando o Prometheus" datasource_ok
eventually "Grafana: dashboards $expected_dashboards na pasta Recorvia" dashboards_ok
eventually "Jaeger: rastros de recorvia-api e recorvia-worker" jaeger_services

log "alertas em andamento (informativo):"
get "$prometheus/api/v1/alerts" | jq -r '.data.alerts[] | "    \(.labels.alertname) \(.state) \(.labels.severity)"'
log "alertas no Alertmanager (informativo):"
get "$alertmanager/api/v2/alerts" | jq -r '.[] | "    \(.labels.alertname) \(.status.state) \([.receivers[].name] | join(","))"' || true
log "e-mails de alerta no Mailpit (informativo):"
get --get "$mailpit/api/v1/search" --data-urlencode 'query=subject:"[Recorvia]"' | jq -r '.messages[] | "    \(.Created) \(.Subject)"' || true
log "stack de observabilidade conferida"
