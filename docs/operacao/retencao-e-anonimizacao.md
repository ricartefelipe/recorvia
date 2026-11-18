# Retenção e anonimização de dados

Este documento descreve **o que o Recorvia faz hoje** com o tempo de vida dos dados, o que **não é
automatizado** e como tratar pedidos de eliminação ou anonimização enquanto não houver um caso de
uso próprio (ADR-056). Não é um parecer jurídico nem uma declaração de conformidade com qualquer
legislação: prazos legais de guarda (fiscais, contábeis, contratuais) e o tratamento de pedidos de
titulares são definidos pelos responsáveis de cada organização.

## Expurgos automáticos existentes

Executados pelo agendador do worker (`scheduled_jobs`, com lease; ADR-011):

| Tarefa | Frequência | Efeito | Função |
|---|---|---|---|
| `outbox.purge` | diária | remove eventos da outbox **publicados** há mais de 7 dias; pendentes nunca são removidos | `app_outbox_purge(7)` |
| `idempotency.purge` | horária | remove chaves de idempotência **concluídas** e expiradas (validade de 24 h); chaves em processamento permanecem | `app_idempotency_purge_expired()` |

Observações:
- A proteção de longo prazo contra repetição de operações financeiras não depende das chaves
  expurgadas: as chaves de negócio derivadas (`payments.request_key`,
  `customer_credit_entries.request_key`, `credit_notes.request_key`) são permanentes (ADR-022).
- Os eventos da outbox carregam identificadores e valores (por exemplo, `invoice_id`, valor da
  cobrança e link de checkout); depois do expurgo, a mensagem original deixa de ser consultável pelo
  `message_id` na outbox, mas o registro em `dead_letters` (quando houver) permanece.

## Expiração fora do PostgreSQL

| Dado | Onde | Tempo de vida |
|---|---|---|
| Sessão do usuário (cifrada) | Redis | absoluto de 12 h e inatividade de 2 h (`RECORVIA_SESSION_TTL`, `RECORVIA_SESSION_IDLE_TTL`) |
| Índice de sessões por usuário | Redis | 24 h, renovado a cada sessão |
| Fluxo de login (state, nonce, PKCE) | Redis | 10 min, uso único |
| Contadores de limite de taxa | Redis | janela do limite mais 1 s |
| Rastros (spans) | Jaeger do perfil `observability` | somente em memória (até 50.000 rastros); perdidos ao reiniciar |
| Métricas | Prometheus do perfil `observability` | 7 dias (`--storage.tsdb.retention.time=7d`) |
| Logs | saída padrão dos containers | conforme o driver de logs do Docker ou do orquestrador; o Recorvia não define retenção |

No ambiente local, o Redis roda sem persistência (`--save ""`, `--appendonly no`). Os logs redigem
chaves sensíveis (senhas, tokens, cookies, credenciais) e mascaram documento, e-mail e telefone
(`internal/infra/logging`). Os spans exportados pelo Collector não levam o token do caminho dos
webhooks, a consulta da URL nem o endereço do cliente (ADR-052); as métricas não têm rótulos de
tenant, cliente ou fatura.

## Dados retidos sem expurgo

Tudo o que não está nas tabelas acima é mantido enquanto o banco existir, inclusive depois de
encerrar assinaturas, arquivar clientes ou desativar organizações. Em particular:

| Categoria | Tabelas | Observação |
|---|---|---|
| Registros financeiros imutáveis | `ledger_transactions`, `ledger_entries`, `payments`, `invoice_items`, `invoice_status_history`, `credit_notes`, `customer_credit_entries`, `subscription_prorations` | gatilhos rejeitam `UPDATE` e `DELETE` (ADR-014); correções por lançamento compensatório |
| Faturas | `invoices` (inclui `customer_snapshot` com nome, documento e contato do cliente na emissão) | a fatura emitida é protegida contra alteração |
| Trilha de auditoria | `audit_log` | imutável; `details` pode conter identificadores e valores alterados |
| Cadastro de clientes | `customers`, `customer_contacts`, `customer_billing_addresses`, `customer_provider_refs` | arquivar (`archived`) não apaga dados; a coluna `customers.anonymized_at` existe no modelo, mas nenhum caso de uso a preenche |
| Usuários e membros | `users` (e-mail, nome), `memberships`, `invitations` | a remoção de membro muda a situação, sem apagar |
| Integração com provedores | `provider_webhook_events` (corpo recebido do provedor), `charges`, `payment_operations` (`provider_response`), `payment_provider_accounts` (credenciais cifradas com AES-256-GCM) | o corpo do webhook pode conter dados do pagador enviados pelo provedor |
| Mensageria | `inbox_messages`, `dead_letters` (payload e headers do evento) | sem expurgo; crescem com o volume de eventos |
| Histórico e consumo | `subscription_history`, `usage_events`, `usage_aggregates` | `usage_events.properties` contém o que a integração do tenant enviar |
| Backups | arquivos gerados por `make backup` | retenção a cargo de quem opera (ver [backup](backup-e-restauracao.md)) |

## O que não é automatizado

- Anonimização ou eliminação de clientes, contatos, usuários ou organizações.
- Expurgo de `inbox_messages`, `dead_letters` resolvidas, `provider_webhook_events`,
  `audit_log` e `usage_events` por idade.
- Encerramento de uma organização com exportação e eliminação dos seus dados.
- Retenção dos backups, dos logs e dos dados do Keycloak (contas de usuário no provedor de
  identidade).
- Registro formal de pedidos de titulares (não há tela nem endpoint).

## Procedimento manual para pedidos de anonimização (recomendação)

Enquanto não houver caso de uso com migration, auditoria e testes, um pedido de anonimização de
cliente deve ser tratado assim:

1. **Registrar o pedido** fora do sistema (quem pediu, quando, qual cliente, decisão e
   justificativa) e confirmar com os responsáveis pela organização se há obrigação de guarda que
   impeça a eliminação (por exemplo, de documentos comerciais e financeiros).
2. **Arquivar o cliente** pela API (`POST /api/v1/customers/{id}/archive`), o que impede novas
   assinaturas e cobranças, e encerrar as assinaturas ativas.
3. **Não alterar tabelas imutáveis** (ledger, pagamentos, itens e histórico de faturas, notas de
   crédito, auditoria): os gatilhos rejeitam a operação e a alteração quebraria a rastreabilidade
   financeira. O `customer_snapshot` das faturas emitidas também é protegido.
4. **Dados cadastrais mutáveis** (nome, e-mail, telefone, contatos, endereço) podem ser
   substituídos por valores neutros pela API de clientes (`PATCH /api/v1/customers/{id}` com
   `If-Match`, `DELETE /api/v1/customers/{id}/contacts/{contact_id}` e
   `PUT /api/v1/customers/{id}/billing-address`), com registro na auditoria. O documento
   (CPF/CNPJ) identifica o devedor nas faturas emitidas, que guardam a cópia do cadastro do
   momento da emissão.
5. **Provedor de pagamento**: o cadastro do cliente no provedor segue as regras do provedor e
   precisa ser tratado nele.
6. **Backups**: cópias anteriores continuam contendo os dados até expirarem pela política de
   retenção dos backups; registre esse prazo na resposta ao pedido.

Limitações deste procedimento: não remove dados de `provider_webhook_events`, `dead_letters`,
`audit_log` nem dos snapshots de faturas, e não há verificação automática de que todos os campos
foram tratados. Um caso de uso dedicado (com preenchimento de `customers.anonymized_at`, regras
para os snapshots e testes) é o caminho para automatizar.
