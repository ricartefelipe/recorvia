import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { ApiCredential } from '../../api/types';
import { AUDITOR_PERMISSIONS, makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { CredentialsTab, grantableScopes } from './CredentialsTab';

const active: ApiCredential = {
  id: 'cred-1',
  name: 'ERP interno',
  prefix: 'rcv_abcdefghijklmnop',
  scopes: ['customers.read', 'usage.write'],
  created_at: '2026-11-01T12:00:00Z',
  expires_at: '2099-11-01T12:00:00Z',
  last_used_at: null,
  revoked_at: null,
  version: 2,
};

describe('credenciais de API', () => {
  it('oferece como escopos apenas permissões delegáveis do próprio papel', () => {
    expect(
      grantableScopes(['usage.write', 'members.manage', 'customers.read', 'api_credentials.manage', 'platform_billing.manage', 'customers.anonymize']),
    ).toEqual([
      'customers.read',
      'usage.write',
    ]);
  });

  it('lista sem segredos e exibe o segredo da rotação uma única vez', async () => {
    const rotated: string[] = [];
    const versions: (string | null)[] = [];
    server.use(
      http.get('/api/v1/api-credentials', () =>
        HttpResponse.json({
          data: [
            active,
            { ...active, id: 'cred-2', name: 'CRM antigo', prefix: 'rcv_qrstuvwxyz234567', revoked_at: '2026-11-10T12:00:00Z' },
            { ...active, id: 'cred-3', name: 'Relatórios', prefix: 'rcv_aaaaaaaaaaaaaaaa', expires_at: '2026-01-01T00:00:00Z' },
          ],
        }),
      ),
      http.post('/api/v1/api-credentials/:id/rotate', ({ params, request }) => {
        rotated.push(String(params.id));
        versions.push(request.headers.get('If-Match'));
        return HttpResponse.json({ credential: { ...active, version: 3 }, secret: `${active.prefix}_${'a'.repeat(52)}` }, { status: 201, headers: { ETag: '"3"' } });
      }),
    );
    const { user } = renderPage(<CredentialsTab />);
    const table = await screen.findByRole('table', { name: 'Credenciais de API' });
    expect(within(table).getByText('rcv_abcdefghijklmnop…')).toBeInTheDocument();
    expect(within(table).getByText('Revogada')).toBeInTheDocument();
    expect(within(table).getByText('Expirada')).toBeInTheDocument();
    // Revogada não tem ações; expirada só pode ser revogada.
    expect(screen.queryByRole('button', { name: 'Revogar credencial CRM antigo' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Rotacionar credencial Relatórios' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Revogar credencial Relatórios' })).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Rotacionar credencial ERP interno' }));
    await user.click(await screen.findByRole('button', { name: 'Rotacionar' }));
    expect(await screen.findByLabelText('Segredo da credencial')).toHaveValue(`${active.prefix}_${'a'.repeat(52)}`);
    expect(rotated).toEqual(['cred-1']);
    expect(versions).toEqual(['"2"']);
    await user.click(screen.getByRole('button', { name: 'Já copiei, fechar' }));
    await waitFor(() => expect(screen.queryByLabelText('Segredo da credencial')).not.toBeInTheDocument());
  });

  it('revogação envia If-Match com a versão da credencial', async () => {
    const versions: (string | null)[] = [];
    server.use(
      http.get('/api/v1/api-credentials', () => HttpResponse.json({ data: [active], next_cursor: null })),
      http.delete('/api/v1/api-credentials/:id', ({ request }) => {
        versions.push(request.headers.get('If-Match'));
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const { user } = renderPage(<CredentialsTab />);
    await user.click(await screen.findByRole('button', { name: 'Revogar credencial ERP interno' }));
    await user.click(await screen.findByRole('button', { name: 'Revogar' }));
    await waitFor(() => expect(versions).toEqual(['"2"']));
  });

  it('a listagem segue o next_cursor', async () => {
    const cursors: (string | null)[] = [];
    server.use(
      http.get('/api/v1/api-credentials', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('cursor');
        cursors.push(cursor);
        return cursor
          ? HttpResponse.json({ data: [{ ...active, id: 'cred-9', name: 'Integração antiga', revoked_at: '2026-10-01T12:00:00Z' }], next_cursor: null })
          : HttpResponse.json({ data: [active], next_cursor: 'cred-pagina-2' });
      }),
    );
    const { user } = renderPage(<CredentialsTab />);
    expect(await screen.findByText('ERP interno')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /Próxima/ }));
    expect(await screen.findByText('Integração antiga')).toBeInTheDocument();
    expect(cursors).toEqual([null, 'cred-pagina-2']);
  });

  it('sem api_credentials.manage não consulta a API', async () => {
    let requests = 0;
    server.use(
      http.get('/api/v1/api-credentials', () => {
        requests++;
        return HttpResponse.json({ data: [] });
      }),
    );
    renderPage(<CredentialsTab />, { session: makeSession({ permissions: AUDITOR_PERMISSIONS }) });
    expect(await screen.findByText('Acesso restrito')).toBeInTheDocument();
    expect(requests).toBe(0);
  });
});
