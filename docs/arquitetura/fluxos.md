# Fluxos principais

## Faturamento recorrente

```mermaid
sequenceDiagram
  autonumber
  participant S as Agendador (worker)
  participant E as Motor de recorrência
  participant DB as PostgreSQL
  participant R as Relay da outbox
  participant MQ as RabbitMQ
  S->>DB: reivindica tarefa billing.run_cycle (lease, SKIP LOCKED)
  S->>E: RunAll
  E->>DB: app_tenants_with_due_billing(now) (somente IDs)
  loop por tenant e por assinatura vencida
    E->>DB: BEGIN; set_config(app.tenant_id); SELECT … FOR UPDATE SKIP LOCKED
    E->>E: NextStep → BuildCycleInvoice (fixo antecipado, consumo anterior, ajustes, desconto, crédito)
    E->>DB: número sequencial; INSERT fatura e itens; histórico; ledger (D contas a receber / C receita)
    E->>DB: outbox invoice.issued; auditoria; avança a assinatura; COMMIT
  end
  R->>DB: app_outbox_claim (lease)
  R->>MQ: publica (publisher confirm)
  R->>DB: app_outbox_mark_published
```

Garantias: assinatura reivindicada por uma instância de cada vez; índice único por assinatura e
período; numeração por linha bloqueada da sequência (sem lacunas); catch-up de até 24 períodos
por transação. Depois de uma reativação que pulou períodos inteiramente suspensos, o primeiro
passo fecha o consumo pendente anterior à suspensão (`pending_usage_starts`) no lugar do período
anterior regular e limpa a lista na mesma transação.

## Cobrança no provedor

```mermaid
sequenceDiagram
  autonumber
  participant C as Consumidor invoice.issued
  participant P as Pagamentos
  participant DB as PostgreSQL
  participant G as Provedor
  C->>P: HandleInvoiceIssued(tenant, fatura, mensagem)
  P->>DB: tx: cobrança pending + operação create_charge (referência = id da cobrança)
  P->>DB: tx curta: operação in_flight (lease > timeout da chamada)
  P->>G: (sem transação aberta) consulta por referência, se não for a 1ª tentativa
  P->>G: cria cliente/cobrança (checkout hospedado)
  alt sucesso
    P->>DB: cobrança awaiting_payment + checkout; operação succeeded; charge.created
  else recusa (4xx)
    P->>DB: cobrança failed; operação failed; charge.failed
  else falha repetível (conexão recusada, 503, 429)
    P->>DB: operação pending com backoff
  else ambígua (timeout, 5xx, resposta perdida)
    P->>DB: operação unknown com backoff
  end
  Note over P,G: A recuperação agendada consulta por referência antes de qualquer nova criação.<br/>Sem confirmação segura, a operação permanece unknown e aparece na conciliação.
  C->>DB: inbox (mensagem processada)
```

A cobrança acompanha o saldo vigente da fatura. A criação no provedor ocorre fora de transação e
pode levar tempo (timeout, retentativas, recuperação agendada); qualquer mudança de saldo nesse
intervalo, como nota de crédito ou pagamento manual parcial, leva à substituição na conclusão. Ao
registrar o resultado, o valor criado é comparado com o saldo da fatura: havendo divergência, a
cobrança é cancelada localmente e, na mesma transação, é gravada a nova cobrança pelo saldo (com
auditoria `charge.canceled_after_balance_change`), criada no provedor depois do commit; sem saldo, não
há substituta. A substituta segue o mesmo fluxo e pode ser substituída de novo. Quando a criação
termina dentro da própria solicitação (`RequestCharge`, inclusive pela nova tentativa
`POST /api/v1/invoices/{id}/charges`), a resposta é a cobrança vigente, isto é, a última
substituta. Depois de criada, a cobrança aguardando pagamento só é ajustada pela nota de crédito
(consumidor `payments.invoice_credited`). Toda cobrança cancelada no Recorvia com vínculo no provedor é
cancelada também no provedor (operação `cancel_charge`, ADR-037); um pagamento que ainda chegue pela
cobrança antiga (antes do cancelamento no provedor ou com ele falhando) continua sendo aplicado e o
excedente vira crédito do cliente.

## Webhook do provedor

```mermaid
sequenceDiagram
  autonumber
  participant G as Provedor
  participant A as API
  participant DB as PostgreSQL
  participant W as Consumidor de webhooks
  G->>A: POST /webhooks/providers/{provedor}/{token}
  A->>A: limite de 256 KiB, JSON válido
  A->>DB: app_resolve_webhook_endpoint(provedor, sha256(token)) → tenant, conta
  A->>A: autenticação do provedor (HMAC com timestamp no simulador; asaas-access-token no Asaas)
  A->>DB: tx: INSERT evento (único por provedor+id do evento) + outbox provider_webhook.received
  A-->>G: 200 (somente após persistir)
  W->>DB: lê evento e cobrança (sem lock)
  alt evento fora de ordem ou sem dados suficientes
    W->>G: GetCharge (situação autoritativa, sem transação aberta)
  end
  W->>DB: tx: inbox + cobrança FOR UPDATE + aplica situação sem regredir
  W->>DB: pagamento (único por cobrança), fatura, ledger (D liquidação, D taxas / C contas a receber, C liquidação), eventos, auditoria
```

A ordem de chegada não decide o estado: eventos com situação anterior à local são registrados sem
efeito; saltos (por exemplo, reembolso antes da confirmação) levam à consulta ao provedor.

Dinheiro recebido nunca é descartado: a confirmação de pagamento de cobrança `failed` (recusada) ou
`canceled` (cancelada só no Recorvia) leva a `paid` e segue a aplicação normal. O valor quita o
saldo da fatura e o excedente vira crédito do cliente; se a fatura já estiver paga ou cancelada, o
valor integral vira crédito. A taxa do provedor é sempre lançada no ledger, e a transição incomum é
auditada (`charge.paid_after_failure` ou `charge.paid_after_local_cancellation`).

## Ordem de bloqueio

Transações que alteram as mesmas linhas bloqueiam na mesma ordem, para se serializarem sem impasse:
cobrança, fatura, assinatura e cliente. O motor, as mudanças de assinatura, a correção de consumo e a
ingestão bloqueiam a assinatura antes do cliente (crédito consumido ou concedido). A aplicação de
pagamento segue a mesma ordem: cobrança e fatura (`FOR UPDATE`), depois a situação da assinatura
(`RefreshSubscriptionStanding`) e só então o crédito do excedente, que bloqueia o cliente. No
registro do resultado da criação, o vínculo do cliente no provedor (`customer_provider_refs`, cuja
chave estrangeira bloqueia a linha do cliente em modo compartilhado) é gravado depois da aplicação de
um pagamento já confirmado. Inserções com chave estrangeira para `customers` contam como bloqueio do
cliente nessa ordem.

Reembolsos, disputas e conciliação seguem a mesma ordem: o pedido e a confirmação de reembolso bloqueiam a
cobrança, depois o reembolso, a fatura (histórico) e por último o cliente (consumo de crédito). O registro
do resultado de uma operação externa bloqueia a operação e depois a cobrança; a sincronização feita sob o
bloqueio da cobrança (webhook) só altera operações que não estão em execução. A resolução de divergência
bloqueia a divergência antes da cobrança, e a execução da conciliação bloqueia as divergências antes de gravar
referências às cobranças.

## Nota de crédito

1. `POST /api/v1/invoices/{id}/credit-notes` com `Idempotency-Key`; a mesma chave devolve a nota já
   emitida.
2. Na transação: valida o limite (saldo em aberto), grava a nota (`credit_notes`, imutável,
   número `<fatura>-NCnn`), atualiza `amount_credited` da fatura, registra histórico, lança
   `revenue_adjustments` × `accounts_receivable` no ledger e grava `invoice.credited` na outbox
   (e `invoice.paid` quando o saldo zera).
3. O consumidor `payments.invoice_credited` cancela a cobrança aguardando pagamento com valor
   divergente (no Recorvia e, depois do commit, no provedor) e solicita nova cobrança pelo saldo
   atualizado. Pagamento que ainda chegue pela cobrança antiga é aplicado e o excedente vira crédito do
   cliente.
4. Se a cobrança ainda está em criação no provedor (`pending`), o consumidor apenas registra a
   mensagem. Ao concluir a operação de criação, o registro do resultado compara o valor criado com
   o saldo da fatura: havendo divergência, cancela a cobrança localmente, grava na mesma transação
   a nova cobrança pelo saldo (com auditoria `charge.canceled_after_balance_change`) e a cria no
   provedor depois do commit; uma falha nessa criação fica com a recuperação de operações. A mesma
   comparação vale para qualquer mudança de saldo durante a criação (ver "Cobrança no provedor").

## Ingestão de consumo

1. `POST /api/v1/usage-events` com até 500 eventos; resultado por evento (`accepted`, `duplicate`,
   `late` ou `rejected` com motivo).
2. `external_id` único por tenant: reenvio idêntico é `duplicate`; conteúdo diferente é rejeitado,
   inclusive quando o `external_id` se repete no mesmo lote. A deduplicação vem antes das validações
   que dependem do estado atual: o reenvio de um evento gravado é respondido pelo conteúdo, mesmo
   que a assinatura, o preço ou a métrica tenham mudado depois. `occurred_at` é truncado a
   microssegundos (precisão do `timestamptz`) antes de validar, gravar e comparar.
3. Validações: textos sem caractere NUL (nem UTF-8 inválido) e `properties` sem `\u0000`, unidade,
   quantidade decimal não negativa com até 6 casas e máximo por evento, `unique_key` para contagem
   distinta, instante dentro da vigência faturável e fora de suspensões (pelo histórico da
   assinatura, mesmo depois da reativação). Com o período aberto, a métrica precisa fazer parte da
   versão de preço que vai precificá-lo: a da mudança agendada pendente com data efetiva até o
   início do período (em vigor e ainda não aplicada pelo motor) ou, sem ela, a vigente. Período já
   fechado com agregado da métrica aceita o evento como `late` (dentro da tolerância), ainda que o
   plano tenha mudado. Período cujo fechamento já passou sem agregado da métrica, ou que nunca
   será fechado (inteiramente suspenso), rejeita o evento; o período em que a suspensão começou
   continua aberto depois da reativação até o primeiro faturamento após ela. Rejeições valem por
   evento e não desfazem o lote.
4. O fechamento agrega por período conforme a métrica (`sum`, `max`, `unique_count`, `last`) e
   vincula o agregado ao item da fatura.
5. Ordem de bloqueio: no início da transação do lote, antes de qualquer outra leitura ou
   gravação, a ingestão bloqueia com `FOR SHARE` as assinaturas distintas do lote, uma vez cada e
   em ordem crescente de ID; só depois consulta métrica, mudança agendada, deduplicação e
   fechamento e grava os eventos. A mudança agendada é lida sem bloqueio: o agendamento e a
   aplicação pelo motor bloqueiam a assinatura com `FOR UPDATE` antes de alterá-la. O motor
   reivindica a assinatura com `FOR UPDATE SKIP LOCKED` e fecha o período na mesma transação:
   enquanto um lote mantém a assinatura, a reivindicação a pula (ela é faturada numa reivindicação
   seguinte); um lote que chega durante o fechamento espera o commit e classifica o evento como
   `late`, nunca como `accepted` num período já fechado. `FOR SHARE` é compatível entre lotes concorrentes; a ordem crescente (a
   mesma em que a correção de consumo percorre os grupos, bloqueando cada assinatura com
   `FOR UPDATE`) impede que transações que precisam de várias assinaturas as bloqueiem em ordens
   opostas.

## Reembolso

```mermaid
sequenceDiagram
  autonumber
  participant U as Usuário (payments.refund)
  participant A as API
  participant P as Pagamentos
  participant DB as PostgreSQL
  participant G as Provedor
  U->>A: POST /api/v1/charges/{id}/refunds (Idempotency-Key, valor, motivo)
  A->>P: RequestRefund(cobrança, valor, motivo, chave de negócio)
  P->>DB: tx: cobrança FOR UPDATE; chave do pedido; reserva amount_refund_pending; reembolso requested; operação refund; auditoria
  P->>DB: tx curta: bloqueio consultivo dos reembolsos da cobrança; operação in_flight (outro reembolso em execução adia esta)
  P->>G: (sem transação) consulta a cobrança se não for a 1ª tentativa; POST de reembolso com referência = ID do reembolso
  alt efetivado
    P->>DB: reembolso succeeded; reserva vira amount_refunded; crédito do cliente do excedente consumido; ledger refund.succeeded; histórico da fatura; refund.succeeded
  else aceito em processamento
    P->>DB: reembolso requested; operação succeeded (confirmação por webhook ou consulta)
  else recusa
    P->>DB: reembolso failed; reserva liberada; refund.failed
  else falha repetível (503, conexão recusada)
    P->>DB: reembolso requested; operação pending com backoff
  else ambígua (timeout, 5xx, resposta perdida)
    P->>DB: reembolso unknown; reserva mantida; operação unknown com backoff
  end
  A-->>U: 201 com o reembolso (requested, succeeded, failed ou unknown)
```

1. **Pedido.** A rota exige `payments.refund` e `Idempotency-Key`. A repetição com a mesma chave devolve a
   resposta registrada; depois da expurgação do registro HTTP (24 h), a chave de negócio
   (`refunds.request_key`) ainda devolve o mesmo reembolso. Mesma chave com outro valor ou cobrança gera
   409 `request_key_reused`.
2. **Saldo elegível.** O pedido bloqueia a cobrança e reserva o valor; pedidos concorrentes se serializam
   nesse bloqueio e o que não cabe no saldo recebe 409 `refund_exceeds_balance`. Somente cobranças `paid`
   ou `partially_refunded` aceitam reembolso (`charge_not_refundable` nas demais, inclusive em disputa), e
   um reembolso com resultado desconhecido bloqueia novos pedidos da mesma cobrança
   (`refund_outcome_unknown`). O banco garante os limites (`charges_refund_reservation_limit`,
   `charges_refund_limit`).
3. **Execução.** A chamada ao provedor ocorre logo após o commit, ainda na requisição; se não for possível
   (outro reembolso da cobrança em execução, provedor indisponível, processo interrompido), a operação fica
   com a recuperação (`payments.recover_operations`, a cada minuto no worker). A reivindicação de uma
   operação de reembolso toma um bloqueio consultivo por cobrança, de modo que duas instâncias nunca enviam
   ao mesmo tempo reembolsos da mesma cobrança (ADR-035).
4. **Confirmação.** Webhooks de reembolso (deduplicados, em qualquer ordem) e consultas ao provedor aplicam
   os reembolsos informados por associação exata (referência ou ID) ou, sem identificação, por valor
   (ADR-035); reembolsos feitos diretamente no provedor são registrados com `origin = 'provider'`. Os dados
   são cumulativos: um retrato mais antigo que chega depois não desfaz nem duplica efeitos.
5. **Efeitos.** A fatura não é reaberta: a devolução fica no histórico (`reason = 'refund'`), no ledger
   (`refund.succeeded`: débito em `refunds` ou, para o excedente do próprio pagamento, em
   `customer_credits`; crédito em `provider_clearing`) e no evento `refund.succeeded` (ADR-034).

## Disputas e chargebacks

```mermaid
sequenceDiagram
  autonumber
  participant G as Provedor
  participant C as Consumidor payments.provider_webhooks
  participant P as Pagamentos
  participant DB as PostgreSQL
  G->>C: webhook de chargeback (via recepção, outbox e RabbitMQ)
  C->>P: ProcessWebhook (consulta o provedor quando o evento salta etapas)
  P->>DB: tx: inbox; cobrança FOR UPDATE; disputa aberta? (disputes_open_key)
  alt chargeback solicitado
    P->>DB: disputa opened pelo saldo não reembolsado nem estornado; cobrança disputed; ledger dispute.opened (retenção); dispute.opened
  else em análise
    P->>DB: disputa under_review; dispute_events
  else ganha
    P->>DB: disputa won; estorno da retenção; cobrança paid, partially_refunded ou refunded (conforme os reembolsos); dispute.closed
  else perdida
    P->>DB: disputa lost; amount_charged_back; cobrança charged_back; histórico da fatura (chargeback); dispute.closed
  end
```

A situação de chargeback do provedor (webhook ou consulta) abre a disputa e retém o valor no ledger; a
decisão ganha estorna a retenção; a perdida mantém o valor em `chargebacks` sem reabrir a fatura. Webhook
da decisão que chega antes do da abertura abre e decide a disputa na mesma transação; o da abertura, que
chega depois com retrato anterior, é ignorado. Eventos repetidos, fora de ordem ou anteriores ao
encerramento não reabrem disputas (ADR-036). Durante a disputa e depois do chargeback a cobrança não aceita
reembolso. `GET /api/v1/disputes` (`payments.read`) lista as disputas mais recentes.

## Cancelamento no provedor

1. A cobrança é cancelada no Recorvia (fatura cancelada, nota de crédito ou substituição na conclusão da
   criação) e, na mesma transação, é gravada a operação `cancel_charge` (única por cobrança).
2. Depois do commit, `CancelCharge` é chamado sem transação aberta, pelo consumidor que cancelou
   (`payments.invoice_voided`, `payments.invoice_credited`) ou pelo registro do resultado da criação;
   falhas ficam com a recuperação de operações, que consulta o provedor antes de repetir.
3. Cobrança inexistente no provedor equivale a cancelada. Cobrança já paga no provedor encerra a operação
   como falha e o pagamento é aplicado (ADR-030).

## Conciliação

```mermaid
sequenceDiagram
  autonumber
  participant J as Job payments.reconcile ou usuário (reconciliation.run)
  participant P as Pagamentos
  participant DB as PostgreSQL
  participant G as Provedor
  J->>P: RunReconciliation(janela)
  P->>DB: tx: execução running (uma por conta, lease); encerra execuções interrompidas
  P->>G: ListCharges paginado (sem transação)
  P->>DB: cobranças locais da janela ou listadas, reembolsos, operações unknown
  P->>G: GetCharge das locais ausentes da listagem
  P->>P: Compare (pura): divergências por tipo e chave de deduplicação
  P->>DB: tx: grava/atualiza/reabre divergências; resolve automaticamente as não constatadas; conclui a execução
```

A conciliação compara cobranças, pagamentos, reembolsos e chargebacks locais com o provedor na janela e
registra divergências por tipo: cobrança sem correspondência (nos dois sentidos), valor ou taxa divergente,
pagamento não refletido, reembolso/chargeback pendente e operação com resultado desconhecido (ADR-038).

- **Execução manual.** `POST /api/v1/reconciliation/runs` (`reconciliation.run`, `Idempotency-Key`) executa
  na requisição a janela padrão de 30 dias do provedor padrão e devolve a execução encerrada. Falha do
  provedor encerra a execução como `failed`, com a causa em `error` (resposta 200); a repetição com a mesma
  chave devolve a mesma execução. Sem provedor padrão, 409 `provider_not_configured`; com outra execução em
  andamento na conta, 409 `reconciliation_running`.
- **Execução agendada.** O job `payments.reconcile` (a cada hora, com lease do agendador) concilia, com
  janela de 7 dias, os tenants cujo plano da plataforma tem `reconciliation_automation`, com provedor padrão
  ativo e sem execução agendada nas últimas 24 horas.
- **Consulta.** `GET /api/v1/reconciliation/runs` e `GET /api/v1/reconciliation/discrepancies` (filtros
  `status` e `kind`) exigem `reconciliation.read`; o detalhe traz o histórico de auditoria da divergência
  (detecção, reabertura, resolução automática e resolução).
- **Resolução.** `POST /api/v1/reconciliation/discrepancies/{id}/resolve` exige `reconciliation.resolve`,
  justificativa de 10 a 2000 caracteres e uma ação aplicável ao tipo (`resolution_not_applicable` nas
  demais): `dismiss`, `keep_local`, `adjust_ledger` (lançamento `reconciliation.adjustment` pela diferença de
  taxa ou de valor pago) ou `accept_provider` (adota o provedor). O responsável é o usuário autenticado;
  responsável, data, justificativa e lançamento ficam na divergência e na auditoria. Divergência já
  resolvida devolve 409 `discrepancy_not_open`; o provedor indisponível durante a resolução devolve 503.

## Worker: tarefas e métricas de recebíveis

| Tarefa ou consumidor | Efeito |
|---|---|
| `payments.provider_webhooks` | aplica pagamentos, reembolsos e disputas informados por webhook (inbox na mesma transação dos efeitos) |
| `payments.invoice_voided`, `payments.invoice_credited` | cancelam a cobrança em aberto no Recorvia e, depois do commit, no provedor |
| `payments.recover_operations` (1 min) | retoma operações `create_charge`, `refund` e `cancel_charge` pendentes, desconhecidas ou com lease expirado, sempre consultando o provedor antes de repetir |
| `payments.reconcile` (1 h) | conciliação agendada dos tenants elegíveis |
| `metrics.collect` (15 s) | `recorvia_reconciliation_open_discrepancies{kind}`, `recorvia_refunds_total{outcome}` e `recorvia_refunds_in_progress{status}` a partir de contagens agregadas do banco (ADR-039) |

Os eventos `refund.succeeded`, `refund.failed`, `dispute.opened`, `dispute.closed` e
`reconciliation.discrepancy_detected` são gravados na outbox na mesma transação dos efeitos e publicados no
RabbitMQ; nesta versão nenhum consumidor interno depende deles (ficam disponíveis para notificações e
webhooks enviados).
