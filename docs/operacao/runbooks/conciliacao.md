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
3. Esgotadas as tentativas: pedir a conciliação do período
   (`POST /api/v1/reconciliation/runs`, `reconciliation.run`, com `Idempotency-Key`; a resposta é
   `202` e o worker executa, ver [Execuções de conciliação](#execucoes-de-conciliacao)); a operação
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

<a id="execucoes-de-conciliacao"></a>

## Execuções de conciliação

A conciliação manual é assíncrona (ADR-071): `POST /api/v1/reconciliation/runs` só registra a
execução (`reconciliation_runs`, situação `running`, sem lease) e responde `202` com o header
`Location`; o executor do worker (fora do agendador, 2 instâncias por worker, consulta a cada 2 s)
assume a execução com lease de 15 minutos, coleta no provedor (prazo de 13 minutos por tentativa),
compara e a encerra. A conciliação agendada (`payments.reconcile`) é registrada já assumida e
executada na própria tarefa; se for interrompida, segue o mesmo caminho de retomada.

| Situação (`GET /api/v1/reconciliation/runs/{id}`) | Significado |
|---|---|
| `running`, `attempts = 0` | registrada, aguardando um worker |
| `running`, `attempts >= 1` | em execução por um worker ou, se o worker foi interrompido, aguardando a retomada |
| `completed`, `error` vazio | concluída; `checked_count` e `discrepancy_count` valem para a janela inteira |
| `completed`, `error` preenchido | **parcial**: o texto traz o motivo (limite de páginas, de consultas individuais, de cobranças locais, de operações desconhecidas ou prazo da coleta); o que não foi verificado não gera nem resolve divergência |
| `failed` | encerrada sem concluir: `error` traz a causa (etapa e mensagem do provedor, ou as três tentativas interrompidas); nenhuma divergência desta execução foi gravada |

Regras que valem para qualquer ação abaixo:
- há no máximo uma execução `running` por conta do provedor, inclusive enquanto ela aguarda o
  worker ou a retomada: outro pedido responde `409 reconciliation_running` com `Retry-After`;
- a mesma `Idempotency-Key` devolve sempre a mesma execução, na situação atual (`202` enquanto
  `running`, `200` depois de encerrada); para acompanhar, consulte `GET .../runs/{id}`, não repita
  o `POST`;
- as divergências só são gravadas na transação que encerra a execução e são deduplicadas pela
  chave: uma execução interrompida não deixa divergência pela metade, e a retomada não duplica
  nenhuma.

<a id="execucoes-paradas"></a>

### Execução parada (sem worker ou interrompida)

#### Sintomas
- Execução `running` há vários minutos; a interface mostra "Em execução" sem término; novos pedidos
  respondem `409 reconciliation_running`.

#### Métricas e alertas
- `RecorviaReconciliationRunsWaiting`: `max(recorvia_reconciliation_oldest_waiting_seconds) > 300`
  por 5 minutos. A métrica é a espera mais longa entre as execuções sem worker: desde o registro
  (aguardando) ou desde o fim do lease (interrompida).
- `recorvia_reconciliation_runs_in_progress{state}`: `queued` (aguardando um worker), `executing`
  (lease vigente) e `stalled` (lease expirado, aguardando a retomada).
- As duas métricas saem do worker (tarefa `metrics.collect`) e da API, que as lê do banco a cada
  coleta. Com todos os workers parados, as séries do worker somem e a da API sustenta o alerta, que
  dispara junto com `RecorviaTargetDown` do worker.
- Limite do alerta: com a API e o worker sem acesso ao banco, as séries ficam no último valor lido
  e o alerta pode não disparar; o sinal é `RecorviaDependencyUnavailable` (PostgreSQL, na API) e o
  `/readyz` do worker.

#### Diagnóstico
```sql
-- Execuções em andamento: estado, tentativas e espera (superusuário).
SELECT tenant_id, id, trigger_type, attempts, started_at, lease_until,
       CASE WHEN lease_until IS NULL THEN 'queued' WHEN lease_until < now() THEN 'stalled' ELSE 'executing' END AS estado
  FROM reconciliation_runs WHERE status = 'running' ORDER BY started_at;
```
- `queued` ou `stalled` que não andam: o worker está parado (`RecorviaTargetDown` também ativo),
  sem acesso ao banco ou com as duas instâncias do executor ocupadas por execuções longas
  (`executing`). Verifique o processo (`/readyz` do worker, que confere o PostgreSQL) e o log
  `executor de conciliações falhou`. O executor não depende do RabbitMQ: parte junto com o
  agendador.
- `executing` por mais de 15 minutos não existe: o lease expira e a execução passa a `stalled`.
- Auditoria da execução (`GET /api/v1/audit-events?resource_type=reconciliation_run&resource_id=<id>`):
  `reconciliation.run_started`, `reconciliation.run_resumed` (retomada, com o número da tentativa),
  `reconciliation.run_released` (worker encerrado durante a execução), `reconciliation.run_completed`
  e `reconciliation.run_failed`.

#### Ações
1. Restabelecer o worker. Na partida, o executor assume as execuções `queued` e as `stalled` sem
   intervenção; a execução devolvida por um worker que encerrou normalmente (`run_released`) é
   retomada de imediato, e a de um worker interrompido, depois do fim do lease (até 15 minutos).
2. Muitas execuções longas ao mesmo tempo: acrescentar instâncias do worker (cada uma executa duas
   conciliações em paralelo).
3. Execução encerrada como `failed` depois de três tentativas interrompidas: corrigir a causa das
   interrupções (reinícios do worker, falta de memória, prazo da tentativa) e pedir nova
   conciliação; a anterior não gravou divergências.
4. Não altere `reconciliation_runs` à mão: encerrar uma execução por `UPDATE` não impede o worker
   que a detém de concluí-la e deixa a auditoria sem a causa.

#### Verificação
`recorvia_reconciliation_oldest_waiting_seconds` volta a zero, a execução termina em `completed` ou
`failed` com a causa e a conta volta a aceitar pedidos.

### Execução parcial ou com falha

1. `failed` com etapa do provedor em `error` (`listagem do provedor`, `consulta da cobrança ...`):
   ver [Falhas de cobrança](faturamento.md#falhas-de-cobranca) (credencial, limite de requisições,
   indisponibilidade) e pedir nova conciliação depois de corrigir.
2. `completed` com aviso de execução parcial: as divergências registradas valem; o restante do
   escopo fica para a próxima execução (as cobranças fora da janela entram pelo rodízio). Se o
   motivo for o prazo da coleta, o provedor está lento: confira `recorvia_charge_failures_total` e
   repita fora do horário de pico.

<a id="chargeback-com-credito"></a>

## Chargeback de pagamento com excedente

Quando o pagamento contestado gerou crédito do cliente por excedente, a disputa perdida estorna
também esse crédito (ADR-070). Em `GET /api/v1/disputes`, `credit_amount` é o crédito consumido do
saldo do cliente e `recovery_amount` é a parte que ele já havia usado em outras faturas: o saldo
de crédito nunca fica negativo, e esse valor é lançado na conta `customer_recoverables` (valores a
recuperar de clientes) do ledger.

```sql
-- Disputas perdidas com saldo a recuperar do cliente, por tenant (superusuário).
SELECT d.tenant_id, d.id, d.charge_id, c.customer_id, d.recovery_amount, d.recovery_received_amount,
       d.recovery_written_off_amount,
       d.recovery_amount - d.recovery_received_amount - d.recovery_written_off_amount AS outstanding, d.closed_at
  FROM disputes d JOIN charges c ON c.tenant_id = d.tenant_id AND c.id = d.charge_id
 WHERE d.status = 'lost' AND d.recovery_amount > d.recovery_received_amount + d.recovery_written_off_amount
 ORDER BY d.closed_at DESC;
```
- A cobrança do valor a recuperar é decisão do tenant (por exemplo, uma fatura avulsa para o
  cliente). O Recorvia registra o valor na disputa, no histórico da fatura (`reason = chargeback`),
  na auditoria (`dispute.lost`, `dispute.chargeback_increased`) e no evento `dispute.closed`.
- O lançamento `dispute.credit_reversed` (`GET /api/v1/ledger/transactions?reference_type=dispute&reference_id=<id>`)
  mostra as duas partes: débito em `customer_credits` (crédito consumido) e em
  `customer_recoverables` (valor a recuperar), crédito em `chargebacks`.

<a id="baixa-do-valor-a-recuperar"></a>

### Baixa do valor a recuperar

O valor a recuperar sai de `customer_recoverables` pela baixa da disputa (ADR-073), feita na tela
de disputas ("Dar baixa") ou por `POST /api/v1/disputes/{id}/recovery-settlements`
(`payments.settle_recovery`, com `Idempotency-Key`), sempre com valor, data efetiva e
justificativa:

| Desfecho | Quando usar | Lançamento |
|---|---|---|
| `received` (recebido) | o cliente pagou o valor fora do provedor (transferência, acerto comercial) | `dispute.recovery_received`: débito em `cash`, crédito em `customer_recoverables` |
| `written_off` (perdido) | o tenant desistiu de cobrar | `dispute.recovery_written_off`: débito em `bad_debt`, crédito em `customer_recoverables` |

- Baixas parciais se acumulam; a soma nunca passa do valor a recuperar (o banco recusa, também com
  pedidos concorrentes: `409 recovery_exceeds_outstanding`). Com o saldo zerado, a disputa recusa
  novas baixas (`409 recovery_already_settled`).
- `GET /api/v1/disputes/{id}` mostra `recovery_received_amount`, `recovery_written_off_amount` e
  `recovery_outstanding_amount` (quanto falta recuperar); `GET /api/v1/disputes/{id}/recovery-settlements`
  lista as baixas com justificativa e autor (nulo para credencial de API). Cada baixa grava a
  auditoria `dispute.recovery_settled` e o evento `dispute.recovery_settled`.
- A baixa é imutável e não há, nesta versão, operação de estorno da baixa nem de recuperação de
  valor já baixado como perdido (ADR-073). Antes de registrar, confira valor e desfecho (a tela pede
  confirmação). Uma baixa registrada por engano, ou um recebimento que chega depois da baixa como
  perdido, exige decisão do financeiro sobre o lançamento compensatório; até lá, registre o caso na
  justificativa das baixas seguintes ou no histórico do cliente.
- O recebimento da baixa vai para o disponível liquidado (`cash`) pelo instante do registro, não
  pela data efetiva; ele não entra no indicador "Recebido" do painel (que soma pagamentos de
  faturas). O indicador "Valores a recuperar de clientes" (`open_recoverables`) é o saldo atual de
  `customer_recoverables` e deve bater com a soma de `outstanding` da consulta acima.

```sql
-- Conferência por tenant (superusuário): saldo de customer_recoverables no ledger contra o saldo a
-- recuperar das disputas. Esperado: nenhuma linha.
SELECT coalesce(l.tenant_id, d.tenant_id) AS tenant_id, coalesce(l.balance, 0) AS ledger_balance,
       coalesce(d.outstanding, 0) AS disputes_outstanding
  FROM (SELECT e.tenant_id, sum(CASE WHEN e.direction = 'debit' THEN e.amount ELSE -e.amount END) AS balance
          FROM ledger_entries e JOIN ledger_accounts a ON a.tenant_id = e.tenant_id AND a.id = e.account_id
         WHERE a.code = 'customer_recoverables' GROUP BY e.tenant_id) l
  FULL JOIN (SELECT tenant_id, sum(recovery_amount - recovery_received_amount - recovery_written_off_amount) AS outstanding
               FROM disputes GROUP BY tenant_id) d ON d.tenant_id = l.tenant_id
 WHERE coalesce(l.balance, 0) <> coalesce(d.outstanding, 0);
```

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
  `GET /api/v1/reconciliation/discrepancies/{id}` (valores local e do provedor, histórico),
  `GET /api/v1/reconciliation/runs` (execuções, janela e erro) e
  `GET /api/v1/reconciliation/runs/{id}` (uma execução, para acompanhar até o encerramento).
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
1. Corrigir a causa sistêmica primeiro (webhooks, credenciais, consumidor parado); depois pedir
   nova conciliação da janela e acompanhar a execução até `completed`: ela fecha as divergências
   resolvidas pelo próprio fluxo.
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
