import type { QueryClient } from '@tanstack/react-query';
import { api, applySessionToClient } from '../api/client';
import { SESSION_KEY } from '../api/queryKeys';
import type { OrganizationInput, Role, Session } from '../api/types';

/** Carrega a sessão e atualiza CSRF e organização usados pelo cliente HTTP. */
export async function fetchSession(): Promise<Session> {
  const session = await api.get<Session>('/api/v1/session', { tenant: false });
  applySessionToClient(session);
  return session;
}

/** Recarrega a sessão do zero (sem cache). */
export function reloadSession(queryClient: QueryClient): Promise<Session> {
  return queryClient.fetchQuery({ queryKey: SESSION_KEY, queryFn: fetchSession, staleTime: 0 });
}

/**
 * Troca a organização ativa: PUT na sessão, cancelamento e limpeza de todo o
 * cache e recarga da sessão. A navegação para o dashboard fica com quem chama.
 */
export async function performTenantSwitch(queryClient: QueryClient, tenantId: string): Promise<Session> {
  await api.put<Session>('/api/v1/session/tenant', { tenant_id: tenantId }, { tenant: false });
  await queryClient.cancelQueries();
  queryClient.clear();
  return reloadSession(queryClient);
}

/** 409 tenant_mismatch: a sessão foi trocada em outra aba. */
export async function recoverFromTenantMismatch(queryClient: QueryClient): Promise<Session> {
  await queryClient.cancelQueries();
  queryClient.clear();
  return reloadSession(queryClient);
}

/** Onboarding: cria a organização; a sessão volta rotacionada com ela ativa. */
export async function createOrganization(queryClient: QueryClient, input: OrganizationInput): Promise<Session> {
  await api.post<Session>('/api/v1/organizations', input, { tenant: false });
  await queryClient.cancelQueries();
  queryClient.clear();
  return reloadSession(queryClient);
}

export function acceptInvitation(token: string): Promise<{ tenant_id: string; role: Role }> {
  return api.post('/api/v1/invitations/accept', { token }, { tenant: false });
}

/** Encerra a sessão e segue para o endereço indicado pelo backend. */
export async function logout(assign: (url: string) => void = (url) => window.location.assign(url)): Promise<void> {
  const result = await api.post<{ redirect_url: string }>('/auth/logout', undefined, { tenant: false });
  applySessionToClient(null);
  assign(result?.redirect_url || '/entrar');
}

/**
 * Encerra todas as sessões do usuário, em todos os navegadores, e segue para
 * o encerramento da sessão no provedor indicado pelo backend.
 */
export async function logoutAll(assign: (url: string) => void = (url) => window.location.assign(url)): Promise<void> {
  const result = await api.post<{ redirect_url: string }>('/auth/logout-all', undefined, { tenant: false });
  applySessionToClient(null);
  assign(result?.redirect_url || '/entrar');
}
