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
  past_due --> active: faturas regularizadas
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
reembolsos em andamento (`amount_refund_pending`).

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
quando outro reembolso da mesma cobrança ainda está em execução no provedor (operação adiada). A
resposta de `POST /api/v1/charges/{id}/refunds` traz a situação alcançada na própria requisição.

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

Cada transição gera uma entrada imutável em `dispute_events`. Há no máximo uma disputa aberta por
cobrança.

## Conciliação (`reconciliation_runs`, `reconciliation_discrepancies`)

```mermaid
stateDiagram-v2
  [*] --> running
  running --> completed
  running --> failed: erro do provedor ou lease expirado
```

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
  in_flight --> failed: recusa definitiva
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
