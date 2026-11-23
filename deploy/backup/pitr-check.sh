#!/usr/bin/env bash
# Exercício de recuperação para um ponto no tempo (ADR-087).
# Uso: make pitr-check (ou bash deploy/backup/pitr-check.sh).
#
# Num projeto isolado do docker compose (deploy/backup/pitr.compose.yaml, com
# o MinIO como armazenamento compatível com S3):
#   1. sobe o PostgreSQL com arquivamento contínuo de WAL e o agente de backup
#      (backup base na partida), o Keycloak com o banco dele no mesmo servidor
#      e a aplicação; carrega o seed, faz backups base pelo agente e confere a
#      retenção;
#   2. grava dados: credencial de API (login OIDC real), fatura e cobrança
#      emitidas pelo worker, usuário criado no Keycloak e uma marca num banco
#      de conferência;
#   3. com a escrita da aplicação parada, registra o instante T e o
#      manifesto do banco nesse instante;
#   4. grava mais dados depois de T (outra marca, pagamento da cobrança no
#      provedor confirmado pelo webhook, outro usuário no Keycloak) e mede o
#      atraso do arquivamento (RPO);
#   5. destrói o servidor (PostgreSQL e volume, agente, Keycloak, API, worker,
#      Redis e RabbitMQ; o armazenamento e o provedor de pagamento são
#      externos e sobrevivem);
#   6. confere que a restauração com a chave de cifragem do backup errada ou
#      ausente é recusada, sem alterar o volume, e que o WAL-G não decifra os
#      arquivos sem a chave certa;
#   7. restaura até T num servidor novo (deploy/backup/pitr-restore.sh) e
#      confere que os dados até T voltaram (manifesto idêntico, marca, usuário
#      do Keycloak, credencial) e os de depois de T não;
#   8. religa a aplicação como na recuperação de desastre: chaves conferidas,
#      API sem o worker, conciliação da janela perdida (o pagamento depois de
#      T volta pelo provedor) e só então o worker; mede o RTO.
#
# Variáveis (todas opcionais):
#   RECORVIA_PITR_PROJECT        projeto do compose (padrão: recorvia-pitrcheck),
#                                derrubado com os volumes no início e no fim;
#                                nunca o do ambiente local
#   RECORVIA_PITR_REUSE_PROJECT  1 confirma a remoção de um projeto existente
#                                que o exercício não criou
#   RECORVIA_PITR_DIR            relatório, manifestos e logs (padrão:
#                                backups/pitr-check, fora do Git), diretório
#                                próprio do exercício
#   RECORVIA_PITR_IMAGE_TAG      etiqueta das imagens construídas (padrão: pitr)
#   RECORVIA_PITR_BUILD          0 reutiliza as imagens já construídas (padrão: 1)
#   RECORVIA_PITR_KEEP           1 mantém o ambiente e o cofre para inspeção
#   RECORVIA_PITR_TIMEOUT        espera máxima, em segundos, de cada condição
#                                (padrão: 300)
#
# Exige docker com o plugin compose 2.24 ou superior, curl, jq, openssl e
# realpath. Não publica portas fixas no host e só remove o que o próprio
# projeto criou (rótulo recorvia.dr-check, deploy/backup/dr-lib.sh).
set -euo pipefail
umask 077
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)
cd "$root"

project=${RECORVIA_PITR_PROJECT:-recorvia-pitrcheck}
out_dir=${RECORVIA_PITR_DIR:-backups/pitr-check}
image_tag=${RECORVIA_PITR_IMAGE_TAG:-pitr}
timeout_s=${RECORVIA_PITR_TIMEOUT:-300}
caller_backup_dir=${RECORVIA_BACKUP_DIR:-backups}
export RECORVIA_DR_IMAGE_TAG=$image_tag
export RECORVIA_DR_REUSE_PROJECT=${RECORVIA_PITR_REUSE_PROJECT:-0}
# shellcheck source=deploy/backup/common.sh
source "$here/common.sh"
dr_dir_var=RECORVIA_PITR_DIR
dr_dir_default=backups/pitr-check
dr_dir_purpose="Diretório do exercício de recuperação para um ponto no tempo (make pitr-check)"
dr_project_var=RECORVIA_PITR_PROJECT
dr_reuse_var=RECORVIA_PITR_REUSE_PROJECT
# shellcheck source=deploy/backup/dr-lib.sh
source "$here/dr-lib.sh"

api_url=http://localhost:18081
keycloak_host=localhost:8180
demo_user=carla.owner@beta.test
demo_password='Recorvia#Demo2026'
probe_db=recorvia_pitr_conferencia

phase() {
  echo >&2
  echo "=== $* ===" >&2
}
ok() {
  echo "    ok: $*" >&2
  echo "- $*" >>"$checks_file"
}
now_s() { date -u +%s; }
now_iso() { date -u +%Y-%m-%dT%H:%M:%SZ; }

for tool in docker curl jq openssl realpath; do
  command -v "$tool" >/dev/null || die "$tool não encontrado"
done
compose_version=$(docker compose version --short 2>/dev/null) || die "plugin docker compose não encontrado"
IFS=. read -r compose_major compose_minor _ <<<"${compose_version#v}"
if ((compose_major < 2 || (compose_major == 2 && compose_minor < 24))); then
  die "docker compose $compose_version: o exercício exige 2.24 ou superior"
fi
[[ $project =~ ^[a-z0-9][a-z0-9_-]*$ ]] || die "nome de projeto inválido: $project"
[[ $project != recorvia ]] || die "o exercício não pode usar o projeto do ambiente local (recorvia)"
[[ $timeout_s =~ ^[1-9][0-9]*$ ]] || die "RECORVIA_PITR_TIMEOUT inválido: $timeout_s"
require_safe_dr_dir "$out_dir" "$root" "$caller_backup_dir"
require_own_project "$project"
# Variáveis de quem chama que o compose usaria no lugar das do exercício.
unset COMPOSE_PROJECT_NAME COMPOSE_FILE COMPOSE_PROFILES COMPOSE_ENV_FILES KEYCLOAK_PORT WEB_PORT API_PORT OTEL_EXPORTER_OTLP_ENDPOINT \
  POSTGRES_SUPERUSER_PASSWORD RECORVIA_OWNER_PASSWORD RECORVIA_APP_PASSWORD RABBITMQ_USER RABBITMQ_PASSWORD \
  KEYCLOAK_ADMIN_USER KEYCLOAK_ADMIN_PASSWORD RECORVIA_OIDC_CLIENT_SECRET RECORVIA_OIDC_REDIRECT_URL \
  RECORVIA_ENCRYPTION_KEYS RECORVIA_ENCRYPTION_ACTIVE_KEY RECORVIA_ENCRYPTION_STARTUP_CHECK RECORVIA_ENCRYPTION_LOST_KEYS \
  RECORVIA_POSTGRES_IMAGE_TAG RECORVIA_PITR_SUMMARY
while IFS='=' read -r name _; do
  unset "$name"
done < <(env | grep '^RECORVIA_BACKUP_' || true)

# Cofre: chaves da aplicação, chave de cifragem do backup e senhas, fora do
# diretório do exercício e do repositório.
vault=$(mktemp -d "${TMPDIR:-/tmp}/recorvia-pitr-cofre.XXXXXX")
env_file="$vault/ambiente.env"
work="$vault/trabalho"
mkdir -p "$work"
prepare_dr_dir "$out_dir"
checks_file="$out_dir/conferencias.txt"
: >"$checks_file"

compose_files=("$root/compose.yaml" "$root/deploy/backup/dr.compose.yaml" "$root/deploy/postgres/compose.yaml" "$root/deploy/backup/pitr.compose.yaml")
dc_argv=(-p "$project" --env-file "$env_file")
for f in "${compose_files[@]}"; do dc_argv+=(-f "$f"); done
dc() { docker compose "${dc_argv[@]}" "$@"; }
# pitr_restore ARGUMENTOS: o roteiro de restauração com o compose do exercício
# (variáveis padrão do docker compose, como faria quem opera).
pitr_restore() {
  (
    COMPOSE_PROJECT_NAME=$project COMPOSE_ENV_FILES=$env_file
    COMPOSE_FILE=$(
      IFS=:
      echo "${compose_files[*]}"
    )
    export COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES COMPOSE_FILE
    bash "$here/pitr-restore.sh" "$@"
  )
}

finished=0
cleanup() {
  local code=$?
  trap - EXIT
  if ((finished == 0)); then
    echo "erro: exercício interrompido (código $code); logs em $out_dir/compose-logs.txt" >&2
  fi
  dc ps -a >"$out_dir/compose-ps.txt" 2>&1 || true
  dc logs --no-color --tail 2000 >"$out_dir/compose-logs.txt" 2>&1 || true
  if [[ ${RECORVIA_PITR_KEEP:-0} == 1 ]]; then
    echo "==> ambiente mantido (RECORVIA_PITR_KEEP=1): projeto $project, cofre em $vault" >&2
    echo "    para remover: docker compose -p $project down -v --remove-orphans && rm -rf $vault" >&2
  else
    dc --profile seed down -v --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$vault"
  fi
  exit "$code"
}
trap cleanup EXIT

wait_until() {
  local what=$1 start=$SECONDS
  shift
  until "$@"; do
    ((SECONDS - start < timeout_s)) || die "tempo esgotado ($timeout_s s) aguardando: $what"
    sleep 2
  done
}

port_of() {
  local addr
  addr=$(dc port "$1" "$2" 2>/dev/null | head -n 1)
  [[ -n $addr ]] || die "porta de $1 não publicada"
  echo "${addr##*:}"
}

resolve_pg() {
  RECORVIA_PG_CONTAINER=$(dc ps -q postgres)
  export RECORVIA_PG_CONTAINER
  resolve_container
}
sql() { psql_in "$RECORVIA_PG_DATABASE" -c "$1"; }
probe() { psql_in "$probe_db" -c "$1"; }

refresh_ports() {
  api_port=$(port_of api 8081)
  connect=(--connect-to "localhost:18081:127.0.0.1:$api_port")
}

# api MÉTODO CAMINHO [CORPO]: chamada com a credencial de API da organização;
# exige 2xx e imprime o corpo. A versão esperada vai em if_match.
api() {
  local code args=(-sS -o "$work/resposta" -w '%{http_code}' -X "$1" "${connect[@]}" -H "Authorization: Bearer $api_token" -H 'Accept: application/json')
  if [[ $1 != GET ]]; then
    args+=(-H "Idempotency-Key: pitr-$(openssl rand -hex 12)")
  fi
  if [[ -n ${if_match:-} ]]; then
    args+=(-H "If-Match: \"$if_match\"")
  fi
  if [[ -n ${3:-} ]]; then
    args+=(-H 'Content-Type: application/json' -d "$3")
  fi
  code=$(curl "${args[@]}" "$api_url$2") || die "sem resposta da API em $1 $2"
  if [[ $code != 2* ]]; then
    cat "$work/resposta" >&2
    die "$1 $2 respondeu $code"
  fi
  cat "$work/resposta"
}

# login: fluxo OIDC real (API, formulário do Keycloak, callback), como no
# dr-check.sh; deixa o cookie em $work/cookies e o token CSRF em $csrf.
login() {
  local kc_port location action
  kc_port=$(port_of keycloak 8080)
  local kc=("${connect[@]}" --connect-to "$keycloak_host:127.0.0.1:$kc_port" -c "$work/cookies" -b "$work/cookies")
  rm -f "$work/cookies"
  curl -sS "${kc[@]}" -o /dev/null -D "$work/cabecalhos" "$api_url/auth/login"
  location=$(sed -n 's/^[Ll]ocation: //p' "$work/cabecalhos" | tr -d '\r')
  [[ $location == http://$keycloak_host/* ]] || die "login: redirecionamento inesperado para o provedor de identidade"
  curl -sS "${kc[@]}" -o "$work/login.html" "$location"
  action=$(grep -o 'action="[^"]*"' "$work/login.html" | head -n 1 | sed 's/^action="//; s/"$//; s/&amp;/\&/g')
  [[ -n $action ]] || die "login: formulário do provedor de identidade não encontrado"
  curl -sS "${kc[@]}" -o /dev/null -D "$work/cabecalhos" --data-urlencode "username=$demo_user" \
    --data-urlencode "password=$demo_password" "$action"
  location=$(sed -n 's/^[Ll]ocation: //p' "$work/cabecalhos" | tr -d '\r')
  [[ $location == "$api_url"/auth/callback* ]] || die "login: credenciais recusadas pelo provedor de identidade"
  curl -sS "${kc[@]}" -o /dev/null "$location"
  curl -sS "${connect[@]}" -b "$work/cookies" -o "$work/sessao.json" "$api_url/api/v1/session"
  csrf=$(jq -r '.csrf_token // empty' "$work/sessao.json")
  [[ -n $csrf ]] || die "login: sessão não criada"
}

# session_call MÉTODO CAMINHO [CORPO]: chamada com o cookie da sessão e CSRF;
# a versão esperada vai em if_match.
session_call() {
  local code args=(-sS -o "$work/resposta" -w '%{http_code}' -X "$1" "${connect[@]}" -b "$work/cookies" -H "X-CSRF-Token: $csrf")
  if [[ -n ${if_match:-} ]]; then
    args+=(-H "If-Match: \"$if_match\"")
  fi
  if [[ -n ${3:-} ]]; then
    args+=(-H 'Content-Type: application/json' -d "$3")
  fi
  code=$(curl "${args[@]}" "$api_url$2")
  if [[ $code != 2* ]]; then
    cat "$work/resposta" >&2
    die "$1 $2 respondeu $code"
  fi
  cat "$work/resposta"
}

# kc_admin MÉTODO CAMINHO [CORPO]: API de administração do Keycloak com o
# administrador do realm master; imprime o corpo.
kc_admin() {
  local kc_port token args
  kc_port=$(port_of keycloak 8080)
  token=$(curl -sS --fail -H "Host: $keycloak_host" --data-urlencode grant_type=password --data-urlencode client_id=admin-cli \
    --data-urlencode username=admin --data-urlencode "password=$keycloak_admin_password" \
    "http://127.0.0.1:$kc_port/realms/master/protocol/openid-connect/token" | jq -r .access_token)
  args=(-sS --fail-with-body -X "$1" -H "Host: $keycloak_host" -H "Authorization: Bearer $token")
  if [[ -n ${3:-} ]]; then
    args+=(-H 'Content-Type: application/json' -d "$3")
  fi
  curl "${args[@]}" "http://127.0.0.1:$kc_port/admin$2"
}
kc_user_count() { kc_admin GET "/realms/recorvia/users?exact=true&username=$1" | jq length; }
keycloak_ready() { [[ $(kc_user_count "$demo_user" 2>/dev/null) == 1 ]]; }

provider_pay() {
  curl -sS --fail-with-body -X POST -H 'Content-Type: application/json' -d '{"action":"pay"}' \
    "http://127.0.0.1:$paysim_port/sim/charges/$1/actions" >/dev/null
}

beta_invoice_status() { sql "SELECT i.status FROM invoices i JOIN tenants t ON t.id = i.tenant_id WHERE t.slug = 'beta' ORDER BY i.created_at LIMIT 1"; }
beta_charge() { sql "SELECT c.provider_charge_id FROM charges c JOIN tenants t ON t.id = c.tenant_id WHERE t.slug = 'beta' AND c.status = 'awaiting_payment' AND c.provider_charge_id IS NOT NULL LIMIT 1"; }
beta_charge_ready() { [[ -n $(beta_charge 2>/dev/null) ]]; }
beta_paid() { [[ $(beta_invoice_status 2>/dev/null) == paid ]]; }
manifest_of() { manifest_sql | psql_in "$RECORVIA_PG_DATABASE" >"$1"; }
ledger_balanced() {
  [[ $(sql "SELECT count(*) FROM (SELECT 1 FROM ledger_entries GROUP BY tenant_id, transaction_id, currency
             HAVING sum(amount) FILTER (WHERE direction = 'debit') IS DISTINCT FROM sum(amount) FILTER (WHERE direction = 'credit')) x") == 0 ]]
}
# backups: lista do armazenamento pelo agente (sem a saída de log do WAL-G).
backups() { dc exec -T postgres-backup recorvia-pgbackup list 2>/dev/null | grep '^base_' || true; }
backup_count_is() { [[ $(backups | wc -l) == "$1" ]]; }
timeline_backup_exists() { grep -q "^base_$(printf '%08X' "$1")" <<<"$(backups)"; }
# archived SEGMENTO: o segmento está completo e não espera mais arquivamento
# (sem o arquivo .ready em pg_wal/archive_status).
archived() {
  [[ $(psql_in postgres -c "SELECT pg_walfile_name(pg_current_wal_lsn()) > '$1'
          AND NOT EXISTS (SELECT 1 FROM pg_ls_archive_statusdir() WHERE name = '$1.ready')") == t ]]
}
agent_metric() {
  local metrics
  metrics=$(curl -sS --fail "http://127.0.0.1:$(port_of postgres-backup 9187)/metrics") || die "métricas do agente indisponíveis"
  awk -v m="$1" '$1 ~ "^"m"({|$)" { print $2; exit }' <<<"$metrics"
}
api_ready() { [[ $(curl -s -o "$work/readyz" -w '%{http_code}' "${connect[@]}" "$api_url/readyz") == 200 ]]; }
worker_ready() { [[ $(curl -s -o "$work/readyz-worker" -w '%{http_code}' "http://127.0.0.1:$worker_port/readyz") == 200 ]]; }
pgdata_empty() {
  # shellcheck disable=SC2016 # expandido no container
  dc run --rm --no-deps -T --user postgres --entrypoint sh postgres -c 'test -z "$(ls -A "$PGDATA" 2>/dev/null)"' >/dev/null 2>&1
}

# ---------- 1. Origem com arquivamento contínuo ----------

phase "1. Origem com arquivamento contínuo de WAL (projeto $project)"
app_key=$(openssl rand -base64 32)
backup_key=$(openssl rand -base64 32)
keycloak_admin_password=$(openssl rand -hex 16)
s3_access=pitr$(openssl rand -hex 6)
s3_secret=$(openssl rand -hex 20)
cat >"$env_file" <<EOF
POSTGRES_SUPERUSER_PASSWORD=$(openssl rand -hex 16)
RECORVIA_OWNER_PASSWORD=$(openssl rand -hex 16)
RECORVIA_APP_PASSWORD=$(openssl rand -hex 16)
RABBITMQ_USER=recorvia
RABBITMQ_PASSWORD=$(openssl rand -hex 16)
KEYCLOAK_ADMIN_PASSWORD=$keycloak_admin_password
RECORVIA_OIDC_CLIENT_SECRET=local-oidc-client-secret
RECORVIA_OIDC_REDIRECT_URL=$api_url/auth/callback
RECORVIA_POSTGRES_IMAGE_TAG=$image_tag
RECORVIA_PITR_S3_ACCESS_KEY_ID=$s3_access
RECORVIA_PITR_S3_SECRET_ACCESS_KEY=$s3_secret
RECORVIA_PITR_KEYCLOAK_DB_PASSWORD=$(openssl rand -hex 16)
RECORVIA_BACKUP_S3_PREFIX=s3://recorvia-backup/pitr-check
RECORVIA_BACKUP_S3_ENDPOINT=http://minio:9000
RECORVIA_BACKUP_S3_FORCE_PATH_STYLE=true
RECORVIA_BACKUP_S3_ACCESS_KEY_ID=$s3_access
RECORVIA_BACKUP_S3_SECRET_ACCESS_KEY=$s3_secret
RECORVIA_BACKUP_RETAIN_FULL=2
RECORVIA_BACKUP_RETAIN_DAYS=0
EOF
keycloak_db_password=$(sed -n 's/^RECORVIA_PITR_KEYCLOAK_DB_PASSWORD=//p' "$env_file")
# Chaves entregues aos serviços pelo ambiente das chamadas do compose (o
# arquivo de ambiente não as contém), como no dr-check.sh.
export RECORVIA_ENCRYPTION_KEYS="pitr1:$app_key" RECORVIA_ENCRYPTION_ACTIVE_KEY=pitr1 RECORVIA_BACKUP_ENCRYPTION_KEY=$backup_key

dc --profile seed down -v --remove-orphans >/dev/null 2>&1 || true
if [[ ${RECORVIA_PITR_BUILD:-1} == 1 ]]; then
  log "construindo as imagens (etiqueta $image_tag)"
  docker build -q -f deploy/postgres/Dockerfile -t "recorvia-postgres:$image_tag" . >/dev/null
  dc --profile seed build -q migrate-bootstrap api worker seed paysim keycloak
fi
dc up -d --wait --quiet-pull minio postgres
resolve_pg
# Banco do Keycloak no mesmo servidor: coberto pelo mesmo backup base e WAL.
psql_in postgres >/dev/null <<SQL
CREATE ROLE keycloak LOGIN PASSWORD '$keycloak_db_password';
CREATE DATABASE keycloak OWNER keycloak;
SQL
log "subindo o agente de backup, Keycloak, Redis, RabbitMQ, Mailpit, simulador, API e worker"
dc up -d --wait --quiet-pull postgres-backup redis rabbitmq keycloak mailpit paysim api worker
dc --profile seed run --rm -T seed >"$out_dir/seed.log" 2>&1 || {
  cat "$out_dir/seed.log" >&2
  die "seed falhou"
}
wait_until "primeiro backup base do agente" backup_count_is 1
first_backup=$(backups | awk '{ print $1 }')
[[ $(sql "SHOW archive_mode") == on ]] || die "archive_mode deveria estar ligado"
ok "origem no ar: PostgreSQL $(sql "SHOW server_version") com archive_mode=on e archive_timeout=$(sql "SHOW archive_timeout"); primeiro backup base do agente na partida ($first_backup)"

# Retenção (2 backups base): o terceiro backup remove o primeiro.
dc exec -T postgres-backup recorvia-pgbackup backup >"$out_dir/backup-manual-1.log" 2>&1
dc exec -T postgres-backup recorvia-pgbackup backup >"$out_dir/backup-manual-2.log" 2>&1
backups >"$out_dir/backups-antes-de-t.txt"
[[ $(wc -l <"$out_dir/backups-antes-de-t.txt") == 2 ]] || die "a retenção deveria manter 2 backups base: $(cat "$out_dir/backups-antes-de-t.txt")"
! grep -q "^$first_backup " "$out_dir/backups-antes-de-t.txt" || die "a retenção deveria ter removido o primeiro backup base"
grep -q 'esta chave$' "$out_dir/backups-antes-de-t.txt" || die "backups sem a impressão digital da chave"
base_backup=$(tail -n 1 "$out_dir/backups-antes-de-t.txt" | awk '{ print $1 }')
ok "dois backups base pedidos ao agente (recorvia-pgbackup backup); retenção de 2: o primeiro ($first_backup) foi removido com o WAL anterior, ficaram $(awk '{ print $1 }' "$out_dir/backups-antes-de-t.txt" | paste -sd ' ')"

# ---------- 2. Dados antes de T ----------

phase "2. Dados gravados antes de T"
refresh_ports
paysim_port=$(port_of paysim 8090)
worker_port=$(port_of worker 9091)
wait_until "Keycloak com o realm" keycloak_ready
login
[[ $(jq -r '.active_tenant.slug' "$work/sessao.json") == beta ]] || die "organização ativa inesperada"
non_delegable='["members.manage","platform_billing.manage","api_credentials.manage","customers.anonymize"]'
scopes=$(jq -c --argjson nd "$non_delegable" '[.active_tenant.permissions[] | select(. as $p | $nd | index($p) | not)]' "$work/sessao.json")
# Credenciais de API exigem um plano da plataforma que as inclua (ADR-085:
# a troca exige a versão da assinatura).
if_match=$(session_call GET /api/v1/platform/overview | jq -r .subscription.version) \
  session_call PUT /api/v1/platform/plan '{"plan_code":"escala"}' >/dev/null
api_token=$(session_call POST /api/v1/api-credentials "$(jq -n --argjson s "$scopes" '{name: "exercicio-pitr", scopes: $s}')" | jq -r .secret)
[[ $api_token == rcv_* ]] || die "credencial de API não criada"
wait_until "cobrança da fatura da organização beta criada no provedor" beta_charge_ready
charge_t=$(beta_charge)
[[ $(beta_invoice_status) == open ]] || die "a fatura da organização beta deveria estar em aberto"
kc_admin POST /realms/recorvia/users '{"username":"pitr-antes-de-t","enabled":true}' >/dev/null
psql_in postgres -c "CREATE DATABASE $probe_db" >/dev/null
probe "CREATE TABLE marcas (fase text PRIMARY KEY, gravada_em timestamptz NOT NULL DEFAULT clock_timestamp())" >/dev/null
probe "INSERT INTO marcas (fase) VALUES ('antes-de-t')" >/dev/null
ok "credencial de API da organização beta (login OIDC real), fatura em aberto com a cobrança $charge_t no provedor, usuário pitr-antes-de-t no Keycloak e marca antes-de-t, todos depois do último backup base ($base_backup)"

# ---------- 3. Instante T ----------

phase "3. Instante T, com a escrita da aplicação parada"
# O worker para (agendador, outbox e consumidores); a API não recebe
# requisições. O manifesto é o do banco em T.
dc stop worker >/dev/null 2>&1
manifest_of "$out_dir/origem-em-t.manifest"
t_iso=$(psql_in postgres -c "SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')")
t_at=$(now_s)
t_segment=$(psql_in postgres -c "SELECT pg_walfile_name(pg_current_wal_lsn())")
manifest_of "$work/origem-em-t-conferencia.manifest"
diff -u "$out_dir/origem-em-t.manifest" "$work/origem-em-t-conferencia.manifest" >&2 || die "o banco mudou durante o registro de T: a escrita da aplicação deveria estar parada"
ok "T = $t_iso (relógio do servidor), segmento de WAL $t_segment; manifesto do banco em T ($(grep -c '^table ' "$out_dir/origem-em-t.manifest") tabelas) estável antes e depois do registro"

# ---------- 4. Dados depois de T e atraso do arquivamento ----------

phase "4. Dados gravados depois de T e atraso do arquivamento"
probe "INSERT INTO marcas (fase) VALUES ('depois-de-t')" >/dev/null
after_segment=$(psql_in postgres -c "SELECT pg_walfile_name(pg_current_wal_lsn())")
dc start worker >/dev/null 2>&1
provider_pay "$charge_t"
wait_until "pagamento da organização beta confirmado pelo webhook do provedor" beta_paid
kc_admin POST /realms/recorvia/users '{"username":"pitr-depois-de-t","enabled":true}' >/dev/null
manifest_of "$out_dir/origem-no-desastre.manifest"
awk 'NR == FNR { if ($1 == "table") t[$2] = $3; next }
     $1 == "table" && $3 != t[$2] { printf "%-28s %8d %8d %+8d\n", $2, t[$2], $3, $3 - t[$2] }' \
  "$out_dir/origem-em-t.manifest" "$out_dir/origem-no-desastre.manifest" >"$out_dir/depois-de-t.txt"
(($(wc -l <"$out_dir/depois-de-t.txt") >= 1)) || die "a origem deveria ter avançado depois de T"
# O WAL até T precisa estar no armazenamento, e também o de depois de T: a
# recuperação só para ao encontrar um commit posterior ao instante.
wait_until "segmento de WAL de T ($t_segment) arquivado" archived "$t_segment"
t_archived_at=$(now_s)
wait_until "segmento de WAL da marca depois de T ($after_segment) arquivado" archived "$after_segment"
ok "depois de T: marca depois-de-t, pagamento da fatura beta pelo provedor e usuário pitr-depois-de-t; $(wc -l <"$out_dir/depois-de-t.txt") tabelas com linhas além de T; WAL de T arquivado $((t_archived_at - t_at)) s depois de T"

# RPO observado no instante do desastre: idade do último segmento arquivado
# (o que foi gravado depois dele se perderia) e segmentos pendentes.
disaster_at=$(now_s)
disaster_iso=$(now_iso)
last_archived=$(agent_metric recorvia_backup_wal_last_archived_timestamp_seconds)
pending=$(agent_metric recorvia_backup_wal_pending_segments)
rpo_at_disaster=$(awk -v d="$disaster_at" -v l="$last_archived" 'BEGIN { printf "%d", d - l }')
ok "métricas do agente no desastre: último segmento arquivado há $rpo_at_disaster s, $pending segmentos aguardando arquivamento"

# ---------- 5. Desastre ----------

phase "5. Desastre: servidor destruído"
dc rm -f -s -v api worker keycloak postgres-backup postgres redis rabbitmq >/dev/null 2>&1
for volume in postgres-data rabbitmq-data; do
  name=$(docker volume ls -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.volume=$volume")
  [[ -n $name ]] || die "volume $volume da origem não encontrado"
  docker volume rm "$name" >/dev/null
done
ok "PostgreSQL (com o volume e o banco do Keycloak), agente, Keycloak, API, worker, Redis e RabbitMQ removidos; MinIO e o provedor de pagamento continuam no ar"

# ---------- 6. Chave do backup errada ou ausente ----------

phase "6. Restauração com a chave do backup errada ou ausente"
wrong_key=$(openssl rand -base64 32)
code=0
RECORVIA_BACKUP_ENCRYPTION_KEY=$wrong_key pitr_restore --target-time "$t_iso" >"$out_dir/restauracao-chave-errada.txt" 2>&1 || code=$?
((code != 0)) || die "restauração com a chave errada deveria falhar"
grep -q 'chave de cifragem do backup diferente da usada no backup base' "$out_dir/restauracao-chave-errada.txt" ||
  die "restauração com a chave errada sem a explicação esperada: $(tail -n 5 "$out_dir/restauracao-chave-errada.txt")"
pgdata_empty || die "a tentativa com a chave errada alterou o volume de dados"
code=0
RECORVIA_BACKUP_ENCRYPTION_KEY='' pitr_restore --target-time "$t_iso" >"$out_dir/restauracao-chave-ausente.txt" 2>&1 || code=$?
((code != 0)) || die "restauração sem a chave deveria falhar"
grep -q 'chave de cifragem do backup ausente' "$out_dir/restauracao-chave-ausente.txt" || die "restauração sem a chave sem a explicação esperada"
pgdata_empty || die "a tentativa sem a chave alterou o volume de dados"
# O WAL-G, sem a conferência da impressão digital: com outra chave, o backup
# base e o WAL não decifram (cifragem autenticada); o restore_command devolve
# erro fatal, que interrompe a recuperação em vez de encerrá-la cedo.
# shellcheck disable=SC2016 # expandido no container, com o ambiente do serviço
walg_env='export WALG_S3_PREFIX=$RECORVIA_BACKUP_S3_PREFIX AWS_ENDPOINT=$RECORVIA_BACKUP_S3_ENDPOINT AWS_S3_FORCE_PATH_STYLE=true
  export AWS_REGION=us-east-1 AWS_ACCESS_KEY_ID=$RECORVIA_BACKUP_S3_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY=$RECORVIA_BACKUP_S3_SECRET_ACCESS_KEY
  export WALG_LIBSODIUM_KEY=$RECORVIA_BACKUP_ENCRYPTION_KEY WALG_LIBSODIUM_KEY_TRANSFORM=base64 WALG_DOWNLOAD_CONCURRENCY=1 WALG_DOWNLOAD_FILE_RETRIES=0'
code=0
RECORVIA_BACKUP_ENCRYPTION_KEY=$wrong_key dc run --rm --no-deps -T --user postgres --entrypoint sh postgres -c \
  "$walg_env; mkdir -p /tmp/extracao && wal-g backup-fetch /tmp/extracao $base_backup" >"$out_dir/walg-chave-errada.txt" 2>&1 || code=$?
((code != 0)) || die "o WAL-G extraiu o backup base com outra chave"
grep -q 'failed to extract files' "$out_dir/walg-chave-errada.txt" || die "WAL-G com outra chave sem o erro de extração esperado"
code=0
RECORVIA_BACKUP_ENCRYPTION_KEY=$wrong_key dc run --rm --no-deps -T --user postgres --entrypoint recorvia-pgbackup postgres \
  wal-fetch "$t_segment" /tmp/segmento >"$out_dir/restore-command-chave-errada.txt" 2>&1 || code=$?
((code == 126)) || die "restore_command com outra chave devolveu $code; esperado 126 (erro fatal)"
code=0
dc run --rm --no-deps -T --user postgres --entrypoint recorvia-pgbackup postgres \
  wal-fetch 0000000100000000000000FF /tmp/segmento >"$work/restore-command-ausente.txt" 2>&1 || code=$?
((code == 1)) || die "restore_command para um segmento inexistente devolveu $code; esperado 1 (fim do WAL)"
ok "chave do backup errada e ausente recusadas pelo roteiro de restauração antes de baixar qualquer arquivo, com o volume vazio; com outra chave, o WAL-G não extrai o backup base e o restore_command devolve 126 (fatal), enquanto um segmento inexistente devolve 1 (fim do WAL)"

# ---------- 7. Restauração até T ----------

phase "7. Restauração até T num servidor novo"
restore_started=$(now_s)
RECORVIA_PITR_SUMMARY="$work/resumo.txt" RECORVIA_PITR_DIR="$out_dir" pitr_restore --target-time "$t_iso" \
  --manifest "$out_dir/origem-em-t.manifest" 2>&1 | tee "$out_dir/restauracao.txt" >&2
restore_finished=$(now_s)
summary_value() { sed -n "s/^$1=//p" "$work/resumo.txt"; }
resolve_pg
[[ $(probe "SELECT string_agg(fase, ',' ORDER BY fase) FROM marcas") == antes-de-t ]] || die "o banco de conferência deveria ter só a marca antes-de-t"
[[ $(beta_invoice_status) == open ]] || die "a fatura da organização beta deveria estar em aberto no instante T"
[[ $(sql "SELECT count(*) FROM api_credentials WHERE name = 'exercicio-pitr'") == 1 ]] || die "a credencial de API criada antes de T deveria existir"
timeline=$(summary_value timeline)
((timeline >= 2)) || die "o servidor restaurado deveria estar numa linha do tempo nova"
ok "restaurado até T a partir de $base_backup: manifesto idêntico ao de T, ledger balanceado, RLS ativa, marca antes-de-t presente e depois-de-t ausente, fatura beta em aberto, credencial de API anterior a T presente; linha do tempo $timeline; último commit aplicado em $(summary_value last_commit)"
wait_until "backup base da linha do tempo $timeline pelo agente" timeline_backup_exists "$timeline"
backups >"$out_dir/backups-depois-da-restauracao.txt"
ok "agente religado: backup base da linha do tempo $timeline feito ao reiniciar ($(grep "^base_$(printf '%08X' "$timeline")" "$out_dir/backups-depois-da-restauracao.txt" | awk '{ print $1 }' | head -n 1))"

# ---------- 8. Aplicação religada ----------

phase "8. Aplicação religada: chaves, API, conciliação da janela e worker"
dc run --rm -T --no-deps worker keyring-check >"$out_dir/keyring-check.txt" 2>&1 || {
  cat "$out_dir/keyring-check.txt" >&2
  die "as chaves do cofre não decifram o banco restaurado"
}
grep -q -F 'keyring compatível com o banco' "$out_dir/keyring-check.txt" || die "keyring-check sem a confirmação esperada"
dc up -d --wait redis keycloak
wait_until "Keycloak sobre o banco restaurado" keycloak_ready
[[ $(kc_user_count pitr-antes-de-t) == 1 && $(kc_user_count pitr-depois-de-t) == 0 ]] || die "o Keycloak deveria ter o usuário de antes de T e não o de depois"
ok "Keycloak sobre o banco restaurado (mesmo backup e WAL): usuário pitr-antes-de-t presente, pitr-depois-de-t ausente"
dc up -d --wait api
refresh_ports
wait_until "readiness da API" api_ready
api_ready_at=$(now_s)
# Conciliação da janela perdida com o worker parado (como no dr-check.sh):
# o pagamento feito no provedor depois de T volta como divergência.
run_id=$(api POST /api/v1/reconciliation/runs | jq -r .id)
timeout 900 docker compose "${dc_argv[@]}" run --rm -T --no-deps worker reconcile-pending >"$out_dir/conciliacao-da-janela.txt" 2>&1 || {
  cat "$out_dir/conciliacao-da-janela.txt" >&2
  die "reconcile-pending não concluiu a conciliação da janela"
}
[[ $(api GET "/api/v1/reconciliation/runs/$run_id" | jq -r .status) == completed ]] || die "conciliação não concluída"
api GET "/api/v1/reconciliation/discrepancies?status=open&limit=100" >"$work/divergencias.json"
paid=$(jq -r --arg c "$charge_t" '.data[] | select(.kind == "payment_not_reflected" and .provider_charge_id == $c) | .id' "$work/divergencias.json")
[[ -n $paid ]] || die "a conciliação não apontou o pagamento feito depois de T"
if_match=$(jq -r --arg id "$paid" '.data[] | select(.id == $id) | .version' "$work/divergencias.json") \
  api POST "/api/v1/reconciliation/discrepancies/$paid/resolve" \
  '{"resolution":"accept_provider","justification":"Pagamento recebido no provedor depois do instante da restauração."}' >/dev/null
beta_paid || die "a fatura beta deveria estar paga depois da conciliação"
reconciled_at=$(now_s)
ok "API pronta com o worker parado; conciliação pela API e reconcile-pending: pagamento depois de T (payment_not_reflected) adotado do provedor"
dc up -d --wait rabbitmq worker
worker_port=$(port_of worker 9091)
wait_until "readiness do worker" worker_ready
worker_ready_at=$(now_s)
outbox_drained() { [[ $(sql "SELECT count(*) FROM outbox_events WHERE published_at IS NULL") == 0 ]]; }
wait_until "outbox do banco restaurado publicada" outbox_drained
ledger_balanced || die "ledger desbalanceado depois da retomada"
[[ $(sql "SELECT count(*) FROM (SELECT 1 FROM payments WHERE charge_id IS NOT NULL GROUP BY tenant_id, charge_id HAVING count(*) > 1) x") == 0 ]] ||
  die "pagamento duplicado para a mesma cobrança"
[[ $(sql "SELECT count(*) FROM dead_letters") == 0 ]] || die "mensagens em dead letter depois da retomada"
done_at=$(now_s)
ok "worker religado depois da conciliação: readiness, outbox publicada, ledger balanceado, sem pagamento duplicado nem dead letters"

# ---------- Relatório ----------

phase "Relatório"
revision=$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo desconhecida)
if [[ -n $(git -C "$root" status --porcelain 2>/dev/null) ]]; then
  revision="$revision com alterações locais"
fi
{
  echo "# Exercício de recuperação para um ponto no tempo"
  echo
  echo "Executado em $(now_iso), revisão $revision, docker compose $compose_version, projeto \`$project\`."
  echo
  echo "## Medições (observadas neste exercício; não são garantia)"
  echo
  echo "| Medida | Valor |"
  echo "|---|---|"
  echo "| Instante T (alvo da restauração) | $t_iso |"
  echo "| Backup base usado | $base_backup |"
  echo "| Atraso do arquivamento do WAL de T (de T ao segmento no armazenamento) | $((t_archived_at - t_at)) s |"
  echo "| Idade do último segmento arquivado no desastre (RPO observado) | $rpo_at_disaster s |"
  echo "| Segmentos aguardando arquivamento no desastre | $pending |"
  echo "| Instante do desastre | $disaster_iso |"
  echo "| Extração do backup base | $(summary_value fetch_seconds) s |"
  echo "| Recuperação até T (WAL do armazenamento e promoção) | $(summary_value recovery_seconds) s |"
  echo "| Roteiro de restauração completo (extração, recuperação e conferências) | $((restore_finished - restore_started)) s |"
  echo "| Do desastre à API pronta sobre o banco restaurado (inclui as tentativas com chave errada e ausente) | $((api_ready_at - disaster_at)) s |"
  echo "| Etapa: conciliação da janela pela API | $((reconciled_at - api_ready_at)) s |"
  echo "| Do desastre ao worker pronto, com a janela conciliada (RTO observado) | $((worker_ready_at - disaster_at)) s |"
  echo "| Do desastre ao fim das conferências | $((done_at - disaster_at)) s |"
  echo
  echo "## Gravado depois de T (linhas por tabela, descartadas pela restauração)"
  echo
  echo '```text'
  printf '%-28s %8s %8s %8s\n' tabela T desastre diferença
  cat "$out_dir/depois-de-t.txt"
  echo '```'
  echo
  echo "## Backups base"
  echo
  echo '```text'
  echo "antes de T:"
  cat "$out_dir/backups-antes-de-t.txt"
  echo "depois da restauração:"
  cat "$out_dir/backups-depois-da-restauracao.txt"
  echo '```'
  echo
  echo "## Conferências"
  echo
  cat "$checks_file"
} >"$out_dir/relatorio.md"
cat "$out_dir/relatorio.md" >&2
finished=1
log "exercício de recuperação para um ponto no tempo concluído: relatório em $out_dir/relatorio.md"
