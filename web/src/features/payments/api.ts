import { api } from '../../api/client';
import type {
  Charge,
  ChargePage,
  ChargeStatus,
  CursorPage,
  DataList,
  Dispute,
  Payment,
  PaymentOperation,
  RecoverySettlement,
  RecoverySettlementInput,
  Refund,
} from '../../api/types';

const enc = encodeURIComponent;

export type PaymentOperationStatus = PaymentOperation['status'];

export const paymentsApi = {
  listCharges: (params: { status?: ChargeStatus; invoice_id?: string; cursor?: string; limit?: number }, signal?: AbortSignal) =>
    api.get<ChargePage>('/api/v1/charges', { query: { ...params }, signal }),
  getCharge: (id: string, signal?: AbortSignal) => api.get<Charge>(`/api/v1/charges/${enc(id)}`, { signal }),
  refund: (id: string, input: { amount: number; reason: string }, idempotencyKey: string) =>
    api.post<Refund>(`/api/v1/charges/${enc(id)}/refunds`, input, { idempotencyKey }),
  listRefunds: (params: { cursor?: string; limit?: number }, signal?: AbortSignal) =>
    api.get<CursorPage<Refund>>('/api/v1/refunds', { query: { ...params }, signal }),
  listPayments: (params: { invoice_id?: string; cursor?: string; limit?: number }, signal?: AbortSignal) =>
    api.get<CursorPage<Payment>>('/api/v1/payments', { query: { ...params }, signal }),
  listDisputes: (signal?: AbortSignal) => api.get<DataList<Dispute>>('/api/v1/disputes', { signal }),
  getDispute: (id: string, signal?: AbortSignal) => api.get<Dispute>(`/api/v1/disputes/${enc(id)}`, { signal }),
  listRecoverySettlements: (disputeId: string, signal?: AbortSignal) =>
    api.get<DataList<RecoverySettlement>>(`/api/v1/disputes/${enc(disputeId)}/recovery-settlements`, { signal }),
  settleRecovery: (disputeId: string, input: RecoverySettlementInput, idempotencyKey: string) =>
    api.post<RecoverySettlement>(`/api/v1/disputes/${enc(disputeId)}/recovery-settlements`, input, { idempotencyKey }),
  listOperations: (status: PaymentOperationStatus | undefined, signal?: AbortSignal) =>
    api.get<DataList<PaymentOperation>>('/api/v1/payment-operations', { query: { status }, signal }),
  retryOperation: (id: string) => api.post<PaymentOperation>(`/api/v1/payment-operations/${enc(id)}/retry`),
};

/** Valor ainda reembolsável de uma cobrança. */
export function refundableAmount(charge: Charge): number {
  if (typeof charge.refundable_amount === 'number') return charge.refundable_amount;
  return Math.max(0, charge.amount_paid - charge.amount_refunded - charge.amount_charged_back);
}

/**
 * Situações que aceitam pedido de reembolso (ADR-034). A cobrança em disputa
 * mantém o valor reembolsável, mas a API recusa o pedido até a decisão.
 */
export const REFUNDABLE_STATUSES: readonly ChargeStatus[] = ['paid', 'partially_refunded'];

/** Indica se a cobrança aceita um novo pedido de reembolso. */
export function canRequestRefund(charge: Charge): boolean {
  return REFUNDABLE_STATUSES.includes(charge.status) && refundableAmount(charge) > 0;
}
