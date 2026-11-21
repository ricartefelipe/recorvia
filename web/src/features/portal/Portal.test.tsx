import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { createMemoryRouter, RouterProvider } from 'react-router';
import { describe, expect, it } from 'vitest';
import { AppProviders } from '../../app/providers';
import { createTestQueryClient, renderPage } from '../../test/render';
import { server } from '../../test/server';
import { PortalLayout } from './PortalLayout';
import { PortalInvoicePage, PortalInvoicesPage, PortalPreferencesPage } from './PortalPages';

const session = {
  customer: { id: 'c1', name: 'Cliente Exemplo Ltda' },
  organization: { name: 'Acme Software' },
  csrf_token: 'portal-csrf',
  contacts: [
    { id: 'k1', name: 'Financeiro', email: 'fin@cliente.test', role: '', receives_billing_email: true, created_at: '2026-11-01T12:00:00Z' },
    { id: 'k2', name: 'Operações', email: 'ops@cliente.test', role: 'Compras', receives_billing_email: false, created_at: '2026-11-01T12:00:00Z' },
  ],
};

const invoice = {
  id: 'f1',
  number: 'ACME-000123',
  status: 'open',
  currency: 'BRL',
  total: 15990,
  amount_due: 15990,
  issued_at: '2026-11-01T12:00:00Z',
  due_date: '2026-11-10',
  overdue: true,
  checkout_url: 'https://pagamentos.exemplo.test/checkout/abc',
  items: [{ description: 'Plano Profissional', amount: 15990 }],
};

/** Monta o portal com as rotas reais em memória. */
function renderPortal(route: string) {
  const router = createMemoryRouter(
    [
      {
        path: '/portal',
        element: <PortalLayout />,
        children: [
          { index: true, element: <PortalInvoicesPage /> },
          { path: 'faturas/:id', element: <PortalInvoicePage /> },
          { path: 'preferencias', element: <PortalPreferencesPage /> },
        ],
      },
    ],
    { initialEntries: [route] },
  );
  const user = userEvent.setup();
  render(
    <AppProviders queryClient={createTestQueryClient()} env="test">
      <RouterProvider router={router} />
    </AppProviders>,
  );
  return { user };
}

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

  it('carrega mais faturas pelo cursor, mantendo as já exibidas', async () => {
    const cursors: (string | null)[] = [];
    const pages: Record<string, { data: (typeof invoice)[]; next_cursor: string | null }> = {
      '': {
        data: [invoice, { ...invoice, id: 'f2', number: 'ACME-000122' }],
        next_cursor: 'cursor-2',
      },
      'cursor-2': { data: [{ ...invoice, id: 'f3', number: 'ACME-000121' }], next_cursor: null },
    };
    server.use(
      http.get('/api/portal/v1/session', () => HttpResponse.json(session)),
      http.get('/api/portal/v1/invoices', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('cursor');
        cursors.push(cursor);
        return HttpResponse.json(pages[cursor ?? '']);
      }),
    );
    const { user } = renderPortal('/portal');
    const table = await screen.findByRole('table', { name: 'Faturas' });
    expect(within(table).getAllByRole('row')).toHaveLength(3);
    expect(screen.getByText('2 faturas exibidas; há faturas mais antigas.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Carregar mais faturas' }));
    expect(await within(table).findByText('ACME-000121')).toBeInTheDocument();
    // As páginas se acumulam, na ordem recebida, sem repetir faturas.
    expect(within(table).getAllByRole('link', { name: /^ACME-/ }).map((a) => a.textContent)).toEqual(['ACME-000123', 'ACME-000122', 'ACME-000121']);
    expect(cursors).toEqual([null, 'cursor-2']);
    expect(screen.queryByRole('button', { name: 'Carregar mais faturas' })).not.toBeInTheDocument();
    expect(screen.getByText('3 faturas exibidas; não há mais faturas.')).toBeInTheDocument();
  });

  it('falha ao carregar mais mantém as faturas exibidas e permite tentar de novo', async () => {
    let failures = 1;
    server.use(
      http.get('/api/portal/v1/session', () => HttpResponse.json(session)),
      http.get('/api/portal/v1/invoices', ({ request }) => {
        if (!new URL(request.url).searchParams.get('cursor')) return HttpResponse.json({ data: [invoice], next_cursor: 'cursor-2' });
        if (failures-- > 0) {
          return HttpResponse.json({ type: 'about:blank', title: 'Indisponível', status: 503, code: 'unavailable' }, { status: 503 });
        }
        return HttpResponse.json({ data: [{ ...invoice, id: 'f2', number: 'ACME-000122' }], next_cursor: null });
      }),
    );
    const { user } = renderPortal('/portal');
    const table = await screen.findByRole('table', { name: 'Faturas' });
    await user.click(screen.getByRole('button', { name: 'Carregar mais faturas' }));
    await user.click(await screen.findByRole('button', { name: 'Tentar novamente' }));
    expect(await within(table).findByText('ACME-000122')).toBeInTheDocument();
    expect(within(table).getByText('ACME-000123')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Tentar novamente' })).not.toBeInTheDocument();
  });

  it('mostra ao cliente a baixa interna como fatura encerrada, sem botão de pagamento', async () => {
    server.use(
      http.get('/api/portal/v1/session', () => HttpResponse.json(session)),
      http.get('/api/portal/v1/invoices', () =>
        HttpResponse.json({ data: [{ ...invoice, id: 'f2', number: 'ACME-000124', status: 'uncollectible', amount_due: 0, overdue: false, checkout_url: null }] }),
      ),
    );
    renderPortal('/portal');
    const table = await screen.findByRole('table', { name: 'Faturas' });
    expect(within(table).getByText('Encerrada')).toBeInTheDocument();
    expect(within(table).queryByText('Incobrável')).not.toBeInTheDocument();
    expect(within(table).queryByRole('link', { name: /Pagar agora/ })).not.toBeInTheDocument();
  });

  it('detalhe da fatura mostra vencida e abre o documento comercial isolado', async () => {
    server.use(
      http.get('/api/portal/v1/session', () => HttpResponse.json(session)),
      http.get('/api/portal/v1/invoices/:id', () => HttpResponse.json(invoice)),
      http.get('/api/portal/v1/invoices/:id/document', () =>
        new HttpResponse('<!doctype html><html><body><h1>Fatura ACME-000123</h1></body></html>', { headers: { 'Content-Type': 'text/html; charset=utf-8' } }),
      ),
    );
    const { user } = renderPortal('/portal/faturas/f1');
    expect(await screen.findByRole('heading', { name: 'Fatura ACME-000123' })).toBeInTheDocument();
    expect(screen.getByText('Vencida')).toBeInTheDocument();
    expect(screen.getByRole('table', { name: 'Itens da fatura' })).toHaveTextContent('Plano Profissional');
    await user.click(screen.getByRole('button', { name: 'Ver documento' }));
    const frame = await screen.findByTitle('Fatura ACME-000123');
    expect(frame).toHaveAttribute('sandbox', 'allow-same-origin allow-modals');
    expect(frame.getAttribute('srcdoc')).toContain('Fatura ACME-000123');
  });

  it('fatura de outro cliente responde como inexistente', async () => {
    server.use(
      http.get('/api/portal/v1/session', () => HttpResponse.json(session)),
      http.get('/api/portal/v1/invoices/:id', () =>
        HttpResponse.json({ type: 'about:blank', title: 'Não encontrado', status: 404, code: 'invoice_not_found', detail: 'Fatura não encontrada.' }, { status: 404 }),
      ),
    );
    renderPortal('/portal/faturas/outra');
    expect(await screen.findByText('Fatura não encontrada')).toBeInTheDocument();
    // O portal não oferece caminho para o painel interno.
    expect(screen.getByRole('link', { name: 'Voltar às faturas' })).toHaveAttribute('href', '/portal');
    expect(screen.queryByRole('link', { name: 'Ir para o painel' })).not.toBeInTheDocument();
  });

  it('preferências enviam o token CSRF do portal e gravam a escolha de cada contato', async () => {
    const sent: { csrf: string | null; body: unknown }[] = [];
    server.use(
      http.get('/api/portal/v1/session', () => HttpResponse.json(session)),
      http.put('/api/portal/v1/contact-preferences', async ({ request }) => {
        const body = (await request.json()) as { contacts: { id: string; receives_billing_email: boolean }[] };
        sent.push({ csrf: request.headers.get('X-CSRF-Token'), body });
        const chosen = new Map(body.contacts.map((c) => [c.id, c.receives_billing_email]));
        return HttpResponse.json({ ...session, contacts: session.contacts.map((c) => ({ ...c, receives_billing_email: chosen.get(c.id) ?? c.receives_billing_email })) });
      }),
    );
    const { user } = renderPortal('/portal/preferencias');
    await user.click(await screen.findByRole('checkbox', { name: /Operações/ }));
    await user.click(screen.getByRole('checkbox', { name: /Financeiro/ }));
    await user.click(screen.getByRole('button', { name: 'Salvar preferências' }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0].csrf).toBe('portal-csrf');
    expect(sent[0].body).toEqual({
      contacts: [
        { id: 'k1', receives_billing_email: false },
        { id: 'k2', receives_billing_email: true },
      ],
    });
    expect(await screen.findByText('Preferências gravadas.')).toBeInTheDocument();
  });

  it('sair encerra a sessão do portal com o token CSRF', async () => {
    const logouts: (string | null)[] = [];
    server.use(
      http.get('/api/portal/v1/session', () => HttpResponse.json(session)),
      http.get('/api/portal/v1/invoices', () => HttpResponse.json({ data: [] })),
      http.post('/api/portal/v1/logout', ({ request }) => {
        logouts.push(request.headers.get('X-CSRF-Token'));
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const { user } = renderPortal('/portal');
    expect(await screen.findByText('Nenhuma fatura')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Sair' }));
    expect(await screen.findByRole('heading', { name: 'Sessão encerrada' })).toBeInTheDocument();
    expect(logouts).toEqual(['portal-csrf']);
  });
});
