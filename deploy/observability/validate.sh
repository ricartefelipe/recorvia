#!/usr/bin/env bash
# Valida as configurações do perfil observability com as ferramentas oficiais,
# executadas em container a partir das mesmas imagens que o compose usa (as
# configurações entram por COPY; nenhum bind mount). Uso: make obs-check.
#
#   OpenTelemetry Collector  otelcol-contrib validate
#   Jaeger                   jaeger validate
#   Prometheus               promtool check config, check rules e test rules
#   Grafana                  JSON e consultas dos dashboards (go test); o
#                            provisionamento é conferido por
#                            TestGrafanaProvisionsDatasourceAndDashboards
#                            (make test-integration)
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
obs="$root/deploy/observability"
log() { echo "==> $*"; }

log "build das imagens (os Dockerfiles também validam as configurações)"
docker build -q -t recorvia-otel-collector:local "$obs/otel-collector" >/dev/null
docker build -q -t recorvia-jaeger:local "$obs/jaeger" >/dev/null
docker build -q -t recorvia-prometheus-rules-test:local --target rules-test "$obs/prometheus" >/dev/null
docker build -q -t recorvia-prometheus:local "$obs/prometheus" >/dev/null
docker build -q -t recorvia-grafana:local "$obs/grafana" >/dev/null

log "otelcol-contrib validate"
docker run --rm recorvia-otel-collector:local validate --config=/etc/otelcol-contrib/config.yaml
echo "configuração do Collector válida"

log "jaeger validate"
docker run --rm recorvia-jaeger:local validate --config=file:/etc/jaeger/config.yaml
echo "configuração do Jaeger válida"

log "promtool check config"
docker run --rm --entrypoint promtool recorvia-prometheus:local check config /etc/prometheus/prometheus.yml

log "promtool check rules"
docker run --rm --entrypoint promtool recorvia-prometheus:local check rules /etc/prometheus/rules/recorvia.yml

log "promtool test rules"
docker run --rm -w /etc/prometheus/tests --entrypoint promtool recorvia-prometheus-rules-test:local test rules recorvia_test.yml

log "dashboards do Grafana e regras: JSON válido, fonte de dados provisionada, métricas existentes e runbooks"
(cd "$root" && go test -count=1 -run 'TestDashboardsAreValidAndUseProvisionedDatasource|TestAlertRulesAndDashboardsUseDeclaredMetrics|TestAlertRulesLinkToExistingRunbookSections' ./internal/infra/telemetry/)

log "configurações de observabilidade válidas"
