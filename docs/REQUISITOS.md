# Requisitos do Recorvia

Este documento registra os requisitos identificados e os critérios de aceite
verificáveis. Os identificadores (`RF-xx`, `RNF-xx`) são citados em testes,
pull requests e no `docs/STATUS.md`.

## Contexto do produto

O Recorvia é uma plataforma SaaS B2B multitenant para gestão de assinaturas,
faturamento recorrente, cobranças, recuperação de inadimplência e conciliação
de recebíveis. O público inicial são prestadores de serviços recorrentes
(software houses, consultorias e suporte de TI).

Há dois relacionamentos comerciais distintos, separados em nomes, permissões
e modelos de dados:

| Relacionamento | Quem paga | Quem recebe | Módulo | Prefixo de dados |
|---|---|---|---|---|
| Uso da plataforma | Empresa contratante (tenant) | Recorvia | `platformbilling` | `platform_*` |
| Serviços do tenant | Clientes do tenant | Tenant | `subscriptions`, `billing`, `payments` | sem prefixo, sempre com `tenant_id` |

O Recorvia não custodia dinheiro, não oferece carteira digital e não processa
cartões. Integra-se a provedores de pagamento e nunca recebe PAN ou CVV.

## Requisitos funcionais

### RF-01 Organizações, onboarding e membros
- Cadastro de organização (tenant) com nome, documento, fuso IANA, prefixo de
  numeração e prazo padrão de pagamento.
- Onboarding: quem cria a organização torna-se proprietário e recebe o plano
  inicial da plataforma em período de avaliação.
- Convites com expiração, uso único e revogação; o token é guardado apenas
  como hash.
- Gestão de membros: listar, alterar papel, remover. O último proprietário não
  pode ser removido nem rebaixado.
- Um usuário pode participar de vários tenants e alternar entre eles.

**Aceite:** convite expirado, revogado ou já usado é rejeitado; aceitar o
mesmo convite duas vezes não cria duas associações; alternar de tenant rotaciona
a sessão e exige associação ativa.

### RF-02 Autorização
- Papéis: `owner`, `admin`, `finance`, `operator`, `auditor`.
- Permissões explícitas por ação (matriz em `docs/arquitetura/permissoes.md`).
- O tenant informado por header, rota ou interface é sempre validado contra a
  identidade autenticada e a associação ativa.
- Verificação de propriedade de recurso em todos os acessos (filtro explícito
  por `tenant_id`, FKs compostas e RLS).

**Aceite:** um auditor não altera dados; um operador não estorna; um usuário
sem associação recebe 403 mesmo informando um tenant existente; recurso de
outro tenant responde 404.

### RF-03 Clientes
- Pessoa física ou jurídica, contatos, endereço de cobrança, identificador
  externo único por tenant e histórico de alterações.
- Validação estrutural de CPF/CNPJ (dígitos verificadores). A validação não
  confirma identidade.

**Aceite:** CPF/CNPJ com dígitos inválidos é rejeitado; identificador externo
duplicado no mesmo tenant gera conflito; o mesmo identificador em outro tenant
é permitido.

### RF-04 Catálogo e preços
- Produtos, planos e versões de preço imutáveis.
- Periodicidade mensal e anual.
- Modelos: fixo, por quantidade (usuários/unidades), por consumo e fixo com
  excedente de consumo.
- Mudanças de preço criam nova versão; assinaturas e faturas existentes
  mantêm a versão e o snapshot originais.

**Aceite:** alterar o preço de um plano não altera assinatura existente nem
fatura emitida; versão de preço não pode ser alterada após criada.

### RF-05 Assinaturas
- Trial configurável, início, renovação, quantidade contratada.
- Upgrade/downgrade imediato (com prorrata) ou no próximo ciclo.
- Suspensão e reativação; cancelamento imediato ou ao fim do período.
- Reajuste programado com histórico.
- Máquina de estados explícita com rejeição de transições inválidas.

**Aceite:** transições inválidas retornam conflito; a prorrata segue a política
documentada em `docs/arquitetura/dinheiro-e-datas.md` com exemplos testados.

### RF-06 Faturamento
- Geração de faturas pelo motor de recorrência, itens discriminados com
  snapshot de preço, descontos e cupons verificáveis, créditos e ajustes,
  vencimento e condições, pagamentos parciais, histórico de transições,
  numeração única por tenant, documento HTML e exportação CSV.
- Fatura emitida não é reescrita nem excluída; correções usam nota de crédito,
  cancelamento (`void`) com histórico e lançamentos compensatórios.
- Fatura comercial não é documento fiscal.

**Aceite:** existe no máximo uma fatura por assinatura e período (constraint);
retomada após indisponibilidade gera as faturas atrasadas sem duplicar;
numeração não se repete por tenant.

### RF-07 Medição de consumo
- Eventos com identificador externo único, tenant, cliente, assinatura,
  métrica, quantidade, data de ocorrência e de recebimento.
- Deduplicação, validação de unidade e limites, política para eventos
  atrasados, janela de fechamento e rastreabilidade até o item faturado.
- Agregações por semântica da métrica: `sum`, `max`, `unique_count`, `last`.
- Correção após fechamento gera ajuste na próxima fatura, sem alterar a
  fatura emitida.

**Aceite:** evento repetido não altera totais; evento após o fechamento vira
ajuste rastreável; métrica `max` não soma eventos.

### RF-08 Pagamentos
- Porta de integração com dois adaptadores: simulador local determinístico e
  Asaas (sandbox).
- Criar cobrança, consultar situação, solicitar reembolso.
- Checkout hospedado do provedor; nenhum dado de cartão trafega pelo Recorvia.
- Credenciais por tenant, cifradas em repouso e nunca devolvidas pela API.
- O Recorvia controla a recorrência: cada fatura gera uma cobrança avulsa no
  provedor; a recorrência do provedor não é habilitada.

**Aceite:** o simulador reproduz confirmação, falha, demora, timeout,
duplicidade, eventos fora de ordem, reembolso e divergência de conciliação.

### RF-09 Idempotência e falhas distribuídas
- `Idempotency-Key` obrigatório nas operações financeiras de criação (lista
  e critério na ADR-075).
- Escopo: tenant, cliente da API e operação; impressão (hash do método, do
  caminho e dos bytes do corpo) do pedido e resultado persistidos.
- Repetição equivalente retorna o resultado registrado; conteúdo diferente
  gera conflito; concorrência não duplica efeito; processamento interrompido é
  recuperado após expiração do lease.
- Resposta perdida do provedor não gera nova cobrança cega: o estado
  `unknown` é resolvido por consulta via referência externa ou conciliação.

### RF-10 Eventos e processamento assíncrono
- Transactional Outbox no PostgreSQL com lease, confirmação de publicação e
  recuperação.
- RabbitMQ com filas duráveis, mensagens persistentes, publisher confirms, ack
  manual, prefetch e concorrência configuráveis, retentativas limitadas com
  backoff e jitter, DLQ e reprocessamento autorizado e auditado.
- Consumidores com inbox persistente; ack somente após commit.
- Agendamentos seguros com múltiplas instâncias.

### RF-11 Webhooks
- Recebidos: autenticação conforme o provedor, limite de tamanho, vínculo
  confiável com o tenant, persistência antes do 200, processamento
  assíncrono, deduplicação, tolerância a ordem e consulta ao provedor.
- Reembolsos, disputas e chargebacks tardios modelados.
- Enviados: assinatura HMAC com timestamp, identificador estável, rotação de
  segredo, retentativas, histórico e proteção contra SSRF.

### RF-12 Ledger, recebíveis e conciliação
- Ledger operacional imutável de partidas dobradas, balanceado por moeda.
- Contas distintas para faturamento, contas a receber, confirmação, taxas,
  valores em liquidação, reembolso e chargeback.
- Correções por lançamentos compensatórios.
- Conciliação com o provedor: cobrança sem correspondência, valor divergente,
  taxa divergente, pagamento não refletido, reembolso/chargeback pendente e
  operação com resultado desconhecido.
- Resolução com responsável, justificativa, histórico e permissão específica.
- Reembolsos concorrentes nunca excedem o saldo elegível.

O ledger não substitui contabilidade fiscal nem reconhecimento contábil.

### RF-13 Inadimplência e notificações
- Políticas configuráveis de lembretes antes e depois do vencimento.
- Identidade de negócio por notificação (sem duplicidade em retentativas).
- Templates, preferências de contato, janela de horário e fuso do tenant,
  limites de frequência, histórico de envio, interrupção após resolução,
  suspensão e reativação conforme política.
- E-mail via SMTP (Mailpit no ambiente local). Outros canais não aparecem na
  interface.

### RF-14 Identidade e sessões
- Keycloak com Authorization Code + PKCE; sessão no backend Go com cookie
  HttpOnly, `Secure` em produção e `SameSite=Lax`.
- Tokens do provedor permanecem no servidor; proteção CSRF; expiração,
  revogação, rotação e logout.
- Credenciais máquina a máquina com escopos e vínculo a um tenant, rotação e
  revogação.
- Portal do cliente via link temporário de uso único trocado por sessão
  restrita ao cliente e tenant.

### RF-15 API e interface
- OpenAPI consistente com os handlers (teste de contrato).
- Paginação com limite, filtros e ordenação em allowlist, ordenação
  determinística, `X-Request-ID`, erros estruturados (RFC 9457) e
  concorrência otimista (`If-Match`/`version`).
- Interface em português com as telas: login e seleção de organização,
  onboarding, dashboard, clientes, catálogo e preços, assinaturas, faturas e
  pagamentos, conciliação, políticas de cobrança, usuários e permissões,
  integrações e webhooks, auditoria e portal do cliente.

### RF-16 Faturamento da plataforma
- Planos da plataforma com limites de usuários, contratos ativos e
  funcionalidades; implantação assistida como serviço adicional.
- Valores são hipóteses configuráveis (`docs/negocio/modelo-comercial.md`).
- Limites aplicados nos casos de uso (convites, assinaturas ativas, recursos).

### RF-17 Dashboard
- Receita faturada e recebida separadas; MRR, ARR, inadimplência, churn e
  demais indicadores com fórmulas documentadas.

## Requisitos não funcionais

| ID | Requisito | Verificação |
|---|---|---|
| RNF-01 | Dinheiro em unidades mínimas `int64`, cálculos com `big.Rat`, sem `float` | testes e fuzz em `internal/kernel/money` |
| RNF-02 | Instantes em UTC; calendário no fuso IANA do tenant | testes em `internal/kernel/calendar` |
| RNF-03 | Isolamento por RLS com papel sem privilégios | testes de integração com o papel `recorvia_app` |
| RNF-04 | Cobertura ≥ 90% de statements nos pacotes de domínio e aplicação | `make coverage` |
| RNF-05 | Testes unitários sem Docker, rede ou credenciais | `make test` |
| RNF-06 | Testes de integração com PostgreSQL, RabbitMQ e Redis reais | `make test-integration` |
| RNF-07 | Logs estruturados com correlação HTTP/eventos/tarefas | revisão e testes de middleware |
| RNF-08 | Métricas sem rótulos de alta cardinalidade | revisão em `internal/infra/telemetry` |
| RNF-09 | Liveness e readiness com semânticas distintas | testes HTTP |
| RNF-10 | Versões fixadas de dependências, ferramentas e imagens | revisão e CI |
| RNF-11 | Encerramento gracioso e propagação de cancelamento | testes de ciclo de vida |
| RNF-12 | Segredos cifrados em repouso e redigidos em logs | testes de `internal/infra/secrets` e logging |
