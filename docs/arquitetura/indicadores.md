# Indicadores do painel financeiro

Fórmulas do painel (`GET /api/v1/dashboard`, RF-17), conforme a ADR-074. O código está em
`internal/reporting` (fórmulas, bases e arredondamento), `internal/pgstore/reporting.go` (leitura) e
`db/queries/reporting.sql` (agregações). A resposta traz, para cada indicador, a fórmula resumida em
`definitions` e a base temporal em `bases`, com a mesma chave do campo; a interface usa essas chaves na dica
de cada card e na tabela "Como calculamos". Um teste (`internal/httpapi/dashboard_test.go`) exige que todo
indicador da resposta tenha fórmula e base no backend e no contrato.

## Convenções

- **Mês consultado.** `month=AAAA-MM` (padrão: o mês corrente) vira o intervalo `[início, fim)` da meia-noite
  do dia 1 à meia-noite do dia 1 do mês seguinte, no fuso da organização (`reporting.MonthRange`). Mês futuro é
  recusado com 422 (`future_month`): não há fluxo nem posição a mostrar.
- **Bases temporais** (`bases`):
  - `month`: fluxo do mês, somado no intervalo `[início, fim)`.
  - `month_end`: posição no instante `position_at`, que é o último instante do mês (fim menos 1 µs, a
    resolução do PostgreSQL) ou, no mês corrente, o momento da consulta (`reporting.PositionAt`). Eventos com instante até `position_at`, inclusive, entram.
  - `current`: posição atual, seja qual for o mês. Usada só onde não há histórico que permita reconstruir o
    valor de uma data passada com fidelidade (ver a ADR-074).
- **Valores** em centavos (`int64`). Somas exatas; frações (MRR) em `numeric` no banco e `big.Rat` na
  aplicação, arredondadas uma única vez com half-even.
- **Taxas** em pontos-base (100 pontos-base = 1%): `numerador × 10000 ÷ denominador`, arredondado com
  half-even; zero quando o denominador ou o numerador é zero ou negativo (`reporting.ratio`).
- **Situação de uma assinatura num instante**: a `to_status` do último registro de `subscription_history`
  com `occurred_at` até o instante (ordem por `occurred_at` e `id`, UUIDv7). Antes do primeiro registro (início
  retroativo, permitido até 366 dias: o histórico só começa na criação), a situação vem do contrato: `trialing`
  enquanto `trial_ends_at` for posterior ao instante e `active` depois. Assim, uma assinatura criada em 05/03
  com início em 01/12 e 14 dias de trial conta como ativa no início de janeiro, fevereiro e março. Toda mudança
  de situação passa por
  `Subscription.transition` e grava o histórico na mesma transação. O encerramento usa `ended_at`: no
  cancelamento ao fim do período, o fim do período, embora o motor registre a mudança no histórico um pouco
  depois.
- **Saldo de contas a receber de uma fatura num instante**: soma (débitos menos créditos) das partidas da conta
  `accounts_receivable` com `ledger_transactions.occurred_at` até o instante. As partidas referenciam a fatura
  (emissão, crédito aplicado na emissão, cancelamento por estorno da emissão e baixa como incobrável), o
  pagamento (`payments.invoice_id`: valor aplicado e recuperação de incobrável) ou a nota de crédito
  (`credit_notes.invoice_id`). No instante atual, o saldo de cada fatura em aberto é o seu `amount_due`
  (verificado em `TestDashboardReceivablesAtMonthEnd`). A data considerada é a do registro no ledger, não a
  data informada no recebimento manual (`paid_at`): assim, um mês encerrado não muda depois.
- **Leitura dos recebíveis.** No mês corrente, a posição é o momento da consulta e os recebíveis vêm da soma de
  `amount_due` das faturas `open` e `partially_paid` (`DashboardReceivablesCurrent`), sem ler o ledger. Num mês
  encerrado, `DashboardReceivablesAt` lê o ledger só das faturas que podiam ter saldo na posição: emitidas até
  ela e ainda em aberto, ou com `paid_at`, `voided_at` ou `uncollectible_at` depois dela (uma fatura encerrada
  antes da posição tinha saldo zero). As partidas de cada uma são achadas pela referência do lançamento (a
  fatura, os seus pagamentos e as suas notas de crédito), por índice (migration `00023`). As duas formas e a
  soma de todas as partidas do tenant coincidem em todos os instantes testados (`checkReceivableForms`), e
  `TestDashboardReceivablesAtDoesNotScanLedgerHistory` exige, com 3.000 faturas pagas no histórico, que a
  consulta não faça varredura sequencial nem leia mais que 50 linhas de cada tabela, pelo plano
  (`EXPLAIN ANALYZE`) e pelas estatísticas da transação.

## Receita

| Chave | Base | Fórmula |
|---|---|---|
| `billed` | mês | Soma de `invoices.total` (após descontos) das faturas com `issued_at` no mês, exceto rascunhos e canceladas. Inclui mensalidade, consumo, prorratas e ajustes. |
| `billed_recurring` | mês | Das mesmas faturas, soma de `amount − discount_amount` dos itens `subscription_fee` (componente fixo do ciclo). |
| `billed_usage` | mês | Das mesmas faturas, soma de `amount − discount_amount` dos itens `usage` e `overage` (consumo medido e excedente de franquia). |
| `received` | mês | Soma de `payments.amount` com `paid_at` no mês (provedor ou registro manual). Inclui a parte do pagamento que virou crédito do cliente por excedente. |

Prorratas e ajustes entram só em `billed`, por isso `billed_recurring + billed_usage` pode ser menor que
`billed`. Faturado e recebido são medidas distintas: uma fatura emitida num mês pode ser paga em outro.
Limitação: a exclusão das canceladas usa a situação atual, então cancelar hoje uma fatura emitida em mês
anterior reduz o faturado daquele mês.

## Recorrência

| Chave | Base | Fórmula |
|---|---|---|
| `mrr` | atual | Σ, nas assinaturas `active` ou `past_due`, de (componente fixo − desconto recorrente), com os planos anuais divididos por 12; arredondado uma vez. |
| `arr` | atual | MRR exato × 12, arredondado uma vez (não é o MRR arredondado × 12). |
| `active_subscriptions` | fim do mês | Assinaturas com início até a posição, situação `trialing`, `active` ou `past_due` na posição e sem encerramento (`ended_at`) até ela. Suspensas e encerradas ficam fora. Uma assinatura cadastrada depois com início retroativo passa a contar: o valor de um mês encerrado pode mudar. |
| `new_subscriptions` | mês | Assinaturas com `started_at` no mês (início do contrato, inclusive em trial; o início retroativo conta no mês do início, não no da criação). Pelo mesmo motivo, o valor de um mês encerrado pode mudar quando uma assinatura retroativa é cadastrada depois. |
| `canceled_subscriptions` | mês | Assinaturas com `ended_at` no mês, inclusive as canceladas no trial e as iniciadas no próprio mês. |
| `churn_rate_bp` | mês | Vigentes no início ÷ encerradas delas, em pontos-base (ver abaixo). |

**Componente fixo** (o mesmo valor da fatura de ciclo, `BuildCycleInvoice` com `Subscription.EffectivePrice`):
nos modelos `flat` e `flat_plus_overage`, `price_override_amount` (reajuste vigente, ADR-027) ou, sem
reajuste, `flat_amount` da versão de preço; no modelo `per_unit`, (`price_override_amount` ou `unit_amount`)
× `quantity`, exato, sem arredondar por assinatura; no modelo `usage`, zero.

**Desconto recorrente**: o cupom do desconto ativo da assinatura (`subscription_discounts.status = 'active'`)
com duração `forever` ou `repeating` (enquanto houver ciclos restantes, o desconto continua ativo). Percentual:
componente fixo × `percent_off_bp` ÷ 10000. Valor fixo: `min(amount_off, componente fixo)`; nos planos
anuais, o valor fixo é por fatura anual e também é dividido por 12. O cupom de uso único (`once`) fica fora:
reduz uma fatura, não a receita recorrente. O MRR é, portanto, **líquido** de descontos recorrentes.

Diferenças em relação à fatura: na fatura, o desconto incide sobre a base descontável inteira (inclusive
consumo) e é arredondado por fatura; no MRR, ele é limitado ao componente fixo e mantido exato até o
arredondamento final. Ficam fora do MRR: trial, suspensas, consumo medido, excedente, prorratas e cobranças
avulsas.

**Churn de contratos**:

- Denominador: assinaturas com `started_at` antes do início do mês, sem `ended_at` antes do início e com
  situação `active`, `past_due` ou `suspended` no início do mês (último registro do histórico antes do início
  ou, sem registro, a situação derivada do contrato).
- Numerador: dessas, as com `ended_at` no mês.
- Assinaturas em trial no início do mês (inclusive as canceladas no trial) e as iniciadas no mês não entram
  em nenhum dos dois: com uma ativa e uma cancelada no trial, o churn é 0%.

As contagens de assinaturas leem só as assinaturas iniciadas antes do fim do mês e não encerradas antes do
início dele; as encerradas antes do mês não entram em nenhum indicador e o histórico delas não é consultado
(`TestDashboardSubscriptionCountsSkipsSubscriptionsEndedBeforeMonth`).

## Recebíveis e inadimplência

| Chave | Base | Fórmula |
|---|---|---|
| `open_receivables` | fim do mês | Σ do saldo de contas a receber de cada fatura na posição, somadas as faturas com saldo positivo. |
| `overdue_receivables` | fim do mês | A mesma soma, só das faturas com `due_at` até a posição. |
| `delinquency_rate_bp` | fim do mês | Saldo na posição das faturas com `due_at` em `(posição − 90 dias, posição]` ÷ `invoices.total` dessas faturas, em pontos-base. |

Na base da inadimplência entram as faturas emitidas (não rascunho) com vencimento na janela e não canceladas
até a posição (`voided_at` nulo ou depois dela). Uma fatura baixada como incobrável continua na base com saldo
zero; uma cancelada depois do mês continua na base e no saldo daquele mês.

## Estornos e exceções

| Chave | Base | Fórmula |
|---|---|---|
| `refunds` | mês | Movimento líquido (débitos − créditos) da conta `refunds` com `occurred_at` no mês. |
| `chargebacks` | mês | Movimento líquido da conta `chargebacks` no mês: retenção na abertura, menos a retenção liberada na disputa perdida (parte reembolsada ou reservada a reembolso), o estorno da disputa ganha e a parte que era pagamento excedente (ADR-070). Pode ser negativo no mês em que uma disputa de mês anterior é ganha. |
| `open_recoverables` | fim do mês | Saldo (débitos − créditos) da conta `customer_recoverables` com `occurred_at` até a posição: excedente já usado estornado por chargeback menos as baixas recebidas e perdidas (ADR-073). |
| `open_discrepancies` | atual | Divergências de conciliação com `status = 'open'`, de qualquer mês. |
| `unknown_operations` | atual | Operações de pagamento (`payment_operations`) com `status = 'unknown'`. |

## Por que alguns indicadores são posição atual

- **MRR e ARR.** Reconstruir o MRR de uma data passada exigiria a versão de preço, a quantidade, o reajuste e o
  desconto vigentes na data. Esses valores existem só no estado atual da assinatura; o passado está apenas nos
  detalhes JSON do histórico (`subscription_history.details`), um registro de auditoria sem contrato de
  formato, e o desconto por ciclos não registra quantos ciclos restavam em cada data. Um MRR reconstruído a
  partir disso seria uma estimativa apresentada como fato. A solução fiel é registrar movimentos de MRR (ou um
  retrato mensal) daqui em diante, o que fica como evolução.
- **Divergências abertas e operações desconhecidas.** São filas de trabalho (os cards levam às telas que
  mostram a fila atual) e não têm histórico de situação: `payment_operations` não registra quando entrou ou
  saiu de `unknown`, e a divergência resolvida automaticamente que volta a ser detectada reabre na mesma linha,
  apagando a data da resolução anterior.

A interface rotula esses cards com "Posição atual" e os de base `month_end` com "Posição em <data e hora>", e
explica as bases no topo do painel a partir de `bases`.
