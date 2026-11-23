# Proxy reverso de produção (ADR-086): Caddy com HTTPS automático e a
# configuração embutida na imagem (ADR-020: sem bind mounts).
# Uso (contexto é a pasta deploy/producao/caddy):
#   docker build -t recorvia-proxy:<versão> deploy/producao/caddy
#
# O binário é compilado aqui (xcaddy, na imagem oficial de build) para
# atualizar dependências com correção de segurança publicada antes de uma nova
# versão do Caddy: o binário da imagem caddy:2.11.9-alpine traz o
# golang.org/x/net v0.60.0 (CVE-2026-78661, HIGH, corrigida na v0.62.0), e o
# job image-scan falha em HIGH/CRITICAL corrigível (ADR-057). Vão junto as
# outras correções publicadas para o mesmo binário: golang.org/x/crypto v0.59.0
# (GO-2026-5932) e o OpenTelemetry v1.47.0 (CVE-2026-128085, MEDIUM; o núcleo
# inteiro na mesma versão, para manter o conjunto coerente). Ao subir a versão
# do Caddy, confira se as substituições abaixo ainda são necessárias.
FROM caddy:2.11.9-builder-alpine AS build
RUN xcaddy build v2.11.9 \
      --output /usr/bin/caddy \
      --replace golang.org/x/net=golang.org/x/net@v0.62.0 \
      --replace golang.org/x/crypto=golang.org/x/crypto@v0.59.0 \
      --replace go.opentelemetry.io/otel=go.opentelemetry.io/otel@v1.47.0 \
      --replace go.opentelemetry.io/otel/metric=go.opentelemetry.io/otel/metric@v1.47.0 \
      --replace go.opentelemetry.io/otel/trace=go.opentelemetry.io/otel/trace@v1.47.0 \
      --replace go.opentelemetry.io/otel/sdk=go.opentelemetry.io/otel/sdk@v1.47.0 \
      --replace go.opentelemetry.io/otel/sdk/metric=go.opentelemetry.io/otel/sdk/metric@v1.47.0 \
      --replace go.opentelemetry.io/otel/log=go.opentelemetry.io/otel/log@v1.47.0 \
      --replace go.opentelemetry.io/otel/sdk/log=go.opentelemetry.io/otel/sdk/log@v1.47.0 \
 && /usr/bin/caddy version

FROM caddy:2.11.9-alpine
COPY --from=build /usr/bin/caddy /usr/bin/caddy
COPY Caddyfile /etc/caddy/Caddyfile
# Usuário sem privilégios. O binário recebe só a capability de abrir portas
# abaixo de 1024 (80 e 443); /data (certificados e conta ACME) e /config
# pertencem ao usuário, e os volumes nomeados herdam essa posse na criação.
# A imagem base pode não incorporar correções já publicadas no Alpine 3.24
# (job image-scan): os pacotes do sistema são atualizados no build.
RUN apk upgrade --no-cache \
 && apk add --no-cache libcap \
 && addgroup -S -g 10001 caddy \
 && adduser -S -D -H -u 10001 -G caddy -s /sbin/nologin caddy \
 && setcap cap_net_bind_service=+ep /usr/bin/caddy \
 && apk del --no-cache libcap \
 && mkdir -p /data/caddy /config/caddy \
 && chown -R caddy:caddy /data /config
USER caddy
# Recusa no build uma configuração inválida (com valores de exemplo para as
# variáveis lidas na partida).
RUN RECORVIA_DOMAIN=app.example.com KEYCLOAK_DOMAIN=auth.example.com ACME_EMAIL=ops@example.com \
    KEYCLOAK_ADMIN_ALLOWED_CIDRS="127.0.0.1/32 ::1/128" \
    caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
