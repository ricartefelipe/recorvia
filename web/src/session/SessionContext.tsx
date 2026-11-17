import { useQueryClient } from '@tanstack/react-query';
import { createContext, useContext, useEffect, useLayoutEffect, useMemo, useRef, type ReactNode } from 'react';
import { applySessionToClient } from '../api/client';
import { createQueryKeys, SESSION_KEY, type KeyFactory } from '../api/queryKeys';
import type { ActiveTenant, Session } from '../api/types';
import type { Permission } from './permissions';

export interface SessionContextValue {
  session: Session;
  user: Session['user'];
  tenant: ActiveTenant | null;
  can: (permission: Permission) => boolean;
  /** Fábrica de chaves do escopo usuário + organização (null sem organização ativa). */
  keys: KeyFactory | null;
}

const SessionContext = createContext<SessionContextValue | null>(null);

export function buildSessionValue(session: Session): SessionContextValue {
  const tenant = session.active_tenant ?? null;
  const permissions = new Set(tenant?.permissions ?? []);
  return {
    session,
    user: session.user,
    tenant,
    can: (permission) => permissions.has(permission),
    keys: tenant ? createQueryKeys({ userId: session.user.id, tenantId: tenant.id }) : null,
  };
}

export function SessionProvider({ session, children }: { session: Session; children: ReactNode }) {
  const queryClient = useQueryClient();
  const tenantId = session.active_tenant?.id;
  const previousTenant = useRef(tenantId);

  useLayoutEffect(() => {
    applySessionToClient(session);
  }, [session]);

  // Sessão recarregada com outra organização (por exemplo, troca em outra aba):
  // descarta dados de qualquer outro escopo.
  useEffect(() => {
    if (previousTenant.current !== tenantId) {
      previousTenant.current = tenantId;
      queryClient.removeQueries({
        predicate: (q) =>
          q.queryKey[0] !== SESSION_KEY[0] && (q.queryKey[0] !== session.user.id || q.queryKey[1] !== tenantId),
      });
    }
  }, [queryClient, tenantId, session.user.id]);

  const value = useMemo(() => buildSessionValue(session), [session]);
  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionContextValue {
  const ctx = useContext(SessionContext);
  if (!ctx) throw new Error('useSession fora de SessionProvider');
  return ctx;
}

export interface TenantContextValue extends SessionContextValue {
  tenant: ActiveTenant;
  keys: KeyFactory;
  timeZone: string;
}

/** Contexto com organização ativa garantida (rotas internas). */
export function useTenant(): TenantContextValue {
  const ctx = useSession();
  if (!ctx.tenant || !ctx.keys) throw new Error('Nenhuma organização ativa');
  return { ...ctx, tenant: ctx.tenant, keys: ctx.keys, timeZone: ctx.tenant.timezone || 'America/Sao_Paulo' };
}

export function usePermission(permission: Permission): boolean {
  return useSession().can(permission);
}

/** Renderiza o conteúdo apenas com a permissão; o backend continua validando. */
export function Can({
  permission,
  children,
  fallback = null,
}: {
  permission: Permission | Permission[];
  children: ReactNode;
  fallback?: ReactNode;
}) {
  const { can } = useSession();
  const list = Array.isArray(permission) ? permission : [permission];
  return <>{list.every(can) ? children : fallback}</>;
}
