# Jaeger v2 com a configuração embutida (ADR-020: sem bind mounts).
FROM jaegertracing/jaeger:2.23.0
COPY config.yaml /etc/jaeger/config.yaml
# Falha o build se a configuração for inválida.
RUN ["/cmd/jaeger/jaeger-linux", "validate", "--config=file:/etc/jaeger/config.yaml"]
CMD ["--config=file:/etc/jaeger/config.yaml"]
EXPOSE 4317 4318 8888 13133 16686
