#!/usr/bin/env bash
# Verificação da restauração: restaura um backup num banco novo do mesmo
# servidor, compara o manifesto (contagens por tabela, estrutura de RLS e
# gatilhos, somas e conteúdo do ledger e um lançamento em detalhe), confere o
# isolamento com o papel da aplicação e remove o banco de verificação.
# Uso: make restore-check [DUMP=backups/recorvia-....dump]
#      (ou bash deploy/backup/restore-check.sh [arquivo.dump])
#
# Exige os papéis recorvia_owner e recorvia_app no servidor (criados pelo
# bootstrap: cmd/migrate bootstrap). RECORVIA_RESTORE_KEEP=1 mantém o banco
# restaurado para inspeção.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/backup/common.sh
source "$here/common.sh"

dump=${1:-}
if [[ -z $dump ]]; then
  dump=$(find "$RECORVIA_BACKUP_DIR" -maxdepth 1 -name '*.dump' -type f 2>/dev/null | sort | tail -n 1)
  [[ -n $dump ]] || die "nenhum backup em $RECORVIA_BACKUP_DIR; execute make backup ou informe o arquivo"
fi
[[ -f $dump ]] || die "arquivo não encontrado: $dump"
manifest=${dump%.dump}.manifest
[[ -f $manifest ]] || die "manifesto ausente: $manifest"
require_manifest_lines "$manifest"
checksum=${dump%.dump}.sha256
if [[ -f $checksum ]]; then
  (cd "$(dirname "$dump")" && sha256sum --quiet -c "$(basename "$checksum")") || die "soma SHA-256 divergente: $dump"
  log "soma SHA-256 conferida"
else
  log "aviso: sem arquivo .sha256; integridade do arquivo não conferida"
fi

resolve_container
for role in recorvia_owner recorvia_app; do
  [[ $(psql_in postgres -c "SELECT count(*) FROM pg_roles WHERE rolname = '$role'") == 1 ]] ||
    die "papel $role ausente no servidor: execute o bootstrap (cmd/migrate bootstrap) antes de restaurar"
done

target=${RECORVIA_RESTORE_DATABASE:-recorvia_restore_check_$(date -u +%Y%m%d%H%M%S)_$$}
valid_identifier "$target"
[[ $(psql_in postgres -c "SELECT count(*) FROM pg_database WHERE datname = '$target'") == 0 ]] ||
  die "o banco $target já existe; a verificação só restaura em banco novo"

cleanup() {
  if [[ ${RECORVIA_RESTORE_KEEP:-0} == 1 ]]; then
    log "banco $target mantido (RECORVIA_RESTORE_KEEP=1)"
    return
  fi
  psql_in postgres -c "DROP DATABASE IF EXISTS $target WITH (FORCE)" >/dev/null || log "aviso: não foi possível remover $target"
}
trap cleanup EXIT

# Banco criado como no bootstrap (dono e privilégios de banco não fazem parte
# do pg_dump sem --create); schema, privilégios do schema e dados vêm do dump.
log "criando o banco de verificação $target"
psql_in postgres >/dev/null <<SQL
CREATE DATABASE $target OWNER recorvia_owner;
REVOKE ALL ON DATABASE $target FROM PUBLIC;
GRANT CONNECT, TEMPORARY ON DATABASE $target TO recorvia_app;
GRANT CONNECT ON DATABASE $target TO recorvia_owner;
SQL

log "pg_restore --exit-on-error --single-transaction ($dump)"
docker exec -i "$PG" pg_restore -U "$RECORVIA_PG_SUPERUSER" -d "$target" --exit-on-error --single-transaction <"$dump"

log "comparando o manifesto"
actual=$(mktemp)
trap 'rm -f "$actual"; cleanup' EXIT
manifest_sql | psql_in "$target" >"$actual"
if ! diff -u "$manifest" "$actual" >&2; then
  die "o banco restaurado difere do manifesto do backup"
fi

# Isolamento no banco restaurado: sem contexto de tenant, o papel da aplicação
# não vê partidas do ledger; com o tenant do lançamento de referência, vê as
# mesmas partidas que o superusuário.
read -r _ sample_tx sample_tenant sample_kind sample_entries sample_debits sample_credits _ < <(grep '^ledger_sample ' "$manifest" || true)
if [[ -n ${sample_tx:-} ]]; then
  [[ $sample_tenant =~ ^[0-9a-f-]{36}$ && $sample_tx =~ ^[0-9a-f-]{36}$ ]] || die "lançamento de referência inválido no manifesto"
  app_query() { docker exec -i "$PG" psql -X -q -A -t -v ON_ERROR_STOP=1 -U recorvia_app -d "$target"; }
  hidden=$(app_query <<<"SELECT count(*) FROM ledger_entries;")
  visible=$(app_query <<SQL
BEGIN;
SELECT set_config('app.tenant_id', '$sample_tenant', true) IS NULL;
SELECT count(*) FROM ledger_entries WHERE tenant_id = '$sample_tenant' AND transaction_id = '$sample_tx';
COMMIT;
SQL
  )
  visible=$(tail -n 1 <<<"$visible")
  [[ $hidden == 0 ]] || die "RLS não aplicada no banco restaurado: $hidden partidas visíveis sem tenant"
  [[ $visible == "$sample_entries" ]] || die "papel da aplicação vê $visible partidas do lançamento, esperado $sample_entries"
  log "isolamento conferido com recorvia_app: 0 partidas sem tenant; $visible partidas do lançamento com o tenant"
  log "lançamento de referência $sample_tx ($sample_kind): $sample_entries partidas, débitos $sample_debits, créditos $sample_credits"
else
  log "aviso: backup sem lançamentos no ledger; conferência do lançamento não aplicável"
fi

tables=$(grep -c '^table ' "$manifest")
rows=$(awk '$1 == "table" { s += $3 } END { print s + 0 }' "$manifest")
log "restauração verificada em $target: $tables tabelas e $rows linhas iguais ao manifesto; ledger idêntico"
