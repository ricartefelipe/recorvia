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

case ${1:-} in
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
    sed -n '2,17p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 0
    ;;
  -*) die "opção desconhecida: $1 (use --senha, --chave ou o caminho do arquivo)" ;;
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
if [[ -n $id ]]; then
  echo "chave de cifragem nova: $id. Guarde o par completo de RECORVIA_ENCRYPTION_KEYS no cofre antes da primeira subida." >&2
fi
pending=$(grep -nE '^[A-Z][A-Z0-9_]*=.*(example\.com|PREENCHER)' "$target" | cut -d= -f1 | sed -E 's/^([0-9]+):/  linha \1: /' || true)
if [[ -n $pending ]]; then
  echo "ainda com valor de exemplo (preencha à mão):" >&2
  echo "$pending" >&2
fi
