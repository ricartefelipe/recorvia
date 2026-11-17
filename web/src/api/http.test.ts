import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { server } from '../test/server';
import { ApiError, createHttpClient, loginUrl, NetworkError, redirectToLogin, resetRedirectGuard } from './http';

function setup() {
  const onUnauthorized = vi.fn();
  const onTenantMismatch = vi.fn();
  const client = createHttpClient({
    getCsrfToken: () => 'csrf-abc',
    getTenantId: () => 'tenant-1',
    onUnauthorized,
    onTenantMismatch,
  });
  return { client, onUnauthorized, onTenantMismatch };
}

function capture(method: 'get' | 'post' | 'patch' | 'put' | 'delete', path: string, response: () => Response) {
  const requests: Request[] = [];
  server.use(
    http[method](path, ({ request }) => {
      requests.push(request.clone());
      return response();
    }),
  );
  return requests;
}

describe('cliente HTTP', () => {
  afterEach(() => resetRedirectGuard());

  it('GET envia a organização ativa, não envia CSRF e ignora filtros vazios', async () => {
    const { client } = setup();
    const requests = capture('get', '/api/v1/customers', () => HttpResponse.json({ data: [] }));
    await client.get('/api/v1/customers', { query: { q: 'acme', status: '', cursor: undefined, limit: 25 } });
    const [req] = requests;
    expect(req.headers.get('X-Recorvia-Tenant')).toBe('tenant-1');
    expect(req.headers.get('X-CSRF-Token')).toBeNull();
    const url = new URL(req.url);
    expect(url.searchParams.get('q')).toBe('acme');
    expect(url.searchParams.get('limit')).toBe('25');
    expect(url.searchParams.has('status')).toBe(false);
    expect(url.searchParams.has('cursor')).toBe(false);
  });

  it('métodos que alteram estado enviam CSRF, If-Match entre aspas e Idempotency-Key', async () => {
    const { client } = setup();
    const requests = capture('post', '/api/v1/invoices/abc/payments', () => HttpResponse.json({ id: 'p1' }, { status: 201 }));
    const result = await client.post<{ id: string }>(
      '/api/v1/invoices/abc/payments',
      { amount: 1000, method: 'pix' },
      { idempotencyKey: 'chave-0001', ifMatch: 3 },
    );
    expect(result).toEqual({ id: 'p1' });
    const [req] = requests;
    expect(req.headers.get('X-CSRF-Token')).toBe('csrf-abc');
    expect(req.headers.get('X-Recorvia-Tenant')).toBe('tenant-1');
    expect(req.headers.get('If-Match')).toBe('"3"');
    expect(req.headers.get('Idempotency-Key')).toBe('chave-0001');
    expect(req.headers.get('Content-Type')).toBe('application/json');
    expect(await req.json()).toEqual({ amount: 1000, method: 'pix' });
  });

  it('omite o header de organização quando solicitado (endpoints da sessão)', async () => {
    const { client } = setup();
    const requests = capture('put', '/api/v1/session/tenant', () => HttpResponse.json({}));
    await client.put('/api/v1/session/tenant', { tenant_id: 't2' }, { tenant: false });
    expect(requests[0].headers.get('X-Recorvia-Tenant')).toBeNull();
    expect(requests[0].headers.get('X-CSRF-Token')).toBe('csrf-abc');
  });

  it('interpreta application/problem+json com erros de campo e request_id', async () => {
    const { client } = setup();
    capture('post', '/api/v1/customers', () =>
      HttpResponse.json(
        {
          type: 'about:blank',
          title: 'Dados inválidos',
          status: 422,
          code: 'validation_failed',
          detail: 'Há campos inválidos.',
          request_id: 'req-42',
          errors: [
            { field: 'document_number', message: 'CPF inválido' },
            { field: 'email', message: 'E-mail inválido' },
          ],
        },
        { status: 422, headers: { 'Content-Type': 'application/problem+json' } },
      ),
    );
    const error = await client.post('/api/v1/customers', {}).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ApiError);
    const apiError = error as ApiError;
    expect(apiError.status).toBe(422);
    expect(apiError.code).toBe('validation_failed');
    expect(apiError.detail).toBe('Há campos inválidos.');
    expect(apiError.requestId).toBe('req-42');
    expect(apiError.fieldErrors).toEqual([
      { field: 'document_number', message: 'CPF inválido' },
      { field: 'email', message: 'E-mail inválido' },
    ]);
  });

  it('401 aciona o redirecionamento para o login', async () => {
    const { client, onUnauthorized } = setup();
    capture('get', '/api/v1/session', () =>
      HttpResponse.json({ type: 'about:blank', title: 'Não autenticado', status: 401, code: 'unauthenticated' }, { status: 401 }),
    );
    await expect(client.get('/api/v1/session')).rejects.toMatchObject({ status: 401 });
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
  });

  it('409 tenant_mismatch aciona a recarga da sessão', async () => {
    const { client, onTenantMismatch, onUnauthorized } = setup();
    capture('get', '/api/v1/invoices', () =>
      HttpResponse.json({ type: 'about:blank', title: 'Conflito', status: 409, code: 'tenant_mismatch' }, { status: 409 }),
    );
    await expect(client.get('/api/v1/invoices')).rejects.toMatchObject({ code: 'tenant_mismatch' });
    expect(onTenantMismatch).toHaveBeenCalledTimes(1);
    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it('erro sem corpo JSON vira ApiError genérico; 204 devolve undefined', async () => {
    const { client } = setup();
    capture('get', '/api/v1/dashboard', () => new HttpResponse('falha', { status: 502, headers: { 'X-Request-ID': 'req-9' } }));
    const error = (await client.get('/api/v1/dashboard').catch((e: unknown) => e)) as ApiError;
    expect(error.status).toBe(502);
    expect(error.isServerError).toBe(true);
    expect(error.requestId).toBe('req-9');

    capture('delete', '/api/v1/members/u1', () => new HttpResponse(null, { status: 204 }));
    await expect(client.delete('/api/v1/members/u1')).resolves.toBeUndefined();
  });

  it('falha de rede vira NetworkError', async () => {
    const { client } = setup();
    server.use(http.get('/api/v1/plans', () => HttpResponse.error()));
    await expect(client.get('/api/v1/plans')).rejects.toBeInstanceOf(NetworkError);
  });

  it('redireciona o navegador inteiro para /auth/login com a rota atual', () => {
    const assign = vi.fn();
    redirectToLogin({ pathname: '/faturas', search: '?status=open', hash: '', assign });
    expect(assign).toHaveBeenCalledWith('/auth/login?return_to=%2Ffaturas%3Fstatus%3Dopen');
    redirectToLogin({ pathname: '/outra', search: '', hash: '', assign });
    expect(assign).toHaveBeenCalledTimes(1);
    expect(loginUrl('https://externo.test/x')).toBe('/auth/login?return_to=%2F');
  });
});
