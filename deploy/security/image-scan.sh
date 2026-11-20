#!/usr/bin/env bash
# Análise de vulnerabilidades das imagens com Trivy em versão fixa (ADR-057).
# Falha em vulnerabilidades HIGH ou CRITICAL com correção disponível. As
# exceções documentadas ficam em .trivyignore.yaml (com justificativa e
# expiração) e entram no container pela entrada padrão, sem bind mount
# (ADR-020). Uso: bash deploy/security/image-scan.sh imagem[:tag]...
set -euo pipefail
: "${TRIVY_IMAGE:=aquasec/trivy:0.76.2@sha256:8f6bb947dbc528902e11f2f5be52f7b519f6a39d5d148f41166f2040499768fb}"
: "${TRIVY_CACHE_VOLUME:=recorvia-trivy-cache}"
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
ignore="$root/.trivyignore.yaml"
[[ $# -gt 0 ]] || {
  echo "uso: $0 imagem[:tag]..." >&2
  exit 2
}
failed=()
for img in "$@"; do
  echo "== $img"
  # shellcheck disable=SC2016 # "$@" é expandido pelo sh do container
  if ! docker run --rm -i -v /var/run/docker.sock:/var/run/docker.sock -v "$TRIVY_CACHE_VOLUME:/root/.cache/" \
    --entrypoint sh "$TRIVY_IMAGE" -c 'cat >/tmp/trivyignore.yaml && exec trivy image --ignorefile /tmp/trivyignore.yaml "$@"' trivy \
    --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 --no-progress --timeout 15m "$img" <"$ignore"; then
    failed+=("$img")
  fi
done
if ((${#failed[@]})); then
  echo "imagens com vulnerabilidades HIGH/CRITICAL corrigíveis: ${failed[*]}" >&2
  exit 1
fi
echo "nenhuma vulnerabilidade HIGH/CRITICAL com correção disponível"
