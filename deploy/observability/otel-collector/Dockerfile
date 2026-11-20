# OpenTelemetry Collector com a configuração embutida (ADR-020: sem bind mounts).
FROM otel/opentelemetry-collector-contrib:0.165.0
COPY config.yaml /etc/otelcol-contrib/config.yaml
# Falha o build se a configuração for inválida.
RUN ["/otelcol-contrib", "validate", "--config=/etc/otelcol-contrib/config.yaml"]
EXPOSE 4317 4318 8888 13133
