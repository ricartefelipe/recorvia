import { useQuery } from '@tanstack/react-query';
import { api } from '../../api/client';
import type {
  Coupon,
  CouponInput,
  CursorPage,
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
import { byName, fetchAllPages, type PageRequest } from '../../lib/pages';
import { useTenant } from '../../session/SessionContext';

const enc = encodeURIComponent;
type Status = 'active' | 'archived';

/** Listas do catálogo: paginadas por cursor (ADR-085). */
export const catalogApi = {
  listProducts: (params: { status?: Status } & PageRequest, signal?: AbortSignal) =>
    api.get<CursorPage<Product>>('/api/v1/products', { query: { ...params }, signal }),
  createProduct: (input: ProductInput) => api.post<Product>('/api/v1/products', input),
  updateProduct: (id: string, version: number, input: ProductInput & { status: Status }) =>
    api.patch<Product>(`/api/v1/products/${enc(id)}`, input, { ifMatch: version }),

  listMetrics: (params: PageRequest, signal?: AbortSignal) => api.get<CursorPage<Metric>>('/api/v1/metrics', { query: { ...params }, signal }),
  createMetric: (input: MetricInput) => api.post<Metric>('/api/v1/metrics', input),

  listPlans: (params: { status?: Status; product_id?: string } & PageRequest, signal?: AbortSignal) =>
    api.get<CursorPage<Plan>>('/api/v1/plans', { query: { ...params }, signal }),
  getPlan: (id: string, signal?: AbortSignal) => api.get<PlanDetails>(`/api/v1/plans/${enc(id)}`, { signal }),
  createPlan: (input: PlanInput) => api.post<PlanDetails>('/api/v1/plans', input),
  updatePlan: (id: string, version: number, input: PlanUpdate) =>
    api.patch<PlanDetails>(`/api/v1/plans/${enc(id)}`, input, { ifMatch: version }),
  /** A nova versão de preço altera o plano: If-Match é a versão do plano. */
  addPriceVersion: (id: string, version: number, input: PriceInput) =>
    api.post<PriceVersion>(`/api/v1/plans/${enc(id)}/prices`, input, { ifMatch: version }),

  listCoupons: (params: PageRequest, signal?: AbortSignal) => api.get<CursorPage<Coupon>>('/api/v1/coupons', { query: { ...params }, signal }),
  createCoupon: (input: CouponInput) => api.post<Coupon>('/api/v1/coupons', input),
  archiveCoupon: (id: string, version: number) => api.post<Coupon>(`/api/v1/coupons/${enc(id)}/archive`, undefined, { ifMatch: version }),
};

/*
 * Listas inteiras para campos de seleção e consultas por identificador
 * (nome do produto, descrição do preço com a métrica): percorrem todas as
 * páginas pelo next_cursor. As telas de listagem paginam com CursorPager.
 */

/** Todas as métricas, pelo código. */
export function useMetrics() {
  const { keys } = useTenant();
  return useQuery({
    queryKey: keys('metrics', 'all'),
    queryFn: async ({ signal }) => ({ data: await fetchAllPages((page) => catalogApi.listMetrics(page, signal)) }),
  });
}

/** Todos os produtos (opcionalmente só os de uma situação), pelo nome. */
export function useProducts(status?: Status) {
  const { keys } = useTenant();
  return useQuery({
    queryKey: keys('products', 'all', status ?? 'all'),
    queryFn: async ({ signal }) => ({ data: byName(await fetchAllPages((page) => catalogApi.listProducts({ status, ...page }, signal))) }),
  });
}

/** Todos os planos dos filtros informados, pelo nome. */
export function usePlans(params: { status?: Status; product_id?: string } = {}) {
  const { keys } = useTenant();
  return useQuery({
    queryKey: keys('plans', 'all', params),
    queryFn: async ({ signal }) => ({ data: byName(await fetchAllPages((page) => catalogApi.listPlans({ ...params, ...page }, signal))) }),
  });
}
