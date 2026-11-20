import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { api, applySessionToClient } from '../api/client';
import { createQueryKeys, isKeyInScope, SESSION_KEY } from '../api/queryKeys';
import type { Session } from '../api/types';
import { makeSession, TENANT_A, TENANT_B, USER_ID } from '../test/fixtures';
import { createTestQueryClient } from '../test/render';
import { server } from '../test/server';
import { performTenantSwitch, recoverFromTenantMismatch } from './sessionApi';

describe('fábrica de chaves', () => {
  it('prefixa toda chave com usuário e organização', () => {
    const keysA = createQueryKeys({ userId: USER_ID, tenantId: TENANT_A });
    const keysB = createQueryKeys({ userId: USER_ID, tenantId: TENANT_B });
    expect(keysA('customers', 'list', { q: 'x' })).toEqual([USER_ID, TENANT_A, 'customers', 'list', { q: 'x' }]);
    expect(keysA('invoices', 'detail', '1')).not.toEqual(keysB('invoices', 'detail', '1'));
    expect(isKeyInScope(keysA('dashboard'), { userId: USER_ID, tenantId: TENANT_A })).toBe(true);
    expect(isKeyInScope(keysA('dashboard'), { userId: USER_ID, tenantId: TENANT_B })).toBe(false);
  });
});

describe('troca de organização', () => {
  it('cancela consultas, limpa todo o cache e recarrega a sessão', async () => {
    const queryClient = createTestQueryClient();
    const sessionA = makeSession({ tenantId: TENANT_A, csrf: 'csrf-a' });
    const sessionB = makeSession({ tenantId: TENANT_B, csrf: 'csrf-b' });
    applySessionToClient(sessionA);
    const keysA = createQueryKeys({ userId: USER_ID, tenantId: TENANT_A });
    queryClient.setQueryData(SESSION_KEY, sessionA);
    queryClient.setQueryData(keysA('customers', 'list', {}), { data: [{ id: 'cliente-da-acme' }] });

    const putRequests: Request[] = [];
    server.use(
      http.put('/api/v1/session/tenant', async ({ request }) => {
        putRequests.push(request.clone());
        return HttpResponse.json(sessionB);
      }),
      http.get('/api/v1/session', () => HttpResponse.json(sessionB)),
    );

    const cancel = vi.spyOn(queryClient, 'cancelQueries');
    const clear = vi.spyOn(queryClient, 'clear');

    const result = await performTenantSwitch(queryClient, TENANT_B);

    expect(result.active_tenant?.id).toBe(TENANT_B);
    expect(cancel).toHaveBeenCalled();
    expect(clear).toHaveBeenCalled();
    expect(cancel.mock.invocationCallOrder[0]).toBeLessThan(clear.mock.invocationCallOrder[0]);
    expect(queryClient.getQueryData(keysA('customers', 'list', {}))).toBeUndefined();
    expect(queryClient.getQueryData<Session>(SESSION_KEY)?.active_tenant?.id).toBe(TENANT_B);

    const [put] = putRequests;
    expect(await put.json()).toEqual({ tenant_id: TENANT_B });
    expect(put.headers.get('X-CSRF-Token')).toBe('csrf-a');

    // Chamadas seguintes usam a nova organização e o novo CSRF.
    const next: Request[] = [];
    server.use(
      http.post('/api/v1/customers', ({ request }) => {
        next.push(request.clone());
        return HttpResponse.json({}, { status: 201 });
      }),
    );
    await api.post('/api/v1/customers', {});
    expect(next[0].headers.get('X-Recorvia-Tenant')).toBe(TENANT_B);
    expect(next[0].headers.get('X-CSRF-Token')).toBe('csrf-b');
  });

  it('409 tenant_mismatch: limpa o cache e recarrega a sessão', async () => {
    const queryClient = createTestQueryClient();
    const keysA = createQueryKeys({ userId: USER_ID, tenantId: TENANT_A });
    queryClient.setQueryData(keysA('invoices', 'list', {}), { data: [{ id: 'fatura-da-acme' }] });
    server.use(http.get('/api/v1/session', () => HttpResponse.json(makeSession({ tenantId: TENANT_B }))));

    const session = await recoverFromTenantMismatch(queryClient);

    expect(session.active_tenant?.id).toBe(TENANT_B);
    expect(queryClient.getQueryData(keysA('invoices', 'list', {}))).toBeUndefined();
  });
});
