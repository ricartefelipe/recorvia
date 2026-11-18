# shellcheck shell=bash
# Funções comuns dos scripts de backup e de verificação da restauração do
# PostgreSQL. pg_dump, pg_restore e psql são executados dentro do container
# do PostgreSQL (mesma versão do servidor) e os dados trafegam pela entrada e
# saída padrão do docker exec: não há bind mounts nem portas publicadas
# envolvidas (ADR-020, ADR-055).
#
# Variáveis:
#   RECORVIA_PG_CONTAINER  container do PostgreSQL (padrão: serviço "postgres"
#                          do docker compose do diretório atual)
#   RECORVIA_PG_DATABASE   banco de origem (padrão: recorvia)
#   RECORVIA_PG_SUPERUSER  superusuário local do container (padrão: postgres)
#   RECORVIA_BACKUP_DIR    diretório dos arquivos de backup (padrão: backups)

: "${RECORVIA_PG_DATABASE:=recorvia}"
: "${RECORVIA_PG_SUPERUSER:=postgres}"
: "${RECORVIA_BACKUP_DIR:=backups}"

die() {
  echo "erro: $*" >&2
  exit 1
}

log() { echo "==> $*" >&2; }

# resolve_container define PG com o container do PostgreSQL.
resolve_container() {
  command -v docker >/dev/null || die "docker não encontrado"
  PG=${RECORVIA_PG_CONTAINER:-}
  if [[ -z $PG ]]; then
    PG=$(docker compose ps -q postgres 2>/dev/null || true)
  fi
  [[ -n $PG ]] || die "container do PostgreSQL não encontrado: suba o ambiente (make up) ou defina RECORVIA_PG_CONTAINER"
  docker exec "$PG" pg_isready -q -U "$RECORVIA_PG_SUPERUSER" ||
    die "PostgreSQL do container $PG não está aceitando conexões"
}

# psql_in BANCO [argumentos]: psql no container como superusuário, sem
# arquivo .psqlrc, interrompendo no primeiro erro.
psql_in() {
  local db=$1
  shift
  docker exec -i "$PG" psql -X -q -A -t -v ON_ERROR_STOP=1 -U "$RECORVIA_PG_SUPERUSER" -d "$db" "$@"
}

# valid_identifier NOME: nomes de banco aceitos pelos scripts (evita
# interpolação de SQL arbitrário nos comandos de criação e remoção).
valid_identifier() {
  [[ $1 =~ ^[a-z_][a-z0-9_]{0,62}$ ]] || die "nome de banco inválido: $1"
}

# manifest_sql: consultas do manifesto. Executadas na origem (no mesmo
# snapshot do pg_dump) e no banco restaurado; as saídas precisam ser iguais.
#   table <tabela> <linhas>          contagem exata de cada tabela do schema public
#   goose_version <versão>           última migration aplicada
#   rls_tables <n> / policies <n>    tabelas com RLS e políticas
#   triggers <n>                     gatilhos (imutabilidade, balanceamento)
#   ledger_balance <moeda> <d> <c>   soma de débitos e créditos por moeda
#   ledger_transactions_md5 <hash>   conteúdo de todas as transações do ledger
#   ledger_entries_md5 <hash>        conteúdo de todas as partidas do ledger
#   ledger_sample <id> <tenant> <tipo> <partidas> <débitos> <créditos> <hash>
#                                    lançamento mais recente, conferido em detalhe
manifest_sql() {
  cat <<'SQL'
SET TimeZone = 'UTC';
SET DateStyle = 'ISO, YMD';
SELECT 'table ' || c.relname || ' ' ||
       (xpath('/row/n/text()', query_to_xml(format('SELECT count(*) AS n FROM public.%I', c.relname), false, true, '')))[1]::text
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
 ORDER BY c.relname;
SELECT 'goose_version ' || COALESCE(max(version_id), 0) FROM goose_db_version WHERE is_applied;
SELECT 'rls_tables ' || count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relrowsecurity;
SELECT 'policies ' || count(*) FROM pg_policies WHERE schemaname = 'public';
SELECT 'triggers ' || count(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND NOT t.tgisinternal;
SELECT 'ledger_balance ' || currency || ' ' || sum(amount) FILTER (WHERE direction = 'debit') || ' ' ||
       sum(amount) FILTER (WHERE direction = 'credit')
  FROM ledger_entries GROUP BY currency ORDER BY currency;
SELECT 'ledger_transactions_md5 ' || md5(COALESCE(string_agg(md5(t::text), '' ORDER BY t.id), '')) FROM ledger_transactions t;
SELECT 'ledger_entries_md5 ' || md5(COALESCE(string_agg(md5(e::text), '' ORDER BY e.id), '')) FROM ledger_entries e;
SELECT 'ledger_sample ' || t.id || ' ' || t.tenant_id || ' ' || t.kind || ' ' || count(e.id) || ' ' ||
       COALESCE(sum(e.amount) FILTER (WHERE e.direction = 'debit'), 0) || ' ' ||
       COALESCE(sum(e.amount) FILTER (WHERE e.direction = 'credit'), 0) || ' ' ||
       md5(string_agg(e.account_id || ':' || e.direction || ':' || e.amount || ':' || e.currency, ',' ORDER BY e.id))
  FROM (SELECT * FROM ledger_transactions ORDER BY created_at DESC, id DESC LIMIT 1) t
  JOIN ledger_entries e ON e.tenant_id = t.tenant_id AND e.transaction_id = t.id
 GROUP BY t.id, t.tenant_id, t.kind;
SQL
}
