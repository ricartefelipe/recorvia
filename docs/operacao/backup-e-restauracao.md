# Backup e restauração do PostgreSQL

Procedimento de cópia lógica do banco `recorvia` e de verificação por restauração (ADR-055) e,
em [Backup contínuo e recuperação para um ponto no tempo](#backup-contínuo-e-recuperação-para-um-ponto-no-tempo),
o arquivamento contínuo de WAL com backups base cifrados e a restauração até um instante escolhido
(ADR-087). Este documento separa o que **é exercitado** (onde, como e o que é conferido) do que é
**recomendação** para ambientes com dados reais e não é exercitado.

O procedimento é exercitado pela CI, a cada push e pull request para `develop` e `main`: pelo job
`backup-restore` (ambiente do `docker compose`, alvos `make backup` e `make restore-check` e
recuperação num servidor novo), pelo job `disaster-recovery` (`make dr-check`: recuperação de ponta
a ponta, com a API e o worker religados sobre o banco restaurado e as chaves de cifragem guardadas
à parte do backup, ADR-064), pelo job `pitr-check` (`make pitr-check`: recuperação para um ponto no
tempo a partir do backup contínuo, ADR-087) e pelo teste de integração
`TestBackupAndRestoreCheckScripts` (job `go-integration`). Os detalhes estão em [O que é exercitado](#o-que-é-exercitado); a sequência de
comandos de uma recuperação real, no [runbook](runbooks/recuperacao-de-desastre.md).

## Componentes

| Arquivo | Função |
|---|---|
| `deploy/backup/backup.sh` | `make backup`: `pg_dump -Fc` no container, manifesto no mesmo snapshot, soma SHA-256 |
| `deploy/backup/restore-check.sh` | `make restore-check`: restaura num banco novo, compara o manifesto, confere o isolamento e remove o banco |
| `deploy/backup/common.sh` | funções comuns e consultas do manifesto |
| `deploy/backup/dr-check.sh` | `make dr-check`: exercício de recuperação de desastre de ponta a ponta num projeto isolado do compose (ADR-064) |
| `deploy/backup/dr.compose.yaml` | ambiente isolado do exercício: sem portas fixas no host, imagens com etiqueta própria e receptor de webhooks |
| `tools/hooksink` | receptor de webhooks do exercício (faz o papel do sistema do tenant; não faz parte do produto) |
| subcomando `keyring-check` da API e do worker | conferência das chaves de cifragem contra o banco, sem alterar dados (ADR-063) |
| `.github/workflows/ci.yml`, job `backup-restore` | exercício do procedimento completo no ambiente do compose |
| `.github/workflows/ci.yml`, job `disaster-recovery` | `make dr-check` num runner limpo, com o relatório publicado como artefato |
| `deploy/postgres/`, `cmd/pgbackup` | backup contínuo do PostgreSQL com o WAL-G: [componente](#componente) (ADR-087) |
| `deploy/backup/pitr-restore.sh`, `deploy/backup/pitr-check.sh`, `deploy/backup/pitr.compose.yaml` | `make pitr-restore` e `make pitr-check`: [recuperação para um instante](#recuperação-para-um-instante) e o [exercício](#exercício-make-pitr-check-job-pitr-check) |
| `deploy/backup/keycloak-export.sh` | `make keycloak-export`: exportação cifrada do realm do Keycloak ([Keycloak](#backup-do-keycloak)) |
| `internal/pgstore/backup_integration_test.go` | exercício dos scripts com escrita concorrente e manifesto adulterado (`make test-integration`) |
| `internal/pgstore/backup_script_test.go` | `backup.sh` com um `docker` falso: manifesto incompleto (prazo esgotado, sessão encerrada ou linhas ausentes) não é publicado (`make test`) |
| `internal/pgstore/keyring_integration_test.go`, `internal/app/keyring_integration_test.go` | conferência do keyring contra um banco real (chave ausente, chave com outro valor, rotação, dados fora de uso) e reconfiguração do provedor com a chave perdida (`make test-integration`) |

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
| `RECORVIA_BACKUP_MANIFEST_TIMEOUT` | `1800` | espera máxima, em segundos, por cada linha do manifesto (as somas do ledger percorrem todas as partidas e crescem com o banco) |
| `RECORVIA_RESTORE_DATABASE` | `recorvia_restore_check_<instante>_<pid>` | nome do banco de verificação |
| `RECORVIA_RESTORE_KEEP` | `0` | `1` mantém o banco restaurado para inspeção |

## Uso

```bash
make up                     # ambiente em execução
make backup                 # backups/recorvia-<instante UTC>.{dump,manifest,sha256}
make restore-check          # restaura o backup mais recente num banco novo e confere
make restore-check DUMP=backups/recorvia-20261118T120000Z.dump
make dr-check               # exercício de recuperação de desastre num projeto isolado (não usa o ambiente do make up)
make pitr-check             # exercício de recuperação para um ponto no tempo (backup contínuo, MinIO) num projeto isolado
make pitr-restore TARGET=2026-11-22T10:15:00Z   # restauração até um instante, com o componente deploy/postgres
make keycloak-export        # exportação cifrada do realm do Keycloak para o destino do backup contínuo
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

O backup só é publicado com o manifesto completo: a sessão do snapshot precisa devolver a
sentinela `end-of-manifest` (cada linha dentro de `RECORVIA_BACKUP_MANIFEST_TIMEOUT` segundos) e o
manifesto precisa ter as linhas obrigatórias (contagens de tabelas, `goose_version`, `rls_tables`,
`policies`, `triggers`, `ledger_transactions_md5` e `ledger_entries_md5`; `ledger_balance` e
`ledger_sample` só existem com lançamentos). Prazo esgotado, sessão encerrada antes da sentinela
ou linha ausente encerram o backup com erro (`manifesto incompleto`), sem publicar nenhum arquivo.

Os três arquivos são criados com permissão `0600` (`umask 077`). Arquivos parciais
(`*.partial`) são removidos em caso de falha.

### O que a verificação confere

1. Soma SHA-256 do arquivo e presença das linhas obrigatórias no manifesto do backup.
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

### Exercício de recuperação de desastre (`make dr-check`, job `disaster-recovery`)

`make dr-check` (`deploy/backup/dr-check.sh`, ADR-064) executa a recuperação de ponta a ponta, com
a aplicação real, num projeto do `docker compose` separado do ambiente local (padrão
`recorvia-drcheck`): PostgreSQL, Redis e RabbitMQ sem portas no host; os serviços consultados pelo
exercício publicam uma porta livre escolhida pelo Docker, só em `127.0.0.1`; as imagens levam a
etiqueta `dr`, sem substituir as do ambiente local. Na CI, o job `disaster-recovery` roda o mesmo
alvo a cada push e pull request para `develop` e `main`.

Papéis no exercício: o simulador de pagamentos é o provedor, o Mailpit é o servidor de e-mail, o
Keycloak é o provedor de identidade e o `hooksink` é o sistema do tenant que recebe webhooks.
Esses quatro são **externos** ao desastre e continuam no ar; o que se perde é o banco, o broker,
o Redis, a API e o worker.

1. **Origem.** Sobe o ambiente com a chave de cifragem `dr1` e carrega o seed (duas organizações;
   as credenciais das contas de provedor do seed ficam cifradas com `dr1`).
2. **Rotação em duas fases e dados pela API.** A chave `dr2` entra no cofre e é distribuída à API
   e ao worker, ainda com `dr1` ativa (fase 1; o `keyring-check` dentro de cada container confirma
   as chaves do processo). Na fase 2, `dr2` passa a ser a ativa **primeiro só na API**: o trabalho
   da origem acontece com a API cifrando com `dr2` e o worker ainda na fase 1, como num reinício
   real, que nunca é atômico. Depois de um login OIDC real, com uma credencial de API da
   organização `acme`, o exercício configura a conta do provedor (chave de API e segredo do
   webhook, cifrados com `dr2`), cria um endpoint de webhook (segredo cifrado com `dr2`), um
   convite (token cifrado com `dr2`), produto, plano, cliente e assinatura. O worker da fase 1
   envia o e-mail do convite, cria a cobrança no provedor e entrega os webhooks assinados, e o
   exercício confere que nenhuma notificação ou entrega foi encerrada por chave desconhecida; só
   então o worker é reiniciado com `dr2` ativa. Com o servidor de e-mail fora do ar, a notificação
   da fatura fica pendente, com o conteúdo cifrado no banco. As entregas de webhook da origem têm
   a assinatura conferida com `openssl`, com o segredo devolvido na criação do endpoint.
3. **Backup com trabalho pendente.** Com o worker parado, a API cria outro cliente e outra
   assinatura (eventos pendentes na outbox e assinatura ainda sem fatura) e `deploy/backup/backup.sh`
   gera o dump, o manifesto e a soma. O exercício confere que o arquivo de chaves do cofre tem
   permissão `0600`, que nenhuma chave está no diretório do backup e que o conteúdo do dump
   (`pg_restore -f -`) não traz as chaves, as chaves de API do provedor, os segredos de webhook nem
   o token da credencial de API em claro. Cada segredo é procurado como texto (colunas `text` e
   `jsonb`) e como bytes em hexadecimal, que é como o `pg_dump` escreve as colunas `bytea`, onde
   ficam os dados cifrados; as chaves e o segredo do endpoint são procurados também pelos bytes
   que o base64 representa. Três controles positivos provam que a varredura enxerga o que
   procura: um dado conhecido de uma coluna de texto, o segredo cifrado do endpoint lido do banco
   em hexadecimal e um segredo em claro numa linha `bytea` de teste.
4. **Janela perdida e desastre.** O worker volta: emite a fatura da segunda assinatura e cria a
   cobrança no provedor; a primeira fatura é paga no provedor e confirmada pelo webhook. Em
   seguida, a API, o worker, o PostgreSQL, o Redis e o RabbitMQ são removidos com os volumes. Daí
   em diante, as chaves só existem no cofre.
5. **Restauração.** Redis e RabbitMQ (sem dados a recuperar) e o servidor de e-mail sobem em
   paralelo. Num PostgreSQL novo e vazio: `migrate bootstrap`, `sha256sum -c`,
   `pg_restore --exit-on-error --single-transaction`, `migrate up`, `restore-check` (banco de
   verificação, manifesto, isolamento com `recorvia_app`), manifesto do banco restaurado idêntico
   ao do backup e ledger balanceado (nenhum lançamento desbalanceado; débitos iguais a créditos
   por moeda).
6. **Chaves.** Antes de qualquer processo subir: `keyring-check` com outro valor para `dr1` e sem
   `dr1` termina com erro que cita a chave e a finalidade; o worker com a chave errada e a API sem
   a chave se recusam a iniciar (`A aplicação não foi iniciada`); sem `dr1`, mas com ela
   declarada perdida (`RECORVIA_ENCRYPTION_LOST_KEYS=dr1`), o `keyring-check` aceita o banco e avisa
   o que depende da chave. O manifesto do banco continua idêntico ao do backup depois das cinco
   execuções, e a notificação pendente continua agendada. Com as chaves lidas do cofre, o
   `keyring-check` responde `keyring compatível com o banco`.
7. **API e conciliação, com o worker parado.** A API sobe com as chaves do cofre: `GET /readyz`
   200 (PostgreSQL e Redis), a credencial de API emitida antes do backup continua valendo, a
   sessão anterior é recusada (o Redis se perdeu) e um novo login funciona. A conciliação é pedida
   pela API (`POST /api/v1/reconciliation/runs`, que registra a execução e responde 202) e executada
   pelo subcomando `reconcile-pending` do worker, que termina sem iniciar o agendador, os consumidores
   nem o publicador da outbox. Ela aponta as duas diferenças da janela: `payment_not_reflected`
   (fatura paga no provedor), resolvida com `accept_provider`, e `missing_local` (cobrança criada na
   janela, sem registro local), cancelada no provedor e resolvida com `dismiss`.
8. **Worker religado.** `GET /readyz` do worker 200 (PostgreSQL e RabbitMQ). O exercício confere:
   - a segunda assinatura recebe **uma** fatura e o provedor fica com **uma** cobrança aberta para
     ela (a da janela está cancelada);
   - a notificação pendente no backup é enviada **uma** vez (conteúdo cifrado antes do backup,
     decifrado depois);
   - as entregas de webhook posteriores à restauração têm assinatura válida para o segredo que o
     tenant guardou antes do backup, e os eventos pendentes no backup chegam de novo com o mesmo
     `webhook-id` da linha do tempo perdida;
   - os webhooks do provedor das duas organizações são autenticados com os segredos restaurados
     (uma conta cifrada com `dr2`, outra com `dr1`) e as faturas ficam pagas; a cobrança nova foi
     criada no provedor com a chave de API decifrada;
   - depois de mais dois ciclos dos agendamentos `billing.run_cycle` e
     `payments.recover_operations`, faturas, cobranças, pagamentos e lançamentos continuam iguais, o
     provedor não recebeu cobranças novas, o ledger está balanceado, não há dead letters nem mais
     de uma cobrança em aberto por fatura.

Ao final, o projeto é derrubado com os volumes (`down -v` do próprio projeto), o cofre é apagado e
ficam em `backups/dr-check/` (fora do Git): `relatorio.md` (tempos medidos por etapa, linhas
perdidas na janela e conferências), os manifestos (`backup`, `origem-no-desastre`, `restaurado`),
as saídas do `keyring-check` e das recusas, o inventário da janela e os logs dos containers. Na CI, esses
arquivos são publicados como artefato `disaster-recovery` por 14 dias; o dump e as chaves não são
publicados.

O exercício remove coisas, e por isso só mexe no que é dele:
- **Diretório de saída.** `RECORVIA_DR_DIR` precisa ser um diretório próprio do exercício: novo,
  vazio ou criado por uma execução anterior (marcado com o arquivo `.recorvia-dr-check`). O
  diretório dos backups (`backups` ou o `RECORVIA_BACKUP_DIR` de quem chama), a raiz do
  repositório, um diretório acima dela e qualquer diretório com conteúdo sem a marca são
  recusados antes de qualquer alteração. Numa nova execução, os arquivos da anterior (relatório,
  manifestos, efeitos, textos, logs e o backup) vão para `anterior/`, no lugar da penúltima; o
  que não foi gerado pelo exercício fica onde estava.
- **Projeto do compose.** O projeto é derrubado com os volumes no início e no fim. Os containers
  e os volumes do exercício levam o rótulo `recorvia.dr-check`; se o projeto informado já tiver
  containers ou volumes sem esse rótulo (por exemplo, uma restauração manual mantida para
  inspeção com o mesmo nome), o exercício para sem remover nada. `RECORVIA_DR_REUSE_PROJECT=1`
  confirma a remoção. O nome padrão (`recorvia-drcheck`) não coincide com o do segundo servidor
  do job `backup-restore` (`recorvia-dr`).

| Variável | Padrão | Uso |
|---|---|---|
| `RECORVIA_DR_PROJECT` | `recorvia-drcheck` | projeto do compose, **derrubado com os volumes** no início e no fim (o do ambiente local, `recorvia`, é recusado; um projeto com containers ou volumes que o exercício não criou também) |
| `RECORVIA_DR_REUSE_PROJECT` | `0` | `1` confirma a remoção de um projeto existente que o exercício não criou |
| `RECORVIA_DR_DIR` | `backups/dr-check` | relatório, manifestos, logs e o dump do exercício, num diretório **próprio do exercício** (novo, vazio ou de uma execução anterior, cujos arquivos vão para `anterior/`); nunca o diretório dos backups |
| `RECORVIA_DR_IMAGE_TAG` | `dr` | etiqueta das imagens construídas |
| `RECORVIA_DR_BUILD` | `1` | `0` reutiliza as imagens já construídas |
| `RECORVIA_DR_KEEP` | `0` | `1` mantém o ambiente, o cofre e o dump para inspeção |
| `RECORVIA_DR_TIMEOUT` | `300` | espera máxima, em segundos, de cada condição |

Requisitos: Docker com Compose 2.24 ou superior, `curl`, `jq`, `openssl`, `sha256sum`, `base64`,
`od` e `realpath`.

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

### Verificação local

Com Docker disponível, foram executados localmente `make test` (inclui
`TestBackupScriptRejectsIncompleteManifest`, com um `docker` falso), `make test-integration`
(inclui `TestBackupAndRestoreCheckScripts` contra o PostgreSQL 18.7 iniciado pelo
Testcontainers, e os testes do keyring) e `make dr-check`, no projeto isolado `recorvia-drcheck`, com
o resultado registrado em [RPO e RTO observados](#rpo-e-rto-observados). O job `backup-restore`
usa as portas do ambiente local do `docker compose` e não foi executado localmente (portas
compartilhadas com outros projetos); a referência para ele é a CI.

### Não exercitado

- **Recuperação sem as chaves** (chave perdida): o `dr-check` exercita só a conferência com a
  chave declarada perdida (`RECORVIA_ENCRYPTION_LOST_KEYS`), sem subir a aplicação desse jeito. A
  inicialização no modo `enforce` com a chave declarada perdida, a reconfiguração da conta sem
  decifrar as credenciais anteriores e a rotação do segredo do endpoint são cobertas por
  `TestKeyringCheckAgainstRestoredDatabase` (que inclui a troca num único passo quando a chave
  perdida é a ativa) e `TestProviderCredentialsUnreadableAndReconfiguration`
  (`make test-integration`); o caminho completo, com as organizações refazendo as integrações,
  não faz parte do `dr-check`.
- **Conciliação de várias organizações e sem credencial do tenant**: o exercício concilia uma
  organização, com uma credencial de API emitida por ela antes do backup. Numa recuperação real,
  quem opera a plataforma depende de uma credencial de cada organização (ou de um membro dela);
  não existe um meio de plataforma para conciliar todas antes de religar o worker
  ([Conciliação pela API](#conciliação-pela-api-caminho-exercitado)).
- **Rotação com várias réplicas**: a convivência das fases é exercitada com uma API e um worker;
  mais de uma réplica de cada processo e atualizações graduais (rolling update) não são.
- **Linhas da tabela de conciliação manual** que o exercício não produz: cobrança da janela já
  paga e sem registro local, cobrança cancelada, estornada ou contestada no provedor durante a
  janela, fatura anulada na janela e operações de pagamento `pending`, `in_flight` ou `unknown`
  no instante do backup. A consulta prévia ao provedor antes de criar uma cobrança retomada é
  coberta por `TestRecoveryLooksUpProviderBeforeFirstAttempt` (`internal/payments`); o inventário
  das cobranças que já existiam no backup, por `TestRestoreWindowInventoryListsLinkedCharges`
  (`internal/pgstore`, `make test-integration`), e as consultas SQL deste guia são executadas
  contra o esquema por `TestOperationsDocsSQLRunsAgainstSchema`.
- **Asaas**: o provedor do exercício é o simulador. No Asaas, a listagem e o cancelamento das
  cobranças da janela são feitos no painel ou pela API do provedor e não foram exercitados (o
  adaptador ainda não foi exercitado no sandbox: `docs/integracoes/asaas.md`).
- **Régua de cobrança depois da restauração**: as faturas do exercício não vencem durante a
  execução; lembretes e suspensão sobre faturas pagas na janela dependem da conciliação feita
  antes de religar o worker, que é exercitada, mas a régua em si não age no exercício.
- **Escala e infraestrutura**: bancos grandes (tempo, espaço e impacto do `pg_dump` e do
  `pg_restore`), várias réplicas da API e do worker, restauração em outra região, backup do cofre
  de chaves e a troca de DNS ou de balanceador. O arquivamento contínuo de WAL e o banco do
  Keycloak são exercitados pelo `make pitr-check`
  ([seção](#exercício-make-pitr-check-job-pitr-check)), não pelo `dr-check`.
- **Tempo humano**: obter o backup e as chaves, decidir pela restauração, obter a credencial de
  cada organização e conciliar muitas organizações. No exercício, a conciliação cobre uma
  organização e é automática.

## Recuperação de desastre

Para restaurar num servidor novo, na ordem. Os passos 1 a 6 são exercitados pelo job
`backup-restore` da CI (seção [anterior](#job-backup-restore-da-ci), sem `GET /readyz` nem as
consultas do runbook de isolamento, que o `restore-check` substitui pela conferência da RLS com
`recorvia_app`); os passos 1 a 10 são exercitados de ponta a ponta por `make dr-check`
([seção](#exercício-de-recuperação-de-desastre-make-dr-check-job-disaster-recovery)), com os
dados e os limites descritos em [Não exercitado](#não-exercitado). A sequência de comandos está no
[runbook](runbooks/recuperacao-de-desastre.md).

Com o backup contínuo, os passos 1 a 6 são substituídos pela
[recuperação para um instante](#recuperação-para-um-instante) (`make pitr-restore`), sem bootstrap
nem `pg_restore`: a cópia física traz papéis, privilégios e o esquema. Os passos 7 a 10 continuam,
com a janela perdida indo do instante restaurado ao incidente.

1. Subir o PostgreSQL na mesma versão principal (18) ou superior.
2. Executar o bootstrap (`cmd/migrate bootstrap`), que cria os papéis, o banco vazio e os
   privilégios de banco e de schema. Papéis e privilégios de banco não fazem parte do `pg_dump`;
   as senhas dos papéis são as informadas ao bootstrap, não as da origem. **Não** execute
   `migrate up` antes da restauração: o dump já contém o esquema e a tabela de versões das
   migrations.
3. Conferir a soma: `sha256sum -c recorvia-<instante>.sha256`.
4. Restaurar como superusuário:
   `pg_restore -U postgres -d recorvia --exit-on-error --single-transaction recorvia-<instante>.dump`
   (dentro do container: `docker exec -i <container> pg_restore ... < arquivo.dump`).
5. Executar `migrate up` para aplicar migrations posteriores ao backup, se houver.
6. Conferir com `restore-check` (de preferência antes, num banco de verificação) e, depois, com
   as consultas do runbook de [isolamento](runbooks/isolamento-de-dados.md).
7. **Conferir as chaves de cifragem** com o subcomando `keyring-check`, com as chaves do cofre no
   ambiente do serviço, antes de iniciar qualquer processo
   ([Chaves de cifragem](#chaves-de-cifragem-keyring)):
   `docker compose run --rm --no-deps worker keyring-check`. A API e o worker fazem a mesma
   conferência ao iniciar e se recusam a subir com chave errada ou ausente.
8. **Manter o processamento parado.** Não inicie o worker; inicie a API e confira `GET /readyz`,
   mas mantenha-a fora do tráfego de usuários, integrações e provedores (por exemplo, fora do
   balanceador). Sem o worker não há publicação da outbox, consumo de eventos nem tarefas
   agendadas; a API, porém, chama o provedor de forma síncrona
   (`POST /api/v1/invoices/{id}/charges`, `POST /api/v1/payment-operations/{id}/retry` e a
   conciliação). Quem vai conciliar precisa alcançar a API por um endereço restrito (rede de
   operação, VPN ou lista de endereços permitidos).
9. **Conciliar a janela perdida** (do instante do backup ao incidente) contra o provedor, conforme
   [Janela perdida](#janela-perdida), antes de liberar o processamento, organização por
   organização e com uma credencial de cada uma
   ([quem executa](#conciliação-pela-api-caminho-exercitado)): as cobranças criadas no
   provedor na janela e as cobranças que já existiam no backup e mudaram de situação no provedor
   (pagamento, cancelamento, estorno ou contestação), cujos webhooks foram respondidos na linha do
   tempo perdida e não são reenviados.
10. **Liberar o processamento**: iniciar o worker, conferir `GET /readyz` dele e devolver o tráfego
    à API. A régua (`collections.run`) volta a rodar em até 5 minutos sobre as faturas em aberto;
    por isso os recebimentos do passo 9 precisam estar registrados antes. Acompanhe
    `RecorviaPaymentOperationsUnknown` e `RecorviaPaymentOperationsExhausted` até a normalização.

### Janela perdida

O banco restaurado volta ao instante do backup. O que aconteceu entre esse instante e o incidente
se perdeu localmente, mas pode ter tido efeitos externos: cobranças criadas ou pagas no provedor,
e-mails enviados e webhooks entregues aos tenants. **O inbox não deduplica esses efeitos:** inbox e
outbox estão no mesmo banco e voltam ao mesmo instante. Os eventos pendentes no backup são
publicados de novo quando o worker volta e, como o inbox restaurado não registra o consumo feito
depois do backup, são processados de novo; o inbox só evita a repetição de reentregas do que já
estava registrado no instante do backup. Além disso, o motor de faturamento emite de novo as
faturas dos períodos vencidos depois do backup, com novos identificadores, e cria as cobranças
delas com novas referências externas.

#### Conciliação pela API (caminho exercitado)

Com a API no ar sobre o banco restaurado e o worker parado, a conciliação da organização
(`POST /api/v1/reconciliation/runs`, permissão `reconciliation.run`, com `Idempotency-Key`)
consulta o provedor e compara com o banco restaurado; a janela padrão cobre os últimos 30 dias. O
pedido só registra a execução (202, ADR-071). Para executá-la sem religar o worker, rode uma vez o
subcomando `reconcile-pending` com a mesma configuração do worker (por exemplo,
`docker compose run --rm --no-deps worker reconcile-pending`): ele confere as chaves, executa as
conciliações registradas de todas as organizações e termina, sem iniciar o agendador, os consumidores
nem o publicador da outbox. Termina com erro se alguma execução falhar ou ficar sem terminar; nesse
caso, consulte a causa em `GET /api/v1/reconciliation/runs/{id}` antes de seguir. As divergências
(`GET /api/v1/reconciliation/discrepancies?status=open`) trazem o que aconteceu na janela:

| Divergência | Significado na janela | Resolução (`POST .../discrepancies/{id}/resolve`, `reconciliation.resolve`) |
|---|---|---|
| `payment_not_reflected` | cobrança do backup paga no provedor durante a janela | `accept_provider`: o pagamento é aplicado como faria o webhook (cobrança paga, taxa do provedor no ledger, fatura quitada), com os eventos na outbox para quando o worker voltar |
| `missing_local` | cobrança criada no provedor na janela, sem registro local | não paga: cancelar no provedor (painel ou API do provedor) e resolver com `dismiss`, porque a fatura será emitida de novo e receberá outra cobrança; paga: não cancelar, seguir a última linha de [Cobranças criadas na janela](#cobranças-criadas-na-janela) |
| `refund_pending`, `chargeback_pending`, `unknown_operation` e demais | estorno, contestação ou operação sem confirmação | [runbook de conciliação](runbooks/conciliacao.md#divergencias-abertas) |

A conciliação é por organização e usa a conta padrão do provedor: repita para cada organização
com cobranças no período. `make dr-check` exercita as duas primeiras linhas.

**Quem executa e com qual credencial.** As rotas de conciliação são de tenant: exigem
`reconciliation.run`, `reconciliation.read` e `reconciliation.resolve` na organização (papéis de
proprietário, administrador e financeiro). A equipe da plataforma (`platform_staff`) só vê o
catálogo de organizações e não tem como conciliar em nome de um tenant
(`docs/arquitetura/permissoes.md`). Numa recuperação, portanto:

| Quem | Credencial | Observações |
|---|---|---|
| Operação da plataforma, por acordo de suporte com o tenant | credencial de API da organização (`rcv_...`) com as três permissões, emitida por um proprietário ou administrador **antes do incidente** e guardada em cofre | caminho do exercício; as credenciais anteriores ao backup continuam válidas no banco restaurado, e as emitidas na janela perdida deixam de existir |
| Um membro da organização (ou alguém do suporte convidado por ela antes do incidente) | sessão de usuário, pelo login no provedor de identidade | a pessoa precisa alcançar o endereço restrito da API; convites feitos na janela perdida não existem no banco restaurado |
| Operação da plataforma, sem credencial do tenant | nenhuma | não concilia pela API: faz o inventário por SQL (somente leitura) e a conferência no provedor junto com o tenant, pelas seções seguintes, e registra no incidente as organizações que ficaram sem conciliar |

Enquanto a conciliação não termina, a API fica num endereço restrito, fora do tráfego geral.
Liberá-la a todos os usuários de uma organização para que eles mesmos conciliem libera também as
demais operações, inclusive as que chamam o provedor; é uma decisão de quem conduz o incidente.
A conciliação agendada (`payments.reconcile`) roda no worker e, por isso, não serve a esta etapa.
Falta um meio de plataforma para conciliar todas as organizações antes de religar o worker
(limitação registrada na ADR-064 e em [Não exercitado](#não-exercitado)); até lá, combine com
cada tenant, antes de um incidente, a credencial de suporte e onde ela fica guardada.

As seções seguintes descrevem o inventário e a conciliação manual, para conferir o resultado ou
quando a conciliação pela API não puder ser usada (sem credencial da organização ou com as
credenciais do provedor ilegíveis).

#### Inventário local

Inventário (superusuário, banco restaurado):
```sql
-- Instante mais recente registrado (aproxima o instante do backup).
SELECT max(occurred_at) AS ultimo_registro FROM audit_log;
-- Eventos pendentes no backup: serão publicados e consumidos de novo ao religar o worker.
SELECT event_type, count(*), min(occurred_at) AS mais_antigo, max(occurred_at) AS mais_recente
  FROM outbox_events WHERE published_at IS NULL GROUP BY 1 ORDER BY 1;
-- Operações de pagamento não concluídas no backup, por tenant.
SELECT tenant_id, kind, status, attempts, count(*) FROM payment_operations
 WHERE status IN ('pending', 'in_flight', 'unknown') GROUP BY 1, 2, 3, 4 ORDER BY 1;
-- Cobranças locais ainda sem cobrança do provedor (o id é a referência externa a procurar).
SELECT tenant_id, id AS referencia_externa, invoice_id, amount, created_at FROM charges
 WHERE status = 'pending' ORDER BY created_at;
```

#### Cobranças criadas na janela

No provedor, liste as cobranças criadas desde o instante do backup (painel ou API, filtro pela
data de criação), com a referência externa (`externalReference` no Asaas), que é o id local da
cobrança. Para cada uma:

| Situação | Ação antes do passo 10 |
|---|---|
| Referência igual a uma cobrança local com operação `pending`, `in_flight` ou `unknown` | Nenhuma: a recuperação agendada consulta o provedor pela referência antes de criar e vincula a cobrança existente, mesmo para operações sem tentativa registrada (`TestRecoveryLooksUpProviderBeforeFirstAttempt`). Uma operação `pending` que já estava no limite de 6 tentativas não é retomada sozinha: reexecute-a com `POST /api/v1/payment-operations/{id}/retry`, que também consulta antes de criar. Se a cobrança local está cancelada (fatura anulada antes do backup), a execução só vincula a cobrança do provedor, sem `charge.created`; cancele-a no provedor se não foi paga |
| Sem cobrança local correspondente e não paga | Cancelar no provedor: ao religar o worker, a fatura é emitida de novo e recebe outra cobrança, e o provedor ficaria com duas para o mesmo período |
| Sem cobrança local correspondente e paga | Não cancelar nem estornar (o dinheiro recebido não é descartado, ADR-030). Registrar o caso; depois do passo 10, registrar o recebimento na fatura emitida de novo (`POST /api/v1/invoices/{id}/payments`, com a referência do provedor na observação) e cancelar no provedor a nova cobrança aberta para ela |

#### Cobranças que já existiam no backup

Uma cobrança criada antes do backup (operação `succeeded`, vinculada a `provider_charge_id`) pode
ter sido paga, cancelada, estornada ou contestada no provedor durante a janela. O webhook dessa
mudança foi respondido com 200 na linha do tempo perdida e o provedor não o reenvia: no banco
restaurado, a cobrança e a fatura continuam na situação do backup. Ao religar o worker, a régua
trataria como vencida a fatura que o cliente já pagou (lembretes, marcação de vencida e suspensão
da assinatura).

Inventário local (superusuário, banco restaurado):
```sql
-- Cobranças vinculadas ao provedor cuja situação pode ter mudado na janela; compare cada uma com
-- a situação atual no provedor (provider_status é a última situação recebida antes do backup).
SELECT c.tenant_id, c.id AS referencia_externa, c.provider, c.provider_charge_id, c.status, c.provider_status,
       c.provider_status_at, c.invoice_id, i.status AS situacao_fatura, i.amount_due
  FROM charges c JOIN invoices i ON i.tenant_id = c.tenant_id AND i.id = c.invoice_id
 WHERE c.provider_charge_id IS NOT NULL
   AND c.status IN ('awaiting_payment', 'failed', 'canceled', 'paid', 'partially_refunded', 'disputed')
 ORDER BY c.tenant_id, c.created_at;
```

No provedor, liste as cobranças com mudança de situação desde o instante do backup (painel ou
API: filtro pela data do pagamento e, para cancelamentos, estornos e contestações, o histórico de
eventos da cobrança) e cruze pelo id da cobrança no provedor (`provider_charge_id`) ou pela
referência externa. Para cada divergência:

| Situação no provedor | Situação local | Ação antes do passo 10 |
|---|---|---|
| Paga (recebida ou confirmada) | Cobrança `awaiting_payment` ou `failed`; fatura `open`, `partially_paid` ou `uncollectible` | Registrar o recebimento na fatura antes de liberar a régua: `POST /api/v1/invoices/{invoice_id}/payments` (`payments.record_manual`, com `Idempotency-Key`), com o valor recebido no provedor, `method` `other`, `paid_at` igual à data do pagamento no provedor e o `provider_charge_id` na observação. Com o valor integral, a fatura fica quitada e a régua não age sobre ela (um valor menor deixa o saldo restante em aberto; um excedente vira crédito do cliente, pela regra normal). A cobrança local continua `awaiting_payment` e a taxa do provedor não entra no ledger: guarde a relação (cobrança, pagamento manual e cobrança do provedor) para a conciliação e não aplique o recebimento outra vez. Um evento posterior do provedor para a mesma cobrança (estorno, contestação ou reenvio) confirmaria o pagamento de novo como crédito do cliente: trate-o pela conciliação |
| Paga | Cobrança `canceled` (fatura anulada) | A fatura anulada não aceita pagamento e a régua não age sobre ela. Não estornar (ADR-030); registrar o caso para a conciliação: o valor recebido deve virar crédito do cliente, como faria o webhook |
| Cancelada, removida ou estornada integralmente sem pagamento a registrar | Cobrança `awaiting_payment`; fatura em aberto | O link enviado ao cliente não funciona mais e a régua mandaria lembretes com ele. Confirmar com o tenant se a fatura foi anulada na janela: se foi, anulá-la de novo (`POST /api/v1/invoices/{id}/void`, `invoices.void`) antes do passo 10. Se a fatura continua devida, registrar o caso e avisar o tenant antes de liberar a régua, para combinar com o cliente um novo meio de pagamento (o recebimento é registrado depois com `POST /api/v1/invoices/{id}/payments`) |
| Estornada ou contestada (chargeback) | Cobrança `paid`, `partially_refunded` ou `disputed` | A fatura continua quitada e a régua não age sobre ela. Registrar o caso para a conciliação e não solicitar novo reembolso pelo Recorvia antes de conferir o provedor, para não devolver duas vezes. As contestações chegam pelo webhook ou pela conciliação, e os reembolsos são pedidos pela API (`POST /api/v1/charges/{id}/refunds`) depois da conferência |
| Igual à situação local | Qualquer | Nenhuma |

Comunicação com clientes e tenants:
- E-mails ligados aos eventos pendentes no backup e às faturas emitidas de novo (fatura emitida,
  pagamento confirmado, convites, lembretes da régua) são enviados outra vez.
- Webhooks pendentes no backup são entregues de novo com o mesmo `webhook-id` (destinatários que
  deduplicam pelo identificador ignoram a repetição); eventos gerados de novo depois da
  restauração têm identificadores novos.
- Números de fatura emitidos na janela podem ser reutilizados por faturas emitidas de novo, não
  necessariamente para o mesmo cliente.

Avise os tenants afetados conforme a política de incidentes da organização.

Pontos de atenção:
- **Chaves de cifragem**: credenciais de provedores, segredos de webhook e conteúdo de
  notificações ficam cifrados com `RECORVIA_ENCRYPTION_KEYS`, que não estão no banco nem no
  backup. A guarda e a restauração das chaves estão em
  [Chaves de cifragem](#chaves-de-cifragem-keyring).
- **Dados fora do PostgreSQL**: sessões e limites de taxa (Redis) são descartáveis (os usuários
  fazem login de novo); filas do RabbitMQ são reconstruídas a partir da outbox e dos provedores; o
  realm do Keycloak local vem da imagem, mas um Keycloak de produção tem o banco dele no mesmo
  servidor PostgreSQL, coberto pelo backup contínuo ([Keycloak](#backup-do-keycloak)).
- **Perda máxima**: a janela perdida existe porque o backup lógico só recupera o instante do
  dump; com o backup contínuo ([seção](#backup-contínuo-e-recuperação-para-um-ponto-no-tempo)),
  ela se reduz ao segmento de WAL ainda não arquivado (até cerca de 1 minuto), mas a conciliação
  continua necessária para essa janela e para a que vai do instante escolhido ao incidente.

## Backup contínuo e recuperação para um ponto no tempo

O backup lógico recupera o instante do dump. O backup contínuo (ADR-087) recupera **qualquer
instante** desde o backup base mais antigo mantido pela retenção, até alguns segundos antes da
perda do servidor: o PostgreSQL envia cada segmento de WAL completo a um armazenamento
compatível com S3 e um agente faz backups base no intervalo configurado. Os dois são cifrados no
cliente, antes de sair do servidor. A ferramenta é o [WAL-G](https://github.com/wal-g/wal-g)
(justificativa na ADR-087). O backup lógico continua: ele é portável entre versões, serve à
verificação por manifesto e a cópias para fora do armazenamento do backup contínuo.

### Componente

| Arquivo | Função |
|---|---|
| `deploy/postgres/Dockerfile` | imagem `recorvia-postgres`: a oficial `postgres:18.7-alpine` com o WAL-G 3.0.10 compilado do código-fonte (soma SHA-256 do pacote fixada no Dockerfile, dependências conferidas pelo `go.sum` do WAL-G, libsodium estática) e o agente `recorvia-pgbackup`; `make postgres-image` |
| `deploy/postgres/entrypoint.sh` | confere a configuração do backup antes de iniciar o servidor (sem destino ou sem chave, o servidor não sobe) e segue o entrypoint oficial |
| `deploy/postgres/compose.yaml` | serviços `postgres` (arquivamento ligado) e `postgres-backup` (agente), para incluir em outro compose |
| `cmd/pgbackup`, `internal/infra/pgbackup` | agente: `archive_command`, `restore_command`, backup base agendado, retenção, restauração, listagem e métricas |
| `deploy/backup/pitr-restore.sh` | `make pitr-restore`: restauração até um instante num servidor novo, com conferência |
| `deploy/backup/pitr-check.sh`, `deploy/backup/pitr.compose.yaml` | `make pitr-check`: exercício com MinIO num projeto isolado do compose |
| `.github/workflows/ci.yml`, job `pitr-check` | o exercício num runner limpo, com o relatório publicado como artefato |

O servidor roda com `wal_level=replica`, `archive_mode=on`, `archive_timeout=60s`,
`archive_command='recorvia-pgbackup wal-push %p'` e `restore_command='recorvia-pgbackup wal-fetch
%f %p'` (CMD da imagem). O `restore_command` só é usado numa recuperação e devolve erro fatal
(código 126) quando um segmento existe mas não pode ser lido ou decifrado: sem isso, o
PostgreSQL trataria o erro como fim do WAL e seria promovido antes do instante pedido. O agente
roda com a mesma imagem, como usuário `postgres`, lê o diretório de dados pelo mesmo volume
(somente leitura) e conecta ao servidor pela rede do compose com o superusuário.

Comandos do agente (dentro do container `postgres-backup`, que tem a configuração):

| Comando | Uso |
|---|---|
| `recorvia-pgbackup list` | backups base no armazenamento: início, fim, linha do tempo, tamanho e se foram cifrados com a chave configurada (`esta chave`, `outra (<impressão digital>)`) |
| `recorvia-pgbackup backup` | backup base imediato e retenção (depois de trocar a chave, de uma restauração ou antes de uma manutenção) |
| `recorvia-pgbackup check-config` | confere a configuração e mostra o destino e a impressão digital da chave |
| `recorvia-pgbackup restore (--target-time <instante> \| --latest) [--backup <nome>]` | prepara um servidor novo (usado por `pitr-restore.sh`) |
| `recorvia-pgbackup put-object <nome>` / `get-object <nome>` | envia a entrada padrão, compactada e cifrada com a chave do backup, para `extra/<nome>` no destino, e a lê de volta decifrada (exportação do Keycloak); entrada vazia não é enviada |

#### Como incluir noutro compose

O arquivo não tem caminhos relativos (a imagem é construída à parte, com `make postgres-image` ou
`docker build -f deploy/postgres/Dockerfile -t recorvia-postgres:<etiqueta> .` na raiz do
repositório) e usa os mesmos nomes de serviço e de volume do compose da aplicação. Duas formas:

```bash
# Sobreposição: o serviço postgres do compose passa a usar a imagem e a configuração do componente,
# e o agente é acrescentado.
docker compose -f compose.yaml -f deploy/postgres/compose.yaml up -d
```
```yaml
# extends, num compose próprio (por exemplo, o de produção). Volumes, redes e depends_on ficam no
# compose que inclui; a coleta recorvia-backup do Prometheus encontra o agente pelo nome
# postgres-backup na mesma rede.
services:
  postgres:
    extends: { file: ../postgres/compose.yaml, service: postgres }
  postgres-backup:
    extends: { file: ../postgres/compose.yaml, service: postgres-backup }
    depends_on:
      postgres: { condition: service_healthy }
volumes:
  postgres-data:
```

A imagem precisa ser a mesma (versão e variante alpine) no servidor de origem e no de
restauração: o backup base é uma cópia física, que só volta para a mesma versão principal do
PostgreSQL e a mesma biblioteca de ordenação de texto. Trocar a imagem de um servidor existente
(por exemplo, da oficial para a `recorvia-postgres`) não exige migração dos dados, porque a base
é a mesma `postgres:18.7-alpine`.

### Configuração

Os segredos ficam fora do código e do repositório: no ambiente do compose (`--env-file` fora do
repositório) ou em arquivos montados pelo orquestrador, com as variáveis `*_FILE` no lugar das que
trazem o valor.

| Variável | Padrão | Uso |
|---|---|---|
| `RECORVIA_BACKUP_S3_PREFIX` | obrigatória | destino, `s3://bucket[/caminho]`; um prefixo por servidor (dois servidores no mesmo prefixo são recusados pelo WAL-G na primeira colisão) |
| `RECORVIA_BACKUP_S3_ENDPOINT` | vazio (AWS) | endereço do serviço compatível com S3 (MinIO, provedores de nuvem) |
| `RECORVIA_BACKUP_S3_REGION` | `us-east-1` | região |
| `RECORVIA_BACKUP_S3_FORCE_PATH_STYLE` | `false` | `true` para MinIO e serviços sem endereçamento por subdomínio |
| `RECORVIA_BACKUP_S3_ACCESS_KEY_ID[_FILE]`, `RECORVIA_BACKUP_S3_SECRET_ACCESS_KEY[_FILE]` | vazias | credencial do armazenamento; vazias, vale a do ambiente (papel da instância) |
| `RECORVIA_BACKUP_ENCRYPTION_KEY[_FILE]` | obrigatória | chave de cifragem do backup: 32 bytes aleatórios em base64 (`openssl rand -base64 32`) |
| `RECORVIA_BACKUP_BASE_INTERVAL` | `24h` | intervalo entre backups base (agente) |
| `RECORVIA_BACKUP_RETRY_INTERVAL` | `15m` | espera depois de uma tentativa (agente) |
| `RECORVIA_BACKUP_RETAIN_FULL` | `7` | backups base mantidos, no mínimo (agente) |
| `RECORVIA_BACKUP_RETAIN_DAYS` | `7` | dias de backups mantidos, no mínimo; `0` usa só a contagem (agente) |
| `RECORVIA_BACKUP_PG_PASSWORD[_FILE]` | a de `POSTGRES_SUPERUSER_PASSWORD` no compose | senha do superusuário usada pelo agente |
| `RECORVIA_BACKUP_METRICS_ADDR` | `:9187` | endereço das métricas e do `/healthz` do agente |

Ajustes avançados do WAL-G (`WALG_UPLOAD_CONCURRENCY`, `WALG_COMPRESSION_METHOD`, limites de
banda) podem ser passados no ambiente dos dois serviços; as variáveis de destino, credencial e
cifragem são sempre as traduzidas da configuração acima.

<a id="chave-de-cifragem-do-backup"></a>

### Chave de cifragem do backup

- **Cifragem no cliente.** Backup base e WAL são compactados e cifrados no servidor, antes do
  envio, com a libsodium (secretstream XChaCha20-Poly1305, cifragem autenticada). O armazenamento
  e quem tiver acesso a ele só veem dados cifrados; um arquivo alterado não decifra.
- **Obrigatória.** Sem chave, o servidor não inicia, o `archive_command` falha (o WAL fica no
  servidor e o alerta dispara) e a restauração é recusada: nada é enviado sem cifragem.
- **Guarda.** A chave do backup é guardada como as chaves de cifragem da aplicação
  ([Guarda](#guarda)): fora do armazenamento do backup, em conta e controle de acesso diferentes,
  com cópia que não dependa da infraestrutura do banco. O backup contínuo contém o banco inteiro,
  inclusive os dados cifrados pela aplicação; as chaves da aplicação
  (`RECORVIA_ENCRYPTION_KEYS`) continuam fora dele e são outro segredo, guardado à parte.
  Restaurar exige as duas.
- **Impressão digital.** Cada backup base leva, nos metadados, a impressão digital da chave (16
  dígitos hexadecimais de um HMAC-SHA256, que não revela a chave). A restauração recusa uma chave
  diferente antes de baixar qualquer arquivo (`chave de cifragem do backup diferente da usada no
  backup base ...`); sem a conferência, o WAL-G insistiria por vários minutos em arquivos que não
  decifram. `recorvia-pgbackup list` mostra a situação de cada backup.
- **Troca da chave.** Configure a chave nova nos dois serviços, recrie-os e faça um backup base
  imediato (`recorvia-pgbackup backup`). A recuperação usa uma chave por vez: backups base e WAL
  anteriores à troca exigem a chave anterior, e um instante entre a troca e o fim do backup base
  novo não é recuperável com nenhuma das duas sozinha. Guarde a chave anterior até a retenção
  remover os backups cifrados com ela.

### Retenção e backup base agendado

Na partida e a cada 5 minutos, o agente lê a lista de backups do armazenamento. O backup base
começa imediatamente quando não há nenhum, quando o mais recente está numa linha do tempo
anterior à do servidor (depois de uma restauração para um instante ou de uma promoção) ou quando
o intervalo desde o fim do último venceu; senão, no fim do último mais o intervalo. O agendamento
sobrevive a reinícios do agente, porque se baseia no armazenamento. Depois de cada backup, a
retenção mantém os `RECORVIA_BACKUP_RETAIN_FULL` mais recentes e todos os terminados nos últimos
`RECORVIA_BACKUP_RETAIN_DAYS` dias (o maior dos dois conjuntos) e remove o WAL anterior ao mais
antigo mantido. A janela de recuperação vai do fim do backup base mais antigo mantido até o
último segmento arquivado. Com os padrões, ao menos 7 dias.

### Recuperação para um instante

Num servidor novo, com a imagem `recorvia-postgres`, a configuração do backup e o volume de
dados vazio (a sequência completa, com a aplicação, está no
[runbook](runbooks/recuperacao-de-desastre.md#recuperação-para-um-ponto-no-tempo)):

```bash
make pitr-restore TARGET=2026-11-22T10:15:00Z      # ou TARGET=latest: até o último segmento arquivado
make pitr-restore TARGET=2026-11-22T10:15:00Z MANIFEST=backups/recorvia-20261122T101500Z.manifest
```

`deploy/backup/pitr-restore.sh`:
1. recusa se o servidor estiver em execução e para o agente de backup;
2. num container avulso do serviço `postgres`, recusa um volume com dados (a restauração nunca
   sobrescreve um servidor), escolhe o backup base mais recente terminado antes do instante,
   recusa chave ausente ou diferente da usada nele, extrai o backup e grava `recovery.signal`, o
   instante (`recovery_target_time`) e a promoção ao alcançá-lo;
3. inicia o servidor, que busca o WAL no armazenamento, para antes do primeiro commit posterior
   ao instante e é promovido numa linha do tempo nova; registra o último commit aplicado (log
   `last completed transaction was at log time ...`). Se o WAL até o instante não existir ou não
   decifrar, o servidor termina com erro em vez de subir num ponto anterior;
4. remove o alvo da recuperação da configuração (`ALTER SYSTEM RESET`);
5. confere o banco: grava o manifesto (as mesmas consultas do backup lógico, `common.sh`) em
   `backups/recorvia-pitr-<instante>.manifest`, compara com `MANIFEST` se informado, confere o
   ledger balanceado e a RLS para `recorvia_app`;
6. religa o agente, que faz o backup base da linha do tempo nova.

Conferência com o manifesto lógico: o manifesto de `make backup` descreve o banco no instante do
snapshot do dump. Restaurado até esse instante, o banco é idêntico ao manifesto se nada foi gravado
perto dele; com escrita concorrente, as tabelas gravadas nos milissegundos em volta podem
divergir, e o resultado da comparação deve ser lido tabela a tabela. O exercício registra T com a
escrita parada e exige o manifesto idêntico.

Depois da recuperação, a linha do tempo é a nova: o WAL e os backups seguintes vão para o mesmo
destino, e o WAL-G distingue as linhas do tempo. Uma restauração **de teste** com o servidor de
origem ainda em uso nunca deve usar o mesmo destino: configure outro `RECORVIA_BACKUP_S3_PREFIX`
com leitura do original (ou uma cópia), ou não inicie o servidor restaurado com arquivamento.

<a id="exercicio-pitr"></a>

### Exercício (`make pitr-check`, job `pitr-check`)

`make pitr-check` (`deploy/backup/pitr-check.sh`) executa a recuperação para um instante de ponta a
ponta num projeto do compose separado do ambiente local (padrão `recorvia-pitrcheck`), com as
mesmas proteções do `dr-check` (rótulo `recorvia.dr-check`, diretório próprio, projeto `recorvia`
recusado) e o MinIO como armazenamento compatível com S3. O MinIO e o simulador de pagamentos
fazem o papel de serviços externos e sobrevivem ao desastre. O Keycloak usa um banco no mesmo
servidor PostgreSQL, coberto pelo mesmo backup.

1. **Origem.** Imagem `recorvia-postgres` com a etiqueta `pitr`, agente com retenção de 2 backups
   base; o primeiro backup base é feito pelo agente na partida. Depois do seed, dois backups base
   pedidos ao agente: a retenção remove o primeiro. Os backups levam a impressão digital da chave.
2. **Dados antes de T**, todos depois do último backup base (só existem no WAL): credencial de API
   da organização beta (login OIDC real), fatura em aberto e cobrança criada no provedor pelo
   worker, usuário `pitr-antes-de-t` no Keycloak e a marca `antes-de-t` num banco de conferência.
3. **Instante T**, com o worker parado e a API sem requisições: o manifesto do banco é calculado
   antes e depois de registrar T (relógio do servidor) e precisa ser igual.
4. **Depois de T**: marca `depois-de-t`, pagamento da cobrança no provedor confirmado pelo webhook
   e usuário `pitr-depois-de-t`. O exercício espera o segmento de WAL de T e o da marca seguinte
   chegarem ao armazenamento e mede o atraso do arquivamento; no instante do desastre, lê nas
   métricas do agente a idade do último segmento arquivado e os segmentos pendentes.
5. **Desastre**: PostgreSQL com o volume (e o banco do Keycloak), agente, Keycloak, API, worker,
   Redis e RabbitMQ removidos.
6. **Chave do backup errada e ausente**: `pitr-restore.sh` recusa as duas antes de baixar qualquer
   arquivo, com o volume vazio depois de cada tentativa; sem a conferência da impressão digital,
   o WAL-G com outra chave não extrai o backup base (`failed to extract files`) e o
   `restore_command` devolve 126 (fatal) para um segmento existente, enquanto um segmento
   inexistente devolve 1 (fim do WAL).
7. **Restauração até T** com a chave do cofre: manifesto idêntico ao de T, ledger balanceado, RLS,
   marca `antes-de-t` presente e `depois-de-t` ausente, fatura em aberto, credencial de API
   presente, linha do tempo nova e backup base dela feito pelo agente ao religar.
8. **Aplicação religada** como na recuperação de desastre: `keyring-check` com as chaves do cofre,
   Keycloak sobre o banco restaurado (usuário de antes de T presente, o de depois ausente), API
   com o worker parado, conciliação pela API e `reconcile-pending` (o pagamento feito depois de T
   volta como `payment_not_reflected` e é adotado do provedor) e só então o worker; outbox
   publicada, ledger balanceado, sem pagamento duplicado nem dead letters.

Ao final, o projeto é derrubado com os volumes e o cofre é apagado; ficam em `backups/pitr-check/`
(fora do Git) o relatório com os tempos, os manifestos, as listas de backups, as saídas das
recusas e da restauração e os logs. Na CI, esses arquivos são publicados como artefato
`pitr-check` por 14 dias; nenhum dado nem chave é publicado.

| Variável | Padrão | Uso |
|---|---|---|
| `RECORVIA_PITR_PROJECT` | `recorvia-pitrcheck` | projeto do compose, **derrubado com os volumes** no início e no fim |
| `RECORVIA_PITR_REUSE_PROJECT` | `0` | `1` confirma a remoção de um projeto existente que o exercício não criou |
| `RECORVIA_PITR_DIR` | `backups/pitr-check` | relatório, manifestos e logs, num diretório próprio do exercício |
| `RECORVIA_PITR_IMAGE_TAG` | `pitr` | etiqueta das imagens construídas |
| `RECORVIA_PITR_BUILD` | `1` | `0` reutiliza as imagens já construídas |
| `RECORVIA_PITR_KEEP` | `0` | `1` mantém o ambiente e o cofre para inspeção |
| `RECORVIA_PITR_TIMEOUT` | `300` | espera máxima, em segundos, de cada condição |

<a id="backup-do-keycloak"></a>

### Keycloak

O Keycloak de produção guarda usuários, credenciais, clientes e a configuração do realm no banco
dele. Duas cópias complementares:

- **Banco do Keycloak no mesmo servidor PostgreSQL** (banco `keycloak`, papel `keycloak`,
  `KC_DB=postgres`): fica coberto pelo mesmo backup base e pelo mesmo WAL, e a recuperação para um
  instante traz o Keycloak e o Recorvia **no mesmo instante** (os `sub` das sessões e dos membros
  continuam coerentes). É o arranjo recomendado e o exercitado pelo `make pitr-check`. Com o
  Keycloak num servidor PostgreSQL separado, aplique o mesmo componente a ele, com outro prefixo
  de destino, e restaure os dois no mesmo instante.
- **Exportação do realm** (`make keycloak-export`, `deploy/backup/keycloak-export.sh`): cópia
  lógica, para inspeção, recuperação seletiva ou migração entre versões do Keycloak. Executa
  `kc.sh export --realm recorvia --users same_file` num container avulso do serviço do Keycloak
  (mesma configuração e mesmo banco do servidor em execução) e envia o arquivo pelo agente
  (`recorvia-pgbackup put-object`), que o compacta e cifra com a chave do backup em
  `extra/keycloak/realm-recorvia-<instante>.json` no mesmo destino. O arquivo contém hashes de
  senha e segredos de clientes e não fica no host. Para ler:
  `docker compose exec -T postgres-backup recorvia-pgbackup get-object keycloak/realm-recorvia-<instante>.json > realm.json`;
  a importação, num Keycloak novo, usa `kc.sh import --file` ou o `--import-realm` da imagem.
  A exportação não é agendada (fica fora do agente, que não executa comandos no container do
  Keycloak) e não é removida pela retenção.

### RPO e RTO esperados

| Medida | Esperado | Do que depende |
|---|---|---|
| RPO (perda máxima numa destruição do servidor) | até ~1 min com o arquivamento em dia | `archive_timeout` (60 s) mais o envio do segmento; o que estiver no segmento ainda não arquivado se perde. Um atraso do arquivamento aumenta a perda e é alertado a partir de 10 min (`RecorviaWALArchiveStale`) |
| RPO de uma recuperação para um instante escolhido | zero até o instante | o instante precisa estar na janela da retenção e o WAL até ele, arquivado |
| RTO do banco | extração do backup base mais a reaplicação do WAL desde ele | tamanho do banco, banda até o armazenamento e volume de escrita desde o último backup base (no máximo um intervalo, 24 h por padrão) |
| RTO da aplicação | o do banco mais as etapas da [recuperação de desastre](#recuperação-de-desastre) | conferência das chaves, conciliação da janela entre o instante e o incidente (por organização) e religação do worker |

Os valores observados no exercício estão em [RPO e RTO observados](#rpo-e-rto-observados).

### Fora do backup contínuo

- **Réplica de leitura ou de espera** (*standby*): não há réplica; a recuperação é sempre uma
  restauração. Uma réplica reduziria o RTO a segundos, mas não substitui o backup (erros lógicos
  se replicam).
- **Outra região**: o destino é um único bucket. A cópia para outra região (replicação do bucket
  pelo provedor ou `wal-g copy`) e a restauração lá não são feitas nem exercitadas.
- **Verificação periódica automática das cópias de produção**: o exercício usa dados de
  demonstração; a restauração de uma cópia real num servidor novo continua um procedimento
  periódico manual ([Recomendações](#recomendações-para-produção-não-exercitadas)).
- **Papel de banco com privilégio mínimo para o agente**: o agente usa o superusuário.
- **Exportação periódica do realm do Keycloak**: documentada acima, não agendada.

## Chaves de cifragem (keyring)

O backup não contém as chaves de cifragem: um dump sem as chaves não revela os segredos, e as
chaves sem o dump não servem para nada. As duas coisas precisam existir, em lugares diferentes,
para uma recuperação completa.

### O que depende das chaves

| Dado | Onde fica | Sem a chave |
|---|---|---|
| Chave de API e segredo de webhook das contas de provedor | `payment_provider_accounts` | cobranças, reembolsos, conciliação e webhooks do provedor falham com 503 `provider_credentials_unreadable` até a conta ser configurada de novo |
| Segredos de assinatura dos endpoints de webhook | `webhook_endpoint_secrets` | as entregas se encerram como `exhausted` até o tenant gerar um novo segredo |
| Conteúdo das notificações ainda não enviadas | `notifications` | a notificação se encerra como `failed` (`content_unavailable`) |
| Token do convite no evento `member.invited` pendente | `outbox_events` | o e-mail do convite não é enviado; o convite pode ser refeito |
| Sessões e fluxos de login | Redis | descartáveis: os usuários entram de novo |

Cada dado cifrado guarda o **identificador** da chave usada (`dr1`, `2026a`...). O valor da chave
só existe na configuração (`RECORVIA_ENCRYPTION_KEYS`, lista `identificador:base64` de chaves de
32 bytes) e a chave das novas cifragens é `RECORVIA_ENCRYPTION_ACTIVE_KEY`. Hashes (tokens de
credenciais de API, de links do portal e de convites) não dependem das chaves.

### Guarda

- **Fora do backup.** As chaves ficam num cofre de segredos (gerenciador de segredos do provedor
  de nuvem, HSM ou cofre offline), em armazenamento, conta e controle de acesso diferentes dos do
  dump. Nunca no mesmo diretório, bucket ou servidor do backup, nem no repositório.
- **Identificador e valor.** Guarde o par completo `identificador:valor`, como na variável: o
  identificador gravado nos dados precisa ser o mesmo na restauração.
- **Todas as versões em uso.** Uma chave só sai do cofre quando nenhum dado a referencia. Não há
  recifragem automática: dados antigos continuam com a chave antiga até serem substituídos (conta
  de provedor configurada de novo, segredo de webhook rotacionado) ou encerrados (notificação
  enviada). A consulta de identificadores referenciados está no
  [runbook](runbooks/recuperacao-de-desastre.md#chaves-de-cifragem).
- **Antes de ativar.** Na rotação, a chave nova entra no cofre **antes** de virar a ativa: o que
  for cifrado entre a ativação e a guarda se perde com a chave.
- **Acesso restrito e registrado**, com ao menos uma cópia que não dependa da mesma
  infraestrutura do banco (um desastre que leve o banco não pode levar o cofre).
- **Teste.** Em cada verificação de restauração, confira também as chaves do cofre contra a cópia
  restaurada (`keyring-check`); é a única prova de que o cofre tem o que o backup exige.

### Rotação

A rotação é feita em **duas fases**, porque o reinício nunca é atômico: a API e o worker (e as
réplicas de cada um) reiniciam em momentos diferentes. Um processo que já cifra com a chave nova
enquanto outro ainda não a conhece produz dados que o segundo trata como chave desconhecida, com
efeito sem volta: notificação encerrada como `failed` (`content_unavailable`), e-mail de convite
suprimido, entregas de webhook `exhausted` e sessões ilegíveis entre réplicas da API. Além disso,
um processo antigo que reinicie no meio passa a ser recusado pela conferência da inicialização
(chave ausente).

1. Gerar a chave (`openssl rand -base64 32`) e guardá-la no cofre com um identificador novo.
2. **Fase 1, distribuir.** Acrescentar a chave a `RECORVIA_ENCRYPTION_KEYS` em **todos** os
   processos (API e worker, todas as réplicas), mantendo as anteriores e **sem alterar**
   `RECORVIA_ENCRYPTION_ACTIVE_KEY`; reiniciar todos. Nesta fase ninguém cifra com a chave nova.
3. Conferir que **cada processo em execução** tem a chave nova antes de seguir. O `keyring-check`
   é executado **dentro de cada container em execução**, da API e do worker, réplica por réplica:
   ```bash
   docker compose ps api worker                                 # containers em execução; cada réplica tem um índice
   docker compose exec -T api /app keyring-check
   docker compose exec -T worker /app keyring-check
   docker compose exec -T --index 2 worker /app keyring-check   # repita para cada réplica além da primeira
   ```
   Em todos, a primeira linha precisa trazer a chave nova entre as `chaves configuradas` (em
   ordem alfabética), com a anterior ainda ativa, por exemplo
   `chaves configuradas: k1, k2 (ativa: k1)`. Não avance com algum processo ainda sem a chave
   (por exemplo, uma réplica que não foi recriada).

   `docker compose run --rm --no-deps worker keyring-check` **não serve a esta conferência**: o
   `run` cria um container novo com a configuração atual do compose e mostra a chave nova mesmo
   que um processo em execução ainda esteja com o keyring anterior. O `exec` lê o ambiente do
   container em execução, que é o do processo. Fora do compose, execute `/app keyring-check`
   em cada container em execução, pelo comando equivalente do orquestrador.
4. **Fase 2, ativar.** Apontar `RECORVIA_ENCRYPTION_ACTIVE_KEY` para a chave nova em todos os
   processos e reiniciar. Durante esse reinício convivem processos que cifram com a chave
   anterior e com a nova, e todos decifram as duas. Ao final, a mesma conferência do passo 3
   mostra a chave nova como ativa em todos.
5. As chaves anteriores permanecem na lista e no cofre enquanto o `keyring-check` mostrar dados em
   uso cifrados com elas. A retirada de uma chave também é feita em todos os processos de uma
   vez, e só depois de o `keyring-check` não mostrar mais registros em uso com ela.

As duas fases supõem que a chave ativa anterior continua disponível. Se a chave **perdida** é a
ativa, a fase 1 não é possível (a configuração é recusada: a chave ativa precisa estar na lista);
a troca é feita num único passo, com todos os processos parados, conforme
[Chave perdida](#chave-perdida).

`make dr-check` segue as duas fases, confere as chaves de cada processo com o `keyring-check`
dentro do container em execução e executa o trabalho da origem com a API já na fase 2 e o
worker ainda na fase 1; `TestKeyRotationPhasesAcrossProcesses` (`make test-integration`) mostra
os dois lados: a convivência das fases funciona, e um processo sem a chave nova encerra a entrega
de webhook sem volta e é recusado ao reiniciar.

### Restauração das chaves

1. Obter do cofre todas as chaves em uso no instante do backup e configurá-las no ambiente da API
   e do worker (`RECORVIA_ENCRYPTION_KEYS` e `RECORVIA_ENCRYPTION_ACTIVE_KEY`).
2. Conferir contra o banco restaurado, sem iniciar a aplicação e sem alterar dados:
   ```text
   $ docker compose run --rm --no-deps worker keyring-check
   chaves configuradas: dr1, dr2 (ativa: dr2); organizações conferidas: 2
   FINALIDADE                           CHAVE  REGISTROS  EM USO  NÃO DECIFRAM  RESULTADO
   conteúdo de notificação pendente     dr1    1          1       0             ok
   credencial de provedor de pagamento  dr1    2          2       0             ok
   credencial de provedor de pagamento  dr2    2          2       0             ok
   segredo de endpoint de webhook       dr2    1          1       0             ok
   keyring compatível com o banco
   ```
   O subcomando existe na API e no worker (`/app keyring-check`), usa a configuração do próprio
   serviço, lê as organizações uma a uma com o papel da aplicação e termina com código diferente
   de zero quando algum dado em uso não decifra por engano do keyring (chave ausente ou chave com
   outro valor):
   ```text
   erro: secrets: keyring incompatível com os dados cifrados: chave "dr1" ausente do keyring: 2
   registro(s) de credencial de provedor de pagamento não podem ser decifrados (2 em uso)
   ```
3. Iniciar a API e, depois da conciliação, o worker.

A API e o worker repetem a conferência ao iniciar (`RECORVIA_ENCRYPTION_STARTUP_CHECK`, ADR-063):

| Modo | Comportamento |
|---|---|
| `enforce` (padrão) | com chave ausente ou com outro valor para dado em uso, ou se a conferência não puder ser concluída (falha de leitura do banco, prazo de 5 minutos), o processo registra o erro (chave, finalidade e quantidade) e não inicia |
| `warn` | registra o erro e inicia, inclusive quando a conferência não pôde ser concluída; os dados ilegíveis se degradam item a item, como na tabela acima. Uso: emergência em que a própria conferência impede a inicialização. Desliga a proteção para **todas** as chaves; não é o caminho da chave perdida |
| `off` | não confere |

A recusa existe porque, com a chave errada, o worker encerraria sem volta as notificações
pendentes e as entregas de webhook, e toda chamada ao provedor falharia. A conferência lê uma
notificação pendente por chave e por organização, todas as contas de provedor e todos os segredos
de endpoints. O token cifrado dos convites ainda pendentes na outbox não é conferido (a outbox não
é lida por organização): sem a chave, o e-mail desses convites é suprimido e o convite pode ser
refeito. O custo cresce com o número de organizações (uma transação por organização, a cada
inicialização).

O diagnóstico é **por chave**: basta um registro decifrado, de qualquer finalidade, para o valor
da chave estar comprovado. Em qualquer modo, não impedem a inicialização:
- **dados fora de uso**: segredos de webhook expirados, endpoints removidos e contas de provedor
  desativadas aparecem no relatório com `(fora de uso)`;
- **registros ilegíveis de uma chave comprovada** (dado adulterado, corrompido ou copiado de outro
  contexto): nenhuma correção do keyring os resolve e a recusa deixaria todas as organizações fora
  do ar por um registro de uma delas. Ficam no log em nível de erro e no relatório
  (`registros ilegíveis`, com uma linha `atenção:`), falham item a item e pedem investigação como
  incidente de integridade. Limite: se o registro corrompido for o **único** cifrado com aquela
  chave, nada comprova a chave e o resultado é `chave com outro valor`, que bloqueia;
- **dados de chaves declaradas perdidas** (`RECORVIA_ENCRYPTION_LOST_KEYS`, seção seguinte).

### Chave perdida

Sem a chave não há como recuperar os dados cifrados com ela. O caminho é declarar a perda, por
chave, em `RECORVIA_ENCRYPTION_LOST_KEYS` (lista de identificadores separados por vírgula, sem os
valores; uma chave que está em `RECORVIA_ENCRYPTION_KEYS` não pode ser declarada perdida). Com a
declaração:
- a API e o worker iniciam no modo `enforce` e registram, a cada inicialização, um aviso por
  finalidade com o que ainda depende da chave perdida;
- o `keyring-check` termina com sucesso, mostra as linhas `chave declarada perdida` e um aviso
  `atenção:` para o que está em uso (saída do exercício, sem a chave `dr1`):
  ```text
  chaves configuradas: dr2 (ativa: dr2); declaradas perdidas: dr1; organizações conferidas: 2
  FINALIDADE                           CHAVE  REGISTROS  EM USO  NÃO DECIFRAM  RESULTADO
  conteúdo de notificação pendente     dr1    1          1       1             chave declarada perdida
  credencial de provedor de pagamento  dr1    2          2       2             chave declarada perdida
  credencial de provedor de pagamento  dr2    2          2       0             ok
  segredo de endpoint de webhook       dr2    1          1       0             ok
  keyring compatível com o banco
  atenção: chave "dr1" declarada perdida: 1 registro(s) de conteúdo de notificação pendente não podem ser decifrados (1 em uso); os dados ficam ilegíveis até serem substituídos
  atenção: chave "dr1" declarada perdida: 2 registro(s) de credencial de provedor de pagamento não podem ser decifrados (2 em uso); os dados ficam ilegíveis até serem substituídos
  ```
- **todas as outras chaves continuam conferidas**: um engano de keyring posterior continua
  recusado, o que não acontece no modo `warn`.

Quando a chave perdida é a **ativa**, a instalação precisa de uma chave nova para cifrar, e a
[rotação em duas fases](#rotação) não se aplica: a fase 1 manteria como ativa a chave que não
existe mais, e a configuração é recusada (`RECORVIA_ENCRYPTION_ACTIVE_KEY deve referenciar uma
chave configurada`). Com a API e o worker parados (todas as réplicas), a troca é feita num único
passo: as chaves restantes mais a nova em `RECORVIA_ENCRYPTION_KEYS`, a nova, com um
identificador novo e já guardada no cofre, em `RECORVIA_ENCRYPTION_ACTIVE_KEY`, e a perdida em
`RECORVIA_ENCRYPTION_LOST_KEYS`; o `keyring-check` confere essa configuração antes de qualquer
processo iniciar. Sem processos em execução, nenhum fica com o keyring anterior, que é o risco
que as duas fases evitam.

Os dados cifrados com a chave perdida degradam item a item (tabela de
[O que depende das chaves](#o-que-depende-das-chaves)) até serem refeitos: as contas de provedor
são configuradas de novo (`PUT /api/v1/payment-providers/{provider}`, que não depende das
credenciais anteriores) e os tenants geram novos segredos de webhook. Uma organização que não
refaça as integrações continua com as próprias operações falhando, sem impedir a inicialização
nem afetar as demais; não é preciso desativar contas nem expirar segredos para voltar ao modo
`enforce`, porque a aplicação nunca saiu dele. A sequência está no
[runbook](runbooks/recuperacao-de-desastre.md#chave-perdida). O `dr-check` exercita a conferência
com a chave declarada perdida; a inicialização e a reconfiguração são cobertas por testes de
integração ([Não exercitado](#não-exercitado)).

## RPO e RTO observados

Medições do `make dr-check` (arquivo `backups/dr-check/relatorio.md`; na CI, artefato
`disaster-recovery`). **São observações de um exercício com os dados de demonstração, não
garantias de serviço.**

Execução local de 2026-11-21 (01:52 UTC), numa estação de desenvolvimento com Docker Desktop
(máquina virtual de 12 vCPUs e 7,8 GB), compartilhada com outras suítes de teste em execução
(carga média em torno de 30) e com parte das camadas das imagens em cache:

| Medida | Valor observado |
|---|---|
| Janela perdida (RPO observado: do backup ao desastre) | 45 s |
| Linhas perdidas na janela | 63, em 19 tabelas (1 fatura, 1 cobrança, 1 pagamento, 2 lançamentos no ledger e os eventos, entregas e históricos associados) |
| Duração do backup (dump e manifesto; 68 tabelas, 194 linhas, 389 KB) | 5 s |
| `pg_restore` | 7 s |
| Etapa: servidor novo, bootstrap, `pg_restore`, `migrate up`, `restore-check` e manifesto | 60 s |
| Etapa: conferência do keyring (quatro recusas, chave declarada perdida e as chaves do cofre) | 29 s |
| Etapa: API no ar e pronta | 14 s |
| Etapa: conciliação da janela pela API (uma organização, duas divergências) | 4 s |
| Etapa: worker no ar e pronto | 21 s |
| Do desastre à API pronta sobre o banco restaurado | 103 s |
| **Do desastre ao worker pronto, com a janela conciliada (RTO observado)** | **128 s** |
| Do desastre ao fim das conferências (inclui a espera de dois ciclos dos agendamentos) | 250 s |
| Exercício completo, da construção das imagens à remoção do ambiente | 16 min |

Os tempos variam com a carga da máquina: em execuções anteriores do mesmo roteiro, na mesma
estação e com a máquina mais carregada (carga média acima de 40), o trajeto do desastre ao worker
pronto levou 170 s e, antes de o Redis e o RabbitMQ passarem a subir em paralelo à restauração,
670 s, dos quais 452 s entre o fim da conciliação e o worker pronto, à espera do servidor de
e-mail e do RabbitMQ ficarem saudáveis.

Como ler os números:
- **RPO.** O backup lógico recupera o instante do dump: a perda é tudo o que aconteceu desde o
  último backup. No exercício, a janela é a que o roteiro produz (segundos); em produção, é o
  intervalo entre backups mais o tempo até o incidente (com backup diário, até 24 horas). O
  backup contínuo reduz essa perda ao segmento de WAL ainda não arquivado (até cerca de 1 minuto),
  como medido acima. O que se perde
  localmente não se perde no provedor: a conciliação recupera os pagamentos e identifica as
  cobranças da janela.
- **RTO.** O tempo medido vai da remoção do banco até o worker pronto sobre o banco restaurado,
  com a conferência das chaves e a conciliação de uma organização feitas por script. Não inclui
  detectar o incidente, decidir pela restauração, provisionar o servidor, obter o backup e as
  chaves do cofre nem a conciliação feita por pessoas. O `pg_restore` cresce com o tamanho do
  banco (aqui, poucas centenas de linhas); a conferência do keyring e a conciliação crescem com o
  número de organizações.

### Recuperação para um ponto no tempo (`make pitr-check`)

Execução local de 2026-11-23 (02:55 UTC), na mesma estação, com carga média em torno de 20 e as
imagens construídas na execução (exercício completo, da construção à remoção do ambiente, em 14
min; relatório em `backups/pitr-check/relatorio.md`):

| Medida | Valor observado |
|---|---|
| Atraso do arquivamento do WAL de T (de T ao segmento no armazenamento) | 14 s (em execuções anteriores, de 10 s a 56 s; o limite é o `archive_timeout` de 60 s) |
| Idade do último segmento arquivado no desastre (RPO observado) | 3 s, sem segmentos pendentes |
| Gravado depois de T e descartado pela restauração | 10 tabelas (pagamento, 3 lançamentos no ledger, eventos, inbox e notificações) |
| Extração do backup base (9 MB compactados, MinIO local) | 11 s |
| Recuperação até T (WAL do armazenamento e promoção) | 4 s |
| Roteiro de restauração completo (extração, recuperação e conferências) | 20 s |
| Etapa: conciliação da janela pela API | 3 s |
| **Do desastre ao worker pronto, com a janela conciliada (RTO observado)** | **204 s**, dos quais a maior parte nas tentativas com a chave errada e ausente e na subida do Keycloak sobre o banco restaurado |

## Recomendações para produção (não exercitadas)

- **Frequência e retenção**: backup lógico diário, com retenção definida pela organização (por
  exemplo, 7 diários, 4 semanais e 12 mensais) e `make restore-check` (ou equivalente) em cada
  cópia ou ao menos semanalmente, com registro do resultado.
- **Recuperação para um instante**: use o componente de backup contínuo (`deploy/postgres`,
  ADR-087) e mantenha o backup lógico diário como segunda cópia, independente do formato físico e
  do mesmo armazenamento. Repita a restauração para um instante com uma cópia real ao menos uma vez
  por semestre, com as chaves (do backup e da aplicação) vindas do cofre.
- **Armazenamento**: cópias fora do servidor do banco, cifradas em repouso, com acesso restrito a
  quem opera o banco (os arquivos contêm dados pessoais e financeiros de todos os tenants) e
  registro de acesso.
- **Execução**: em janela de menor carga; o `pg_dump` mantém um snapshot aberto durante a cópia, o
  que retém versões antigas de linhas (vacuum) enquanto durar.
- **Teste de recuperação**: `make dr-check` cobre o produto com dados de demonstração; ao menos
  uma vez por semestre, repita a recuperação com uma cópia real, num servidor novo, com as chaves
  vindas do cofre de produção (`restore-check` e `keyring-check`), e registre o tempo medido.
- **Cofre de chaves**: guarda conforme [Chaves de cifragem](#chaves-de-cifragem-keyring), com
  cópia fora da infraestrutura do banco e restauração testada junto com o backup.
