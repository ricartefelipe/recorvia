import { screen, within } from '@testing-library/react';
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

  it('troca de plano pede confirmação e envia o código do plano', async () => {
    const bodies: unknown[] = [];
    server.use(
      ...platformHandlers(),
      http.put('/api/v1/platform/plan', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ ...overview, plan: escala });
      }),
    );
    const { user } = renderPage(<PlatformPage />, { path: '/plataforma' });
    await user.click(await screen.findByRole('button', { name: 'Mudar para este plano' }));
    const dialog = await screen.findByRole('dialog', { name: 'Trocar plano do Recorvia' });
    await user.click(within(dialog).getByRole('button', { name: 'Trocar plano' }));
    expect(await screen.findByText('Plano do Recorvia alterado para Escala.')).toBeInTheDocument();
    expect(bodies).toEqual([{ plan_code: 'escala' }]);
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
