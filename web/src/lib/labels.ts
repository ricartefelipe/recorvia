/** Rótulos em português para os enums do contrato e cores de situação. */

type Labels = Record<string, string>;

export function labelOf(labels: Labels, value: string | null | undefined): string {
  if (value === null || value === undefined || value === '') return '—';
  return labels[value] ?? value;
}

export function optionsOf(labels: Labels): { value: string; label: string }[] {
  return Object.entries(labels).map(([value, label]) => ({ value, label }));
}

export const roleLabels: Labels = {
  owner: 'Proprietário',
  admin: 'Administrador',
  finance: 'Financeiro',
  operator: 'Operador',
  auditor: 'Auditor',
};

export const invitationStateLabels: Labels = {
  pending: 'Pendente',
  accepted: 'Aceito',
  revoked: 'Revogado',
  expired: 'Expirado',
};

/** Situação dos links do portal do cliente. */
export const portalLinkStatusLabels: Labels = {
  pending: 'Pendente',
  used: 'Utilizado',
  expired: 'Expirado',
  revoked: 'Revogado',
};

export const memberStatusLabels: Labels = { active: 'Ativo', removed: 'Removido' };

export const customerKindLabels: Labels = { individual: 'Pessoa física', company: 'Pessoa jurídica' };
export const customerStatusLabels: Labels = { active: 'Ativo', archived: 'Arquivado' };
export const catalogStatusLabels: Labels = { active: 'Ativo', archived: 'Arquivado' };

export const pricingModelLabels: Labels = {
  flat: 'Valor fixo',
  per_unit: 'Por quantidade (unidades ou usuários)',
  usage: 'Por consumo medido',
  flat_plus_overage: 'Fixo com excedente de consumo',
};

export const billingIntervalLabels: Labels = { month: 'Mensal', year: 'Anual' };

export const aggregationLabels: Labels = {
  sum: 'Soma',
  max: 'Máximo',
  unique_count: 'Contagem de únicos',
  last: 'Último valor',
};

export const aggregationHelp: Labels = {
  sum: 'Soma as quantidades de todos os eventos do período (ex.: chamadas de API).',
  max: 'Usa o maior valor informado no período, sem somar eventos (ex.: pico de usuários simultâneos).',
  unique_count:
    'Conta chaves distintas (unique_key) no período; repetições da mesma chave contam uma vez (ex.: usuários ativos).',
  last: 'Usa o último valor informado no período (ex.: armazenamento ocupado no fechamento).',
};

export const discountTypeLabels: Labels = { percent: 'Percentual', amount: 'Valor fixo' };
export const couponDurationLabels: Labels = {
  once: 'Somente na primeira fatura',
  repeating: 'Por um número de ciclos',
  forever: 'Enquanto a assinatura existir',
};

export const subscriptionStatusLabels: Labels = {
  trialing: 'Em período de teste',
  active: 'Ativa',
  past_due: 'Em atraso',
  suspended: 'Suspensa',
  canceled: 'Cancelada',
};

export const changeTimingLabels: Labels = {
  immediate: 'Imediatamente (com prorrata)',
  next_cycle: 'No próximo ciclo',
};

export const scheduledChangeKindLabels: Labels = {
  plan_change: 'Troca de plano',
  quantity_change: 'Alteração de quantidade',
};
export const scheduledChangeStatusLabels: Labels = { pending: 'Pendente', applied: 'Aplicada', canceled: 'Cancelada' };

export const priceAdjustmentKindLabels: Labels = { percent: 'Percentual', fixed_amount: 'Novo valor' };
export const priceAdjustmentStatusLabels: Labels = { scheduled: 'Agendado', applied: 'Aplicado', canceled: 'Cancelado' };
export const discountStatusLabels: Labels = { active: 'Ativo', ended: 'Encerrado' };

export const invoiceStatusLabels: Labels = {
  draft: 'Rascunho',
  open: 'Em aberto',
  partially_paid: 'Parcialmente paga',
  paid: 'Paga',
  void: 'Cancelada',
  uncollectible: 'Incobrável',
};

/**
 * Situações exibidas ao cliente no portal: a baixa como incobrável é um
 * registro interno da organização; para o cliente, a fatura está encerrada.
 */
export const portalInvoiceStatusLabels: Labels = { ...invoiceStatusLabels, uncollectible: 'Encerrada' };

export const invoiceKindLabels: Labels = {
  subscription_cycle: 'Ciclo da assinatura',
  final_usage: 'Consumo final',
  proration: 'Prorrata',
  manual: 'Avulsa',
};

export const invoiceItemKindLabels: Labels = {
  subscription_fee: 'Mensalidade',
  usage: 'Consumo',
  overage: 'Excedente',
  proration: 'Prorrata',
  adjustment: 'Ajuste',
};

export const paymentMethodLabels: Labels = {
  provider: 'Pelo provedor de pagamento',
  unknown: 'Não informado',
  pix: 'PIX',
  bank_transfer: 'Transferência bancária',
  boleto: 'Boleto',
  cash: 'Dinheiro',
  other: 'Outro',
};

export const paymentSourceLabels: Labels = { provider: 'Provedor de pagamento', manual: 'Registro manual' };

export const chargeStatusLabels: Labels = {
  pending: 'Pendente',
  awaiting_payment: 'Aguardando pagamento',
  paid: 'Paga',
  failed: 'Falhou',
  canceled: 'Cancelada',
  partially_refunded: 'Parcialmente reembolsada',
  refunded: 'Reembolsada',
  disputed: 'Em disputa',
  charged_back: 'Chargeback',
};

export const refundStatusLabels: Labels = {
  requested: 'Solicitado',
  succeeded: 'Concluído',
  failed: 'Falhou',
  unknown: 'Resultado desconhecido',
};

export const disputeStatusLabels: Labels = {
  opened: 'Aberta',
  under_review: 'Em análise',
  won: 'Ganha',
  lost: 'Perdida',
};

export const recoveryOutcomeLabels: Labels = {
  received: 'Recebido',
  written_off: 'Perdido',
};

export const paymentOperationStatusLabels: Labels = {
  pending: 'Pendente',
  in_flight: 'Em andamento',
  succeeded: 'Concluída',
  failed: 'Falhou',
  unknown: 'Resultado desconhecido',
};

export const paymentOperationKindLabels: Labels = {
  create_charge: 'Criação de cobrança',
  refund: 'Reembolso',
  cancel_charge: 'Cancelamento no provedor',
};

export const creditKindLabels: Labels = {
  grant: 'Concessão',
  consumption: 'Uso em fatura',
  return: 'Devolução',
  overpayment: 'Pagamento a maior',
  proration: 'Proporcional de mudança de plano',
  usage_correction: 'Correção de consumo',
};

export const providerLabels: Labels = { simulator: 'Simulador de pagamentos', asaas: 'Asaas' };
export const providerEnvironmentLabels: Labels = {
  local: 'Local',
  sandbox: 'Sandbox (testes)',
  production: 'Produção',
};

export const ledgerAccountKindLabels: Labels = {
  asset: 'Ativo',
  liability: 'Passivo',
  revenue: 'Receita',
  contra_revenue: 'Dedução de receita',
  expense: 'Despesa',
};

export const ledgerReferenceLabels: Labels = {
  invoice: 'Fatura',
  payment: 'Pagamento',
  refund: 'Reembolso',
  dispute: 'Disputa',
  dispute_recovery_settlement: 'Baixa de valor a recuperar',
  credit_note: 'Nota de crédito',
  customer_credit: 'Crédito de cliente',
  settlement: 'Liquidação',
  reconciliation_discrepancy: 'Divergência de conciliação',
};

export const discrepancyKindLabels: Labels = {
  missing_local: 'Cobrança sem registro local',
  missing_provider: 'Cobrança sem registro no provedor',
  amount_mismatch: 'Valor divergente',
  fee_mismatch: 'Taxa divergente',
  payment_not_reflected: 'Pagamento não refletido',
  refund_pending: 'Reembolso pendente',
  chargeback_pending: 'Chargeback pendente',
  unknown_operation: 'Operação com resultado desconhecido',
};

export const discrepancyStatusLabels: Labels = { open: 'Aberta', resolved: 'Resolvida', dismissed: 'Descartada' };

export const resolutionLabels: Labels = {
  accept_provider: 'Aceitar o valor do provedor',
  keep_local: 'Manter o valor local',
  adjust_ledger: 'Ajustar o ledger com lançamento compensatório',
  dismiss: 'Descartar a divergência',
  auto_resolved: 'Resolvida automaticamente (não constatada em nova execução)',
};

export const reconciliationRunStatusLabels: Labels = {
  running: 'Em execução',
  completed: 'Concluída',
  failed: 'Falhou',
};

export const reconciliationRunTriggerLabels: Labels = {
  manual: 'Manual',
  scheduled: 'Agendada',
};

export const templateKeyLabels: Labels = {
  invoice_issued: 'Fatura emitida',
  reminder_before_due: 'Lembrete antes do vencimento',
  reminder_after_due: 'Lembrete após o vencimento',
  payment_confirmed: 'Pagamento confirmado',
  subscription_suspended: 'Assinatura suspensa',
  member_invitation: 'Convite de membro',
};

export const notificationStatusLabels: Labels = {
  scheduled: 'Agendada',
  sending: 'Enviando',
  sent: 'Enviada',
  failed: 'Falhou',
  canceled: 'Cancelada',
  suppressed: 'Suprimida',
};

/**
 * Motivos da situação de uma notificação (status_reason) e dos eventos da
 * linha do tempo, pelos códigos estáveis do contrato.
 */
export const notificationReasonLabels: Labels = {
  scheduled: 'Agendada',
  attempt: 'Tentativa de envio',
  sent: 'Aceita pelo servidor de e-mail',
  retry_scheduled: 'Nova tentativa agendada',
  outside_send_window: 'Fora da janela de envio',
  frequency_limit: 'Limite diário de mensagens atingido',
  no_recipient: 'Sem destinatário',
  invitation_link_unavailable: 'Link do convite indisponível',
  invoice_paid: 'Fatura paga',
  invoice_credited: 'Fatura quitada por nota de crédito',
  invoice_voided: 'Fatura cancelada',
  invoice_uncollectible: 'Fatura baixada como incobrável',
  superseded: 'Substituída por um passo posterior da régua',
  due_date_passed: 'O vencimento já passou',
  invitation_not_pending: 'Convite não está mais pendente',
  subscription_not_suspended: 'A assinatura não está mais suspensa',
  feature_unavailable: 'Recurso indisponível no plano da plataforma',
  permanent_failure: 'Falha permanente no envio',
  attempts_exhausted: 'Tentativas esgotadas',
  content_unavailable: 'Conteúdo da mensagem indisponível',
};

export const channelLabels: Labels = { email: 'E-mail' };

export const webhookStatusLabels: Labels = { active: 'Ativo', disabled: 'Desativado' };
export const deliveryStatusLabels: Labels = {
  pending: 'Pendente',
  delivering: 'Entregando',
  succeeded: 'Entregue',
  retrying: 'Nova tentativa agendada',
  exhausted: 'Tentativas esgotadas',
};

export const deadLetterStatusLabels: Labels = { open: 'Aberta', replayed: 'Reprocessada', discarded: 'Descartada' };

export const platformSubscriptionStatusLabels: Labels = {
  trialing: 'Em período de teste',
  active: 'Ativa',
  past_due: 'Em atraso',
  canceled: 'Cancelada',
};

export const addonOrderStatusLabels: Labels = {
  requested: 'Solicitado',
  scheduled: 'Agendado',
  completed: 'Concluído',
  canceled: 'Cancelado',
};

export const usageEventStatusLabels: Labels = { accepted: 'Aceito', late: 'Atrasado' };

export const organizationStatusLabels: Labels = { active: 'Ativa', suspended: 'Suspensa', closed: 'Encerrada' };

export const actorTypeLabels: Labels = {
  user: 'Usuário',
  system: 'Sistema',
  api_credential: 'Credencial de API',
  provider: 'Provedor',
  customer: 'Cliente',
  customer_portal: 'Portal do cliente',
};

/** Motivo das transições do histórico da fatura; cancelamento e baixa guardam o texto digitado. */
export const invoiceHistoryReasonLabels: Labels = {
  issued: 'Emissão',
  issued_fully_credited: 'Emitida já quitada com crédito do cliente',
  payment: 'Pagamento',
  credit_note: 'Nota de crédito',
  refund: 'Reembolso',
  chargeback: 'Chargeback',
};

/** Eventos do histórico da assinatura. */
export const subscriptionEventLabels: Labels = {
  created: 'Assinatura criada',
  trial_ended: 'Fim do período de teste',
  renewed: 'Renovação',
  ended_at_period_end: 'Encerrada no fim do período',
  final_usage_billed: 'Consumo final faturado',
  canceled: 'Cancelamento',
  cancel_scheduled: 'Cancelamento agendado',
  cancel_unscheduled: 'Agendamento de cancelamento desfeito',
  suspended: 'Suspensão',
  reactivated: 'Reativação',
  past_due: 'Pagamento em atraso',
  payment_recovered: 'Pagamento regularizado',
  plan_changed: 'Troca de plano',
  quantity_changed: 'Alteração de quantidade',
  change_scheduled: 'Mudança agendada',
  price_adjusted: 'Reajuste aplicado',
  price_adjustment_canceled: 'Reajuste cancelado',
  price_adjustment_scheduled: 'Reajuste agendado',
  discount_applied: 'Desconto aplicado',
  discount_removed: 'Desconto removido',
};

/** Ações registradas na auditoria. */
export const auditActionLabels: Labels = {
  'organization.created': 'Organização criada',
  'organization.updated': 'Dados da organização alterados',
  'organization.address_updated': 'Endereço da organização alterado',
  'member.invited': 'Membro convidado',
  'member.invitation_revoked': 'Convite revogado',
  'member.joined': 'Membro entrou na organização',
  'member.role_changed': 'Papel do membro alterado',
  'member.removed': 'Membro removido',
  'api_credential.created': 'Credencial de API criada',
  'api_credential.rotated': 'Credencial de API renovada',
  'api_credential.revoked': 'Credencial de API revogada',
  'customer.created': 'Cliente cadastrado',
  'customer.updated': 'Cliente alterado',
  'customer.archived': 'Cliente arquivado',
  'customer.anonymized': 'Cliente anonimizado',
  'customer.contact_added': 'Contato adicionado',
  'customer.contact_updated': 'Contato alterado',
  'customer.contact_removed': 'Contato removido',
  'customer.address_updated': 'Endereço do cliente alterado',
  'customer.portal_link_created': 'Link do portal criado',
  'customer.portal_link_revoked': 'Link do portal revogado',
  'customer.contact_preferences_updated': 'Preferências de contato alteradas',
  'customer.credit_granted': 'Crédito concedido ao cliente',
  'customer_portal.session_started': 'Acesso ao portal do cliente',
  'customer_portal.session_ended': 'Saída do portal do cliente',
  'catalog.product_created': 'Produto criado',
  'catalog.product_updated': 'Produto alterado',
  'catalog.metric_created': 'Métrica de consumo criada',
  'catalog.plan_created': 'Plano criado',
  'catalog.plan_updated': 'Plano alterado',
  'catalog.price_version_created': 'Nova versão de preço',
  'coupon.created': 'Cupom criado',
  'coupon.archived': 'Cupom arquivado',
  'subscription.created': 'Assinatura criada',
  'subscription.canceled': 'Assinatura cancelada',
  'subscription.renewal_resumed': 'Renovação retomada',
  'subscription.suspended': 'Assinatura suspensa',
  'subscription.reactivated': 'Assinatura reativada',
  'subscription.plan_changed': 'Plano da assinatura trocado',
  'subscription.quantity_changed': 'Quantidade da assinatura alterada',
  'subscription.change_scheduled': 'Mudança de assinatura agendada',
  'subscription.discount_applied': 'Desconto aplicado',
  'subscription.discount_removed': 'Desconto removido',
  'subscription.price_adjustment_scheduled': 'Reajuste agendado',
  'subscription.price_adjustment_canceled': 'Reajuste cancelado',
  'subscription.price_adjusted': 'Reajuste aplicado',
  'customer_credit.proration': 'Crédito de prorrata',
  'usage.corrected': 'Consumo corrigido',
  'invoice.issued': 'Fatura emitida',
  'invoice.voided': 'Fatura cancelada',
  'invoice.marked_uncollectible': 'Fatura baixada como incobrável',
  'invoice.credit_note_issued': 'Nota de crédito emitida',
  'invoices.exported': 'Faturas exportadas',
  'payment.applied': 'Pagamento aplicado',
  'payment.overpayment_credited': 'Pagamento a maior convertido em crédito',
  'charge.requested': 'Cobrança solicitada ao provedor',
  'charge.linked_after_local_cancellation': 'Cobrança vinculada após cancelamento local',
  'charge.canceled_after_balance_change': 'Cobrança cancelada após mudança de saldo',
  'charge.canceled_after_credit_note': 'Cobrança cancelada após nota de crédito',
  'charge.paid_after_failure': 'Cobrança paga após falha',
  'charge.paid_after_local_cancellation': 'Cobrança paga após cancelamento local',
  'charge.provider_canceled': 'Cobrança cancelada no provedor',
  'charge.provider_cancel_failed': 'Falha ao cancelar cobrança no provedor',
  'charge.canceled_missing_at_provider': 'Cobrança cancelada (inexistente no provedor)',
  'payment_operation.succeeded': 'Operação de pagamento concluída',
  'payment_operation.failed': 'Operação de pagamento com falha',
  'payment_operation.pending': 'Operação de pagamento pendente',
  'payment_operation.unknown': 'Operação de pagamento com resultado desconhecido',
  'refund.requested': 'Reembolso solicitado',
  'refund.reopened_by_provider': 'Reembolso reaberto pelo provedor',
  'refund.succeeded': 'Reembolso concluído',
  'refund.failed': 'Reembolso com falha',
  'refund.recorded_from_provider': 'Reembolso registrado a partir do provedor',
  'dispute.opened': 'Disputa aberta',
  'dispute.under_review': 'Disputa em análise',
  'dispute.won': 'Disputa ganha',
  'dispute.lost': 'Disputa perdida',
  'dispute.chargeback_increased': 'Valor do chargeback aumentado',
  'dispute.recovery_settled': 'Valor a recuperar baixado',
  'reconciliation.run_started': 'Conciliação iniciada',
  'reconciliation.run_failed': 'Conciliação com falha',
  'reconciliation.run_resumed': 'Conciliação retomada',
  'reconciliation.run_released': 'Conciliação liberada',
  'reconciliation.run_completed': 'Conciliação concluída',
  'reconciliation.discrepancy_detected': 'Divergência encontrada',
  'reconciliation.discrepancy_reopened': 'Divergência reaberta',
  'reconciliation.discrepancy_auto_resolved': 'Divergência resolvida automaticamente',
  'reconciliation.discrepancy_resolved': 'Divergência resolvida',
  'integration.provider_configured': 'Provedor de pagamento configurado',
  'webhook_endpoint.created': 'Endpoint de webhook criado',
  'webhook_endpoint.updated': 'Endpoint de webhook alterado',
  'webhook_endpoint.deleted': 'Endpoint de webhook excluído',
  'webhook_endpoint.secret_rotated': 'Segredo do webhook renovado',
  'webhook_delivery.retried': 'Entrega de webhook reenviada',
  'notification_template.updated': 'Template de e-mail alterado',
  'collection_policy.created': 'Política de cobrança criada',
  'collection_policy.updated': 'Política de cobrança alterada',
  'platform.plan_changed': 'Plano Recorvia trocado',
  'platform.addon_requested': 'Serviço adicional solicitado',
  'platform.addon_scheduled': 'Serviço adicional agendado',
  'platform.addon_completed': 'Serviço adicional concluído',
  'platform.addon_canceled': 'Serviço adicional cancelado',
  'operations.dead_letter_replayed': 'Mensagem reprocessada',
  'operations.dead_letter_discarded': 'Mensagem descartada',
};

/** Tipos de recurso da auditoria. */
export const auditResourceLabels: Labels = {
  api_credential: 'Credencial de API',
  charge: 'Cobrança',
  collection_policy: 'Política de cobrança',
  coupon: 'Cupom',
  customer: 'Cliente',
  dead_letter: 'Mensagem não processada',
  dispute: 'Disputa',
  invitation: 'Convite',
  invoice: 'Fatura',
  membership: 'Membro',
  metric: 'Métrica de consumo',
  notification_template: 'Template de e-mail',
  organization: 'Organização',
  payment_operation: 'Operação de pagamento',
  payment_provider_account: 'Conta do provedor',
  plan: 'Plano',
  platform_addon_order: 'Pedido de serviço adicional',
  platform_subscription: 'Plano Recorvia',
  product: 'Produto',
  reconciliation_discrepancy: 'Divergência de conciliação',
  reconciliation_run: 'Execução de conciliação',
  refund: 'Reembolso',
  subscription: 'Assinatura',
  webhook_delivery: 'Entrega de webhook',
  webhook_endpoint: 'Endpoint de webhook',
};

/** Rotinas automáticas que aparecem como ator (actor_id de ator do tipo sistema). */
export const systemActorLabels: Labels = {
  payments: 'Rotina de pagamentos',
  'billing.engine': 'Faturamento automático',
  'billing.usage_correction': 'Correção de consumo',
  collections: 'Régua de cobrança',
  reconciliation: 'Conciliação automática',
  provider: 'Provedor de pagamento',
};

/** Tipos de lançamento do ledger; estornos terminam em ".reversal". */
export const ledgerTransactionKindLabels: Labels = {
  'invoice.issued': 'Emissão de fatura',
  'invoice.uncollectible': 'Baixa como incobrável',
  'invoice.bad_debt_recovered': 'Recuperação de perda',
  'payment.confirmed': 'Pagamento confirmado',
  'payment.received': 'Pagamento recebido',
  'payment.manual': 'Pagamento manual',
  'payment.overpayment': 'Pagamento a maior',
  'refund.succeeded': 'Reembolso',
  'dispute.opened': 'Abertura de disputa',
  'dispute.hold_released': 'Liberação do valor retido',
  'dispute.hold_restored': 'Retenção restabelecida',
  'dispute.credit_reversed': 'Crédito estornado por disputa',
  'dispute.recovery_received': 'Valor recuperado recebido',
  'dispute.recovery_written_off': 'Valor a recuperar baixado',
  'credit_note.issued': 'Nota de crédito',
  'customer_credit.granted': 'Crédito concedido',
  'settlement.received': 'Liquidação recebida',
  'settlement.debited': 'Liquidação debitada',
  'reconciliation.adjustment': 'Ajuste de conciliação',
};

export function ledgerTransactionKindLabel(kind: string | null | undefined): string {
  if (kind && kind.endsWith('.reversal')) {
    return `Estorno: ${labelOf(ledgerTransactionKindLabels, kind.slice(0, -'.reversal'.length)).toLowerCase()}`;
  }
  return labelOf(ledgerTransactionKindLabels, kind);
}

/** Contas do plano de contas operacional (o nome também vem da API na lista de contas). */
export const ledgerAccountLabels: Labels = {
  accounts_receivable: 'Contas a receber',
  billed_revenue: 'Receita faturada',
  provider_confirmed: 'Pagamentos confirmados no provedor',
  provider_clearing: 'Valores em liquidação no provedor',
  cash: 'Disponível liquidado',
  provider_fees: 'Taxas do provedor',
  refunds: 'Reembolsos',
  chargebacks: 'Chargebacks',
  customer_credits: 'Créditos de clientes',
  revenue_adjustments: 'Ajustes de faturamento',
  bad_debt: 'Perdas com inadimplência',
  reconciliation_adjustments: 'Ajustes de conciliação',
  customer_recoverables: 'Valores a recuperar de clientes',
};

/** Recursos dos planos do Recorvia. */
export const platformFeatureLabels: Labels = {
  recurring_billing: 'Cobrança recorrente',
  email_dunning: 'Lembretes de cobrança por e-mail',
  usage_billing: 'Cobrança por consumo medido',
  outbound_webhooks: 'Webhooks para os seus sistemas',
  api_access: 'Acesso à API',
  asaas_integration: 'Integração com o Asaas',
  reconciliation_automation: 'Conciliação automática',
};

export const platformPlanLabels: Labels = { essencial: 'Essencial', profissional: 'Profissional', escala: 'Escala' };

/**
 * Situação informada pelo provedor (simulador em minúsculas, Asaas em
 * maiúsculas), consultada sem diferenciar maiúsculas.
 */
export const providerChargeStatusLabels: Labels = {
  pending: 'Pendente',
  confirmed: 'Confirmada',
  paid: 'Paga',
  failed: 'Falhou',
  canceled: 'Cancelada',
  cancelled: 'Cancelada',
  refunded: 'Reembolsada',
  partially_refunded: 'Parcialmente reembolsada',
  received: 'Recebida',
  received_in_cash: 'Recebida em dinheiro',
  overdue: 'Vencida',
  awaiting_risk_analysis: 'Em análise de risco',
  dunning_requested: 'Negativação solicitada',
  dunning_received: 'Recebida após negativação',
  refund_requested: 'Reembolso solicitado',
  refund_in_progress: 'Reembolso em andamento',
  chargeback_requested: 'Chargeback solicitado',
  chargeback_dispute: 'Chargeback em disputa',
  awaiting_chargeback_reversal: 'Aguardando reversão de chargeback',
  deleted: 'Excluída',
  done: 'Concluída',
};

export function providerChargeStatusLabel(status: string | null | undefined): string {
  if (!status) return '—';
  return providerChargeStatusLabels[status.toLowerCase()] ?? status;
}

/** Situação dos demonstrativos mensais do Recorvia. */
export const platformStatementStatusLabels: Labels = { open: 'Em aberto', paid: 'Pago', void: 'Cancelado' };

/** Situação da conta configurada num provedor de pagamento. */
export const providerAccountStatusLabels: Labels = { active: 'Ativa', disabled: 'Desativada' };

/** Eventos de webhook enviados aos sistemas do cliente. */
export const webhookEventLabels: Labels = {
  'charge.created': 'Cobrança criada',
  'charge.failed': 'Cobrança com falha',
  'customer.created': 'Cliente cadastrado',
  'customer.updated': 'Cliente alterado',
  'dispute.opened': 'Disputa aberta',
  'dispute.closed': 'Disputa encerrada',
  'dispute.recovery_settled': 'Valor a recuperar baixado',
  'invoice.issued': 'Fatura emitida',
  'invoice.paid': 'Fatura paga',
  'invoice.partially_paid': 'Fatura parcialmente paga',
  'invoice.credited': 'Fatura quitada por nota de crédito',
  'invoice.overdue': 'Fatura vencida',
  'invoice.uncollectible': 'Fatura baixada como incobrável',
  'invoice.voided': 'Fatura cancelada',
  'payment.confirmed': 'Pagamento confirmado',
  'refund.succeeded': 'Reembolso concluído',
  'refund.failed': 'Reembolso com falha',
  'subscription.created': 'Assinatura criada',
  'subscription.updated': 'Assinatura alterada',
  'subscription.canceled': 'Assinatura cancelada',
  'subscription.suspended': 'Assinatura suspensa',
  'subscription.reactivated': 'Assinatura reativada',
};

/** Tipos de evento internos que podem aparecer em mensagens não processadas. */
export const internalEventLabels: Labels = {
  ...webhookEventLabels,
  'provider_webhook.received': 'Aviso recebido do provedor',
  'member.invited': 'Membro convidado',
  'operations.dead_letter_replay': 'Reprocessamento de mensagem',
};

/** Consumidores internos de eventos (filas). */
export const consumerLabels: Labels = {
  'payments.invoice_issued': 'Pagamentos: fatura emitida',
  'payments.provider_webhooks': 'Pagamentos: avisos do provedor',
  'payments.invoice_voided': 'Pagamentos: fatura cancelada',
  'payments.invoice_credited': 'Pagamentos: fatura com nota de crédito',
  'notifications.invoice_issued': 'E-mails: fatura emitida',
  'notifications.payment_confirmed': 'E-mails: pagamento confirmado',
  'notifications.member_invited': 'E-mails: convite de membro',
  'collections.invoice_resolved': 'Régua de cobrança: fatura resolvida',
  'webhooks.fanout': 'Webhooks: distribuição de eventos',
};

/** Campos comparados na conciliação (valor local x valor do provedor). */
export const reconciliationFieldLabels: Labels = {
  charge_id: 'Cobrança',
  invoice_id: 'Fatura',
  status: 'Situação',
  normalized_status: 'Situação normalizada',
  amount: 'Valor',
  amount_paid: 'Valor pago',
  fee: 'Taxa',
  amount_refunded: 'Valor reembolsado',
  amount_charged_back: 'Valor em chargeback',
  refunded_amount: 'Valor reembolsado',
  provider_charge_id: 'Identificador no provedor',
  external_reference: 'Referência externa',
  paid_at: 'Pago em',
  direction: 'Sentido',
  basis: 'Base',
  refund_id: 'Reembolso',
  refund_status: 'Situação do reembolso',
  refund_amount: 'Valor do reembolso',
  referenced_charge_id: 'Cobrança referenciada',
  referenced_charge_status: 'Situação da cobrança referenciada',
  referenced_provider_charge_id: 'Cobrança referenciada no provedor',
  operation_id: 'Operação',
  kind: 'Tipo',
  attempts: 'Tentativas',
  last_error: 'Último erro',
  currency: 'Moeda',
};

/** Valores codificados nos campos da conciliação. */
export const reconciliationValueLabels: Record<string, Labels> = {
  direction: { provider_paid: 'Pago no provedor', local_paid: 'Pago no Recorvia', provider_received: 'Recebido pelo provedor' },
  basis: { charge: 'Cobrança', paid: 'Pagamento' },
};

/** Cor do Badge por situação. */
const statusColors: Record<string, string> = {
  active: 'green',
  trialing: 'cyan',
  past_due: 'orange',
  suspended: 'yellow',
  canceled: 'gray',
  archived: 'gray',
  draft: 'gray',
  open: 'blue',
  partially_paid: 'cyan',
  paid: 'green',
  void: 'gray',
  uncollectible: 'red',
  pending: 'yellow',
  awaiting_payment: 'blue',
  failed: 'red',
  partially_refunded: 'grape',
  refunded: 'grape',
  disputed: 'orange',
  charged_back: 'red',
  requested: 'yellow',
  succeeded: 'green',
  unknown: 'red',
  opened: 'orange',
  under_review: 'yellow',
  won: 'green',
  lost: 'red',
  received: 'green',
  written_off: 'red',
  in_flight: 'blue',
  resolved: 'green',
  dismissed: 'gray',
  running: 'blue',
  completed: 'green',
  scheduled: 'blue',
  sending: 'blue',
  sent: 'green',
  suppressed: 'gray',
  disabled: 'gray',
  delivering: 'blue',
  retrying: 'orange',
  exhausted: 'red',
  replayed: 'green',
  discarded: 'gray',
  accepted: 'green',
  revoked: 'gray',
  expired: 'gray',
  used: 'green',
  removed: 'gray',
  applied: 'green',
  ended: 'gray',
  late: 'orange',
  closed: 'gray',
};

export function statusColor(status: string | null | undefined): string {
  return (status && statusColors[status]) || 'gray';
}
