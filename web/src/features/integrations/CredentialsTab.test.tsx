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
};

describe('credenciais de API', () => {
  it('oferece como escopos apenas permissões delegáveis do próprio papel', () => {
    expect(grantableScopes(['usage.write', 'members.manage', 'customers.read', 'api_credentials.manage', 'platform_billing.manage'])).toEqual([
      'customers.read',
      'usage.write',
    ]);
  });

  it('lista sem segredos e exibe o segredo da rotação uma única vez', async () => {
    const rotated: string[] = [];
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
      http.post('/api/v1/api-credentials/:id/rotate', ({ params }) => {
        rotated.push(String(params.id));
        return HttpResponse.json({ credential: active, secret: `${active.prefix}_${'a'.repeat(52)}` }, { status: 201 });
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
    await user.click(screen.getByRole('button', { name: 'Já copiei, fechar' }));
    await waitFor(() => expect(screen.queryByLabelText('Segredo da credencial')).not.toBeInTheDocument());
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
