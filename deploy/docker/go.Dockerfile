# Build multiestágio para os executáveis Go (api, worker, migrate, seed, paysim).
# Uso: docker build -f deploy/docker/go.Dockerfile --build-arg CMD=api .
# PKG troca o pacote compilado (padrão ./cmd/<CMD>); usado pelo receptor de
# webhooks do exercício de recuperação de desastre (./tools/hooksink).
FROM golang:1.27.4-alpine3.24 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG CMD
ARG PKG=./cmd/${CMD}
ARG VERSION=dev
RUN test -n "${CMD}" && go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/app "${PKG}" && mkdir -p /out/data

# Imagem final mínima: sem shell, executada como usuário sem privilégios.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/app /app
# Diretório gravável para estado local (usado pelo simulador de pagamentos).
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENTRYPOINT ["/app"]
