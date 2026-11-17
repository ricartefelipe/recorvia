# Status da implementação

Atualizado a cada incremento. Registra o que foi concluído, as verificações
executadas (com resultado real) e as pendências.

## Etapas de execução

| # | Etapa | Branch | Situação |
|---|---|---|---|
| 1 | Fundação: núcleo de dinheiro/calendário, infraestrutura, bootstrap do banco, CI | `feature/fundacao-do-projeto` | em revisão |
| 2 | Fatia vertical: autenticação, tenant, cliente, plano, assinatura, fatura, cobrança simulada, webhook, ledger, outbox | `feature/fatia-vertical-cobranca` | pendente |
| 3 | Interface web (React) das telas da fatia vertical e E2E | `feature/interface-web` | pendente |
| 4 | Catálogo completo, consumo medido, cupons, créditos, reajustes, prorrata | `feature/faturamento-avancado` | pendente |
| 5 | Reembolsos, disputas, conciliação, Asaas sandbox | `feature/recebiveis-conciliacao` | pendente |
| 6 | Inadimplência, notificações por e-mail, webhooks enviados | `feature/cobranca-e-notificacoes` | pendente |
| 7 | Plataforma (planos/limites), portal do cliente, credenciais de API, auditoria | `feature/plataforma-e-portal` | pendente |
| 8 | Observabilidade, runbooks, backup/restauração, documentação final | `feature/operacao` | pendente |
| 9 | Release `0.1.0` | `release/0.1.0` | pendente |

## Concluído

### Etapa 1 – Fundação
- `internal/kernel/money`: unidades mínimas, overflow, `big.Rat`, half-even,
  alocação por maior resto; fuzz de alocação, multiplicação e parsing.
- `internal/kernel/calendar`: âncora preservada (31 → 28/29 → 31), anos
  bissextos, `[início, fim)`, prorrata por dias civis; fuzz de invariantes.
  O fuzz encontrou e motivou a correção do início de dia em fusos com
  horário de verão à meia-noite (America/Sao_Paulo, 1996).
- `internal/kernel/clock`, `internal/kernel/apperr`.
- `internal/infra/config`, `internal/infra/logging`, `internal/infra/postgres`,
  `internal/infra/migrate`, `cmd/migrate`.
- Migration `00001_fundacao.sql`: `app_current_tenant()`, `tenants` com RLS.
- Testes de integração com PostgreSQL 18.7 real e papel `recorvia_app`.

## Verificações executadas

| Data | Comando | Resultado |
|---|---|---|
| 2026-11-16 | `make fmt-check vet lint` | sem achados |
| 2026-11-16 | `make test` (`-race`) | aprovado |
| 2026-11-16 | `make coverage` | 97,7% (344/352 statements) nos pacotes de domínio |
| 2026-11-16 | `make fuzz` (10–20 s por alvo) | aprovado após correção de DST |
| 2026-11-16 | `go test -race -tags integration ./internal/infra/postgres/...` | aprovado (PostgreSQL 18.7) |
| 2026-11-16 | `make vulncheck` | nenhuma vulnerabilidade |
| 2026-11-16 | `docker compose up postgres` + `migrate bootstrap` + `migrate up` | papéis sem superuser/BYPASSRLS; RLS ativa em `tenants` |

## Pendências reais

- Etapas 2 a 9.
- Proteção remota das branches `main` e `develop`: será configurada após a
  primeira execução da CI, exigindo os checks `go-quality`, `go-integration`
  e `secrets-scan`.

## Ambiente de desenvolvimento observado

- O checkout está em partição vfat sem bit de execução: ferramentas Go são
  instaladas em `$(go env GOPATH)/bin`; `node_modules` do frontend não pode
  usar symlinks neste disco (ver `web/README.md` quando criado).
- A porta 6379 do host já estava ocupada por outro projeto; o Redis do
  Recorvia é publicado em `127.0.0.1:6380`.

## Ponto de retomada

Próximo passo: abrir PR da etapa 1, configurar proteção de branches e iniciar
`feature/fatia-vertical-cobranca` a partir de `develop`.
