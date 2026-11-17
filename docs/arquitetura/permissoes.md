# Matriz de permissões

Gerada por `go run ./tools/permdoc` a partir de `internal/tenancy/permissions.go`; a CI verifica
que este arquivo está atualizado. Toda rota da API declara a permissão exigida
(`internal/httpapi/routes.go`), e o teste de contrato garante que nenhuma rota de tenant
fica sem declaração.

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
