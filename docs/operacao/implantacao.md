# Implantação em produção

Guia para colocar o Recorvia no ar num servidor próprio, com o pacote de `deploy/producao/`
(decisões na ADR-086). O pacote não depende de nenhum provedor de hospedagem: precisa só de um
servidor Linux com Docker, de um domínio e de um servidor de e-mail.

## O que sobe

```text
Internet ──80/443──> proxy (Caddy, HTTPS automático)
                      ├── RECORVIA_DOMAIN ──> web (nginx: interface + /api, /auth, /webhooks, /livez) ──> api
                      └── KEYCLOAK_DOMAIN ──> keycloak (login)
api, worker ──> postgres (bancos recorvia e keycloak), redis, rabbitmq      (rede interna, sem internet)
worker ──> SMTP, provedor de pagamento, webhooks dos clientes                (saída para a internet)
```

- Só o proxy publica portas no servidor: 80 e 443 (TCP e UDP). Banco, Redis, RabbitMQ, API e
  Keycloak não ficam acessíveis de fora. No domínio do Keycloak, o proxy só repassa o realm
  `recorvia` (login) e os recursos estáticos; o console, a API de administração e o realm
  `master` só abrem para `KEYCLOAK_ADMIN_ALLOWED_CIDRS` (fechados por padrão).
- Redes: `edge` (proxy, web e Keycloak, com IPv6 e endereços fixos para o proxy), `app` (web,
  API, worker e Keycloak, com saída para a internet) e `data` (banco, Redis e RabbitMQ, sem saída
  para a internet).
- A cada subida, o `migrate-bootstrap` acerta os papéis do banco, o `migrate` aplica as
  migrations e só então a API e o worker iniciam.
- Não há simulador de pagamentos, Mailpit nem dados de demonstração. O realm do Keycloak começa
  vazio.
- O perfil `observability` (opcional) acrescenta Prometheus, Alertmanager, Grafana, Jaeger e o
  OpenTelemetry Collector, com as interfaces só no loopback do servidor.

Arquivos:

| Arquivo | Para que serve |
|---|---|
| `deploy/producao/compose.yaml` | serviços, redes, volumes, limites e healthchecks |
| `deploy/producao/.env.example` | todas as variáveis, explicadas; copie para `.env` |
| `deploy/producao/gerar-segredos.sh` | gera senhas, a chave de cifragem e a chave do backup contínuo com segurança |
| `deploy/producao/caddy/` | imagem do proxy (Caddyfile; Caddy compilado com dependências corrigidas, rodando como usuário sem privilégios com permissão só para abrir 80 e 443) |
| `deploy/producao/keycloak/` | imagem do Keycloak de produção e o realm sem usuários |
| `deploy/producao/verificar.sh` | verificação automatizada (`make prod-check`), para rodar fora da produção |

## Requisitos do servidor

- **Sistema**: Linux de 64 bits (por exemplo, Ubuntu 24.04 LTS ou Debian 13), com acesso SSH por
  chave e atualizações automáticas de segurança.
- **Docker** 27 ou superior, com o plugin Compose 2.24 ou superior, e **Git**. Mantenha o
  `ip6tables` do daemon ligado (padrão do Docker 27 em diante; confira que `/etc/docker/daemon.json`
  não tem `"ip6tables": false`): é ele que entrega as conexões IPv6 ao proxy com o endereço real
  do cliente.
- **Tamanho mínimo**: 4 vCPU, 8 GB de RAM e 80 GB de SSD. Os limites do compose somam cerca de
  7 GB de memória (PostgreSQL 2 GB, Keycloak 1,5 GB, RabbitMQ 1 GB, API, worker e agente de
  backup 512 MB cada, demais menores). Com o perfil `observability`, conte mais 2 GB. Para ajustar os limites, use
  um arquivo compose adicional (veja [Extensões](#extensões)).
- **Rede**: IP público fixo (IPv4, e IPv6 se houver); portas 80 e 443 (TCP e UDP) liberadas no
  firewall de entrada; porta do SSH restrita à operação. Saída liberada para a internet
  (certificados, SMTP, provedor de pagamento, webhooks dos clientes e o armazenamento S3 do
  backup contínuo).
- **Disco**: os logs dos containers giram (5 arquivos de 20 MB por serviço). Monitore o espaço
  livre (veja [O que monitorar](#o-que-monitorar)).
- **Relógio** sincronizado (NTP): tokens do login e assinaturas de webhook dependem dele.

## DNS

Dois nomes, ambos apontando para o IP do servidor (registros `A` e, se o servidor tiver IPv6
público, `AAAA`):

| Variável | Exemplo | Uso |
|---|---|---|
| `RECORVIA_DOMAIN` | `app.example.com` | interface, API, portal do cliente e webhooks dos provedores |
| `KEYCLOAK_DOMAIN` | `auth.example.com` | login (Keycloak) |

Crie os registros **antes** da primeira subida: o Caddy pede os certificados na partida e a
autoridade só os emite se o nome já apontar para o servidor. Se houver CDN ou balanceador à
frente, desligue o proxy dele nesses nomes (somente DNS) ou declare-o em `trusted_proxies` no
`Caddyfile`: o Caddy ignora `X-Forwarded-For` vindo de quem não é confiável.

**IPv6.** A rede `edge` tem IPv6 (sub-rede ULA fixa), e o Docker, com `ip6tables`, encaminha as
conexões IPv6 publicadas direto ao proxy, que vê o endereço real do cliente (limites por IP e
restrição do console do Keycloak). Publique o `AAAA` só depois de conferir isso: de uma rede
IPv6 externa (por exemplo, o celular sem Wi-Fi), abra `https://RECORVIA_DOMAIN/clientes` e veja
`docker compose logs --tail 5 web`. A linha precisa trazer o IPv6 do aparelho; se trouxer um
endereço `10.86.86.1` ou `fd52:6563:6f72:86::1` (o gateway da rede), o `ip6tables` está
desligado no daemon: ligue-o ou **não publique o `AAAA`**. Se as faixas `10.86.86.0/24` ou
`fd52:6563:6f72:86::/64` colidirem com a rede do servidor, troque-as no `.env`
(`RECORVIA_EDGE_SUBNET*` e `RECORVIA_PROXY_IPV*`).

Para o e-mail transacional, configure no domínio do remetente os registros SPF, DKIM e DMARC
indicados pelo provedor de e-mail.

## Variáveis e segredos

No servidor:

```bash
git clone https://github.com/ricartefelipe/recorvia.git /opt/recorvia
cd /opt/recorvia
git checkout v0.3.0                      # a versão a implantar
cd deploy/producao
cp .env.example .env
bash gerar-segredos.sh .env
```

O script:

- troca toda linha `NOME=GERAR` por um valor aleatório de 32 bytes de `/dev/urandom` (senhas em
  hexadecimal, que entram nas URLs de conexão sem escape);
- gera a chave de cifragem (`RECORVIA_ENCRYPTION_KEYS`, no formato `identificador:base64`) e
  aponta `RECORVIA_ENCRYPTION_ACTIVE_KEY` para ela;
- cria a chave de cifragem do backup contínuo em `segredos/backup-cifragem.chave` (pasta 0700,
  arquivo 0444, montado como segredo no PostgreSQL e no agente; fora do `.env`), se ainda não
  existir. O PostgreSQL e o agente rodam como o usuário `postgres` da imagem (uid 70), sem root:
  um arquivo de chave (ou de credencial `*_FILE`) trocado à mão precisa continuar legível por esse
  usuário (0444, ou dono uid 70 com 0400); legível só pelo root, o servidor não inicia;
- não troca valores já preenchidos (rodar de novo é seguro), não imprime segredos e deixa o
  `.env` com permissão 600;
- lista as linhas que ainda estão com valor de exemplo.

Preencha à mão o que o script listou (todas marcadas com `[PREENCHER]` no `.env.example`):

| Variável | O que colocar |
|---|---|
| `RECORVIA_DOMAIN`, `KEYCLOAK_DOMAIN` | os dois nomes do DNS, sem `https://` |
| `ACME_EMAIL` | e-mail que recebe avisos da autoridade certificadora |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD` | servidor de e-mail transacional |
| `SMTP_TLS` e `KEYCLOAK_SMTP_STARTTLS`/`KEYCLOAK_SMTP_SSL` | `starttls` (587) com `true`/`false`, ou `tls` (465) com `false`/`true` |
| `RECORVIA_SMTP_FROM`, `KEYCLOAK_SMTP_FROM` | remetentes das cobranças e do login |
| `RECORVIA_BACKUP_S3_PREFIX`, `RECORVIA_BACKUP_S3_ENDPOINT`, `RECORVIA_BACKUP_S3_ACCESS_KEY_ID`, `RECORVIA_BACKUP_S3_SECRET_ACCESS_KEY` | destino S3 do backup contínuo (bucket só para ele, em conta diferente da do servidor) e a credencial; o PostgreSQL não inicia sem eles ([Backup](#backup)) |
| `KEYCLOAK_ADMIN_ALLOWED_CIDRS` | IPs (CIDR, separados por espaço) da operação ou da VPN que podem abrir o console do Keycloak e o realm `master`. Sem a variável, ninguém abre; liberar para todos exige `0.0.0.0/0 ::/0` escrito de propósito (não recomendado) |

O que a aplicação confere sozinha em produção (`RECORVIA_ENV=production`, já fixado no compose; a
API ou o worker não sobem se algo falhar):

- `RECORVIA_PUBLIC_URL` com `https` (o compose monta `https://RECORVIA_DOMAIN`);
- cookies de sessão e do portal com `Secure` e prefixo `__Host-` (o padrão);
- SMTP do worker com TLS (`starttls` ou `tls`);
- nenhum destino privado de webhook (`RECORVIA_WEBHOOK_ALLOWED_PRIVATE_CIDRS` fica vazio);
- chave de cifragem com 32 bytes e a ativa presente na lista; prazos de retenção nos limites;
- a conferência das chaves contra o banco na partida (`RECORVIA_ENCRYPTION_STARTUP_CHECK=enforce`).

`RECORVIA_ALLOWED_ORIGINS` fica vazio: a interface e a API estão na mesma origem e nenhuma outra
origem precisa de CORS. Não defina `RECORVIA_PAYSIM_URL`: sem ela o simulador não é registrado.

**Guarde no cofre de segredos da operação**, fora do servidor: uma cópia do `.env` e, à parte, o
par completo de `RECORVIA_ENCRYPTION_KEYS` e o arquivo `segredos/backup-cifragem.chave`. Sem as
chaves, o backup do banco não recupera as credenciais dos provedores nem os segredos dos webhooks,
e sem a chave do backup nenhum backup contínuo pode ser restaurado
([guarda das chaves](backup-e-restauracao.md#guarda)).

## Primeira subida

```bash
cd /opt/recorvia/deploy/producao
docker compose up -d --build --wait
docker compose ps
curl -s https://app.example.com/livez         # {"status":"ok"} (pela internet)
docker compose exec -T web wget -q -O - http://api:8081/readyz
# {"checks":{"postgres":"ok","redis":"ok"},"status":"ok"} (só pela rede interna)
```

`/readyz` consulta o PostgreSQL e o Redis a cada chamada: fica só na rede interna, para que
ninguém de fora esgote o pool de conexões. Pela internet, `/livez` diz se a API responde, sem tocar
nas dependências.

A construção das imagens leva alguns minutos. O Keycloak, na primeira partida, cria as tabelas,
importa o realm e o administrador temporário (até 2 minutos). Se o certificado não sair, veja
`docker compose logs proxy` (em geral, DNS ainda não propagado ou porta 80 bloqueada).

### Administrador definitivo do Keycloak

1. Defina `KEYCLOAK_ADMIN_ALLOWED_CIDRS` no `.env` com o seu IP (por exemplo,
   `203.0.113.10/32`) e recrie o proxy (`docker compose up -d proxy`). Abra
   `https://auth.example.com/admin/` e entre com `KEYCLOAK_ADMIN_USER` e
   `KEYCLOAK_ADMIN_PASSWORD`. O Keycloak avisa que é um administrador temporário.
2. No realm **master**, crie o administrador definitivo (*Users → Add user*), defina a senha, dê
   o papel `admin` (*Role mapping*) e exija segundo fator (*Required user actions → Configure
   OTP*).
3. Saia, entre com o novo usuário, configure o segundo fator e apague o usuário temporário.
4. As variáveis `KEYCLOAK_ADMIN_USER` e `KEYCLOAK_ADMIN_PASSWORD` deixam de ter efeito (só
   valem com o banco vazio); pode manter os valores no `.env`.
5. Fora das janelas de administração, remova seu IP de `KEYCLOAK_ADMIN_ALLOWED_CIDRS` (ou
   restrinja à VPN) e recrie o proxy.

O realm `master` é importado da imagem na primeira partida com proteção contra força bruta (5
falhas, espera de 1 a 15 minutos), senha de 12 a 128 caracteres e eventos de login e de
administração.

### Primeiro usuário e organização

O realm `recorvia` não tem autocadastro (decisão do dono, veja as pendências). O primeiro usuário
é criado pela operação:

1. No console, troque para o realm **recorvia**, *Users → Add user*: e-mail, nome e sobrenome;
   marque *Email verified* só se o e-mail já foi confirmado por outro meio (senão o Keycloak manda
   a verificação no primeiro login). Em *Credentials*, defina uma senha temporária (o usuário a
   troca no primeiro acesso) ou use *Send email* com a ação *Update password*.
2. A pessoa entra em `https://app.example.com`, faz login e cria a organização na tela de
   boas-vindas: quem cria vira proprietário (RF-01). Os demais membros entram por convite da
   própria organização; cada convidado precisa de um usuário no realm (criado da mesma forma).
3. Para a equipe da plataforma (atendimento de serviços adicionais), a pessoa entra uma vez no
   Recorvia e depois é incluída em `platform_staff` conforme o
   [runbook](runbooks/equipe-da-plataforma.md), usando
   `docker compose exec postgres psql -U postgres -d recorvia` nesta pasta.

As regras do realm de produção: senha de 12 a 128 caracteres, diferente do usuário, do e-mail e
das 5 anteriores; e-mail verificado; bloqueio temporário depois de 5 falhas seguidas (espera de 1
minuto, crescente até 15 minutos); troca de senha por e-mail; eventos de login e de administração
guardados por 90 dias (*Events* no console).

## Backup

O PostgreSQL usa a imagem `recorvia-postgres` (ADR-087): arquiva cada segmento de WAL, cifrado
no servidor, no destino S3 do `.env`, e o serviço `postgres-backup` faz os backups base, a
retenção e as métricas (coleta `recorvia-backup` do Prometheus; alertas e ações no
[runbook](runbooks/backup-continuo.md)). Os dois ficam nas redes `data` (sem saída) e `storage`
(com saída para o armazenamento). Restauração para um instante, linhas do tempo e exportação
do realm do Keycloak: [backup contínuo](backup-e-restauracao.md) e
[recuperação de desastre](runbooks/recuperacao-de-desastre.md). Confira depois da subida:

```bash
docker compose exec -T postgres-backup recorvia-pgbackup check-config   # destino e chave
docker compose exec -T postgres-backup recorvia-pgbackup list           # backups base ("esta chave")
```

O backup lógico continua útil como cópia complementar (antes de atualizações, por exemplo), com
os roteiros de [backup e restauração](backup-e-restauracao.md) apontando para o container de
produção:

```bash
cd /opt/recorvia
export RECORVIA_PG_CONTAINER=$(docker compose -f deploy/producao/compose.yaml ps -q postgres)
make backup                         # banco recorvia, com manifesto, em backups/
make restore-check                  # restaura numa cópia e confere
```

### Backup do Keycloak

Usuários, senhas (hash), segundo fator, configuração do realm e sessões persistentes ficam no
banco `keycloak`. Faça o dump junto com o do Recorvia:

```bash
cd /opt/recorvia/deploy/producao
docker compose exec -T postgres pg_dump -U postgres -Fc keycloak >"keycloak-$(date -u +%Y%m%dT%H%M%SZ).dump"
docker compose exec -T postgres pg_restore --list <keycloak-AAAAMMDDTHHMMSSZ.dump | head   # confere que é legível
```

Restauração num servidor novo: suba só o banco e o bootstrap (`docker compose up -d --wait
postgres` e `docker compose run --rm keycloak-db-bootstrap`), restaure
(`docker compose exec -T postgres pg_restore -U postgres -d keycloak --clean --if-exists
--no-owner --role=keycloak <arquivo.dump`) e só então suba o restante. O realm já existe no
banco restaurado, então a importação da imagem é ignorada. Guarde os dumps com a mesma proteção
dos backups do Recorvia (têm dados pessoais e hashes de senha).

Guarde também, fora do servidor: o `.env` (cofre) e, se quiser evitar nova emissão de
certificados, o volume `caddy-data` (opcional: o Caddy emite de novo sozinho).

## Atualização de versão

As migrations não têm garantia de compatibilidade com a versão anterior da aplicação, por isso a
API e o worker param durante a migração (alguns minutos fora do ar). Leia o `CHANGELOG.md` da
versão antes.

```bash
cd /opt/recorvia
git fetch --tags && git checkout v0.4.0
sed -i 's/^RECORVIA_VERSION=.*/RECORVIA_VERSION=0.4.0/' deploy/producao/.env
diff <(grep -oE '^#?[A-Z_]+=' deploy/producao/.env.example | tr -d '#' | sort -u) \
     <(grep -oE '^#?[A-Z_]+=' deploy/producao/.env | tr -d '#' | sort -u)   # variáveis novas na versão
cd deploy/producao
docker compose build                                   # com a versão antiga ainda no ar
RECORVIA_PG_CONTAINER=$(docker compose ps -q postgres) make -C ../.. backup
docker compose stop api worker                         # o worker drena por até 2m30s
docker compose up -d --wait                            # bootstrap, migrations, API, worker, web
docker compose logs migrate                            # migrations aplicadas
docker compose exec -T web wget -q -O - http://api:8081/readyz
```

**Voltar atrás**: as imagens da versão anterior continuam no servidor com a etiqueta antiga. Se
a versão nova não aplicou migrations, basta voltar `RECORVIA_VERSION` e o checkout e repetir o
`up`. Se aplicou, restaure o backup feito antes da atualização
([recuperação](backup-e-restauracao.md#recuperação-de-desastre)) e suba a versão anterior.

O Keycloak, o PostgreSQL, o Redis, o RabbitMQ e o Caddy mudam de versão junto com o repositório
(tags fixas no compose e nos Dockerfiles). Atualização de versão principal do PostgreSQL exige
`pg_upgrade` ou dump e restauração: não troque a tag sem um plano próprio.

## Rotação de chaves e senhas

Em toda rotação: gere o valor novo (`bash gerar-segredos.sh --senha` ou `--chave`), guarde-o no
cofre **antes** de aplicar, atualize o `.env` e registre quem fez e quando.

| Segredo | Como trocar |
|---|---|
| Chave de cifragem (`RECORVIA_ENCRYPTION_KEYS`) | em duas fases, conforme [rotação](backup-e-restauracao.md#rotação): acrescente a chave nova à lista e recrie API e worker (`docker compose up -d api worker`); confira com `docker compose exec -T api /app keyring-check` e `docker compose exec -T worker /app keyring-check`; depois troque `RECORVIA_ENCRYPTION_ACTIVE_KEY` e recrie de novo. A chave antiga só sai quando o `keyring-check` não mostrar dados com ela |
| `RECORVIA_OWNER_PASSWORD`, `RECORVIA_APP_PASSWORD` | troque no `.env` e rode `docker compose up -d --wait`: o `migrate-bootstrap` aplica a senha nova aos papéis antes de a API e o worker serem recriados |
| `KEYCLOAK_DB_PASSWORD` | troque no `.env` e rode `docker compose up -d --wait`: o `keycloak-db-bootstrap` aplica a senha e o Keycloak é recriado |
| `POSTGRES_SUPERUSER_PASSWORD` | `docker compose exec postgres psql -U postgres -c "\password postgres"` com o valor novo, depois atualize o `.env` (a variável da imagem só vale na criação do volume) |
| `REDIS_PASSWORD` | troque no `.env` e `docker compose up -d --wait redis api` (as sessões persistem no volume) |
| `RABBITMQ_PASSWORD` | `RABBITMQ_USER=$(sed -n 's/^RABBITMQ_USER=//p' .env)` e `docker compose exec rabbitmq rabbitmqctl change_password "$RABBITMQ_USER" '<nova>'`; atualize o `.env` e `docker compose up -d --wait worker api` |
| `RECORVIA_OIDC_CLIENT_SECRET` | no console, realm recorvia, *Clients → recorvia-web → Credentials → Regenerate*; copie o valor para o `.env` e `docker compose up -d --wait api` (logins em andamento nesse intervalo falham e são repetidos) |
| Administrador do Keycloak | pelo console (*Users → Credentials*) |
| `GRAFANA_ADMIN_PASSWORD` | pelo Grafana (*Profile → Change password*) e no `.env` |

O realm só é importado na primeira partida. Para trocar depois o domínio, o SMTP ou os endereços
do cliente, use o console: *Realm settings → Email* para o SMTP e *Clients → recorvia-web* para
*Valid redirect URIs*, *Web origins* e *Valid post logout redirect URIs*. Trocar
`RECORVIA_DOMAIN` também muda a URL dos webhooks dos provedores: reconfigure as contas de
provedor das organizações.

## O que monitorar

Com o perfil `observability`:

```bash
cd /opt/recorvia/deploy/producao
# no .env: OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 e os destinos ALERTMANAGER_*
docker compose --profile observability up -d --build --wait
# da sua máquina: túnel para Grafana, Prometheus e Alertmanager
ssh -L 3000:127.0.0.1:3000 -L 9090:127.0.0.1:9090 -L 9093:127.0.0.1:9093 servidor
```

Os alertas, os dashboards e os runbooks são os de [observabilidade](observabilidade.md) e
[runbooks](runbooks/README.md). Configure o e-mail ou o webhook do Alertmanager e confirme o
recebimento do alerta diário `RecorviaWatchdog`: sem ele, nada avisa que os alertas pararam.

Fora do servidor (o que o próprio servidor não consegue avisar):

- **Disponibilidade externa**: um verificador externo consultando `https://RECORVIA_DOMAIN/livez`
  (espera 200 e `"status":"ok"`) e `https://KEYCLOAK_DOMAIN/realms/recorvia` a cada minuto. A
  prontidão das dependências (`/readyz`, só na rede interna) aparece no Prometheus
  (`recorvia_dependency_up`) e no alerta `RecorviaDependencyUnavailable`.
- **Certificados**: validade dos dois nomes (o Caddy renova cerca de 30 dias antes; alerte com
  menos de 14 dias).
- **Recursos do servidor**: disco (alerta com menos de 20% livre; o banco e os logs crescem),
  memória, CPU e reinícios de containers (`docker compose ps`, coluna *STATUS*).
- **Backup**: alertas do backup contínuo (`RecorviaWALArchiveStale`, `RecorviaBaseBackupStale` e
  os demais do grupo `recorvia-backup`), idade do último backup lógico e resultado do último
  `restore-check`.
- **Entregabilidade**: bounces e reclamações no painel do provedor de e-mail.
- **Segurança**: eventos de login com erro no Keycloak (*Events*), atualizações de segurança do
  sistema e das imagens (job `image-scan` da CI).

## Extensões

Para mudar algo sem editar o `compose.yaml` (limites de memória, parâmetros do banco, outro
volume), crie um arquivo ao lado, por exemplo `compose.servidor.yaml`:

```yaml
services:
  postgres:
    deploy:
      resources:
        limits: { cpus: "4", memory: 6g }
```

e declare os dois no `.env`: `COMPOSE_FILE=compose.yaml:compose.servidor.yaml`. O serviço
`postgres` usa a imagem `recorvia-postgres` (a oficial com o arquivamento de WAL); `command`,
`volumes` e `environment` são os pontos de extensão, e o volume de dados é `postgres-data`.

## Verificação automatizada

`make prod-check` (`deploy/producao/verificar.sh`, job `production-stack` da CI) sobe este
compose numa máquina de desenvolvimento ou de CI, num projeto isolado (`recorvia-prodcheck`), com
os domínios `localhost` e `auth.localhost` (certificados da autoridade interna do Caddy) e o proxy
publicado só em 127.0.0.1. Confere a configuração (portas, serviços de demonstração, healthchecks,
restart e limites), HTTPS e HSTS, `/livez` pela origem pública e `/readyz` só pela rede interna, a
interface, o endereço do cliente visto pelo nginx (também por IPv6, e sem aceitar
`X-Forwarded-For` forjado), o login redirecionando ao Keycloak com o `redirect_uri` do domínio,
problem+json numa rota inexistente, as portas dos containers em execução, a lista do que o proxy
repassa no domínio do Keycloak (console, API de administração, realm `master` e variantes de
caminho com 403), os realms `recorvia` e `master`, o backup do banco do Keycloak e o backup
contínuo (com um MinIO só no compose de verificação: segmento de WAL arquivado, agente saudável,
backup base cifrado com a chave do arquivo, métricas pela rede `data` e o armazenamento só pela
rede `storage`), e derruba o projeto no fim. Exige as portas 80 e 443 de 127.0.0.1 livres. Não rode no
servidor de produção.

## Pendências que dependem do dono

- **Provedor de hospedagem**: onde fica o servidor (e em qual região), backup fora do servidor
  (armazenamento de objetos ou outro provedor), cofre de segredos e, se desejado, banco gerenciado
  no lugar do PostgreSQL do compose. O pacote funciona em qualquer servidor com Docker.
- **Asaas em produção**: o adaptador ainda não foi exercitado no sandbox
  ([guia](../integracoes/asaas.md)). Em produção, cada organização configura a própria conta
  (chave de API de produção) na tela de provedores; os webhooks chegam em
  `https://RECORVIA_DOMAIN/webhooks/providers/asaas/<token>`.
- **E-mail transacional (SMTP de produção)**: escolher o provedor (entregabilidade no Brasil,
  SPF/DKIM/DMARC, limites de envio, retorno de bounces, contrato de tratamento de dados) e
  preencher `SMTP_*`. Sem ele, o worker não envia cobranças nem convites e o Keycloak não envia
  verificação de e-mail nem troca de senha.
- **Autocadastro no login**: o realm de produção não permite que alguém crie a própria conta.
  Liberar (*Realm settings → Login → User registration*) exige decidir quem pode criar
  organizações e como evitar abuso.
- **Alta disponibilidade**: um servidor é ponto único de falha. Réplicas, banco com failover e
  outra região ficam para quando o volume justificar (ADR-086).
