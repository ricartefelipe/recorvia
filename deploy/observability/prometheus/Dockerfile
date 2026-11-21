# Prometheus com configuração e regras de alerta embutidas (ADR-020: sem bind
# mounts). O estágio rules-test executa os testes das regras (promtool test
# rules) e é usado pela validação (make obs-check), não pela imagem final.
FROM prom/prometheus:v3.16.0 AS base
COPY prometheus.yml /etc/prometheus/prometheus.yml
COPY rules/ /etc/prometheus/rules/
RUN ["promtool", "check", "config", "/etc/prometheus/prometheus.yml"]

FROM base AS rules-test
COPY tests/ /etc/prometheus/tests/
WORKDIR /etc/prometheus/tests
RUN ["promtool", "test", "rules", "recorvia_test.yml"]

FROM base
# Endereço em que os runbooks são publicados. As regras trazem o caminho do
# runbook no repositório (anotação "runbook"); aqui cada regra recebe também
# a anotação "runbook_url", com o endereço completo, que as notificações do
# Alertmanager (e-mail e webhook) levam como link (ADR-065).
ARG RUNBOOK_BASE_URL=https://github.com/ricartefelipe/recorvia/blob/main/
USER root
RUN case "$RUNBOOK_BASE_URL" in \
      *[!A-Za-z0-9._~:/?#@%+=-]*) echo "RUNBOOK_BASE_URL com caracteres não permitidos" >&2; exit 1 ;; \
      http://*/ | https://*/) ;; \
      *) echo "RUNBOOK_BASE_URL deve começar com http:// ou https:// e terminar com /" >&2; exit 1 ;; \
    esac \
 && for f in /etc/prometheus/rules/*.yml; do \
      sed -i -E "/^ +runbook: /{p;s|^( +)runbook: |\\1runbook_url: ${RUNBOOK_BASE_URL}|;}" "$f" \
      && test "$(grep -c '^ *- alert: ' "$f")" -eq "$(grep -c "^ *runbook_url: ${RUNBOOK_BASE_URL}docs/operacao/runbooks/" "$f")" \
      || { echo "$f: nem toda regra recebeu runbook_url" >&2; exit 1; }; \
    done \
 && promtool check rules /etc/prometheus/rules/*.yml
USER nobody
CMD ["--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/prometheus", "--storage.tsdb.retention.time=7d"]
