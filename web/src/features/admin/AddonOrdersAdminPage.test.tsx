import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { Session, StaffAddonOrder } from '../../api/types';
import { makeSession, TENANT_A } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { AddonOrdersAdminPage } from './AddonOrdersAdminPage';

const ORDER_ID = '0195f1a0-0000-7000-8000-0000000a0001';

function staffSession(): Session {
  return { ...makeSession({ tenantId: null }), organizations: [], platform_staff: true };
}

function order(overrides: Partial<StaffAddonOrder> = {}): StaffAddonOrder {
  return {
    id: ORDER_ID,
    tenant_id: TENANT_A,
    tenant_name: 'Acme Software',
    tenant_slug: 'acme',
    addon_code: 'implantacao_assistida',
    addon_name: 'Implantação assistida',
    status: 'requested',
    notes: 'Migrar 300 contratos',
    price: 250000,
    currency: 'BRL',
    created_at: '2026-11-10T12:00:00Z',
    scheduled_for: null,
    completed_at: null,
    canceled_at: null,
    cancel_reason: null,
    billed: false,
    version: 1,
    ...overrides,
  };
}

const base = `/api/v1/admin/tenants/${TENANT_A}/addon-orders/${ORDER_ID}`;

function listHandler(byStatus: Record<string, StaffAddonOrder[]>, seen: string[] = []) {
  return http.get('/api/v1/admin/addon-orders', ({ request }) => {
    const status = new URL(request.url).searchParams.get('status') ?? '';
    seen.push(status);
    expect(request.headers.get('X-Recorvia-Tenant')).toBeNull();
    return HttpResponse.json({ data: byStatus[status] ?? [] });
  });
}

describe('atendimento de serviços adicionais', () => {
  it('carrega as páginas seguintes pelo cursor', async () => {
    const cursors: (string | null)[] = [];
    server.use(
      http.get('/api/v1/admin/addon-orders', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('cursor');
        cursors.push(cursor);
        return cursor
          ? HttpResponse.json({ data: [order({ id: 'o2', tenant_name: 'Beta Consultoria' })], next_cursor: null })
          : HttpResponse.json({ data: [order()], next_cursor: 'cursor-1' });
      }),
    );
    const { user } = renderPage(<AddonOrdersAdminPage />, { path: '/admin/servicos', session: staffSession() });
    const table = await screen.findByRole('table', { name: 'Pedidos de serviços adicionais' });
    expect(within(table).getAllByRole('row')).toHaveLength(2);
    await user.click(screen.getByRole('button', { name: 'Carregar mais' }));
    await waitFor(() => expect(within(table).getAllByRole('row')).toHaveLength(3));
    expect(within(table).getAllByRole('row')[2]).toHaveTextContent('Beta Consultoria');
    expect(cursors).toEqual([null, 'cursor-1']);
    expect(screen.queryByRole('button', { name: 'Carregar mais' })).not.toBeInTheDocument();
  });

  it('quem não integra a equipe da plataforma vê acesso negado, sem consultar a API', async () => {
    const seen: string[] = [];
    server.use(listHandler({}, seen));
    renderPage(<AddonOrdersAdminPage />, { path: '/admin/servicos' });
    expect(await screen.findByText('Acesso negado')).toBeInTheDocument();
    expect(screen.getByText('Esta área é restrita à equipe da plataforma Recorvia.')).toBeInTheDocument();
    expect(seen).toEqual([]);
  });

  it('lista os pedidos em aberto e filtra por situação', async () => {
    const seen: string[] = [];
    server.use(
      listHandler(
        {
          '': [order(), order({ id: 'o2', status: 'scheduled', scheduled_for: '2026-11-25T13:00:00Z', tenant_name: 'Beta Consultoria' })],
          completed: [order({ id: 'o3', status: 'completed', completed_at: '2026-11-12T15:00:00Z', billed: true })],
        },
        seen,
      ),
    );
    const { user } = renderPage(<AddonOrdersAdminPage />, { path: '/admin/servicos', session: staffSession() });
    const table = await screen.findByRole('table', { name: 'Pedidos de serviços adicionais' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent('Acme Software');
    expect(rows[0]).toHaveTextContent('acme');
    expect(rows[0]).toHaveTextContent('Implantação assistida');
    expect(rows[0]).toHaveTextContent('R$ 2.500,00');
    expect(rows[0]).toHaveTextContent('Solicitado');
    expect(rows[0]).toHaveTextContent('Migrar 300 contratos');
    expect(within(rows[0]).getByRole('button', { name: 'Agendar Acme Software' })).toBeInTheDocument();
    expect(within(rows[1]).getByRole('button', { name: 'Reagendar Beta Consultoria' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Voltar às organizações' })).toHaveAttribute('href', '/organizacoes');

    await user.click(screen.getByText('Concluídos'));
    expect(await screen.findByText('Cobrado')).toBeInTheDocument();
    const done = within(screen.getByRole('table', { name: 'Pedidos de serviços adicionais' })).getAllByRole('row').slice(1);
    expect(done).toHaveLength(1);
    expect(within(done[0]).queryByRole('button')).not.toBeInTheDocument();
    expect(seen).toEqual(['', 'completed']);

    await user.click(screen.getByText('Cancelados'));
    expect(await screen.findByText('Nenhum pedido')).toBeInTheDocument();
  });

  it('agenda o atendimento enviando o instante em UTC, com CSRF e sem organização', async () => {
    const requests: Request[] = [];
    server.use(
      listHandler({ '': [order()] }),
      http.post(`${base}/schedule`, ({ request }) => {
        requests.push(request.clone());
        return HttpResponse.json(order({ status: 'scheduled', scheduled_for: '2026-11-25T13:00:00Z', version: 2 }));
      }),
    );
    const { user } = renderPage(<AddonOrdersAdminPage />, { path: '/admin/servicos', session: staffSession() });
    await user.click(await screen.findByRole('button', { name: 'Agendar Acme Software' }));
    const dialog = await screen.findByRole('dialog', { name: 'Agendar atendimento: Acme Software' });
    await user.click(within(dialog).getByRole('button', { name: 'Salvar agendamento' }));
    expect(await within(dialog).findByText('Informe a data e a hora do atendimento.')).toBeInTheDocument();

    fireEvent.change(within(dialog).getByLabelText(/Data e hora do atendimento/), { target: { value: '2026-11-25T10:00' } });
    await user.click(within(dialog).getByRole('button', { name: 'Salvar agendamento' }));
    expect(await screen.findByText('Atendimento de Acme Software agendado.')).toBeInTheDocument();
    const [req] = requests;
    expect(req.headers.get('X-CSRF-Token')).toBe('csrf-123');
    expect(req.headers.get('X-Recorvia-Tenant')).toBeNull();
    expect(req.headers.get('If-Match')).toBe('"1"');
    const body = (await req.json()) as { scheduled_for: string };
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    expect(body.scheduled_for).toMatch(/^2026-11-2[45]T\d{2}:00:00Z$/);
    if (tz === 'UTC') expect(body.scheduled_for).toBe('2026-11-25T10:00:00Z');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('erro de validação do servidor aparece no campo do agendamento', async () => {
    server.use(
      listHandler({ '': [order()] }),
      http.post(`${base}/schedule`, () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'Dados inválidos',
            status: 422,
            code: 'invalid_addon_schedule',
            detail: 'Data de atendimento inválida.',
            errors: [{ field: 'scheduled_for', message: 'Informe um instante futuro, em até um ano.' }],
          },
          { status: 422, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    );
    const { user } = renderPage(<AddonOrdersAdminPage />, { path: '/admin/servicos', session: staffSession() });
    await user.click(await screen.findByRole('button', { name: 'Agendar Acme Software' }));
    const dialog = await screen.findByRole('dialog', { name: 'Agendar atendimento: Acme Software' });
    fireEvent.change(within(dialog).getByLabelText(/Data e hora do atendimento/), { target: { value: '2020-01-01T10:00' } });
    await user.click(within(dialog).getByRole('button', { name: 'Salvar agendamento' }));
    const errors = await within(dialog).findAllByText('Informe um instante futuro, em até um ano.');
    expect(errors.length).toBeGreaterThan(0);
    expect(within(dialog).getByLabelText(/Data e hora do atendimento/)).toHaveAttribute('aria-invalid', 'true');
  });

  it('conclui somente depois da confirmação, que explica a cobrança no demonstrativo', async () => {
    let completes = 0;
    const seen: string[] = [];
    const versions: (string | null)[] = [];
    server.use(
      listHandler({ '': [order({ status: 'scheduled', scheduled_for: '2026-11-25T13:00:00Z', version: 2 })] }, seen),
      http.post(`${base}/complete`, ({ request }) => {
        completes++;
        versions.push(request.headers.get('If-Match'));
        expect(request.headers.get('X-Recorvia-Tenant')).toBeNull();
        return HttpResponse.json(order({ status: 'completed', completed_at: '2026-11-25T15:00:00Z', version: 3 }));
      }),
    );
    const { user } = renderPage(<AddonOrdersAdminPage />, { path: '/admin/servicos', session: staffSession() });
    await user.click(await screen.findByRole('button', { name: 'Concluir Acme Software' }));
    let dialog = await screen.findByRole('dialog', { name: 'Concluir atendimento' });
    expect(dialog).toHaveTextContent('entra no próximo demonstrativo mensal da organização');
    await user.click(within(dialog).getByRole('button', { name: 'Cancelar' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Concluir atendimento' })).not.toBeInTheDocument());
    expect(completes).toBe(0);

    await user.click(screen.getByRole('button', { name: 'Concluir Acme Software' }));
    dialog = await screen.findByRole('dialog', { name: 'Concluir atendimento' });
    await user.click(within(dialog).getByRole('button', { name: 'Concluir' }));
    expect(await screen.findByText('Atendimento concluído.')).toBeInTheDocument();
    expect(completes).toBe(1);
    expect(versions).toEqual(['"2"']);
    await waitFor(() => expect(seen.length).toBe(2)); // a lista é recarregada
  });

  it('cancela com justificativa obrigatória', async () => {
    const bodies: unknown[] = [];
    server.use(
      listHandler({ '': [order()] }),
      http.post(`${base}/cancel`, async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(order({ status: 'canceled', canceled_at: '2026-11-20T12:00:00Z', cancel_reason: 'Cliente desistiu.' }));
      }),
    );
    const { user } = renderPage(<AddonOrdersAdminPage />, { path: '/admin/servicos', session: staffSession() });
    await user.click(await screen.findByRole('button', { name: 'Cancelar Acme Software' }));
    const dialog = await screen.findByRole('dialog', { name: 'Cancelar pedido: Acme Software' });
    await user.click(within(dialog).getByLabelText(/Justificativa/));
    await user.paste('ok');
    await user.click(within(dialog).getByRole('button', { name: 'Cancelar pedido' }));
    expect(await within(dialog).findByText('O motivo deve ter pelo menos 3 caracteres.')).toBeInTheDocument();
    expect(bodies).toEqual([]);

    await user.clear(within(dialog).getByLabelText(/Justificativa/));
    await user.click(within(dialog).getByLabelText(/Justificativa/));
    await user.paste('Cliente desistiu.');
    await user.click(within(dialog).getByRole('button', { name: 'Cancelar pedido' }));
    expect(await screen.findByText('Pedido de Acme Software cancelado.')).toBeInTheDocument();
    expect(bodies).toEqual([{ reason: 'Cliente desistiu.' }]);
  });

  it.each([
    ['agendamento', { action: 'Agendar', title: 'Agendar atendimento: Acme Software', path: 'schedule', submit: 'Salvar agendamento' }],
    ['cancelamento', { action: 'Cancelar', title: 'Cancelar pedido: Acme Software', path: 'cancel', submit: 'Cancelar pedido' }],
  ] as const)('depois do 412 no %s, recarregar fecha o modal e a nova tentativa usa a versão atual', async (_name, { action, title, path, submit }) => {
    const versions: (string | null)[] = [];
    let current = order();
    server.use(
      http.get('/api/v1/admin/addon-orders', () => HttpResponse.json({ data: [current], next_cursor: null })),
      http.post(`${base}/${path}`, ({ request }) => {
        versions.push(request.headers.get('If-Match'));
        if (versions.length === 1) {
          // Outra pessoa da equipe alterou o pedido com o modal aberto.
          current = order({ notes: 'Atualizado por outra pessoa', version: 2 });
          return HttpResponse.json(
            { type: 'about:blank', title: 'Versão divergente', status: 412, code: 'version_mismatch' },
            { status: 412, headers: { 'Content-Type': 'application/problem+json' } },
          );
        }
        return HttpResponse.json(order({ status: path === 'cancel' ? 'canceled' : 'scheduled', version: 3 }), { headers: { ETag: '"3"' } });
      }),
    );
    const { user } = renderPage(<AddonOrdersAdminPage />, { path: '/admin/servicos', session: staffSession() });
    const send = async () => {
      await user.click(await screen.findByRole('button', { name: `${action} Acme Software` }));
      const dialog = await screen.findByRole('dialog', { name: title });
      if (path === 'schedule') {
        fireEvent.change(within(dialog).getByLabelText(/Data e hora do atendimento/), { target: { value: '2026-11-25T10:00' } });
      } else {
        await user.click(within(dialog).getByLabelText(/Justificativa/));
        await user.paste('Cliente desistiu.');
      }
      await user.click(within(dialog).getByRole('button', { name: submit }));
    };
    await send();
    await user.click(await screen.findByRole('button', { name: 'Recarregar dados' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: title })).not.toBeInTheDocument());
    expect(await screen.findByText('Atualizado por outra pessoa')).toBeInTheDocument();
    await send();
    await waitFor(() => expect(versions).toEqual(['"1"', '"2"']));
  });

  it('conflito do servidor (pedido já encerrado) é informado', async () => {
    server.use(
      listHandler({ '': [order()] }),
      http.post(`${base}/complete`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Conflito', status: 409, code: 'addon_order_closed', detail: 'O pedido já foi concluído ou cancelado.' },
          { status: 409, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    );
    const { user } = renderPage(<AddonOrdersAdminPage />, { path: '/admin/servicos', session: staffSession() });
    await user.click(await screen.findByRole('button', { name: 'Concluir Acme Software' }));
    const dialog = await screen.findByRole('dialog', { name: 'Concluir atendimento' });
    await user.click(within(dialog).getByRole('button', { name: 'Concluir' }));
    expect(await screen.findByText('O pedido já foi concluído ou cancelado.')).toBeInTheDocument();
  });
});
