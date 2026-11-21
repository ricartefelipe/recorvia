import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { AuditEvent, PortalLink } from '../../api/types';
import { AUDITOR_PERMISSIONS, makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { CustomerDetailPage } from './CustomerDetailPage';

const CUSTOMER_ID = '0195f1a0-0000-7000-8000-00000000d001';
const LINKS = `/api/v1/customers/${CUSTOMER_ID}/portal-links`;

const customer = {
  id: CUSTOMER_ID,
  kind: 'company',
  name: 'Cliente Exemplo Ltda',
  document_type: 'cnpj',
  document_number: '11222333000181',
  email: 'financeiro@exemplo.test',
  status: 'active',
  created_at: '2026-11-01T12:00:00Z',
  updated_at: '2026-11-01T12:00:00Z',
  version: 1,
  contacts: [],
  billing_address: null,
};

const used: PortalLink = {
  id: '0195f1a0-0000-7000-8000-0000000000e3',
  status: 'used',
  created_at: '2026-11-18T12:00:00Z',
  expires_at: '2026-11-21T12:00:00Z',
  used_at: '2026-11-18T13:00:00Z',
  revoked_at: null,
  revoke_reason: null,
  created_by: { type: 'user', id: '0195f1a0-0000-7000-8000-000000000001', name: 'Ana Owner' },
};
const superseded: PortalLink = {
  ...used,
  id: '0195f1a0-0000-7000-8000-0000000000e2',
  status: 'revoked',
  created_at: '2026-11-10T12:00:00Z',
  expires_at: '2026-11-13T12:00:00Z',
  used_at: null,
  revoked_at: '2026-11-18T12:00:00Z',
  revoke_reason: 'superseded',
  created_by: { type: 'api_credential', id: '0195f1a0-0000-7000-8000-0000000000c1', name: 'ERP interno' },
};
const expired: PortalLink = {
  ...used,
  id: '0195f1a0-0000-7000-8000-0000000000e1',
  status: 'expired',
  created_at: '2026-11-01T12:00:00Z',
  expires_at: '2026-11-04T12:00:00Z',
  used_at: null,
};

const HISTORY = `/api/v1/customers/${CUSTOMER_ID}/history`;
const PORTAL_URL = 'https://app.recorvia.test/portal/access#token=segredo-de-uso-unico';

const createdEvent: AuditEvent = {
  id: '0195f1a0-0000-7000-8000-0000000000a1',
  occurred_at: '2026-11-01T12:00:00Z',
  actor_type: 'user',
  actor_id: '0195f1a0-0000-7000-8000-000000000001',
  actor_label: 'Ana Owner',
  action: 'customer.created',
  resource_type: 'customer',
  resource_id: CUSTOMER_ID,
  details: {},
};
const linkCreatedEvent: AuditEvent = {
  ...createdEvent,
  id: '0195f1a0-0000-7000-8000-0000000000a2',
  occurred_at: '2026-11-19T12:00:00Z',
  action: 'customer.portal_link_created',
  details: { link_id: '0195f1a0-0000-7000-8000-0000000000e4', superseded_links: 1 },
};

function renderCustomer(options: { session?: ReturnType<typeof makeSession>; tab?: string } = {}) {
  server.use(http.get(`/api/v1/customers/${CUSTOMER_ID}`, () => HttpResponse.json(customer)));
  return renderPage(<CustomerDetailPage />, {
    path: '/clientes/:id',
    route: `/clientes/${CUSTOMER_ID}?aba=${options.tab ?? 'portal'}`,
    session: options.session,
  });
}

/** A partir da chamada, a ficha do cliente responde 503 e as consultas são contadas. */
function failCustomerDetail(): { requests: () => number } {
  let requests = 0;
  server.use(
    http.get(`/api/v1/customers/${CUSTOMER_ID}`, () => {
      requests += 1;
      return HttpResponse.json({ code: 'unavailable', message: 'Serviço indisponível.' }, { status: 503 });
    }),
  );
  return { requests: () => requests };
}

describe('links do portal na ficha do cliente', () => {
  it('lista situação, datas e autoria sem expor o endereço do link', async () => {
    server.use(http.get(LINKS, () => HttpResponse.json({ data: [used, superseded, expired], next_cursor: null })));
    renderCustomer();
    const table = await screen.findByRole('table', { name: 'Links do portal' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(3);
    expect(within(rows[0]).getByText('Utilizado')).toBeInTheDocument();
    expect(within(rows[0]).getByText('Ana Owner')).toBeInTheDocument();
    expect(within(rows[0]).getByText('Usuário')).toBeInTheDocument();
    expect(within(rows[1]).getByText('Revogado')).toBeInTheDocument();
    expect(within(rows[1]).getByText('Substituído por um link mais novo')).toBeInTheDocument();
    expect(within(rows[1]).getByText('ERP interno')).toBeInTheDocument();
    expect(within(rows[1]).getByText('Credencial de API')).toBeInTheDocument();
    expect(within(rows[2]).getByText('Expirado')).toBeInTheDocument();
    // Só o link que ainda dá acesso pode ser revogado.
    expect(within(rows[0]).getByRole('button', { name: 'Revogar' })).toBeInTheDocument();
    expect(within(rows[1]).queryByRole('button', { name: 'Revogar' })).not.toBeInTheDocument();
    expect(within(rows[2]).queryByRole('button', { name: 'Revogar' })).not.toBeInTheDocument();
    expect(table).not.toHaveTextContent('/portal/access');
    expect(table).not.toHaveTextContent('token');
  });

  it('revoga somente depois da confirmação e recarrega a lista', async () => {
    let revoked = false;
    const deletes: { path: string; csrf: string | null; tenant: string | null }[] = [];
    server.use(
      http.get(LINKS, () =>
        HttpResponse.json({
          data: [revoked ? { ...used, status: 'revoked', revoked_at: '2026-11-19T12:00:00Z', revoke_reason: 'revoked' } : used],
          next_cursor: null,
        }),
      ),
      http.delete(`${LINKS}/:linkId`, ({ request }) => {
        deletes.push({
          path: new URL(request.url).pathname,
          csrf: request.headers.get('X-CSRF-Token'),
          tenant: request.headers.get('X-Recorvia-Tenant'),
        });
        revoked = true;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const { user, session } = renderCustomer();
    const table = await screen.findByRole('table', { name: 'Links do portal' });

    // Cancelar a confirmação não envia nada.
    await user.click(within(table).getByRole('button', { name: 'Revogar' }));
    let dialog = await screen.findByRole('dialog', { name: 'Revogar link do portal' });
    expect(dialog).toHaveTextContent('A sessão do portal aberta com ele é encerrada agora');
    await user.click(within(dialog).getByRole('button', { name: 'Cancelar' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Revogar link do portal' })).not.toBeInTheDocument());
    expect(deletes).toHaveLength(0);

    await user.click(within(table).getByRole('button', { name: 'Revogar' }));
    dialog = await screen.findByRole('dialog', { name: 'Revogar link do portal' });
    await user.click(within(dialog).getByRole('button', { name: 'Revogar link' }));
    expect(await screen.findByText('Link revogado. A sessão do portal aberta com ele foi encerrada.')).toBeInTheDocument();
    expect(deletes).toEqual([
      { path: `${LINKS}/${used.id}`, csrf: session.csrf_token, tenant: session.active_tenant?.id ?? null },
    ]);
    await waitFor(() => expect(within(table).getByText('Revogado pela organização')).toBeInTheDocument());
    expect(within(table).queryByRole('button', { name: 'Revogar' })).not.toBeInTheDocument();
  });

  it('link pendente: a confirmação explica que ele não poderá ser aberto', async () => {
    server.use(http.get(LINKS, () => HttpResponse.json({ data: [{ ...used, status: 'pending', used_at: null }], next_cursor: null })));
    const { user } = renderCustomer();
    const table = await screen.findByRole('table', { name: 'Links do portal' });
    expect(within(table).getByText('Pendente')).toBeInTheDocument();
    await user.click(within(table).getByRole('button', { name: 'Revogar' }));
    const dialog = await screen.findByRole('dialog', { name: 'Revogar link do portal' });
    expect(dialog).toHaveTextContent('Ele deixa de funcionar e não poderá ser aberto');
    server.use(http.delete(`${LINKS}/:linkId`, () => new HttpResponse(null, { status: 204 })));
    await user.click(within(dialog).getByRole('button', { name: 'Revogar link' }));
    expect(await screen.findByText('Link revogado. Ele não pode mais ser aberto.')).toBeInTheDocument();
  });

  it('pagina a lista pelo cursor', async () => {
    const cursors: (string | null)[] = [];
    server.use(
      http.get(LINKS, ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('cursor');
        cursors.push(cursor);
        return HttpResponse.json(cursor ? { data: [expired], next_cursor: null } : { data: [used, superseded], next_cursor: 'c2' });
      }),
    );
    const { user } = renderCustomer();
    const table = await screen.findByRole('table', { name: 'Links do portal' });
    expect(within(table).getAllByRole('row')).toHaveLength(3);
    await user.click(screen.getByRole('button', { name: /Próxima/ }));
    expect(await within(table).findByText('Expirado')).toBeInTheDocument();
    await waitFor(() => expect(within(table).getAllByRole('row')).toHaveLength(2));
    expect(cursors).toEqual([null, 'c2']);
    expect(screen.getByRole('button', { name: /Anterior/ })).toBeEnabled();
  });

  it('gerar link recarrega a lista de links, sem recarregar a ficha, e mantém o link de uso único na tela', async () => {
    const pending: PortalLink = {
      ...used,
      id: '0195f1a0-0000-7000-8000-0000000000e4',
      status: 'pending',
      created_at: '2026-11-19T12:00:00Z',
      expires_at: '2026-11-22T12:00:00Z',
      used_at: null,
    };
    let generated = false;
    server.use(
      http.get(LINKS, () =>
        HttpResponse.json({
          data: generated ? [pending, { ...used, status: 'revoked', revoked_at: pending.created_at, revoke_reason: 'superseded' }] : [used],
          next_cursor: null,
        }),
      ),
      http.post(LINKS, () => {
        generated = true;
        return HttpResponse.json({ id: pending.id, url: PORTAL_URL, expires_at: pending.expires_at }, { status: 201 });
      }),
    );
    const { user } = renderCustomer();
    const table = await screen.findByRole('table', { name: 'Links do portal' });
    expect(within(table).getAllByRole('row')).toHaveLength(2);

    // A partir daqui a ficha do cliente não pode mais ser recarregada: o link
    // só existe na tela, e o anterior já foi revogado pela geração.
    const detail = failCustomerDetail();
    await user.click(screen.getByRole('button', { name: 'Link do portal' }));
    expect(await screen.findByDisplayValue(PORTAL_URL)).toBeInTheDocument();

    // A lista é recarregada e mostra o novo link e a substituição do anterior.
    expect(await within(table).findByText('Pendente')).toBeInTheDocument();
    expect(within(table).getByText('Substituído por um link mais novo')).toBeInTheDocument();
    expect(detail.requests()).toBe(0);
    expect(screen.queryByRole('button', { name: 'Tentar novamente' })).not.toBeInTheDocument();
    expect(screen.getByDisplayValue(PORTAL_URL)).toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'Link do portal do cliente' })).toBeInTheDocument();
  });

  it('gerar link com a aba Histórico aberta recarrega o histórico e mostra a geração', async () => {
    let generated = false;
    let historyRequests = 0;
    server.use(
      http.get(HISTORY, () => {
        historyRequests += 1;
        return HttpResponse.json({ data: generated ? [linkCreatedEvent, createdEvent] : [createdEvent] });
      }),
      http.post(LINKS, () => {
        generated = true;
        return HttpResponse.json({ id: used.id, url: PORTAL_URL, expires_at: used.expires_at }, { status: 201 });
      }),
    );
    const { user } = renderCustomer({ tab: 'historico' });
    expect(await screen.findByText('customer.created')).toBeInTheDocument();
    expect(screen.queryByText('customer.portal_link_created')).not.toBeInTheDocument();
    expect(historyRequests).toBe(1);

    const detail = failCustomerDetail();
    await user.click(screen.getByRole('button', { name: 'Link do portal' }));
    expect(await screen.findByDisplayValue(PORTAL_URL)).toBeInTheDocument();

    // O evento de geração aparece sem depender de foco da janela nem de remontagem.
    expect(await screen.findByText('customer.portal_link_created')).toBeInTheDocument();
    expect(historyRequests).toBe(2);
    expect(detail.requests()).toBe(0);
    expect(screen.getByDisplayValue(PORTAL_URL)).toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'Link do portal do cliente' })).toBeInTheDocument();
  });

  it('falha ao recarregar o histórico depois de gerar o link não tira o link da tela', async () => {
    let generated = false;
    let historyRequests = 0;
    server.use(
      http.get(HISTORY, () => {
        historyRequests += 1;
        return generated
          ? HttpResponse.json({ code: 'unavailable', message: 'Serviço indisponível.' }, { status: 503 })
          : HttpResponse.json({ data: [createdEvent] });
      }),
      http.post(LINKS, () => {
        generated = true;
        return HttpResponse.json({ id: used.id, url: PORTAL_URL, expires_at: used.expires_at }, { status: 201 });
      }),
    );
    const { user } = renderCustomer({ tab: 'historico' });
    expect(await screen.findByText('customer.created')).toBeInTheDocument();

    const detail = failCustomerDetail();
    await user.click(screen.getByRole('button', { name: 'Link do portal' }));
    expect(await screen.findByDisplayValue(PORTAL_URL)).toBeInTheDocument();

    // O erro fica restrito ao painel do histórico; o modal e o link continuam.
    await waitFor(() => expect(historyRequests).toBe(2));
    await waitFor(() => expect(screen.queryByText('customer.created')).not.toBeInTheDocument());
    expect(detail.requests()).toBe(0);
    expect(screen.getByDisplayValue(PORTAL_URL)).toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'Link do portal do cliente' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: customer.name })).toBeInTheDocument();
  });

  it('cliente sem links orienta a gerar o primeiro', async () => {
    server.use(http.get(LINKS, () => HttpResponse.json({ data: [], next_cursor: null })));
    renderCustomer();
    expect(await screen.findByText('Nenhum link gerado')).toBeInTheDocument();
  });

  it('sem a permissão de links, a aba Portal não aparece nem é consultada', async () => {
    let requested = false;
    server.use(
      http.get(LINKS, () => {
        requested = true;
        return HttpResponse.json({ data: [used], next_cursor: null });
      }),
    );
    renderCustomer({ session: makeSession({ permissions: AUDITOR_PERMISSIONS }) });
    expect(await screen.findByRole('tab', { name: 'Dados' })).toHaveAttribute('aria-selected', 'true');
    expect(screen.queryByRole('tab', { name: 'Portal' })).not.toBeInTheDocument();
    expect(screen.queryByRole('table', { name: 'Links do portal' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Link do portal' })).not.toBeInTheDocument();
    expect(requested).toBe(false);
  });
});
