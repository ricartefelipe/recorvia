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
  awaiting_payment --> canceled: fatura cancelada
  paid --> partially_refunded
  paid --> refunded
  paid --> disputed: chargeback solicitado
  partially_refunded --> refunded
  partially_refunded --> disputed
  disputed --> paid: disputa ganha
  disputed --> charged_back: disputa perdida
```

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
