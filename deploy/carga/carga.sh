#!/usr/bin/env bash
# Teste de carga do Recorvia (ADR-088, docs/operacao/carga.md).
# Uso: make load-test (ou bash deploy/carga/carga.sh).
#
# Num projeto isolado do docker compose (deploy/carga/carga.compose.yaml, com
# os limites de CPU e memória do pacote de produção):
#   1. constrói as imagens da API, do worker, do migrate e do simulador com a
#      etiqueta "carga" e sobe PostgreSQL (com pg_stat_statements), Redis,
#      RabbitMQ, Mailpit, simulador de pagamentos, API e worker;
#   2. gera os dados sintéticos pelos serviços e pela API (tools/carga
#      preparar e popular): organizações, catálogo, credenciais de API,
#      clientes, assinaturas retroativas com o histórico de faturas emitido
#      pelo motor de recorrência e pagamentos manuais;
#   3. configura o simulador como provedor de cada organização e executa os
#      cenários: (c) faturamento em lote, (a) leitura, (b) escrita com
#      Idempotency-Key, (e) pico de webhooks e (d) consumo medido;
#   4. junta os relatórios num só (relatorio.md), com a máquina, as consultas
#      lentas registradas pelo PostgreSQL e o uso dos containers, e derruba o
#      projeto com os volumes.
#
# Variáveis (todas opcionais):
#   RECORVIA_LOAD_PROJECT    projeto do compose (padrão: recorvia-carga); nunca
#                            o do ambiente local, e recusado se já tiver
#                            containers ou volumes que o teste não criou
#   RECORVIA_LOAD_DIR        diretório dos resultados (padrão: backups/carga,
#                            fora do Git); a execução anterior vai para
#                            anterior/. Precisa ser novo, vazio ou criado
#                            pelo teste (arquivo .recorvia-carga)
#   RECORVIA_LOAD_ORGS       organizações (padrão: 20)
#   RECORVIA_LOAD_SCALE      multiplicador de clientes por organização
#                            (padrão: 1, cerca de 6.600 clientes em 20
#                            organizações)
#   RECORVIA_LOAD_SCENARIOS  cenários, na ordem (padrão:
#                            faturamento,leitura,escrita,webhooks,consumo)
#   RECORVIA_LOAD_READ_LEVELS, RECORVIA_LOAD_WRITE_LEVELS,
#   RECORVIA_LOAD_USAGE_LEVELS  níveis de concorrência (padrões: 8,32,64;
#                            4,16; 4,16)
#   RECORVIA_LOAD_DURATION   duração de cada nível (padrão: 45s)
#   RECORVIA_LOAD_BUILD      0 reutiliza as imagens "carga" já construídas
#   RECORVIA_LOAD_KEEP       1 mantém o ambiente no ar ao terminar
#   RECORVIA_LOAD_TIMEOUT    prazo, em segundos, de cada etapa (padrão: 3600)
#   RECORVIA_LOAD_STALL      tempo máximo sem andamento numa espera (padrão:
#                            10m); passado esse tempo, a etapa falha
#   RECORVIA_LOAD_INHIBIT    0 não bloqueia a suspensão da máquina (padrão: 1,
#                            com systemd-inhibit quando houver)
#   RECORVIA_LOAD_SLOW_MS    limite de log_min_duration_statement (padrão: 200)
#   RECORVIA_LOAD_API_MAX_CONNS, RECORVIA_LOAD_WORKER_MAX_CONNS,
#   RECORVIA_LOAD_WORKER_CONCURRENCY, RECORVIA_LOAD_WORKER_PREFETCH,
#   RECORVIA_LOAD_BILLING_CONCURRENCY
#                            pool e concorrência (padrões de produção)
#   RECORVIA_LOAD_WEBHOOK_RATE_LIMIT  webhooks de provedores por minuto e por
#                            IP (padrão: sem limite prático; 1200 mede com o
#                            limite de produção)
#
# Exige docker com o plugin compose 2.24 ou superior, Go (para compilar
# tools/carga), openssl e timeout. Não publica portas fixas (somente portas
# livres em 127.0.0.1) e só remove containers, volumes e redes do próprio
# projeto.
set -euo pipefail
umask 077
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)
cd "$root"

# A suspensão da máquina no meio da execução congela os containers e o teste
# sem erro nenhum (o relógio monotônico e os prazos param junto). Com o
# systemd-logind, o roteiro roda sob um bloqueio de suspensão, ociosidade e
# tampa (RECORVIA_LOAD_INHIBIT=0 desliga).
if [[ ${RECORVIA_LOAD_INHIBIT:-1} == 1 ]]; then
  inhibit=(systemd-inhibit --what=sleep:idle:handle-lid-switch --who=Recorvia --why="teste de carga em andamento" --mode=block)
  if command -v systemd-inhibit >/dev/null 2>&1 && "${inhibit[@]}" true >/dev/null 2>&1; then
    RECORVIA_LOAD_INHIBIT=0 exec "${inhibit[@]}" bash "$here/carga.sh" "$@"
  fi
  echo "aviso: sem systemd-inhibit; desative a suspensão automática durante o teste" >&2
fi

project=${RECORVIA_LOAD_PROJECT:-recorvia-carga}
out_dir=${RECORVIA_LOAD_DIR:-backups/carga}
orgs=${RECORVIA_LOAD_ORGS:-20}
scale=${RECORVIA_LOAD_SCALE:-1}
scenarios=${RECORVIA_LOAD_SCENARIOS:-faturamento,leitura,escrita,webhooks,consumo}
duration=${RECORVIA_LOAD_DURATION:-45s}
step_timeout=${RECORVIA_LOAD_TIMEOUT:-3600}
label=recorvia.load-test

log() { echo "==> $*" >&2; }
die() {
  echo "erro: $*" >&2
  exit 1
}

for tool in docker go openssl timeout; do
  command -v "$tool" >/dev/null || die "$tool não encontrado"
done
compose_version=$(docker compose version --short 2>/dev/null) || die "plugin docker compose não encontrado"
IFS=. read -r compose_major compose_minor _ <<<"${compose_version#v}"
if ((compose_major < 2 || (compose_major == 2 && compose_minor < 24))); then
  die "docker compose $compose_version: o teste exige 2.24 ou superior"
fi
[[ $project =~ ^[a-z0-9][a-z0-9_-]*$ ]] || die "nome de projeto inválido: $project"
[[ $project != recorvia ]] || die "o teste de carga não pode usar o projeto do ambiente local (recorvia)"
[[ $step_timeout =~ ^[1-9][0-9]*$ ]] || die "RECORVIA_LOAD_TIMEOUT inválido: $step_timeout"

# O diretório dos resultados é esvaziado a cada execução (a anterior vai para
# anterior/): só um diretório novo, vazio ou criado pelo próprio teste (com o
# arquivo marcador) é aceito, nunca um diretório qualquer como "." ou $HOME.
marker=.recorvia-carga
[[ -n $out_dir ]] || die "RECORVIA_LOAD_DIR vazio"
if [[ -e $out_dir || -L $out_dir ]]; then
  [[ -d $out_dir && ! -L $out_dir ]] || die "RECORVIA_LOAD_DIR não é um diretório: $out_dir"
  if [[ ! -f $out_dir/$marker && -n $(ls -A "$out_dir") ]]; then
    die "RECORVIA_LOAD_DIR ($out_dir) não está vazio e não foi criado pelo teste de carga (sem o arquivo $marker): nada foi removido"
  fi
fi

# Antes de remover qualquer coisa: o projeto precisa ser do teste de carga.
for kind in containers volumes; do
  if [[ $kind == containers ]]; then
    all=$(docker ps -a -q --filter "label=com.docker.compose.project=$project")
    own=$(docker ps -a -q --filter "label=com.docker.compose.project=$project" --filter "label=$label=1")
  else
    all=$(docker volume ls -q --filter "label=com.docker.compose.project=$project")
    own=$(docker volume ls -q --filter "label=com.docker.compose.project=$project" --filter "label=$label=1")
  fi
  [[ $(sort <<<"$all") == "$(sort <<<"$own")" ]] ||
    die "o projeto $project tem $kind que não foram criados pelo teste de carga (sem o rótulo $label): nada foi removido"
done

# Variáveis de quem chama que o compose usaria no lugar das do teste.
unset COMPOSE_PROJECT_NAME COMPOSE_FILE COMPOSE_PROFILES OTEL_EXPORTER_OTLP_ENDPOINT POSTGRES_SUPERUSER_PASSWORD \
  RECORVIA_OWNER_PASSWORD RECORVIA_APP_PASSWORD RABBITMQ_USER RABBITMQ_PASSWORD KEYCLOAK_ADMIN_PASSWORD \
  RECORVIA_OIDC_CLIENT_SECRET RECORVIA_ENCRYPTION_KEYS RECORVIA_ENCRYPTION_ACTIVE_KEY

# Segredos e estado ficam num diretório temporário, fora do repositório.
vault=$(mktemp -d "${TMPDIR:-/tmp}/recorvia-carga.XXXXXX")
env_file="$vault/ambiente.env"
key=$(openssl rand -base64 32)
rabbit_user=carga
rabbit_pass=$(openssl rand -hex 16)
pg_pass=$(openssl rand -hex 16)
app_pass=$(openssl rand -hex 16)
cat >"$env_file" <<EOF
POSTGRES_SUPERUSER_PASSWORD=$pg_pass
RECORVIA_OWNER_PASSWORD=$(openssl rand -hex 16)
RECORVIA_APP_PASSWORD=$app_pass
RABBITMQ_USER=$rabbit_user
RABBITMQ_PASSWORD=$rabbit_pass
KEYCLOAK_ADMIN_PASSWORD=$(openssl rand -hex 16)
RECORVIA_OIDC_CLIENT_SECRET=$(openssl rand -hex 16)
RECORVIA_ENCRYPTION_KEYS=carga1:$key
RECORVIA_ENCRYPTION_ACTIVE_KEY=carga1
EOF

dc() {
  docker compose -p "$project" --env-file "$env_file" -f "$root/compose.yaml" -f "$here/carga.compose.yaml" "$@"
}

if [[ -f $out_dir/$marker ]]; then
  rm -rf "$out_dir/anterior"
  mkdir -p "$out_dir/anterior"
  find "$out_dir" -mindepth 1 -maxdepth 1 ! -name anterior ! -name "$marker" -exec mv -t "$out_dir/anterior" {} +
fi
mkdir -p "$out_dir"
chmod 755 "$out_dir"
: >"$out_dir/$marker"

finished=0
cleanup() {
  local code=$?
  trap - EXIT
  ((finished == 1)) || echo "erro: teste de carga interrompido (código $code); logs em $out_dir/compose-logs.txt" >&2
  dc logs --no-color --tail 3000 >"$out_dir/compose-logs.txt" 2>&1 || true
  if [[ ${RECORVIA_LOAD_KEEP:-0} == 1 ]]; then
    log "ambiente mantido (RECORVIA_LOAD_KEEP=1): projeto $project, segredos em $vault"
    log "para remover: docker compose -p $project down -v --remove-orphans && rm -rf $vault"
  else
    timeout 300 docker compose -p "$project" --env-file "$env_file" -f "$root/compose.yaml" -f "$here/carga.compose.yaml" \
      down -v --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$vault"
  fi
  exit "$code"
}
trap cleanup EXIT

log "removendo uma execução anterior do projeto $project"
timeout 300 docker compose -p "$project" --env-file "$env_file" -f "$root/compose.yaml" -f "$here/carga.compose.yaml" \
  down -v --remove-orphans >/dev/null 2>&1 || true

if [[ ${RECORVIA_LOAD_BUILD:-1} == 1 ]]; then
  for cmd in api worker migrate paysim; do
    log "construindo recorvia-$cmd:carga"
    timeout 900 docker build -q -f deploy/docker/go.Dockerfile --build-arg CMD=$cmd -t "recorvia-$cmd:carga" . >/dev/null
  done
fi
log "compilando tools/carga"
CGO_ENABLED=0 timeout 600 go build -o "$vault/carga" ./tools/carga

log "subindo o ambiente (projeto $project)"
timeout 600 docker compose -p "$project" --env-file "$env_file" -f "$root/compose.yaml" -f "$here/carga.compose.yaml" \
  up -d --wait --no-build postgres redis rabbitmq mailpit paysim api worker

port_of() {
  local addr
  addr=$(dc port "$1" "$2" 2>/dev/null | head -n 1)
  [[ -n $addr ]] || die "porta $2 de $1 não publicada"
  echo "${addr##*:}"
}
pg_port=$(port_of postgres 5432)
api_port=$(port_of api 8081)
api_metrics_port=$(port_of api 9091)
worker_metrics_port=$(port_of worker 9091)
paysim_port=$(port_of paysim 8090)
rabbit_port=$(port_of rabbitmq 15672)

# Ambiente de tools/carga: os subcomandos preparar e provedor usam os serviços
# da aplicação (como o seed) direto no banco, com o papel da aplicação; os
# demais falam com a API e com o simulador e só leem o banco (observador).
export RECORVIA_ENV=development
export RECORVIA_DATABASE_URL="postgres://recorvia_app:$app_pass@127.0.0.1:$pg_port/recorvia?sslmode=disable"
export RECORVIA_DATABASE_MAX_CONNS=4
export RECORVIA_ENCRYPTION_KEYS="carga1:$key" RECORVIA_ENCRYPTION_ACTIVE_KEY=carga1
export RECORVIA_PUBLIC_URL=http://localhost:8080
export RECORVIA_PAYSIM_URL="http://127.0.0.1:$paysim_port"
# Endereço registrado no simulador para os webhooks (rede do compose).
export RECORVIA_WEBHOOK_PUBLIC_URL=http://api:8081/webhooks/providers
export RECORVIA_WEBHOOK_ALLOWED_PRIVATE_CIDRS=127.0.0.0/8,172.16.0.0/12,192.168.0.0/16,10.0.0.0/8
export RECORVIA_LOAD_API="http://127.0.0.1:$api_port"
export RECORVIA_LOAD_API_METRICS="http://127.0.0.1:$api_metrics_port/metrics"
# Porta do worker no início; cada subcomando de tools/carga descobre a atual
# (docker port), porque o faturamento religa o worker e a porta muda.
export RECORVIA_LOAD_WORKER_METRICS="http://127.0.0.1:$worker_metrics_port/metrics"
export RECORVIA_LOAD_PAYSIM="http://127.0.0.1:$paysim_port"
export RECORVIA_LOAD_RABBIT_API="http://127.0.0.1:$rabbit_port"
export RECORVIA_LOAD_RABBIT_USER=$rabbit_user RECORVIA_LOAD_RABBIT_PASSWORD=$rabbit_pass
export RECORVIA_LOAD_OBSERVER_DB="postgres://postgres:$pg_pass@127.0.0.1:$pg_port/recorvia?sslmode=disable"
export RECORVIA_LOAD_STATE="$vault/estado.json"
export RECORVIA_LOAD_OUT=$out_dir
export RECORVIA_LOAD_PROJECT=$project
RECORVIA_LOAD_WORKER_CONTAINER=$(dc ps -q worker)
export RECORVIA_LOAD_WORKER_CONTAINER

step() {
  log "carga $*"
  timeout --signal=INT --kill-after=60 "$step_timeout" "$vault/carga" "$@"
}

step preparar -orgs "$orgs" -escala "$scale"
step popular
step provedor
IFS=, read -r -a list <<<"$scenarios"
for s in "${list[@]}"; do
  case $s in
    leitura) step leitura -niveis "${RECORVIA_LOAD_READ_LEVELS:-8,32,64}" -duracao "$duration" ;;
    escrita) step escrita -niveis "${RECORVIA_LOAD_WRITE_LEVELS:-4,16}" -duracao "$duration" ;;
    consumo) step consumo -niveis "${RECORVIA_LOAD_USAGE_LEVELS:-4,16}" -duracao "$duration" ;;
    faturamento | webhooks) step "$s" ;;
    *) die "cenário desconhecido: $s" ;;
  esac
done

# Consultas registradas por log_min_duration_statement durante toda a
# execução (somente a duração e o início do texto da consulta).
dc logs --no-color postgres 2>/dev/null | grep -E 'duration: [0-9.]+ ms' |
  sed -E 's/.*duration: ([0-9.]+) ms +(statement|execute [^:]*): */\1 ms | /' | cut -c1-260 |
  sort -t' ' -k1,1 -rn | head -n 30 >"$out_dir/consultas-lentas.txt" || true
slow_count=$(dc logs --no-color postgres 2>/dev/null | grep -cE 'duration: [0-9.]+ ms' || true)

{
  echo "# Teste de carga do Recorvia"
  echo
  echo "- Data: $(date -u +%Y-%m-%dT%H:%M:%SZ); commit $(git rev-parse --short HEAD 2>/dev/null || echo '?')$(git diff --quiet 2>/dev/null || echo ' com alterações locais')."
  echo "- Máquina: $(nproc) núcleos ($(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ //')), $(awk '/MemTotal/ {printf "%.0f GiB", $2/1048576}' /proc/meminfo) de RAM, $(awk '/MemAvailable/ {printf "%.1f GiB", $2/1048576}' /proc/meminfo) livres no fim; Docker $(docker version -f '{{.Server.Version}}')."
  echo "- Outros containers em execução na máquina durante o teste: $(($(docker ps -q | wc -l) - $(docker ps -q --filter "label=com.docker.compose.project=$project" | wc -l))) (os números valem para uma máquina compartilhada)."
  echo "- Limites: os do pacote de produção (PostgreSQL 2 CPUs e 2 GiB, API e worker 1,5 CPU e 512 MiB, RabbitMQ 1 CPU e 1 GiB, Redis 0,5 CPU e 384 MiB); pool de ${RECORVIA_LOAD_API_MAX_CONNS:-20} conexões na API e ${RECORVIA_LOAD_WORKER_MAX_CONNS:-20} no worker; worker com concorrência ${RECORVIA_LOAD_WORKER_CONCURRENCY:-8}, prefetch ${RECORVIA_LOAD_WORKER_PREFETCH:-16} e ${RECORVIA_LOAD_BILLING_CONCURRENCY:-4} tenants faturados ao mesmo tempo; webhooks de provedores limitados a ${RECORVIA_LOAD_WEBHOOK_RATE_LIMIT:-1000000} por minuto e por IP."
  echo "- Consultas acima de ${RECORVIA_LOAD_SLOW_MS:-200} ms registradas pelo PostgreSQL: $slow_count (as mais lentas em consultas-lentas.txt)."
  echo
  for f in "$out_dir"/[0-9][0-9]-*.md; do
    [[ -f $f ]] || continue
    cat "$f"
    echo
  done
} >"$out_dir/relatorio.md"
finished=1
log "relatório em $out_dir/relatorio.md"
