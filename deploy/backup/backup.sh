#!/usr/bin/env bash
# Backup lógico do banco do Recorvia com pg_dump (formato custom) executado no
# container do PostgreSQL. Uso: make backup (ou bash deploy/backup/backup.sh).
#
# Produz, em RECORVIA_BACKUP_DIR:
#   recorvia-<instante UTC>.dump      cópia lógica (pg_restore)
#   recorvia-<instante UTC>.manifest  contagens e somas de verificação do ledger
#   recorvia-<instante UTC>.sha256    soma do arquivo .dump
# O manifesto é calculado no mesmo snapshot do pg_dump (pg_export_snapshot),
# o que o torna comparável com o banco restaurado mesmo com escrita
# concorrente na origem. Os arquivos contêm dados pessoais e financeiros:
# permissões 0600, fora do repositório (backups/ está no .gitignore).
#
# RECORVIA_BACKUP_MANIFEST_TIMEOUT (segundos, padrão 1800) limita a espera
# por cada linha do manifesto; as somas do ledger percorrem todas as
# partidas e crescem com o banco. O backup só é publicado se o manifesto
# terminar (sentinela lida) e tiver todas as linhas obrigatórias.
set -euo pipefail
umask 077
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/backup/common.sh
source "$here/common.sh"

resolve_container
valid_identifier "$RECORVIA_PG_DATABASE"
mkdir -p "$RECORVIA_BACKUP_DIR"
stamp=$(date -u +%Y%m%dT%H%M%SZ)
base="$RECORVIA_BACKUP_DIR/$RECORVIA_PG_DATABASE-$stamp"
tmp_dump="$base.dump.partial"
tmp_manifest="$base.manifest.partial"
trap 'rm -f "$tmp_dump" "$tmp_manifest"' EXIT

log "exportando snapshot de $RECORVIA_PG_DATABASE (container $PG)"
# Sessão que mantém o snapshot aberto durante o dump e o manifesto.
coproc SNAP { psql_in "$RECORVIA_PG_DATABASE"; }
# shellcheck disable=SC2153 # SNAP_PID é definido pelo coproc
snap_pid=$SNAP_PID
snap_in=${SNAP[1]}
snap_out=${SNAP[0]}
printf '%s\n' "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;" "SELECT pg_export_snapshot();" >&"$snap_in"
read -r -t 60 snapshot <&"$snap_out" || die "não foi possível exportar o snapshot"
[[ $snapshot =~ ^[0-9A-F-]+$ ]] || die "snapshot inesperado: $snapshot"

log "pg_dump -Fc --snapshot=$snapshot"
docker exec "$PG" pg_dump -U "$RECORVIA_PG_SUPERUSER" -d "$RECORVIA_PG_DATABASE" -Fc --snapshot="$snapshot" >"$tmp_dump"

manifest_timeout=${RECORVIA_BACKUP_MANIFEST_TIMEOUT:-1800}
[[ $manifest_timeout =~ ^[1-9][0-9]*$ ]] || die "RECORVIA_BACKUP_MANIFEST_TIMEOUT inválido: $manifest_timeout"
log "calculando o manifesto no mesmo snapshot"
{
  manifest_sql
  echo "SELECT 'end-of-manifest';"
} >&"$snap_in"
# O laço termina sem erro tanto pela sentinela quanto por fim do prazo ou da
# saída da sessão: só a sentinela indica manifesto completo.
manifest_done=0
while read -r -t "$manifest_timeout" line <&"$snap_out"; do
  if [[ $line == end-of-manifest ]]; then
    manifest_done=1
    break
  fi
  echo "$line"
done >"$tmp_manifest"
if ((manifest_done == 0)); then
  kill "$snap_pid" 2>/dev/null || true
  die "manifesto incompleto: a sessão do snapshot não concluiu as consultas em ${manifest_timeout} s por linha (RECORVIA_BACKUP_MANIFEST_TIMEOUT) ou terminou antes"
fi
printf '%s\n' "COMMIT;" '\q' >&"$snap_in"
wait "$snap_pid" || die "a sessão do snapshot terminou com erro"
require_manifest_lines "$tmp_manifest"

# Confere que o arquivo é legível pelo pg_restore antes de publicá-lo.
docker exec -i "$PG" pg_restore --list <"$tmp_dump" >/dev/null || die "arquivo de dump ilegível"
mv "$tmp_dump" "$base.dump"
mv "$tmp_manifest" "$base.manifest"
(cd "$RECORVIA_BACKUP_DIR" && sha256sum "$(basename "$base.dump")" >"$(basename "$base.sha256")")
trap - EXIT

tables=$(grep -c '^table ' "$base.manifest")
rows=$(awk '$1 == "table" { s += $3 } END { print s + 0 }' "$base.manifest")
log "backup concluído: $base.dump ($(wc -c <"$base.dump") bytes; $tables tabelas, $rows linhas)"
echo "$base.dump"
