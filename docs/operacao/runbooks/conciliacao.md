# Runbook: conciliação e operações com resultado desconhecido

Cobre as operações externas sem confirmação segura (`payment_operations` em `unknown`, ADR-008) e
as divergências entre o Recorvia e o provedor de pagamento (RF-12). As execuções e divergências
de conciliação seguem o contrato `api/openapi.yaml` (tag `reconciliation`); a situação de cada
funcionalidade está em `docs/STATUS.md`. A métrica `recorvia_reconciliation_open_discrepancies`
(por `kind`) é exposta pelo worker junto com a funcionalidade de conciliação; enquanto ela não
tiver séries, os alertas de divergência não disparam.

Princípios (válidos em qualquer ação abaixo):
- dinheiro recebido nunca é descartado (ADR-030);
- nenhuma cobrança é recriada às cegas: antes de repetir, consulta-se o provedor pela referência
  externa (o id local da cobrança);
- o ledger é imutável: correções são lançamentos compensatórios, nunca `UPDATE` ou `DELETE`;
- toda resolução tem responsável, justificativa e histórico (permissão `reconciliation.resolve`).

<a id="operacoes-com-resultado-desconhecido"></a>

## Operações com resultado desconhecido

### Sintomas
- Cobrança `pending` que não avança para `awaiting_payment` nem `failed`; fatura sem link de
  pagamento.

### Métricas e alertas
- `RecorviaPaymentOperationsUnknown`: `max(recorvia_payment_operations{status="unknown"}) > 0` por
  30 minutos.
- `recorvia_charge_failures_total{kind="ambiguous"}` (timeouts, 5xx, resposta perdida) e
  `RecorviaPaymentProviderUnavailable` costumam anteceder este alerta.

### Diagnóstico
```sql
-- Operações em unknown por tenant, tentativas e próxima tentativa (superusuário).
SELECT tenant_id, count(*), max(attempts) AS tentativas, min(next_attempt_at) AS proxima, left(max(last_error), 200) AS erro
  FROM payment_operations WHERE status = 'unknown' GROUP BY 1 ORDER BY 2 DESC;
-- Operações que esgotaram as tentativas automáticas (6) e dependem da conciliação.
SELECT tenant_id, id, charge_id, external_reference, attempts, updated_at
  FROM payment_operations WHERE status = 'unknown' AND attempts >= 6 ORDER BY updated_at;
```
- API (`payments.read`): `GET /api/v1/payment-operations` e `GET /api/v1/charges/{id}`.
- Abaixo de 6 tentativas, a tarefa `payments.recover_operations` (a cada minuto) consulta o
  provedor pela `external_reference` no horário de `next_attempt_at`: se a cobrança existir, é
  registrada; se não existir, a criação é repetida com segurança.
- No provedor: procure a cobrança pela referência externa (no Asaas, `externalReference`; no
  simulador, `GET /v1/charges?external_reference=<referência>`).

### Ações
1. Provedor indisponível: aguardar; a recuperação resolve sozinha dentro das tentativas.
2. Dentro do limite: `POST /api/v1/payment-operations/{id}/retry` (`payments.charge`) antecipa a
   tentativa (sempre consultando antes de criar).
3. Esgotadas as tentativas: executar a conciliação do período
   (`POST /api/v1/reconciliation/runs`, `reconciliation.run`, com `Idempotency-Key`); a operação
   aparece como divergência `unknown_operation` e é resolvida conforme
   [Divergências abertas](#divergencias-abertas).
4. Não altere `payment_operations` nem `charges` à mão.

### Verificação
`recorvia_payment_operations{status="unknown"}` volta a zero e a cobrança tem situação definitiva
(`awaiting_payment`, `paid` ou `failed`).

<a id="divergencias-abertas"></a>

## Divergências abertas

### Sintomas
- Diferença entre o recebido no provedor e o refletido nas faturas, no ledger ou no relatório de
  liquidação.

### Métricas e alertas
- `RecorviaReconciliationDiscrepanciesOpen`: divergências abertas de um tipo por mais de 4 horas.
- `RecorviaReconciliationDiscrepanciesHigh` (crítico): mais de 25 divergências abertas por
  30 minutos, sinal de falha sistêmica.
- Painel "Divergências abertas por tipo" (dashboard *visão geral*) e indicador
  `open_discrepancies` do dashboard da aplicação (`GET /api/v1/dashboard`).

### Diagnóstico
- API (`reconciliation.read`): `GET /api/v1/reconciliation/discrepancies?status=open&kind=<tipo>`,
  `GET /api/v1/reconciliation/discrepancies/{id}` (valores local e do provedor, histórico) e
  `GET /api/v1/reconciliation/runs` (execuções, janela e erro).
- Causa provável por tipo:

| Tipo | Causa provável | Primeira verificação |
|---|---|---|
| `missing_local` | cobrança no provedor sem registro local (criação fora do Recorvia ou resposta perdida) | referência externa e operação `unknown` correspondente |
| `missing_provider` | cobrança local sem correspondência no provedor | operação `failed`/`unknown` e conta do provedor usada |
| `amount_mismatch` | valor criado difere do registrado (nota de crédito ou mudança de saldo durante a criação) | histórico da fatura e auditoria `charge.canceled_after_balance_change` |
| `fee_mismatch` | taxa do provedor diferente da lançada | relatório de liquidação e lançamento de taxas no ledger |
| `payment_not_reflected` | pagamento confirmado sem webhook processado | `provider_webhook_events`, dead letters de `payments.webhooks` e `RecorviaWebhooksRejected` |
| `refund_pending`, `chargeback_pending` | reembolso ou disputa no provedor ainda não refletidos | eventos tardios do provedor |
| `unknown_operation` | operação sem confirmação após as tentativas | seção anterior |

- Volume alto de um único tipo: procure a causa comum antes de resolver item a item (por exemplo,
  webhooks recusados por segredo trocado geram `payment_not_reflected` em série).
```sql
-- Eventos de webhook recebidos e ainda não processados por tenant (superusuário).
SELECT tenant_id, provider, count(*), min(received_at)
  FROM provider_webhook_events WHERE processed_at IS NULL GROUP BY 1, 2;
```

### Ações
1. Corrigir a causa sistêmica primeiro (webhooks, credenciais, consumidor parado); depois executar
   nova conciliação da janela, que fecha as divergências resolvidas pelo próprio fluxo.
2. Resolver as restantes uma a uma: `POST /api/v1/reconciliation/discrepancies/{id}/resolve` com
   `resolution` (`accept_provider`, `keep_local`, `adjust_ledger` ou `dismiss`) e `justification`
   (mínimo de 10 caracteres). `adjust_ledger` gera lançamento compensatório; a resolução é
   auditada.
3. Em caso de dúvida sobre valores, prevalece a confirmação do provedor para dinheiro recebido
   (ADR-030); valores a devolver seguem o fluxo de reembolso.

### Verificação
- A divergência passa a `resolved` ou `dismissed` e `recorvia_reconciliation_open_discrepancies`
  do tipo diminui.
- Para `adjust_ledger`: `GET /api/v1/ledger/transactions` mostra o lançamento compensatório
  balanceado e a trilha (`GET /api/v1/audit-events`) registra quem resolveu e por quê.
