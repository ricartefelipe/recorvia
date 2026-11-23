# Implantação em produção

Guia para colocar o Recorvia no ar num servidor próprio, com o pacote de `deploy/producao/`
(decisões na ADR-086). O pacote não depende de nenhum provedor de hospedagem: precisa só de um
servidor Linux com Docker, de um domínio e de um servidor de e-mail.

## O que sobe

```text
Internet ──80/443──> proxy (Caddy, HTTPS automático)
                      ├── RECORVIA_DOMAIN ──> web (nginx: interface + /api, /auth, /webhooks, /readyz) ──> api
                      └── KEYCLOAK_DOMAIN ──> keycloak (login)
api, worker ──> postgres (bancos recorvia e keycloak), redis, rabbitmq      (rede interna, sem internet)
worker ──> SMTP, provedor de pagamento, webhooks dos clientes                (saída para a internet)
```

- Só o proxy publica portas no servidor: 80 e 443 (TCP e UDP). Banco, Redis, RabbitMQ, API e
  Keycloak não ficam acessíveis de fora.
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
| `deploy/producao/gerar-segredos.sh` | gera senhas e a chave de cifragem com segurança |
| `deploy/producao/caddy/` | imagem do proxy (Caddyfile) |
| `deploy/producao/keycloak/` | imagem do Keycloak de produção e o realm sem usuários |
| `deploy/producao/verificar.sh` | verificação automatizada (`make prod-check`), para rodar fora da produção |

## Requisitos do servidor

- **Sistema**: Linux de 64 bits (por exemplo, Ubuntu 24.04 LTS ou Debian 13), com acesso SSH por
  chave e atualizações automáticas de segurança.
- **Docker** 27 ou superior, com o plugin Compose 2.24 ou superior, e **Git**.
- **Tamanho mínimo**: 4 vCPU, 8 GB de RAM e 80 GB de SSD. Os limites do compose somam cerca de
  6,5 GB de memória (PostgreSQL 2 GB, Keycloak 1,5 GB, RabbitMQ 1 GB, API e worker 512 MB cada,
  demais menores). Com o perfil `observability`, conte mais 2 GB. Para ajustar os limites, use
  um arquivo compose adicional (veja [Extensões](#extensões)).
- **Rede**: IP público fixo (IPv4, e IPv6 se houver); portas 80 e 443 (TCP e UDP) liberadas no
  firewall de entrada; porta do SSH restrita à operação. Saída liberada para a internet
  (certificados, SMTP, provedor de pagamento e webhooks dos clientes).
- **Disco**: os logs dos containers giram (5 arquivos de 20 MB por serviço). Monitore o espaço
  livre (veja [O que monitorar](#o-que-monitorar)).
- **Relógio** sincronizado (NTP): tokens do login e assinaturas de webhook dependem dele.

## DNS

Dois nomes, ambos apontando para o IP do servidor (registros `A` e, com IPv6, `AAAA`):

| Variável | Exemplo | Uso |
|---|---|---|
| `RECORVIA_DOMAIN` | `app.example.com` | interface, API, portal do cliente e webhooks dos provedores |
| `KEYCLOAK_DOMAIN` | `auth.example.com` | login (Keycloak) |

Crie os registros **antes** da primeira subida: o Caddy pede os certificados na partida e a
autoridade só os emite se o nome já apontar para o servidor. Se houver CDN ou balanceador à
frente, desligue o proxy dele nesses nomes (somente DNS) ou declare-o em `trusted_proxies` no
`Caddyfile`: o Caddy ignora `X-Forwarded-For` vindo de quem não é confiável.

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
| `KEYCLOAK_ADMIN_ALLOWED_CIDRS` | IPs da operação que podem abrir o console do Keycloak (recomendado) |

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
par completo de `RECORVIA_ENCRYPTION_KEYS`. Sem as chaves, o backup do banco não recupera as
credenciais dos provedores nem os segredos dos webhooks
([guarda das chaves](backup-e-restauracao.md#guarda)).

## Primeira subida

```bash
cd /opt/recorvia/deploy/producao
docker compose up -d --build --wait
docker compose ps
curl -s https://app.example.com/readyz        # {"checks":{"postgres":"ok","redis":"ok"},"status":"ok"}
```

A construção das imagens leva alguns minutos. O Keycloak, na primeira partida, cria as tabelas,
importa o realm e o administrador temporário (até 2 minutos). Se o certificado não sair, veja
`docker compose logs proxy` (em geral, DNS ainda não propagado ou porta 80 bloqueada).

### Administrador definitivo do Keycloak

1. Abra `https://auth.example.com/admin/` e entre com `KEYCLOAK_ADMIN_USER` e
   `KEYCLOAK_ADMIN_PASSWORD`. O Keycloak avisa que é um administrador temporário.
2. No realm **master**, crie o administrador definitivo (*Users → Add user*), defina a senha, dê
   o papel `admin` (*Role mapping*) e exija segundo fator (*Required user actions → Configure
   OTP*).
3. Saia, entre com o novo usuário, configure o segundo fator e apague o usuário temporário.
4. As variáveis `KEYCLOAK_ADMIN_*` deixam de ter efeito (só valem com o banco vazio); pode
   manter os valores no `.env`.

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

O backup contínuo do PostgreSQL (arquivamento de WAL) entra depois por um arquivo compose
adicional. Até lá, use o backup lógico de [backup e restauração](backup-e-restauracao.md),
apontando para o container de produção:

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
curl -s https://app.example.com/readyz
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
| `RABBITMQ_PASSWORD` | `docker compose exec rabbitmq rabbitmqctl change_password recorvia '<nova>'`, atualize o `.env` e `docker compose up -d --wait worker api` |
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

- **Disponibilidade externa**: um verificador externo consultando `https://RECORVIA_DOMAIN/readyz`
  (espera 200 e `"status":"ok"`) e `https://KEYCLOAK_DOMAIN/realms/recorvia` a cada minuto.
- **Certificados**: validade dos dois nomes (o Caddy renova cerca de 30 dias antes; alerte com
  menos de 14 dias).
- **Recursos do servidor**: disco (alerta com menos de 20% livre; o banco e os logs crescem),
  memória, CPU e reinícios de containers (`docker compose ps`, coluna *STATUS*).
- **Backup**: idade do último backup e resultado do último `restore-check`.
- **Entregabilidade**: bounces e reclamações no painel do provedor de e-mail.
- **Segurança**: eventos de login com erro no Keycloak (*Events*), atualizações de segurança do
  sistema e das imagens (job `image-scan` da CI).

## Extensões

Para mudar algo sem editar o `compose.yaml` (limites de memória, backup contínuo do banco, outro
volume), crie um arquivo ao lado, por exemplo `compose.servidor.yaml`:

```yaml
services:
  postgres:
    deploy:
      resources:
        limits: { cpus: "4", memory: 6g }
```

e declare os dois no `.env`: `COMPOSE_FILE=compose.yaml:compose.servidor.yaml`. O serviço
`postgres` usa a imagem oficial sem configuração própria; `command`, `volumes` e `environment` são
os pontos de extensão, e o volume de dados é `postgres-data`.

## Verificação automatizada

`make prod-check` (`deploy/producao/verificar.sh`, job `production-stack` da CI) sobe este
compose numa máquina de desenvolvimento ou de CI, num projeto isolado (`recorvia-prodcheck`), com
os domínios `localhost` e `auth.localhost` (certificados da autoridade interna do Caddy) e o proxy
publicado só em 127.0.0.1. Confere a configuração (portas, serviços de demonstração, healthchecks,
restart e limites), HTTPS e HSTS, `/readyz`, a interface, o endereço do cliente visto pelo nginx,
o login redirecionando ao Keycloak com o `redirect_uri` do domínio, problem+json numa rota
inexistente, as portas dos containers em execução, o bloqueio do console de administração, o realm
de produção e o backup do banco do Keycloak, e derruba o projeto no fim. Exige as portas 80 e 443 de 127.0.0.1 livres. Não rode no
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
