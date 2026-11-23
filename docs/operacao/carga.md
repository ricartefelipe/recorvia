# Teste de carga

Quanto o Recorvia aguenta num servidor com os limites do pacote de produção, onde está o primeiro
gargalo e com que limites começar a operação (ADR-088). O teste é manual: não roda na CI dos pull
requests, porque leva de 30 a 40 minutos e depende da máquina.

## Como repetir

```bash
make load-test
```

O alvo chama `deploy/carga/carga.sh`, que:

1. constrói as imagens da API, do worker, do migrate e do simulador com a etiqueta `carga` (as
   imagens `local` do ambiente de desenvolvimento não mudam);
2. sobe, no projeto `recorvia-carga` do Docker Compose (`compose.yaml` com
   `deploy/carga/carga.compose.yaml`), PostgreSQL, Redis, RabbitMQ, Mailpit, simulador de
   pagamentos, API e worker, com os limites de CPU e memória de `deploy/producao/compose.yaml`;
3. gera os dados sintéticos pelos serviços e pela API (`tools/carga preparar` e `popular`);
4. configura o simulador como provedor de pagamento de cada organização e executa os cenários;
5. junta os relatórios em `backups/carga/relatorio.md` (fora do Git), com as tabelas por cenário,
   a máquina, as consultas lentas e o uso dos containers, e derruba o projeto com os volumes.

O projeto do ambiente local (`recorvia`) nunca é usado nem derrubado; um projeto `recorvia-carga`
com containers ou volumes que o teste não criou (sem o rótulo `recorvia.load-test`) é recusado
antes de qualquer remoção. Nenhuma porta fixa é publicada: os serviços consultados pelo teste
publicam portas livres escolhidas pelo Docker, só em 127.0.0.1. Cada etapa tem prazo próprio
(`RECORVIA_LOAD_TIMEOUT`, padrão de 3600 s), e a geração de dados e os cenários mostram o
progresso a cada poucos segundos. Toda espera falha depois de `RECORVIA_LOAD_STALL` (padrão de
10 min) sem nenhum andamento, e cada consulta do observador e cada chamada HTTP têm prazo próprio.

A máquina não pode suspender durante o teste: a suspensão congela os containers e o próprio teste
sem erro nenhum, porque o relógio monotônico (e, com ele, todos os prazos) para junto. Com o
systemd-logind, o roteiro roda sob `systemd-inhibit` (suspensão, ociosidade e tampa;
`RECORVIA_LOAD_INHIBIT=0` desliga); sem ele, desative a suspensão automática. Em qualquer caso,
`tools/carga` compara o relógio de parede com o monotônico, e um cenário em que a máquina parou por
mais de 30 s falha em vez de gravar números enganosos.

Variáveis principais (todas opcionais; a lista completa está no cabeçalho do roteiro):

| Variável | Padrão | Uso |
|---|---|---|
| `RECORVIA_LOAD_ORGS` | 20 | organizações |
| `RECORVIA_LOAD_SCALE` | 1 | multiplicador de clientes por organização (1 = cerca de 6.600) |
| `RECORVIA_LOAD_SCENARIOS` | `faturamento,leitura,escrita,webhooks,consumo` | cenários, na ordem |
| `RECORVIA_LOAD_DURATION` | 45s | duração de cada nível de concorrência |
| `RECORVIA_LOAD_READ_LEVELS` | 8,32,64 | usuários simultâneos da leitura |
| `RECORVIA_LOAD_DIR` | `backups/carga` | resultados (a execução anterior vai para `anterior/`) |
| `RECORVIA_LOAD_BUILD` | 1 | 0 reaproveita as imagens `carga` |
| `RECORVIA_LOAD_KEEP` | 0 | 1 mantém o ambiente no ar para inspeção |
| `RECORVIA_LOAD_BILLING_CONCURRENCY` | 4 | tenants faturados ao mesmo tempo pelo motor |
| `RECORVIA_LOAD_WEBHOOK_RATE_LIMIT` | sem limite prático | 1200 mede com o limite de produção |
| `RECORVIA_LOAD_STALL` | 10m | tempo máximo sem andamento numa espera |
| `RECORVIA_LOAD_INHIBIT` | 1 | 0 não bloqueia a suspensão da máquina |

Exemplo de uma execução curta, só para conferir o roteiro (três organizações e 10% dos clientes):

```bash
RECORVIA_LOAD_ORGS=3 RECORVIA_LOAD_SCALE=0.1 RECORVIA_LOAD_DURATION=10s make load-test
```

Exige Docker com o plugin compose 2.24 ou superior, Go (para compilar `tools/carga`), `openssl` e
`timeout`. Imagens baixadas: só as do `compose.yaml` (PostgreSQL, Redis, RabbitMQ e Mailpit, já
fixadas por tag); a ferramenta de carga é um programa Go do próprio repositório (`tools/carga`),
fixado pelo `go.mod`.

## O que é gerado

Tudo passa pelas regras da aplicação, sem INSERT direto:

- **Organizações** (`preparar`): pelos serviços, como o seed: proprietário, organização (fuso de São
  Paulo), plano Escala da plataforma, catálogo com três planos mensais de preço fixo (R$ 99, R$ 299 e
  R$ 899) e um plano de consumo medido (R$ 199 com 1.000 chamadas incluídas e R$ 0,05 por chamada
  excedente) e uma credencial de API com os escopos de leitura e escrita.
- **Clientes e assinaturas** (`popular`): pela API, com credencial Bearer e Idempotency-Key. O tamanho
  das organizações segue um perfil B2B: 10% grandes (1.500 clientes), 30% médias (400) e 60%
  pequenas (100). Cada cliente (pessoa jurídica com CNPJ válido) tem uma assinatura que começou em
  até 150 dias atrás; 20% delas são de consumo medido.
- **Histórico**: o motor de recorrência emite as faturas retroativas (e o ledger registra a emissão),
  e 80% das faturas em aberto recebem pagamento manual pela API.
- **Provedor** (`provedor`): o simulador de pagamentos passa a ser o provedor padrão de cada
  organização, como no seed. As faturas emitidas a partir daí geram cobranças no simulador.

Com o padrão, são 20 organizações, 6.600 clientes, 6.600 assinaturas, 19.432 faturas e 15.604
pagamentos antes dos cenários.

Limite conhecido dos dados: o histórico é emitido no dia da execução, então todas as faturas e os
lançamentos do histórico caem no mês corrente. O painel do mês lê cerca de cinco meses de
movimento de uma vez; os números do painel são, por isso, pessimistas para uma base com a mesma
quantidade de assinaturas.

## Cenários

| Cenário | O que faz | Medidas |
|---|---|---|
| (c) faturamento | com o worker parado, cria uma assinatura nova (início hoje) para metade dos clientes, como as renovações do fechamento; religa o worker e espera a última fatura e a última cobrança no simulador; depois paga 70% das cobranças no simulador, que devolve os webhooks | tempo até a primeira e a última fatura, faturas por segundo, cobranças, pico da outbox, pagamentos confirmados por segundo |
| (a) leitura | listas de clientes, faturas e assinaturas (com cursor) e painel, com várias organizações e credencial Bearer, em 8, 32 e 64 usuários simultâneos | p50, p95, p99, vazão e erros por operação |
| (b) escrita | cliente e assinatura com Idempotency-Key; 20% repetem a criação com a mesma chave e o mesmo conteúdo, 10% com outro conteúdo (deve ser recusada) | idem, e repetições que devolveram outra assinatura |
| (e) webhooks | paga em silêncio no simulador as cobranças pendentes e envia à API, de uma vez, os webhooks assinados: o `charge.paid` três vezes e o `charge.created` antigo, embaralhados (parte fora de ordem) | vazão da recepção, tempo até o último processado, pagamentos confirmados e duplicados |
| (d) consumo | lotes de 100 eventos de consumo, com 5% de reenvios idênticos de eventos recentes | eventos por segundo, latência dos lotes, resultados |

Em todos: CPU e memória por container (`docker stats` a cada 2 s), espera por conexão no pool do
PostgreSQL da API e do worker, pico das filas do RabbitMQ, respostas da API e mensagens do worker
pelas métricas, e as consultas de maior tempo total (`pg_stat_statements`, zerado antes de cada
cenário). O PostgreSQL do ambiente de carga também registra as consultas acima de 200 ms
(`log_min_duration_statement`, em `consultas-lentas.txt`). Os dois recursos só existem neste
ambiente.

O subcomando `tools/carga motor` é um diagnóstico à parte: com o worker parado, executa o motor de
recorrência no próprio processo, sem os consumidores, e mostra assinaturas por segundo, as consultas
do motor e, opcionalmente, o perfil de CPU.

## Máquina e limitações dos números

Medido em 23/11/2026 numa estação de trabalho com 12 núcleos (Intel Core 7 250U), 30 GiB de
RAM (cerca de 12 GiB livres) e 36 GB livres no disco NVMe (93% ocupado), com Docker 29.8. Outra frente
de trabalho rodava exercícios Docker ao mesmo tempo (um cluster kind e um exercício de backup
contínuo do PostgreSQL com 13 containers): carga média de 19 a 22 e pressão de E/S de 4% a 10% do
tempo com todas as tarefas paradas esperando o disco. O efeito é visível: numa das execuções, os
`COMMIT` do PostgreSQL ficaram até 26 s parados ao mesmo tempo (E/S do disco compartilhado), e o
mesmo cenário levou 8 min numa execução e 3 min 18 s na outra. Os números são, portanto, um piso
conservador para um servidor dedicado com os mesmos limites; servem para comparar versões na mesma
máquina e para achar gargalos, não como capacidade garantida.

Os containers rodam com os limites do pacote de produção: PostgreSQL 2 CPUs e 2 GiB (configuração
padrão da imagem: `shared_buffers` de 128 MB), API e worker 1,5 CPU e 512 MiB, RabbitMQ 1 CPU e
1 GiB, Redis 0,5 CPU e 384 MiB; pool de 20 conexões na API e no worker; worker com concorrência 8
e prefetch 16 por fila.

## Resultados

Antes = código de `develop` (c8f3983); depois = com as correções desta frente (ADR-088). Todas as
execuções usaram o volume padrão (20 organizações, 6.600 clientes e assinaturas, 19.432 faturas e
15.604 pagamentos de histórico) e os mesmos cenários, durações e níveis. Como a máquina era
compartilhada, cada lado foi medido mais de uma vez, em horários diferentes, e as tabelas mostram
todas as execuções, na ordem abaixo (a carga média é a do sistema no início e no fim):

| Execução | Código | Horário (23/11) | Cenários | Carga média |
|---|---|---|---|---|
| A1 | antes | 13h07 às 13h37 | faturamento, leitura, escrita (webhooks interrompido) | 19 a 22 |
| A2 | antes | 13h46 às 14h09 | faturamento, webhooks, consumo | 19 a 22 |
| A3 | antes | 18h38 às 19h12 | todos | 17 a 20 |
| D1 | depois, sem a correção do painel | 14h15 às 14h32 | faturamento, leitura, escrita | não registrada |
| D2 | depois, sem a correção do painel | 14h33 às 14h49 | faturamento, webhooks, consumo | não registrada |
| D3 | depois | 18h13 às 18h35 | todos | 12 a 15 |
| D4 | depois | 19h17 às 19h38 | todos | 15 a 17 |

A variação entre execuções do mesmo código chega a 2 vezes (leitura com 32 usuários: 93 e 156
requisições por segundo em D3 e D4). Por isso as conclusões abaixo se apoiam no que mudou de forma
estrutural (erros, espera fixa, consultas por requisição, tempo de uma consulta) e só citam vazão
quando todas as execuções de depois ficam acima de todas as de antes. Uma execução anterior de
depois (das 14h52) ficou quase três horas parada porque a máquina suspendeu no meio da geração de
dados; foi descartada, e o roteiro passou a impedir a suspensão (ver "Como repetir").

### Gargalos encontrados

1. **Fechamento (motor de recorrência).** O motor processava um tenant por vez, uma transação por
   assinatura, e parava em 500 assinaturas por tenant por execução; a execução seguinte só vinha
   depois do intervalo da tarefa (1 min). O diagnóstico `motor` mostrou o motor parado quase todo
   o tempo esperando o banco (3% de CPU no perfil) e as consultas do motor somando menos de 10% do
   tempo: o custo é a latência de cerca de 30 idas ao banco e um `COMMIT` por assinatura, não uma
   consulta lenta. Na prática, as organizações grandes terminavam um minuto depois das outras.
2. **Retorno dos webhooks do provedor.** O limite de 1.200 webhooks por minuto por IP de origem (20
   por segundo) recusa com 429 boa parte de um pico: no cenário (e), 70% das entregas; no
   fechamento, a confirmação dos pagamentos ficou em 20 a 26 por segundo, o ritmo do limite, e o
   resto esperou as novas tentativas do simulador. O provedor real envia de poucos IPs.
3. **Leitura: o painel satura o PostgreSQL.** Com 32 usuários, o PostgreSQL fica no limite de 2 CPUs
   e três consultas do painel somam 80% do tempo do banco; a vazão cai com mais usuários (108, 89 e
   70 requisições por segundo em 8, 32 e 64). Parte do custo vem dos dados (o mês corrente tem
   cinco meses de histórico) e parte do plano genérico da consulta de reembolsos e chargebacks, que
   percorre todos os lançamentos do mês em vez de partir das duas contas.
4. **Lista de assinaturas com N+1.** A lista já lia o nome do cliente e do plano, mas a API
   descartava e lia de novo, uma transação por item: 26 idas ao pool por página. Com o pool cheio,
   cada ida esperava (98,6% das aquisições esperaram conexão, 69 ms em média com 64 usuários) e a
   lista chegou a 2,4 s na mediana.
5. **Consumo medido: deadlock.** Dois lotes simultâneos com `external_id` em comum (a nova tentativa
   de um cliente enquanto o primeiro envio ainda é processado) gravavam os eventos em ordens opostas
   e se bloqueavam na unicidade do `external_id`: 1% dos lotes respondeu 500 (14 deadlocks
   registrados pelo PostgreSQL).
6. **Criação de assinatura.** A checagem do limite de contratos bloqueia a linha do plano da
   plataforma (ADR-077) e, com ela bloqueada, contava todas as assinaturas ativas do tenant, mesmo
   com o plano sem limite: 30 ms de espera média pelo bloqueio e 6 ms de contagem por criação com 16
   clientes simultâneos.
7. **Pool do worker.** Cada uma das nove filas tem a própria concorrência (8): até 72 tratamentos
   simultâneos, mais o motor, o publicador da outbox e as conciliações, para 20 conexões. Durante o
   fechamento, 15% das aquisições esperaram conexão (16,5 ms em média) e, na volta dos pagamentos,
   46% (34 ms).

### Correções e efeito medido

Faixas com todas as execuções de cada lado (A1 a A3 e D1 a D4, quando o cenário foi medido nelas).

| Gargalo | Correção | Antes | Depois |
|---|---|---|---|
| 1. Fechamento | até 4 tenants ao mesmo tempo; tenant no limite de 500 volta na hora | primeira fatura em 20 a 88 s; 3.287 faturas em 3m18s a 8m3s (6,8 a 16,6 por segundo); histórico de 19.432 faturas em 7m43s a 11m35s | primeira fatura em 6 s nas quatro; 3.287 faturas em 1m23s a 2m30s (21,9 a 39,8 por segundo); histórico em 3m47s a 4m43s |
| 2. Webhooks do provedor | limite por IP configurável (`RECORVIA_WEBHOOK_RATE_LIMIT_PER_MINUTE`; o padrão continua 1200) | com 1200 por minuto: 39% a 70% das 3.948 entregas do cenário (e) recusadas com 429; 641 a 935 de 987 pagamentos confirmados; no fechamento, 4.195 a 6.595 respostas 429 e 16,4 a 25,6 confirmações por segundo | sem limite prático (ambiente de carga): nenhum 429; 987 de 987 confirmados, sem pagamento duplicado; no fechamento, 31,3 a 51,9 confirmações por segundo |
| 3. Painel | consulta de reembolsos e chargebacks parte das duas contas | essa consulta: 182 a 235 ms em média com 32 usuários, 35% do tempo do banco (A3); painel com 32 usuários: p50 501 a 826 ms, p95 1.395 a 1.694 ms | essa consulta: 19 ms em média (D3), fora das cinco maiores em D4; painel com 32 usuários: p50 423 a 721 ms, p95 787 a 1.282 ms; o PostgreSQL continua no limite de 2 CPUs |
| 4. Lista de assinaturas | nomes lidos na própria consulta (sem N+1) | 11.375 a 24.750 consultas de nome por nível de 45 s; com 64 usuários, p50 de 2,4 a 3,6 s | nenhuma consulta de nome por item; com 64 usuários, p50 de 305 a 504 ms (D1, D3 e D4) |
| 5. Consumo medido | eventos gravados em ordem de `external_id` | 3 a 7 lotes com 500 (0,6% a 1,1% de 535 e 663 lotes); 3 a 14 deadlocks no log do PostgreSQL | nenhum 500 e nenhum deadlock em D2, D3 e D4 (517 a 1.023 lotes por execução); os lotes com `external_id` em comum esperam um pelo outro (34 a 53 esperas de cerca de 1 s registradas) |
| 6. Criação de assinatura | sem contagem quando o plano da plataforma não tem limite | criar assinatura com 16 clientes: p50 194 a 282 ms, p95 386 a 504 ms | p50 186 a 214 ms, p95 337 a 395 ms; dentro da variação da máquina: o bloqueio do plano continua serializando a criação por tenant |
| 7. Pool do worker | nenhuma (descrito) | espera por conexão em 2% a 15% das aquisições na emissão e 41% a 47% nos pagamentos | 44% a 52% na emissão (o motor agora usa até 4 conexões ao mesmo tempo) e 44% a 61% nos pagamentos, de 22 a 40 ms em média |

### Números por cenário

Latências em milissegundos; vazão em requisições por segundo (eventos por segundo no consumo);
erros = respostas 5xx ou falhas de transporte, além das 409 esperadas na escrita e das 429 do
limitador quando indicadas. A3 é o antes mais recente; D3 e D4, os depois medidos na mesma tarde.

**(c) Fechamento** (3.287 assinaturas novas; 2.300 cobranças pagas no simulador):

| Medida | A1 | A2 | A3 | D1 | D2 | D3 | D4 |
|---|---:|---:|---:|---:|---:|---:|---:|
| Primeira fatura (s) | 88 | 20 | 29 | 6 | 6 | 6 | 6 |
| Última fatura (s) | 483 | 198 | 261 | 113 | 115 | 150 | 83 |
| Faturas por segundo (do religamento à última) | 6,8 | 16,6 | 12,6 | 29,2 | 28,6 | 21,9 | 39,8 |
| Pico em 10 s | 30,2 | 37,0 | 21,8 | 96,1 | 81,8 | 68,5 | 126,6 |
| Pico da outbox pendente | 93 | 1.187 | 2.387 | 1.708 | 2.699 | 2.742 | 1.491 |
| Pagamentos confirmados por segundo | 25,6 | 19,5 | 16,4 | 41,0 | 42,0 | 31,3 | 51,9 |
| Respostas 429 aos webhooks | 4.308 | 6.595 | 4.195 | 0 | 0 | 0 | 0 |
| Dead letters | 0 | 0 | 0 | 0 | 0 | 0 | 0 |

**(a) Leitura** (todas as operações juntas; nenhum erro em nenhuma execução):

| Usuários | Execução | Vazão | p50 | p95 | p99 | Painel p50 / p95 | Lista de assinaturas p50 / p95 |
|---:|---|---:|---:|---:|---:|---:|---:|
| 8 | A1 | 107,7 | 27 | 325 | 491 | 190 / 501 | 101 / 191 |
| 8 | A3 | 55,6 | 77 | 522 | 776 | 301 / 798 | 268 / 396 |
| 8 | D3 | 95,6 | 65 | 329 | 490 | 228 / 500 | 51 / 94 |
| 8 | D4 | 165,0 | 17 | 213 | 300 | 129 / 306 | 15 / 76 |
| 32 | A1 | 88,7 | 180 | 1.102 | 2.312 | 501 / 1.395 | 700 / 2.073 |
| 32 | A3 | 50,1 | 304 | 1.820 | 2.088 | 826 / 1.694 | 1.672 / 2.024 |
| 32 | D3 | 93,2 | 286 | 898 | 1.215 | 721 / 1.282 | 214 / 390 |
| 32 | D4 | 156,1 | 181 | 517 | 726 | 423 / 787 | 120 / 216 |
| 64 | A1 | 69,9 | 408 | 2.992 | 3.969 | 980 / 1.865 | 2.410 / 3.974 |
| 64 | A3 | 50,2 | 609 | 3.930 | 4.738 | 1.206 / 2.291 | 3.616 / 4.913 |
| 64 | D3 | 102,3 | 581 | 1.205 | 1.500 | 1.022 / 1.517 | 496 / 696 |
| 64 | D4 | 161,6 | 374 | 721 | 978 | 680 / 997 | 305 / 474 |

D1 (sem a correção do painel) mediu 92,5, 90,7 e 90,9 requisições por segundo com 8, 32 e 64
usuários e a lista de assinaturas já sem N+1 (p50 de 504 ms com 64). Em todas as execuções o
PostgreSQL ficou entre 209% e 234% de CPU em média em cada nível (o limite é 200%; as amostras do
`docker stats` passam dele por instantes) e a API esperou conexão em mais de 97% das aquisições a
partir de 32 usuários: o banco, não a API, é o limite.

**(b) Escrita** (cliente e assinatura com Idempotency-Key; nenhum 5xx; nenhuma repetição devolveu
outra assinatura; as 409 são as recusas esperadas da mesma chave com outro conteúdo):

| Clientes | Execução | Vazão | p50 | p95 | p99 | Criar assinatura p50 / p95 | 409 esperadas |
|---:|---|---:|---:|---:|---:|---:|---:|
| 4 | A1 | 93,8 | 31 | 108 | 209 | 58 / 144 | 160 |
| 4 | A3 | 122,3 | 26 | 86 | 121 | 38 / 102 | 253 |
| 4 | D1 | 105,2 | 26 | 110 | 188 | 39 / 173 | 208 |
| 4 | D3 | 111,9 | 30 | 68 | 86 | 50 / 78 | 197 |
| 4 | D4 | 195,1 | 17 | 54 | 85 | 24 / 70 | 357 |
| 16 | A1 | 123,5 | 98 | 314 | 429 | 194 / 386 | 252 |
| 16 | A3 | 87,0 | 137 | 415 | 579 | 282 / 504 | 179 |
| 16 | D1 | 104,8 | 111 | 303 | 389 | 214 / 337 | 222 |
| 16 | D3 | 119,5 | 107 | 306 | 445 | 186 / 395 | 237 |
| 16 | D4 | 112,4 | 109 | 323 | 464 | 203 / 391 | 215 |

**(e) Webhooks** (987 cobranças; 3.948 entregas: cada `charge.paid` três vezes e o `charge.created`
atrasado, 723 cobranças com entrega fora de ordem). Antes, com o limite fixo de 1200 por minuto;
depois, sem limite prático:

| Execução | Vazão da recepção | p50 | p95 | p99 | 429 | Recepção (s) | Até o último processado (s) | Pagamentos confirmados | Duplicados |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| A2 | 729,5 | 15 | 170 | 195 | 2.748 | 5,4 | 19,4 | 641 | 0 |
| A3 | 358,9 | 95 | 197 | 242 | 1.548 | 11,0 | 37,2 | 935 | 0 |
| D2 | 296,4 | 101 | 193 | 212 | 0 | 13,3 | 31,6 | 987 | 0 |
| D3 | 226,3 | 122 | 251 | 301 | 0 | 17,4 | 43,5 | 987 | 0 |
| D4 | 417,1 | 84 | 115 | 175 | 0 | 9,5 | 22,7 | 987 | 0 |

A vazão de antes é maior porque 39% a 70% das respostas eram 429, que não chegam ao banco.

**(d) Consumo** (lotes de 100 eventos, 5% de reenvios idênticos):

| Clientes | Execução | Lotes | Eventos por segundo | p50 | p95 | p99 | 500 |
|---:|---|---:|---:|---:|---:|---:|---:|
| 4 | A2 | 298 | 650 | 540 | 1.030 | 2.425 | 3 |
| 4 | A3 | 200 | 438 | 864 | 1.500 | 1.771 | 1 |
| 4 | D2 | 347 | 765 | 506 | 761 | 992 | 0 |
| 4 | D3 | 230 | 505 | 743 | 1.270 | 1.506 | 0 |
| 4 | D4 | 518 | 1.144 | 320 | 583 | 775 | 0 |
| 16 | A2 | 365 | 786 | 1.899 | 3.008 | 4.392 | 4 |
| 16 | A3 | 335 | 721 | 2.083 | 3.590 | 5.053 | 2 |
| 16 | D2 | 401 | 869 | 1.639 | 3.231 | 4.906 | 0 |
| 16 | D3 | 287 | 613 | 2.213 | 5.113 | 7.362 | 0 |
| 16 | D4 | 505 | 1.099 | 1.213 | 2.770 | 5.437 | 0 |

Com 16 clientes, a latência triplica e a vazão quase não muda: o PostgreSQL já estava no limite com 4.

## Limites recomendados para começar

Valores para começar num servidor com os limites do pacote de produção, tirados das medições
acima (com a máquina compartilhada, devem ser tratados como piso). Os ajustes marcados como "não
medido" são recomendações a confirmar com uma nova execução, não resultados.

- **Motor de recorrência:** `RECORVIA_BILLING_CONCURRENCY=4` (padrão). Medido: 3.287 faturas em 83 a
  150 s, de 22 a 40 por segundo do religamento do worker à última fatura. Um fechamento com todas as
  assinaturas no mesmo dia cabe em poucos minutos até algumas dezenas de milhares de assinaturas;
  acima disso, medir de novo antes de subir a concorrência, porque o pool do worker já espera.
- **Pool:** manter 20 conexões na API e 20 no worker. A espera por conexão é sintoma do PostgreSQL no
  limite de CPU, não falta de conexões; o primeiro passo de capacidade é dar mais CPU ao PostgreSQL
  (não medido).
- **Leitura pela API:** até 32 usuários ou integrações simultâneos por servidor: p95 de 0,5 a 0,9 s
  no total e de 0,8 a 1,3 s no painel. Com 64, a vazão não sobe (93 a 165 requisições por segundo
  em qualquer nível) e o p95 vai a 0,7 a 1,2 s. O limite de 600 requisições por minuto por credencial
  de API continua adequado.
- **Escrita:** até 4 clientes criando ao mesmo tempo por organização: p95 de 54 a 68 ms com 112 a 195
  requisições por segundo (D3 e D4). Com 16, a vazão fica igual (112 a 120) e o p95 sobe para 306 a
  323 ms.
- **Consumo medido:** orientar os clientes a mandar lotes de até 100 eventos com no máximo 4 envios
  simultâneos por organização; medido: 505 a 1.144 eventos por segundo, p95 de 0,6 a 1,3 s. Com 16
  envios, o p95 vai a 2,8 a 5,1 s sem ganho de vazão.
- **Webhooks de provedores:** manter o padrão de 1200 por minuto por IP enquanto o provedor enviar de
  muitos endereços. Com um provedor que envia de poucos IPs (como o Asaas em produção), subir para
  6000 por minuto (100 por segundo): fica abaixo da recepção medida sem limite (226 a 417 por
  segundo) e acima do ritmo de confirmação do fechamento (31 a 52 por segundo); não medido com esse
  valor exato.

## O que fica para depois

Gargalos medidos que não foram corrigidos nesta versão, por ordem de efeito:

1. **PostgreSQL no limite de CPU na leitura.** Com o painel corrigido, as consultas de faturamento
   do mês (101 a 149 ms em média) e de posições das assinaturas (77 a 120 ms) somam de 67% a 71% do
   tempo do banco com 32 usuários (D3 e D4). Caminhos: agregados mensais mantidos pelo próprio faturamento ou cache do
   painel por organização e mês, com validade curta; e rever `shared_buffers` (128 MB, o padrão da
   imagem) junto com o pacote de produção.
2. **Pool do worker.** Nove filas com concorrência 8 (até 72 tratamentos) mais o motor, a outbox e as
   conciliações disputam 20 conexões: de 44% a 61% das aquisições esperaram no fechamento depois da
   correção do motor. Caminhos: um limite global de tratamentos simultâneos no worker, menor que o
   pool, ou um pool maior junto com mais CPU no PostgreSQL. Precisa de medição própria.
3. **Criação de assinatura serializada por organização.** O bloqueio da linha do plano da plataforma
   (ADR-077) continua em toda criação; a correção só tirou a contagem quando não há limite. Com 16
   clientes simultâneos, criar assinatura fica entre 186 e 214 ms na mediana. Trocar por um bloqueio
   só quando o plano tem limite exige rever a corrida com a troca de plano.
4. **Limite de webhooks por IP.** Com um provedor que envia de poucos endereços, o limite por IP mede o
   provedor, não o abuso. Caminho: limite por endpoint (o token da URL) ou lista dos IPs do provedor.
5. **Dados do teste.** O histórico sintético cai todo no mês corrente, o que deixa o painel mais caro
   que numa base real; gerar o histórico com datas reais exigiria emitir as faturas pelo motor com o
   relógio no passado.
6. **Máquina dedicada.** Repetir o teste num servidor sem outras cargas, com o volume padrão e com o
   dobro (`RECORVIA_LOAD_SCALE=2`), antes de abrir para clientes maiores.
