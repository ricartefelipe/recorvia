#!/usr/bin/env bash
# Exportação do realm do Keycloak para o destino do backup contínuo (ADR-087).
# Uso: make keycloak-export (ou bash deploy/backup/keycloak-export.sh).
#
# Complementa o backup do banco do Keycloak (que, no mesmo servidor
# PostgreSQL, já está no backup base e no WAL): uma cópia lógica do realm,
# com usuários e credenciais, útil para inspeção, recuperação seletiva ou
# migração entre versões do Keycloak. Executa kc.sh export num container
# avulso do serviço do Keycloak (mesma configuração e mesmo banco do servidor
# em execução) e envia o arquivo pelo agente de backup, que o compacta e o
# cifra com a chave do backup (recorvia-pgbackup put-object). O arquivo
# contém hashes de senha e segredos de clientes: nada fica no host.
#
# Usa o docker compose do diretório atual ou o definido pelas variáveis do
# compose (COMPOSE_PROJECT_NAME, COMPOSE_FILE, COMPOSE_ENV_FILES). Exige o
# Keycloak com banco externo (KC_DB=postgres, por exemplo o banco keycloak no
# mesmo servidor PostgreSQL) e jq.
# Variáveis (opcionais):
#   RECORVIA_KEYCLOAK_SERVICE  serviço do Keycloak (padrão: keycloak)
#   RECORVIA_KEYCLOAK_REALM    realm exportado (padrão: recorvia)
#   RECORVIA_PGBACKUP_SERVICE  serviço do agente de backup (padrão: postgres-backup)
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/backup/common.sh
source "$here/common.sh"

kc_service=${RECORVIA_KEYCLOAK_SERVICE:-keycloak}
realm=${RECORVIA_KEYCLOAK_REALM:-recorvia}
agent_service=${RECORVIA_PGBACKUP_SERVICE:-postgres-backup}
[[ $realm =~ ^[a-z0-9][a-z0-9_-]*$ ]] || die "nome de realm inválido: $realm"
command -v jq >/dev/null || die "jq não encontrado"
dc() { docker compose "$@"; }
[[ -n $(dc ps -q --status running "$agent_service" 2>/dev/null) ]] || die "o agente de backup ($agent_service) precisa estar em execução"
[[ -n $(dc ps -q --status running "$kc_service" 2>/dev/null) ]] || die "o Keycloak ($kc_service) precisa estar em execução"
# A exportação roda num container avulso, que só enxerga os dados do servidor
# em execução se eles estiverem num banco externo. Com o banco embutido do
# modo de desenvolvimento (dev-file, H2 dentro do container, como no compose
# local), o container avulso teria um banco vazio só dele.
# shellcheck disable=SC2016 # expandido no container
kc_db=$(dc exec -T "$kc_service" sh -c 'echo "${KC_DB:-}"' 2>/dev/null || true)
case $kc_db in
  postgres | mysql | mariadb | mssql | oracle) ;;
  *)
    die "o Keycloak ($kc_service) usa o banco embutido (KC_DB=${kc_db:-não definido}): a exportação exige um banco externo (KC_DB=postgres, ADR-087); nada foi exportado"
    ;;
esac
object="keycloak/realm-$realm-$(date -u +%Y%m%dT%H%M%SZ).json"

log "exportando o realm $realm (kc.sh export num container avulso do serviço $kc_service, banco $kc_db)"
# A saída do kc.sh vai para a saída de erro; o arquivo exportado fica numa
# variável (em memória, nunca no disco do host) e só segue para o agente,
# que o compacta e cifra, depois de conferido.
realm_json=$(dc run --rm --no-deps -T --entrypoint sh "$kc_service" -c \
  "/opt/keycloak/bin/kc.sh export --realm '$realm' --users same_file --file /tmp/realm.json >&2 && cat /tmp/realm.json") ||
  die "kc.sh export falhou; nada foi enviado"
users=$(jq -er --arg r "$realm" 'select(.realm == $r) | .users | length' <<<"$realm_json") ||
  die "a exportação não trouxe o realm $realm; nada foi enviado"
printf '%s' "$realm_json" | dc exec -T "$agent_service" recorvia-pgbackup put-object "$object"
unset realm_json
log "realm $realm exportado ($users usuários), compactado e cifrado em extra/$object no destino do backup"
log "para ler: docker compose exec -T $agent_service recorvia-pgbackup get-object $object > realm.json"
echo "$object"
