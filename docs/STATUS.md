# Status da implementação

Atualizado a cada incremento. Registra o que está concluído, as verificações executadas (com
resultado real) e as pendências.

## Etapas de execução

| # | Etapa | Branch | Situação |
|---|---|---|---|
| 1 | Fundação: núcleo de dinheiro/calendário, infraestrutura, bootstrap do banco, CI | `feature/fundacao-do-projeto` | integrada em `develop` (PR #1) |
| 2 | Fatia vertical: autenticação, tenant, cliente, plano, assinatura, fatura, cobrança simulada, webhook, ledger, outbox, worker | `feature/fatia-vertical-cobranca` | em revisão |
| 3 | Interface web (React) e E2E | `feature/interface-web` | em desenvolvimento |
| 4 | Consumo medido, cupons, prorrata, troca de plano/quantidade, reajustes, notas de crédito | `feature/faturamento-avancado` | pendente |
| 5 | Reembolsos, disputas, conciliação, validação no sandbox do Asaas | `feature/recebiveis-conciliacao` | pendente |
| 6 | Inadimplência, notificações por e-mail, webhooks enviados com proteção SSRF | `feature/cobranca-e-notificacoes` | pendente |
| 7 | Portal do cliente, credenciais de API com escopos | `feature/portal-e-credenciais` | pendente |
| 8 | Observabilidade (perfil opcional), runbooks, backup/restauração, análise de imagens | `feature/operacao` | pendente |
| 9 | Release `0.1.0` | `release/0.1.0` | pendente |

## Concluído

### Etapa 1: fundação
Núcleo de dinheiro (`int64` + `big.Rat`, half-even, alocação por maior resto), calendário com
âncora preservada, relógio injetável, erros de aplicação, configuração validada, logs com redação,
transações com contexto de tenant, bootstrap de papéis, migrations e CI.

### Etapa 2: fatia vertical
- **Banco**: 8 migrations com RLS em 38 tabelas de tenant, FKs compostas, ledger com balanceamento
  verificado no commit, gatilhos de imutabilidade (ledger, auditoria, pagamentos, itens de fatura,
  versões de preço) e de proteção da fatura emitida, funções `SECURITY DEFINER` de descoberta.
  182 consultas tipadas com sqlc (`internal/dbgen`).
- **Domínio e casos de uso**: organizações, membros, convites e permissões; plano da plataforma com
  limites; identidade OIDC com sessão no servidor; clientes; catálogo com versões de preço imutáveis
  e quatro modelos de preço; assinaturas com máquina de estados; faturas e motor de recorrência com
  catch-up; ledger de partidas dobradas; cobranças com operações externas persistidas e estado
  `unknown`; recepção e processamento assíncrono de webhooks; pagamentos manuais; créditos;
  dashboard.
- **Integração**: simulador determinístico (`cmd/paysim`) e adaptadores do simulador e do Asaas
  (conforme documentação oficial; ainda não exercitado no sandbox).
- **Infraestrutura**: outbox com lease e relay com publisher confirms; RabbitMQ com ack manual,
  prefetch e concorrência configuráveis, filas de espera por tentativa e DLQ; agendador com lease no
  PostgreSQL; sessões cifradas no Redis; métricas Prometheus; rastreamento OpenTelemetry opcional.
- **API**: 79 das 127 operações do contrato OpenAPI implementadas (as 48 restantes listadas como pendentes no
  teste de contrato), sessão com CSRF, validação de tenant por requisição, permissão declarada por
  rota, `Idempotency-Key`, `If-Match`, paginação por cursor, erros RFC 9457.
- **Ambiente local**: compose com PostgreSQL, Redis, RabbitMQ, Keycloak (realm embutido), Mailpit,
  simulador, API, worker e seed idempotente de dois tenants.

## Verificações executadas

| Data | Comando | Resultado |
|---|---|---|
| 2026-11-16 | `make fmt-check vet lint` (fundação) | sem achados |
| 2026-11-16 | `make coverage` (fundação) | 97,7% nos pacotes de kernel |
| 2026-11-16 | CI do PR #1 | `go-quality`, `go-integration` e `secrets-scan` aprovados |
| 2026-11-17 | `go test -race -tags integration ./internal/pgstore/` | aprovado: RLS em todas as tabelas de tenant, privilégios do papel, ledger desbalanceado rejeitado, FKs compostas, isolamento com conexão compartilhada, motores concorrentes sem duplicidade e numeração sem lacunas, retomada com âncora no dia 31, fatura emitida protegida, rollback sem evento, disputa da outbox, broker indisponível, idempotência concorrente, webhook repetido e fora de ordem, timeout após sucesso sem cobrança duplicada |
| 2026-11-17 | `go test -race -tags integration ./internal/infra/rabbit/ ./internal/infra/redisstore/` | aprovado: confirms, retentativa, DLQ, reinício após commit antes do ack (efeito único), sessões cifradas e revogação, falha do Redis → 503 |
| 2026-11-17 | `go test -race -tags integration ./internal/httpapi/` | aprovado: 401/403/404/409/412/413/422/428, CSRF, permissões por papel, Idempotency-Key |
| 2026-11-17 | `go test ./internal/httpapi/ -run Contract` | rotas coincidem com o OpenAPI |
| 2026-11-17 | `make sqlc-check`, `make secrets-scan`, `golangci-lint` (código de produção) | sem diferenças, sem vazamentos, sem achados |
| 2026-11-17 | Stack local (`docker compose up`) + roteiro automatizado via API | login real no Keycloak com PKCE, cliente, plano, assinatura, fatura `ACME-2026-000001`, cobrança no simulador, pagamento pelo checkout, webhook HMAC, fatura paga, ledger (receita 99.000; liquidação 95.991; taxa 3.009), reenvio sem duplicidade, auditoria, dashboard e isolamento entre tenants |

## Pendências reais

- Etapas 3 a 9.
- **Asaas**: o adaptador segue a documentação oficial (criação de cliente e cobrança com
  `billingType=UNDEFINED`, consulta por id e por `externalReference`, reembolso, criação de webhook
  com `authToken`, autenticação por `asaas-access-token`), mas **não foi exercitado no sandbox**:
  exige chave de API do sandbox e URL pública para receber webhooks.
- **Revisão independente**: o repositório tem um único mantenedor; a proteção de branches exige os
  checks da CI e PR, com 0 aprovações obrigatórias. Recomenda-se exigir ao menos uma aprovação quando
  houver outro revisor.

## Ambiente de desenvolvimento observado

- O checkout está em partição vfat sem bit de execução: ferramentas Go são instaladas em
  `$(go env GOPATH)/bin`; o frontend é desenvolvido em worktree em outro sistema de arquivos ou via
  Docker.
- Docker Desktop: bind mounts de `/run/media` aparecem vazios no container; por isso o compose não
  usa bind mounts (ADR-020).
- Portas 6379 e 8081 do host já estavam ocupadas por outros projetos: Redis publicado em
  `127.0.0.1:6380` e API em `127.0.0.1:18081`.

## Ponto de retomada

Concluir os testes unitários dos pacotes de domínio (meta de 90%), abrir o PR da etapa 2, integrar
a interface web (etapa 3) e seguir para a etapa 4.
