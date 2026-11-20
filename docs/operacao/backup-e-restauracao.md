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
| `internal/pgstore/backup_script_test.go` | `backup.sh` com um `docker` falso: manifesto incompleto (prazo esgotado, sessão encerrada ou linhas ausentes) não é publicado (`make test`) |

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

Na revisão desta etapa, com Docker disponível, foram executados localmente `make test` (inclui
`TestBackupScriptRejectsIncompleteManifest`, com um `docker` falso) e `make test-integration`
(inclui `TestBackupAndRestoreCheckScripts` contra o PostgreSQL 18.7 iniciado pelo
Testcontainers). O job `backup-restore` usa o ambiente do `docker compose` e não foi executado
localmente (portas compartilhadas com outros projetos); a referência para ele é a CI.

### Não exercitado

- Troca da API e do worker para o banco restaurado e conciliação da janela entre o backup e o
  incidente (passos 7 a 9 da recuperação de desastre): o exercício não inicia a aplicação contra o
  servidor novo. A consulta prévia ao provedor antes de criar uma cobrança retomada é coberta por
  `TestRecoveryLooksUpProviderBeforeFirstAttempt` (`internal/payments`); o inventário das
  cobranças que já existiam no backup, por `TestRestoreWindowInventoryListsLinkedCharges`
  (`internal/pgstore`, `make test-integration`), e as consultas SQL deste guia são executadas
  contra o esquema por `TestOperationsDocsSQLRunsAgainstSchema`.
- Uso, depois da restauração, das credenciais de provedores cifradas com chaves guardadas à parte
  do backup (a decifragem só acontece quando a aplicação usa a conta do provedor).
- Backup de bancos grandes (tempo, espaço e impacto na produção).

## Recuperação de desastre

Para restaurar num servidor novo, na ordem. Os passos 1 a 6 são exercitados pelo job
`backup-restore` da CI com os dados de demonstração (seção [anterior](#job-backup-restore-da-ci),
sem `GET /readyz` nem as consultas do runbook de isolamento, que o `restore-check` substitui pela
conferência da RLS com `recorvia_app`); os passos 7 a 9 e os pontos de atenção são recomendação.

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
7. **Manter o processamento parado.** Não inicie o worker e mantenha a API fora do tráfego de
   usuários, integrações e provedores (por exemplo, fora do balanceador). Sem o worker não há
   publicação da outbox, consumo de eventos nem tarefas agendadas; a API, porém, chama o provedor
   de forma síncrona (`POST /api/v1/invoices/{id}/charges` e
   `POST /api/v1/payment-operations/{id}/retry`).
8. **Conciliar a janela perdida** (do instante do backup ao incidente) contra o provedor, conforme
   [Janela perdida](#janela-perdida), antes de liberar o processamento: as cobranças criadas no
   provedor na janela e as cobranças que já existiam no backup e mudaram de situação no provedor
   (pagamento, cancelamento, estorno ou contestação), cujos webhooks foram respondidos na linha do
   tempo perdida e não são reenviados.
9. **Liberar o processamento**: iniciar o worker e devolver o tráfego à API. A régua
   (`collections.run`) volta a rodar em até 5 minutos sobre as faturas em aberto; por isso os
   recebimentos do passo 8 precisam estar registrados antes. Acompanhe
   `RecorviaPaymentOperationsUnknown` e `RecorviaPaymentOperationsExhausted` até a normalização. A
   conciliação do período pela API (`POST /api/v1/reconciliation/runs`,
   [runbook](runbooks/conciliacao.md)) chega com a etapa 5 (recebíveis e conciliação), ainda não
   integrada; quando disponível, ela roda depois deste passo e não substitui o passo 8.

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

Inventário local (superusuário, banco restaurado):
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

| Situação | Ação antes do passo 9 |
|---|---|
| Referência igual a uma cobrança local com operação `pending`, `in_flight` ou `unknown` | Nenhuma: a recuperação agendada consulta o provedor pela referência antes de criar e vincula a cobrança existente, mesmo para operações sem tentativa registrada (`TestRecoveryLooksUpProviderBeforeFirstAttempt`). Uma operação `pending` que já estava no limite de 6 tentativas não é retomada sozinha: reexecute-a com `POST /api/v1/payment-operations/{id}/retry`, que também consulta antes de criar. Se a cobrança local está cancelada (fatura anulada antes do backup), a execução só vincula a cobrança do provedor, sem `charge.created`; cancele-a no provedor se não foi paga |
| Sem cobrança local correspondente e não paga | Cancelar no provedor: ao religar o worker, a fatura é emitida de novo e recebe outra cobrança, e o provedor ficaria com duas para o mesmo período |
| Sem cobrança local correspondente e paga | Não cancelar nem estornar (o dinheiro recebido não é descartado, ADR-030). Registrar o caso; depois do passo 9, registrar o recebimento na fatura emitida de novo (`POST /api/v1/invoices/{id}/payments`, com a referência do provedor na observação) e cancelar no provedor a nova cobrança aberta para ela |

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

| Situação no provedor | Situação local | Ação antes do passo 9 |
|---|---|---|
| Paga (recebida ou confirmada) | Cobrança `awaiting_payment` ou `failed`; fatura `open`, `partially_paid` ou `uncollectible` | Registrar o recebimento na fatura antes de liberar a régua: `POST /api/v1/invoices/{invoice_id}/payments` (`payments.record_manual`, com `Idempotency-Key`), com o valor recebido no provedor, `method` `other`, `paid_at` igual à data do pagamento no provedor e o `provider_charge_id` na observação. Com o valor integral, a fatura fica quitada e a régua não age sobre ela (um valor menor deixa o saldo restante em aberto; um excedente vira crédito do cliente, pela regra normal). A cobrança local continua `awaiting_payment` e a taxa do provedor não entra no ledger: guarde a relação (cobrança, pagamento manual e cobrança do provedor) para a conciliação e não aplique o recebimento outra vez. Um evento posterior do provedor para a mesma cobrança (estorno, contestação ou reenvio) confirmaria o pagamento de novo como crédito do cliente: trate-o pela conciliação |
| Paga | Cobrança `canceled` (fatura anulada) | A fatura anulada não aceita pagamento e a régua não age sobre ela. Não estornar (ADR-030); registrar o caso para a conciliação: o valor recebido deve virar crédito do cliente, como faria o webhook |
| Cancelada, removida ou estornada integralmente sem pagamento a registrar | Cobrança `awaiting_payment`; fatura em aberto | O link enviado ao cliente não funciona mais e a régua mandaria lembretes com ele. Confirmar com o tenant se a fatura foi anulada na janela: se foi, anulá-la de novo (`POST /api/v1/invoices/{id}/void`, `invoices.void`) antes do passo 9. Se a fatura continua devida, registrar o caso e avisar o tenant antes de liberar a régua, para combinar com o cliente um novo meio de pagamento (o recebimento é registrado depois com `POST /api/v1/invoices/{id}/payments`) |
| Estornada ou contestada (chargeback) | Cobrança `paid`, `partially_refunded` ou `disputed` | A fatura continua quitada e a régua não age sobre ela. Registrar o caso para a conciliação e não solicitar novo reembolso pelo Recorvia antes de conferir o provedor, para não devolver duas vezes. Reembolsos e contestações pela API chegam com a etapa 5 |
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
- **Chaves de cifragem**: credenciais de provedores ficam cifradas com
  `RECORVIA_ENCRYPTION_KEYS`, que não estão no banco. Guarde as chaves (todas as versões ainda em
  uso) separadamente do backup; sem elas, as contas de provedor precisam ser reconfiguradas.
- **Dados fora do PostgreSQL**: sessões e limites de taxa (Redis) são descartáveis (os usuários
  fazem login de novo); filas do RabbitMQ são reconstruídas a partir da outbox e dos provedores; o
  realm do Keycloak local vem da imagem, mas um Keycloak de produção tem backup próprio.
- **Perda máxima**: a janela perdida existe porque o backup lógico só recupera o instante do
  dump; com arquivamento contínuo de WAL (recomendação abaixo), ela se reduz a minutos, mas a
  conciliação continua necessária.

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
