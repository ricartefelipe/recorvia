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
  trialing: 'Em avaliação',
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

export const paymentOperationStatusLabels: Labels = {
  pending: 'Pendente',
  in_flight: 'Em andamento',
  succeeded: 'Concluída',
  failed: 'Falhou',
  unknown: 'Resultado desconhecido',
};

export const paymentOperationKindLabels: Labels = { create_charge: 'Criação de cobrança', refund: 'Reembolso' };

export const creditKindLabels: Labels = {
  grant: 'Concessão',
  consumption: 'Uso em fatura',
  return: 'Devolução',
  overpayment: 'Pagamento a maior',
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
};

export const reconciliationRunStatusLabels: Labels = {
  running: 'Em execução',
  completed: 'Concluída',
  failed: 'Falhou',
};

export const templateKeyLabels: Labels = {
  invoice_issued: 'Fatura emitida',
  reminder_before_due: 'Lembrete antes do vencimento',
  reminder_after_due: 'Lembrete após o vencimento',
  payment_confirmed: 'Pagamento confirmado',
  subscription_suspended: 'Assinatura suspensa',
};

export const notificationStatusLabels: Labels = {
  scheduled: 'Agendada',
  sending: 'Enviando',
  sent: 'Enviada',
  failed: 'Falhou',
  canceled: 'Cancelada',
  suppressed: 'Suprimida',
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
  trialing: 'Em avaliação',
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
  removed: 'gray',
  applied: 'green',
  ended: 'gray',
  late: 'orange',
  closed: 'gray',
};

export function statusColor(status: string | null | undefined): string {
  return (status && statusColors[status]) || 'gray';
}
