# Backup e restauração do PostgreSQL

Procedimento de cópia lógica do banco `recorvia` e de verificação por restauração (ADR-055). Este
documento separa o que **é exercitado** (onde, como e o que é conferido) do que é
**recomendação** para ambientes com dados reais e não é exercitado.

O procedimento é exercitado pela CI, a cada push e pull request para `develop` e `main`: pelo job
`backup-restore` (ambiente do `docker compose`, alvos `make backup` e `make restore-check` e
recuperação num servidor novo) e pelo teste de integração `TestBackupAndRestoreCheckScripts`
(job `go-integration`). Os detalhes estão em [O que é exercitado](#o-que-é-exercitado).

## Componentes

| Arquivo | Função |
|---|---|
| `deploy/backup/backup.sh` | `make backup`: `pg_dump -Fc` no container, manifesto no mesmo snapshot, soma SHA-256 |
| `deploy/backup/restore-check.sh` | `make restore-check`: restaura num banco novo, compara o manifesto, confere o isolamento e remove o banco |
| `deploy/backup/common.sh` | funções comuns e consultas do manifesto |
| `.github/workflows/ci.yml`, job `backup-restore` | exercício do procedimento completo no ambiente do compose |
| `internal/pgstore/backup_integration_test.go` | exercício dos scripts com escrita concorrente e manifesto adulterado (`make test-integration`) |

Os scripts são chamados com `bash` (o checkout pode estar em disco sem permissão de execução) e
executam `pg_dump`, `pg_restore` e `psql` **dentro do container do PostgreSQL**, com a mesma versão
do servidor (18.7), como superusuário local do container. Os dados trafegam pela entrada e saída
padrão do `docker exec`: não há bind mount nem porta publicada envolvida (ADR-020).

Variáveis (todas opcionais):

| Variável | Padrão | Uso |
|---|---|---|
| `RECORVIA_PG_CONTAINER` | serviço `postgres` do `docker compose` | container do PostgreSQL |
| `RECORVIA_PG_DATABASE` | `recorvia` | banco de origem |
| `RECORVIA_PG_SUPERUSER` | `postgres` | superusuário local do container |
| `RECORVIA_BACKUP_DIR` | `backups` | diretório dos arquivos (fora do Git: `.gitignore`) |
| `RECORVIA_RESTORE_DATABASE` | `recorvia_restore_check_<instante>_<pid>` | nome do banco de verificação |
| `RECORVIA_RESTORE_KEEP` | `0` | `1` mantém o banco restaurado para inspeção |

## Uso

```bash
make up                     # ambiente em execução
make backup                 # backups/recorvia-<instante UTC>.{dump,manifest,sha256}
make restore-check          # restaura o backup mais recente num banco novo e confere
make restore-check DUMP=backups/recorvia-20261118T120000Z.dump
```

### O que o backup produz

- `recorvia-<instante>.dump`: formato custom do `pg_dump` (compactado; restauração seletiva com
  `pg_restore -l`/`-L`), com esquema, dados, privilégios das tabelas e do schema, políticas de
  RLS, funções e gatilhos. Conferido com `pg_restore --list` antes de ser publicado no diretório.
- `recorvia-<instante>.manifest`: gerado **no mesmo snapshot** do dump (`pg_export_snapshot` numa
  transação `REPEATABLE READ` mantida aberta durante o `pg_dump`), portanto comparável com o banco
  restaurado mesmo com escrita concorrente na origem. Linhas:
  - `table <tabela> <linhas>`: contagem exata de cada tabela do schema `public`;
  - `goose_version`, `rls_tables`, `policies`, `triggers`: estrutura de migrations, RLS e gatilhos;
  - `ledger_balance <moeda> <débitos> <créditos>`;
  - `ledger_transactions_md5`, `ledger_entries_md5`: hash do conteúdo de todas as linhas do ledger;
  - `ledger_sample <id> <tenant> <tipo> <partidas> <débitos> <créditos> <hash>`: o lançamento mais
    recente, conferido em detalhe.
- `recorvia-<instante>.sha256`: soma do arquivo de dump.

Os três arquivos são criados com permissão `0600` (`umask 077`). Arquivos parciais
(`*.partial`) são removidos em caso de falha.

### O que a verificação confere

1. Soma SHA-256 do arquivo.
2. Existência dos papéis `recorvia_owner` e `recorvia_app` (pré-requisito da restauração).
3. Criação do banco de verificação como no bootstrap (dono `recorvia_owner`, `REVOKE ALL ... FROM
   PUBLIC`, `CONNECT` e `TEMPORARY` para `recorvia_app`).
4. `pg_restore --exit-on-error --single-transaction`.
5. Manifesto do banco restaurado idêntico ao do backup (`diff`).
6. Isolamento no banco restaurado com o papel `recorvia_app`: sem contexto de tenant, nenhuma
   partida do ledger visível; com o tenant do lançamento de referência, exatamente as partidas
   desse lançamento.
7. Remoção do banco de verificação ao final, com sucesso ou falha.

## O que é exercitado

### Job `backup-restore` da CI

Executado em cada push e pull request para `develop` e `main` (`.github/workflows/ci.yml`), num
runner limpo, com o ambiente do `docker compose` e os mesmos alvos do Makefile usados localmente:

1. `make env` e `docker compose up -d --build --wait worker`: PostgreSQL 18.7, bootstrap
   (`migrate bootstrap`), as migrations (`migrate up`), RabbitMQ e o worker.
2. `docker compose --profile seed run --rm --build seed`: dados de demonstração (dois tenants,
   membros, contas do simulador de pagamentos, catálogo, cliente e assinatura).
3. Espera, com prazo de 5 minutos, até o motor de faturamento do worker emitir a fatura da
   assinatura de demonstração e o lançamento `invoice.issued` no ledger (`ledger_transactions` com
   ao menos uma linha). Sem lançamento, o job falha: a conferência do lançamento de referência não
   pode ser dispensada.
4. `make backup` com o worker em execução, que continua gravando na origem durante o dump (por
   exemplo, o agendador em `scheduled_jobs`); o container é resolvido por
   `docker compose ps -q postgres`.
5. `make restore-check`: restauração num banco novo do mesmo servidor e todas as conferências da
   seção [O que a verificação confere](#o-que-a-verificação-confere) (contagens de todas as
   tabelas, estrutura de RLS e gatilhos, somas e conteúdo do ledger, lançamento de referência em
   detalhe e isolamento com `recorvia_app`).
6. Cópia do backup com um byte acrescentado ao arquivo de dump: `make restore-check DUMP=<cópia>`
   precisa falhar (soma SHA-256 divergente).
7. Recuperação num **segundo servidor** PostgreSQL vazio (projeto do compose `recorvia-dr`, com
   volume e rede próprios), seguindo os passos 1 a 6 da
   [recuperação de desastre](#recuperação-de-desastre): `migrate bootstrap`, `sha256sum -c`,
   `pg_restore --exit-on-error --single-transaction` dentro do container, `migrate up` depois da
   restauração (sem migrations a aplicar), `make backup` no servidor novo com o manifesto
   comparado por `diff` ao manifesto da origem e `make restore-check DUMP=<backup da origem>`
   contra o servidor novo.

A saída dos scripts fica no log do job; os manifestos (contagens e somas de verificação, sem dados
de clientes) e os logs dos containers são publicados como artefato `backup-restore` por 14 dias.
Os arquivos de dump não são publicados.

### Teste de integração (job `go-integration`)

`TestBackupAndRestoreCheckScripts` (`internal/pgstore`, tag `integration`, parte de
`make test-integration`) executa os scripts contra um PostgreSQL 18.7 iniciado pelo
Testcontainers (mesma imagem do compose), depois do bootstrap e das migrations:

1. Dados criados pelos serviços reais: dois tenants, cada um com proprietário, plano mensal de
   R$ 123,45, três clientes e três assinaturas; o motor de faturamento emite as seis faturas e os
   lançamentos `invoice.issued` no ledger (débito em `accounts_receivable`, crédito em
   `billed_revenue`).
2. `bash deploy/backup/backup.sh` com `RECORVIA_PG_CONTAINER` apontando para o container,
   **enquanto outra conexão grava continuamente** registros de auditoria na origem. O teste
   confere que a origem terminou com mais registros do que o manifesto (escrita depois do
   snapshot) e que, ainda assim, a restauração é idêntica ao manifesto (dump e manifesto no mesmo
   snapshot), além das permissões `0600` dos arquivos e do lançamento de referência balanceado.
3. `bash deploy/backup/restore-check.sh <dump>`, com as mensagens de soma conferida, isolamento
   conferido e restauração verificada.
4. Um manifesto adulterado (uma fatura a mais) é rejeitado pela verificação.
5. Nenhum banco `recorvia_restore_check_*` permanece no servidor.

Com `RECORVIA_BACKUP_EXPORT_DIR=<diretório>`, o teste guarda uma cópia do backup gerado, útil para
repetir a recuperação manualmente noutro servidor.

### Formato da saída

```text
$ make backup
==> exportando snapshot de recorvia (container <id>)
==> pg_dump -Fc --snapshot=<snapshot>
==> calculando o manifesto no mesmo snapshot
==> backup concluído: backups/recorvia-<instante>.dump (<bytes> bytes; <n> tabelas, <m> linhas)
backups/recorvia-<instante>.dump

$ make restore-check
==> soma SHA-256 conferida
==> criando o banco de verificação recorvia_restore_check_<instante>_<pid>
==> pg_restore --exit-on-error --single-transaction (backups/recorvia-<instante>.dump)
==> comparando o manifesto
==> isolamento conferido com recorvia_app: 0 partidas sem tenant; <k> partidas do lançamento com o tenant
==> lançamento de referência <id> (invoice.issued): <k> partidas, débitos <valor>, créditos <valor>
==> restauração verificada em recorvia_restore_check_<...>: <n> tabelas e <m> linhas iguais ao manifesto; ledger idêntico
```

Qualquer diferença entre o banco restaurado e o manifesto é impressa como `diff` e encerra a
verificação com erro (`o banco restaurado difere do manifesto do backup`).

### Verificação local nesta etapa

No ambiente de desenvolvimento desta etapa o Docker não estava disponível. Localmente, os scripts
foram verificados apenas quanto à sintaxe (`bash -n`) e o teste de integração quanto à compilação
(`go vet -tags integration`); a execução do procedimento acontece nos jobs `backup-restore` e
`go-integration` da CI, cujo resultado é a referência.

### Não exercitado

- Troca da API e do worker para o banco restaurado e conciliação da janela entre o backup e a
  restauração: o exercício não inicia a aplicação contra o servidor novo.
- Uso, depois da restauração, das credenciais de provedores cifradas com chaves guardadas à parte
  do backup (a decifragem só acontece quando a aplicação usa a conta do provedor).
- Backup de bancos grandes (tempo, espaço e impacto na produção).

## Recuperação de desastre

Para restaurar num servidor novo, na ordem. Os passos 1 a 6 são exercitados pelo job
`backup-restore` da CI com os dados de demonstração (seção [anterior](#job-backup-restore-da-ci),
sem `GET /readyz` nem as consultas do runbook de isolamento, que o `restore-check` substitui pela
conferência da RLS com `recorvia_app`); o passo 7 e os pontos de atenção são recomendação.

1. Subir o PostgreSQL na mesma versão principal (18) ou superior.
2. Executar o bootstrap (`cmd/migrate bootstrap`), que cria os papéis, o banco vazio e os
   privilégios de banco e de schema. Papéis e privilégios de banco não fazem parte do `pg_dump`.
   **Não** execute `migrate up` antes da restauração: o dump já contém o esquema e a tabela de
   versões das migrations.
3. Conferir a soma: `sha256sum -c recorvia-<instante>.sha256`.
4. Restaurar como superusuário:
   `pg_restore -U postgres -d recorvia --exit-on-error --single-transaction recorvia-<instante>.dump`
   (dentro do container: `docker exec -i <container> pg_restore ... < arquivo.dump`).
5. Executar `migrate up` para aplicar migrations posteriores ao backup, se houver.
6. Conferir com `restore-check` (de preferência antes, num banco de verificação) e, depois, com
   `GET /readyz` e as consultas do runbook de [isolamento](runbooks/isolamento-de-dados.md).
7. Reapontar a API e o worker. Os eventos da outbox não publicados no momento do backup são
   publicados de novo; os consumidores deduplicam pelo inbox.

Pontos de atenção:
- **Chaves de cifragem**: credenciais de provedores ficam cifradas com
  `RECORVIA_ENCRYPTION_KEYS`, que não estão no banco. Guarde as chaves (todas as versões ainda em
  uso) separadamente do backup; sem elas, as contas de provedor precisam ser reconfiguradas.
- **Dados fora do PostgreSQL**: sessões e limites de taxa (Redis) são descartáveis (os usuários
  fazem login de novo); filas do RabbitMQ são reconstruídas a partir da outbox e dos provedores; o
  realm do Keycloak local vem da imagem, mas um Keycloak de produção tem backup próprio.
- **Estado após a restauração**: cobranças criadas no provedor depois do instante do backup não
  existirão localmente; execute a conciliação da janela entre o backup e a restauração para
  registrá-las (runbook de [conciliação](runbooks/conciliacao.md)).

## Recomendações para produção (não exercitadas)

- **Frequência e retenção**: backup lógico diário, com retenção definida pela organização (por
  exemplo, 7 diários, 4 semanais e 12 mensais) e `make restore-check` (ou equivalente) em cada
  cópia ou ao menos semanalmente, com registro do resultado.
- **Recuperação para um instante**: o backup lógico só recupera o instante do dump. Para perda
  máxima de minutos, use arquivamento contínuo de WAL com backup físico (por exemplo,
  `pg_basebackup` e uma ferramenta de gestão de WAL) ou o serviço gerenciado do provedor de nuvem.
- **Armazenamento**: cópias fora do servidor do banco, cifradas em repouso, com acesso restrito a
  quem opera o banco (os arquivos contêm dados pessoais e financeiros de todos os tenants) e
  registro de acesso.
- **Execução**: em janela de menor carga; o `pg_dump` mantém um snapshot aberto durante a cópia, o
  que retém versões antigas de linhas (vacuum) enquanto durar.
- **Teste de recuperação**: ao menos um exercício completo por semestre num servidor novo
  (incluindo bootstrap, chaves de cifragem e troca da aplicação), com tempo medido.
