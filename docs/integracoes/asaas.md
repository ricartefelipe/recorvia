# Asaas: validação no sandbox

Guia para exercitar o adaptador do Asaas (`internal/payments/gateways/asaas`) contra o sandbox
oficial (`https://api-sandbox.asaas.com/v3`). O adaptador foi implementado conforme a documentação
pública da API v3 e verificado por testes de contrato com servidor HTTP local; o comportamento real
do provedor (credenciais, entrega de webhooks, prazos e situações de reembolso) só é confirmado no
sandbox. Decisões relacionadas: ADR-008, ADR-013 e ADR-034 a ADR-038 em `docs/DECISOES.md`.

## O que já está validado e o que depende do sandbox

| Comportamento | Testes de contrato (servidor local, `go test ./internal/payments/gateways/asaas/`) | Depende do sandbox |
|---|---|---|
| Autenticação das chamadas | header `access_token` e `User-Agent` em todas as requisições | chave aceita; mensagens de erro de chave inválida ou de outro ambiente |
| Cliente | `GET /customers?externalReference=` e `POST /customers` com `name`, `cpfCnpj`, `email` e `externalReference`; recusa 400 tratada como permanente | validação real de CPF/CNPJ e de e-mail |
| Cobrança | `POST /payments` com `billingType=UNDEFINED`, valor decimal em reais, `dueDate`, `description` e `externalReference`; leitura de `invoiceUrl`, `netValue` e situação | fatura hospedada (`invoiceUrl`) e meios de pagamento disponíveis no sandbox |
| Resultado desconhecido | consulta por `GET /payments?externalReference=` antes de repetir (ADR-008) | latência e consistência da listagem logo após a criação |
| Webhook | `POST /webhooks` com `url`, `email`, `authToken`, `apiVersion=3`, `sendType=SEQUENTIALLY` e a lista de eventos assinados; autenticação por `asaas-access-token` em tempo constante; leitura de `id`, `event`, `dateCreated` e `payment` | aceitação do cadastro (regras do `authToken`, e-mail), entrega efetiva, reenvio, ordem e unicidade do `id` do evento |
| Reembolso | `POST /payments/{id}/refund` com `value` e `description`; resposta como cobrança com a lista `refunds` (`PENDING`, `AWAITING_*`, `CANCELLED`, `DONE`); classificação de erros 400/404/429/5xx | quais meios aceitam reembolso parcial, prazo até `DONE`, eventos `PAYMENT_REFUND_IN_PROGRESS`, `PAYMENT_PARTIALLY_REFUNDED` e `PAYMENT_REFUNDED`, valor de `value` após o reembolso |
| Cancelamento | `DELETE /payments/{id}` com resposta `{deleted, id}`; `deleted` diferente de `true` é ambíguo; 404 equivale a cancelada; 400 é recusa | erro devolvido para cobrança já recebida; evento `PAYMENT_DELETED` |
| Conciliação | `GET /payments` com `dateCreated[ge]`, `dateCreated[le]`, `offset`, `limit` (até 100) e `hasMore`; taxa derivada de `value - netValue`; cobranças fora da janela consultadas por rodízio com `GET /payments/{id}` | fuso das datas de criação nos filtros, `netValue` antes e depois da liquidação |
| Chargeback | mapeamento de `CHARGEBACK_REQUESTED`, `CHARGEBACK_DISPUTE` e `AWAITING_CHARGEBACK_REVERSAL` e dos eventos correspondentes | o sandbox pode não oferecer simulação de chargeback; a decisão perdida não é descrita pela documentação (ADR-036) |

Os fluxos de reembolso, disputa, cancelamento e conciliação foram validados de ponta a ponta com o
simulador local (`cmd/paysim`), que reproduz as mesmas situações com dados determinísticos
(`internal/pgstore/receivables_*_integration_test.go`, `internal/httpapi/receivables_integration_test.go`).
A associação dos reembolsos do Asaas por valor (ADR-035), que não recebem identificador próprio,
é coberta por testes de unidade do domínio (`internal/payments/receivables_domain_test.go` e
`internal/payments/refunds_service_test.go`).

## Pré-requisitos

1. **Conta no sandbox do Asaas** e a **chave de API do sandbox**, gerada no painel do sandbox. A
   chave do sandbox não funciona no ambiente de produção, e vice-versa.
2. **Plano da plataforma com o recurso `asaas_integration`** (Profissional ou Escala). A
   conciliação agendada exige `reconciliation_automation` (Escala); a manual está disponível em
   todos os planos.
3. **URL pública HTTPS** que encaminhe para o Recorvia. O Asaas envia os webhooks para
   `<RECORVIA_WEBHOOK_PUBLIC_URL>/asaas/<token>`; no ambiente local, a interface (porta `8080`)
   encaminha `/webhooks/` para a API. Um túnel HTTPS para `http://localhost:8080` atende (por
   exemplo, `cloudflared tunnel --url http://localhost:8080` ou `ngrok http 8080`).
4. **E-mail de notificação válido**: o cadastro do webhook informa ao Asaas o endereço de
   `RECORVIA_SMTP_FROM`, que recebe avisos de falha de entrega. O valor padrão do ambiente local
   (`cobranca@recorvia.local`) pode ser recusado; use um endereço real da equipe.
5. **Token do webhook** (`webhook_secret`): de 32 a 255 caracteres, sem espaços e diferente da chave
   de API. Gere com `openssl rand -hex 24`.

## Configuração

1. Crie `compose.override.yaml` na raiz do repositório (não versionado; o Docker Compose o aplica
   automaticamente) com a URL pública e o e-mail para a API e o worker:
   ```yaml
   services:
     api:
       environment:
         RECORVIA_WEBHOOK_PUBLIC_URL: https://<endereco-do-tunel>/webhooks/providers
         RECORVIA_SMTP_FROM: <email-da-equipe>
     worker:
       environment:
         RECORVIA_WEBHOOK_PUBLIC_URL: https://<endereco-do-tunel>/webhooks/providers
         RECORVIA_SMTP_FROM: <email-da-equipe>
   ```
   Suba o ambiente (`make up`) e confirme que `https://<endereco-do-tunel>/healthz` responde.
2. Entre com um usuário proprietário (por exemplo, `ana.owner@acme.test`) e, se necessário, mude o
   plano da organização em *Plano Recorvia* para Profissional ou Escala.
3. Em *Integrações → Provedores de pagamento → Asaas → Configurar*, informe o ambiente `sandbox`, a
   chave de API e o token do webhook e marque como provedor padrão. Pela API, com a versão atual da
   conta no `If-Match` (`version` em `GET /api/v1/payment-providers`, ou `"0"` se o Asaas ainda não
   foi configurado):
   ```bash
   curl -X PUT http://localhost:8080/api/v1/payment-providers/asaas \
     -H 'Content-Type: application/json' -H 'X-Recorvia-Tenant: <tenant_id>' -H 'X-CSRF-Token: <csrf>' \
     -H 'If-Match: "0"' \
     -b 'recorvia_session=<sessao>' \
     -d '{"environment":"sandbox","api_key":"<chave_do_sandbox>","webhook_secret":"<token>","make_default":true}'
   ```
   O Recorvia valida as credenciais cadastrando o webhook (`POST /v3/webhooks`) antes de gravar a
   conta; recusa do provedor resulta em 422 `provider_rejected_configuration`. A resposta exibe uma
   única vez `webhook_endpoint_url`, que contém o identificador opaco da conta.
4. No painel do sandbox, confira o webhook *Recorvia*: URL igual a `webhook_endpoint_url`, envio
   sequencial, ativo, com os eventos `PAYMENT_*` assinados pelo adaptador (lista em `asaas.Events`).
   Reconfigurar a conta gera nova URL e cadastra um novo webhook; desative o anterior no painel.

## Cenários

Registre, para cada cenário, a data, a versão do Recorvia (commit), os identificadores `pay_...` e
`evt_...` envolvidos, o resultado esperado e o observado.

### 1. Cobrança e pagamento

1. Crie cliente (com CPF ou CNPJ válido), plano e assinatura e execute o faturamento
   (*Faturas → Executar faturamento agora*).
2. Esperado no Recorvia: a cobrança `#1` fica *Aguardando pagamento* com *Abrir checkout* apontando
   para `invoiceUrl` e *Identificador no provedor* `pay_...`.
3. Esperado no Asaas: cliente com `externalReference` igual ao ID do cliente no Recorvia e cobrança
   `UNDEFINED` com `externalReference` igual ao ID da cobrança local.
4. Pague pela fatura hospedada com um meio de teste do sandbox ou confirme o recebimento em
   dinheiro no painel. Esperado: webhook recebido (`provider_webhook_events`), cobrança *Paga*,
   fatura *Paga*, lançamentos `payment.confirmed` e de taxa no ledger, taxa igual a
   `value - netValue`.
5. A verificar: qual evento o sandbox envia para cada meio (`PAYMENT_CONFIRMED`,
   `PAYMENT_RECEIVED`) e se `netValue` muda entre confirmação e recebimento.

### 2. Webhook repetido e fora de ordem

1. No painel do sandbox, reenvie um evento já entregue (ou repita a entrega com o mesmo corpo e o
   header `asaas-access-token`):
   ```bash
   curl -X POST "<webhook_endpoint_url>" -H 'Content-Type: application/json' \
     -H 'asaas-access-token: <token>' -d @evento.json
   ```
2. Esperado: resposta 200 com `duplicate: true`, sem segundo pagamento nem novo lançamento.
3. Entregue um evento anterior depois de um posterior (por exemplo, `PAYMENT_CREATED` depois de
   `PAYMENT_RECEIVED`). Esperado: a cobrança não regride; a situação recebida é registrada.
4. Token incorreto ou ausente: 401, sem persistir o evento.

### 3. Reembolso parcial e total

1. Na cobrança paga, use *Reembolsar* com valor parcial e motivo.
2. Esperado no Recorvia: `POST /v3/payments/{id}/refund` com `value` parcial; o reembolso fica
   *Concluído* quando a resposta já traz o estorno `DONE` ou *Solicitado* até o webhook
   (`PAYMENT_PARTIALLY_REFUNDED`) ou a consulta confirmarem; a cobrança fica *Parcialmente
   reembolsada*, a fatura continua *Paga* e o ledger registra `refund.succeeded`.
3. Reembolse o saldo. Esperado: cobrança *Reembolsada* e reembolsável zero; um novo pedido é
   recusado (409).
4. A verificar no sandbox: meios que aceitam reembolso parcial, situação inicial do estorno
   (`PENDING` ou `DONE`), tempo até `DONE`, se `value` da cobrança permanece o valor original e se
   a lista `refunds` mantém os estornos anteriores. Divergências aparecem na conciliação como
   *Reembolso pendente* ou *Valor divergente*.

### 4. Reembolso feito no painel do Asaas

1. Estorne parcialmente uma cobrança paga diretamente no painel do sandbox.
2. Esperado: ao receber o webhook, o Recorvia registra o reembolso com origem *provedor*
   (`origin = 'provider'`), sem solicitante, e aplica os mesmos efeitos na cobrança e no ledger
   (ADR-035). Valor que exceda o saldo elegível não é registrado e aparece na conciliação.

### 5. Cancelamento no provedor

1. Emita uma fatura, aguarde a cobrança no Asaas e cancele a fatura no Recorvia.
2. Esperado: operação `cancel_charge` em *Operações de pagamento* concluída, cobrança removida no
   Asaas (`DELETE /v3/payments/{id}`) e evento `PAYMENT_DELETED` sem efeito adicional.
3. Repita com uma cobrança paga no Asaas antes do cancelamento local. Esperado: o provedor recusa
   a exclusão, a operação termina como falha e o pagamento recebido é aplicado; com a fatura
   cancelada, o valor vira crédito do cliente (ADR-030 e ADR-037).

### 6. Conciliação e divergência

1. Em *Conciliação → Execuções*, use *Executar conciliação agora*. Esperado: execução *Concluída*
   com as cobranças da janela de 30 dias verificadas e nenhuma divergência.
2. Crie uma divergência: desative temporariamente o webhook no painel do sandbox, confirme o
   recebimento de uma cobrança em aberto e execute a conciliação. Esperado: divergência
   *Pagamento não refletido*; *Aceitar o valor do provedor* aplica o pagamento, registra
   responsável, justificativa e histórico e mantém o ledger balanceado.
3. Reative o webhook e confirme que os eventos represados não duplicam o pagamento.
4. A verificar: o filtro `dateCreated` usa datas sem horário; cobranças criadas perto da meia-noite
   podem ficar fora da listagem e são consultadas individualmente (`GET /payments/{id}`). Registre
   se o sandbox interpreta as datas no fuso de Brasília.
5. Limitação da listagem: a API filtra `GET /payments` pela data de criação, sem filtro por data de
   atualização. Uma cobrança criada antes da janela que recebe chargeback, estorno no painel ou
   pagamento tardio com o webhook perdido não aparece na listagem nem é atualizada localmente. Para
   cobrir esse caso, cada execução consulta individualmente até 100 cobranças fora da janela, criadas
   nos últimos 180 dias e ainda sujeitas a mudança (aguardando pagamento, pagas, parcialmente
   reembolsadas ou em disputa), em rodízio pelas verificadas há mais tempo (ADR-038). Com muitas
   cobranças nessa situação, a detecção pode levar várias execuções; para verificar uma cobrança
   específica antes disso, consulte-a no painel do provedor.

### 7. Chargeback

O sandbox pode não permitir simular contestação. Se houver simulação disponível para cartão de
crédito, verifique a abertura da disputa em *Disputas* com `PAYMENT_CHARGEBACK_REQUESTED`, a retenção
no ledger e o encerramento como ganha com `PAYMENT_AWAITING_CHARGEBACK_REVERSAL`. A decisão perdida
não tem situação documentada pelo Asaas (ADR-036); sem simulação, o fluxo permanece validado somente
com o simulador local.

### 8. Resultado desconhecido

Falhas de rede durante a criação, o reembolso ou o cancelamento não são reproduzíveis de forma
controlada no sandbox. O tratamento (consulta por `externalReference` ou pela cobrança antes de
repetir, operação `unknown` visível em *Operações de pagamento* e na conciliação) é validado com os
cenários `timeout_after_success`, `error_after_success` e `refund_error_after_success` do
simulador (`docs/roteiro-demo.md`).

## Encerramento

1. Desative ou exclua o webhook *Recorvia* no painel do sandbox e remova `compose.override.yaml`.
2. Revogue a chave de API usada no teste se ela tiver sido compartilhada.
3. Registre o resultado de cada cenário (validado, divergente ou não aplicável no sandbox) e as
   diferenças encontradas em relação à documentação, para ajuste do adaptador e dos testes de
   contrato.
