# Roteiro demonstrativo

Ambiente: `make up` a partir de um checkout limpo (ver README). Dados de **demonstração**
criados pelo seed (executado por `make up`; repetir com `docker compose --profile seed run --rm seed`):

| Usuário (Keycloak local) | Senha | Organização | Papel |
|---|---|---|---|
| `ana.owner@acme.test` | `Recorvia#Demo2026` | Acme Software (demonstração) | proprietária |
| `bruno.finance@acme.test` | `Recorvia#Demo2026` | Acme Software (demonstração) | financeiro |
| `davi.auditor@acme.test` | `Recorvia#Demo2026` | Acme Software (demonstração) | auditor |
| `carla.owner@beta.test` | `Recorvia#Demo2026` | Beta Consultoria (demonstração) | proprietária |
| `nova.empresa@exemplo.test` | `Recorvia#Demo2026` | sem organização (onboarding) | — |

Endereços: aplicação em http://localhost:8080, simulador de pagamentos em http://localhost:8090,
Mailpit em http://localhost:8025 e RabbitMQ em http://localhost:15672.

## Passos

1. **Entrar no tenant A**: em http://localhost:8080, clique em *Entrar* e use `ana.owner@acme.test`.
   Com uma única organização, ela já fica ativa.
2. **Cadastrar cliente e plano**: em *Clientes → Novo*, crie uma pessoa jurídica (por exemplo, CNPJ
   `11.222.333/0001-81`). Em *Catálogo*, crie um produto e um plano mensal de valor fixo
   (R$ 990,00).
3. **Criar assinatura**: em *Assinaturas → Nova*, escolha o cliente e o plano (sem trial).
4. **Gerar a fatura pelo motor de recorrência**: em *Faturas*, use *Executar faturamento agora*
   (`POST /api/v1/billing-runs`). O motor também roda a cada minuto no worker. A fatura
   `ACME-AAAA-000001` aparece com itens e snapshot de preço.
5. **Criar a cobrança no simulador**: o worker consome `invoice.issued` e cria a cobrança no
   simulador; o detalhe da fatura mostra a cobrança `awaiting_payment` e o link do checkout.
6. **Confirmar o pagamento por webhook**: abra o link do checkout e clique em *Confirmar
   pagamento*. O simulador envia o webhook assinado (HMAC com timestamp); a API persiste e
   responde 200; o worker aplica o pagamento.
7. **Observar**: a fatura fica *paga*; em *Pagamentos*, o recebimento com taxa do provedor; em
   *Ledger*, receita faturada, valores em liquidação e taxas (contas a receber zerada); em
   *Auditoria*, a sequência `invoice.issued → charge.requested → payment_operation.succeeded →
   payment.applied`; no *Dashboard*, MRR, faturado e recebido do mês.
8. **Repetir o evento**: reenvie os webhooks da cobrança:
   ```bash
   curl -X POST http://localhost:8090/sim/charges/<id_no_simulador>/actions -d '{"action":"replay"}'
   ```
   A recepção identifica os eventos como duplicados (`duplicate: true`); não há segundo pagamento
   nem novo lançamento no ledger.
9. **Reembolso, divergência e conciliação** (roteiro automatizado em `web/e2e/04-conciliacao.spec.ts`):
   1. *Reembolso parcial*: abra a cobrança paga (link `#1` na tabela *Cobranças* da fatura) e use
      *Reembolsar*. O valor sugerido é o reembolsável; informe `50,00`, o motivo e confirme. A resposta
      (`POST /api/v1/charges/{id}/refunds` com `Idempotency-Key`) mostra *Reembolso concluído*; a cobrança
      fica *Parcialmente reembolsada*, o reembolsável cai para o saldo restante e a fatura continua paga,
      com a devolução no histórico. Em *Reembolsos* aparece o pedido com o identificador do provedor; no
      *Ledger*, o lançamento `refund.succeeded` (débito em reembolsos, crédito em valores em liquidação).
      Valor acima do reembolsável é recusado no formulário e pela API (409 `refund_exceeds_balance`).
   2. *Divergência no provedor*: altere a taxa da cobrança no simulador sem notificar o Recorvia. O
      identificador no simulador aparece no detalhe da cobrança (*Identificador no provedor*) e no
      endereço do checkout:
      ```bash
      curl -X POST http://localhost:8090/sim/charges/<id_no_simulador>/actions -d '{"action":"diverge","fee":999}'
      ```
   3. *Conciliação*: em *Conciliação → Execuções*, use *Executar conciliação agora*
      (`POST /api/v1/reconciliation/runs`). O pedido é aceito de imediato (`202`, execução em
      andamento) e o worker executa em segundo plano; a tela acompanha a execução e avisa quando ela
      conclui, com as cobranças verificadas e uma divergência. O job `payments.reconcile` concilia a
      cada hora os planos com conciliação automática.
   4. *Análise*: na aba *Divergências*, a divergência *Taxa divergente* da cobrança está *Aberta*.
      *Analisar* mostra o comparativo local × provedor, com a taxa destacada (local: taxa original;
      provedor: R$ 9,99) e o histórico com `reconciliation.discrepancy_detected`.
   5. *Resolução*: escolha *Ajustar o ledger com lançamento compensatório* (ou *Aceitar o valor do
      provedor*, que também atualiza a taxa da cobrança), informe a justificativa (mínimo de 10
      caracteres) e confirme. A divergência fica *Resolvida* com responsável, data e justificativa; o
      histórico registra `reconciliation.discrepancy_resolved`; no *Ledger*, o lançamento
      `reconciliation.adjustment` (referência *Divergência de conciliação*) lança a diferença de taxa e as
      contas continuam balanceadas.
   6. *Nova execução*: executar a conciliação de novo não reabre a divergência resolvida (mesmos valores).
      Uma taxa diferente gera nova divergência; quando o provedor volta ao valor do Recorvia, a divergência
      aberta é resolvida automaticamente na execução seguinte.
   7. *Chargeback (opcional)*: em outra cobrança paga, use `{"action":"chargeback","stage":"requested"}` e
      depois `"won"` ou `"lost"`. Em *Disputas* a disputa aparece aberta (valor retido no ledger) e depois
      ganha (retenção estornada) ou perdida (cobrança *Chargeback*); a cobrança em disputa não aceita
      reembolso.
10. **Isolamento**: saia e entre como `carla.owner@beta.test`. A lista de clientes não contém os
    dados da Acme; abrir a URL de uma fatura da Acme mostra *não encontrado* (404); enviar
    `X-Recorvia-Tenant` da Acme resulta em 409; trocar para a Acme é negado (403).

## Cenários adicionais do simulador

Agende cenários para a próxima cobrança do comerciante (a chave de API do seed é
`sim_acme_demonstracao_0001`):

```bash
curl -X POST http://localhost:8090/sim/scenarios -d '{"api_key":"sim_acme_demonstracao_0001","scenario":"timeout_after_success"}'
```

| Cenário | Efeito esperado no Recorvia |
|---|---|
| `decline` | cobrança `failed`; nova tentativa manual cria a tentativa 2 |
| `unavailable` | operação `pending` com backoff (repetição segura) |
| `timeout_after_success` | operação `unknown`; a recuperação consulta pela referência e vincula sem duplicar |
| `error_after_success` | operação `unknown`; resolvida pela consulta |
| `slow` | resposta atrasada dentro do timeout |
| `duplicate_webhooks` | cada evento chega duas vezes; deduplicado |
| `out_of_order` | `charge.paid` chega antes de `charge.created`; sem regressão |
| `refund_unavailable` | próximo reembolso recusado sem processar (503): operação `pending` com backoff |
| `refund_error_after_success` | próximo reembolso efetivado com resposta perdida: reembolso `unknown`, resolvido pela consulta sem repetir |
| `cancel_unavailable` | próximo cancelamento no provedor indisponível: operação `cancel_charge` aguarda nova tentativa |

Ações sobre uma cobrança: `pay`, `pay_silently` (sem webhook), `fail`, `refund`, `chargeback`
(`stage`: `requested`, `won`, `lost`), `diverge` e `replay`. Uma cobrança recusada (`fail`) ainda
aceita `pay`, como o pagamento por outro meio depois da recusa do cartão; no Recorvia, a cobrança
recusada passa a paga e o valor é aplicado à fatura. `refund` feito pelo painel do simulador aparece no
Recorvia como reembolso registrado pelo provedor; `chargeback` abre, analisa e decide a disputa;
`diverge` e `pay_silently` produzem divergências na conciliação. `DELETE /v1/charges/{id}` (com a chave
de API) cancela a cobrança pendente, como faz o Recorvia ao cancelar uma cobrança localmente.
