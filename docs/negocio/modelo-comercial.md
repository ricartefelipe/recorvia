# Modelo comercial (hipóteses)

Os valores abaixo são **hipóteses de trabalho configuráveis**, não dados de mercado. Ficam na
tabela `platform_plans` (seed da migration `00003_plataforma.sql`) e podem ser alterados sem
mudança de código.

## Planos da plataforma (mensalidade do tenant ao Recorvia)

| Plano | Mensalidade (hipótese) | Usuários | Contratos ativos | Recursos |
|---|---|---|---|---|
| Essencial (padrão, 14 dias de avaliação) | R$ 199,00 | 3 | 100 | recorrência, cobrança por e-mail |
| Profissional | R$ 499,00 | 10 | 1.000 | + consumo medido, webhooks enviados, API, Asaas |
| Escala | R$ 1.299,00 | ilimitado | ilimitado | + automação de conciliação |

- Limites são aplicados nos casos de uso: convite de membro (membros ativos + convites pendentes),
  criação de assinatura (contratos ativos = `trialing`, `active`, `past_due`) e recursos (por
  exemplo, preço por consumo exige `usage_billing`; Asaas exige `asaas_integration`).
- Troca de plano valida o consumo atual contra os limites do plano de destino.
- Demonstrativo mensal (`platform_statements`) por período; períodos inteiramente em avaliação não
  geram cobrança da mensalidade.

## Serviço adicional

| Serviço | Valor (hipótese) | Tipo |
|---|---|---|
| Implantação assistida | R$ 2.500,00 | cobrança única |

Inclui configuração do catálogo, migração de contratos (assinaturas retroativas até 366 dias) e
treinamento. O pedido é registrado pela organização e acompanhado pela equipe da plataforma.

## Separação dos relacionamentos

A assinatura do tenant ao Recorvia (`tenant_platform_subscriptions`, módulo `platformbilling`,
permissões `platform_billing.*`) é distinta das assinaturas que o tenant vende aos próprios clientes
(`subscriptions`, módulos `subscriptions`/`billing`, permissões `subscriptions.*`, `invoices.*`).

## Premissas a validar

- Disposição a pagar por faixa de contratos ativos em software houses e consultorias.
- Proporção de clientes que exigem implantação assistida.
- Custo de suporte por organização e impacto das integrações com provedores.
