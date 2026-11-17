# Status da implementação

Atualizado a cada incremento. Registra o que está concluído, as verificações executadas (com
resultado real) e as pendências.

## Etapas de execução

| # | Etapa | Branch | Situação |
|---|---|---|---|
| 1 | Fundação: núcleo de dinheiro/calendário, infraestrutura, bootstrap do banco, CI | `feature/fundacao-do-projeto` | integrada em `develop` (PR #1) |
| 2 | Fatia vertical: autenticação, tenant, cliente, plano, assinatura, fatura, cobrança simulada, webhook, ledger, outbox, worker | `feature/fatia-vertical-cobranca` | integrada em `develop` (PR #2) |
| 3 | Interface web (React) e E2E | `feature/interface-web` | integrada em `develop` (PR #3) |
| 4 | Consumo medido, cupons, prorrata, troca de plano/quantidade, reajustes, notas de crédito | `feature/faturamento-avancado` | em revisão |
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

### Etapa 3: interface web
React 19 + TypeScript + Vite, TanStack Query (chaves com usuário e organização; cache descartado
na troca de organização), React Router, React Hook Form + Zod e Mantine (ADR-015). Tipos gerados do
contrato OpenAPI (`web/src/api/schema.d.ts`, verificados na CI). Telas: entrada e seleção de
organização, onboarding e convites, dashboard com fórmulas, clientes, catálogo, assinaturas,
faturas, pagamentos, ledger, conciliação, cobrança, membros, integrações, auditoria, operações,
plano da plataforma e portal do cliente. Servida por nginx sem root na mesma origem da API
(ADR-025).

### Etapa 4: faturamento avançado
- **Cupons** com validade, limite de usos e planos elegíveis; aplicação e remoção na assinatura.
- **Mudança de plano e quantidade** imediata (prorrata por dias civis: fatura de prorrata ou crédito
  do cliente) ou no próximo ciclo (versão de preço fixada no agendamento).
- **Reajustes programados** (percentual ou novo valor) aplicados no primeiro ciclo iniciado na data
  efetiva, com valor exato para preço por unidade e histórico.
- **Consumo medido**: ingestão em lote com deduplicação por `external_id`, validação de unidade,
  escala e limites, política de atraso por métrica, serialização com o fechamento do período e
  correção posterior sem alterar a fatura emitida.
- **Notas de crédito** limitadas ao saldo em aberto, com ledger e ajuste da cobrança.
- **API**: 93 das 127 operações do contrato implementadas.

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
| 2026-11-17 | `make lint`, `make test`, `make coverage` | sem achados; todos aprovados; 97,0% de cobertura em domínio e aplicação (mínimo 90%) |
| 2026-11-17 | Testes unitários novos | 11 bugs reais encontrados e corrigidos (âncora da plataforma, redirecionamento por TAB, baixa e recuperação de incobráveis, taxa do provedor, repetição de pagamento manual, isolamento de falhas no motor, rejeições que alteravam a fatura) |
| 2026-11-17 | `make reset && make up` (volumes novos) + roteiro automatizado via API | aprovado novamente após as correções; corrigidos no caminho: RabbitMQ executado como `rabbitmq` (cookie criado pelo healthcheck como root impedia a partida) e seed fora do `up --wait` |
| 2026-11-17 | `cd web && npm ci && npm run verify` | tipos e lint sem erros; 17 arquivos, 95 testes de componente aprovados; build concluído |
| 2026-11-17 | `make up` + `make web-e2e` (Playwright, Chromium) | 4 roteiros aprovados: login no Keycloak e cadastro, faturamento, cobrança e checkout pela interface; pagamento no simulador com fatura paga, ledger e auditoria; isolamento entre organizações; auditor sem ações de escrita |
| 2026-11-17 | Revisão independente da etapa 4 (3 revisores e 1 verificador por achado) | 26 achados, 22 confirmados e 4 refutados; os 22 (17 problemas distintos, somado o bug da prorrata encontrado pelos testes) corrigidos, cada um com o teste que o demonstrava passando |
| 2026-11-17 | `make lint test coverage` + `make test-integration` (etapa 4) | sem achados; todos aprovados; 97,1% de cobertura em domínio e aplicação (`metering` 99,2%, `billing` 94,6%, `subscriptions` 98,4%) |
| 2026-11-17 | `cd web && npm run verify` (etapa 4) | 17 arquivos e 97 testes de componente aprovados; tipos gerados do contrato atualizados |
| 2026-11-17 | Segunda rodada de revisão independente (3 frentes, 1 revisor por frente) | 8 pontos residuais corrigidos (base do crédito da prorrata pelo valor faturado, desconto de valor fixo na correção de consumo, limite numérico, consumo anterior à suspensão, mudança agendada em vigor, pagamento após recusa, ordem de bloqueio, resposta da nova tentativa) e 4 casos de borda adicionais; revisores sem achados de severidade alta ou média |
| 2026-11-17 | `make lint test coverage fuzz test-integration` (final da etapa 4) | sem achados; todos aprovados; 97,1% de cobertura |
| 2026-11-17 | `make reset && make up` + `make web-e2e` (etapa 4) | 9 migrations aplicadas em banco novo; 4 roteiros E2E aprovados |
| 2026-11-17 | Stack local (`docker compose up`) + roteiro automatizado via API | login real no Keycloak com PKCE, cliente, plano, assinatura, fatura `ACME-2026-000001`, cobrança no simulador, pagamento pelo checkout, webhook HMAC, fatura paga, ledger (receita 99.000; liquidação 95.991; taxa 3.009), reenvio sem duplicidade, auditoria, dashboard e isolamento entre tenants |

## Pendências reais

- Etapas 5 a 9.
- **Telas que dependem de etapas futuras**: conciliação, reembolsos e disputas (etapa 5), políticas
  de cobrança, templates e webhooks enviados (etapa 6), portal do cliente e credenciais de API
  (etapa 7) já consomem o contrato, mas as rotas ainda não existem; até lá a tela mostra o estado de
  erro com nova tentativa.
- **ESLint 9.39.5** está fora do período de suporte do projeto; a migração para a versão 10 fica
  para uma atualização dedicada de dependências do frontend.
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

Integrar a interface web (PR da etapa 3) e concluir a etapa 4 (`feature/faturamento-avancado`:
código de cupons, mudanças agendadas, prorrata, reajustes, correção de consumo, notas de crédito e
ingestão de consumo commitado; faltam testes unitários e de integração e o PR).
