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
# compose (COMPOSE_PROJECT_NAME, COMPOSE_FILE, COMPOSE_ENV_FILES).
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
dc() { docker compose "$@"; }
[[ -n $(dc ps -q --status running "$agent_service" 2>/dev/null) ]] || die "o agente de backup ($agent_service) precisa estar em execução"
object="keycloak/realm-$realm-$(date -u +%Y%m%dT%H%M%SZ).json"

log "exportando o realm $realm (kc.sh export num container avulso do serviço $kc_service)"
# A saída do kc.sh vai para a saída de erro; só o arquivo exportado segue
# pela saída padrão até o agente, que só envia depois de ler a entrada
# inteira (com a exportação falhando, a entrada fica vazia e nada é enviado).
dc run --rm --no-deps -T --entrypoint sh "$kc_service" -c \
  "/opt/keycloak/bin/kc.sh export --realm '$realm' --users same_file --file /tmp/realm.json >&2 && cat /tmp/realm.json" |
  dc exec -T "$agent_service" recorvia-pgbackup put-object "$object"
log "realm $realm exportado, compactado e cifrado em extra/$object no destino do backup"
log "para ler: docker compose exec -T $agent_service recorvia-pgbackup get-object $object > realm.json"
echo "$object"
