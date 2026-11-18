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
- [Decisões arquiteturais](docs/DECISOES.md) · [Requisitos](docs/REQUISITOS.md)
- [Modelo comercial (hipóteses)](docs/negocio/modelo-comercial.md)
- [Roteiro demonstrativo](docs/roteiro-demo.md)
- [Contrato da API (OpenAPI)](api/openapi.yaml)
- [Guia de desenvolvimento](CLAUDE.md) · [Contribuição (GitFlow)](CONTRIBUTING.md) · [Changelog](CHANGELOG.md)

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

## Configuração

Variáveis `RECORVIA_*` validadas na inicialização (`internal/infra/config`); em produção são
exigidos HTTPS, cookie `__Host-` seguro, SMTP com TLS e ausência de destinos privados de webhook.
Credenciais de provedores ficam cifradas (AES-256-GCM) com chave fornecida por
`RECORVIA_ENCRYPTION_KEYS`/`RECORVIA_ENCRYPTION_ACTIVE_KEY` (rotação por identificador de chave).

## Limitações conhecidas

- O adaptador do Asaas segue a documentação oficial, mas ainda não foi exercitado no sandbox
  (requer credenciais e URL pública de webhook); o passo a passo da validação está em
  `docs/integracoes/asaas.md`.
- Funcionalidades em desenvolvimento estão listadas em `docs/STATUS.md`.
- O ledger é operacional: não substitui contabilidade fiscal. Faturas são documentos comerciais,
  não documentos fiscais.
- Validação de CPF/CNPJ é estrutural e não confirma identidade.
- Os controles implementados não constituem, por si, conformidade jurídica ou regulatória.
