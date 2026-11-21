import { api } from '../../api/client';
import type {
  ChangeTiming,
  DataList,
  PriceAdjustment,
  PriceAdjustmentInput,
  Subscription,
  SubscriptionDiscount,
  SubscriptionHistoryEntry,
  SubscriptionInput,
  SubscriptionPage,
  SubscriptionStatus,
  SubscriptionUsage,
  UsageEventPage,
} from '../../api/types';

const enc = encodeURIComponent;
const base = (id: string) => `/api/v1/subscriptions/${enc(id)}`;

export interface SubscriptionListParams {
  status?: SubscriptionStatus;
  /** Somente em trial, ativas ou em atraso, a base das assinaturas ativas do painel. */
  in_force?: boolean;
  customer_id?: string;
  cursor?: string;
  limit?: number;
}

export const subscriptionsApi = {
  list: (params: SubscriptionListParams, signal?: AbortSignal) =>
    api.get<SubscriptionPage>('/api/v1/subscriptions', { query: { ...params }, signal }),
  get: (id: string, signal?: AbortSignal) => api.get<Subscription>(base(id), { signal }),
  create: (input: SubscriptionInput, idempotencyKey: string) =>
    api.post<Subscription>('/api/v1/subscriptions', input, { idempotencyKey }),
  history: (id: string, signal?: AbortSignal) =>
    api.get<DataList<SubscriptionHistoryEntry>>(`${base(id)}/history`, { signal }),
  cancel: (s: Subscription, input: { mode: 'immediate' | 'period_end'; reason?: string }) =>
    api.post<Subscription>(`${base(s.id)}/cancel`, input, { ifMatch: s.version }),
  resumeRenewal: (s: Subscription) => api.post<Subscription>(`${base(s.id)}/resume-renewal`, undefined, { ifMatch: s.version }),
  suspend: (s: Subscription, input: { reason?: string }) =>
    api.post<Subscription>(`${base(s.id)}/suspend`, input, { ifMatch: s.version }),
  reactivate: (s: Subscription) => api.post<Subscription>(`${base(s.id)}/reactivate`, undefined, { ifMatch: s.version }),
  changePlan: (s: Subscription, input: { plan_id: string; quantity?: number; timing: ChangeTiming }) =>
    api.post<Subscription>(`${base(s.id)}/change-plan`, input, { ifMatch: s.version }),
  changeQuantity: (s: Subscription, input: { quantity: number; timing: ChangeTiming }) =>
    api.post<Subscription>(`${base(s.id)}/change-quantity`, input, { ifMatch: s.version }),
  listAdjustments: (id: string, signal?: AbortSignal) =>
    api.get<DataList<PriceAdjustment>>(`${base(id)}/price-adjustments`, { signal }),
  scheduleAdjustment: (id: string, input: PriceAdjustmentInput, idempotencyKey: string) =>
    api.post<PriceAdjustment>(`${base(id)}/price-adjustments`, input, { idempotencyKey }),
  cancelAdjustment: (id: string, adjustmentId: string) => api.delete(`${base(id)}/price-adjustments/${enc(adjustmentId)}`),
  applyCoupon: (id: string, couponCode: string, idempotencyKey: string) =>
    api.post<SubscriptionDiscount>(`${base(id)}/discounts`, { coupon_code: couponCode }, { idempotencyKey }),
  removeDiscount: (id: string) => api.delete(`${base(id)}/discounts`),
  usage: (id: string, signal?: AbortSignal) => api.get<SubscriptionUsage>(`${base(id)}/usage`, { signal }),
  usageEvents: (params: { subscription_id?: string; metric_id?: string; cursor?: string; limit?: number }, signal?: AbortSignal) =>
    api.get<UsageEventPage>('/api/v1/usage-events', { query: { ...params }, signal }),
};
