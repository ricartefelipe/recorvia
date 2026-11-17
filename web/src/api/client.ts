import { createHttpClient, redirectToLogin, type ApiError } from './http';
import type { Session } from './types';

/**
 * Contexto da sessão usado pelo cliente HTTP da aplicação. É atualizado
 * sempre que a sessão é carregada, antes de qualquer consulta da organização.
 */
const sessionContext: { csrfToken?: string; tenantId?: string } = {};

export function applySessionToClient(session: Pick<Session, 'csrf_token' | 'active_tenant'> | null): void {
  sessionContext.csrfToken = session?.csrf_token;
  sessionContext.tenantId = session?.active_tenant?.id ?? undefined;
}

export function currentClientTenant(): string | undefined {
  return sessionContext.tenantId;
}

type Handler = (error: ApiError) => void;
let tenantMismatchHandler: Handler | undefined;

export function setTenantMismatchHandler(handler: Handler | undefined): void {
  tenantMismatchHandler = handler;
}

/** Cliente da área interna (/api/v1). */
export const api = createHttpClient({
  getCsrfToken: () => sessionContext.csrfToken,
  getTenantId: () => sessionContext.tenantId,
  onUnauthorized: () => redirectToLogin(),
  onTenantMismatch: (error) => tenantMismatchHandler?.(error),
});

/** Contexto separado do portal do cliente (/api/portal/v1). */
const portalContext: { csrfToken?: string } = {};
let portalUnauthorizedHandler: Handler | undefined;

export function setPortalCsrfToken(token: string | undefined): void {
  portalContext.csrfToken = token;
}

export function setPortalUnauthorizedHandler(handler: Handler | undefined): void {
  portalUnauthorizedHandler = handler;
}

/** Cliente do portal: sem organização no header e sem redirecionar para o login interno. */
export const portalApi = createHttpClient({
  getCsrfToken: () => portalContext.csrfToken,
  onUnauthorized: (error) => portalUnauthorizedHandler?.(error),
});
