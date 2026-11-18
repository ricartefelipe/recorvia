#!/usr/bin/env bash
# Conferência da stack de observabilidade em execução (make up-obs), pelas
# portas publicadas em 127.0.0.1. Usada pelo job observability-stack da CI e
# disponível localmente com make obs-stack-check. Confere:
#   - Prometheus: os seis alvos de coleta com health=up, todas as regras de
#     alerta do arquivo de regras carregadas e avaliadas sem erro e as séries
#     da aplicação e do RabbitMQ das quais os alertas dependem;
#   - Grafana: banco interno saudável, fonte de dados provisionada consultando
#     o Prometheus e os dashboards provisionados na pasta Recorvia;
#   - Jaeger: rastros da API e do worker recebidos pelo Collector.
#
# Variáveis (opcionais): PROMETHEUS_PORT, GRAFANA_PORT, JAEGER_UI_PORT e
# GRAFANA_ADMIN_PASSWORD (padrão: valores do .env, se existir, ou do
# .env.example) e OBS_CHECK_TIMEOUT (prazo de cada verificação, em segundos).
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
: "${GRAFANA_PORT:=$(env_value GRAFANA_PORT)}"
: "${JAEGER_UI_PORT:=$(env_value JAEGER_UI_PORT)}"
: "${GRAFANA_ADMIN_PASSWORD:=$(env_value GRAFANA_ADMIN_PASSWORD)}"
prometheus="http://127.0.0.1:${PROMETHEUS_PORT:-9090}"
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

expected_jobs='["otel-collector","prometheus","rabbitmq","rabbitmq-queues","recorvia-api","recorvia-worker"]'
targets_up() {
  get "$prometheus/api/v1/targets?state=active" | jq -e --argjson want "$expected_jobs" '
    ($want - [.data.activeTargets[] | select(.health == "up") | .labels.job]) | length == 0' >/dev/null
}

expected_rules=$(grep -c '^ *- alert: ' "$obs/prometheus/rules/recorvia.yml")
rules_ok() {
  get "$prometheus/api/v1/rules?type=alert" | jq -e --argjson n "$expected_rules" '
    [.data.groups[].rules[]] | length == $n and all(.[]; .health == "ok")' >/dev/null
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

log "Prometheus em $prometheus, Grafana em $grafana, Jaeger em $jaeger (prazo de $timeout s por verificação)"
eventually "Prometheus: alvos $expected_jobs com health=up" targets_up
eventually "Prometheus: $expected_rules regras de alerta carregadas e avaliadas sem erro" rules_ok
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
log "stack de observabilidade conferida"
