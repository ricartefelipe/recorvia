#!/usr/bin/env bash
# Exercício de recuperação de desastre de ponta a ponta (ADR-064).
# Uso: make dr-check (ou bash deploy/backup/dr-check.sh).
#
# Num projeto isolado do docker compose (deploy/backup/dr.compose.yaml):
#   1. sobe o ambiente de origem e gera dados pelo seed e pela API, inclusive
#      segredos cifrados pelo keyring com duas versões de chave (rotação em
#      duas fases, com a API e o worker convivendo em fases diferentes);
#   2. guarda as chaves num cofre à parte do backup e faz o backup
#      (deploy/backup/backup.sh);
#   3. deixa a origem trabalhar mais um pouco (janela perdida) e a destrói
#      (banco, broker, sessões, API e worker; provedor de pagamento, servidor
#      de e-mail, provedor de identidade e sistema do tenant são externos e
#      sobrevivem);
#   4. restaura num servidor PostgreSQL novo (bootstrap, pg_restore, migrate
#      up, restore-check) e confere o manifesto e o ledger, enquanto Redis,
#      RabbitMQ e o servidor de e-mail sobem em paralelo;
#   5. confere que chave errada ou ausente é recusada com erro claro, sem
#      alterar o banco, e que as chaves do cofre são aceitas;
#   6. sobe a API sobre o banco restaurado, concilia a janela perdida com o
#      provedor e só então religa o worker;
#   7. confere readiness, decifragem real dos segredos (cobrança no provedor,
#      webhook do provedor, entrega de webhook assinada, e-mail pendente),
#      ausência de efeitos duplicados e mede os tempos (RPO e RTO observados).
#
# Variáveis (todas opcionais):
#   RECORVIA_DR_PROJECT    projeto do compose (padrão: recorvia-drcheck),
#                          derrubado com os volumes no início e no fim; nunca
#                          o projeto do ambiente local, e recusado se já tiver
#                          containers ou volumes que o exercício não criou
#   RECORVIA_DR_REUSE_PROJECT  1 confirma a remoção de um projeto existente
#                          que o exercício não criou
#   RECORVIA_DR_DIR        relatório, manifestos, logs e o dump do exercício
#                          (padrão: backups/dr-check, fora do Git). Precisa
#                          ser um diretório próprio: novo, vazio ou criado por
#                          uma execução anterior (os arquivos dela vão para
#                          anterior/); nunca o diretório dos backups
#   RECORVIA_DR_IMAGE_TAG  etiqueta das imagens construídas (padrão: dr)
#   RECORVIA_DR_BUILD      0 reutiliza as imagens já construídas (padrão: 1)
#   RECORVIA_DR_KEEP       1 mantém o ambiente e o cofre para inspeção
#   RECORVIA_DR_TIMEOUT    espera máxima, em segundos, de cada condição
#                          (padrão: 300)
#
# Exige docker com o plugin compose 2.24 ou superior, curl, jq, openssl,
# sha256sum, base64, od e realpath. Não publica portas fixas no host, só
# remove containers, volumes e redes do próprio projeto e só mexe nos
# arquivos que ele mesmo gera.
set -euo pipefail
umask 077
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)
cd "$root"

project=${RECORVIA_DR_PROJECT:-recorvia-drcheck}
dr_dir=${RECORVIA_DR_DIR:-backups/dr-check}
image_tag=${RECORVIA_DR_IMAGE_TAG:-dr}
timeout_s=${RECORVIA_DR_TIMEOUT:-300}
# Diretório dos backups de quem chama (make backup): o exercício não o usa.
caller_backup_dir=${RECORVIA_BACKUP_DIR:-backups}
export RECORVIA_DR_IMAGE_TAG=$image_tag
export RECORVIA_BACKUP_DIR="$dr_dir/backup"
# shellcheck source=deploy/backup/common.sh
source "$here/common.sh"
# shellcheck source=deploy/backup/dr-lib.sh
source "$here/dr-lib.sh"

# URLs públicas do ambiente (como o navegador as veria). O exercício chega a
# elas pelas portas livres que o Docker escolheu, com --connect-to.
api_url=http://localhost:18081
keycloak_host=localhost:8180
demo_user=ana.owner@acme.test
demo_password='Recorvia#Demo2026'

phase() { echo >&2; echo "=== $* ===" >&2; }
ok() {
  echo "    ok: $*" >&2
  echo "- $*" >>"$checks_file"
}
note() { echo "$*" >>"$notes_file"; }
now_s() { date -u +%s; }
now_iso() { date -u +%Y-%m-%dT%H:%M:%SZ; }

for tool in docker curl jq openssl sha256sum base64 od realpath; do
  command -v "$tool" >/dev/null || die "$tool não encontrado"
done
compose_version=$(docker compose version --short 2>/dev/null) || die "plugin docker compose não encontrado"
IFS=. read -r compose_major compose_minor _ <<<"${compose_version#v}"
if ((compose_major < 2 || (compose_major == 2 && compose_minor < 24))); then
  die "docker compose $compose_version: o exercício exige 2.24 ou superior"
fi
[[ $project =~ ^[a-z0-9][a-z0-9_-]*$ ]] || die "nome de projeto inválido: $project"
[[ $project != recorvia ]] || die "o exercício não pode usar o projeto do ambiente local (recorvia)"
[[ $timeout_s =~ ^[1-9][0-9]*$ ]] || die "RECORVIA_DR_TIMEOUT inválido: $timeout_s"
# Antes de criar, mover ou remover qualquer coisa: o diretório de saída e o
# projeto do compose precisam ser do exercício.
require_safe_dr_dir "$dr_dir" "$root" "$caller_backup_dir"
require_own_project "$project"
# Variáveis do ambiente de quem chama que o compose usaria no lugar das do
# exercício (projeto, arquivos, URLs, senhas, chaves e modo da conferência).
unset COMPOSE_PROJECT_NAME COMPOSE_FILE COMPOSE_PROFILES KEYCLOAK_PORT WEB_PORT API_PORT OTEL_EXPORTER_OTLP_ENDPOINT \
  POSTGRES_SUPERUSER_PASSWORD RECORVIA_OWNER_PASSWORD RECORVIA_APP_PASSWORD RABBITMQ_USER RABBITMQ_PASSWORD \
  KEYCLOAK_ADMIN_USER KEYCLOAK_ADMIN_PASSWORD RECORVIA_OIDC_CLIENT_SECRET RECORVIA_OIDC_REDIRECT_URL \
  RECORVIA_ENCRYPTION_KEYS RECORVIA_ENCRYPTION_ACTIVE_KEY RECORVIA_ENCRYPTION_STARTUP_CHECK RECORVIA_ENCRYPTION_LOST_KEYS

# O cofre guarda as chaves de cifragem e as senhas do ambiente, fora do
# diretório do backup e fora do repositório.
vault=$(mktemp -d "${TMPDIR:-/tmp}/recorvia-dr-cofre.XXXXXX")
env_file="$vault/ambiente.env"
keyring_file="$vault/keyring.env"
work="$vault/trabalho"
mkdir -p "$work"
prepare_dr_dir "$dr_dir"
checks_file="$dr_dir/conferencias.txt"
notes_file="$dr_dir/observacoes.txt"
: >"$checks_file"
: >"$notes_file"

dc() {
  docker compose -p "$project" --env-file "$env_file" -f "$root/compose.yaml" -f "$root/deploy/backup/dr.compose.yaml" "$@"
}

# use_keys CHAVES ATIVA: chaves entregues à API e ao worker nas próximas
# chamadas do compose (o arquivo de ambiente não as contém).
use_keys() {
  export RECORVIA_ENCRYPTION_KEYS=$1 RECORVIA_ENCRYPTION_ACTIVE_KEY=$2
}

finished=0
cleanup() {
  local code=$?
  trap - EXIT
  if ((finished == 0)); then
    echo "erro: exercício interrompido (código $code); logs em $dr_dir/compose-logs.txt" >&2
  fi
  dc ps -a >"$dr_dir/compose-ps.txt" 2>&1 || true
  dc logs --no-color --tail 2000 >"$dr_dir/compose-logs.txt" 2>&1 || true
  if [[ ${RECORVIA_DR_KEEP:-0} == 1 ]]; then
    echo "==> ambiente mantido (RECORVIA_DR_KEEP=1): projeto $project, cofre em $vault" >&2
    echo "    para remover: docker compose -p $project down -v --remove-orphans && rm -rf $vault" >&2
    echo "    (a próxima execução do exercício também remove o projeto $project)" >&2
  else
    dc --profile seed down -v --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$vault"
  fi
  exit "$code"
}
trap cleanup EXIT

# ---------- Funções auxiliares ----------

# wait_until DESCRIÇÃO COMANDO...: repete o comando até ter sucesso.
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

# refresh_ports: as portas do host mudam quando um container é recriado.
refresh_ports() {
  api_port=$(port_of api 8081)
  connect=(--connect-to "localhost:18081:127.0.0.1:$api_port")
}

sql() { psql_in "$RECORVIA_PG_DATABASE" -c "$1"; }

# api_call MÉTODO CAMINHO [CORPO]: grava o corpo da resposta em $work/resposta
# e devolve o status HTTP na saída padrão.
api_call() {
  local method=$1 path=$2 body=${3:-}
  local args=(-sS -o "$work/resposta" -w '%{http_code}' -X "$method" "${connect[@]}" -H "Authorization: Bearer $api_token"
    -H 'Accept: application/json')
  if [[ $method != GET ]]; then
    args+=(-H "Idempotency-Key: dr-$(openssl rand -hex 12)")
  fi
  if [[ -n $body ]]; then
    args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  curl "${args[@]}" "$api_url$path"
}

# api MÉTODO CAMINHO [CORPO]: exige resposta 2xx e imprime o corpo.
api() {
  local code
  code=$(api_call "$@") || die "sem resposta da API em $1 $2"
  if [[ $code != 2* ]]; then
    cat "$work/resposta" >&2
    die "$1 $2 respondeu $code"
  fi
  cat "$work/resposta"
}

# api_quiet: como api, mas só devolve falha (para condições de espera).
api_quiet() {
  local code
  code=$(api_call "$@" 2>/dev/null) || return 1
  [[ $code == 2* ]] || return 1
  cat "$work/resposta"
}

# login: fluxo OIDC real (API, formulário do Keycloak, callback) com curl.
# Deixa o cookie de sessão em $work/cookies e o token CSRF em $csrf.
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

# session_call MÉTODO CAMINHO CORPO: chamada com o cookie da sessão e CSRF
# (operações que não aceitam credencial de API).
session_call() {
  local code
  code=$(curl -sS -o "$work/resposta" -w '%{http_code}' -X "$1" "${connect[@]}" -b "$work/cookies" -H "X-CSRF-Token: $csrf" \
    -H 'Content-Type: application/json' -d "$3" "$api_url$2")
  if [[ $code != 2* ]]; then
    cat "$work/resposta" >&2
    die "$1 $2 respondeu $code"
  fi
  cat "$work/resposta"
}

# provider MÉTODO CAMINHO [CORPO]: API do provedor de pagamento (simulador)
# com a chave de API do tenant, como faria quem opera a conta no provedor.
provider() {
  local args=(-sS --fail-with-body -X "$1" -H "Authorization: Bearer $provider_key")
  if [[ -n ${3:-} ]]; then
    args+=(-H 'Content-Type: application/json' -d "$3")
  fi
  curl "${args[@]}" "http://127.0.0.1:$paysim_port$2"
}

# provider_pay COBRANÇA: o cliente paga no provedor (que avisa por webhook).
provider_pay() {
  curl -sS --fail-with-body -X POST -H 'Content-Type: application/json' -d '{"action":"pay"}' \
    "http://127.0.0.1:$paysim_port/sim/charges/$1/actions" >/dev/null
}

sink_requests() { curl -sS --fail "http://127.0.0.1:$sink_port/requests?name=acme"; }

# signature_ok ARQUIVO ÍNDICE: confere a assinatura de uma entrega recebida
# com o segredo que o tenant guardou ao criar o endpoint (Standard Webhooks:
# HMAC-SHA256 de "id.timestamp.corpo" com a chave em base64 depois de whsec_).
signature_ok() {
  local file=$1 i=$2 id ts header key_hex expected
  id=$(jq -r ".[$i].headers[\"webhook-id\"]" "$file")
  ts=$(jq -r ".[$i].headers[\"webhook-timestamp\"]" "$file")
  header=$(jq -r ".[$i].headers[\"webhook-signature\"]" "$file")
  key_hex=$(printf %s "${hook_secret#whsec_}" | base64 -d | od -An -tx1 | tr -d ' \n')
  expected=$({
    printf '%s.%s.' "$id" "$ts"
    jq -r ".[$i].body_base64" "$file" | base64 -d
  } | openssl dgst -sha256 -mac HMAC -macopt "hexkey:$key_hex" -binary | base64)
  [[ " $header " == *" v1,$expected "* ]]
}

# manifest_of ARQUIVO: manifesto do banco em uso (mesmas consultas do backup).
manifest_of() { manifest_sql | psql_in "$RECORVIA_PG_DATABASE" >"$1"; }

mail_count() {
  curl -sS --fail "http://127.0.0.1:$mail_port/api/v1/messages?limit=200" |
    jq --arg to "$1" --arg subject "$2" '[.messages[] | select(any(.To[]; .Address == $to)) | select(.Subject | contains($subject))] | length'
}

# ---------- Condições de espera ----------

invoice_of_subscription() { api_quiet GET "/api/v1/invoices?subscription_id=$1" | jq -e -r '.data[0].id // empty'; }
charge_awaiting() { # FATURA: cobrança criada no provedor
  api_quiet GET "/api/v1/charges?invoice_id=$1" |
    jq -e -r '[.data[] | select(.status == "awaiting_payment" and .provider_charge_id != null)][0].provider_charge_id // empty'
}
invoice_is() { [[ $(api_quiet GET "/api/v1/invoices/$1" | jq -r '.status // empty') == "$2" ]]; }
notification_is() { # FATURA SITUAÇÃO TENTATIVAS_MÍNIMAS
  api_quiet GET "/api/v1/notifications?invoice_id=$1" |
    jq -e --arg s "$2" --argjson n "$3" 'any(.data[]; .template_key == "invoice_issued" and .status == $s and .attempts >= $n)' >/dev/null
}
sink_has() { # TIPO SEQ_MÍNIMO: entrega do tipo recebida depois da sequência
  sink_requests | jq -e --arg t "$1" --argjson s "$2" 'any(.[]; .seq > $s and .headers["x-recorvia-event-type"] == $t)' >/dev/null
}
api_ready() { [[ $(curl -s -o "$work/readyz" -w '%{http_code}' "${connect[@]}" "$api_url/readyz") == 200 ]]; }
worker_ready() { [[ $(curl -s -o "$work/readyz-worker" -w '%{http_code}' "http://127.0.0.1:$worker_port/readyz") == 200 ]]; }
outbox_drained() { [[ $(sql "SELECT count(*) FROM outbox_events WHERE published_at IS NULL") == 0 ]]; }
jobs_ran_since() { # CONTAGEM_ANTERIOR: os dois agendamentos rodaram mais duas vezes
  [[ $(sql "SELECT count(*) FROM scheduled_jobs WHERE name IN ('billing.run_cycle', 'payments.recover_operations') AND run_count >= $1 + 2") == 2 ]]
}
beta_invoice_paid() {
  [[ $(sql "SELECT count(*) FROM invoices i JOIN tenants t ON t.id = i.tenant_id WHERE t.slug = 'beta' AND i.status = 'paid'") == 1 ]]
}

# effects: retrato dos efeitos financeiros locais (para conferir que nada se
# repete): faturas, cobranças, pagamentos e lançamentos por organização.
effects() {
  sql "SELECT t.slug || ' faturas=' || (SELECT count(*) FROM invoices i WHERE i.tenant_id = t.id)
              || ' cobrancas=' || (SELECT count(*) FROM charges c WHERE c.tenant_id = t.id)
              || ' pagamentos=' || (SELECT count(*) FROM payments p WHERE p.tenant_id = t.id)
              || ' lancamentos=' || (SELECT count(*) FROM ledger_transactions l WHERE l.tenant_id = t.id)
         FROM tenants t ORDER BY t.slug"
}

# ledger_balanced: nenhum lançamento desbalanceado e, por moeda, débitos
# iguais a créditos.
ledger_balanced() {
  local unbalanced totals
  unbalanced=$(sql "SELECT count(*) FROM (SELECT transaction_id FROM ledger_entries GROUP BY tenant_id, transaction_id, currency
                      HAVING sum(amount) FILTER (WHERE direction = 'debit') IS DISTINCT FROM sum(amount) FILTER (WHERE direction = 'credit')) x")
  totals=$(sql "SELECT count(*) FROM (SELECT currency FROM ledger_entries GROUP BY currency
                  HAVING sum(amount) FILTER (WHERE direction = 'debit') IS DISTINCT FROM sum(amount) FILTER (WHERE direction = 'credit')) x")
  [[ $unbalanced == 0 && $totals == 0 ]]
}

# ---------- 1. Ambiente de origem ----------

phase "1. Ambiente de origem (projeto $project)"
key1=$(openssl rand -base64 32)
key2=$(openssl rand -base64 32)
cat >"$env_file" <<EOF
POSTGRES_SUPERUSER_PASSWORD=$(openssl rand -hex 16)
RECORVIA_OWNER_PASSWORD=$(openssl rand -hex 16)
RECORVIA_APP_PASSWORD=$(openssl rand -hex 16)
RABBITMQ_USER=recorvia
RABBITMQ_PASSWORD=$(openssl rand -hex 16)
KEYCLOAK_ADMIN_PASSWORD=$(openssl rand -hex 16)
RECORVIA_OIDC_CLIENT_SECRET=local-oidc-client-secret
RECORVIA_OIDC_REDIRECT_URL=$api_url/auth/callback
EOF
# A chave entra no cofre antes de cifrar o primeiro dado.
printf 'RECORVIA_ENCRYPTION_KEYS=dr1:%s\nRECORVIA_ENCRYPTION_ACTIVE_KEY=dr1\n' "$key1" >"$keyring_file"
use_keys "dr1:$key1" dr1

dc --profile seed down -v --remove-orphans >/dev/null 2>&1 || true
if [[ ${RECORVIA_DR_BUILD:-1} == 1 ]]; then
  log "construindo as imagens (etiqueta $image_tag)"
  dc --profile seed build -q migrate-bootstrap api worker seed paysim keycloak hooksink
fi
log "subindo PostgreSQL, Redis, RabbitMQ, Keycloak, Mailpit, simulador, receptor de webhooks, API e worker"
dc up -d --wait --quiet-pull postgres redis rabbitmq keycloak mailpit paysim hooksink api worker
log "seed de demonstração (duas organizações; credenciais de provedor cifradas com a chave dr1)"
dc --profile seed run --rm -T seed >"$dr_dir/seed.log" 2>&1 || {
  cat "$dr_dir/seed.log" >&2
  die "seed falhou"
}
resolve_pg() {
  RECORVIA_PG_CONTAINER=$(dc ps -q postgres)
  export RECORVIA_PG_CONTAINER
  resolve_container
}
resolve_pg
paysim_port=$(port_of paysim 8090)
sink_port=$(port_of hooksink 8099)
ok "origem no ar: PostgreSQL $(sql "SHOW server_version"), migration $(sql "SELECT max(version_id) FROM goose_db_version WHERE is_applied"), $(sql "SELECT count(*) FROM tenants") organizações do seed"

# ---------- 2. Rotação de chave e dados pela API ----------

phase "2. Rotação da chave de cifragem em duas fases e dados criados pela API"
# running_keyring SERVIÇO: chaves do processo em execução, lidas pelo
# keyring-check no próprio container (confere também os dados já cifrados).
running_keyring() { dc exec -T "$1" /app keyring-check | sed -n 's/^chaves configuradas: \(.*\); organizações conferidas.*$/\1/p'; }
expect_keyring() { # SERVIÇO ESPERADO
  local got
  got=$(running_keyring "$1") || die "keyring-check falhou no serviço $1"
  [[ $got == "$2" ]] || die "serviço $1 com o keyring \"$got\"; esperado \"$2\""
}

# Fase 1: a chave nova entra no cofre e é distribuída a todos os processos,
# ainda sem ser a ativa. Nenhum processo cifra com ela enquanto houver outro
# que não a conheça.
printf 'RECORVIA_ENCRYPTION_KEYS=dr1:%s,dr2:%s\nRECORVIA_ENCRYPTION_ACTIVE_KEY=dr1\n' "$key1" "$key2" >"$keyring_file"
use_keys "dr1:$key1,dr2:$key2" dr1
dc up -d --wait --no-deps api worker
expect_keyring api "dr1, dr2 (ativa: dr1)"
expect_keyring worker "dr1, dr2 (ativa: dr1)"
ok "rotação, fase 1: API e worker reiniciados com as chaves dr1 e dr2, ainda com dr1 ativa (keyring-check nos dois processos)"

# Fase 2: a chave nova passa a ser a ativa. O reinício não é atômico: a API
# já cifra com dr2 enquanto o worker continua na fase 1 (conhece dr2, cifra
# com dr1). O trabalho abaixo acontece nessa convivência; o worker só é
# reiniciado depois dele.
printf 'RECORVIA_ENCRYPTION_KEYS=dr1:%s,dr2:%s\nRECORVIA_ENCRYPTION_ACTIVE_KEY=dr2\n' "$key1" "$key2" >"$keyring_file"
use_keys "dr1:$key1,dr2:$key2" dr2
dc up -d --wait --no-deps api
refresh_ports
expect_keyring api "dr1, dr2 (ativa: dr2)"
expect_keyring worker "dr1, dr2 (ativa: dr1)"
ok "rotação, fase 2 em andamento: API com dr2 ativa e worker ainda com dr1 ativa (as duas chaves nos dois processos)"

login
[[ $(jq -r '.active_tenant.slug' "$work/sessao.json") == acme ]] || die "organização ativa inesperada"
session_call PUT /api/v1/platform/plan '{"plan_code":"escala"}' >/dev/null
scopes=$(jq -c '[.active_tenant.permissions[] | select(. != "api_credentials.manage" and . != "members.manage" and . != "platform_billing.manage")]' "$work/sessao.json")
api_token=$(session_call POST /api/v1/api-credentials "$(jq -n --argjson s "$scopes" '{name: "exercicio-dr", scopes: $s}')" | jq -r .secret)
[[ $api_token == rcv_* ]] || die "credencial de API não criada"
ok "login OIDC, plano com webhooks e credencial de API da organização acme"

# Credenciais da conta de provedor configuradas pela API (cifradas com dr2);
# as da organização beta, criadas pelo seed, continuam cifradas com dr1.
provider_key="sim_dr_$(openssl rand -hex 8)"
provider_secret=$(openssl rand -hex 24)
api PUT /api/v1/payment-providers/simulator "$(jq -n --arg k "$provider_key" --arg s "$provider_secret" \
  '{environment: "local", api_key: $k, webhook_secret: $s, make_default: true}')" >/dev/null
event_types=$(api GET /api/v1/webhook-event-types | jq -c .data)
api POST /api/v1/webhook-endpoints "$(jq -n --argjson e "$event_types" \
  '{url: "http://hooksink:8099/hooks/acme", description: "Sistema do tenant (exercício de recuperação)", event_types: $e}')" >"$work/endpoint.json"
hook_secret=$(jq -r .secret "$work/endpoint.json")
[[ $hook_secret == whsec_* ]] || die "segredo do endpoint de webhook não devolvido"
product_id=$(api POST /api/v1/products '{"name":"Plataforma do exercício"}' | jq -r .id)
plan_id=$(api POST /api/v1/plans "$(jq -n --arg p "$product_id" \
  '{product_id: $p, code: "dr-mensal", name: "Plano do exercício", billing_interval: "month", trial_days: 0, price: {pricing_model: "flat", flat_amount: 12345}}')" | jq -r .id)
customer_a=$(api POST /api/v1/customers '{"kind":"company","name":"Cliente A do exercício","document_number":"11222333000181","email":"financeiro-a@cliente-dr.test"}' | jq -r .id)

# Convite criado na convivência das fases: a API cifra o token com dr2 e o
# worker, ainda na fase 1, o decifra para montar o link do e-mail.
session_call POST /api/v1/invitations '{"email":"convidada@acme-dr.test","role":"auditor"}' >/dev/null
mail_port=$(port_of mailpit 8025)
invitation_mailed() { [[ $(mail_count convidada@acme-dr.test "" 2>/dev/null) -ge 1 ]]; }
wait_until "e-mail do convite enviado pelo worker ainda na fase 1 da rotação" invitation_mailed

# O servidor de e-mail sai do ar: a notificação da fatura fica pendente, com
# o conteúdo cifrado no banco, e entra assim no backup.
dc stop mailpit >/dev/null 2>&1
sub_a=$(api POST /api/v1/subscriptions "$(jq -n --arg c "$customer_a" --arg p "$plan_id" '{customer_id: $c, plan_id: $p, quantity: 1}')" | jq -r .id)
wait_until "fatura da assinatura A emitida pelo worker" invoice_of_subscription "$sub_a" >/dev/null
invoice_a=$(invoice_of_subscription "$sub_a")
wait_until "cobrança da fatura A criada no provedor" charge_awaiting "$invoice_a" >/dev/null
provider_charge_a=$(charge_awaiting "$invoice_a")
wait_until "notificação da fatura A pendente depois da falha de envio" notification_is "$invoice_a" scheduled 1
wait_until "entregas de webhook da origem" sink_has charge.created 0
ok "fatura A emitida, cobrança $provider_charge_a criada no provedor com a chave de API cifrada, notificação pendente (conteúdo cifrado)"

# Tudo acima foi feito com a API e o worker em fases diferentes da rotação: o
# worker decifrou a chave de API do provedor e o segredo do endpoint, cifrados
# pela API com dr2, e nada foi encerrado por chave desconhecida.
dropped=$(sql "SELECT (SELECT count(*) FROM notifications WHERE status_reason IN ('content_unavailable', 'invitation_link_unavailable'))
                    + (SELECT count(*) FROM webhook_deliveries WHERE status = 'exhausted')")
[[ $dropped == 0 ]] || die "notificações ou entregas de webhook encerradas durante a rotação da chave ($dropped)"
[[ $(sql "SELECT count(*) FROM webhook_endpoint_secrets WHERE secret_key_id = 'dr2'") -ge 1 ]] || die "o segredo do endpoint deveria estar cifrado com dr2"
[[ $(sql "SELECT count(*) FROM payment_provider_accounts WHERE api_key_key_id = 'dr2'") == 1 ]] || die "a conta de provedor configurada pela API deveria estar cifrada com dr2"
# Fim da fase 2: o worker também passa a cifrar com dr2.
dc up -d --wait --no-deps worker
expect_keyring worker "dr1, dr2 (ativa: dr2)"
ok "rotação, fase 2 concluída: o worker da fase 1 decifrou os dados cifrados pela API com dr2 (token do convite, chave de API do provedor e segredo do endpoint), nada foi encerrado por chave desconhecida e o worker passou a dr2"

sink_requests >"$work/sink-origem.json"
origin_deliveries=$(jq length "$work/sink-origem.json")
for ((i = 0; i < origin_deliveries; i++)); do
  signature_ok "$work/sink-origem.json" "$i" || die "assinatura inválida numa entrega de webhook da origem"
done
ok "$origin_deliveries entregas de webhook da origem com assinatura válida para o segredo do endpoint"

# ---------- 3. Backup e guarda das chaves ----------

phase "3. Backup com trabalho pendente e chaves guardadas à parte"
# O worker para: o que a API gravar agora fica pendente na outbox e entra
# assim no backup (assinatura B sem fatura e eventos ainda não publicados).
dc stop worker >/dev/null 2>&1
customer_b=$(api POST /api/v1/customers '{"kind":"company","name":"Cliente B do exercício","document_number":"11444777000161","email":"financeiro-b@cliente-dr.test"}' | jq -r .id)
sub_b=$(api POST /api/v1/subscriptions "$(jq -n --arg c "$customer_b" --arg p "$plan_id" '{customer_id: $c, plan_id: $p, quantity: 1}')" | jq -r .id)
pending_events=$(sql "SELECT count(*) FROM outbox_events WHERE published_at IS NULL")
((pending_events >= 2)) || die "esperava eventos pendentes na outbox antes do backup (encontrados: $pending_events)"
sink_seq_backup=$(sink_requests | jq '[.[].seq] | max // 0')

backup_started=$(now_s)
backup_iso=$(now_iso)
dump=$(bash "$here/backup.sh")
backup_finished=$(now_s)
manifest=${dump%.dump}.manifest
ok "backup publicado: $(basename "$dump") ($(wc -c <"$dump") bytes; $(grep -c '^table ' "$manifest") tabelas; $pending_events eventos pendentes na outbox)"

# As chaves ficam no cofre, nunca no diretório do backup; o dump não contém
# as chaves nem os segredos em claro.
[[ $(stat -c %a "$keyring_file") == 600 ]] || die "o arquivo de chaves do cofre deveria ter permissão 0600"
if grep -r -l -F -e "$key1" -e "$key2" "$dr_dir" >/dev/null 2>&1; then
  die "chave de cifragem encontrada no diretório do backup"
fi
# O conteúdo do dump em SQL fica no cofre só durante a varredura. Os dados
# cifrados ficam em colunas bytea, que o pg_dump escreve em hexadecimal: cada
# segredo é procurado como texto e como bytes em hexadecimal, e as chaves e o
# segredo do endpoint também pelos bytes que o base64 representa.
docker exec -i "$PG" pg_restore -f - <"$dump" >"$work/dump.sql"
raw_hex() { printf %s "$1" | base64 -d | od -An -v -tx1 | tr -d ' \n'; }
leaked=$(count_secret_leaks "$key1" "$key2" "$(raw_hex "$key1")" "$(raw_hex "$key2")" "$provider_key" "$provider_secret" \
  "${hook_secret#whsec_}" "$(raw_hex "${hook_secret#whsec_}")" "$api_token" sim_beta_demonstracao_0001 <"$work/dump.sql")
[[ $leaked == 0 ]] || die "o dump contém chaves ou segredos em claro ($leaked linhas)"
# Controles positivos: a varredura lê as colunas de texto (um dado conhecido)
# e as colunas bytea em hexadecimal (o segredo cifrado do endpoint, lido do
# banco) e encontra o que procura quando está lá.
control_text=$(grep -c -F -e 'Cliente A do exercício' "$work/dump.sql" || true)
ciphertext_hex=$(sql "SELECT encode(secret_ciphertext, 'hex') FROM webhook_endpoint_secrets ORDER BY created_at LIMIT 1")
[[ ${#ciphertext_hex} -ge 56 ]] || die "ciphertext de controle não encontrado no banco"
control_bytea=$(grep -c -F -e "\\\\x$ciphertext_hex" "$work/dump.sql" || true)
control_leak=$(printf 'COPY public.controle (segredo) FROM stdin;\n\\\\x%s\n' "$(hex_of "$provider_key")" | count_secret_leaks "$provider_key")
((control_text >= 1)) || die "a varredura do dump não leu os dados (controle de texto ausente)"
((control_bytea >= 1)) || die "a varredura do dump não leu as colunas bytea em hexadecimal (controle ausente)"
[[ $control_leak == 1 ]] || die "a varredura não reconhece um segredo em claro numa coluna bytea"
rm -f "$work/dump.sql"
ok "chaves no cofre (0600), fora do diretório do backup; dump sem chaves nem segredos em claro, como texto ou como bytes em hexadecimal das colunas bytea (varredura com controles positivos de texto e de bytea)"

# ---------- 4. Janela perdida e desastre ----------

phase "4. Janela perdida (trabalho depois do backup) e desastre"
dc start worker >/dev/null 2>&1
wait_until "fatura da assinatura B emitida na janela" invoice_of_subscription "$sub_b" >/dev/null
lost_invoice_b=$(invoice_of_subscription "$sub_b")
wait_until "cobrança da fatura B criada no provedor na janela" charge_awaiting "$lost_invoice_b" >/dev/null
lost_provider_charge_b=$(charge_awaiting "$lost_invoice_b")
provider_pay "$provider_charge_a"
wait_until "pagamento da fatura A confirmado pelo webhook do provedor na janela" invoice_is "$invoice_a" paid
wait_until "entrega de webhook do pagamento na janela" sink_has invoice.paid "$sink_seq_backup"
wait_until "outbox da origem publicada" outbox_drained
manifest_of "$dr_dir/origem-no-desastre.manifest"
effects >"$dr_dir/origem-no-desastre.efeitos"
sink_requests >"$work/sink-janela.json"
sink_seq_disaster=$(jq '[.[].seq] | max // 0' "$work/sink-janela.json")
# Linhas por tabela no backup e na origem no instante do desastre: a
# diferença é o que a restauração não traz de volta.
awk 'NR == FNR { if ($1 == "table") backup[$2] = $3; next }
     $1 == "table" && $3 != backup[$2] { printf "%-28s %8d %8d %+8d\n", $2, backup[$2], $3, $3 - backup[$2] }' \
  "$manifest" "$dr_dir/origem-no-desastre.manifest" >"$dr_dir/perdido-na-janela.txt"
lost_rows=$(wc -l <"$dr_dir/perdido-na-janela.txt")
((lost_rows >= 1)) || die "a origem deveria ter avançado depois do backup"
ok "janela perdida: fatura B emitida e cobrança $lost_provider_charge_b criada no provedor; fatura A paga no provedor; $lost_rows tabelas com linhas além do backup"

disaster_at=$(now_s)
disaster_iso=$(now_iso)
log "desastre: removendo API, worker, PostgreSQL, Redis e RabbitMQ com os volumes"
dc rm -f -s -v api worker postgres redis rabbitmq >/dev/null 2>&1
for volume in postgres-data rabbitmq-data; do
  name=$(docker volume ls -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.volume=$volume")
  [[ -n $name ]] || die "volume $volume da origem não encontrado"
  docker volume rm "$name" >/dev/null
done
[[ -z $(docker volume ls -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.volume=postgres-data") ]] ||
  die "o volume do banco de origem ainda existe"
# Daqui em diante, as chaves só existem no cofre.
unset key1 key2
use_keys "" ""
ok "origem destruída: containers e volumes do banco e do broker removidos; chaves só no cofre"

# ---------- 5. Restauração num servidor novo ----------

phase "5. Restauração num servidor PostgreSQL novo"
# Redis e RabbitMQ não guardam nada que precise voltar (sessões e filas são
# refeitas) e o servidor de e-mail retorna: sobem em paralelo à restauração.
# Nenhum processo da aplicação é iniciado ainda.
dc up -d redis rabbitmq mailpit
dc up -d --wait postgres
resolve_pg
[[ $(psql_in postgres -c "SELECT count(*) FROM pg_database WHERE datname = 'recorvia'") == 0 ]] || die "o servidor novo deveria estar vazio"
dc run --rm -T --no-deps migrate-bootstrap >"$dr_dir/restauracao.log" 2>&1 || {
  cat "$dr_dir/restauracao.log" >&2
  die "bootstrap falhou"
}
(cd "$(dirname "$dump")" && sha256sum --quiet -c "$(basename "${dump%.dump}.sha256")") || die "soma SHA-256 divergente"
restore_started=$(now_s)
docker exec -i "$PG" pg_restore -U "$RECORVIA_PG_SUPERUSER" -d "$RECORVIA_PG_DATABASE" --exit-on-error --single-transaction <"$dump"
restore_finished=$(now_s)
dc run --rm -T --no-deps migrate >>"$dr_dir/restauracao.log" 2>&1 || {
  cat "$dr_dir/restauracao.log" >&2
  die "migrate up falhou depois da restauração"
}
bash "$here/restore-check.sh" "$dump"
manifest_of "$dr_dir/restaurado.manifest"
diff -u "$manifest" "$dr_dir/restaurado.manifest" >&2 || die "o banco restaurado difere do manifesto do backup"
ledger_balanced || die "ledger desbalanceado no banco restaurado"
cp "$manifest" "$dr_dir/backup.manifest"
verified_at=$(now_s)
ok "bootstrap, soma SHA-256, pg_restore, migrate up e restore-check; manifesto do banco restaurado idêntico ao do backup ($(grep -c '^table ' "$manifest") tabelas, $(awk '$1 == "table" { s += $3 } END { print s + 0 }' "$manifest") linhas); ledger balanceado"

# ---------- 6. Keyring: chave errada, chave ausente e chaves do cofre ----------

phase "6. Conferência do keyring antes de subir a aplicação"
vault_keys=$(sed -n 's/^RECORVIA_ENCRYPTION_KEYS=//p' "$keyring_file")
vault_active=$(sed -n 's/^RECORVIA_ENCRYPTION_ACTIVE_KEY=//p' "$keyring_file")
vault_key2=${vault_keys#*,}
wrong_key=$(openssl rand -base64 32)

# keyring_run ARQUIVO CHAVES ATIVA SERVIÇO [COMANDO]: execução avulsa do
# serviço com as chaves informadas (e, em $lost_keys, as declaradas
# perdidas); devolve o código de saída do processo.
lost_keys=""
keyring_run() {
  local out=$1 keys=$2 active=$3
  shift 3
  timeout 180 docker compose -p "$project" --env-file "$env_file" -f "$root/compose.yaml" -f "$root/deploy/backup/dr.compose.yaml" \
    run --rm -T --no-deps -e "RECORVIA_ENCRYPTION_KEYS=$keys" -e "RECORVIA_ENCRYPTION_ACTIVE_KEY=$active" \
    -e "RECORVIA_ENCRYPTION_LOST_KEYS=$lost_keys" "$@" >"$out" 2>&1
}
# expect_refusal ARQUIVO TRECHO DESCRIÇÃO CHAVES ATIVA SERVIÇO [COMANDO]
expect_refusal() {
  local out=$1 fragment=$2 what=$3 code=0
  shift 3
  keyring_run "$out" "$@" || code=$?
  if ((code == 0 || code == 124)); then
    cat "$out" >&2
    die "$what: o processo deveria ter terminado com erro (código $code)"
  fi
  grep -q -F -- "$fragment" "$out" || {
    cat "$out" >&2
    die "$what: erro sem a explicação esperada ($fragment)"
  }
}
expect_refusal "$dr_dir/keyring-chave-errada.txt" 'chave "dr1" com valor diferente do usado na cifragem' \
  "keyring-check com outro valor para dr1" "dr1:$wrong_key,$vault_key2" dr2 worker keyring-check
expect_refusal "$dr_dir/keyring-chave-ausente.txt" 'chave "dr1" ausente do keyring' \
  "keyring-check sem a chave dr1" "$vault_key2" dr2 worker keyring-check
expect_refusal "$dr_dir/worker-chave-errada.txt" 'A aplicação não foi iniciada' \
  "worker com outro valor para dr1" "dr1:$wrong_key,$vault_key2" dr2 worker
expect_refusal "$dr_dir/api-chave-ausente.txt" 'A aplicação não foi iniciada' \
  "API sem a chave dr1" "$vault_key2" dr2 api
# Chave perdida: sem dr1 no keyring, mas declarada perdida, a conferência
# aceita o banco e lista o que ficou ilegível (o caminho de quem não tem mais
# a chave; aqui só a conferência, sem subir a aplicação desse jeito).
lost_keys=dr1
keyring_run "$dr_dir/keyring-chave-perdida.txt" "$vault_key2" dr2 worker keyring-check || {
  cat "$dr_dir/keyring-chave-perdida.txt" >&2
  die "keyring-check deveria aceitar o banco com a chave dr1 declarada perdida"
}
lost_keys=""
grep -q -F 'declaradas perdidas: dr1' "$dr_dir/keyring-chave-perdida.txt" || die "keyring-check sem as chaves declaradas perdidas"
grep -q -F 'atenção: chave "dr1" declarada perdida' "$dr_dir/keyring-chave-perdida.txt" || die "keyring-check sem o aviso dos dados que dependem da chave perdida"
manifest_of "$work/depois-das-recusas.manifest"
diff -u "$manifest" "$work/depois-das-recusas.manifest" >&2 || die "as tentativas com chave errada alteraram o banco restaurado"
pending_notifications=$(sql "SELECT count(*) FROM notifications WHERE status = 'scheduled' AND content_ciphertext IS NOT NULL")
((pending_notifications >= 1)) || die "a notificação pendente deveria continuar agendada depois das recusas"
ok "chave errada e chave ausente recusadas pelo keyring-check, pelo worker e pela API, com a chave e a finalidade no erro; chave declarada perdida aceita pelo keyring-check, com o aviso do que depende dela; banco restaurado inalterado"

keyring_run "$dr_dir/keyring-cofre.txt" "$vault_keys" "$vault_active" worker keyring-check || {
  cat "$dr_dir/keyring-cofre.txt" >&2
  die "as chaves do cofre não decifram o banco restaurado"
}
grep -q -F 'keyring compatível com o banco' "$dr_dir/keyring-cofre.txt" || die "keyring-check sem a confirmação esperada"
use_keys "$vault_keys" "$vault_active"
keyring_done_at=$(now_s)
ok "chaves do cofre conferidas contra o banco restaurado (keyring-check)"

# ---------- 7. API sobre o banco restaurado e conciliação da janela ----------

phase "7. API no ar (worker parado) e conciliação da janela perdida"
dc up -d --wait redis api
refresh_ports
wait_until "readiness da API" api_ready
api_ready_at=$(now_s)
[[ $(jq -r '.checks.postgres' "$work/readyz") == ok && $(jq -r '.checks.redis' "$work/readyz") == ok ]] || die "readiness da API incompleta"
[[ $(curl -s -o /dev/null -w '%{http_code}' "${connect[@]}" -b "$work/cookies" "$api_url/api/v1/session") == 401 ]] ||
  die "a sessão anterior ao desastre deveria ter sido perdida com o Redis"
hint=$(api GET /api/v1/payment-providers | jq -r '.data[] | select(.provider == "simulator") | .api_key_hint')
[[ $hint == "${provider_key: -4}" ]] || die "conta de provedor restaurada inesperada"
login
ok "readiness da API (PostgreSQL e Redis); credencial de API anterior ao backup aceita; sessões perdidas com o Redis e novo login concluído"

# Inventário do banco restaurado (consultas do guia de recuperação).
{
  echo "eventos pendentes na outbox (serão publicados ao religar o worker):"
  sql "SELECT event_type || ' ' || count(*) FROM outbox_events WHERE published_at IS NULL GROUP BY event_type ORDER BY event_type"
  echo "cobranças vinculadas ao provedor no backup:"
  sql "SELECT c.provider_charge_id || ' ' || c.status || ' fatura=' || i.status FROM charges c JOIN invoices i ON i.tenant_id = c.tenant_id AND i.id = c.invoice_id WHERE c.provider_charge_id IS NOT NULL ORDER BY c.created_at"
  echo "cobranças alteradas no provedor desde o backup ($backup_iso):"
  provider GET "/v1/charges?updated_from=$backup_iso" | jq -r '.data[] | "\(.id) \(.status) referencia=\(.external_reference)"'
} >"$dr_dir/inventario-da-janela.txt"
restored_invoices_b=$(api GET "/api/v1/invoices?subscription_id=$sub_b" | jq '.data | length')
[[ $restored_invoices_b == 0 ]] || die "a fatura B não deveria existir no banco restaurado"
invoice_is "$invoice_a" open || die "a fatura A deveria estar em aberto no banco restaurado"

# Conciliação pedida pela API, com o worker parado: o provedor é a fonte do
# que aconteceu na janela. O pedido só registra a execução (ADR-071); quem a
# executa é o subcomando reconcile-pending, que termina sem iniciar o
# agendador, os consumidores nem o publicador da outbox.
api POST /api/v1/reconciliation/runs >"$work/conciliacao.json"
[[ $(jq -r .status "$work/conciliacao.json") == running ]] || die "pedido de conciliação não registrado"
run_id=$(jq -r .id "$work/conciliacao.json")
timeout 900 docker compose -p "$project" --env-file "$env_file" -f "$root/compose.yaml" -f "$root/deploy/backup/dr.compose.yaml" \
  run --rm -T --no-deps worker reconcile-pending >"$dr_dir/conciliacao-da-janela.txt" 2>&1 || {
  cat "$dr_dir/conciliacao-da-janela.txt" >&2
  die "reconcile-pending não concluiu a conciliação da janela"
}
api GET "/api/v1/reconciliation/runs/$run_id" >"$work/conciliacao.json"
[[ $(jq -r .status "$work/conciliacao.json") == completed ]] || die "conciliação não concluída"
api GET "/api/v1/reconciliation/discrepancies?status=open" >"$work/divergencias.json"
paid_discrepancy=$(jq -r --arg c "$provider_charge_a" '.data[] | select(.kind == "payment_not_reflected" and .provider_charge_id == $c) | .id' "$work/divergencias.json")
orphan_discrepancy=$(jq -r --arg c "$lost_provider_charge_b" '.data[] | select(.kind == "missing_local" and .provider_charge_id == $c) | .id' "$work/divergencias.json")
[[ -n $paid_discrepancy ]] || die "a conciliação não apontou o pagamento da fatura A recebido na janela"
[[ -n $orphan_discrepancy ]] || die "a conciliação não apontou a cobrança criada na janela sem registro local"
# Pagamento recebido na janela: adota o provedor (dinheiro recebido não é descartado).
api POST "/api/v1/reconciliation/discrepancies/$paid_discrepancy/resolve" \
  '{"resolution":"accept_provider","justification":"Pagamento recebido no provedor durante a janela perdida na recuperação de desastre."}' >/dev/null
# Cobrança criada na janela, não paga e sem registro local: cancelada no
# provedor, porque a fatura será emitida de novo ao religar o worker.
provider DELETE "/v1/charges/$lost_provider_charge_b" >/dev/null
api POST "/api/v1/reconciliation/discrepancies/$orphan_discrepancy/resolve" \
  '{"resolution":"dismiss","justification":"Cobrança da janela perdida cancelada no provedor; a fatura será emitida de novo."}' >/dev/null
invoice_is "$invoice_a" paid || die "a fatura A deveria estar paga depois da conciliação"
[[ $(api GET "/api/v1/reconciliation/discrepancies?status=open" | jq '.data | length') == 0 ]] || die "restaram divergências abertas"
reconciled_at=$(now_s)
ok "janela conciliada antes de religar o worker: pagamento da fatura A adotado do provedor; cobrança $lost_provider_charge_b cancelada no provedor; nenhuma divergência aberta"

# ---------- 8. Worker religado ----------

phase "8. Worker religado sobre o banco restaurado"
dc up -d --wait mailpit rabbitmq worker
mail_port=$(port_of mailpit 8025)
refresh_ports
worker_port=$(port_of worker 9091)
wait_until "readiness do worker" worker_ready
worker_ready_at=$(now_s)
[[ $(jq -r '.checks.postgres' "$work/readyz-worker") == ok && $(jq -r '.checks.rabbitmq' "$work/readyz-worker") == ok ]] || die "readiness do worker incompleta"
api_ready || die "readiness da API depois de religar o worker"
ok "readiness do worker (PostgreSQL e RabbitMQ) e da API"

# Faturamento da janela: a assinatura B recebe uma fatura e uma cobrança.
wait_until "fatura da assinatura B emitida de novo" invoice_of_subscription "$sub_b" >/dev/null
invoice_b=$(invoice_of_subscription "$sub_b")
[[ $invoice_b != "$lost_invoice_b" ]] || die "a fatura B deveria ter novo identificador"
wait_until "cobrança da fatura B criada no provedor" charge_awaiting "$invoice_b" >/dev/null
provider_charge_b=$(charge_awaiting "$invoice_b")
wait_until "outbox do banco restaurado publicada" outbox_drained
[[ $(api GET "/api/v1/invoices?subscription_id=$sub_b" | jq '.data | length') == 1 ]] || die "a assinatura B deveria ter uma única fatura"
invoice_number_b=$(api GET "/api/v1/invoices/$invoice_b" | jq -r .number)
provider GET /v1/charges >"$work/provedor.json"
open_for_b=$(jq --arg d "Fatura $invoice_number_b" '[.data[] | select(.description == $d and .status != "canceled")] | length' "$work/provedor.json")
[[ $open_for_b == 1 ]] || die "o provedor deveria ter uma única cobrança aberta da fatura $invoice_number_b (encontradas: $open_for_b)"
[[ $(jq --arg c "$lost_provider_charge_b" -r '.data[] | select(.id == $c) | .status' "$work/provedor.json") == canceled ]] || die "a cobrança da janela deveria estar cancelada no provedor"
ok "faturamento retomado sem duplicar: uma fatura ($invoice_number_b) e uma cobrança aberta no provedor ($provider_charge_b) para a assinatura B"

# Notificação pendente no backup: o conteúdo, cifrado pelo worker antes do
# backup, é decifrado e o e-mail é enviado uma vez.
wait_until "notificação da fatura A enviada" notification_is "$invoice_a" sent 1
invoice_number_a=$(api GET "/api/v1/invoices/$invoice_a" | jq -r .number)
[[ $(mail_count financeiro-a@cliente-dr.test "Fatura $invoice_number_a emitida") == 1 ]] || die "o e-mail da fatura A deveria ter chegado uma única vez"
ok "conteúdo de notificação cifrado antes do backup decifrado: e-mail da fatura $invoice_number_a entregue uma vez"

# Entregas de webhook depois da restauração, assinadas com o segredo
# restaurado (cifrado com dr2) e conferidas com o segredo que o tenant guardou.
wait_until "entrega de webhook do pagamento conciliado" sink_has invoice.paid "$sink_seq_disaster"
wait_until "entrega de webhook da fatura emitida de novo" sink_has charge.created "$sink_seq_disaster"
sink_requests >"$work/sink-final.json"
total_deliveries=$(jq length "$work/sink-final.json")
restored_deliveries=0
for ((i = 0; i < total_deliveries; i++)); do
  signature_ok "$work/sink-final.json" "$i" || die "assinatura inválida numa entrega de webhook (sequência $(jq ".[$i].seq" "$work/sink-final.json"))"
  if (($(jq ".[$i].seq" "$work/sink-final.json") > sink_seq_disaster)); then
    restored_deliveries=$((restored_deliveries + 1))
  fi
done
((restored_deliveries >= 3)) || die "poucas entregas depois da restauração: $restored_deliveries"
# Eventos pendentes no backup foram entregues nas duas linhas do tempo com o
# mesmo webhook-id (o destinatário que deduplica ignora a repetição).
repeated=$(jq --argjson b "$sink_seq_backup" --argjson d "$sink_seq_disaster" \
  '[.[] | select(.seq > $b and .seq <= $d) | .headers["webhook-id"]] as $janela | [.[] | select(.seq > $d) | .headers["webhook-id"]] as $depois
   | [$janela[] | select(. as $id | any($depois[]; . == $id))] | length' "$work/sink-final.json")
((repeated >= 1)) || die "esperava eventos pendentes no backup entregues de novo com o mesmo webhook-id"
ok "$restored_deliveries entregas de webhook depois da restauração com assinatura válida para o segredo anterior ao backup; $repeated eventos pendentes no backup repetidos com o mesmo webhook-id"

# Credenciais de provedor decifradas: o webhook do provedor é autenticado com
# o segredo da conta (acme: dr2; beta: dr1, do seed).
provider_pay "$provider_charge_b"
wait_until "pagamento da fatura B confirmado pelo webhook do provedor" invoice_is "$invoice_b" paid
beta_charge=$(sql "SELECT c.provider_charge_id FROM charges c JOIN tenants t ON t.id = c.tenant_id WHERE t.slug = 'beta' AND c.status = 'awaiting_payment'")
[[ -n $beta_charge ]] || die "cobrança da organização beta não encontrada"
provider_pay "$beta_charge"
wait_until "pagamento da organização beta confirmado (credenciais cifradas com a chave antiga)" beta_invoice_paid
ok "webhooks do provedor autenticados com os segredos restaurados das duas organizações (chaves dr2 e dr1): faturas pagas"

# Nada se repete: efeitos financeiros estáveis depois de mais dois ciclos dos
# agendamentos de faturamento e de recuperação de operações.
wait_until "outbox publicada" outbox_drained
ledger_balanced || die "ledger desbalanceado depois da retomada"
[[ $(api GET "/api/v1/invoices?subscription_id=$sub_a" | jq '.data | length') == 1 ]] || die "a assinatura A deveria ter uma única fatura"
[[ $(api GET "/api/v1/invoices?subscription_id=$sub_b" | jq '.data | length') == 1 ]] || die "a assinatura B deveria ter uma única fatura"
[[ $(sql "SELECT count(*) FROM (SELECT 1 FROM charges WHERE status IN ('pending', 'awaiting_payment') GROUP BY tenant_id, invoice_id HAVING count(*) > 1) x") == 0 ]] ||
  die "mais de uma cobrança em aberto para a mesma fatura"
[[ $(sql "SELECT count(*) FROM charges WHERE status <> 'canceled'") == $(sql "SELECT count(*) FROM invoices") ]] ||
  die "cada fatura deveria ter exatamente uma cobrança não cancelada"
[[ $(sql "SELECT count(*) FROM (SELECT 1 FROM payments WHERE charge_id IS NOT NULL GROUP BY tenant_id, charge_id HAVING count(*) > 1) x") == 0 ]] ||
  die "pagamento duplicado para a mesma cobrança"
[[ $(sql "SELECT count(*) FROM dead_letters") == 0 ]] || die "mensagens em dead letter depois da retomada"
[[ $(jq '[.data[] | select(.status != "canceled") | .external_reference] | group_by(.) | map(select(length > 1)) | length' "$work/provedor.json") == 0 ]] ||
  die "o provedor tem mais de uma cobrança ativa para a mesma referência"
effects >"$dr_dir/restaurado-final.efeitos"
runs_before=$(sql "SELECT min(run_count) FROM scheduled_jobs WHERE name IN ('billing.run_cycle', 'payments.recover_operations')")
provider_charges_before=$(provider GET /v1/charges | jq '.data | length')
wait_until "mais dois ciclos de faturamento e de recuperação de operações" jobs_ran_since "$runs_before"
effects >"$work/efeitos-depois.txt"
diff -u "$dr_dir/restaurado-final.efeitos" "$work/efeitos-depois.txt" >&2 || die "os efeitos financeiros mudaram em ciclos posteriores (duplicação)"
[[ $(provider GET /v1/charges | jq '.data | length') == "$provider_charges_before" ]] || die "novas cobranças criadas no provedor em ciclos posteriores"
ledger_balanced || die "ledger desbalanceado depois dos ciclos posteriores"
done_at=$(now_s)
ok "sem efeitos duplicados: faturas, cobranças, pagamentos e lançamentos iguais depois de mais dois ciclos; ledger balanceado; nenhuma dead letter"
open_after=$(api GET "/api/v1/reconciliation/discrepancies?status=open" | jq -r '[.data[].kind] | join(", ")')
note "Divergências de conciliação abertas ao final: ${open_after:-nenhuma}."

# ---------- Relatório ----------

phase "Relatório"
revision=$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo desconhecida)
if [[ -n $(git -C "$root" status --porcelain 2>/dev/null) ]]; then
  revision="$revision com alterações locais"
fi
{
  echo "# Exercício de recuperação de desastre"
  echo
  echo "Executado em $(now_iso), revisão $revision, docker compose $compose_version, projeto \`$project\`."
  echo
  echo "## Medições (observadas neste exercício; não são garantia)"
  echo
  echo "| Medida | Valor |"
  echo "|---|---|"
  echo "| Instante do backup | $backup_iso |"
  echo "| Instante do desastre | $disaster_iso |"
  echo "| Janela perdida (RPO observado: do backup ao desastre) | $((disaster_at - backup_started)) s |"
  echo "| Duração do backup (dump e manifesto) | $((backup_finished - backup_started)) s |"
  echo "| Tamanho do dump | $(wc -c <"$dump") bytes |"
  echo "| \`pg_restore\` | $((restore_finished - restore_started)) s |"
  echo "| Etapa: servidor novo, bootstrap, \`pg_restore\`, \`migrate up\`, \`restore-check\` e manifesto | $((verified_at - disaster_at)) s |"
  echo "| Etapa: conferência do keyring (quatro recusas, chave declarada perdida e as chaves do cofre) | $((keyring_done_at - verified_at)) s |"
  echo "| Etapa: API no ar e pronta | $((api_ready_at - keyring_done_at)) s |"
  echo "| Etapa: conciliação da janela pela API | $((reconciled_at - api_ready_at)) s |"
  echo "| Etapa: worker no ar e pronto | $((worker_ready_at - reconciled_at)) s |"
  echo "| Do desastre à API pronta sobre o banco restaurado | $((api_ready_at - disaster_at)) s |"
  echo "| Do desastre ao worker pronto, com a janela conciliada (RTO observado) | $((worker_ready_at - disaster_at)) s |"
  echo "| Do desastre ao fim das conferências (inclui a espera de dois ciclos dos agendamentos) | $((done_at - disaster_at)) s |"
  echo
  echo "## Perdido na janela (linhas por tabela)"
  echo
  echo '```text'
  printf '%-28s %8s %8s %8s\n' tabela backup desastre perdidas
  cat "$dr_dir/perdido-na-janela.txt"
  echo '```'
  echo
  echo "## Conferências"
  echo
  cat "$checks_file"
  echo
  echo "## Observações"
  echo
  cat "$notes_file"
  echo
  echo "## Efeitos financeiros (origem no desastre e banco restaurado ao final)"
  echo
  echo '```text'
  echo "origem no desastre:"
  cat "$dr_dir/origem-no-desastre.efeitos"
  echo "restaurado ao final:"
  cat "$dr_dir/restaurado-final.efeitos"
  echo '```'
} >"$dr_dir/relatorio.md"
# O dump (dados de demonstração) só fica para inspeção com RECORVIA_DR_KEEP=1.
if [[ ${RECORVIA_DR_KEEP:-0} != 1 ]]; then
  rm -f "$dump"
fi
cat "$dr_dir/relatorio.md" >&2
finished=1
log "exercício de recuperação de desastre concluído: relatório em $dr_dir/relatorio.md"
