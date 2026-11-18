# Estratégia de multitenancy

Cada empresa contratante é um tenant. O isolamento usa defesa em profundidade:

1. **Autenticação e associação**: toda requisição autenticada resolve a organização ativa da
   sessão e valida a associação ativa (`tenancy.ResolveAccess`) contra o banco. O header
   `X-Recorvia-Tenant`, quando enviado, precisa coincidir com a organização ativa (409
   `tenant_mismatch`); ele nunca seleciona um tenant por si só.
2. **Filtro explícito**: todas as consultas do sqlc recebem `tenant_id` e filtram por ele.
3. **Chaves compostas**: cada tabela de tenant tem `UNIQUE (tenant_id, id)` e as FKs são
   compostas `(tenant_id, x_id)`, de modo que o banco recusa vínculos entre tenants.
4. **Row-Level Security**: a política `tenant_isolation` (`tenant_id = app_current_tenant()`) vale
   para leitura e escrita. `app_current_tenant()` devolve `NULL` sem contexto, o que nega acesso.
5. **Contexto local à transação**: `postgres.DB.InTenantTx` executa
   `set_config('app.tenant_id', $1, true)`; ao fim da transação o valor é descartado. O código nunca
   usa `SET` de sessão. Um teste de integração com pool de uma conexão prova que o contexto não
   vaza entre transações.
6. **Papel sem privilégios**: a aplicação conecta como `recorvia_app` (`NOSUPERUSER`,
   `NOBYPASSRLS`, `NOINHERIT`, sem posse de tabelas), com `GRANT`s mínimos. Ledger, auditoria,
   pagamentos, itens de fatura e versões de preço não têm `UPDATE`/`DELETE` para esse papel.
   Migrations rodam como `recorvia_owner`; o superusuário só cria os papéis.

## Exceções globais

| Tabela/função | Motivo |
|---|---|
| `users` | identidade global (uma pessoa em vários tenants) |
| `platform_plans`, `platform_addons` | catálogo comercial do Recorvia |
| `platform_staff` | equipe da plataforma |
| `scheduled_jobs` | agendamento técnico, sem dados de tenant |
| `tenants` | RLS pelo próprio `id` |

Funções `SECURITY DEFINER` (donas: `recorvia_owner`) expõem somente o necessário para descobrir
trabalho sem abrir dados: `app_tenants_with_due_billing`, `app_tenants_with_pending_payment_operations`,
`app_resolve_webhook_endpoint` (vínculo do webhook pelo hash do token), `app_find_invitation`,
`app_user_memberships`, `app_outbox_claim` e afins, `app_platform_tenant_catalog` (sem valores
financeiros), `app_tenants_due_reconciliation` (tenants elegíveis à conciliação agendada) e as contagens
agregadas para métricas (`app_payment_operation_stats`, `app_open_discrepancy_stats`, `app_refund_stats`). O processamento ocorre depois, dentro da transação de cada tenant.

## Workers, eventos, cache e arquivos

- Mensagens carregam `tenant_id` no header; o consumidor abre a transação daquele tenant. Mensagem
  sem tenant válido vai para a DLQ e é registrada sem tenant (visível apenas à operação).
- O inbox (`inbox_messages`) é por tenant e consumidor.
- O Redis guarda sessões (cifradas, chaveadas pelo hash do identificador) e contadores de limite;
  nenhum dado financeiro fica em cache. Na interface, as chaves de cache incluem usuário e tenant
  e o cache é limpo na troca de organização.
- Exportações CSV são geradas sob demanda na requisição do tenant (sem arquivos armazenados) e
  auditadas.
- Webhooks recebidos são vinculados ao tenant pelo token opaco do endpoint (somente o hash fica no
  banco) e autenticados conforme o provedor.

## Testes

`internal/pgstore/isolation_integration_test.go` e `internal/infra/postgres/tenantctx_integration_test.go`
usam o papel real `recorvia_app` e verificam RLS em todas as tabelas com `tenant_id`, negação sem
contexto, escrita em outro tenant negada, FKs compostas, privilégios do papel e reutilização de
conexão. `internal/httpapi/api_integration_test.go` cobre 404 entre tenants, header forjado e troca
de organização sem associação.
