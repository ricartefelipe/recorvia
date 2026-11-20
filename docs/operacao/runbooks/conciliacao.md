# Runbook: conciliação e operações com resultado desconhecido

Cobre as operações externas sem confirmação segura (`payment_operations` em `unknown`, ADR-008), as
operações que esgotaram as tentativas automáticas, os reembolsos sem conclusão e as divergências
entre o Recorvia e o provedor de pagamento (RF-12). As execuções e divergências
de conciliação seguem o contrato `api/openapi.yaml` (tag `reconciliation`); a situação de cada
funcionalidade está em `docs/STATUS.md`. A métrica `recorvia_reconciliation_open_discrepancies`
(por `kind`) é exposta pelo worker junto com a funcionalidade de conciliação (etapa 5, recebíveis
e conciliação); enquanto ela não tiver séries, os alertas de divergência não disparam.

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
-- Operações em unknown que esgotaram as tentativas automáticas (6) e dependem da conciliação.
-- As esgotadas em pending e in_flight estão em Operações esgotadas, abaixo.
SELECT tenant_id, id, charge_id, external_reference, attempts, updated_at
  FROM payment_operations WHERE status = 'unknown' AND attempts >= 6 ORDER BY updated_at;
```
- API (`payments.read`): `GET /api/v1/payment-operations` e `GET /api/v1/charges/{id}`.
- Abaixo de 6 tentativas, a tarefa `payments.recover_operations` (a cada minuto) retoma a operação
  no horário de `next_attempt_at` e sempre consulta o provedor pela `external_reference` antes de
  criar: se a cobrança existir, é registrada; se não existir, a criação é repetida com segurança.
  Só a execução imediata, logo após gravar a operação, cria sem consultar antes.
- No provedor: procure a cobrança pela referência externa (no Asaas, `externalReference`; no
  simulador, `GET /v1/charges?external_reference=<referência>`).

### Ações
1. Provedor indisponível: aguardar; a recuperação resolve sozinha dentro das tentativas.
2. Dentro do limite: `POST /api/v1/payment-operations/{id}/retry` (`payments.charge`) executa a
   tentativa agora, sem esperar `next_attempt_at`, e sempre consulta o provedor antes de criar; a
   resposta 200 traz a situação resultante. Operação concluída, em execução com lease vigente ou
   esgotada responde `409` sem executar nada (código no corpo: `operation_completed`,
   `operation_in_progress` ou `operation_attempts_exhausted`).
3. Esgotadas as tentativas: executar a conciliação do período
   (`POST /api/v1/reconciliation/runs`, `reconciliation.run`, com `Idempotency-Key`); a operação
   aparece como divergência `unknown_operation` e é resolvida conforme
   [Divergências abertas](#divergencias-abertas).
4. Não altere `payment_operations` nem `charges` à mão.

### Verificação
`recorvia_payment_operations{status="unknown"}` volta a zero e a cobrança tem situação definitiva
(`awaiting_payment`, `paid` ou `failed`).

<a id="operacoes-esgotadas"></a>

## Operações esgotadas

### Sintomas
- Cobrança `pending` sem link de pagamento cuja operação de criação parou: a recuperação agendada
  não retoma operações que chegaram a 6 tentativas (`payments.MaxOperationAttempts`).
- Uma criação `pending` cuja cobrança deixou de estar pendente (cancelada com a fatura anulada)
  não conta como esgotada: o provedor recusou a última tentativa de forma transitória, a cobrança
  não existe nele e não será mais criada (`Operation.Exhausted`).

### Métricas e alertas
- `RecorviaPaymentOperationsExhausted`:
  `max by (status) (recorvia_payment_operations_exhausted{status=~"pending|in_flight"}) > 0` por
  15 minutos. A contagem é feita a cada minuto pela tarefa `payments.recover_operations`, nos
  tenants com operações vencidas, e só é substituída por uma execução completa.
- As esgotadas em `unknown` entram em `RecorviaPaymentOperationsUnknown`
  ([seção anterior](#operacoes-com-resultado-desconhecido)).
- Painel "Operações de pagamento por situação" (dashboard *visão geral*, séries "esgotadas").

### Diagnóstico
```sql
-- Operações paradas no limite de tentativas, por tenant e situação (superusuário), com a
-- mesma condição da métrica: sem as criações pending de cobranças que deixaram de estar pendentes.
SELECT o.tenant_id, o.kind, o.status, count(*), min(o.updated_at) AS desde, left(max(o.last_error), 200) AS erro
  FROM payment_operations o JOIN charges c ON c.tenant_id = o.tenant_id AND c.id = o.charge_id
 WHERE o.attempts >= 6
   AND ((o.status IN ('pending', 'unknown') AND o.next_attempt_at <= now()) OR (o.status = 'in_flight' AND o.lease_until < now()))
   AND NOT (o.status = 'pending' AND o.kind = 'create_charge' AND c.status <> 'pending')
 GROUP BY 1, 2, 3 ORDER BY 4 DESC;
```
- `pending`: todas as tentativas terminaram com recusa transitória do provedor (limite de
  requisições, indisponibilidade) e, a partir da segunda, cada uma consultou o provedor pela
  referência externa antes de criar: a cobrança não existe no provedor.
- `in_flight` com lease vencido: o processo foi interrompido durante a última chamada; o resultado
  é desconhecido.

### Ações
1. Corrigir a causa (limite de requisições, credencial, indisponibilidade; ver
   [Falhas de cobrança](faturamento.md#falhas-de-cobranca)).
2. `pending`: reexecutar cada operação com `POST /api/v1/payment-operations/{id}/retry`
   (`payments.charge`). Acima do limite, a reexecução manual só é aceita para `pending`; ela
   consulta o provedor antes de criar e conta mais uma tentativa, e a recuperação agendada continua
   sem retomar a operação. Se a cobrança deixou de estar pendente (fatura anulada) depois da
   consulta acima, a reexecução não cria nada: só consulta o provedor pela referência externa,
   vincula a cobrança encontrada sem `charge.created` (auditoria
   `charge.linked_after_local_cancellation`) ou, sem cobrança no provedor, encerra a operação como
   `failed`.
3. `in_flight`: tratar como resultado desconhecido e executar a conciliação do período; a operação
   aparece como divergência `unknown_operation` e é resolvida conforme
   [Divergências abertas](#divergencias-abertas).
4. Não altere `attempts` nem `status` à mão.

### Verificação
`recorvia_payment_operations_exhausted` volta a zero na execução seguinte da recuperação e as
cobranças afetadas têm situação definitiva (`awaiting_payment`, `paid`, `failed` ou, com a fatura
anulada, `canceled`).

<a id="reembolsos-sem-confirmacao"></a>

## Reembolsos sem confirmação

As métricas desta seção (`recorvia_refunds_in_progress` e `recorvia_refunds_total`) são exportadas
pela etapa 5 (recebíveis e conciliação); até a integração, os alertas não têm dados.

### Sintomas
- Reembolso solicitado (`POST /api/v1/charges/{id}/refunds`) sem conclusão, ou recusado pelo
  provedor.

### Métricas e alertas
- `RecorviaRefundsUnknown`: `max(recorvia_refunds_in_progress{status="unknown"}) > 0` por
  30 minutos. O pedido ao provedor ficou sem resposta segura: o cliente pode ter sido reembolsado
  sem registro local.
- `RecorviaRefundsFailed`: `sum(increase(recorvia_refunds_total{outcome="failed"}[1h])) > 0`. O
  provedor recusou o reembolso e o valor não foi devolvido.

### Diagnóstico
```sql
-- Operações de reembolso sem conclusão, por tenant e situação (superusuário).
SELECT tenant_id, status, count(*), max(attempts) AS tentativas, left(max(last_error), 200) AS erro
  FROM payment_operations WHERE kind = 'refund' AND status IN ('pending', 'in_flight', 'unknown')
 GROUP BY 1, 2;
```
- API (`payments.read`): `GET /api/v1/refunds` e `GET /api/v1/payment-operations?status=unknown`.
- No provedor: o estorno da cobrança pela referência externa.

### Ações
1. Resultado desconhecido: não solicite novo reembolso antes de consultar o provedor (risco de
   devolver duas vezes). Execute a conciliação do período; a operação aparece como divergência
   `unknown_operation` e um estorno já registrado no provedor, como `refund_pending`.
2. Recusado: registre o motivo (`last_error` da operação), corrija a causa (saldo da conta no
   provedor, cobrança que não admite estorno) e solicite novo reembolso, ou devolva por outro meio
   e registre o ajuste pela conciliação.

### Verificação
`recorvia_refunds_in_progress{status="unknown"}` volta a zero e cada reembolso afetado termina em
`succeeded` ou `failed`, com o motivo registrado.

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
