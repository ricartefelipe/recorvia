#!/usr/bin/env bash
# Verificação automatizada do pacote de produção (ADR-086).
# Uso: make prod-check (ou bash deploy/producao/verificar.sh).
#
# Sobe deploy/producao/compose.yaml num projeto isolado do compose, com os
# domínios de teste localhost (aplicação) e auth.localhost (Keycloak), que
# recebem certificado da autoridade interna do Caddy, segredos gerados por
# gerar-segredos.sh num diretório temporário e o proxy publicado só em
# 127.0.0.1 (verificacao.compose.yaml). Confere:
#   1. a configuração: só o proxy publica portas (80 e 443), as interfaces do
#      perfil observability ficam no loopback, nenhum serviço de demonstração,
#      e todo serviço contínuo tem healthcheck, restart e limites de CPU e
#      memória;
#   2. HTTPS com certificado válido para a autoridade do proxy, HSTS e
#      redirecionamento de HTTP para HTTPS;
#   3. /readyz da API pela origem pública;
#   4. a interface (index.html e um asset) e o endereço do cliente visto pelo
#      nginx (o do cliente, não o do proxy);
#   5. o login: /auth/login redireciona ao Keycloak com o redirect_uri do
#      domínio, e o Keycloak aceita e mostra o formulário;
#   6. problem+json numa rota inexistente da API;
#   7. as portas: nenhum container publica outra porta, /metrics e a saúde do
#      Keycloak não passam pelo proxy e o console de administração do
#      Keycloak é recusado fora de KEYCLOAK_ADMIN_ALLOWED_CIDRS;
#   8. o realm de produção: sem usuários, verificação de e-mail, política de
#      senha, proteção contra força bruta, cliente com os endereços do
#      domínio e o segredo do .env;
#   9. o backup do banco do Keycloak (pg_dump legível por pg_restore).
#
# Variáveis (todas opcionais):
#   RECORVIA_PROD_CHECK_PROJECT  projeto do compose (padrão:
#                                recorvia-prodcheck), derrubado com os volumes
#                                no início e no fim; nunca recorvia nem
#                                recorvia-producao, e recusado se tiver
#                                containers ou volumes sem o rótulo da
#                                verificação
#   RECORVIA_PROD_CHECK_TAG      etiqueta das imagens (padrão: prodcheck)
#   RECORVIA_PROD_CHECK_BUILD    0 reutiliza as imagens já construídas
#                                (padrão: 1)
#   RECORVIA_PROD_CHECK_KEEP     1 mantém o ambiente no fim, para inspeção
#   RECORVIA_PROD_CHECK_TIMEOUT  espera máxima, em segundos, da subida
#                                (padrão: 600)
#
# Exige docker com o plugin compose 2.24 ou superior, curl e jq, e as portas
# 80 e 443 de 127.0.0.1 livres.
set -euo pipefail
umask 077
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cd "$here"

project=${RECORVIA_PROD_CHECK_PROJECT:-recorvia-prodcheck}
tag=${RECORVIA_PROD_CHECK_TAG:-prodcheck}
build=${RECORVIA_PROD_CHECK_BUILD:-1}
keep=${RECORVIA_PROD_CHECK_KEEP:-0}
timeout_s=${RECORVIA_PROD_CHECK_TIMEOUT:-600}
label=recorvia.prod-check
app=localhost
auth=auth.localhost

die() {
  echo "erro: $*" >&2
  exit 1
}
log() { echo "==> $*" >&2; }
ok() { echo "    ok: $*" >&2; }

for cmd in docker curl jq; do
  command -v "$cmd" >/dev/null || die "$cmd não encontrado"
done
case $project in
  recorvia | recorvia-producao) die "RECORVIA_PROD_CHECK_PROJECT não pode ser $project (ambiente local ou produção)" ;;
  "" | *[!a-z0-9_-]*) die "RECORVIA_PROD_CHECK_PROJECT inválido: $project" ;;
esac

work=$(mktemp -d)
env_file=$work/.env
compose() { docker compose -p "$project" -f compose.yaml -f verificacao.compose.yaml --env-file "$env_file" "$@"; }

teardown() {
  local status=$?
  if [[ $status -ne 0 ]]; then
    log "falha: estado e logs do projeto $project"
    compose ps -a >&2 || true
    compose logs --no-color --tail 80 >&2 || true
  fi
  if [[ $keep == 1 ]]; then
    log "ambiente mantido (RECORVIA_PROD_CHECK_KEEP=1): projeto $project, configuração em $work"
  else
    compose down -v --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$work"
  fi
  exit "$status"
}

# require_own_project: recusa remover um projeto com containers ou volumes
# que a verificação não criou (sem o rótulo).
require_own_project() {
  local kind all own
  for kind in containers volumes; do
    if [[ $kind == containers ]]; then
      all=$(docker ps -a -q --filter "label=com.docker.compose.project=$project")
      own=$(docker ps -a -q --filter "label=com.docker.compose.project=$project" --filter "label=$label=1")
    else
      all=$(docker volume ls -q --filter "label=com.docker.compose.project=$project")
      own=$(docker volume ls -q --filter "label=com.docker.compose.project=$project" --filter "label=$label=1")
    fi
    [[ $(sort <<<"$all") == "$(sort <<<"$own")" ]] ||
      die "o projeto $project já tem $kind sem o rótulo $label: nada foi removido; use outro RECORVIA_PROD_CHECK_PROJECT"
  done
}

# set_env NOME VALOR: troca (ou acrescenta) a variável no .env da verificação.
set_env() {
  local tmp=$env_file.tmp
  awk -v k="$1" -v v="$2" 'BEGIN { done = 0 }
    index($0, k "=") == 1 { print k "=" v; done = 1; next }
    { print }
    END { if (!done) print k "=" v }' "$env_file" >"$tmp"
  mv "$tmp" "$env_file"
}
get_env() { sed -n "s/^$1=//p" "$env_file" | tail -n 1; }

log "configuração de teste em $work (domínios $app e $auth)"
cp .env.example "$env_file"
bash gerar-segredos.sh "$env_file" 2>/dev/null
set_env RECORVIA_VERSION "$tag"
set_env RECORVIA_DOMAIN "$app"
set_env KEYCLOAK_DOMAIN "$auth"
set_env ACME_EMAIL operacao@example.com
# Endereço de documentação (TEST-NET-1): o console de administração do
# Keycloak precisa ser recusado para a máquina que verifica.
set_env KEYCLOAK_ADMIN_ALLOWED_CIDRS 192.0.2.0/24
# O worker só valida a configuração do SMTP na partida; nenhum e-mail é
# enviado na verificação.
set_env SMTP_HOST smtp.example.com
set_env SMTP_USERNAME verificacao
set_env SMTP_PASSWORD "$(bash gerar-segredos.sh --senha)"
grep -q '=GERAR$' "$env_file" && die "gerar-segredos.sh deixou valores sem gerar"
ok "segredos gerados por gerar-segredos.sh (permissão $(stat -c %a "$env_file"))"

log "1. configuração do compose"
config=$(compose config --format json)
base=$(docker compose -p "$project" -f compose.yaml --env-file "$env_file" config --format json)
published=$(jq -r '.services | to_entries[] | select(.value.ports) | .key' <<<"$base" | sort -u | tr '\n' ' ')
[[ $published == "proxy " ]] || die "serviços com portas publicadas além do proxy: $published"
targets=$(jq -r '[.services.proxy.ports[] | "\(.target)/\(.protocol // "tcp")"] | sort | join(" ")' <<<"$base")
[[ $targets == "443/tcp 443/udp 80/tcp" ]] || die "portas do proxy: $targets (esperado 80/tcp, 443/tcp e 443/udp)"
ok "só o proxy publica portas: $targets"
loose=$(docker compose -p "$project" -f compose.yaml --env-file "$env_file" --profile observability config --format json |
  jq -r '.services | to_entries[] | select(.key != "proxy") | .value.ports[]? | select(.host_ip != "127.0.0.1") | "\(.published)"')
[[ -z $loose ]] || die "portas do perfil observability fora do loopback: $loose"
ok "interfaces do perfil observability só em 127.0.0.1"
for svc in paysim mailpit seed; do
  jq -e --arg s "$svc" '.services | has($s) | not' <<<"$config" >/dev/null || die "serviço de demonstração presente: $svc"
done
demo=$(jq -r '[.services[].environment // {} | keys[] | select(test("PAYSIM|DEMO_STAFF|WEBHOOK_ALLOWED_PRIVATE"))] | unique | join(" ")' <<<"$config")
[[ -z $demo ]] || die "variáveis de demonstração no compose: $demo"
ok "sem simulador de pagamentos, Mailpit, seed nem variáveis de demonstração"
incomplete=$(docker compose -p "$project" -f compose.yaml --env-file "$env_file" --profile observability config --format json |
  jq -r '.services | to_entries[] | select(.value.restart != "no")
    | select((.value.restart != "unless-stopped") or (.value.deploy.resources.limits.memory == null)
        or (.value.deploy.resources.limits.cpus == null)
        or ((.value.healthcheck == null) and (.key != "otel-collector"))) | .key')
[[ -z $incomplete ]] || die "serviços sem restart, limites ou healthcheck: $incomplete"
ok "serviços contínuos com restart, healthcheck e limites de CPU e memória"

log "subida do projeto $project (etiqueta $tag)"
require_own_project
compose down -v --remove-orphans >/dev/null 2>&1 || true
trap teardown EXIT
for port in 80 443; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    die "a porta $port de 127.0.0.1 está em uso: libere-a para a verificação"
  fi
done
up_args=(up -d --wait --wait-timeout "$timeout_s")
if [[ $build == 1 ]]; then
  up_args+=(--build)
fi
start=$(date +%s)
compose "${up_args[@]}"
ok "ambiente no ar em $(($(date +%s) - start)) s"

ca=$work/ca.crt
for _ in $(seq 1 30); do
  if compose cp proxy:/data/caddy/pki/authorities/local/root.crt "$ca" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
[[ -s $ca ]] || die "certificado raiz da autoridade interna do Caddy não encontrado"
c() {
  curl -sS --max-time 20 --cacert "$ca" \
    --resolve "$app:443:127.0.0.1" --resolve "$app:80:127.0.0.1" --resolve "$auth:443:127.0.0.1" "$@"
}

log "2. HTTPS"
for host in $app $auth; do
  for _ in $(seq 1 30); do
    if c -o /dev/null "https://$host/" 2>/dev/null; then
      break
    fi
    sleep 2
  done
  c -o /dev/null --tlsv1.2 "https://$host/" || die "HTTPS em $host não é válido para a autoridade do proxy"
done
ok "certificados de $app e $auth válidos para a autoridade do proxy"
headers=$(c -D - -o /dev/null "http://$app/")
if ! { grep -qiE '^HTTP/[0-9.]+ 30[18]' <<<"$headers" && grep -qi "^location: https://$app/" <<<"$headers"; }; then
  die "HTTP não redireciona para HTTPS: $(head -n 1 <<<"$headers")"
fi
ok "HTTP redireciona para HTTPS"
headers=$(c -D - -o /dev/null "https://$app/")
grep -qi '^strict-transport-security: max-age=' <<<"$headers" || die "resposta HTTPS sem HSTS"
ok "HSTS presente"

log "3. /readyz"
body=$(c -f "https://$app/readyz") || die "/readyz não respondeu 200"
jq -e '.status == "ok" and .checks.postgres == "ok" and .checks.redis == "ok"' <<<"$body" >/dev/null ||
  die "/readyz inesperado: $body"
ok "/readyz: $body"

log "4. interface"
index=$(c -f "https://$app/") || die "a interface não carregou"
grep -q 'id="root"' <<<"$index" || die "index.html sem o elemento raiz da SPA"
asset=$(grep -oE '/assets/[^"]+\.js' <<<"$index" | head -n 1)
[[ -n $asset ]] || die "index.html sem script em /assets"
ctype=$(c -f -o /dev/null -w '%{content_type}' "https://$app$asset") || die "asset $asset não carregou"
[[ $ctype == *javascript* ]] || die "asset $asset com Content-Type $ctype"
headers=$(c -D - -o /dev/null "https://$app/clientes")
grep -qi "^content-security-policy: default-src 'self'" <<<"$headers" || die "interface sem Content-Security-Policy"
ok "index.html, $asset e rota da SPA com CSP"
# O nginx registra o endereço do cliente que o proxy informou, não o do
# próprio proxy (limites por IP da API, X-Real-IP).
proxy_ips=" $(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "$(compose ps -q proxy)") "
client=$(compose logs --no-log-prefix web | grep -F '"GET /clientes ' | tail -n 1 | awk '{ print $1 }')
[[ -n $client ]] || die "acesso à interface ausente do log do nginx"
[[ $proxy_ips != *" $client "* ]] || die "o nginx vê o endereço do proxy ($client) em vez do cliente"
ok "o nginx vê o endereço do cliente ($client), não o do proxy"

log "5. login"
jar=$work/cookies
# A descoberta OIDC da API é preguiçosa e repetida depois de falha (ADR-021).
for _ in $(seq 1 30); do
  location=$(c -c "$jar" -o /dev/null -w '%{http_code} %{redirect_url}' "https://$app/auth/login")
  [[ ${location%% *} == 503 ]] || break
  sleep 2
done
code=${location%% *}
location=${location#* }
[[ $code == 302 ]] || die "/auth/login respondeu $code"
[[ $location == "https://$auth/realms/recorvia/protocol/openid-connect/auth?"* ]] ||
  die "/auth/login não redireciona ao Keycloak: $location"
for want in "client_id=recorvia-web" "redirect_uri=https%3A%2F%2F$app%2Fauth%2Fcallback" "code_challenge_method=S256"; do
  [[ $location == *"$want"* ]] || die "redirecionamento sem $want: $location"
done
ok "/auth/login redireciona a https://$auth/realms/recorvia (PKCE S256, redirect_uri do domínio)"
page=$(c -f -b "$jar" -c "$jar" "$location") || die "o Keycloak recusou a autorização"
grep -q 'kc-form-login' <<<"$page" || die "o Keycloak não mostrou o formulário de login"
ok "o Keycloak aceita o redirect_uri e mostra o formulário de login"
issuer=$(c -f "https://$auth/realms/recorvia/.well-known/openid-configuration" | jq -r .issuer)
[[ $issuer == "https://$auth/realms/recorvia" ]] || die "emissor inesperado: $issuer"
ok "emissor $issuer"

log "6. problem+json"
resp=$(c -D "$work/h404" "https://$app/api/v1/rota-inexistente")
grep -qiE '^HTTP/[0-9.]+ 404' "$work/h404" || die "rota inexistente: $(head -n 1 "$work/h404")"
grep -qi '^content-type: application/problem+json' "$work/h404" || die "rota inexistente sem application/problem+json"
jq -e '.code == "route_not_found" and (.request_id | length > 0)' <<<"$resp" >/dev/null || die "corpo inesperado: $resp"
ok "404 application/problem+json com code e request_id"

log "7. portas"
ports=$(docker ps --filter "label=com.docker.compose.project=$project" --format '{{.Label "com.docker.compose.service"}} {{.Ports}}' |
  awk '$2 != "" { $1 = $1; print }' | grep -- '->' || true)
others=$(awk '$1 != "proxy"' <<<"$ports")
[[ -z $others ]] || die "containers com portas publicadas além do proxy: $others"
proxy_ports=$(grep -oE '127\.0\.0\.1:[0-9]+->[0-9]+/(tcp|udp)' <<<"$ports" | sort -u | tr '\n' ' ')
[[ $proxy_ports == "127.0.0.1:443->443/tcp 127.0.0.1:443->443/udp 127.0.0.1:80->80/tcp " ]] ||
  die "portas publicadas inesperadas: $ports"
ok "containers em execução: só o proxy publica, e só 80 e 443 ($proxy_ports)"
metrics=$(c "https://$app/metrics")
! grep -q '^# TYPE ' <<<"$metrics" || die "/metrics exposto pela origem pública"
for path in /metrics /health/ready; do
  code=$(c -o "$work/kc" -w '%{http_code}' "https://$auth$path")
  ! grep -qE '^# TYPE |"status": *"UP"' "$work/kc" || die "$path do Keycloak exposto pelo proxy ($code)"
done
ok "métricas da API e saúde e métricas do Keycloak fora do proxy"
code=$(c -o /dev/null -w '%{http_code}' "https://$auth/admin/master/console/")
[[ $code == 403 ]] || die "console de administração do Keycloak respondeu $code fora dos endereços permitidos"
ok "console de administração do Keycloak recusado fora de KEYCLOAK_ADMIN_ALLOWED_CIDRS (403)"

log "8. realm de produção"
kcadm() { compose exec -T keycloak /opt/keycloak/bin/kcadm.sh "$@" --config /tmp/kcadm.config; }
kcadm config credentials --server http://localhost:8080 --realm master \
  --user "$(get_env KEYCLOAK_ADMIN_USER)" --password "$(get_env KEYCLOAK_ADMIN_PASSWORD)" >/dev/null 2>&1 ||
  die "administrador temporário do Keycloak não autenticou"
ok "administrador temporário criado pela variável KEYCLOAK_ADMIN_USER"
realm=$(kcadm get realms/recorvia)
jq -e '.verifyEmail == true and .bruteForceProtected == true and .registrationAllowed == false
  and (.passwordPolicy | contains("length(12)")) and .sslRequired == "external"' <<<"$realm" >/dev/null ||
  die "realm sem as proteções de produção: $(jq -c '{verifyEmail, bruteForceProtected, registrationAllowed, passwordPolicy, sslRequired}' <<<"$realm")"
[[ $(jq -r .smtpServer.host <<<"$realm") == "$(get_env SMTP_HOST)" ]] || die "SMTP do realm não veio da configuração"
ok "verificação de e-mail, política de senha, força bruta e SMTP da configuração"
users=$(kcadm get users -r recorvia --fields username | jq length)
[[ $users == 0 ]] || die "o realm de produção tem $users usuário(s)"
ok "realm sem usuários"
client=$(kcadm get clients -r recorvia -q clientId=recorvia-web | jq '.[0]')
jq -e --arg a "https://$app" '.redirectUris == [$a + "/auth/callback"] and .webOrigins == [$a] and .publicClient == false' <<<"$client" >/dev/null ||
  die "cliente recorvia-web com endereços inesperados: $(jq -c '{redirectUris, webOrigins}' <<<"$client")"
secret=$(kcadm get "clients/$(jq -r .id <<<"$client")/client-secret" -r recorvia | jq -r .value)
[[ $secret == "$(get_env RECORVIA_OIDC_CLIENT_SECRET)" ]] || die "segredo do cliente diferente de RECORVIA_OIDC_CLIENT_SECRET"
ok "cliente recorvia-web com redirect_uri e origem do domínio e o segredo do .env"

log "9. backup do Keycloak"
compose exec -T postgres pg_dump -U postgres -Fc keycloak >"$work/keycloak.dump"
compose exec -T postgres pg_restore --list <"$work/keycloak.dump" | grep -q 'TABLE public user_entity' ||
  die "dump do banco do Keycloak sem a tabela de usuários"
ok "pg_dump do banco keycloak legível ($(stat -c %s "$work/keycloak.dump") bytes)"

log "verificação do pacote de produção concluída"
