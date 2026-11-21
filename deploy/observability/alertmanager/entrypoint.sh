#!/bin/sh
# Partida do Alertmanager do Recorvia (ADR-065). Gera os receptores e as
# definições de SMTP a partir das variáveis de ambiente, acrescenta-os ao
# roteamento fixo da imagem (/etc/alertmanager/alertmanager.yml), valida o
# resultado com amtool check-config e inicia o Alertmanager. Os segredos
# (senha do SMTP, URL e token do webhook) são gravados em arquivos de acesso
# restrito no diretório de execução e referenciados pela configuração; nada
# disso existe na imagem.
#
# Uso:
#   entrypoint.sh [argumentos extras do alertmanager]   inicia o serviço
#   entrypoint.sh check-config                           gera e valida, sem iniciar
#   entrypoint.sh route-test rótulo=valor...             gera, valida e informa o receptor
#                                                        de um alerta com esses rótulos
#
# Variáveis (todas opcionais; sem e-mail nem webhook, os alertas ficam
# visíveis apenas na interface):
#   ALERTMANAGER_EXTERNAL_URL          endereço público da interface (links das notificações)
#   ALERTMANAGER_SMTP_SMARTHOST        servidor SMTP (host:porta)
#   ALERTMANAGER_SMTP_FROM             remetente (padrão alertas@recorvia.local)
#   ALERTMANAGER_SMTP_REQUIRE_TLS      true (padrão) ou false
#   ALERTMANAGER_SMTP_USERNAME         usuário do SMTP (exige a senha)
#   ALERTMANAGER_SMTP_PASSWORD         senha do SMTP (exige o usuário e
#                                      ALERTMANAGER_SMTP_REQUIRE_TLS=true)
#   ALERTMANAGER_EMAIL_TO              destinatários (separados por vírgula)
#   ALERTMANAGER_EMAIL_TO_CRITICAL     destinatários dos críticos (padrão: ALERTMANAGER_EMAIL_TO)
#   ALERTMANAGER_WEBHOOK_URL           endpoint do webhook genérico (POST no formato do Alertmanager)
#   ALERTMANAGER_WEBHOOK_BEARER_TOKEN  token enviado em Authorization: Bearer
#   ALERTMANAGER_CLUSTER_LISTEN_ADDRESS endereço do cluster de alta disponibilidade
#                                      (padrão vazio: instância única, sem cluster)
set -eu

base=/etc/alertmanager/alertmanager.yml
runtime=/etc/alertmanager/runtime

die() {
  echo "alertmanager: $*" >&2
  exit 1
}

# single_line NOME VALOR: recusa quebras de linha e caracteres de controle,
# que alterariam a estrutura do YAML gerado.
single_line() {
  if [ "$(printf '%s' "$2" | tr -d '[:cntrl:]')" != "$2" ]; then
    die "$1 não pode conter quebras de linha nem caracteres de controle"
  fi
}

# plain NOME VALOR: valores copiados para campos interpretados como modelo
# (remetente e destinatários) não podem conter ações de modelo.
plain() {
  single_line "$1" "$2"
  case $2 in
    *'{{'*) die "$1 não pode conter {{" ;;
  esac
}

# yaml_string VALOR: escalar YAML entre aspas simples.
yaml_string() {
  printf "'%s'" "$(printf '%s' "$1" | sed "s/'/''/g")"
}

# secret_file ARQUIVO VALOR: grava o segredo com acesso restrito ao usuário
# do processo.
secret_file() {
  (umask 077 && printf '%s' "$2" >"$1")
}

smarthost=${ALERTMANAGER_SMTP_SMARTHOST:-}
from=${ALERTMANAGER_SMTP_FROM:-alertas@recorvia.local}
require_tls=${ALERTMANAGER_SMTP_REQUIRE_TLS:-true}
smtp_user=${ALERTMANAGER_SMTP_USERNAME:-}
smtp_password=${ALERTMANAGER_SMTP_PASSWORD:-}
email_to=${ALERTMANAGER_EMAIL_TO:-}
email_critical=${ALERTMANAGER_EMAIL_TO_CRITICAL:-$email_to}
webhook_url=${ALERTMANAGER_WEBHOOK_URL:-}
webhook_token=${ALERTMANAGER_WEBHOOK_BEARER_TOKEN:-}
external_url=${ALERTMANAGER_EXTERNAL_URL:-http://localhost:9093}
cluster_address=${ALERTMANAGER_CLUSTER_LISTEN_ADDRESS:-}

single_line ALERTMANAGER_SMTP_SMARTHOST "$smarthost"
single_line ALERTMANAGER_SMTP_USERNAME "$smtp_user"
single_line ALERTMANAGER_SMTP_PASSWORD "$smtp_password"
single_line ALERTMANAGER_WEBHOOK_URL "$webhook_url"
single_line ALERTMANAGER_WEBHOOK_BEARER_TOKEN "$webhook_token"
single_line ALERTMANAGER_EXTERNAL_URL "$external_url"
plain ALERTMANAGER_SMTP_FROM "$from"
plain ALERTMANAGER_EMAIL_TO "$email_to"
plain ALERTMANAGER_EMAIL_TO_CRITICAL "$email_critical"

case $require_tls in
  true | false) ;;
  *) die "ALERTMANAGER_SMTP_REQUIRE_TLS deve ser true ou false" ;;
esac
if [ -n "$email_to" ] && [ -z "$smarthost" ]; then
  die "ALERTMANAGER_EMAIL_TO exige ALERTMANAGER_SMTP_SMARTHOST"
fi
if [ -z "$email_to" ] && [ -n "${ALERTMANAGER_EMAIL_TO_CRITICAL:-}" ]; then
  die "ALERTMANAGER_EMAIL_TO_CRITICAL exige ALERTMANAGER_EMAIL_TO"
fi
if [ -n "$smtp_password" ] && [ -z "$smtp_user" ]; then
  die "ALERTMANAGER_SMTP_PASSWORD exige ALERTMANAGER_SMTP_USERNAME"
fi
# Usuário sem senha passaria na validação da configuração e falharia em todo
# envio a um servidor que anuncia autenticação.
if [ -n "$smtp_user" ] && [ -z "$smtp_password" ]; then
  die "ALERTMANAGER_SMTP_USERNAME exige ALERTMANAGER_SMTP_PASSWORD"
fi
# Sem TLS exigido, o Alertmanager não negocia STARTTLS e a senha trafegaria
# em claro.
if [ -n "$smtp_password" ] && [ "$require_tls" = false ]; then
  die "ALERTMANAGER_SMTP_PASSWORD exige ALERTMANAGER_SMTP_REQUIRE_TLS=true: a senha do SMTP não é enviada por conexão sem TLS"
fi
if [ -n "$webhook_token" ] && [ -z "$webhook_url" ]; then
  die "ALERTMANAGER_WEBHOOK_BEARER_TOKEN exige ALERTMANAGER_WEBHOOK_URL"
fi
case $webhook_url in
  '' | http://* | https://*) ;;
  *) die "ALERTMANAGER_WEBHOOK_URL deve começar com http:// ou https://" ;;
esac
case $external_url in
  http://* | https://*) ;;
  *) die "ALERTMANAGER_EXTERNAL_URL deve começar com http:// ou https://" ;;
esac

mode=run
case ${1:-} in
  check-config | route-test)
    mode=$1
    shift
    runtime=$(mktemp -d)
    trap 'rm -rf "$runtime"' EXIT
    ;;
  *)
    rm -rf "$runtime"
    mkdir -p "$runtime"
    ;;
esac
chmod 700 "$runtime"
config=$runtime/alertmanager.yml

# email_config DESTINATÁRIOS RESOLVIDOS: integração de e-mail de um receptor.
email_config() {
  [ -n "$1" ] || return 0
  cat <<YAML
    email_configs:
      - to: $(yaml_string "$1")
        send_resolved: $2
        html: '{{ template "recorvia.email.html" . }}'
        text: '{{ template "recorvia.email.text" . }}'
        headers:
          Subject: '{{ template "recorvia.email.subject" . }}'
YAML
}

# webhook_config: integração de webhook genérico de um receptor.
webhook_config() {
  [ -n "$webhook_url" ] || return 0
  cat <<YAML
    webhook_configs:
      - url_file: $runtime/webhook_url
        send_resolved: true
        timeout: 10s
YAML
  [ -n "$webhook_token" ] || return 0
  cat <<YAML
        http_config:
          authorization:
            type: Bearer
            credentials_file: $runtime/webhook_token
YAML
}

{
  cat "$base"
  echo
  echo "# Gerado na partida do container a partir das variáveis ALERTMANAGER_*."
  echo "global:"
  echo "  resolve_timeout: 5m"
  if [ -n "$email_to" ]; then
    echo "  smtp_smarthost: $(yaml_string "$smarthost")"
    echo "  smtp_from: $(yaml_string "$from")"
    echo "  smtp_require_tls: $require_tls"
    if [ -n "$smtp_user" ]; then
      echo "  smtp_auth_username: $(yaml_string "$smtp_user")"
    fi
    if [ -n "$smtp_password" ]; then
      echo "  smtp_auth_password_file: $runtime/smtp_password"
    fi
  fi
  echo "receivers:"
  echo "  - name: recorvia-critical"
  email_config "$email_critical" true
  webhook_config
  echo "  - name: recorvia-warning"
  email_config "$email_to" true
  webhook_config
  echo "  - name: recorvia-watchdog"
  email_config "$email_to" false
} >"$config"

if [ -n "$smtp_password" ] && [ -n "$email_to" ]; then
  secret_file "$runtime/smtp_password" "$smtp_password"
fi
if [ -n "$webhook_url" ]; then
  secret_file "$runtime/webhook_url" "$webhook_url"
fi
if [ -n "$webhook_token" ]; then
  secret_file "$runtime/webhook_token" "$webhook_token"
fi

if ! out=$(amtool check-config "$config" 2>&1); then
  echo "$out" >&2
  die "configuração gerada inválida"
fi

receivers=""
[ -z "$email_to" ] || receivers="e-mail"
[ -z "$webhook_url" ] || receivers="${receivers:+$receivers e }webhook"
if [ -z "$receivers" ]; then
  echo "alertmanager: nenhum receptor configurado (ALERTMANAGER_EMAIL_TO, ALERTMANAGER_WEBHOOK_URL): os alertas ficam visíveis apenas na interface" >&2
else
  echo "alertmanager: receptores configurados: $receivers" >&2
fi

case $mode in
  check-config)
    echo "$out"
    exit 0
    ;;
  route-test)
    amtool config routes test --config.file="$config" "$@"
    exit $?
    ;;
esac

exec /bin/alertmanager --config.file="$config" --storage.path=/alertmanager --web.external-url="$external_url" \
  --cluster.listen-address="$cluster_address" "$@"
