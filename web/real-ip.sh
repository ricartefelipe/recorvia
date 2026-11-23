#!/bin/sh
# Partida do nginx da interface (executado pelo docker-entrypoint.sh da imagem,
# em /docker-entrypoint.d). Gera /tmp/recorvia/real-ip.conf, incluído pelo
# nginx.conf, a partir de RECORVIA_TRUSTED_PROXIES: endereços ou CIDRs,
# separados por vírgula, dos proxies cujo X-Forwarded-For vale como endereço
# do cliente (na produção, o Caddy nos endereços fixos da rede edge,
# deploy/producao). Sem a variável (ambiente local), o arquivo fica vazio e o
# X-Forwarded-For enviado pelo cliente não altera o endereço registrado nem o
# X-Real-IP repassado à API.
set -eu

dir=/tmp/recorvia
out=$dir/real-ip.conf
mkdir -p "$dir"
: >"$out"

[ -n "${RECORVIA_TRUSTED_PROXIES:-}" ] || exit 0
for entry in $(printf '%s' "$RECORVIA_TRUSTED_PROXIES" | tr ',' ' '); do
  case $entry in
    *[!0-9A-Fa-f.:/]* | "")
      echo "real-ip.sh: RECORVIA_TRUSTED_PROXIES com valor inválido: $entry" >&2
      exit 1
      ;;
  esac
  printf 'set_real_ip_from %s;\n' "$entry" >>"$out"
done
# Vale o último endereço da lista, o que o proxy confiável acrescentou (o
# Caddy descarta o X-Forwarded-For recebido do cliente).
printf 'real_ip_header X-Forwarded-For;\nreal_ip_recursive off;\n' >>"$out"
