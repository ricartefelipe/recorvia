# Visão geral da arquitetura

## C4: contexto

```mermaid
C4Context
  title Recorvia: diagrama de contexto
  Person(tenantUser, "Equipe do tenant", "Proprietário, financeiro, operador, auditor")
  Person(customer, "Cliente do tenant", "Paga as faturas pelo portal ou checkout")
  Person(staff, "Equipe da plataforma", "Opera o Recorvia sem acesso aos dados financeiros dos tenants")
  System(recorvia, "Recorvia", "Assinaturas, faturamento recorrente, cobranças, inadimplência e conciliação")
  System_Ext(idp, "Keycloak", "Identidade OIDC")
  System_Ext(psp, "Provedor de pagamento", "Asaas (sandbox/produção) ou simulador local")
  System_Ext(smtp, "SMTP", "Envio de e-mails (Mailpit no ambiente local)")
  System_Ext(merchantHook, "Sistemas do tenant", "Recebem webhooks assinados do Recorvia")
  Rel(tenantUser, recorvia, "Usa", "HTTPS")
  Rel(customer, recorvia, "Consulta faturas", "HTTPS")
  Rel(customer, psp, "Paga no checkout hospedado")
  Rel(staff, recorvia, "Catálogo de organizações")
  Rel(recorvia, idp, "Authorization Code + PKCE")
  Rel(recorvia, psp, "Cria/consulta cobranças, reembolsos")
  Rel(psp, recorvia, "Webhooks")
  Rel(recorvia, smtp, "Notificações")
  Rel(recorvia, merchantHook, "Webhooks HMAC")
```

## C4: containers

```mermaid
C4Container
  title Recorvia: diagrama de containers
  Person(user, "Usuário")
  System_Boundary(r, "Recorvia") {
    Container(web, "Interface web", "React, TypeScript, Vite, nginx", "SPA servida na mesma origem; proxy de /api, /auth e /webhooks")
    Container(api, "API", "Go, net/http, Chi", "Sessão no servidor, CSRF, autorização, idempotência, webhooks recebidos")
    Container(worker, "Worker", "Go", "Relay da outbox, consumidores, agendador, cobranças, régua, e-mails, webhooks enviados")
    ContainerDb(pg, "PostgreSQL 18", "RLS por tenant", "Dados de negócio, outbox, inbox, ledger, auditoria")
    ContainerDb(redis, "Redis 8", "", "Sessões cifradas, fluxos de login, limites de taxa")
    ContainerQueue(mq, "RabbitMQ 4", "", "Eventos de domínio, retentativas escalonadas, DLQ")
  }
  System_Ext(kc, "Keycloak")
  System_Ext(psp, "Provedor de pagamento")
  System_Ext(smtp, "SMTP", "Mailpit no ambiente local")
  System_Ext(hooks, "Sistemas do tenant", "Webhooks enviados")
  Rel(user, web, "HTTPS")
  Rel(web, api, "HTTP (mesma origem)")
  Rel(api, pg, "pgx, papel recorvia_app")
  Rel(api, redis, "Sessões")
  Rel(api, kc, "OIDC")
  Rel(worker, pg, "pgx, papel recorvia_app")
  Rel(worker, mq, "Publisher confirms, ack manual")
  Rel(worker, psp, "API do provedor")
  Rel(worker, smtp, "E-mails (TLS conforme configuração)")
  Rel(worker, hooks, "POST assinado (HMAC), cliente protegido contra SSRF")
  Rel(psp, api, "Webhooks")
```

## Módulos

| Módulo | Responsabilidade | Depende de |
|---|---|---|
| `kernel/*` | dinheiro, calendário, relógio, erros, IDs, CPF/CNPJ | — |
| `tenancy` | organizações, membros, convites, papéis e permissões | `platformbilling` |
| `identity` | OIDC, sessões, CSRF | — |
| `apicredentials` | credenciais de API com escopos, rotação, revogação e autenticação Bearer | `tenancy`, `platformbilling` |
| `portal` | links de uso único, sessão restrita, faturas e preferências de contato do portal do cliente | `customers`, `tenancy`, `billing` |
| `platformbilling` | plano do tenant no Recorvia, limites, implantação assistida | — |
| `customers` | clientes, contatos, endereço | `tenancy` (e-mail) |
| `catalog` | produtos, métricas, planos, versões de preço | — |
| `subscriptions` | ciclo de vida das assinaturas | `catalog`, `customers`, `platformbilling` |
| `billing` | faturas, motor de recorrência, créditos | `subscriptions`, `catalog`, `customers`, `ledger` |
| `payments` | cobranças, operações externas, webhooks, recebimentos | `billing`, `ledger` |
| `ledger` | partidas dobradas | — |
| `reporting` | indicadores do dashboard (fórmulas em `indicadores.md`) | — |
| `notifications` | templates, notificações por e-mail com identidade de negócio, envio com lease | `billing`, `tenancy`, `platformbilling` |
| `collections` | políticas de cobrança, régua, inadimplência, suspensão e reativação | `notifications`, `subscriptions`, `platformbilling` |
| `webhooks` | webhooks enviados: endpoints, segredos, entregas e retentativas | `platformbilling` |
| `audit`, `outbox` | trilha imutável e eventos | — |

Adaptadores: `pgstore` (PostgreSQL via sqlc), `httpapi` (HTTP), `payments/gateways/*`
(provedores), `infra/*` (RabbitMQ, Redis, SMTP em `infra/mailer`, cliente HTTP protegido contra SSRF
em `infra/safehttp`, telemetria). A composição fica em `internal/app`.

### Comunicação entre módulos

- **Na mesma transação**: casos de uso recebem uma porta `Tx` do próprio módulo; o adaptador
  `pgstore` implementa todas sobre a mesma transação pgx com contexto de tenant. Assim, fatura,
  ledger, outbox e auditoria são atômicos.
- **Assíncrona**: eventos de domínio (`invoice.issued`, `provider_webhook.received`,
  `invoice.voided`...) passam pela outbox e pelo RabbitMQ; consumidores deduplicam pelo inbox.

### Separação futura

Os limites que permitiriam extrair serviços sem reescrever regras:

1. **Pagamentos**: já se comunica por eventos (`invoice.issued` → cobrança; `payment.confirmed` →
   fatura). A extração exigiria trocar a aplicação síncrona do pagamento à fatura por um
   evento consumido pelo faturamento.
2. **Notificações e webhooks enviados**: consomem eventos e não alteram estado financeiro; são os
   primeiros candidatos.
3. **Medição de consumo**: a ingestão pode ser isolada atrás de uma fila; o fechamento dos agregados
   continua no faturamento.
4. **Ledger**: pode virar serviço com API de lançamento idempotente (a chave de idempotência já
   existe em cada lançamento).

Cada extração exigiria banco próprio por serviço e contratos de eventos versionados; por isso não
há microsserviços nesta versão.

## Executáveis

| Executável | Função |
|---|---|
| `cmd/api` | HTTP: autenticação, sessões, endpoints, recepção de webhooks |
| `cmd/worker` | relay da outbox, consumidores, agendador, recuperação de operações, régua de cobrança, envio de e-mails e entrega de webhooks |
| `cmd/migrate` | bootstrap de papéis (superusuário) e migrations (dono) |
| `cmd/seed` | dados de demonstração idempotentes |
| `cmd/paysim` | provedor de pagamento simulado |
