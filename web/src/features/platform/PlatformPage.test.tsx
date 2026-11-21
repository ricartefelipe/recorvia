import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { AddonOrder, PlatformOverview, PlatformPlan } from '../../api/types';
import { AUDITOR_PERMISSIONS, makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { PlatformPage } from './PlatformPage';

const essencial: PlatformPlan = {
  code: 'essencial',
  name: 'Essencial',
  description: 'Até 3 usuários e 100 contratos ativos.',
  monthly_price: 19900,
  currency: 'BRL',
  max_users: 3,
  max_active_subscriptions: 100,
  features: ['recurring_billing'],
  trial_days: 14,
  is_default: true,
};
const escala: PlatformPlan = { ...essencial, code: 'escala', name: 'Escala', monthly_price: 129900, max_users: null, max_active_subscriptions: null, is_default: false };

const overview: PlatformOverview = {
  plan: essencial,
  subscription: {
    plan_code: 'essencial',
    status: 'active',
    trial_ends_at: null,
    current_period_start: '2026-11-16T03:00:00Z',
    current_period_end: '2026-12-16T03:00:00Z',
    version: 3,
  },
  usage: { active_users: 2, pending_invitations: 1, active_subscriptions: 40 },
};

function order(overrides: Partial<AddonOrder>): AddonOrder {
  return {
    id: '0195f1a0-0000-7000-8000-0000000a0001',
    addon_code: 'implantacao_assistida',
    status: 'requested',
    notes: '',
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

function platformHandlers(orders: AddonOrder[] = []) {
  return [
    http.get('/api/v1/platform/overview', () => HttpResponse.json(overview)),
    http.get('/api/v1/platform/plans', () => HttpResponse.json({ data: [essencial, escala] })),
    http.get('/api/v1/platform/addons', () =>
      HttpResponse.json({
        data: [{ code: 'implantacao_assistida', name: 'Implantação assistida', description: 'Configuração e treinamento.', price: 250000, currency: 'BRL', kind: 'one_time' }],
      }),
    ),
    http.get('/api/v1/platform/addon-orders', () => HttpResponse.json({ data: orders })),
    http.get('/api/v1/platform/statements', () =>
      HttpResponse.json({
        data: [
          {
            id: 's1',
            period_start: '2026-10-16T03:00:00Z',
            period_end: '2026-11-16T03:00:00Z',
            plan_code: 'essencial',
            subscription_amount: 19900,
            addons_amount: 250000,
            total: 269900,
            currency: 'BRL',
            status: 'open',
          },
        ],
      }),
    ),
  ];
}

describe('plano Recorvia', () => {
  it('mostra o consumo com convites pendentes e o andamento dos pedidos', async () => {
    server.use(
      ...platformHandlers([
        order({ id: 'o1', status: 'scheduled', scheduled_for: '2026-11-25T13:00:00Z' }),
        order({ id: 'o2', status: 'completed', completed_at: '2026-11-01T15:00:00Z', billed: true }),
        order({ id: 'o3', status: 'canceled', canceled_at: '2026-11-02T15:00:00Z', cancel_reason: 'Cliente desistiu.' }),
      ]),
    );
    renderPage(<PlatformPage />, { path: '/plataforma' });
    expect(await screen.findByText('2 ativos + 1 convite pendente de 3')).toBeInTheDocument();
    expect(screen.getByText('40 de 100')).toBeInTheDocument();

    const table = await screen.findByRole('table', { name: 'Pedidos de serviços adicionais' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows[0]).toHaveTextContent('Agendado');
    expect(rows[0]).toHaveTextContent('Agendado para 25/11/2026, 10:00');
    expect(rows[1]).toHaveTextContent('Concluído em 01/11/2026, 12:00 · cobrado no demonstrativo');
    expect(rows[2]).toHaveTextContent('Motivo: Cliente desistiu.');

    const statements = await screen.findByRole('table', { name: 'Demonstrativos do Recorvia' });
    expect(statements).toHaveTextContent('R$ 2.500,00');
    expect(statements).toHaveTextContent('R$ 2.699,00');
  });

  it('solicita a implantação assistida com observações', async () => {
    const bodies: unknown[] = [];
    server.use(
      ...platformHandlers(),
      http.post('/api/v1/platform/addon-orders', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(order({}), { status: 201 });
      }),
    );
    const { user } = renderPage(<PlatformPage />, { path: '/plataforma' });
    await user.click(await screen.findByRole('button', { name: 'Solicitar' }));
    const dialog = await screen.findByRole('dialog', { name: 'Solicitar: Implantação assistida' });
    await user.click(within(dialog).getByLabelText('Observações'));
    await user.paste('Migrar 300 contratos');
    await user.click(within(dialog).getByRole('button', { name: 'Enviar pedido' }));
    expect(await screen.findByText('Pedido registrado. Nossa equipe entrará em contato.')).toBeInTheDocument();
    expect(bodies).toEqual([{ addon_code: 'implantacao_assistida', notes: 'Migrar 300 contratos' }]);
  });

  it('troca de plano pede confirmação e envia o código do plano com a versão da assinatura no If-Match', async () => {
    const bodies: unknown[] = [];
    const versions: (string | null)[] = [];
    server.use(
      ...platformHandlers(),
      http.put('/api/v1/platform/plan', async ({ request }) => {
        versions.push(request.headers.get('If-Match'));
        bodies.push(await request.json());
        return HttpResponse.json({ ...overview, plan: escala, subscription: { ...overview.subscription, version: 4 } }, { headers: { ETag: '"4"' } });
      }),
    );
    const { user } = renderPage(<PlatformPage />, { path: '/plataforma' });
    await user.click(await screen.findByRole('button', { name: 'Mudar para este plano' }));
    const dialog = await screen.findByRole('dialog', { name: 'Trocar plano do Recorvia' });
    await user.click(within(dialog).getByRole('button', { name: 'Trocar plano' }));
    expect(await screen.findByText('Plano do Recorvia alterado para Escala.')).toBeInTheDocument();
    expect(bodies).toEqual([{ plan_code: 'escala' }]);
    expect(versions).toEqual(['"3"']);
  });

  it('depois do 412, recarregar traz a versão atual da assinatura e a nova troca a envia', async () => {
    const versions: (string | null)[] = [];
    let current = overview;
    server.use(
      http.get('/api/v1/platform/overview', () => HttpResponse.json(current)),
      ...platformHandlers(),
      http.put('/api/v1/platform/plan', ({ request }) => {
        versions.push(request.headers.get('If-Match'));
        if (versions.length === 1) {
          // O fechamento do demonstrativo avançou a assinatura enquanto a página estava aberta.
          current = { ...overview, subscription: { ...overview.subscription, version: 4 } };
          return HttpResponse.json(
            { type: 'about:blank', title: 'Versão divergente', status: 412, code: 'version_mismatch' },
            { status: 412, headers: { 'Content-Type': 'application/problem+json' } },
          );
        }
        return HttpResponse.json({ ...overview, plan: escala, subscription: { ...overview.subscription, version: 5 } }, { headers: { ETag: '"5"' } });
      }),
    );
    const { user } = renderPage(<PlatformPage />, { path: '/plataforma' });
    const change = async () => {
      await user.click(await screen.findByRole('button', { name: 'Mudar para este plano' }));
      const dialog = await screen.findByRole('dialog', { name: 'Trocar plano do Recorvia' });
      await user.click(within(dialog).getByRole('button', { name: 'Trocar plano' }));
    };
    await change();
    await user.click(await screen.findByRole('button', { name: 'Recarregar dados' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await change();
    expect(await screen.findByText('Plano do Recorvia alterado para Escala.')).toBeInTheDocument();
    expect(versions).toEqual(['"3"', '"4"']);
  });

  it('pedidos e demonstrativos seguem para a página seguinte pelo cursor', async () => {
    const cursors: Record<string, (string | null)[]> = { orders: [], statements: [] };
    const statement = (id: string, start: string) => ({
      id,
      period_start: start,
      period_end: start,
      plan_code: 'essencial',
      subscription_amount: 19900,
      addons_amount: 0,
      total: 19900,
      currency: 'BRL',
      status: 'open',
    });
    server.use(
      http.get('/api/v1/platform/addon-orders', ({ request }) => {
        const query = new URL(request.url).searchParams;
        cursors.orders.push(query.get('cursor'));
        return query.get('cursor')
          ? HttpResponse.json({ data: [order({ id: 'o2', notes: 'Pedido da segunda página' })], next_cursor: null })
          : HttpResponse.json({ data: [order({ id: 'o1', notes: 'Pedido da primeira página' })], next_cursor: 'pedidos-2' });
      }),
      http.get('/api/v1/platform/statements', ({ request }) => {
        const query = new URL(request.url).searchParams;
        cursors.statements.push(query.get('cursor'));
        return query.get('cursor')
          ? HttpResponse.json({ data: [statement('s2', '2026-09-16T03:00:00Z')], next_cursor: null })
          : HttpResponse.json({ data: [statement('s1', '2026-10-16T03:00:00Z')], next_cursor: 'demonstrativos-2' });
      }),
      ...platformHandlers(),
    );
    const { user } = renderPage(<PlatformPage />, { path: '/plataforma' });
    expect(await screen.findByText('Pedido da primeira página')).toBeInTheDocument();
    const statements = await screen.findByRole('table', { name: 'Demonstrativos do Recorvia' });
    expect(statements).toHaveTextContent('16/10/2026');
    const [ordersPager, statementsPager] = screen.getAllByRole('navigation', { name: 'Paginação' });
    await user.click(within(ordersPager).getByRole('button', { name: 'Próxima' }));
    expect(await screen.findByText('Pedido da segunda página')).toBeInTheDocument();
    await user.click(within(statementsPager).getByRole('button', { name: 'Próxima' }));
    await waitFor(() => expect(screen.getByRole('table', { name: 'Demonstrativos do Recorvia' })).toHaveTextContent('16/09/2026'));
    expect(cursors).toEqual({ orders: [null, 'pedidos-2'], statements: [null, 'demonstrativos-2'] });
  });

  it('sem a gestão do plano, não oferece troca nem pedidos', async () => {
    server.use(...platformHandlers());
    renderPage(<PlatformPage />, { path: '/plataforma', session: makeSession({ permissions: AUDITOR_PERMISSIONS }) });
    expect(await screen.findByText('Escala')).toBeInTheDocument();
    expect(await screen.findByText('Implantação assistida')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Mudar para este plano' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Solicitar' })).not.toBeInTheDocument();
    expect(screen.getByText('Nenhum pedido.')).toBeInTheDocument();
  });
});
