# Runbook: backup contínuo do PostgreSQL

Cobre os alertas do arquivamento contínuo de WAL e dos backups base (ADR-087): o servidor
`recorvia-postgres` envia cada segmento de WAL completo ao armazenamento compatível com S3 pelo
`archive_command` (`recorvia-pgbackup wal-push`), e o agente `postgres-backup`
(`recorvia-pgbackup serve`) faz os backups base no intervalo configurado, aplica a retenção e
exporta as métricas. Tudo é cifrado no cliente com a chave de cifragem do backup. O componente, as
variáveis e a recuperação para um instante estão em
[Backup e restauração](../backup-e-restauracao.md#backup-contínuo-e-recuperação-para-um-ponto-no-tempo);
a restauração, no [runbook de recuperação de desastre](recuperacao-de-desastre.md#recuperação-para-um-ponto-no-tempo).

Os comandos usam os serviços do componente (`postgres` e `postgres-backup`). Os do agente rodam
dentro do container dele, que tem a configuração do backup:
```bash
docker compose exec -T postgres-backup recorvia-pgbackup list     # backups base no armazenamento e a chave de cada um
docker compose exec -T postgres-backup recorvia-pgbackup backup   # backup base imediato e retenção
docker compose logs --since 30m postgres postgres-backup
```

## Arquivamento de WAL atrasado

### Sintomas
- O último segmento de WAL arquivado é antigo: o que foi gravado desde então se perde se o
  servidor for destruído. Com `archive_timeout` de 60 s e o agendador do worker gravando a cada
  minuto, o normal é ao menos um segmento por minuto.
- Log do PostgreSQL com `archive command failed with exit code` e a mensagem do
  `recorvia-pgbackup: arquivamento de ... falhou`.
- O diretório `pg_wal` cresce: o PostgreSQL não recicla segmentos não arquivados. Sem correção,
  o disco do servidor enche e o banco para.

### Métricas e alertas
- `RecorviaWALArchiveStale` (aviso com mais de 10 min, crítico com mais de 30 min):
  `time() - max(recorvia_backup_wal_last_archived_timestamp_seconds)`.
- `RecorviaWALArchiveFailing`: `increase(recorvia_backup_wal_archive_failures_total[10m]) > 0` por
  10 min.
- `recorvia_backup_wal_pending_segments` e `recorvia_backup_wal_oldest_pending_seconds`: quantos
  segmentos esperam e há quanto tempo (o atraso real do arquivamento, isto é, o RPO do momento).

### Diagnóstico
```sql
-- Estado do arquivamento (superusuário).
SELECT archived_count, last_archived_wal, last_archived_time, failed_count, last_failed_wal, last_failed_time
  FROM pg_stat_archiver;
-- Segmentos aguardando arquivamento e o mais antigo.
SELECT count(*), min(modification) FROM pg_ls_archive_statusdir() WHERE name LIKE '%.ready';
-- Escrita recente: sem escrita, não há segmento novo a arquivar (o alerta também dispara com o banco parado).
SELECT pg_walfile_name(pg_current_wal_lsn()), pg_current_wal_lsn();
```
```bash
docker compose logs --since 30m postgres | grep -E 'archive|recorvia-pgbackup'
# Configuração vista pelo servidor (destino e impressão digital da chave; nunca o valor da chave).
docker compose exec -T postgres recorvia-pgbackup check-config
```

| Mensagem no log | Causa provável |
|---|---|
| `configuração do backup inválida` | variável ausente ou inválida no serviço `postgres` (destino, chave, credencial) |
| `AccessDenied`, `InvalidAccessKeyId`, `SignatureDoesNotMatch` | credencial do armazenamento errada, expirada ou sem permissão de escrita no prefixo |
| `no such host`, `connection refused`, `i/o timeout` | armazenamento inacessível (rede, DNS, endpoint) |
| `already archived` / arquivo diferente no destino | outro servidor arquivando no mesmo prefixo (por exemplo, uma restauração de teste apontada para o destino de produção); o WAL-G recusa sobrescrever (`WALG_PREVENT_WAL_OVERWRITE`) |
| `NoSuchBucket` | bucket ou prefixo inexistente |

### Ações
1. Corrija a causa (credencial, rede, destino) no ambiente do serviço `postgres` e recrie o
   container (`docker compose up -d postgres`; o PostgreSQL reinicia e retoma o arquivamento do
   segmento pendente mais antigo). Não apague arquivos de `pg_wal`: são o que falta arquivar.
2. Se outro servidor arquiva no mesmo prefixo, pare-o ou aponte-o para outro destino antes de
   qualquer outra ação: dois servidores no mesmo prefixo misturam linhas do tempo.
3. Se o disco do servidor estiver perto de encher e a causa não puder ser corrigida logo, aumente
   o volume. Desligar o arquivamento (`archive_mode=off`) libera o disco, mas interrompe a
   recuperação para um instante a partir daí: só com decisão registrada no incidente e com um
   backup base novo assim que o arquivamento voltar.
4. Depois de um período longo sem arquivamento, faça um backup base
   (`recorvia-pgbackup backup`): ele encurta a recuperação e não depende do WAL do período.

### Verificação
- `pg_stat_archiver.last_archived_time` avança e `failed_count` para de crescer; nenhum `.ready`
  antigo em `pg_ls_archive_statusdir()`.
- `RecorviaWALArchiveStale` e `RecorviaWALArchiveFailing` normalizam.

## Backup base atrasado

### Sintomas
- `recorvia-pgbackup list` sem backup base recente (o normal é um por intervalo, padrão 24 h).
- Log do agente com `backup base falhou` ou `retenção falhou`.

### Métricas e alertas
- `RecorviaBaseBackupStale`: aviso sem backup novo no intervalo mais 2 h, crítico em dois
  intervalos mais 2 h (`recorvia_backup_base_last_success_timestamp_seconds`, lida do próprio
  armazenamento; zero sem nenhum backup).
- `recorvia_backup_base_runs_total{outcome="failure"}` e `recorvia_backup_base_last_duration_seconds`.

### Diagnóstico
```bash
docker compose exec -T postgres-backup recorvia-pgbackup list
docker compose logs --since 26h postgres-backup | grep -E '"level":"(ERROR|WARN)"|ERROR:'
```
- `outro backup base está em andamento neste container`: um `recorvia-pgbackup backup` manual
  coincidiu com o agendado; aguarde.
- Falha de conexão ou de permissão no PostgreSQL: o agente usa o superusuário
  (`RECORVIA_BACKUP_PG_PASSWORD`, a senha de `POSTGRES_SUPERUSER_PASSWORD`).
- Falha de leitura do diretório de dados: o volume `postgres-data` precisa estar montado no
  agente, no mesmo caminho do servidor (`/var/lib/postgresql`).
- O primeiro backup base de um banco grande pode levar mais que os 15 min do `for` do alerta.

### Ações
1. Corrija a causa e faça um backup base imediato:
   `docker compose exec -T postgres-backup recorvia-pgbackup backup`.
2. Se o backup funciona e só a retenção falha (`retenção falhou`), confira a permissão de remoção
   no prefixo; os backups antigos ficam até a retenção voltar a funcionar (custo de
   armazenamento, sem perda).
3. Depois de trocar a chave de cifragem do backup, faça um backup base imediato
   ([chave do backup](../backup-e-restauracao.md#chave-de-cifragem-do-backup)).

### Verificação
`recorvia-pgbackup list` mostra o backup novo com `esta chave` e
`RecorviaBaseBackupStale` normaliza.

## Agente de backup com falha

### Sintomas
- O agente não lê o armazenamento ou o PostgreSQL (`RecorviaBackupDependencyUnavailable`), não
  responde à coleta (`RecorviaTargetDown` com `job="recorvia-backup"`) ou deixou de existir
  (`RecorviaBackupAgentMissing`).
- Sem o agente, o arquivamento de WAL continua (ele é feito pelo servidor), mas param os backups
  base, a retenção e as métricas do arquivamento: os alertas de WAL atrasado deixam de ser
  avaliados.

### Métricas e alertas
- `recorvia_backup_dependency_up{dependency="storage"|"postgres"}`.
- `up{job="recorvia-backup"}`. A coleta usa a descoberta pelo DNS do compose: um container
  removido some dos alvos; `RecorviaBackupAgentMissing` dispara quando a coleta existiu nos
  últimos 7 dias e está ausente há 10 minutos.

### Diagnóstico
```bash
docker compose ps postgres-backup
docker compose logs --since 30m postgres-backup
docker compose exec -T postgres-backup recorvia-pgbackup check-config
docker compose exec -T postgres-backup recorvia-pgbackup healthcheck && echo agente respondendo
```

### Ações
1. Agente parado ou removido: `docker compose up -d postgres-backup`. Na partida, ele confere o
   armazenamento e faz o backup base se o último estiver vencido ou numa linha do tempo anterior
   à do servidor.
2. `storage` indisponível: mesma análise de credencial e rede da seção
   [Arquivamento de WAL atrasado](#arquivamento-de-wal-atrasado) (o agente e o servidor usam a
   mesma configuração).
3. `postgres` indisponível: senha do superusuário (`RECORVIA_BACKUP_PG_PASSWORD`), servidor em
   recuperação ou fora do ar (`RecorviaDependencyUnavailable` da aplicação acompanha).

### Verificação
`up{job="recorvia-backup"} == 1`, `recorvia_backup_dependency_up` igual a 1 nas duas
dependências e as séries `recorvia_backup_wal_*` presentes.
