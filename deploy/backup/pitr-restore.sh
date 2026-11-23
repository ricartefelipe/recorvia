#!/usr/bin/env bash
# Recuperação do PostgreSQL para um ponto no tempo num servidor novo (ADR-087),
# a partir do backup contínuo (backup base e WAL arquivado pelo componente
# deploy/postgres).
# Uso: make pitr-restore TARGET=<instante RFC 3339 | latest> [TIMELINE=<n>] [BACKUP=<nome>] [MANIFEST=<arquivo>]
#      (ou bash deploy/backup/pitr-restore.sh (--target-time <instante> | --latest)
#       [--timeline <n>] [--backup <nome>] [--manifest <arquivo>])
#
# Linha do tempo: depois de uma restauração anterior, o arquivo tem mais de
# uma. Sem --timeline, a recuperação segue a mais recente (a do servidor em
# uso); --timeline escolhe outra (por exemplo, a original, para um instante
# posterior ao da restauração anterior). O backup base escolhido é sempre um
# do histórico da linha seguida.
#
# Usa o docker compose do diretório atual ou o definido pelas variáveis do
# próprio compose (COMPOSE_PROJECT_NAME, COMPOSE_FILE, COMPOSE_ENV_FILES), com
# o serviço do PostgreSQL do componente (imagem recorvia-postgres). A chave de
# cifragem do backup e as credenciais do armazenamento são as do ambiente
# desse serviço. Passos:
#   1. confere que o servidor está parado (nenhum container em execução,
#      reiniciando ou pausado) e para o agente de backup, que é religado se o
#      roteiro falhar antes de o servidor entrar em recuperação;
#   2. num container avulso do serviço, com o volume de dados vazio (servidor
#      novo), escolhe o backup base terminado antes do instante, recusa chave
#      ausente ou diferente da usada no backup antes de baixar qualquer
#      arquivo, extrai o backup e grava o alvo da recuperação
#      (recorvia-pgbackup restore);
#   3. inicia o servidor, que busca o WAL no armazenamento até o instante e é
#      promovido numa linha do tempo nova; registra o último commit aplicado;
#   4. remove o alvo da recuperação da configuração (ALTER SYSTEM RESET);
#   5. confere o banco: manifesto (as mesmas consultas do backup lógico),
#      comparado com --manifest se informado, ledger balanceado e RLS ativa;
#   6. religa o agente de backup, que faz um backup base da linha do tempo
#      nova.
# Não inicia a API nem o worker: chaves, API, conciliação da janela perdida e
# worker seguem o runbook (docs/operacao/runbooks/recuperacao-de-desastre.md).
# Nunca remove volumes: com dados no volume, a restauração é recusada.
#
# Variáveis (opcionais):
#   RECORVIA_PG_SERVICE        serviço do PostgreSQL (padrão: postgres)
#   RECORVIA_PGBACKUP_SERVICE  serviço do agente de backup (padrão: postgres-backup)
#   RECORVIA_PITR_TIMEOUT      espera máxima, em segundos, pela recuperação (padrão: 1800)
#   RECORVIA_PITR_DIR          diretório do manifesto do banco restaurado (padrão: backups)
#   RECORVIA_PITR_SUMMARY      arquivo para o resumo em chave=valor (usado pelo exercício)
set -euo pipefail
umask 077
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/backup/common.sh
source "$here/common.sh"

pg_service=${RECORVIA_PG_SERVICE:-postgres}
agent_service=${RECORVIA_PGBACKUP_SERVICE:-postgres-backup}
timeout_s=${RECORVIA_PITR_TIMEOUT:-1800}
out_dir=${RECORVIA_PITR_DIR:-backups}
summary=${RECORVIA_PITR_SUMMARY:-}

usage() { die "uso: pitr-restore.sh (--target-time <RFC 3339> | --latest) [--timeline <n>] [--backup <nome>] [--manifest <arquivo>]"; }
target="" latest=0 backup="" manifest="" timeline_opt=""
while (($# > 0)); do
  case $1 in
    --latest) latest=1 ;;
    --target-time | --backup | --manifest | --timeline)
      (($# >= 2)) || usage
      case $1 in
        --target-time) target=$2 ;;
        --backup) backup=$2 ;;
        --manifest) manifest=$2 ;;
        --timeline) timeline_opt=$2 ;;
      esac
      shift
      ;;
    *) usage ;;
  esac
  shift
done
[[ -n $target || $latest == 1 ]] || usage
[[ -z $target || $latest == 0 ]] || die "informe --target-time ou --latest, não os dois"
[[ $timeout_s =~ ^[1-9][0-9]*$ ]] || die "RECORVIA_PITR_TIMEOUT inválido: $timeout_s"
[[ -z $timeline_opt || $timeline_opt =~ ^[1-9][0-9]*$ ]] || die "linha do tempo inválida: $timeline_opt"
if [[ -n $manifest ]]; then
  [[ -f $manifest ]] || die "manifesto não encontrado: $manifest"
  require_manifest_lines "$manifest"
fi
command -v docker >/dev/null || die "docker não encontrado"
dc() { docker compose "$@"; }
# A lista vem inteira antes da busca: com pipefail, o grep -q que termina
# antes do fim da saída faria o compose falhar por SIGPIPE.
services=$(dc config --services 2>/dev/null) || die "configuração do compose inválida (defina COMPOSE_FILE ou execute no diretório do compose)"
grep -qx "$pg_service" <<<"$services" || die "serviço $pg_service não encontrado no compose (defina COMPOSE_FILE ou execute no diretório do compose)"
has_agent=0
if grep -qx "$agent_service" <<<"$services"; then
  has_agent=1
fi
now_s() { date -u +%s; }
summary_line() {
  if [[ -n $summary ]]; then
    echo "$1=$2" >>"$summary"
  fi
}
if [[ -n $summary ]]; then
  : >"$summary"
fi

# ---------- 1. Servidor parado ----------
# Só um container parado (exited) ou nunca iniciado (created) do serviço, ou
# nenhum, é aceito: em execução, reiniciando (crash loop), pausado ou em
# remoção, o servidor ainda pode estar usando o volume.
states=$(dc ps -a --format '{{.State}}' "$pg_service" 2>/dev/null) || die "não foi possível consultar o serviço $pg_service"
while IFS= read -r state; do
  case $state in
    "" | exited | created) ;;
    *) die "o serviço $pg_service está no estado $state: a restauração é feita num servidor parado e novo. Pare-o (docker compose stop $pg_service) e use um volume de dados vazio; nada foi alterado" ;;
  esac
done <<<"$states"

# A partir daqui o agente fica parado. Se o roteiro terminar com erro, ele é
# religado, a menos que o servidor esteja no meio da recuperação (iniciado e
# ainda não promovido): aí o agente fica parado até a situação ser resolvida.
agent_stopped=0 agent_restarted=0 server_started=0
server_primary() {
  [[ -n ${PG:-} && $(docker inspect -f '{{.State.Status}}' "$PG" 2>/dev/null) == running ]] &&
    [[ $(docker exec "$PG" psql -X -q -A -t -U "$RECORVIA_PG_SUPERUSER" -d postgres -c "SELECT pg_is_in_recovery()" 2>/dev/null) == f ]]
}
restart_agent_on_failure() {
  local code=$?
  trap - EXIT
  if ((code != 0 && agent_stopped == 1 && agent_restarted == 0)); then
    if ((server_started == 0)) || server_primary; then
      log "religando o agente de backup ($agent_service) depois da falha"
      dc up -d --no-deps "$agent_service" >/dev/null 2>&1 || log "aviso: não foi possível religar o agente; religue com docker compose up -d $agent_service"
    else
      log "aviso: o agente de backup ($agent_service) continua parado, porque o servidor ficou no meio da recuperação; depois de resolver, religue com docker compose up -d $agent_service"
    fi
  fi
  exit "$code"
}
trap restart_agent_on_failure EXIT
if ((has_agent == 1)); then
  log "parando o agente de backup ($agent_service) durante a restauração"
  dc stop "$agent_service" >/dev/null 2>&1 || true
  agent_stopped=1
fi
# Container parado de uma execução anterior: removido (o volume fica).
dc rm -f "$pg_service" >/dev/null 2>&1 || true

# ---------- 2. Backup base e alvo ----------
args=()
if [[ -n $target ]]; then
  args+=(--target-time "$target")
else
  args+=(--latest)
fi
if [[ -n $backup ]]; then
  args+=(--backup "$backup")
fi
if [[ -n $timeline_opt ]]; then
  args+=(--timeline "$timeline_opt")
fi
log "extraindo o backup base (recorvia-pgbackup restore ${args[*]})"
fetch_started=$(now_s)
dc run --rm --no-deps -T --user postgres --entrypoint recorvia-pgbackup "$pg_service" restore "${args[@]}" ||
  die "restauração recusada ou interrompida: veja a mensagem acima"
fetch_finished=$(now_s)
summary_line fetch_seconds $((fetch_finished - fetch_started))

# ---------- 3. Recuperação até o instante ----------
log "iniciando o servidor para a recuperação até ${target:-o fim do WAL arquivado}"
recovery_started=$(now_s)
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
server_started=1
dc up -d --no-deps "$pg_service"
PG=$(dc ps -q "$pg_service")
[[ -n $PG ]] || die "container do serviço $pg_service não encontrado"
in_recovery() { docker exec "$PG" psql -X -q -A -t -U "$RECORVIA_PG_SUPERUSER" -d postgres -c "SELECT pg_is_in_recovery()" 2>/dev/null; }
state=""
while :; do
  status=$(docker inspect -f '{{.State.Status}}' "$PG" 2>/dev/null || echo ausente)
  if [[ $status != running ]]; then
    dc logs --no-color --since "$since" "$pg_service" 2>&1 | tail -n 30 >&2
    die "o servidor terminou durante a recuperação (situação: $status): WAL ausente até o instante, WAL ilegível (chave de cifragem) ou instante anterior ao fim do backup base"
  fi
  state=$(in_recovery || true)
  [[ $state == f ]] && break
  (($(now_s) - recovery_started < timeout_s)) || die "tempo esgotado ($timeout_s s) aguardando a recuperação"
  sleep 2
done
recovery_finished=$(now_s)
summary_line recovery_seconds $((recovery_finished - recovery_started))
recovery_log=$(dc logs --no-color --since "$since" "$pg_service" 2>&1 || true)
stop_line=$(grep -o 'recovery stopping [^"]*' <<<"$recovery_log" | tail -n 1 || true)
last_commit=$(grep -o 'last completed transaction was at log time [^"]*' <<<"$recovery_log" | tail -n 1 | sed 's/^last completed transaction was at log time //' || true)
log "recuperação concluída em $((recovery_finished - recovery_started)) s; ${stop_line:-recuperação até o fim do WAL arquivado}"
log "último commit aplicado: ${last_commit:-não registrado no log}"
summary_line last_commit "${last_commit:-}"
summary_line stop "${stop_line:-}"

# ---------- 4. Configuração ----------
RECORVIA_PG_CONTAINER=$PG
export RECORVIA_PG_CONTAINER
resolve_container
psql_in postgres >/dev/null <<'SQL'
ALTER SYSTEM RESET recovery_target_time;
ALTER SYSTEM RESET recovery_target_action;
ALTER SYSTEM RESET recovery_target_timeline;
SELECT pg_reload_conf();
SQL
timeline=$(psql_in postgres -c "SELECT timeline_id FROM pg_control_checkpoint()")
log "servidor promovido na linha do tempo $timeline; alvo da recuperação removido da configuração"
summary_line timeline "$timeline"

# ---------- 5. Conferência ----------
if [[ $(psql_in postgres -c "SELECT count(*) FROM pg_database WHERE datname = '$RECORVIA_PG_DATABASE'") != 1 ]]; then
  log "aviso: o banco $RECORVIA_PG_DATABASE não existe no instante restaurado; conferência não aplicável"
else
  mkdir -p "$out_dir"
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  restored="$out_dir/$RECORVIA_PG_DATABASE-pitr-$stamp.manifest"
  manifest_sql | psql_in "$RECORVIA_PG_DATABASE" >"$restored"
  require_manifest_lines "$restored"
  summary_line manifest "$restored"
  if [[ -n $manifest ]]; then
    diff -u "$manifest" "$restored" >&2 || die "o banco restaurado difere do manifesto $manifest (manifesto do banco em $restored)"
    log "manifesto do banco restaurado idêntico a $manifest"
  fi
  unbalanced=$(psql_in "$RECORVIA_PG_DATABASE" -c "SELECT count(*) FROM (SELECT 1 FROM ledger_entries GROUP BY tenant_id, transaction_id, currency
    HAVING sum(amount) FILTER (WHERE direction = 'debit') IS DISTINCT FROM sum(amount) FILTER (WHERE direction = 'credit')) x")
  [[ $unbalanced == 0 ]] || die "ledger desbalanceado no banco restaurado ($unbalanced lançamentos)"
  # Sem contexto de tenant, o papel da aplicação não vê partidas do ledger.
  hidden=$(docker exec -i "$PG" psql -X -q -A -t -v ON_ERROR_STOP=1 -U recorvia_app -d "$RECORVIA_PG_DATABASE" -c "SELECT count(*) FROM ledger_entries")
  [[ $hidden == 0 ]] || die "RLS não aplicada no banco restaurado: $hidden partidas visíveis sem tenant"
  tables=$(grep -c '^table ' "$restored")
  rows=$(awk '$1 == "table" { s += $3 } END { print s + 0 }' "$restored")
  log "banco conferido: $tables tabelas, $rows linhas (manifesto em $restored); ledger balanceado; RLS ativa para recorvia_app"
fi

# ---------- 6. Agente de backup ----------
if ((has_agent == 1)); then
  log "religando o agente de backup: o backup base da linha do tempo $timeline começa em seguida"
  dc up -d --no-deps "$agent_service" >/dev/null
  agent_restarted=1
fi
log "servidor restaurado até ${target:-o fim do WAL arquivado}. Próximos passos (runbook): chaves de cifragem (keyring-check), API sem tráfego, conciliação da janela perdida e só então o worker"
