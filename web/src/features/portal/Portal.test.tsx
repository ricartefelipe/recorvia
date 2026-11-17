import { render, screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { createMemoryRouter, RouterProvider } from 'react-router';
import { describe, expect, it } from 'vitest';
import { AppProviders } from '../../app/providers';
import { createTestQueryClient, renderPage } from '../../test/render';
import { server } from '../../test/server';
import { PortalLayout } from './PortalLayout';
import { PortalInvoicesPage } from './PortalPages';

describe('portal do cliente', () => {
  it('sem sessão orienta a acessar pelo link recebido (sem ir ao login interno)', async () => {
    server.use(
      http.get('/api/portal/v1/session', () =>
        HttpResponse.json({ type: 'about:blank', title: 'Não autenticado', status: 401, code: 'unauthenticated' }, { status: 401 }),
      ),
    );
    renderPage(<PortalLayout />, { path: '/portal' });
    expect(await screen.findByRole('heading', { name: 'Acesse pelo link recebido' })).toBeInTheDocument();
  });

  it('lista as faturas com link de pagamento do checkout hospedado', async () => {
    const requests: Request[] = [];
    server.use(
      http.get('/api/portal/v1/session', ({ request }) => {
        requests.push(request.clone());
        return HttpResponse.json({
          customer: { id: 'c1', name: 'Cliente Exemplo Ltda' },
          organization: { name: 'Acme Software' },
          csrf_token: 'portal-csrf',
          contacts: [],
        });
      }),
      http.get('/api/portal/v1/invoices', () =>
        HttpResponse.json({
          data: [
            {
              id: 'f1',
              number: 'ACME-000123',
              status: 'open',
              currency: 'BRL',
              total: 15990,
              amount_due: 15990,
              issued_at: '2026-11-01T12:00:00Z',
              due_date: '2026-11-10',
              overdue: false,
              checkout_url: 'https://pagamentos.exemplo.test/checkout/abc',
              items: [{ description: 'Plano Profissional', amount: 15990 }],
            },
          ],
        }),
      ),
    );
    const router = createMemoryRouter(
      [{ path: '/portal', element: <PortalLayout />, children: [{ index: true, element: <PortalInvoicesPage /> }] }],
      { initialEntries: ['/portal'] },
    );
    render(
      <AppProviders queryClient={createTestQueryClient()} env="test">
        <RouterProvider router={router} />
      </AppProviders>,
    );
    expect(await screen.findByText('Acme Software')).toBeInTheDocument();
    const table = await screen.findByRole('table', { name: 'Faturas' });
    expect(within(table).getByText('ACME-000123')).toBeInTheDocument();
    expect(within(table).getByRole('link', { name: /Pagar agora/ })).toHaveAttribute('href', 'https://pagamentos.exemplo.test/checkout/abc');
    expect(requests[0].headers.get('X-Recorvia-Tenant')).toBeNull();
  });
});
