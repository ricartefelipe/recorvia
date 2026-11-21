# Máquinas de estado

Transições fora das listadas são rejeitadas com `409 invalid_transition` (assinatura),
`invalid_invoice_transition` (fatura) ou `invalid_charge_transition` (cobrança). Cada transição de
assinatura e de fatura gera uma entrada de histórico imutável.

## Assinatura (`internal/subscriptions`)

```mermaid
stateDiagram-v2
  [*] --> trialing: criação com trial
  [*] --> active: criação sem trial
  trialing --> active: fim do trial (primeiro faturamento)
  trialing --> suspended
  trialing --> canceled
  active --> past_due: fatura vencida
  past_due --> active: faturas regularizadas (pagas, creditadas, canceladas ou baixadas)
  active --> suspended: manual ou política de inadimplência
  past_due --> suspended
  suspended --> active: reativação
  suspended --> trialing: reativação antes do fim do trial
  active --> canceled: imediato ou fim do período
  past_due --> canceled
  suspended --> canceled
  canceled --> [*]
```

Cancelamento ao fim do período é uma marcação (`cancel_at_period_end`) desfeita com
`resume-renewal`; a transição para `canceled` ocorre no motor, no início do período seguinte.
Reativação antes do fim do trial volta a `trialing` e mantém a data original de fim do trial.
Reativação que pula períodos inteiramente suspensos registra em `pending_usage_starts` os períodos
com consumo anterior à suspensão ainda não fechado; o primeiro faturamento após a reativação os
fecha e limpa a lista (ver `dinheiro-e-datas.md`).

## Mudança agendada e reajuste programado (`internal/subscriptions`)

```mermaid
stateDiagram-v2
  [*] --> pending: agendamento (next_cycle)
  pending --> applied: início do período seguinte (motor)
  pending --> canceled: nova mudança, mudança imediata ou cancelamento da assinatura
```

```mermaid
stateDiagram-v2
  [*] --> scheduled: programação (data efetiva futura)
  scheduled --> applied: primeiro período iniciado na data efetiva ou depois (motor)
  scheduled --> canceled: cancelamento manual ou da assinatura, troca da versão de preço ou reajuste inaplicável no ciclo
```

O motivo do cancelamento do reajuste fica em `cancel_reason`: `manual`, `subscription_ended`,
`price_version_changed`, `price_without_fixed_component` ou `adjustment_amount_limit`.

Há no máximo uma mudança `pending` por assinatura e um reajuste `scheduled` por data (índices
únicos parciais).

## Fatura (`internal/billing`)

```mermaid
stateDiagram-v2
  [*] --> draft
  draft --> open: emissão
  draft --> paid: emissão com saldo zero (crédito)
  open --> partially_paid: pagamento parcial
  open --> paid
  partially_paid --> paid
  open --> void: cancelamento sem pagamentos
  open --> uncollectible: baixa
  partially_paid --> uncollectible
  uncollectible --> paid: recuperação integral do valor baixado
  paid --> [*]
  void --> [*]
```

Fatura emitida não muda número, itens, valores faturados nem snapshot do cliente (gatilho
`app_invoice_guard`). Correções usam nota de crédito, cancelamento com estorno no ledger ou baixa.
Pagamento de fatura incobrável reverte a baixa na mesma medida (lançamento
`invoice.bad_debt_recovered`); enquanto houver valor baixado, a fatura continua `uncollectible`, com
o recebimento registrado no histórico.

## Cobrança (`internal/payments`)

```mermaid
stateDiagram-v2
  [*] --> pending
  pending --> awaiting_payment: criada no provedor
  pending --> paid
  pending --> failed
  pending --> canceled
  awaiting_payment --> paid
  awaiting_payment --> failed
  awaiting_payment --> canceled: fatura cancelada ou nota de crédito
  failed --> paid: pagamento confirmado pelo provedor
  canceled --> paid: pagamento confirmado pelo provedor
  paid --> partially_refunded
  paid --> refunded
  paid --> disputed: chargeback solicitado
  partially_refunded --> refunded
  partially_refunded --> disputed
  disputed --> paid: disputa ganha
  disputed --> partially_refunded: disputa ganha com reembolso anterior
  disputed --> refunded: disputa ganha com o pago já reembolsado
  disputed --> charged_back: disputa perdida
```

Dinheiro recebido nunca é descartado. A cobrança cancelada no Recorvia é cancelada também no provedor
(operação `cancel_charge`, ADR-037), mas o pagamento pode chegar antes do cancelamento ou o cancelamento
pode falhar; a recusa (por exemplo, do cartão) não impede o pagamento posterior por outro meio no mesmo
checkout. Por isso a confirmação de pagamento pelo provedor é a única transição que sai de `failed` e de
`canceled`: a cobrança passa a `paid` e o valor segue a aplicação normal (quita
o saldo da fatura; o excedente, ou o valor integral quando a fatura já está paga ou cancelada, vira
crédito do cliente; a taxa do provedor é lançada no ledger). A transição incomum é auditada como
`charge.paid_after_failure` ou `charge.paid_after_local_cancellation`, com a situação anterior e,
na recusa, o motivo, que deixa de constar na cobrança paga.

Reembolsos confirmados durante uma disputa ou depois do chargeback somam ao valor reembolsado sem mudar
a situação da cobrança. O saldo reembolsável é o pago menos o reembolsado, o estornado e o reservado por
reembolsos em andamento (`amount_refund_pending`). A disputa perdida estorna no máximo esse saldo: o valor
já reembolsado ou reservado não é estornado de novo, e a confirmação posterior do reembolso reservado
continua cabendo no saldo.

## Reembolso (`refunds`)

```mermaid
stateDiagram-v2
  [*] --> requested: pedido (reserva do valor)
  [*] --> succeeded: feito diretamente no provedor
  requested --> succeeded: confirmado (resposta, webhook ou consulta)
  requested --> failed: recusado pelo provedor
  requested --> unknown: resposta ambígua
  unknown --> requested: consulta mostra o pedido em processamento
  unknown --> succeeded: consulta mostra o reembolso efetivado
  unknown --> failed: recusa ou ausência confirmada na conciliação
```

`failed` libera a reserva; `succeeded` a converte em valor reembolsado. Enquanto houver reembolso
`unknown`, a cobrança não aceita novos pedidos. O reembolso permanece `requested` quando o provedor aceita
o pedido sem efetivá-lo, quando a chamada falha de forma repetível (operação `pending` com backoff) e
quando outro reembolso da mesma cobrança ainda está em execução no provedor (operação adiada). Pedido que
ainda não chegou ao provedor quando a cobrança entra em disputa ou é estornada por chargeback passa a
`failed` sem ser enviado (o pagador receberia o valor duas vezes). A resposta de
`POST /api/v1/charges/{id}/refunds` traz a situação alcançada na própria requisição.

## Disputa (`disputes`)

```mermaid
stateDiagram-v2
  [*] --> opened: chargeback solicitado (valor retido)
  opened --> under_review: em análise
  opened --> won
  opened --> lost
  under_review --> won: estorna a retenção
  under_review --> lost: registra o chargeback
```

Na disputa perdida, a parte da retenção correspondente ao valor reembolsado (confirmado durante a disputa
ou ainda reservado) volta aos valores em liquidação (`dispute.hold_released`). Reembolso reservado recusado
depois da perda passa ao chargeback (`dispute.hold_restored`).

Quando a cobrança foi paga com excedente convertido em crédito do cliente, a parte do valor estornado que
corresponde ao excedente ainda não devolvido sai de `chargebacks` pelo lançamento `dispute.credit_reversed`
(um pela disputa perdida e um por reembolso recusado depois da perda): o crédito ainda disponível é
consumido (`disputes.credit_amount`) e o que o cliente já usou vira valor a recuperar dele
(`disputes.recovery_amount`, conta `customer_recoverables`), sem deixar o saldo de crédito negativo. Assim,
`chargebacks` registra o valor estornado menos o excedente, isto é, a receita efetivamente perdida
(ADR-070). A disputa ganha não mexe no crédito.

A disputa perdida com valor a recuperar não muda de situação com a baixa (ADR-073): continua `lost`, e o
saldo a recuperar diminui a cada baixa, recebida (`recovery_received_amount`) ou perdida
(`recovery_written_off_amount`), até zero. Baixas parciais são permitidas; a soma nunca passa de
`recovery_amount`. O saldo pode voltar a crescer se um reembolso reservado for recusado depois da perda
(`dispute.chargeback_increased`).

```mermaid
stateDiagram-v2
  [*] --> a_recuperar: disputa perdida com recovery_amount > 0
  a_recuperar --> a_recuperar: baixa parcial (recebida ou perdida)
  a_recuperar --> baixado: baixa do saldo restante
  baixado --> a_recuperar: reembolso reservado recusado depois da perda aumenta o valor a recuperar
```

Cada transição gera uma entrada imutável em `dispute_events`. Há no máximo uma disputa aberta por
cobrança.

## Conciliação (`reconciliation_runs`, `reconciliation_discrepancies`)

```mermaid
stateDiagram-v2
  [*] --> running: registrada (manual, aguardando um worker) ou assumida (agendada)
  running --> running: assumida pelo worker, devolvida no encerramento dele ou retomada com o lease expirado (attempts + 1)
  running --> completed: coleta concluída (parcial com aviso em error)
  running --> failed: erro do provedor ou da leitura, ou três tentativas interrompidas
```

A situação `running` cobre três estados, distinguidos pelo lease: aguardando um worker (`lease_until`
nulo), em execução (lease vigente) e interrompida (lease expirado). O lease expirado não encerra a
execução: ela continua em `running`, a conta do provedor continua ocupada (novos pedidos respondem `409
reconciliation_running`) e qualquer worker a retoma do início. Só ao ser assumida depois de três tentativas
interrompidas ela passa a `failed`, com a causa (ADR-071).

```mermaid
stateDiagram-v2
  [*] --> open: constatada
  open --> open: constatada de novo (ocorrências)
  open --> resolved: accept_provider, keep_local, adjust_ledger ou auto_resolved
  open --> dismissed: dismiss
  resolved --> open: auto_resolved constatada de novo
```

Na API, a divergência resolvida automaticamente aparece com `status = resolved`,
`resolution = auto_resolved` e `resolved_by` nulo. Divergência resolvida manualmente não volta a `open`
com os mesmos valores; valores novos geram outra divergência.

## Operação externa (`payment_operations`)

```mermaid
stateDiagram-v2
  [*] --> pending
  pending --> in_flight: reivindicada (lease)
  unknown --> in_flight: nova consulta
  in_flight --> succeeded
  in_flight --> failed: recusa definitiva ou cobrança não mais pendente sem cobrança no provedor
  in_flight --> pending: falha repetível
  in_flight --> unknown: resposta ambígua
  in_flight --> in_flight: lease expirado (outra instância assume)
```

Depois de `MaxOperationAttempts` tentativas, uma operação `unknown` deixa de ser retomada
automaticamente e exige resolução (reconsulta manual ou conciliação).

Tipos: `create_charge` (criação da cobrança, consulta pela referência externa antes de repetir), `refund`
(reembolso, consulta da cobrança e dos reembolsos antes de repetir; no máximo um reembolso em execução ou
desconhecido por cobrança) e `cancel_charge` (cancelamento no provedor, consulta da cobrança antes de
repetir; um por cobrança).

Uma criação cuja cobrança deixou de estar pendente (por exemplo, cancelada com a fatura anulada) é
obsoleta: a execução só consulta o provedor pela referência externa e nunca cria a cobrança. Encontrada
a cobrança (criada por uma tentativa anterior), ela é vinculada sem `charge.created` e cancelada também
no provedor por uma operação `cancel_charge`; um pagamento que chegue por ela antes do cancelamento vira
crédito do cliente. Sem cobrança no provedor, a operação termina em `failed`. Uma criação `pending`
obsoleta não conta como esgotada.

## Notificação (`internal/notifications`)

```mermaid
stateDiagram-v2
  [*] --> scheduled: agendamento (identidade de negócio única)
  [*] --> suppressed: sem destinatário ou convite indisponível
  scheduled --> sending: reivindicada (lease)
  scheduled --> scheduled: adiada (janela de envio ou limite diário)
  scheduled --> canceled: fatura resolvida, passo substituído, convite ou suspensão encerrados
  scheduled --> suppressed: contatos sem e-mail de cobrança no envio
  sending --> sent: aceita pelo SMTP
  sending --> scheduled: falha transitória (backoff)
  sending --> failed: erro permanente (5xx) ou tentativas esgotadas
  scheduled --> failed: conteúdo cifrado ilegível (content_unavailable)
  sending --> sending: lease expirado (outra instância assume)
  sent --> [*]
  failed --> [*]
  canceled --> [*]
  suppressed --> [*]
```

O motivo fica em `status_reason` e cada mudança gera uma entrada em `notification_events`.

## Suspensão pela política (`internal/collections`)

```mermaid
stateDiagram-v2
  [*] --> active: fatura vencida há N dias (assinatura suspensa)
  active --> reactivated: faturas vencidas regularizadas e reativação no pagamento
  active --> released: reativação manual (inclusive seguida de nova suspensão manual) ou cancelamento
  reactivated --> [*]
  released --> [*]
```

## Entrega de webhook (`internal/webhooks`)

```mermaid
stateDiagram-v2
  [*] --> pending: evento assinado pelo endpoint
  pending --> delivering: reivindicada (lease)
  retrying --> delivering: backoff vencido
  delivering --> succeeded: resposta 2xx
  delivering --> retrying: falha (rede, 3xx, 4xx, 5xx)
  delivering --> exhausted: tentativas esgotadas ou destino bloqueado
  delivering --> delivering: lease expirado (outra instância assume)
  delivering --> pending: devolvida sem tentativa (orçamento, cancelamento ou endpoint sem resposta)
  delivering --> retrying: devolvida sem tentativa, após tentativas anteriores
  pending --> exhausted: endpoint desativado ou recurso fora do plano
  retrying --> exhausted: endpoint desativado ou recurso fora do plano
  succeeded --> pending: reenvio manual
  exhausted --> pending: reenvio manual
  retrying --> pending: reenvio manual (antecipa a tentativa)
```

O reenvio manual (`POST /api/v1/webhook-deliveries/{id}/retry`) é recusado com `409` enquanto a
entrega está `pending` ou `delivering`, quando o endpoint está desativado ou removido e quando a
entrega já soma 100 tentativas (`delivery_attempts_limit`); para entregas concluídas, concede uma
tentativa além das já feitas.

## Endpoint de webhook (`internal/webhooks`)

```mermaid
stateDiagram-v2
  [*] --> active: criação (destino validado, primeiro segredo)
  active --> disabled: alteração (status disabled; permitida sem o recurso no plano)
  disabled --> active: alteração (destino revalidado; exige o recurso no plano)
  active --> removido: DELETE
  disabled --> removido: DELETE
  removido --> [*]
```

A remoção grava `deleted_at` e `status = disabled`: o endpoint sai da listagem, as operações sobre
ele respondem `404` e as entregas pendentes terminam como `exhausted` na próxima execução do job.
A rotação de segredo não muda a situação nem a versão do endpoint.

## Inadimplência da fatura (`internal/collections`)

```mermaid
stateDiagram-v2
  [*] --> aberta: fatura em aberto vencida, avulsa ou de qualquer assinatura (invoice.overdue, uma vez)
  aberta --> resolvida: paga, creditada, cancelada ou baixada
  resolvida --> [*]
```

O registro em `invoice_delinquencies` é único por fatura; a resolução (`paid`, `credited`, `void`,
`uncollectible`) é gravada pelo consumidor da fatura resolvida e conferida a cada execução da
régua.
