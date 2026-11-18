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
  disputed --> charged_back: disputa perdida
```

Dinheiro recebido nunca é descartado. O cancelamento de cobrança é apenas local (a cobrança continua
válida no provedor), e a recusa (por exemplo, do cartão) não impede o pagamento posterior por outro
meio no mesmo checkout. Por isso a confirmação de pagamento pelo provedor é a única transição que
sai de `failed` e de `canceled`: a cobrança passa a `paid` e o valor segue a aplicação normal (quita
o saldo da fatura; o excedente, ou o valor integral quando a fatura já está paga ou cancelada, vira
crédito do cliente; a taxa do provedor é lançada no ledger). A transição incomum é auditada como
`charge.paid_after_failure` ou `charge.paid_after_local_cancellation`, com a situação anterior e,
na recusa, o motivo, que deixa de constar na cobrança paga.

## Operação externa (`payment_operations`)

```mermaid
stateDiagram-v2
  [*] --> pending
  pending --> in_flight: reivindicada (lease)
  unknown --> in_flight: nova consulta
  in_flight --> succeeded
  in_flight --> failed: recusa definitiva
  in_flight --> pending: falha repetível
  in_flight --> unknown: resposta ambígua
  in_flight --> in_flight: lease expirado (outra instância assume)
```

Depois de `MaxOperationAttempts` tentativas, uma operação `unknown` deixa de ser retomada
automaticamente e exige resolução (reconsulta manual ou conciliação).

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
