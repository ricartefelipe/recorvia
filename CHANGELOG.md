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

### Corrigido
- Ciclo mensal da plataforma sobrepunha períodos e perdia a âncora dos dias 29
  a 31; a avaliação parcial cobrava o mês inteiro.
- Caminho de retorno do login aceitava caracteres de controle (redirecionamento
  para outro domínio após remoção de TAB pelo navegador).
- Recuperação de fatura incobrável não estornava a perda e quitava a fatura
  com valor ainda baixado; taxa do provedor ignorada em pagamento de fatura já
  quitada; repetição de pagamento manual após quitação era recusada.
- Falha de uma assinatura interrompia o faturamento de todo o tenant.
