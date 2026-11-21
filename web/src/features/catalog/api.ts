import { useQuery } from '@tanstack/react-query';
import { api } from '../../api/client';
import type {
  Coupon,
  CouponInput,
  DataList,
  Metric,
  MetricInput,
  Plan,
  PlanDetails,
  PlanInput,
  PlanUpdate,
  PriceInput,
  PriceVersion,
  Product,
  ProductInput,
} from '../../api/types';
import { useTenant } from '../../session/SessionContext';

const enc = encodeURIComponent;
type Status = 'active' | 'archived';

export const catalogApi = {
  listProducts: (status: Status | undefined, signal?: AbortSignal) =>
    api.get<DataList<Product>>('/api/v1/products', { query: { status }, signal }),
  createProduct: (input: ProductInput) => api.post<Product>('/api/v1/products', input),
  updateProduct: (id: string, version: number, input: ProductInput & { status: Status }) =>
    api.patch<Product>(`/api/v1/products/${enc(id)}`, input, { ifMatch: version }),

  listMetrics: (signal?: AbortSignal) => api.get<DataList<Metric>>('/api/v1/metrics', { signal }),
  createMetric: (input: MetricInput) => api.post<Metric>('/api/v1/metrics', input),

  listPlans: (params: { status?: Status; product_id?: string }, signal?: AbortSignal) =>
    api.get<DataList<Plan>>('/api/v1/plans', { query: params, signal }),
  getPlan: (id: string, signal?: AbortSignal) => api.get<PlanDetails>(`/api/v1/plans/${enc(id)}`, { signal }),
  createPlan: (input: PlanInput) => api.post<PlanDetails>('/api/v1/plans', input),
  updatePlan: (id: string, version: number, input: PlanUpdate) =>
    api.patch<PlanDetails>(`/api/v1/plans/${enc(id)}`, input, { ifMatch: version }),
  /** A nova versão de preço altera o plano: If-Match é a versão do plano. */
  addPriceVersion: (id: string, version: number, input: PriceInput) =>
    api.post<PriceVersion>(`/api/v1/plans/${enc(id)}/prices`, input, { ifMatch: version }),

  listCoupons: (signal?: AbortSignal) => api.get<DataList<Coupon>>('/api/v1/coupons', { signal }),
  createCoupon: (input: CouponInput) => api.post<Coupon>('/api/v1/coupons', input),
  archiveCoupon: (id: string, version: number) => api.post<Coupon>(`/api/v1/coupons/${enc(id)}/archive`, undefined, { ifMatch: version }),
};

export function useMetrics() {
  const { keys } = useTenant();
  return useQuery({ queryKey: keys('metrics', 'list'), queryFn: ({ signal }) => catalogApi.listMetrics(signal) });
}

export function useProducts(status?: Status) {
  const { keys } = useTenant();
  return useQuery({
    queryKey: keys('products', 'list', status ?? 'all'),
    queryFn: ({ signal }) => catalogApi.listProducts(status, signal),
  });
}

export function usePlans(params: { status?: Status; product_id?: string } = {}) {
  const { keys } = useTenant();
  return useQuery({ queryKey: keys('plans', 'list', params), queryFn: ({ signal }) => catalogApi.listPlans(params, signal) });
}
