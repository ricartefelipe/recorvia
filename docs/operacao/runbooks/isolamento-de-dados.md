# Runbook: isolamento de dados entre tenants

Cobre a suspeita ou a confirmação de que um usuário, uma integração ou um processo acessou dados de
outra organização. O modelo de isolamento está em `docs/arquitetura/multitenancy.md` e na ADR-004:
`tenant_id` explícito nas consultas, chaves estrangeiras compostas, RLS com o papel
`recorvia_app` (`NOSUPERUSER NOBYPASSRLS`) e contexto de tenant local à transação
(`set_config('app.tenant_id', ..., true)`).

Trate a confirmação como incidente de segurança: preserve evidências, limite o acesso e só depois
corrija. Este runbook não substitui a avaliação jurídica sobre comunicação a titulares e
autoridades, que cabe aos responsáveis da organização.

## Sintomas
- Relato de usuário que vê cliente, fatura ou cobrança de outra organização.
- Registro de auditoria com `tenant_id` diferente das organizações de que o ator é membro.
- Integração (credencial de API ou webhook enviado) recebendo dados de outro tenant.
- Sinal de falha fechada (não é vazamento, mas indica contexto de tenant ausente): erros
  `postgres: tenant obrigatório` nos logs ou listas vazias inesperadas (a RLS sem contexto não
  mostra nenhuma linha).

## Métricas e alertas
Não existe alerta que detecte vazamento entre tenants: as métricas não têm rótulos de tenant
(RNF-08), por desenho. Sinais indiretos:
- `recorvia_http_requests_total{status_class="4xx"}` por `route`: aumento de 403/404 numa rota de
  tenant pode indicar tentativa de acesso cruzado (identificadores de outra organização).
- `RecorviaHTTPErrorRate` com erros `tenant obrigatório` nos logs.
- As garantias são verificadas continuamente por testes: `TestEveryTenantTableHasRLS`,
  `TestCompositeForeignKeysBlockCrossTenantRelations`, `TestServicesIsolateTenantsOnSharedConnection`
  e `TestAdvancedTablesIsolateTenantsUnderAppRole` (`internal/pgstore`),
  `TestTenantContextIsolationAndConnectionReuse` (`internal/infra/postgres`),
  `TestAPIAuthorizationAndTenantIsolation` e `TestAdvancedAPITenantIsolation`
  (`internal/httpapi`), o roteiro E2E `02-isolamento-organizacoes` e a verificação do
  `make restore-check` (RLS no banco restaurado).

## Diagnóstico

### 1. Preservar evidências
- Anote `request_id` (corpo de erro RFC 9457 e header `X-Request-ID`), horário, usuário e
  organização. Exporte os logs da janela: `docker compose logs --since 2h api worker > incidente.log`
  (o arquivo contém identificadores; guarde-o com acesso restrito).
- Não apague nem altere registros: a trilha de auditoria é imutável e é a principal evidência.

### 2. Conferir o banco (superusuário, somente leitura)
Cada consulta vem depois do comentário com o resultado esperado num banco íntegro. As consultas
marcadas com "Esperado: nenhuma linha" são executadas contra o esquema real por
`TestOperationsDocsSQLRunsAgainstSchema` (`internal/pgstore`, tag `integration`), que confere o
resultado vazio; `TestIsolationRunbookQueriesDetectViolations` confere que elas apontam uma tabela
sem RLS e uma política sem contexto de tenant.
```sql
-- Papéis: nenhum papel da aplicação pode ser superusuário ou ignorar RLS.
-- Esperado: recorvia_app e recorvia_owner com rolsuper = f e rolbypassrls = f.
SELECT rolname, rolsuper, rolbypassrls, rolinherit, rolcreaterole, rolcreatedb
  FROM pg_roles WHERE rolname LIKE 'recorvia_%';

-- Papéis da aplicação herdando privilégios de outro papel.
-- Esperado: nenhuma linha.
SELECT r.rolname AS membro, g.rolname AS grupo FROM pg_auth_members m
  JOIN pg_roles r ON r.oid = m.member JOIN pg_roles g ON g.oid = m.roleid
 WHERE r.rolname LIKE 'recorvia_%';

-- Tabelas com tenant_id sem RLS.
-- Esperado: nenhuma linha.
SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND c.relkind = 'r' AND NOT c.relrowsecurity
   AND EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped);

-- Políticas que não usam o contexto de tenant (no USING ou no WITH CHECK; sem WITH CHECK, vale o
-- USING). Todas as políticas usam app_current_tenant(), inclusive tenants_isolation, que compara
-- o id da própria organização (id = app_current_tenant()).
-- Esperado: nenhuma linha.
SELECT tablename, policyname, qual, with_check FROM pg_policies
 WHERE schemaname = 'public'
   AND (coalesce(qual, with_check) NOT LIKE '%app_current_tenant()%'
     OR coalesce(with_check, qual) NOT LIKE '%app_current_tenant()%');

-- Política da tabela tenants (referência: a única que filtra por id, não por tenant_id).
-- Esperado: tenants_isolation com (id = app_current_tenant()) no USING e no WITH CHECK.
SELECT policyname, qual, with_check FROM pg_policies WHERE schemaname = 'public' AND tablename = 'tenants';

-- Conexões da aplicação: API e worker precisam usar recorvia_app.
SELECT usename, application_name, count(*) FROM pg_stat_activity
 WHERE datname = 'recorvia' AND backend_type = 'client backend' GROUP BY 1, 2;

-- Valor de sessão (fora de transação) do contexto de tenant: deve ser vazio.
SELECT name, setting FROM pg_settings WHERE name = 'app.tenant_id';

-- app.tenant_id definido por papel ou banco (ADR-004 proíbe SET de sessão).
-- Esperado: nenhuma linha.
SELECT r.rolname, d.datname, s.setconfig FROM pg_db_role_setting s
  LEFT JOIN pg_roles r ON r.oid = s.setrole LEFT JOIN pg_database d ON d.oid = s.setdatabase
 WHERE array_to_string(s.setconfig, ',') LIKE '%app.tenant_id%';
```

### 3. Reconstituir o acesso
```sql
-- Ações registradas pela requisição suspeita.
SELECT occurred_at, tenant_id, actor_type, actor_id, action, resource_type, resource_id
  FROM audit_log WHERE request_id = '<request_id>' ORDER BY occurred_at;
-- Tenants em que o usuário agiu versus organizações de que é membro.
SELECT DISTINCT tenant_id FROM audit_log WHERE actor_id = '<user_id>' AND occurred_at > now() - interval '30 days';
SELECT tenant_id, role, status FROM memberships WHERE user_id = '<user_id>';
```
- Ação num tenant sem associação ativa: falha de autorização na API (a sessão valida a associação a
  cada requisição). Registre a rota (`route` no log de acesso) e a permissão declarada
  (`docs/arquitetura/permissoes.md`).
- Dado exibido sem registro de auditoria (leitura): reproduza com o mesmo usuário e a mesma rota
  num ambiente de teste; verifique se a consulta filtra `tenant_id` e se roda dentro de
  `InTenantTx`.
- Interface: as chaves do cache (TanStack Query) incluem usuário e organização e o cache é limpo na
  troca de organização (ADR-015). Um relato que desaparece após recarregar a página aponta para o
  cliente, não para o servidor.

### 4. Reproduzir com os testes de isolamento
```bash
go test -race -count=1 -tags integration -run 'Isolat|RLS|TenantContext|CrossTenant' \
  ./internal/pgstore/ ./internal/infra/postgres/ ./internal/httpapi/
```
Execute na versão implantada (mesma tag). Uma falha reproduz o problema; acrescente um caso com o
cenário relatado.

## Ações
1. **Conter**: retirar a rota ou a funcionalidade afetada (versão anterior ou desativação na
   borda). Encerrar sessões: as sessões ficam no Redis com o prefixo `recorvia:web:session:`; a
   remoção de todas as chaves `recorvia:web:*` desconecta todos os usuários (novo login exigido).
   Credenciais de integração envolvidas devem ser revogadas.
2. **Corrigir**: consulta sem filtro de `tenant_id`, transação sem contexto, rota sem permissão ou
   papel com privilégio indevido. Toda correção acompanha teste de regressão nas suítes de
   isolamento.
3. **Papel com `BYPASSRLS`, superusuário ou herança indevida**: restaurar os atributos do bootstrap
   (`cmd/migrate bootstrap` é idempotente e reaplica `NOSUPERUSER NOBYPASSRLS`) e investigar quem
   alterou o papel (logs do PostgreSQL).
4. **Delimitar o impacto**: com a trilha de auditoria e os logs de acesso, listar tenants e
   registros expostos, sem copiar os dados pessoais para o relatório do incidente.
5. **Comunicar** conforme a política de incidentes da organização; a decisão sobre comunicação
   externa não faz parte deste procedimento.

## Verificação
- As consultas do diagnóstico retornam o esperado (nenhuma tabela sem RLS, papéis sem
  privilégios elevados, conexões como `recorvia_app`).
- Os testes de isolamento passam na versão corrigida, incluindo o novo caso.
- A trilha de auditoria não mostra novas ações do ator fora das suas organizações.
