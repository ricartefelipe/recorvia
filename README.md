# Recorvia

Plataforma SaaS B2B multitenant para gestão de assinaturas, faturamento recorrente, cobranças,
recuperação de inadimplência e conciliação de recebíveis, voltada a prestadores de serviços
recorrentes (software houses, consultorias, suporte de TI).

O Recorvia transforma contratos recorrentes em faturas rastreáveis, solicita a cobrança a um
provedor de pagamento (checkout hospedado), acompanha os recebimentos e registra tudo em um ledger
operacional de partidas dobradas. Não custodia dinheiro nem processa cartões.

> A situação de cada parte (concluído, verificado, pendente) está em [`docs/STATUS.md`](docs/STATUS.md).

## Arquitetura em resumo

Monólito modular em Go com dois executáveis principais: `cmd/api` (HTTP, autenticação, sessões) e
`cmd/worker` (outbox, consumidores, agendador, integrações). PostgreSQL com Row-Level Security por
tenant, RabbitMQ para eventos, Redis para sessões, Keycloak para identidade. Detalhes:

- [Visão geral e C4](docs/arquitetura/visao-geral.md)
- [Multitenancy](docs/arquitetura/multitenancy.md)
- [Dinheiro, datas e arredondamento](docs/arquitetura/dinheiro-e-datas.md)
- [Fluxos de faturamento, cobrança e webhook](docs/arquitetura/fluxos.md)
- [Máquinas de estado](docs/arquitetura/maquinas-de-estado.md)
- [Modelo de dados](docs/arquitetura/modelo-de-dados.md)
- [Matriz de permissões](docs/arquitetura/permissoes.md)
- [Operação: observabilidade, runbooks, backup e retenção](docs/operacao/observabilidade.md)
- [Decisões arquiteturais](docs/DECISOES.md) · [Requisitos](docs/REQUISITOS.md)
- [Modelo comercial (hipóteses)](docs/negocio/modelo-comercial.md)
- [Roteiro demonstrativo](docs/roteiro-demo.md)
- [Contrato da API (OpenAPI)](api/openapi.yaml)
- [Guia de desenvolvimento](docs/DESENVOLVIMENTO.md) · [Contribuição (GitFlow)](CONTRIBUTING.md) · [Changelog](CHANGELOG.md)

## Executar localmente (a partir de um checkout limpo)

Pré-requisitos: Docker com Compose v2, GNU Make e (para desenvolvimento) Go 1.27.4.

```bash
make up                       # cria .env a partir de .env.example, sobe tudo e executa o seed
curl -s http://localhost:18081/readyz
```

| Serviço | Endereço |
|---|---|
| Interface web | http://localhost:8080 |
| API | http://localhost:18081 |
| Keycloak (realm `recorvia`) | http://localhost:8180 |
| Simulador de pagamentos | http://localhost:8090 |
| Mailpit | http://localhost:8025 |
| RabbitMQ | http://localhost:15672 |

Usuários de demonstração e o passo a passo estão em [`docs/roteiro-demo.md`](docs/roteiro-demo.md).
O redirecionamento OIDC padrão aponta para a interface web (porta 8080); para testar o login
diretamente na API, defina `RECORVIA_OIDC_REDIRECT_URL=http://localhost:18081/auth/callback` no `.env`.

## Testes

```bash
make tools             # golangci-lint, govulncheck, sqlc e gitleaks com versões fixadas
make test              # unitários com -race (sem Docker, rede ou credenciais)
make coverage          # cobertura de domínio e aplicação (mínimo 90%)
make fuzz              # fuzz tests curtos
make test-integration  # PostgreSQL, RabbitMQ e Redis reais via Testcontainers
make web-install web-verify  # interface: tipos, lint, testes de componente (Vitest) e build
make web-e2e           # roteiros Playwright contra o ambiente em execução (make up)
make lint vulncheck sqlc-check secrets-scan
```

## Operação

Guias em [`docs/operacao/`](docs/operacao): [observabilidade](docs/operacao/observabilidade.md),
[runbooks](docs/operacao/runbooks/README.md), [backup e restauração](docs/operacao/backup-e-restauracao.md)
e [retenção e anonimização](docs/operacao/retencao-e-anonimizacao.md).

### Observabilidade

```bash
make up-obs          # ambiente completo + OpenTelemetry Collector, Jaeger, Prometheus, Alertmanager e Grafana
make obs-stack-check # confere alvos de coleta, regras, séries, um alerta até o e-mail, dashboards e rastros da stack em execução
make obs-check       # valida configurações, regras de alerta, roteamento e dashboards (otelcol, jaeger, promtool, amtool)
```

| Serviço | Endereço |
|---|---|
| Grafana (pasta *Recorvia*; leitura anônima, administrador `admin` com `GRAFANA_ADMIN_PASSWORD`) | http://localhost:3000 |
| Prometheus (alvos, regras e alertas) | http://localhost:9090 |
| Alertmanager (alertas recebidos, silêncios e inibições; e-mails no Mailpit, http://localhost:8025) | http://localhost:9093 |
| Jaeger (rastros da API e do worker) | http://localhost:16686 |

Dashboards provisionados: *visão geral* (página inicial), *mensageria* (filas, retentativas, DLQs,
consumidores e outbox) e *banco e processos* (pools de conexão, conexões do servidor e tarefas
agendadas). O Alertmanager roteia os alertas por severidade e os notifica por e-mail (no ambiente
local, o Mailpit) e por um webhook genérico, configurados por variáveis `ALERTMANAGER_*`
([encaminhamento de alertas](docs/operacao/observabilidade.md#encaminhamento-de-alertas)). Cada
alerta aponta para a seção do runbook correspondente, com o link em cada notificação:
[faturamento e cobrança](docs/operacao/runbooks/faturamento.md),
[filas e DLQ](docs/operacao/runbooks/filas-e-dlq.md), [conciliação](docs/operacao/runbooks/conciliacao.md),
[notificações e webhooks enviados](docs/operacao/runbooks/notificacoes-e-webhooks.md),
[encaminhamento de alertas](docs/operacao/runbooks/alertas.md)
e [isolamento de dados](docs/operacao/runbooks/isolamento-de-dados.md).

Sondas: `/livez` indica que o processo responde, sem consultar dependências; `/readyz` exige
PostgreSQL e Redis na API e PostgreSQL e RabbitMQ no worker (porta de métricas).

Na CI, o job `observability-check` executa `make obs-check` e o job `observability-stack` executa
`make up-obs` seguido de `make obs-stack-check`.

### Backup e restauração

```bash
make backup                 # pg_dump no container: backups/recorvia-<instante>.{dump,manifest,sha256}
make restore-check          # restaura o backup mais recente num banco novo e confere contagens, ledger e RLS
make restore-check DUMP=backups/recorvia-20261118T120000Z.dump
make dr-check               # recuperação de desastre de ponta a ponta num projeto isolado do compose (relatório em backups/dr-check)
```

O procedimento é exercitado pelo job `backup-restore` da CI (ambiente do compose com migrations,
dados de demonstração e lançamento no ledger; `make backup`, `make restore-check` e recuperação num
servidor PostgreSQL novo) e pelo job `disaster-recovery` (`make dr-check`: backup com as chaves de
cifragem guardadas à parte, destruição do banco, restauração, recusa de chave errada, conciliação
da janela perdida e API e worker religados sobre o banco restaurado, com os tempos medidos). O que
é exercitado, a guarda das chaves e as recomendações para produção estão em
[`docs/operacao/backup-e-restauracao.md`](docs/operacao/backup-e-restauracao.md).

### Análise de imagens

```bash
make images       # constrói api, worker, migrate, seed, paysim, web e keycloak (IMAGE_TAG=local)
make image-scan   # Trivy 0.76.2: falha em HIGH/CRITICAL com correção disponível (.trivyignore.yaml)
```

Na CI, o job `image-scan` constrói e analisa cada imagem com a mesma versão do Trivy; nenhuma
imagem é publicada e não há deploy.

## Configuração

Variáveis `RECORVIA_*` validadas na inicialização (`internal/infra/config`); em produção são
exigidos HTTPS, cookie `__Host-` seguro, SMTP com TLS e ausência de destinos privados de webhook.
Credenciais de provedores ficam cifradas (AES-256-GCM) com chave fornecida por
`RECORVIA_ENCRYPTION_KEYS`/`RECORVIA_ENCRYPTION_ACTIVE_KEY` (rotação por identificador de chave).
A API e o worker conferem, ao iniciar, se essas chaves decifram os dados em uso no banco e não
iniciam com chave errada ou ausente (`RECORVIA_ENCRYPTION_STARTUP_CHECK`: `enforce`, `warn` ou
`off`); o subcomando `keyring-check` faz a mesma conferência sem iniciar o serviço
(`docker compose run --rm --no-deps worker keyring-check`). Uma chave que não existe mais é
declarada em `RECORVIA_ENCRYPTION_LOST_KEYS`, e a rotação é feita em duas fases (distribuir a
chave a todos os processos, conferir cada processo em execução com
`docker compose exec -T <serviço> /app keyring-check` e só depois ativá-la):
`docs/operacao/backup-e-restauracao.md`.

## Limitações conhecidas

- O adaptador do Asaas segue a documentação oficial, mas ainda não foi exercitado no sandbox
  (requer credenciais e URL pública de webhook); o passo a passo da validação está em
  `docs/integracoes/asaas.md`.
- Funcionalidades em desenvolvimento estão listadas em `docs/STATUS.md`.
- O ledger é operacional: não substitui contabilidade fiscal. Faturas são documentos comerciais,
  não documentos fiscais.
- Validação de CPF/CNPJ é estrutural e não confirma identidade.
- Os controles implementados não constituem, por si, conformidade jurídica ou regulatória.
