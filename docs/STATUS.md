# Status da implementação

Atualizado a cada incremento. Registra o que está concluído, as verificações executadas (com
resultado real) e as pendências.

## Etapas de execução

| # | Etapa | Branch | Situação |
|---|---|---|---|
| 1 | Fundação: núcleo de dinheiro/calendário, infraestrutura, bootstrap do banco, CI | `feature/fundacao-do-projeto` | integrada em `develop` (PR #1) |
| 2 | Fatia vertical: autenticação, tenant, cliente, plano, assinatura, fatura, cobrança simulada, webhook, ledger, outbox, worker | `feature/fatia-vertical-cobranca` | integrada em `develop` (PR #2) |
| 3 | Interface web (React) e E2E | `feature/interface-web` | integrada em `develop` (PR #3) |
| 4 | Consumo medido, cupons, prorrata, troca de plano/quantidade, reajustes, notas de crédito | `feature/faturamento-avancado` | integrada em `develop` (PR #4) |
| 5 | Reembolsos, disputas, cancelamento no provedor, conciliação | `feature/recebiveis-conciliacao` | integrada em `develop` (PR #7) |
| 6 | Régua de cobrança, notificações por e-mail, webhooks enviados com proteção SSRF | `feature/cobranca-e-notificacoes` | integrada em `develop` (PR #6) |
| 7 | Portal do cliente, credenciais de API com escopos | `feature/portal-e-credenciais` | integrada em `develop` (PR #5) |
| 8 | Observabilidade (perfil opcional), alertas, runbooks, backup/restauração, análise de imagens | `feature/operacao` | integrada em `develop` (PR #8) |
| 9 | Release `0.1.0` | `release/0.1.0` | integrada em `main` com a tag `v0.1.0` |
| 10 | Pendências da 0.1.0: retenção automática, recuperação de desastre exercitada, Alertmanager e métricas de fila, portal, chargeback com crédito, conciliação assíncrona, baixa do valor a recuperar | sete branches `feature/*` | integradas em `develop` (PRs #12 a #18) |
| 11 | Release `0.2.0` | `release/0.2.0` | integrada em `main` com a tag `v0.2.0` |

A autoria das operações feitas por credencial de API foi corrigida depois das etapas 6 e 7
(`feature/autoria-credenciais-api`, PR #9).

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

### Etapa 5: reembolsos, disputas e conciliação
- **Reembolsos** totais e parciais como operação externa persistida, com reserva do valor sob
  bloqueio da cobrança (nunca acima do pago, mesmo com pedidos concorrentes), consulta ao provedor
  antes de repetir e conclusão por webhook ou consulta; a fatura não é reaberta (ADR-034).
- **Disputas e chargebacks** com retenção no ledger; na disputa perdida, o chargeback fica limitado
  ao saldo elegível e a diferença retida volta (ADR-036).
- **Cancelamento no provedor** da cobrança cancelada localmente; uma criação obsoleta só consulta o
  provedor e nunca cria a cobrança (ADR-037 e ADR-058).
- **Conciliação** manual e agendada com oito tipos de divergência, deduplicação, resolução auditada
  com ajuste no ledger e rodízio de cobranças fora da janela (ADR-038).
- Migration `00013` (a numeração 00010 foi abandonada para seguir as migrations das etapas 6 e 7).

### Etapa 6: régua de cobrança, notificações e webhooks enviados
- **Políticas de cobrança** com passos relativos ao vencimento, janela de horário no fuso do tenant,
  limite de frequência, parada após a resolução, suspensão por atraso e reativação (ADR-040, ADR-041).
- **Notificações por e-mail** com identidade de negócio deduplicada, templates de lista fechada de
  variáveis e envio SMTP com retentativas (ADR-042 a ADR-044).
- **Webhooks enviados** no formato Standard Webhooks, com rotação de segredo, retentativas com
  backoff e jitter, isolamento entre tenants e proteção SSRF validada também na conexão (ADR-045).

### Etapa 7: portal do cliente e credenciais de API
- **Portal do cliente** por link de uso único (somente o hash guardado) trocado por sessão restrita
  no Redis, com cookie próprio e CSRF (ADR-050, ADR-051).
- **Credenciais de API** `rcv_<id>_<segredo>` com escopos limitados a quem as emite, rotação com
  janela de 24 horas, revogação imediata e autenticação Bearer na mesma pilha da sessão (ADR-046 a
  ADR-049). Operações que exigem uma pessoa recusam credenciais com 403 `user_session_required`, e as
  demais gravam autoria nula com o ator `api_credential` na auditoria (ADR-059).
- **API**: todas as 127 operações do contrato implementadas.

### Etapa 8: operação
- **Perfil `observability`** com OpenTelemetry Collector, Jaeger, Prometheus e Grafana, dashboards
  provisionados e 34 regras de alerta testadas (ADR-052, ADR-053); liveness e readiness distintos
  (ADR-054).
- **Backup e restauração** com manifesto e conferência num banco novo (ADR-055); **retenção**
  limitada aos expurgos técnicos existentes (ADR-056); **análise de imagens** com Trivy na CI e
  ESLint 10 (ADR-057).
- **Runbooks** em `docs/operacao/runbooks/`.

### Etapa 10: pendências da versão 0.1.0
- **Retenção automática** (ADR-060 a ADR-062): a tarefa `retention.purge` expurga por idade, em
  lotes e por tenant, onze tipos de dado técnico, com prazos configuráveis e sem tocar no que ainda
  é necessário; registros financeiros e de auditoria ficam retidos de propósito. **Anonimização de
  cliente**, irreversível e idempotente, por sessão de usuário.
- **Recuperação de desastre exercitada** (ADR-063 e ADR-064): `make dr-check` e o job
  `disaster-recovery` destroem e restauram o ambiente com as chaves guardadas à parte, conciliam a
  janela perdida antes de religar o worker (subcomando `reconcile-pending`) e conferem que nada se
  duplicou. A API e o worker não iniciam com chave de cifragem errada ou ausente.
- **Alertmanager** (ADR-065) com envio por e-mail e webhook e **métricas de fila** sem rótulos de
  tenant: dead letters abertas, fila de notificações e de entregas de webhooks (ADR-066, ADR-067).
- **Portal**: revogação explícita e listagem de links (ADR-068) e paginação das faturas (ADR-069).
- **Recebíveis**: chargeback consome o crédito do excedente (ADR-070), conciliação manual
  assíncrona executada pelo worker (ADR-071, ADR-072) e baixa do valor a recuperar (ADR-073).
- **Repositório**: arquivos de ambiente local fora do versionamento; guia de desenvolvimento em
  `docs/DESENVOLVIMENTO.md`.
- **API**: 134 operações no contrato, todas implementadas; migrations até a `00018`.

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
| 2026-11-18 | Revisão independente da etapa 6 (2 lentes, 1 verificador por achado) | 19 defeitos confirmados; 17 corrigidos com teste e, nos 2 restantes, adotada a correção principal sugerida |
| 2026-11-18 | Revisão independente da etapa 7 (2 lentes, 1 verificador por achado) | 4 confirmados e 1 refutado; os 4 corrigidos com teste |
| 2026-11-18 | `make lint test coverage` + `npm run verify` (etapas 6 e 7) | sem achados; 97,0% e 97,4% de cobertura; 103 e 105 testes de componente aprovados |
| 2026-11-18 | CI dos PRs #5 e #6 | todos os checks aprovados, inclusive `e2e` com o roteiro `05-portal`; o #6 de novo depois de integrar `develop` |
| 2026-11-20 | Revisão independente das etapas 5 e 8 (2 lentes, 1 verificador por achado) | etapa 5: 10 defeitos distintos confirmados; etapa 8: 10 confirmados e 1 refutado; todos corrigidos com teste; a revisão das correções apontou 4 e 3 problemas, também corrigidos |
| 2026-11-20 | `make test-integration` (etapa 5) | 42 pacotes aprovados; uma execução anterior, com outra suíte em paralelo, falhou em `TestConsumerRetriesDeadLettersAndDeduplicates` por corrida no próprio teste (corrigida na release) |
| 2026-11-20 | `make obs-check` (etapa 8) | Collector e Jaeger válidos; `promtool check config`, 34 regras e `promtool test rules` aprovados; dashboards válidos |
| 2026-11-20 | `make test-integration` (etapa 8 integrada com a etapa 5) | 43 pacotes aprovados |
| 2026-11-20 | CI dos PRs #7, #8 e #9 | todos os checks aprovados; no #8 e no #9 também `backup-restore`, `observability-check`, `observability-stack` e Trivy nas 7 imagens; no #7, a primeira execução de `web-quality` falhou por notificação que vazava entre testes (corrigido) |
| 2026-11-20 | Correção da autoria por credencial (PR #9) | o teste estático apontou 17 violações nos handlers originais; 43 pacotes de integração aprovados |
| 2026-11-20 | Release: testes de integração com dependência de tempo | `internal/infra/rabbit` e `TestEndToEndDunningNotificationsAndWebhooks` corrigidos e aprovados |
| 2026-11-21 | Revisão independente das cinco frentes (2 lentes, 1 verificador por achado, revisão das correções) | retenção: 6 achados confirmados; recuperação de desastre: 10; alertas: 4; portal: 1; recebíveis: 5; todos corrigidos com teste, e os problemas apontados na revisão das correções também |
| 2026-11-21 | `make test-integration` em cada frente, depois de integrar `develop` | aprovado (44 pacotes); na recuperação de desastre, o teste que ainda esperava a conciliação síncrona foi adaptado |
| 2026-11-21 | `make dr-check` local e job `disaster-recovery` da CI | aprovados; na CI, janela perdida de 49 s e 26 s do desastre ao worker pronto (dados de demonstração); a primeira execução na CI falhou por um escopo não delegável, corrigido com teste que confere as três listas |
| 2026-11-21 | `make obs-check` (frentes de alertas e retenção) | Collector, Jaeger e Alertmanager válidos; regras e `promtool test rules` aprovados; roteamento por severidade conferido |
| 2026-11-21 | CI dos PRs #12 a #18 | todos os checks aprovados, inclusive `observability-stack` (alerta entregue por e-mail no Mailpit) e `disaster-recovery`; no #12, um teste de cifragem com falso alarme ocasional foi corrigido |
| 2026-11-21 | Baixa do valor a recuperar: concorrência | 8 pedidos simultâneos de 6,00 sobre saldo de 20,00: exatamente 3 aceitos; gravação direta no banco também recusada acima do limite |

## Pendências reais

- **Asaas**: o adaptador segue a documentação oficial e é coberto por testes de contrato, mas **não
  foi exercitado no sandbox**: exige chave de API do sandbox e URL pública para receber webhooks. O
  passo a passo está em `docs/integracoes/asaas.md`.
- **Limites dos alertas**: são pontos de partida; a calibração depende de volume real.
- **Retenção e anonimização**: os dados pessoais copiados nas faturas já emitidas permanecem (a
  fatura emitida é imutável, ADR-061); o cadastro do cliente no provedor de pagamento não é alterado;
  não há anonimização de usuários, membros e organizações.
- **Recuperação de desastre**: o exercício usa um servidor e dados de demonstração; réplicas,
  arquivamento contínuo de WAL, outra região e backup do Keycloak ficam fora. Não há meio de
  plataforma para pedir a conciliação de todas as organizações de uma vez.
- **Alertmanager**: instância única; nada alerta pela ausência do sinal diário `RecorviaWatchdog`
  fora do ambiente local.
- **Valor a recuperar**: não há estorno de baixa nem registro de recebimento depois de uma baixa
  como perda; o recebido entra no disponível sem conciliação com o extrato bancário.
- **Migrations com índices sem `CONCURRENTLY`** (00014 e 00015): em bancos grandes, a criação
  bloqueia escritas durante a aplicação.
- **Revisão independente**: o repositório tem um único mantenedor; a proteção de branches exige os
  checks da CI e PR, com 0 aprovações obrigatórias. Recomenda-se exigir ao menos uma aprovação quando
  houver outro revisor.

## Ponto de retomada

Versão `0.2.0` publicada (tag `v0.2.0` em `main`). Próximos passos sugeridos: validar o adaptador do
Asaas no sandbox (`docs/integracoes/asaas.md`), escolher onde hospedar a produção e calibrar os
limites dos alertas com o volume real.
