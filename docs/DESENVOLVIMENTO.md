# Recorvia: guia de desenvolvimento

Referência rápida de comandos, convenções e decisões. Detalhes em
`docs/DECISOES.md`, requisitos em `docs/REQUISITOS.md` e situação atual em
`docs/STATUS.md`.

## Comandos

| Comando | Uso |
|---|---|
| `make tools` | instala golangci-lint, govulncheck, sqlc e gitleaks fixados em `$(go env GOPATH)/bin` |
| `make fmt` / `make fmt-check` | formatação Go |
| `make vet` | `go vet` com e sem a tag `integration` |
| `make lint` | golangci-lint (`.golangci.yml`) |
| `make test` | testes unitários com `-race` (sem Docker, rede ou credenciais) |
| `make coverage` | cobertura de domínio e aplicação, mínimo 90% (`tools/covercheck`) |
| `make fuzz` | fuzz tests curtos (`FUZZTIME=30s make fuzz` para mais tempo) |
| `make test-integration` | PostgreSQL, RabbitMQ e Redis reais via Testcontainers |
| `make sqlc` / `make sqlc-check` | gera/verifica `internal/dbgen` (sqlc v1.31.1; exige gcc para cgo) |
| `make vulncheck` | govulncheck |
| `make secrets-scan` | gitleaks |
| `make up` / `make down` / `make reset` | ambiente local com Docker Compose |
| `make up-obs` | ambiente local com Prometheus, Grafana, Jaeger e OTel Collector |

O checkout pode estar em disco sem permissão de execução (vfat/noexec): as
ferramentas são instaladas fora do repositório e scripts são chamados com
`bash script.sh`. O Docker Compose não usa bind mounts de arquivos do
repositório (configurações entram nas imagens via `COPY`), o que mantém o
ambiente funcional também com Docker Desktop e diretórios não compartilhados.

## Estrutura

- `cmd/api`, `cmd/worker`: executáveis principais. `cmd/migrate`, `cmd/seed`,
  `cmd/paysim`: bootstrap/migrations, seed demonstrativo e simulador de pagamentos.
- `internal/kernel/*`: dinheiro, calendário, relógio e erros de aplicação.
- `internal/<modulo>`: domínio e casos de uso, sem dependências de infraestrutura.
- `internal/<modulo>/<modulo>pg`: adaptador PostgreSQL do módulo.
- `internal/httpapi`: handlers HTTP, middleware e mapeamento de erros.
- `internal/infra/*`: configuração, logs, PostgreSQL, Redis, RabbitMQ, SMTP, telemetria.
- `internal/platformbilling`: planos e cobrança da própria plataforma (prefixo `platform_`).
- `internal/dbgen`: código gerado pelo sqlc. Não editar.
- `db/migrations`: migrations Goose. `db/queries`: consultas do sqlc.
- `api/openapi.yaml`: contrato HTTP. `web/`: interface React.

## Convenções

- **Dinheiro:** `money.Money` (`int64` em unidades mínimas + moeda). Nunca
  `float`. Cálculos intermediários com `big.Rat`; arredondar uma vez com
  `money.DefaultRounding` (half-even).
- **Datas:** `time.Time` em UTC; regras de calendário com o fuso do tenant via
  `internal/kernel/calendar`. Períodos são `[início, fim)`.
- **Tempo:** regras recebem `clock.Clock`; nunca chamar `time.Now()` no domínio.
- **Tenant:** todo acesso a dados de tenant passa por
  `postgres.DB.InTenantTx`. Consultas filtram `tenant_id` explicitamente mesmo
  com RLS. Nunca usar `SET` de sessão; somente `set_config(..., true)`.
- **Erros:** casos de uso retornam `*apperr.Error` com código estável; o HTTP
  traduz a categoria. Encadear causas com `%w`; comparar com `errors.Is/As`.
- **Eventos:** gravar em `outbox_events` na mesma transação da alteração.
  Consumidores registram `inbox_messages` na mesma transação dos efeitos.
- **Interfaces:** pequenas e declaradas no pacote consumidor.
- **Testes:** tabelas de casos, fakes pequenos, `httptest`. Integração com a
  tag `integration`. Sem `time.Sleep` para sincronizar.
- **Logs:** `slog` com contexto (`logging.WithAttrs`); chaves sensíveis são
  redigidas automaticamente.
- **Métricas:** sem rótulos com IDs de tenant, cliente ou fatura.
- **Idioma:** interface, mensagens e documentação em português; identificadores
  de código em inglês.

## Fluxo Git

GitFlow (`CONTRIBUTING.md`): `feature/*` a partir de `develop`, PR com merge
commit (`--no-ff`), Conventional Commits, tags anotadas `vX.Y.Z` em `main`.
Não commitar diretamente em `main` ou `develop`.
