# Changelog

Todas as mudanças relevantes são registradas aqui. Formato baseado em
[Keep a Changelog](https://keepachangelog.com/pt-BR/1.1.0/) e
[versionamento semântico](https://semver.org/lang/pt-BR/).

## [Não lançado]

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
