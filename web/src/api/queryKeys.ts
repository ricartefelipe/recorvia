import type { QueryClient } from '@tanstack/react-query';

/**
 * Fábrica de chaves de consulta. Toda consulta de dados de organização começa
 * com [userId, tenantId], de modo que dados de uma organização nunca sejam
 * servidos do cache para outra.
 */
export interface KeyScope {
  userId: string;
  tenantId: string;
}

export type QueryDomain =
  | 'organization'
  | 'dashboard'
  | 'customers'
  | 'products'
  | 'metrics'
  | 'plans'
  | 'coupons'
  | 'subscriptions'
  | 'usage'
  | 'invoices'
  | 'charges'
  | 'payments'
  | 'refunds'
  | 'disputes'
  | 'payment-operations'
  | 'payment-providers'
  | 'ledger'
  | 'reconciliation'
  | 'collection-policies'
  | 'notification-templates'
  | 'notifications'
  | 'members'
  | 'invitations'
  | 'roles'
  | 'api-credentials'
  | 'webhooks'
  | 'audit'
  | 'dead-letters'
  | 'platform';

export const SESSION_KEY = ['session'] as const;
export const PORTAL_SESSION_KEY = ['portal', 'session'] as const;

export type TenantQueryKey = readonly [string, string, QueryDomain, ...unknown[]];
export type KeyFactory = (domain: QueryDomain, ...rest: unknown[]) => TenantQueryKey;

export function createQueryKeys(scope: KeyScope): KeyFactory {
  return (domain, ...rest) => [scope.userId, scope.tenantId, domain, ...rest] as const;
}

export function portalKey(customerId: string, ...rest: unknown[]): readonly unknown[] {
  return ['portal', customerId, ...rest];
}

/** Indica se a chave pertence ao escopo (usuário + organização). */
export function isKeyInScope(key: readonly unknown[], scope: KeyScope): boolean {
  return key[0] === scope.userId && key[1] === scope.tenantId;
}

/** Remove do cache tudo que não é a própria sessão. */
export function removeTenantData(queryClient: QueryClient): void {
  queryClient.removeQueries({ predicate: (q) => q.queryKey[0] !== SESSION_KEY[0] });
}
