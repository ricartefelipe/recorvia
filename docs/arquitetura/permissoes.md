# Matriz de permissões

Gerada por `go run ./tools/permdoc` a partir de `internal/tenancy/permissions.go` e das rotas
registradas em `internal/httpapi`; a CI verifica que este arquivo está atualizado. Toda rota da
API declara a permissão exigida (`internal/httpapi/routes.go`), e o teste de contrato garante
que nenhuma rota de tenant fica sem declaração.

O acesso à organização é validado a cada requisição contra a associação ativa do usuário;
o identificador de tenant recebido nunca é aceito isoladamente.

| Permissão | owner | admin | finance | operator | auditor |
|---|:-:|:-:|:-:|:-:|:-:|
| `api_credentials.manage` | ✔ | ✔ | — | — | — |
| `audit.read` | ✔ | ✔ | — | — | ✔ |
| `billing.run` | ✔ | ✔ | ✔ | — | — |
| `catalog.read` | ✔ | ✔ | ✔ | ✔ | ✔ |
| `catalog.write` | ✔ | ✔ | — | — | — |
| `collections.read` | ✔ | ✔ | ✔ | ✔ | ✔ |
| `collections.write` | ✔ | ✔ | ✔ | — | — |
| `credits.write` | ✔ | ✔ | ✔ | — | — |
| `customers.portal_links` | ✔ | ✔ | ✔ | ✔ | — |
| `customers.read` | ✔ | ✔ | ✔ | ✔ | ✔ |
| `customers.write` | ✔ | ✔ | — | ✔ | — |
| `dashboard.read` | ✔ | ✔ | ✔ | ✔ | ✔ |
| `integrations.read` | ✔ | ✔ | ✔ | — | ✔ |
| `integrations.write` | ✔ | ✔ | — | — | — |
| `invoices.export` | ✔ | ✔ | ✔ | — | — |
| `invoices.read` | ✔ | ✔ | ✔ | ✔ | ✔ |
| `invoices.void` | ✔ | ✔ | ✔ | — | — |
| `invoices.write` | ✔ | ✔ | ✔ | — | — |
| `ledger.read` | ✔ | ✔ | ✔ | — | ✔ |
| `members.manage` | ✔ | ✔ | — | — | — |
| `members.read` | ✔ | ✔ | ✔ | ✔ | ✔ |
| `operations.read` | ✔ | ✔ | ✔ | — | ✔ |
| `operations.reprocess` | ✔ | ✔ | — | — | — |
| `organization.read` | ✔ | ✔ | ✔ | ✔ | ✔ |
| `organization.update` | ✔ | ✔ | — | — | — |
| `payments.charge` | ✔ | ✔ | ✔ | — | — |
| `payments.read` | ✔ | ✔ | ✔ | ✔ | ✔ |
| `payments.record_manual` | ✔ | ✔ | ✔ | — | — |
| `payments.refund` | ✔ | ✔ | ✔ | — | — |
| `platform_billing.manage` | ✔ | — | — | — | — |
| `platform_billing.read` | ✔ | ✔ | ✔ | — | ✔ |
| `reconciliation.read` | ✔ | ✔ | ✔ | — | ✔ |
| `reconciliation.resolve` | ✔ | ✔ | ✔ | — | — |
| `reconciliation.run` | ✔ | ✔ | ✔ | — | — |
| `subscriptions.cancel` | ✔ | ✔ | ✔ | — | — |
| `subscriptions.read` | ✔ | ✔ | ✔ | ✔ | ✔ |
| `subscriptions.write` | ✔ | ✔ | — | ✔ | — |
| `usage.read` | ✔ | ✔ | ✔ | ✔ | ✔ |
| `usage.write` | ✔ | ✔ | — | ✔ | — |
| `webhooks.read` | ✔ | ✔ | ✔ | — | ✔ |
| `webhooks.write` | ✔ | ✔ | — | — | — |

Papéis: `owner` (proprietário), `admin` (administrador), `finance` (financeiro),
`operator` (operador) e `auditor` (somente leitura, incluindo auditoria).

Regras adicionais aplicadas nos casos de uso:

- Somente proprietários convidam, promovem, rebaixam ou removem proprietários.
- A organização mantém ao menos um proprietário ativo.
- O plano da plataforma (`platform_billing.manage`) é exclusivo do proprietário.
- A equipe da plataforma (`platform_staff`) vê apenas o catálogo de organizações, sem dados
  financeiros; acesso a dados de um tenant exige convite do próprio tenant e fica auditado.
- Credenciais de API recebem como escopos somente permissões de quem as emite; os escopos
  efetivos acompanham o papel atual dessa pessoa.
  Não são delegáveis: `members.manage`, `platform_billing.manage`, `api_credentials.manage`.
- Rotas de conta e de sessão e as rotas que exigem permissão não delegável aceitam apenas a
  sessão de um usuário (credenciais recebem 403 `user_session_required`); nas demais, as ações
  por credencial têm autoria nula nas tabelas e o ator `api_credential` na auditoria (ADR-059).
  O portal do cliente tem sessão própria, restrita ao cliente e à organização do link.

## Rotas restritas à sessão de usuário

Geradas a partir das rotas registradas em `internal/httpapi`. Uma credencial de API recebe 403
`user_session_required` nestas rotas antes da verificação de escopos. Todas as demais rotas de
organização, inclusive as leituras de membros, papéis, convites e do plano da plataforma,
aceitam credenciais pelos escopos.

| Rota | Contexto | Permissão |
|---|---|---|
| `POST /auth/logout` | conta e sessão | — |
| `GET /api/v1/session` | conta e sessão | — |
| `PUT /api/v1/session/tenant` | conta e sessão | — |
| `POST /api/v1/organizations` | conta e sessão | — |
| `POST /api/v1/invitations/accept` | conta e sessão | — |
| `PATCH /api/v1/members/{user_id}` | organização ativa | `members.manage` |
| `DELETE /api/v1/members/{user_id}` | organização ativa | `members.manage` |
| `POST /api/v1/invitations` | organização ativa | `members.manage` |
| `DELETE /api/v1/invitations/{id}` | organização ativa | `members.manage` |
| `PUT /api/v1/platform/plan` | organização ativa | `platform_billing.manage` |
| `POST /api/v1/platform/addon-orders` | organização ativa | `platform_billing.manage` |
| `GET /api/v1/api-credentials` | organização ativa | `api_credentials.manage` |
| `POST /api/v1/api-credentials` | organização ativa | `api_credentials.manage` |
| `POST /api/v1/api-credentials/{id}/rotate` | organização ativa | `api_credentials.manage` |
| `DELETE /api/v1/api-credentials/{id}` | organização ativa | `api_credentials.manage` |
