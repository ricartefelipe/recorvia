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
#   3. /livez da API pela origem pública e /readyz só pela rede interna;
#   4. a interface (index.html e um asset) e o endereço do cliente visto pelo
#      nginx: o do cliente, não o do proxy, também em IPv6, e nunca o de um
#      X-Forwarded-For enviado pelo cliente ou por outro container;
#   5. o login: /auth/login redireciona ao Keycloak com o redirect_uri do
#      domínio, e o Keycloak aceita e mostra o formulário;
#   6. problem+json numa rota inexistente da API;
#   7. as portas: nenhum container publica outra porta, /metrics e a saúde do
#      Keycloak não passam pelo proxy, e o domínio do Keycloak só repassa o
#      realm recorvia e os recursos estáticos: console, API de administração,
#      realm master e variantes de caminho (;, .., codificados) recebem 403
#      com KEYCLOAK_ADMIN_ALLOWED_CIDRS no padrão (fechado);
#   8. os realms: recorvia sem usuários, com verificação de e-mail, política
#      de senha, proteção contra força bruta e cliente com os endereços do
#      domínio e o segredo do .env; master com proteção contra força bruta;
#   9. o backup do banco do Keycloak (pg_dump legível por pg_restore);
#  10. o backup contínuo (ADR-087), com o MinIO de verificacao.compose.yaml
#      como S3 de teste: PostgreSQL com a imagem recorvia-postgres e o
#      arquivamento de WAL funcionando (segmento forçado chega ao
#      armazenamento, sem falhas), agente postgres-backup saudável, com o
#      backup base cifrado com a chave do arquivo e as métricas alcançáveis
#      pela rede do Prometheus, e as redes (data sem saída; storage só para o
#      PostgreSQL e o agente).
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
# Exige docker com o plugin compose 2.24 ou superior e suporte a redes IPv6,
# curl e jq, e as portas 80 e 443 de 127.0.0.1 livres. A chave de cifragem do
# backup é montada como arquivo: fica numa pasta temporária dentro de
# RECORVIA_PROD_CHECK_SECRETS_DIR (padrão: ~/.cache), porque o Docker Desktop
# só compartilha a pasta pessoal com a máquina virtual. Usa a imagem
# curlimages/curl como cliente dentro da rede edge (IPv6 e X-Forwarded-For
# forjado).
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
curl_image=curlimages/curl:8.17.0
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

# O ambiente de quem chama não entra na verificação: as variáveis do Compose e
# todas as que o compose.yaml interpola valem só pelo .env de teste (o
# ambiente do shell teria precedência sobre ele).
unset COMPOSE_PROFILES COMPOSE_FILE COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES COMPOSE_PATH_SEPARATOR OTEL_EXPORTER_OTLP_ENDPOINT
while IFS= read -r var; do
  unset "$var"
done < <({
  grep -oE '\$\{[A-Za-z_][A-Za-z0-9_]*' compose.yaml | cut -c3-
  compgen -e | grep -E '^(RECORVIA_|KEYCLOAK_|SMTP_|ALERTMANAGER_|GRAFANA_|PROMETHEUS_|JAEGER_|RABBITMQ_|REDIS_|POSTGRES_|ACME_|RUNBOOK_|OTEL_)'
} | sort -u)

work=$(mktemp -d)
env_file=$work/.env
secrets_base=${RECORVIA_PROD_CHECK_SECRETS_DIR:-$HOME/.cache}
mkdir -p "$secrets_base"
secrets_dir=$(mktemp -d "$secrets_base/recorvia-prodcheck-segredos.XXXXXX")
compose() { docker compose -p "$project" -f compose.yaml -f verificacao.compose.yaml --env-file "$env_file" "$@"; }

teardown() {
  local status=$?
  if [[ $status -ne 0 ]]; then
    log "falha: estado e logs do projeto $project"
    compose ps -a >&2 || true
    compose logs --no-color --tail 80 >&2 || true
  fi
  if [[ $keep == 1 ]]; then
    log "ambiente mantido (RECORVIA_PROD_CHECK_KEEP=1): projeto $project, configuração em $work e chave do backup em $secrets_dir"
  else
    compose down -v --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$work" "$secrets_dir"
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
# A chave do backup contínuo é criada por gerar-segredos.sh no caminho do .env.
backup_key_file=$secrets_dir/segredos/backup-cifragem.chave
set_env RECORVIA_BACKUP_ENCRYPTION_KEY_HOST_FILE "$backup_key_file"
bash gerar-segredos.sh "$env_file" 2>/dev/null
[[ -f $backup_key_file && $(stat -c %a "$backup_key_file") == 444 && $(stat -c %a "$(dirname "$backup_key_file")") == 700 ]] ||
  die "gerar-segredos.sh não criou a chave do backup (0444 numa pasta 0700)"
# S3 de teste: o MinIO da verificação, com credencial gerada aqui.
set_env RECORVIA_BACKUP_S3_PREFIX s3://recorvia-backup/prodcheck
set_env RECORVIA_BACKUP_S3_ENDPOINT http://minio:9000
set_env RECORVIA_BACKUP_S3_FORCE_PATH_STYLE true
set_env RECORVIA_BACKUP_S3_ACCESS_KEY_ID "prodcheck$(bash gerar-segredos.sh --senha | cut -c1-8)"
set_env RECORVIA_BACKUP_S3_SECRET_ACCESS_KEY "$(bash gerar-segredos.sh --senha)"
set_env RECORVIA_VERSION "$tag"
set_env RECORVIA_DOMAIN "$app"
set_env KEYCLOAK_DOMAIN "$auth"
set_env ACME_EMAIL operacao@example.com
# KEYCLOAK_ADMIN_ALLOWED_CIDRS fica como no .env.example (ausente): o padrão
# do compose precisa manter o console fechado.
grep -q '^KEYCLOAK_ADMIN_ALLOWED_CIDRS=' "$env_file" && die "o .env.example não deve abrir o console do Keycloak por padrão"
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
[[ $(jq -r .services.postgres.image <<<"$base") == recorvia-postgres:* ]] || die "o PostgreSQL deveria usar a imagem recorvia-postgres"
jq -e '.services["postgres-backup"].read_only == true and (.services["postgres-backup"].volumes[0].read_only == true)
  and ([.services.postgres, .services["postgres-backup"]] | all(.secrets[0].target == "/run/secrets/backup-encryption-key"))
  and ([.services.postgres, .services["postgres-backup"]] | all(.environment.RECORVIA_BACKUP_ENCRYPTION_KEY == ""))
  and .networks.data.internal == true and (.networks.storage.internal // false) == false' <<<"$base" >/dev/null ||
  die "backup contínuo mal configurado (agente somente leitura, chave por arquivo, redes data e storage)"
storage_users=$(jq -r '[.services | to_entries[] | select(.value.networks | has("storage")) | .key] | sort | join(" ")' <<<"$base")
[[ $storage_users == "postgres postgres-backup" ]] || die "serviços na rede storage: $storage_users (esperado só postgres e postgres-backup)"
obs=$(docker compose -p "$project" -f compose.yaml --env-file "$env_file" --profile observability config --format json)
jq -e '(.services.prometheus.networks | keys) as $p | (.services["postgres-backup"].networks | keys) as $b | any($p[]; . as $n | $b | index($n))' \
  <<<"$obs" >/dev/null || die "o Prometheus não compartilha rede com o agente de backup (coleta recorvia-backup)"
ok "backup contínuo: imagem recorvia-postgres, chave por arquivo (fora do ambiente), agente somente leitura, rede storage só para o PostgreSQL e o agente, Prometheus na rede do agente"

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

log "3. /livez e /readyz"
body=$(c -f "https://$app/livez") || die "/livez não respondeu 200"
jq -e '.status == "ok"' <<<"$body" >/dev/null || die "/livez inesperado: $body"
ok "/livez pela origem pública: $body"
body=$(c "https://$app/readyz")
! grep -q '"checks"' <<<"$body" || die "/readyz exposto pela origem pública"
ok "/readyz fora da origem pública"
body=$(compose exec -T web wget -q -O - http://api:8081/readyz) || die "/readyz não respondeu 200 na rede interna"
jq -e '.status == "ok" and .checks.postgres == "ok" and .checks.redis == "ok"' <<<"$body" >/dev/null ||
  die "/readyz inesperado: $body"
ok "/readyz pela rede interna: $body"

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
# próprio proxy (limites por IP da API, X-Real-IP), e ignora o
# X-Forwarded-For de qualquer outro remetente.
proxy_id=$(compose ps -q proxy)
proxy_ips=" $(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{.GlobalIPv6Address}} {{end}}' "$proxy_id") "
edge=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}' "$proxy_id")
# logged_client CAMINHO: endereço registrado pelo nginx no último acesso.
logged_client() {
  compose logs --no-log-prefix web | grep -F "\"GET $1 " | tail -n 1 | awk '{ print $1 }'
}
c -o /dev/null -H 'X-Forwarded-For: 203.0.113.7' "https://$app/clientes"
client=$(logged_client /clientes)
[[ -n $client ]] || die "acesso à interface ausente do log do nginx"
[[ $proxy_ips != *" $client "* ]] || die "o nginx vê o endereço do proxy ($client) em vez do cliente"
[[ $client != 203.0.113.7 ]] || die "o X-Forwarded-For enviado pelo cliente alterou o endereço registrado"
ok "o nginx vê o endereço do cliente ($client), não o do proxy nem o X-Forwarded-For forjado"
[[ $(docker network inspect -f '{{.EnableIPv6}}' "$edge") == true ]] || die "rede $edge sem IPv6"
proxy_ip6=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$edge\").GlobalIPv6Address}}" "$proxy_id")
[[ -n $proxy_ip6 ]] || die "proxy sem endereço IPv6 na rede $edge"
# Cliente IPv6 na rede edge, falando com o proxy pelo IPv6: o nginx precisa
# registrar o IPv6 do cliente.
probe=$project-probe
docker run --rm --name "$probe" --network "$edge" "$curl_image" -sk -o /dev/null --max-time 20 \
  --resolve "$app:443:[$proxy_ip6]" "https://$app/clientes-ipv6" ||
  die "cliente IPv6 não alcançou o proxy em $proxy_ip6"
client6=$(logged_client /clientes-ipv6)
[[ $client6 == *:* && $proxy_ips != *" $client6 "* ]] || die "por IPv6, o nginx registrou $client6 (proxy: $proxy_ip6)"
ok "por IPv6, o nginx vê o endereço do cliente ($client6), não o do proxy ($proxy_ip6)"
# Outro container da rede edge, direto no nginx com X-Forwarded-For forjado.
docker run --rm --name "$probe" --network "$edge" "$curl_image" -s -o /dev/null --max-time 20 \
  -H 'X-Forwarded-For: 203.0.113.8' http://web:8080/clientes-direto || die "o container de teste não alcançou o nginx"
direct=$(logged_client /clientes-direto)
[[ -n $direct && $direct != 203.0.113.8 ]] || die "o nginx aceitou o X-Forwarded-For de um remetente que não é o proxy ($direct)"
ok "X-Forwarded-For de outro remetente ignorado (registrado $direct)"

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
for path in /admin /admin/ /admin/master/console/ "/admin;x" "/admin;x/master/console/" /realms/master \
  /realms/master/ /realms/master/protocol/openid-connect/token /realms/master/.well-known/openid-configuration \
  "/realms/recorvia/../master/" "/realms/recorvia/%2e%2e/master/" "/realms/recorvia/..%2fmaster/" \
  "/realms/recorvia;x/../master/" "/realms/recorvia/%252e%252e/master/" "/realms/Master/" "/REALMS/master/" /js/keycloak.js /welcome /; do
  code=$(c --path-as-is -o /dev/null -w '%{http_code}' "https://$auth$path")
  [[ $code == 403 ]] || die "$path no domínio do Keycloak respondeu $code (esperado 403)"
done
code=$(c --path-as-is -o /dev/null -w '%{http_code}' -X POST -d grant_type=password -d client_id=admin-cli \
  -d username=x -d password=y "https://$auth/realms/master/protocol/openid-connect/token")
[[ $code == 403 ]] || die "token do realm master respondeu $code pelo proxy"
ok "console, API de administração, realm master e variantes de caminho recusados (403) com o padrão fechado"
for path in /realms/recorvia /realms/recorvia/.well-known/openid-configuration; do
  code=$(c -o /dev/null -w '%{http_code}' "https://$auth$path")
  [[ $code == 200 ]] || die "$path no domínio do Keycloak respondeu $code (esperado 200)"
done
# O Keycloak 26 não tem robots.txt (404): basta o proxy repassar.
code=$(c -o /dev/null -w '%{http_code}' "https://$auth/robots.txt")
[[ $code != 403 ]] || die "/robots.txt recusado pelo proxy"
resource=$(grep -oE '/resources/[^"]+\.css' <<<"$page" | head -n 1)
[[ -n $resource ]] || die "página de login sem recurso em /resources"
code=$(c -o /dev/null -w '%{http_code}' "https://$auth$resource")
[[ $code == 200 ]] || die "$resource respondeu $code"
ok "realm recorvia, robots.txt e recursos estáticos do login liberados"

log "8. realms"
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
master=$(kcadm get realms/master)
jq -e '.bruteForceProtected == true and .failureFactor == 5 and (.passwordPolicy | contains("length(12)"))' <<<"$master" >/dev/null ||
  die "realm master sem proteção: $(jq -c '{bruteForceProtected, failureFactor, passwordPolicy}' <<<"$master")"
actions=$(kcadm get authentication/required-actions -r master | jq -r '[.[] | select(.enabled) | .alias] | sort | join(" ")')
[[ " $actions " == *" CONFIGURE_TOTP "* && " $actions " == *" UPDATE_PASSWORD "* && " $actions " != *" VERIFY_PROFILE "* ]] ||
  die "ações obrigatórias inesperadas no realm master: $actions"
admins=$(kcadm get users -r master --fields username | jq -r '[.[].username] | join(" ")')
[[ $admins == "$(get_env KEYCLOAK_ADMIN_USER)" ]] || die "usuários inesperados no realm master: $admins"
ok "realm master com proteção contra força bruta, política de senha e segundo fator disponível; só o administrador temporário"

log "9. backup do Keycloak"
compose exec -T postgres pg_dump -U postgres -Fc keycloak >"$work/keycloak.dump"
# A lista vai para um arquivo antes do grep: com pipefail, o grep -q que sai
# na primeira linha encontrada interromperia o pg_restore (SIGPIPE).
compose exec -T postgres pg_restore --list <"$work/keycloak.dump" >"$work/keycloak.list" ||
  die "pg_restore não leu o dump do banco do Keycloak"
grep -q 'TABLE public user_entity' "$work/keycloak.list" || die "dump do banco do Keycloak sem a tabela de usuários"
ok "pg_dump do banco keycloak legível ($(stat -c %s "$work/keycloak.dump") bytes)"

log "10. backup contínuo"
psql_su() { compose exec -T postgres psql -X -q -A -t -v ON_ERROR_STOP=1 -U postgres -d postgres -c "$1"; }
for svc in postgres postgres-backup; do
  [[ $(compose exec -T "$svc" id -un) == postgres ]] || die "o serviço $svc deveria rodar como o usuário postgres, sem root"
done
[[ $(psql_su "SHOW archive_mode") == on ]] || die "archive_mode deveria estar ligado"
segment=$(psql_su "SELECT pg_walfile_name(pg_current_wal_lsn())")
psql_su "SELECT pg_switch_wal()" >/dev/null
archived=""
for _ in $(seq 1 60); do
  archived=$(psql_su "SELECT count(*) FROM pg_stat_archiver WHERE last_archived_wal >= '$segment'")
  [[ $archived == 1 ]] && break
  sleep 2
done
[[ $archived == 1 ]] || die "o segmento $segment não chegou ao armazenamento: $(psql_su "SELECT row_to_json(a) FROM pg_stat_archiver a")"
failed=$(psql_su "SELECT failed_count FROM pg_stat_archiver")
[[ $failed == 0 ]] || die "archive_command com $failed falhas"
ok "PostgreSQL e agente sem root (usuário postgres); arquivamento de WAL no S3 de teste: segmento $segment arquivado, nenhuma falha"
health=$(docker inspect -f '{{.State.Health.Status}}' "$(compose ps -q postgres-backup)")
[[ $health == healthy ]] || die "agente de backup com saúde $health"
listing=""
for _ in $(seq 1 60); do
  listing=$(compose exec -T postgres-backup recorvia-pgbackup list 2>/dev/null || true)
  grep -q '^base_.*esta chave$' <<<"$listing" && break
  sleep 2
done
grep -q '^base_.*esta chave$' <<<"$listing" || die "nenhum backup base cifrado com a chave do arquivo: $listing"
ok "agente de backup saudável; backup base no armazenamento, cifrado com a chave do arquivo ($(grep -c '^base_' <<<"$listing") backup)"
# A coleta recorvia-backup do Prometheus chega ao agente pelo nome do serviço
# na rede data: outro container dessa rede lê as métricas.
metrics=$(compose exec -T redis wget -q -O - http://postgres-backup:9187/metrics) || die "métricas do agente fora do alcance da rede data"
grep -q '^recorvia_backup_dependency_up{dependency="storage",service="pgbackup"} 1' <<<"$metrics" || die "agente sem acesso ao armazenamento"
grep -q '^recorvia_backup_dependency_up{dependency="postgres",service="pgbackup"} 1' <<<"$metrics" || die "agente sem acesso ao PostgreSQL"
grep -q '^recorvia_backup_wal_last_archived_timestamp_seconds' <<<"$metrics" || die "métricas do arquivamento ausentes"
ok "métricas do agente em postgres-backup:9187 pela rede data (armazenamento e PostgreSQL disponíveis)"
# Sem saída da rede data: o Redis (só na data) não alcança o MinIO.
if compose exec -T redis wget -q -T 5 -O /dev/null http://minio:9000/minio/health/live 2>/dev/null; then
  die "a rede data alcança o armazenamento"
fi
ok "o armazenamento só é alcançável pela rede storage"

log "verificação do pacote de produção concluída"
