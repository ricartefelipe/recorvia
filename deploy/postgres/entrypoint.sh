#!/bin/sh
# Entrypoint da imagem recorvia-postgres (ADR-087). Antes de iniciar o
# servidor, confere a configuração do backup contínuo (destino S3 e chave de
# cifragem): sem ela, o archive_command falharia a cada segmento, o WAL se
# acumularia no disco e o servidor pareceria saudável. A conferência roda
# como o usuário postgres, o mesmo do archive_command, que relê os segredos
# *_FILE a cada segmento: um arquivo legível só pelo root é recusado aqui, em
# vez de fazer falhar todo arquivamento depois. Com a configuração válida,
# segue o entrypoint da imagem oficial (initdb, scripts de inicialização e
# troca para o usuário postgres). Outros comandos (por exemplo,
# recorvia-pgbackup serve) são executados diretamente.
set -eu
if [ "${1:-}" = postgres ]; then
  if [ "$(id -u)" = 0 ]; then
    gosu postgres recorvia-pgbackup check-config
  else
    recorvia-pgbackup check-config
  fi
fi
exec docker-entrypoint.sh "$@"
