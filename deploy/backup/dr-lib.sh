# shellcheck shell=bash
# Funções do exercício de recuperação de desastre (dr-check.sh, ADR-064) que
# protegem o que não é do exercício e que conferem o dump: validação e preparo
# do diretório de saída, posse do projeto do compose e varredura do dump por
# segredos em claro. Usa die e log de common.sh.

# Arquivo que marca um diretório criado pelo exercício e rótulo dos containers
# e volumes criados por ele (deploy/backup/dr.compose.yaml).
dr_marker=.recorvia-dr-check
dr_label=recorvia.dr-check
# Variável e padrão do diretório de saída citados nas mensagens, e descrição
# gravada na marca do diretório. O exercício de recuperação para um ponto no
# tempo (pitr-check.sh) define os próprios antes de chamar as funções.
: "${dr_dir_var:=RECORVIA_DR_DIR}"
: "${dr_dir_default:=backups/dr-check}"
: "${dr_dir_purpose:=Diretório do exercício de recuperação de desastre (make dr-check)}"
: "${dr_project_var:=RECORVIA_DR_PROJECT}"
: "${dr_reuse_var:=RECORVIA_DR_REUSE_PROJECT}"

# require_safe_dr_dir DIRETÓRIO RAIZ BACKUPS: encerra com erro, sem alterar
# nada, se o diretório de saída puder conter algo que não é do exercício: a
# raiz do repositório ou um diretório acima dela, o diretório dos backups
# (make backup) ou um diretório já existente, com conteúdo, que o exercício
# não marcou.
require_safe_dr_dir() {
  local given=$1 dir root backups
  [[ -n $given ]] || die "$dr_dir_var vazio"
  dir=$(realpath -m -- "$given")
  root=$(realpath -m -- "$2")
  backups=$(realpath -m -- "$3")
  if [[ $dir == / || $dir == "$root" || $root/ == "$dir"/* ]]; then
    die "$dr_dir_var ($given) é a raiz do repositório ou um diretório acima dela: informe um diretório próprio do exercício (padrão: $dr_dir_default)"
  fi
  if [[ $dir == "$backups" ]]; then
    die "$dr_dir_var ($given) é o diretório dos backups: o exercício usa um diretório próprio (padrão: $dr_dir_default) e nada foi apagado"
  fi
  if [[ -e $dir && ! -d $dir ]]; then
    die "$dr_dir_var ($given) existe e não é um diretório"
  fi
  if [[ -d $dir && ! -f $dir/$dr_marker && -n $(ls -A -- "$dir") ]]; then
    die "$dr_dir_var ($given) já existe, tem conteúdo e não foi criado pelo exercício (sem o arquivo $dr_marker): nada foi apagado; informe um diretório novo ou vazio"
  fi
}

# prepare_dr_dir DIRETÓRIO: cria e marca o diretório de saída. Se ele guarda
# uma execução anterior, os arquivos dela vão para DIRETÓRIO/anterior (no
# lugar da penúltima) em vez de serem apagados. Só mexe nos arquivos que o
# exercício gera: relatório, manifestos, efeitos, textos, logs e o backup.
prepare_dr_dir() {
  local dir=$1 previous=$1/anterior f
  mkdir -p "$dir/backup"
  if [[ -f $dir/$dr_marker ]]; then
    mkdir -p "$previous/backup"
    rm -f "$previous"/relatorio.md "$previous"/*.manifest "$previous"/*.efeitos "$previous"/*.txt "$previous"/*.log \
      "$previous"/backup/recorvia-*
    for f in "$dir"/relatorio.md "$dir"/*.manifest "$dir"/*.efeitos "$dir"/*.txt "$dir"/*.log; do
      if [[ -f $f ]]; then
        mv -f -- "$f" "$previous/"
      fi
    done
    for f in "$dir"/backup/recorvia-*; do
      if [[ -f $f ]]; then
        mv -f -- "$f" "$previous/backup/"
      fi
    done
  fi
  printf '%s. A cada execução, os arquivos da anterior vão para anterior/.\n' "$dr_dir_purpose" >"$dir/$dr_marker"
}

# require_own_project PROJETO: encerra com erro, sem remover nada, se o
# projeto do compose já tem containers ou volumes que o exercício não criou
# (sem o rótulo do exercício). O exercício derruba o próprio projeto com os
# volumes antes de começar e ao terminar; um projeto de mesmo nome criado de
# outra forma (por exemplo, uma restauração manual mantida para inspeção)
# perderia os dados. RECORVIA_DR_REUSE_PROJECT=1 (ou a variável em
# dr_reuse_var) confirma a remoção.
require_own_project() {
  local project=$1 kind all own foreign=""
  for kind in containers volumes; do
    if [[ $kind == containers ]]; then
      all=$(docker ps -a -q --filter "label=com.docker.compose.project=$project") || die "não foi possível listar os containers do projeto $project"
      own=$(docker ps -a -q --filter "label=com.docker.compose.project=$project" --filter "label=$dr_label=1") ||
        die "não foi possível listar os containers do projeto $project"
    else
      all=$(docker volume ls -q --filter "label=com.docker.compose.project=$project") || die "não foi possível listar os volumes do projeto $project"
      own=$(docker volume ls -q --filter "label=com.docker.compose.project=$project" --filter "label=$dr_label=1") ||
        die "não foi possível listar os volumes do projeto $project"
    fi
    if [[ $(sort <<<"$all") != "$(sort <<<"$own")" ]]; then
      foreign+="${foreign:+ e }$kind"
    fi
  done
  [[ -n $foreign ]] || return 0
  if [[ ${!dr_reuse_var:-0} == 1 ]]; then
    log "aviso: o projeto $project tem $foreign que o exercício não criou; serão removidos ($dr_reuse_var=1)"
    return 0
  fi
  die "o projeto $project do compose já tem $foreign que não foram criados pelo exercício (sem o rótulo $dr_label): nada foi removido. Use outro nome em $dr_project_var ou, se o projeto pode ser apagado com os volumes, repita com $dr_reuse_var=1"
}

# hex_of TEXTO: os bytes do texto em hexadecimal, como o pg_dump escreve o
# conteúdo das colunas bytea (bytea_output = hex).
hex_of() { printf %s "$1" | od -An -v -tx1 | tr -d ' \n'; }

# count_secret_leaks SEGREDO...: lê da entrada padrão o conteúdo de um dump em
# SQL (pg_restore -f -) e imprime o número de linhas que trazem algum dos
# segredos em claro, como texto (colunas text e json) ou como bytes em
# hexadecimal (colunas bytea, onde ficam os dados cifrados: um segredo gravado
# sem cifrar apareceria ali como \x seguido dos bytes em hexadecimal).
count_secret_leaks() {
  local s patterns=()
  for s in "$@"; do
    if [[ -n $s ]]; then
      patterns+=(-e "$s" -e "$(hex_of "$s")")
    fi
  done
  ((${#patterns[@]} > 0)) || die "varredura do dump sem segredos a procurar"
  grep -c -F "${patterns[@]}" || true
}
