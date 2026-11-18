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
CMD ["--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/prometheus", "--storage.tsdb.retention.time=7d"]
