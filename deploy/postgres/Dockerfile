# PostgreSQL do Recorvia com arquivamento contínuo de WAL e backups base
# cifrados para armazenamento compatível com S3 (ADR-087). Mesma versão e
# variante da imagem oficial usada pelo compose (18.7-alpine), acrescida de:
#   /usr/local/bin/wal-g               WAL-G compilado do código-fonte na
#                                      versão fixada abaixo, com libsodium
#   /usr/local/bin/recorvia-pgbackup   agente de backup (cmd/pgbackup):
#                                      archive_command, restore_command,
#                                      backup base agendado, retenção,
#                                      restauração para um instante e métricas
#   /usr/local/bin/recorvia-postgres-entrypoint
#                                      confere a configuração do backup antes
#                                      de iniciar o servidor
#
# Uso: docker build -f deploy/postgres/Dockerfile -t recorvia-postgres:local .
# (contexto na raiz do repositório, para compilar o agente). A configuração
# (destino S3, credenciais e chave de cifragem) vem do ambiente ou de
# arquivos em tempo de execução: deploy/postgres/compose.yaml.

# ---------- WAL-G ----------
# Integridade: o pacote do código-fonte é baixado só por HTTPS (inclusive nos
# redirecionamentos do GitHub) e conferido pela soma SHA-256 fixada aqui; as
# dependências Go, pelo go.sum do próprio WAL-G (-mod=readonly).
FROM golang:1.27.4-alpine3.24 AS walg
RUN apk add --no-cache build-base curl libsodium-dev libsodium-static
ARG WALG_VERSION=3.0.10
ARG WALG_SHA256=e7ad190500da1eb7cc7fdbe7e86543ce5b884992e99491042c5b2592caacf5fe
WORKDIR /src
RUN curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL -o /tmp/wal-g.tar.gz \
      "https://github.com/wal-g/wal-g/archive/refs/tags/v${WALG_VERSION}.tar.gz" \
 && echo "${WALG_SHA256}  /tmp/wal-g.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/wal-g.tar.gz --strip-components=1 \
 && rm /tmp/wal-g.tar.gz
# Binário estático (libsodium incluída): roda na imagem alpine sem dependências.
ENV CGO_ENABLED=1 GOFLAGS="-trimpath -mod=readonly"
RUN go build -tags libsodium \
      -ldflags "-s -w -linkmode external -extldflags -static -X github.com/wal-g/wal-g/cmd/pg.walgVersion=v${WALG_VERSION}" \
      -o /out/wal-g ./main/pg \
 && /out/wal-g --version | grep -q "v${WALG_VERSION}"

# ---------- Agente de backup ----------
FROM golang:1.27.4-alpine3.24 AS agent
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/recorvia-pgbackup ./cmd/pgbackup

# ---------- Imagem final ----------
# Roda como o usuário postgres (uid 70 na variante alpine), sem root: o
# /var/lib/postgresql da imagem oficial pertence a ele, e um volume nomeado
# novo montado ali herda esse dono. Os segredos montados (chave do backup,
# credenciais *_FILE) precisam ser legíveis por esse usuário (por exemplo,
# 0444, como os gerados por deploy/producao/gerar-segredos.sh).
FROM postgres:18.7-alpine
COPY --from=walg /out/wal-g /usr/local/bin/wal-g
COPY --from=agent /out/recorvia-pgbackup /usr/local/bin/recorvia-pgbackup
COPY deploy/postgres/entrypoint.sh /usr/local/bin/recorvia-postgres-entrypoint
RUN chmod 0755 /usr/local/bin/recorvia-postgres-entrypoint \
 && wal-g --version >/dev/null \
 && recorvia-pgbackup version >/dev/null \
 && [ "$(stat -c %U /var/lib/postgresql /var/run/postgresql | sort -u)" = postgres ]
USER postgres
ENTRYPOINT ["recorvia-postgres-entrypoint"]
# archive_timeout limita a perda (RPO) com pouca escrita: um segmento
# incompleto é fechado e arquivado depois de 60 s. O restore_command só é
# usado na recuperação (recovery.signal) e devolve erro fatal, em vez de
# "arquivo ausente", quando o WAL existe mas não pode ser lido ou decifrado.
CMD ["postgres", \
     "-c", "wal_level=replica", \
     "-c", "archive_mode=on", \
     "-c", "archive_command=recorvia-pgbackup wal-push %p", \
     "-c", "archive_timeout=60s", \
     "-c", "restore_command=recorvia-pgbackup wal-fetch %f %p"]
