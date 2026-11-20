# Estratégia de multitenancy

Cada empresa contratante é um tenant. O isolamento usa defesa em profundidade:

1. **Autenticação e associação**: toda requisição autenticada resolve a organização ativa da
   sessão e valida a associação ativa (`tenancy.ResolveAccess`) contra o banco. O header
   `X-Recorvia-Tenant`, quando enviado, precisa coincidir com a organização ativa (409
   `tenant_mismatch`); ele nunca seleciona um tenant por si só. Com `Authorization: Bearer rcv_…`,
   o tenant é o da credencial de API, e o header, se enviado, precisa coincidir com ele. A sessão do
   portal do cliente carrega o tenant e o cliente do link e é revalidada a cada requisição; as rotas
   do portal não aceitam `X-Recorvia-Tenant` como seletor nem identificador de cliente na requisição.
2. **Filtro explícito**: todas as consultas do sqlc recebem `tenant_id` e filtram por ele. As
   consultas do portal filtram também `customer_id` (o cliente da sessão) e excluem rascunhos.
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
`app_find_api_credential` (credencial pelo identificador público do token), `app_find_portal_link`
(link do portal pelo hash do token), `app_user_memberships`, `app_outbox_claim` e afins,
`app_platform_tenant_catalog` (sem valores financeiros), `app_tenants_for_collections`,
`app_tenants_with_due_notifications` e `app_tenants_with_due_webhook_deliveries` (estas três
paginadas pelo ID do tenant, com rodízio entre execuções), `app_tenants_due_reconciliation` (tenants
elegíveis à conciliação agendada) e as contagens agregadas para métricas
(`app_payment_operation_stats`, `app_open_discrepancy_stats`, `app_refund_stats`). O processamento
ocorre depois, dentro da transação de cada tenant.

## Workers, eventos, cache e arquivos

- Mensagens carregam `tenant_id` no header; o consumidor abre a transação daquele tenant. Mensagem
  sem tenant válido vai para a DLQ e é registrada sem tenant (visível apenas à operação).
- O inbox (`inbox_messages`) é por tenant e consumidor.
- O Redis guarda sessões do painel e do portal (cifradas, chaveadas pelo hash do identificador, em
  namespaces separados), contadores de limite de taxa e contadores de tentativas inválidas. Os de
  limite de taxa são chaveados pelo escopo e pelo sujeito em claro (IP, usuário, credencial ou
  cliente do portal) e expiram com a janela de um minuto; somente os de tentativas inválidas usam o
  hash da origem. Nenhum dado financeiro fica em cache. Na interface, as chaves de cache incluem
  usuário e tenant e o cache é limpo na troca de organização.
- Exportações CSV são geradas sob demanda na requisição do tenant (sem arquivos armazenados) e
  auditadas.
- Webhooks recebidos são vinculados ao tenant pelo token opaco do endpoint (somente o hash fica no
  banco) e autenticados conforme o provedor.
- Conteúdo das notificações e segredos dos webhooks enviados ficam cifrados com o keyring, com AAD
  que inclui o tenant e o registro (o ciphertext não pode ser copiado para outro tenant).

## Testes

`internal/pgstore/isolation_integration_test.go` e `internal/infra/postgres/tenantctx_integration_test.go`
usam o papel real `recorvia_app` e verificam RLS em todas as tabelas com `tenant_id`, negação sem
contexto, escrita em outro tenant negada, FKs compostas, privilégios do papel e reutilização de
conexão. `internal/httpapi/api_integration_test.go` cobre 404 entre tenants, header forjado e troca
de organização sem associação. `internal/pgstore/access_integration_test.go` e
`internal/httpapi/access_integration_test.go` cobrem links do portal e credenciais de API entre
tenants (recurso de outro tenant responde 404, header divergente da credencial responde 409 e FKs
compostas recusam vínculos cruzados). `internal/pgstore/portal_invoices_integration_test.go` e
`internal/httpapi/portal_integration_test.go` cobrem o escopo de dados do portal: faturas, documento
e contatos de outro cliente ou de outro tenant respondem 404, rascunhos não aparecem e o contexto de
outro tenant não enxerga as faturas mesmo com o identificador do cliente.
