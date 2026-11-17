# Roteiro demonstrativo

Ambiente: `make up` a partir de um checkout limpo (ver README). Dados de **demonstração**
criados pelo seed (`docker compose run --rm seed`, executado também por `make up`):

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
9. **Divergência e conciliação**: altere o valor no provedor sem notificar
   (`{"action":"diverge","fee":999}`) e execute a conciliação. A divergência de taxa aparece em
   *Conciliação* e é resolvida com justificativa (ver `docs/STATUS.md` para a situação desta etapa).
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

Ações sobre uma cobrança: `pay`, `pay_silently` (sem webhook), `fail`, `refund`, `chargeback`
(`stage`: `requested`, `won`, `lost`), `diverge` e `replay`.
