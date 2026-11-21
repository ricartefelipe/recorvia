#!/usr/bin/env bash
# Valida as configurações do perfil observability com as ferramentas oficiais,
# executadas em container a partir das mesmas imagens que o compose usa (as
# configurações entram por COPY; nenhum bind mount). Uso: make obs-check.
#
#   OpenTelemetry Collector  otelcol-contrib validate
#   Jaeger                   jaeger validate
#   Prometheus               promtool check config, check rules e test rules
#   Alertmanager             amtool check-config sobre a configuração gerada
#                            na partida (sem receptores, com os valores do
#                            ambiente local e com todos os receptores),
#                            amtool config routes test (roteamento por
#                            severidade) e recusa de variáveis incoerentes
#                            (inclusive usuário do SMTP sem senha e senha
#                            com TLS desligado)
#   Grafana                  JSON e consultas dos dashboards (go test); o
#                            provisionamento é conferido por
#                            TestGrafanaProvisionsDatasourceAndDashboards
#                            (make test-integration)
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
obs="$root/deploy/observability"
log() { echo "==> $*"; }
die() {
  echo "erro: $*" >&2
  exit 1
}

log "build das imagens (os Dockerfiles também validam as configurações)"
docker build -q -t recorvia-otel-collector:local "$obs/otel-collector" >/dev/null
docker build -q -t recorvia-jaeger:local "$obs/jaeger" >/dev/null
docker build -q -t recorvia-prometheus-rules-test:local --target rules-test "$obs/prometheus" >/dev/null
docker build -q -t recorvia-prometheus:local "$obs/prometheus" >/dev/null
docker build -q -t recorvia-alertmanager:local "$obs/alertmanager" >/dev/null
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

# Receptores como no compose local (Mailpit) e com todas as variáveis
# (valores fictícios, com caracteres que exigem cuidado no YAML gerado).
am_local=(-e ALERTMANAGER_SMTP_SMARTHOST=mailpit:1025 -e ALERTMANAGER_SMTP_REQUIRE_TLS=false -e ALERTMANAGER_EMAIL_TO=plantao@recorvia.local)
am_full=(-e ALERTMANAGER_SMTP_SMARTHOST=smtp.exemplo.test:587 -e "ALERTMANAGER_SMTP_FROM=Alertas do Recorvia <alertas@exemplo.test>"
  -e "ALERTMANAGER_SMTP_USERNAME=alertas'recorvia" -e 'ALERTMANAGER_SMTP_PASSWORD=validacao: "x" #1 $y'
  -e ALERTMANAGER_EMAIL_TO=plantao@exemplo.test -e "ALERTMANAGER_EMAIL_TO_CRITICAL=plantao@exemplo.test, gestao@exemplo.test"
  -e 'ALERTMANAGER_WEBHOOK_URL=https://alertas.exemplo.test/hook?canal=recorvia&x=1' -e ALERTMANAGER_WEBHOOK_BEARER_TOKEN=validacao)

log "amtool check-config (sem receptores)"
docker run --rm recorvia-alertmanager:local check-config
log "amtool check-config (ambiente local: e-mail pelo Mailpit)"
docker run --rm "${am_local[@]}" recorvia-alertmanager:local check-config
log "amtool check-config (e-mail com autenticação e webhook com token)"
docker run --rm "${am_full[@]}" recorvia-alertmanager:local check-config

log "amtool config routes test (roteamento por severidade)"
# expect_route RECEPTOR RÓTULO=VALOR...: receptor de um alerta com os rótulos.
# A saída de erro do container (na partida normal, o resumo dos receptores)
# só é exibida quando o route-test falha.
expect_route() {
  local want=$1 got errors
  shift
  errors=$(mktemp)
  if ! got=$(docker run --rm "${am_full[@]}" recorvia-alertmanager:local route-test "$@" 2>"$errors"); then
    cat "$errors" >&2
    rm -f "$errors"
    die "route-test falhou para {$*}"
  fi
  rm -f "$errors"
  [[ $got == "$want" ]] || die "alerta {$*} roteado para '$got'; esperado '$want'"
  echo "{$*} -> $got"
}
expect_route recorvia-critical alertname=RecorviaOutboxStalled severity=critical
expect_route recorvia-warning alertname=RecorviaOutboxBacklog severity=warning
expect_route recorvia-watchdog alertname=RecorviaWatchdog severity=info
expect_route recorvia-warning alertname=AlertaSemSeveridade

log "variáveis incoerentes impedem a partida do Alertmanager"
# expect_rejected DESCRIÇÃO VARIÁVEL=VALOR...: a configuração não pode ser gerada.
expect_rejected() {
  local what=$1 args=() v
  shift
  for v in "$@"; do args+=(-e "$v"); done
  if docker run --rm "${args[@]}" recorvia-alertmanager:local check-config >/dev/null 2>&1; then
    die "configuração aceita com $what"
  fi
  echo "recusado: $what"
}
expect_rejected "destinatário sem servidor SMTP" ALERTMANAGER_EMAIL_TO=plantao@exemplo.test
expect_rejected "senha do SMTP sem usuário" ALERTMANAGER_SMTP_SMARTHOST=smtp.exemplo.test:587 ALERTMANAGER_EMAIL_TO=plantao@exemplo.test ALERTMANAGER_SMTP_PASSWORD=validacao
expect_rejected "usuário do SMTP sem senha" ALERTMANAGER_SMTP_SMARTHOST=smtp.exemplo.test:587 ALERTMANAGER_EMAIL_TO=plantao@exemplo.test ALERTMANAGER_SMTP_USERNAME=alertas
expect_rejected "senha do SMTP com TLS desligado" ALERTMANAGER_SMTP_SMARTHOST=smtp.exemplo.test:587 ALERTMANAGER_EMAIL_TO=plantao@exemplo.test ALERTMANAGER_SMTP_USERNAME=alertas ALERTMANAGER_SMTP_PASSWORD=validacao ALERTMANAGER_SMTP_REQUIRE_TLS=false
expect_rejected "token sem URL do webhook" ALERTMANAGER_WEBHOOK_BEARER_TOKEN=validacao
expect_rejected "URL do webhook sem esquema" ALERTMANAGER_WEBHOOK_URL=alertas.exemplo.test/hook
expect_rejected "valor com quebra de linha" ALERTMANAGER_SMTP_SMARTHOST=smtp.exemplo.test:587 $'ALERTMANAGER_EMAIL_TO=plantao@exemplo.test\nreceivers: []'
expect_rejected "ALERTMANAGER_SMTP_REQUIRE_TLS inválido" ALERTMANAGER_SMTP_REQUIRE_TLS=talvez

log "regras da imagem final com a anotação runbook_url"
rules=$(docker run --rm --entrypoint sh recorvia-prometheus:local -c 'cat /etc/prometheus/rules/*.yml') ||
  die "não foi possível ler as regras da imagem do Prometheus"
# grep -c sem ocorrências imprime 0 e sai com 1: o status não pode encerrar o
# script antes da comparação, que é quem explica a falha.
alerts=$(grep -c '^ *- alert: ' <<<"$rules" || true)
links=$(grep -Ec '^ *runbook_url: https?://.*/docs/operacao/runbooks/.*\.md#' <<<"$rules" || true)
[[ $alerts -gt 0 && $alerts -eq $links ]] || die "$alerts regras e $links anotações runbook_url na imagem do Prometheus"
echo "$alerts regras com runbook_url"

log "dashboards do Grafana, regras e roteamento: JSON válido, fonte de dados provisionada, métricas existentes, runbooks, severidades e rótulos de inibição"
(cd "$root" && go test -count=1 -run 'TestDashboardsAreValidAndUseProvisionedDatasource|TestAlertRulesAndDashboardsUseDeclaredMetrics|TestAlertRulesLinkToExistingRunbookSections|TestAlertRoutingCoversRuleSeveritiesAndInhibitionLabels' ./internal/infra/telemetry/)

log "configurações de observabilidade válidas"
