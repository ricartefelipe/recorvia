# Dinheiro, datas e arredondamento

## Dinheiro

- Valores finais: `int64` em unidades mínimas (centavos para BRL) com moeda explícita
  (`money.Money`). Nenhum valor monetário passa por `float32`/`float64`, nem no banco (colunas
  `bigint`), nem na API (inteiros em centavos), nem na interface (texto convertido com aritmética
  de string).
- Cálculos intermediários: `math/big.Rat` (racionais exatos). Preços unitários fracionários e
  quantidades são `NUMERIC` no banco e chegam à API como texto decimal (`unit_amount_decimal`, em
  centavos, até 10 casas). Valores na escala do preço unitário (preço por unidade da versão de
  preço, valor unitário de itens, reajustes e bases de prorrata) são `numeric(28,10)`: 18 dígitos
  inteiros comportam `money.MaxAmount` (10¹⁴ centavos) inclusive no limite exato.
- Arredondamento: **uma única vez por item**, com `HalfEven` (empate vai para o par, critério da
  ABNT NBR 5891). `HalfUp` e `TowardZero` existem no código, mas não são usados por padrão.
- Overflow: somas e multiplicações verificam o limite de `int64`; entradas de negócio são
  limitadas a ±10¹⁴ centavos (`money.MaxAmount`).
- Distribuição de diferenças: `money.Allocate` usa o maior resto com desempate pelo menor índice.
  A soma das parcelas é sempre igual ao total (propriedade verificada por fuzz test). É usada
  para distribuir descontos entre itens.
- Moeda: somente BRL nesta versão; operações entre moedas diferentes falham.

### Exemplos (testados)

| Cálculo | Exato | Resultado |
|---|---|---|
| R$ 100,00 × 17/31 (prorrata) | 54,838709… | **R$ 54,84** |
| 10% de R$ 0,25 (half-even) | 0,025 | **R$ 0,02** |
| 10% de R$ 0,25 (half-up) | 0,025 | R$ 0,03 |
| 3 × R$ 49,905 por usuário | 149,715 | **R$ 149,72** |
| Taxa do simulador sobre R$ 990,00 (2,99% + R$ 0,49) | 29,601 + 0,49 | **R$ 30,09** |
| Desconto de R$ 1,00 sobre itens de 1, 1 e 1 | 0,333… cada | 0,34 / 0,33 / 0,33 |

## Datas

- Instantes: `timestamptz` no banco, `time.Time` em UTC no código.
- Regras de calendário: fuso IANA do tenant (padrão `America/Sao_Paulo`). Cada assinatura grava o
  fuso na criação, para que mudanças posteriores do tenant não alterem contratos em curso.
- Intervalos: `[início, fim)`. O início é o primeiro instante do dia civil local; se a meia-noite
  não existir (início de horário de verão, como em São Paulo até 2019), o dia começa na transição.
  O caso foi encontrado pelo fuzz test do calendário.
- Vencimento: `due_at` é o fim exclusivo do dia de vencimento (início do dia seguinte); a fatura
  está vencida quando `agora ≥ due_at`. A data exibida é o dia anterior a `due_at`, no fuso.

### Ciclos e âncora

O n-ésimo período é calculado a partir da **âncora original** (data local do primeiro período),
nunca a partir do período anterior: `início(n) = âncora + n meses`, com o dia limitado ao último
dia do mês.

| Âncora | Períodos mensais |
|---|---|
| 31/01/2025 | 31/01, 28/02, **31/03**, 30/04, **31/05**… |
| 30/01/2028 | 30/01, 29/02 (bissexto), 30/03… |
| 29/02/2024 (anual) | 29/02/2024, 28/02/2025, 28/02/2026, 28/02/2027, 29/02/2028 |

### Retomada após indisponibilidade

O motor fatura todos os períodos vencidos (`next_billing_at ≤ agora`), até 24 por transação, em
ordem. Cada fatura de ciclo é única por `(tenant, assinatura, tipo, início do período)` (índice
único). Uma nova execução não gera duplicatas.

## Política de cobrança

- Componente fixo (valor fixo ou por quantidade): **antecipado**, no início de cada período.
- Consumo e excedente: **pós-pago**, na fatura do período seguinte (ou na fatura final).
- Trial: o primeiro período começa no fim do trial (âncora = data local do fim do trial).
- Suspensão: períodos inteiros decorridos durante a suspensão não são faturados; a reativação
  retoma no período corrente, se ainda não faturado. O consumo anterior à suspensão não se perde
  quando a reativação pula o passo que o fecharia: o período em que a suspensão começou (e, se o
  faturamento estava atrasado no momento da suspensão, os períodos anteriores ainda abertos) fica
  em `subscriptions.pending_usage_starts` e é faturado no primeiro faturamento após a reativação
  (fatura do ciclo, de encerramento ou de consumo final), no lugar do período anterior regular,
  que é inteiramente suspenso. Cada período pendente é fechado com a sua própria agregação e
  precificado pela versão de preço vigente nele. Exemplo: ciclo mensal no dia 10, período
  10/04–10/05 faturado, suspensão em 25/04 e reativação em 15/06: a fatura do período 10/06–10/07
  traz o componente fixo de junho e o consumo de 10/04 a 25/04; o período 10/05–10/06 não tem
  componente fixo, consumo nem agregado.

## Prorrata

- Unidade: **dias civis** do período de cobrança, no fuso do tenant.
- Fração restante = dias da data local da alteração (inclusive) até o fim do período (exclusivo)
  ÷ dias do período. O dia da alteração conta inteiro para a nova condição.
- Exemplo: período 01/03 a 01/04 (31 dias), alteração em 15/03 às 15h → 17/31 restante.
- O resultado fica sempre entre 0 e 1 (propriedade verificada por fuzz test).

### Mudança imediata de plano ou quantidade

Somente com o período corrente já faturado (fora do trial). Credita-se a fração não usufruída do
valor **efetivamente faturado** pelo componente fixo vigente no período e cobra-se a mesma fração
do novo componente, cada valor arredondado uma única vez (half-even). Exemplo: período de 30 dias
faturado por R$ 100,00, troca no dia 21 para R$ 160,00 → fração 10/30; crédito R$ 33,33; cobrança
R$ 53,33; diferença **R$ 20,00**.

| Diferença | Efeito |
|---|---|
| positiva | fatura de prorrata imediata (item de cobrança proporcional seguido do item de crédito proporcional) |
| negativa | crédito do cliente (`customer_credit_entries`, tipo `proration`), consumido nas próximas faturas |
| zero | nenhum documento; somente histórico |

Base do crédito (valor faturado pelo componente fixo vigente para o período integral):

| Situação do período | Base |
|---|---|
| sem mudança imediata anterior | item `subscription_fee` da fatura de ciclo do período: `amount − discount_amount` (desconto distribuído na emissão, percentual ou de valor fixo) |
| mudança imediata anterior no mesmo período | valor cobrado pelo componente que ela pôs em vigor, para o período integral (`subscription_prorations.charge_base`) |
| fatura de ciclo cancelada ou inexistente | sem crédito |

O cupom ativo no momento da mudança não altera o crédito: um cupom aplicado no meio do período não
reduz o crédito (o período foi faturado sem ele) e um cupom de uso único já encerrado não o aumenta
(o período foi faturado com ele). Notas de crédito reduzem o saldo da fatura, não o item, e não
mudam a base. Uma prorrata cuja fatura foi cancelada deixa de valer como base; vale a anterior (ou
a fatura de ciclo). Toda prorrata, inclusive com diferença zero, é registrada em
`subscription_prorations` (imutável), com as bases e os valores do crédito e da cobrança e a fatura
de prorrata, quando houver.

A cobrança desconta o percentual do cupom ativo que vale para o novo plano (zero quando o cupom não
vale para ele e é encerrado na troca); cupom de valor fixo não é prorrateado na cobrança (continua
valendo por fatura). Cada valor é arredondado uma única vez: crédito = base × fração; cobrança =
novo × fração × (1 − p_novo). Exemplos (troca no dia 21 de 30):

| Período faturado | Troca | Crédito | Cobrança | Diferença |
|---|---|---|---|---|
| R$ 90,00 (cupom de 10% válido nos dois planos) | para R$ 160,00 | R$ 30,00 | R$ 48,00 | R$ 18,00 |
| R$ 100,00 (cupom de 10% aplicado depois da fatura) | para R$ 160,00 | R$ 33,33 | R$ 48,00 | R$ 14,67 |
| R$ 90,00 (cupom de uso único, já encerrado) | para R$ 160,00 | R$ 30,00 | R$ 53,33 | R$ 23,33 |
| R$ 50,00 (cupom de R$ 50,00) | para R$ 160,00 | R$ 16,67 | R$ 53,33 | R$ 36,66 |
| R$ 144,00 (cupom de 10% restrito ao plano anterior) | para R$ 100,00 | R$ 48,00 | R$ 33,33 | crédito do cliente de R$ 14,67 |
| fatura de ciclo cancelada | para R$ 160,00 | — | R$ 53,33 | R$ 53,33 |

Duas mudanças no mesmo período: de R$ 100,00 para R$ 160,00 no dia 21 e de volta no dia 26 (5 de
30 dias) → o segundo crédito é 16.000 × 5/30 = R$ 26,67 (o valor cobrado pelo plano de R$ 160,00),
e não 10.000 × 5/30. O item de crédito registra no snapshot a origem da base (`credit_basis`:
`cycle_invoice`, `proration` ou `none`, com a fatura e o item de origem) e `credit_base_decimal`;
o percentual da cobrança fica no próprio item (descrição e snapshot), sem desconto adicional sobre a
diferença. Planos com consumo medido só mudam no próximo ciclo, para que cada período de consumo
seja precificado por uma única versão de preço.

### Mudança no próximo ciclo e reajustes

- Mudança agendada vale no início do período seguinte, com a versão de preço fixada no agendamento;
  o consumo do período anterior continua precificado pela versão anterior. Entre o início do novo
  período e a execução do motor, a mudança já está em vigor mas ainda não foi aplicada: a ingestão
  e a estimativa do consumo corrente usam, para todo período aberto que começa na data efetiva ou
  depois, a versão de preço da mudança (é a versão que vai precificar o fechamento).
  Enquanto uma mudança já em vigor aguarda o processamento do ciclo pelo motor, uma nova mudança é
  recusada (`409 scheduled_change_in_effect`).
- Reajuste programado (percentual em pontos-base ou novo valor) vale no primeiro período iniciado
  na data efetiva ou depois, sobre o componente fixo vigente. No valor fixo, o resultado é
  arredondado a centavos uma única vez. No preço por unidade, o percentual incide sobre o preço
  unitário exato e o resultado é arredondado (half-even) a 10 casas, a escala da coluna
  (`price_override_amount` e `previous_amount`/`resulting_amount` são `numeric(28,10)` e aceitam o
  limite exato de `money.MaxAmount`); o total do
  item (preço unitário × quantidade) é arredondado a centavos uma única vez. Exemplo: R$ 0,045 por
  unidade + 5% = R$ 0,04725; 100.000 unidades = R$ 4.725,00. A API devolve os valores anterior e
  resultante arredondados a centavos (exibição) e exatos em texto (`*_decimal`).
- O limite do valor resultante (`money.MaxAmount`, inclusive no total com a quantidade contratada)
  é verificado no agendamento. Ciclos já faturados nunca mudam.
- Mudança de versão de preço (mudança agendada aplicada pelo motor ou mudança imediata) remove o
  reajuste vigente e cancela os reajustes programados (`cancel_reason = price_version_changed`),
  com registro no histórico: foram programados sobre o preço anterior. Troca de quantidade na
  mesma versão mantém o reajuste.
- No início do ciclo, um reajuste vencido que não pode valer para o preço vigente (preço somente
  por consumo ou valor acima do limite) é cancelado com o motivo, com evento no histórico e
  auditoria, sem interromper o faturamento do ciclo.
- Cancelamento da assinatura descarta mudanças e reajustes pendentes.

### Consumo recebido depois do fechamento

O período de consumo fecha quando o motor fatura o período seguinte. Evento posterior para um
período fechado é aceito como `late` até `late_grace_hours` (configuração da métrica) após o fim
do período; depois disso, é rejeitado. A métrica vale para o período fechado mesmo que o plano
tenha mudado na virada do ciclo (o agregado guarda a versão de preço do fechamento). Consumo
ocorrido durante uma suspensão é rejeitado pelo histórico da assinatura, também depois da
reativação. A correção (job `billing.correct_late_usage`, a cada 5 minutos) trata os eventos
`late` e, como defesa, eventos `accepted` que tenham ficado sem agregado num período já fechado
(a situação do evento não muda; ele passa a apontar o agregado). Ela recalcula a quantidade com a
mesma semântica de agregação, precifica pela versão usada no fechamento e compara com o valor já
faturado mais correções anteriores. A comparação usa valores líquidos de desconto: o valor
faturado é o do item menos o desconto distribuído nele, e o novo valor segue o tipo do desconto que
a emissão registrou no snapshot do item (`discount_type`, `discount_id`, `coupon_code` e
`discount_percent_bp` ou `discount_amount_off`), arredondado uma única vez (`corrected_amount`
guarda o líquido; nos detalhes da correção, `billed_discount_type` repete o tipo do desconto do item
original e `discount_rule` registra a regra usada):

| Desconto do item | Novo líquido | Exemplo: item de R$ 100,00 com R$ 50,00 de desconto, recontagem para R$ 60,00 |
|---|---|---|
| percentual (`percent`) | novo bruto × proporção líquida do item | R$ 30,00 (crédito de R$ 20,00) |
| valor fixo (`amount`) | novo bruto − mesmo desconto, nunca negativo | R$ 10,00 (crédito de R$ 40,00) |
| sem desconto (`none`) | novo bruto | — |
| sem tipo registrado (`unrecorded_ratio`) | proporção, como no percentual | R$ 30,00 |

Itens faturados antes do registro do tipo não têm `discount_type` no snapshot: com desconto, a
correção usa a proporção do item (comportamento anterior), o que trata um desconto de valor fixo
como percentual; a regra aplicada fica registrada para conferência.

| Diferença | Efeito |
|---|---|
| positiva, assinatura com próximo ciclo | item de ajuste na próxima fatura, sem novo desconto |
| positiva, assinatura encerrada | fatura de ajuste imediata |
| negativa | crédito do cliente (tipo `usage_correction`) |

Itens pendentes ainda abertos entram também na fatura de encerramento (fim do período ou consumo
final após cancelamento imediato); sem consumo a faturar, vão para uma fatura de ajuste. O
cancelamento imediato com itens pendentes abertos agenda o fechamento mesmo em plano sem consumo.

A fatura emitida não é alterada (gatilho `app_invoice_guard`).

#### Custo da descoberta de consumo a corrigir

A descoberta (`app_tenants_with_late_usage()`, a cada 5 minutos, e `LateUsageGroups` por tenant)
tem duas partes, cada uma com o predicado de um índice parcial de `usage_events`; assim, o custo
não depende do histórico de eventos já fechados e corrigidos:

| Parte | Índice | Tamanho do índice |
|---|---|---|
| eventos `late` ainda não corrigidos | `usage_events_late_idx` (`status = 'late' AND corrected_at IS NULL`) | eventos atrasados pendentes; a própria correção os retira (marca `corrected_at`) |
| eventos `accepted` sem agregado num período já fechado | `usage_events_unattached_idx` (`status = 'accepted' AND aggregate_id IS NULL`), cruzado com `usage_aggregates` por `(tenant, assinatura, métrica, início do período)` | eventos de períodos ainda abertos: o fechamento vincula os eventos ao agregado na mesma transação |

O custo da primeira parte é proporcional aos eventos atrasados pendentes. O da segunda é uma busca
no índice parcial por agregado (ou uma busca no índice único `usage_aggregates_period_key` por
evento de período aberto, conforme o plano escolhido): cresce com o número de agregados (uma linha
por assinatura, métrica e período fechado) ou com o consumo dos períodos abertos, nunca com o
histórico de eventos. `LateUsageGroups` aplica o filtro de tenant e um limite por transação
(`BatchSize`, 200 grupos). Períodos pendentes de uma reativação continuam abertos até o primeiro
faturamento após ela e, até lá, seus eventos ficam no índice parcial de eventos sem agregado.
