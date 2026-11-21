# Changelog

Todas as mudanças relevantes são registradas aqui. Formato baseado em
[Keep a Changelog](https://keepachangelog.com/pt-BR/1.1.0/) e
[versionamento semântico](https://semver.org/lang/pt-BR/).

## [Não lançado]

## [0.2.0] - 2026-11-21

### Adicionado
- Expurgo automático por idade (tarefa `retention.purge` do worker, horária, com lease, por
  tenant e em lotes): inbox, dead letters tratadas, eventos, entregas e segredos vencidos de
  webhooks enviados, corpo dos webhooks recebidos do provedor, conteúdo e histórico de
  notificações, links do portal, convites não aceitos e consumo já faturado. Prazos em
  `RECORVIA_RETENTION_*_DAYS`, com mínimos validados na partida. Registros financeiros e de
  auditoria ficam retidos de propósito (ADR-060).
- Anonimização de cliente (`POST /api/v1/customers/{id}/anonymize`, permissão
  `customers.anonymize`, só por sessão de usuário), irreversível e idempotente, com a ação na
  ficha do cliente; faturas emitidas, pagamentos, ledger e auditoria permanecem (ADR-061).
- Exercício automatizado de recuperação de desastre (`make dr-check` e job `disaster-recovery`):
  backup com as chaves guardadas à parte, destruição, restauração num servidor novo, recusa de
  chave errada ou ausente, conciliação da janela perdida antes de religar o worker e conferência
  de que nada se duplicou, com RPO e RTO observados.
- Conferência das chaves de cifragem contra o banco na partida da API e do worker e no subcomando
  `keyring-check` (`RECORVIA_ENCRYPTION_STARTUP_CHECK`: `enforce`, `warn` ou `off`), e declaração
  de chave perdida (`RECORVIA_ENCRYPTION_LOST_KEYS`) (ADR-063 e ADR-064).
- Subcomando `reconcile-pending` do worker: executa as conciliações registradas e termina, sem
  agendador, consumidores nem publicador da outbox.
- Alertmanager no perfil `observability`: roteamento por severidade, agrupamento, inibição e
  repetição, envio por e-mail e por webhook configurado por variáveis de ambiente, link do
  runbook em cada notificação (ADR-065).
- Métricas de fila sem rótulos de tenant: dead letters abertas por consumidor, fila de
  notificações e de entregas de webhooks, com a idade do item mais antigo; alertas, painéis e
  runbooks correspondentes (ADR-066 e ADR-067).
- Links do portal: listagem e revogação explícita
  (`GET` e `DELETE /api/v1/customers/{id}/portal-links`), que encerra as sessões abertas pelo link;
  aba Portal na ficha do cliente (ADR-068).
- Paginação por cursor das faturas do portal do cliente (ADR-069).
- Chargeback de pagamento com excedente: a disputa perdida consome o crédito do cliente gerado
  pelo excedente; o crédito já usado vira valor a recuperar, na conta `customer_recoverables`
  (ADR-070).
- `GET /api/v1/reconciliation/runs/{id}` para acompanhar uma execução de conciliação, e executor
  de conciliações no worker, com lease e retomada sem duplicar divergências (ADR-071).
- Métricas e alertas das execuções de conciliação à espera do worker e do expurgo por idade.
- Baixa do valor a recuperar das disputas perdidas, total ou parcial, como recebido (`cash`) ou
  perdido (`bad_debt`), com justificativa, data efetiva, auditoria e evento
  `dispute.recovery_settled`; o limite é garantido pelo banco, inclusive com pedidos simultâneos
  (`GET` e `POST /api/v1/disputes/{id}/recovery-settlements`, `GET /api/v1/disputes/{id}`,
  permissão `payments.settle_recovery`). A tela de disputas mostra o saldo a recuperar e o painel
  ganha o indicador "Valores a recuperar de clientes" (ADR-073).
- Migrations `00014` a `00018` (a numeração `00016` não foi usada).

### Alterado
- **Contrato:** `POST /api/v1/reconciliation/runs` passou a ser assíncrono: registra a execução e
  responde `202` com `Location`; antes respondia `200` com a execução encerrada (ADR-072).
  Respostas `202` não ficam registradas na `Idempotency-Key`.
- **Contrato:** `GET /api/portal/v1/invoices` devolve 25 faturas por página por padrão (antes, até
  100 sem paginação); `POST /api/v1/customers/{id}/portal-links` devolve também o `id` do link.
- A API e o worker não iniciam quando as chaves de cifragem não decifram os dados em uso; antes
  iniciavam, e o worker encerrava notificações e entregas de webhook sem volta.
- `customers.anonymize` não pode ser concedida a credenciais de API; cliente anonimizado recusa
  novos contatos, endereço e assinaturas.
- O indicador de chargebacks do painel financeiro deixa de incluir a parte que era pagamento
  excedente.
- O guia de desenvolvimento passa a ser `docs/DESENVOLVIMENTO.md`; arquivos de ambiente local
  ficam fora do repositório.

### Corrigido
- O chargeback de um pagamento com excedente deixava o crédito com o cliente e registrava como
  receita estornada um valor que nunca foi receita.
- Credenciais de provedor que não decifram respondem 503 `provider_credentials_unreadable` em vez
  de 500; a reconfiguração do provedor não depende mais das credenciais anteriores.
- Teste de cifragem das notificações que acusava falso alarme por coincidência nos bytes
  cifrados.

### Removido
- `portal.Service.RevokeLinks` (revogação em massa não exposta por nenhuma rota), substituída pela
  revogação por identificador.

## [0.1.0] - 2026-11-20

### Adicionado
- Núcleo de dinheiro em unidades mínimas com aritmética protegida contra
  overflow, cálculo racional exato, arredondamento half-even e alocação por
  maior resto.
- Calendário de ciclos com âncora preservada, períodos `[início, fim)` e
  prorrata por dias civis no fuso do tenant.
- Configuração validada na inicialização, logs estruturados com redação e
  transações PostgreSQL com contexto de tenant local.
- Bootstrap de papéis do banco (`recorvia_owner`, `recorvia_app`) e migration
  de fundação com RLS.
- CI com formatação, vet, lint, testes com detector de corridas, cobertura,
  fuzz, integração, govulncheck e detecção de segredos.
- Organizações, membros, convites com expiração e uso único, papéis e
  permissões explícitas; plano da plataforma com limites e demonstrativo mensal.
- Login OIDC (Authorization Code + PKCE) com sessão no servidor, cookie
  `HttpOnly`/`Secure`/`SameSite`, CSRF e troca de organização com rotação.
- Clientes PF/PJ, catálogo com versões de preço imutáveis e quatro modelos de
  preço, assinaturas com máquina de estados e motor de recorrência com retomada.
- Faturas com itens, snapshot de preço e numeração única por tenant; ledger de
  partidas dobradas; créditos de clientes.
- Cobrança por provedor com simulador determinístico e adaptador do Asaas
  (não exercitado no sandbox), operações externas com estado `unknown`,
  webhooks autenticados e processamento assíncrono idempotente.
- Outbox com lease, RabbitMQ com confirmação, ack manual, retentativas e DLQ;
  agendador com lease; `Idempotency-Key`; `If-Match`; erros RFC 9457.
- Ambiente local com Docker Compose (PostgreSQL, Redis, RabbitMQ, Keycloak,
  Mailpit, simulador, API, worker e seed de dois tenants).
- Interface web em português (React, TypeScript, TanStack Query, Mantine) com
  tipos gerados do contrato OpenAPI, testes de componente e roteiros E2E com
  Playwright; servida por nginx sem root na mesma origem da API.

- Cupons de desconto, aplicação e remoção na assinatura.
- Mudança de plano e quantidade imediata (prorrata com fatura ou crédito do
  cliente) ou no próximo ciclo; reajustes programados com histórico.
- Ingestão de consumo medido com deduplicação, política de atraso por métrica e
  correção posterior sem alterar a fatura emitida.
- Notas de crédito limitadas ao saldo em aberto, com ajuste da cobrança.
- Reembolsos totais e parciais como operação externa persistida: o valor é
  reservado sob bloqueio da cobrança (nunca acima do pago, mesmo com pedidos
  concorrentes), o provedor é consultado antes de repetir depois de resposta
  ambígua e reembolsos feitos direto no provedor são registrados.
- Disputas e chargebacks com retenção no ledger, estorno na disputa ganha e
  chargeback limitado ao saldo elegível na disputa perdida.
- Cancelamento da cobrança no provedor quando ela é cancelada localmente
  (fatura cancelada, nota de crédito, substituição); uma criação obsoleta
  nunca cria a cobrança no provedor.
- Conciliação manual e agendada com oito tipos de divergência, deduplicação
  entre execuções, resolução com responsável, justificativa, auditoria e
  ajuste no ledger, e rodízio de cobranças fora da janela para detectar
  chargebacks e reembolsos tardios.
- Régua de cobrança por política do tenant: passos relativos ao vencimento,
  janela de horário no fuso do tenant, limite de frequência, parada após a
  resolução da fatura, suspensão por atraso e reativação.
- Templates de notificação com lista fechada de variáveis e escape de HTML;
  notificações por e-mail com identidade de negócio deduplicada, retentativas
  e respeito às preferências de contato.
- Webhooks enviados no formato Standard Webhooks (HMAC-SHA256 com timestamp,
  rotação de segredo com dupla assinatura, retentativas com backoff e jitter,
  reenvio manual) com proteção contra SSRF validada também no momento da
  conexão.
- Portal do cliente por link de uso único (somente o hash do token é
  guardado) trocado por sessão restrita, com faturas, documentos e
  preferências de contato.
- Credenciais de API com escopos limitados às permissões de quem as emite,
  segredo de 256 bits guardado só como hash, expiração, rotação, revogação e
  autenticação `Authorization: Bearer`.
- Todas as 127 operações do contrato OpenAPI implementadas.
- Perfil opcional de observabilidade (OpenTelemetry Collector, Jaeger,
  Prometheus e Grafana) com dashboards provisionados e 34 regras de alerta
  testadas com `promtool test rules`; liveness e readiness distintos na API e
  no worker; métricas operacionais sem rótulos de alta cardinalidade.
- Backup lógico do PostgreSQL com manifesto e restauração verificada num banco
  novo (`make backup`, `make restore-check`), exercitados na CI.
- Análise das imagens com Trivy na CI, falhando em vulnerabilidades HIGH ou
  CRITICAL com correção disponível.
- Runbooks de faturamento, filas e DLQ, conciliação, notificações e webhooks
  e isolamento de dados; política de retenção e anonimização documentada.

### Corrigido
- Ciclo mensal da plataforma sobrepunha períodos e perdia a âncora dos dias 29
  a 31; a avaliação parcial cobrava o mês inteiro.
- Caminho de retorno do login aceitava caracteres de controle (redirecionamento
  para outro domínio após remoção de TAB pelo navegador).
- Recuperação de fatura incobrável não estornava a perda e quitava a fatura
  com valor ainda baixado; taxa do provedor ignorada em pagamento de fatura já
  quitada; repetição de pagamento manual após quitação era recusada.
- Falha de uma assinatura interrompia o faturamento de todo o tenant.
- Confirmação do provedor para cobrança recusada ou cancelada localmente era
  ignorada; o valor recebido passa a ser aplicado.
- Chave de negócio derivada do `Idempotency-Key` passa a ter tamanho fixo
  (chaves longas provocavam erro interno).
- Credenciais de API recebiam erro interno ao criar políticas de cobrança,
  personalizar templates e criar endpoints de webhook (autoria `uuid.Nil`
  violava a FK para `users`); a autoria passa a ser nula, com o ator
  `api_credential` na auditoria. Gestão de membros, convites, plano da
  plataforma e credenciais recusa credenciais com 403
  `user_session_required`, e o tratamento de dead letters grava a auditoria,
  com a justificativa, na mesma transação da resolução (ADR-059).

[Não lançado]: https://github.com/ricartefelipe/recorvia/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/ricartefelipe/recorvia/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ricartefelipe/recorvia/releases/tag/v0.1.0
