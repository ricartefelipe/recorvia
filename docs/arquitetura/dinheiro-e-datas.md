# Dinheiro, datas e arredondamento

## Dinheiro

- Valores finais: `int64` em unidades mínimas (centavos para BRL) com moeda explícita
  (`money.Money`). Nenhum valor monetário passa por `float32`/`float64`, nem no banco (colunas
  `bigint`), nem na API (inteiros em centavos), nem na interface (texto convertido com aritmética
  de string).
- Cálculos intermediários: `math/big.Rat` (racionais exatos). Preços unitários fracionários e
  quantidades são `NUMERIC` no banco e chegam à API como texto decimal (`unit_amount_decimal`, em
  centavos, até 10 casas).
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
  retoma no período corrente, se ainda não faturado.

## Prorrata

- Unidade: **dias civis** do período de cobrança, no fuso do tenant.
- Fração restante = dias da data local da alteração (inclusive) até o fim do período (exclusivo)
  ÷ dias do período. O dia da alteração conta inteiro para a nova condição.
- Exemplo: período 01/03 a 01/04 (31 dias), alteração em 15/03 às 15h → 17/31 restante.
- O resultado fica sempre entre 0 e 1 (propriedade verificada por fuzz test).

### Mudança imediata de plano ou quantidade

Somente com o período corrente já faturado (fora do trial). Credita-se a fração não usufruída do
componente fixo anterior e cobra-se a mesma fração do novo, cada valor arredondado uma única vez
(half-even). Exemplo: período de 30 dias, troca no dia 21 de R$ 100,00 para R$ 160,00 → fração
10/30; crédito R$ 33,33; cobrança R$ 53,33; diferença **R$ 20,00**.

| Diferença | Efeito |
|---|---|
| positiva | fatura de prorrata imediata (itens de crédito e cobrança proporcionais) |
| negativa | crédito do cliente (`customer_credit_entries`, tipo `proration`), consumido nas próximas faturas |
| zero | nenhum documento; somente histórico |

Cupom percentual vigente reduz a diferença na mesma proporção; cupom de valor fixo não é
prorrateado (continua valendo por fatura). Planos com consumo medido só mudam no próximo ciclo, para
que cada período de consumo seja precificado por uma única versão de preço.

### Mudança no próximo ciclo e reajustes

- Mudança agendada vale no início do período seguinte, com a versão de preço fixada no agendamento;
  o consumo do período anterior continua precificado pela versão anterior.
- Reajuste programado (percentual em pontos-base ou novo valor) vale no primeiro período iniciado
  na data efetiva ou depois, sobre o componente fixo vigente (valor fixo ou preço por unidade),
  arredondado a centavos. Ciclos já faturados nunca mudam. Mudança de versão de preço remove o
  reajuste; cancelamento da assinatura descarta mudanças e reajustes pendentes.

### Consumo recebido depois do fechamento

O período de consumo fecha quando o motor fatura o período seguinte. Evento posterior para um
período fechado é aceito como `late` até `late_grace_hours` (configuração da métrica) após o fim
do período; depois disso, é rejeitado. A correção (job `billing.correct_late_usage`, a cada 5
minutos) recalcula a quantidade com a mesma semântica de agregação, precifica pela versão usada no
fechamento e compara com o valor já faturado mais correções anteriores:

| Diferença | Efeito |
|---|---|
| positiva, assinatura com próximo ciclo | item de ajuste na próxima fatura |
| positiva, assinatura encerrada | fatura de ajuste imediata |
| negativa | crédito do cliente (tipo `usage_correction`) |

A fatura emitida não é alterada (gatilho `app_invoice_guard`).
