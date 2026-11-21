# Retenção e anonimização de dados

Este documento descreve **o que o Recorvia faz** com o tempo de vida dos dados: o que é removido
automaticamente e em que prazo, o que é mantido de propósito, como funciona a anonimização de um
cliente e o que continua fora da automação. Não é um parecer jurídico nem uma declaração de
conformidade com qualquer legislação: prazos legais de guarda (fiscais, contábeis, contratuais) e o
tratamento de pedidos de titulares são definidos pelos responsáveis de cada organização. As
decisões estão nas ADR-060 (expurgo por idade), ADR-061 (anonimização de clientes) e ADR-062
(métricas e alertas); a ADR-056 registra a situação anterior.

## Expurgo automático por idade

Executado pelo agendador do worker (`scheduled_jobs`, com lease; ADR-011). Três tarefas:

| Tarefa | Frequência | Efeito |
|---|---|---|
| `retention.purge` | horária | remove, tenant a tenant e em lotes, os dados técnicos da tabela abaixo |
| `outbox.purge` | diária | remove eventos da outbox **publicados** há mais de 7 dias; pendentes nunca são removidos (`app_outbox_purge(7)`) |
| `idempotency.purge` | horária | remove chaves de idempotência **concluídas** e expiradas (validade de 24 h); chaves em processamento permanecem (`app_idempotency_purge_expired()`) |

### O que `retention.purge` remove

O prazo conta do instante em que o registro deixou de ser necessário (coluna "a partir de"), é
configurado em dias por variável de ambiente e validado na partida: a API e o worker não sobem com
um valor abaixo do mínimo ou acima de 3650.

| Dado (rótulo `kind` das métricas) | O que sai | A partir de | Variável | Padrão | Mínimo |
|---|---|---|---|---:|---:|
| `inbox_messages` | registros de deduplicação dos consumidores | processamento | `RECORVIA_RETENTION_INBOX_DAYS` | 90 | 14 |
| `dead_letters` | dead letters **tratadas** (reprocessadas ou descartadas), com payload e headers | tratamento | `RECORVIA_RETENTION_DEAD_LETTERS_DAYS` | 180 | 30 |
| `webhook_deliveries` | entregas de webhooks enviados **concluídas** (`succeeded` ou `exhausted`) e as tentativas | conclusão | `RECORVIA_RETENTION_WEBHOOKS_DAYS` | 90 | 7 |
| `webhook_events` | eventos de webhooks enviados (payload) sem nenhuma entrega restante | criação | `RECORVIA_RETENTION_WEBHOOKS_DAYS` | 90 | 7 |
| `webhook_endpoint_secrets` | segredos de assinatura vencidos (cifrados) | vencimento | `RECORVIA_RETENTION_WEBHOOKS_DAYS` | 90 | 7 |
| `provider_webhook_payloads` | **corpo** dos webhooks recebidos do provedor já processados ou ignorados; a linha permanece, com `payload = {}` e `payload_purged_at` | processamento | `RECORVIA_RETENTION_PROVIDER_WEBHOOK_PAYLOADS_DAYS` | 90 | 7 |
| `notification_contents` | **conteúdo** cifrado das notificações encerradas; a linha permanece, com `content_purged_at` | encerramento (envio, falha, cancelamento ou supressão) | `RECORVIA_RETENTION_NOTIFICATION_CONTENT_DAYS` | 90 | 7 |
| `notifications` | notificações encerradas (destinatários, assunto) e o histórico de envio | encerramento | `RECORVIA_RETENTION_NOTIFICATIONS_DAYS` | 365 | 30 |
| `portal_links` | links do portal expirados, usados ou revogados | fim da validade do link | `RECORVIA_RETENTION_PORTAL_LINKS_DAYS` | 90 | 1 |
| `usage_events` | eventos de consumo de períodos **já faturados** (inclusive `properties`); o agregado do período permanece, com `events_purged_at` | fim da tolerância de atraso do período | `RECORVIA_RETENTION_USAGE_EVENTS_DAYS` | 400 | 30 |
| `invitations` | convites **não aceitos** (expirados ou revogados) e o e-mail de convite | fim da validade do convite | `RECORVIA_RETENTION_INVITATIONS_DAYS` | 90 | 7 |

Regras adicionais validadas na partida: o prazo das notificações não pode ser menor que o do
conteúdo, e o dos links do portal não pode ser menor que `RECORVIA_PORTAL_SESSION_TTL` (a sessão do
portal é conferida contra o link que a originou enquanto durar). `RECORVIA_RETENTION_BATCH_SIZE`
define quantos registros saem por transação (padrão 500; de 10 a 5000).

### O que nunca é removido, qualquer que seja o prazo

As regras ficam no banco (função `app_retention_purge`), e não no chamador:

- mensagem ainda não processada: o inbox só guarda mensagens já processadas, e a outbox pendente
  não é tocada;
- dead letter **aberta** (inclusive as registradas sem tenant, visíveis apenas à operação);
- entrega de webhook pendente, em reenvio ou em andamento, e o evento que ela referencia;
- segredo de webhook corrente ou ainda dentro da janela de rotação;
- webhook recebido do provedor ainda não processado (`received`);
- notificação agendada ou em envio, e o conteúdo dela;
- notificação de fatura **em aberto**: o registro do lembrete é o que impede reenviá-lo;
- link do portal dentro da validade;
- consumo **não faturado** (evento sem agregado fechado), consumo dentro da tolerância de atraso
  da métrica (`late_grace_hours`, quando o período ainda aceita eventos atrasados) e períodos com
  correção pendente (a correção recalcula o período a partir de todos os eventos);
- convite aceito, convite dentro da validade e convite cujo e-mail ainda não foi enviado.

### Como o expurgo é executado

- **Por tenant**: `app_retention_tenants` (função `SECURITY DEFINER`) devolve apenas os IDs dos
  tenants com dados vencidos; a remoção ocorre na transação de cada tenant (`InTenantTx`), e a
  função recusa remover dados de um tenant diferente do contexto da transação.
- **Em lotes**: cada transação remove no máximo `RECORVIA_RETENTION_BATCH_SIZE` registros, com
  `FOR UPDATE SKIP LOCKED` (linhas em uso por outra transação ficam para a execução seguinte). Uma
  execução dura até 2 minutos, divididos igualmente entre os tipos, e cada tenant recebe até 20
  lotes por tipo; o restante continua na hora seguinte, a partir do tenant em que parou (rodízio).
  Depois de ativar o expurgo num banco com muito histórico, o volume cai ao longo de várias horas.
- **Relógio da aplicação**: o corte de cada tipo é calculado pelo worker (`agora - prazo`).
- **Privilégios**: a aplicação continua sem `DELETE` nas tabelas de histórico; os gatilhos de
  imutabilidade de `notification_events`, `webhook_events`, `webhook_delivery_attempts` e de
  `customer_portal_links` só aceitam a exclusão dentro das funções de retenção (`UPDATE` nunca).

### Efeitos que a operação precisa conhecer

- **Inbox**: depois do prazo, uma mensagem reentregue com o mesmo identificador seria processada de
  novo; por isso o mínimo é o dobro da retenção da outbox, e as operações financeiras têm chaves
  de negócio permanentes (ADR-022).
- **Webhooks enviados**: entregas expurgadas não podem mais ser reenviadas manualmente
  (`POST /api/v1/webhook-deliveries/{id}/retry`).
- **Webhooks recebidos**: a linha sem corpo continua deduplicando reenvios do provedor
  (`provider_event_id`); o corpo não fica mais disponível para diagnóstico.
- **Notificações**: o histórico (`GET /api/v1/notifications`) deixa de listar as expurgadas; antes
  disso, a notificação permanece sem o conteúdo.
- **Consumo**: `GET /api/v1/usage-events` deixa de listar os eventos expurgados; a quantidade e o
  item faturado continuam no agregado e na fatura. O reenvio de um evento expurgado é rejeitado
  (período faturado e fora da tolerância) em vez de respondido como `duplicate`.
- **Backups**: cópias anteriores continuam contendo os dados até expirarem pela política de
  retenção dos backups (ver [backup](backup-e-restauracao.md)). Uma restauração traz de volta dados
  já expurgados; o expurgo os remove de novo nas execuções seguintes.

Acompanhamento: métricas `recorvia_retention_purged_total`,
`recorvia_retention_purge_failures_total` e `recorvia_retention_purge_incomplete_total` (por
`kind`, sem rótulo de tenant), alertas `RecorviaRetentionPurgeStale` e
`RecorviaRetentionPurgeBacklog` e o [runbook de retenção](runbooks/retencao.md).

## Dados mantidos de propósito

Não têm prazo de retenção e nenhuma tarefa os remove, inclusive depois de encerrar assinaturas,
arquivar ou anonimizar clientes:

| Categoria | Tabelas | Proteção |
|---|---|---|
| Registros financeiros imutáveis | `ledger_transactions`, `ledger_entries`, `payments`, `invoice_items`, `invoice_status_history`, `credit_notes`, `customer_credit_entries`, `subscription_prorations`, `dispute_events`, `dispute_recovery_settlements` | gatilho `app_reject_mutation`: `UPDATE` e `DELETE` são rejeitados para qualquer papel (ADR-014); correções por lançamento compensatório |
| Faturas | `invoices` (inclui `customer_snapshot`, a cópia do cadastro do cliente na emissão) | gatilho `app_invoice_guard`: a fatura emitida não é excluída nem reescrita |
| Trilha de auditoria | `audit_log` | gatilho `app_reject_mutation`; os detalhes guardam identificadores, nomes de campos e justificativas, não valores pessoais |
| Histórico de assinaturas e preços | `subscription_history`, `plan_price_versions` | gatilho `app_reject_mutation` |
| Cobranças, reembolsos, disputas e conciliação | `charges`, `payment_operations`, `refunds`, `disputes`, `reconciliation_*` | sem expurgo: sustentam o ledger e a conciliação |
| Agregados de consumo | `usage_aggregates` | quantidade e item faturado de cada período |
| Cadastros | `customers`, `subscriptions`, `users`, `memberships`, `tenants` e o catálogo | arquivar não apaga; clientes podem ser anonimizados (abaixo) |

O expurgo não tem tipo para nenhuma dessas tabelas, e os testes de integração conferem que os
gatilhos rejeitam a exclusão mesmo para o dono das tabelas e mesmo com a variável das funções de
retenção ligada (`TestFinancialAndAuditRecordsAreNeverPurged`). Essa guarda é uma decisão de
projeto (rastreabilidade financeira e de auditoria), não uma afirmação sobre prazos legais.

## Expiração fora do PostgreSQL

| Dado | Onde | Tempo de vida |
|---|---|---|
| Sessão do usuário (cifrada) | Redis | absoluto de 12 h e inatividade de 2 h (`RECORVIA_SESSION_TTL`, `RECORVIA_SESSION_IDLE_TTL`) |
| Sessão do portal do cliente | Redis | até 2 h, 30 min de inatividade (`RECORVIA_PORTAL_SESSION_TTL`, `RECORVIA_PORTAL_SESSION_IDLE_TTL`) |
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

## Anonimização de clientes

`POST /api/v1/customers/{id}/anonymize` (ADR-061), também disponível na ficha do cliente, no botão
**Anonimizar**, com confirmação.

| Requisito | Regra |
|---|---|
| Permissão | `customers.anonymize` (proprietário e administrador); não é delegável a credenciais de API: a rota aceita apenas a sessão de um usuário |
| Justificativa | obrigatória, de 10 a 500 caracteres; fica na auditoria e é recusada se contiver o nome, o documento, o e-mail ou o telefone do cliente (informe o protocolo do pedido e o motivo) |
| `Idempotency-Key` | obrigatória |
| Pré-condição | cliente sem assinatura ativa (em avaliação, ativa, inadimplente ou suspensa), sem fatura em aberto e sem faturamento pendente (ver abaixo); caso contrário, 409 `customer_has_obligations` |

**Faturamento pendente** é o que ainda vai gerar fatura, item ou crédito para o cliente, mesmo sem
assinatura ativa nem fatura em aberto. Enquanto houver, a anonimização é recusada:

| Pendência | Como resolver |
|---|---|
| Assinatura com fechamento agendado (por exemplo, cancelada de imediato com consumo final ou itens a faturar) | aguardar o motor de faturamento emitir a fatura e resolvê-la (pagamento, cancelamento ou baixa) |
| Item pendente ainda sem fatura | aguardar o fechamento que o inclui |
| Consumo atrasado, ou aceito sem agregado num período já fechado, ainda por corrigir | aguardar a correção de consumo e resolver a fatura ou o crédito gerado |
| Período de consumo fechado ainda dentro da tolerância de atraso da métrica (`late_grace_hours`, 72 h por padrão, até 720 h), contada do fim do período ou do encerramento da assinatura, o que vier antes | aguardar o fim da tolerância: até lá a ingestão ainda aceita consumo atrasado do período |

O que a operação faz, numa única transação:

| Dado | Tratamento |
|---|---|
| Nome | substituído por `Cliente anonimizado` |
| Documento (CPF ou CNPJ) | substituído por zeros (`00000000000` ou `00000000000000`), que não identificam ninguém |
| Razão social, e-mail, telefone e identificador externo | apagados |
| Contatos e preferências de e-mail de cobrança | excluídos |
| Endereço de cobrança | excluído |
| Identificador do cliente no provedor de pagamento | excluído do cadastro, devolvido na resposta (`provider_customers`) e registrado na auditoria da anonimização (`details.provider_customers` de `customer.anonymized`), para que o cadastro seja tratado no provedor; a repetição do pedido devolve os mesmos identificadores |
| Links e sessões do portal | links revogados; a sessão aberta é recusada na requisição seguinte e nenhum link novo é gerado |
| Notificações do cliente | excluídas, com destinatários, assunto, conteúdo cifrado e histórico de envio (que pode trazer endereços de e-mail em mensagens de erro) |
| Webhooks recebidos das cobranças do cliente | corpo esvaziado nos já processados (o corpo enviado pelo provedor pode trazer dados do pagador); um webhook ainda não processado mantém o corpo até ser processado e segue o expurgo por idade |
| Situação | cliente arquivado, com `anonymized_at` preenchido |

A operação é **irreversível**: o banco rejeita qualquer alteração posterior da linha do cliente
(gatilho `customers_anonymized_guard`) e novas assinaturas e faturas para ele (gatilhos
`subscriptions_customer_guard` e `invoices_customer_guard`); a ingestão rejeita consumo do cliente
(`Cliente anonimizado: consumo não é mais aceito.`; o reenvio de um evento já gravado continua
respondido como duplicata) e a API recusa novos contatos e endereço (409 `customer_anonymized`). É
**idempotente**: repetida, devolve o estado atual com `already_anonymized` e os identificadores do
provedor registrados na anonimização, sem alterar nada nem auditar de novo. Fica registrada na
auditoria (`customer.anonymized`) com a justificativa, os nomes dos campos, as contagens do que
foi removido e os identificadores do cliente nos provedores, sem valores pessoais, e gera o evento
`customer.updated` com `anonymized: true` para as integrações (sem os identificadores).

Se a resposta da anonimização se perder (queda da API ou da conexão depois da confirmação), os
identificadores do provedor não se perdem: repita o pedido, com a mesma `Idempotency-Key` ou com
outra, ou consulte o histórico do cliente ou `GET /api/v1/audit-events?action=customer.anonymized`.

Na interface, os identificadores aparecem num diálogo que só fecha quando a pessoa o fecha: ele
permanece aberto quando a ficha é recarregada e também quando a recarga falha e a ficha dá lugar à
tela de erro. Se o envio falhar sem resposta e a ficha recarregar com o cliente já anonimizado, o
formulário continua aberto, avisa que o pedido anterior foi concluído e oferece "Enviar novamente",
que reutiliza a chave e devolve os identificadores sem alterar mais nada. A ficha de um cliente
anonimizado traz um aviso permanente de que os identificadores estão no evento de anonimização, na
aba Histórico.

### O que a anonimização não altera

- **Faturas emitidas** continuam com o `customer_snapshot` (nome, razão social, documento, e-mail e
  endereço do momento da emissão). A fatura emitida é o documento comercial da cobrança e é
  protegida por gatilho contra reescrita; alterá-la quebraria a rastreabilidade com o ledger e com
  o que foi enviado ao cliente e ao provedor. Elas permanecem consultáveis por quem tem
  `invoices.read`. Se a organização concluir que não há obrigação de guarda, a remoção dessas
  cópias exige uma decisão registrada e uma migration própria; não há operação para isso.
- **Pagamentos, cobranças, créditos, lançamentos e histórico de assinaturas** continuam vinculados
  ao cliente pelo identificador interno, sem dados pessoais próprios. Um saldo de crédito do
  cliente permanece registrado.
- **Auditoria**: as entradas anteriores permanecem (não guardam valores pessoais).
- **Eventos de consumo** (`usage_events.properties`, com o que a integração da organização enviou)
  seguem o expurgo por idade; o consumo ainda não faturado não é removido.
- **Outbox, dead letters e idempotência**: seguem os prazos de expurgo (os eventos carregam
  identificadores e valores, não o cadastro; a resposta registrada na chave de idempotência da
  própria anonimização expira em 24 h). Os identificadores do cliente nos provedores permanecem na
  auditoria da anonimização, que não é expurgada: são identificadores opacos do provedor, sem
  dados cadastrais.
- **Provedor de pagamento**: o cadastro do cliente e as cobranças no provedor seguem as regras do
  provedor e precisam ser tratados nele.
- **Backups e logs**: cópias anteriores continuam contendo os dados até expirarem; registre esse
  prazo na resposta ao pedido.
- **Notificações já entregues** estão na caixa do destinatário. Uma notificação em envio no
  instante da anonimização pode ainda ser entregue; o registro dela é excluído.

### Procedimento recomendado para um pedido

1. Registrar o pedido no canal da organização (quem pediu, quando, qual cliente) e confirmar com
   os responsáveis se há obrigação de guarda que impeça a eliminação.
2. Encerrar as assinaturas do cliente e resolver as faturas em aberto (pagamento, cancelamento ou
   baixa). Com consumo medido, aguardar o fechamento do consumo final, resolver a fatura gerada e
   esperar o fim da tolerância de atraso da métrica; até lá a anonimização responde 409
   `customer_has_obligations`.
3. Anonimizar o cliente pela ficha ou pela API, informando o protocolo na justificativa.
4. Tratar o cadastro no provedor de pagamento com os identificadores devolvidos (também
   disponíveis no histórico do cliente e na auditoria).
5. Guardar o identificador interno do cliente e o protocolo; a comprovação fica na auditoria
   (`GET /api/v1/audit-events?action=customer.anonymized`).

## O que continua fora da automação

- Anonimização ou eliminação de **usuários, membros e organizações** (inclusive o encerramento de
  uma organização com exportação e eliminação dos dados). A remoção de um membro muda a situação,
  sem apagar; convites aceitos permanecem.
- Remoção dos dados pessoais copiados nas **faturas emitidas** (decisão acima).
- **Dead letters abertas sem tenant** e **chaves de idempotência em processamento** que nunca foram
  concluídas: permanecem até tratamento manual.
- Retenção dos **backups**, dos **logs** e dos dados do **Keycloak** (contas no provedor de
  identidade), a cargo de quem opera.
- Registro formal de **pedidos de titulares**: a anonimização registra quem executou, quando e a
  justificativa; o protocolo e o atendimento do pedido ficam fora do sistema.
