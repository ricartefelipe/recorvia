#!/bin/sh
# Entrypoint da imagem recorvia-postgres (ADR-087). Antes de iniciar o
# servidor, confere a configuração do backup contínuo (destino S3 e chave de
# cifragem): sem ela, o archive_command falharia a cada segmento, o WAL se
# acumularia no disco e o servidor pareceria saudável. Com a configuração
# válida, segue o entrypoint da imagem oficial (initdb, scripts de
# inicialização e troca para o usuário postgres). Outros comandos (por
# exemplo, recorvia-pgbackup serve) são executados diretamente.
set -eu
if [ "${1:-}" = postgres ]; then
  recorvia-pgbackup check-config
fi
exec docker-entrypoint.sh "$@"
