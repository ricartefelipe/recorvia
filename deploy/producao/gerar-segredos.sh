#!/usr/bin/env bash
# Gera os segredos de produção do Recorvia (ADR-086).
#
# Uso:
#   bash gerar-segredos.sh [ARQUIVO]   preenche, no ARQUIVO (padrão: .env
#                                      desta pasta, criado a partir do
#                                      .env.example se não existir), toda
#                                      linha NOME=GERAR com um valor novo
#   bash gerar-segredos.sh --senha     imprime uma senha nova
#   bash gerar-segredos.sh --chave     imprime uma chave de cifragem nova
#                                      (identificador:base64), para a rotação
#   bash gerar-segredos.sh --chave-backup ARQUIVO
#                                      cria o arquivo da chave de cifragem do
#                                      backup contínuo, se ainda não existir
#
# No modo de preenchimento, também cria a chave de cifragem do backup
# contínuo (ADR-087), se ainda não existir, no caminho de
# RECORVIA_BACKUP_ENCRYPTION_KEY_HOST_FILE no ARQUIVO (padrão:
# segredos/backup-cifragem.chave desta pasta; caminhos relativos a esta
# pasta): 32 bytes em base64, permissão 0444 (o PostgreSQL e o agente rodam
# com outro usuário e leem o arquivo montado como segredo) numa pasta 0700
# (ninguém além do dono chega a ele no servidor).
#
# Os valores vêm de /dev/urandom (gerador do kernel, adequado a segredos):
# senhas com 32 bytes em hexadecimal (64 caracteres, sem escape em URLs) e
# chaves de cifragem com 32 bytes em base64. Linhas já preenchidas não são
# alteradas: rodar de novo não troca nenhum segredo. O arquivo fica com
# permissão 600 e nenhum valor é impresso no modo de preenchimento.
set -euo pipefail
umask 077
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

die() {
  echo "erro: $*" >&2
  exit 1
}

random_hex() { od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; }

random_key() { head -c 32 /dev/urandom | base64 | tr -d '\n'; }

# key_id: identificador da chave pela data (UTC); o sufixo distingue chaves
# geradas no mesmo dia.
key_id() { printf 'k%s%s' "$(date -u +%Y%m%d)" "$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')"; }

# backup_key_file ARQUIVO: cria a chave do backup contínuo sem sobrescrever
# uma existente; devolve 0 se criou e 1 se já existia.
backup_key_file() {
  local file=$1 dir
  dir=$(dirname "$file")
  [[ -e $file ]] && return 1
  mkdir -p "$dir"
  chmod 700 "$dir"
  random_key >"$file.tmp"
  chmod 444 "$file.tmp"
  mv "$file.tmp" "$file"
  return 0
}

case ${1:-} in
  --chave-backup)
    [[ -n ${2:-} ]] || die "informe o arquivo da chave do backup"
    if backup_key_file "$2"; then
      echo "chave de cifragem do backup criada em $2 (guarde uma cópia no cofre)" >&2
    else
      echo "$2 já existe: nada foi alterado" >&2
    fi
    exit 0
    ;;
  --senha)
    random_hex
    echo
    exit 0
    ;;
  --chave)
    printf '%s:%s\n' "$(key_id)" "$(random_key)"
    exit 0
    ;;
  -h | --help)
    sed -n '2,28p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 0
    ;;
  -*) die "opção desconhecida: $1 (use --senha, --chave, --chave-backup ou o caminho do arquivo)" ;;
esac

target=${1:-$here/.env}
if [[ ! -e $target ]]; then
  cp "$here/.env.example" "$target"
  echo "criado $target a partir de .env.example" >&2
fi
[[ -f $target && -w $target ]] || die "$target não é um arquivo gravável"

key=""
id=""
if grep -qE '^RECORVIA_ENCRYPTION_(KEYS|ACTIVE_KEY)=GERAR$' "$target"; then
  if ! { grep -qE '^RECORVIA_ENCRYPTION_KEYS=GERAR$' "$target" && grep -qE '^RECORVIA_ENCRYPTION_ACTIVE_KEY=GERAR$' "$target"; }; then
    die "RECORVIA_ENCRYPTION_KEYS e RECORVIA_ENCRYPTION_ACTIVE_KEY são geradas juntas: deixe as duas com GERAR ou preencha as duas"
  fi
  id=$(key_id)
  key="$id:$(random_key)"
fi

tmp=$(mktemp "$target.XXXXXX")
trap 'rm -f "$tmp"' EXIT
count=0
while IFS= read -r line || [[ -n $line ]]; do
  if [[ $line =~ ^([A-Z][A-Z0-9_]*)=GERAR$ ]]; then
    name=${BASH_REMATCH[1]}
    case $name in
      RECORVIA_ENCRYPTION_KEYS) value=$key ;;
      RECORVIA_ENCRYPTION_ACTIVE_KEY) value=$id ;;
      *) value=$(random_hex) ;;
    esac
    line="$name=$value"
    count=$((count + 1))
  fi
  printf '%s\n' "$line" >>"$tmp"
done <"$target"
chmod 600 "$tmp"
mv "$tmp" "$target"
trap - EXIT

echo "segredos gerados: $count (arquivo $target, permissão 600)" >&2
backup_key=$(sed -n 's/^RECORVIA_BACKUP_ENCRYPTION_KEY_HOST_FILE=//p' "$target" | tail -n 1)
backup_key=${backup_key:-./segredos/backup-cifragem.chave}
[[ $backup_key == /* ]] || backup_key=$here/$backup_key
if backup_key_file "$backup_key"; then
  echo "chave de cifragem do backup contínuo criada em $backup_key. Guarde uma cópia no cofre, à parte do .env, antes da primeira subida." >&2
fi
if [[ -n $id ]]; then
  echo "chave de cifragem nova: $id. Guarde o par completo de RECORVIA_ENCRYPTION_KEYS no cofre antes da primeira subida." >&2
fi
pending=$(grep -nE '^[A-Z][A-Z0-9_]*=.*(example\.com|PREENCHER)' "$target" | cut -d= -f1 | sed -E 's/^([0-9]+):/  linha \1: /' || true)
if [[ -n $pending ]]; then
  echo "ainda com valor de exemplo (preencha à mão):" >&2
  echo "$pending" >&2
fi
