import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { Coupon, Metric, Plan, Product } from '../../api/types';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { SubscriptionCreateModal } from '../subscriptions/SubscriptionCreateModal';
import { CouponsPage } from './CouponsPage';
import { MetricsPage } from './MetricsPage';
import { PlansPage } from './PlansPage';
import { ProductsPage } from './ProductsPage';

// ADR-085: as listas do catálogo são paginadas por cursor. As telas de
// listagem paginam com CursorPager; os campos de seleção percorrem todas as
// páginas pelo next_cursor, com o limite máximo da API.

const uuid = (n: number) => `0195f1a0-0000-7000-8000-${String(n).padStart(12, '0')}`;

const product = (n: number, name: string, status: Product['status'] = 'active'): Product => ({
  id: uuid(100 + n),
  name,
  description: '',
  status,
  created_at: '2026-11-01T12:00:00Z',
  version: 1,
});

const plan = (n: number, name: string): Plan => ({
  id: uuid(200 + n),
  product_id: uuid(101),
  code: `plano-${n}`,
  name,
  description: '',
  billing_interval: 'month',
  currency: 'BRL',
  trial_days: 0,
  status: 'active',
  current_price: null,
  created_at: '2026-11-01T12:00:00Z',
  version: 1,
});

const metric = (n: number, code: string): Metric => ({
  id: uuid(300 + n),
  code,
  name: `Métrica ${code}`,
  unit: 'chamada',
  aggregation: 'sum',
  max_quantity_per_event: '1000',
  late_grace_hours: 0,
  status: 'active',
  created_at: '2026-11-01T12:00:00Z',
});

const coupon = (n: number, code: string): Coupon => ({
  id: uuid(400 + n),
  code,
  name: `Cupom ${code}`,
  discount_type: 'percent',
  percent_off_bp: 1000,
  duration: 'once',
  applies_to_plan_ids: [],
  redemptions_count: 0,
  status: 'active',
  currency: 'BRL',
  created_at: '2026-11-01T12:00:00Z',
  version: 1,
});

/**
 * Listagem em duas páginas: sem cursor, a primeira (com next_cursor "p2");
 * com ele, a segunda. Registra os parâmetros de cada pedido.
 */
function twoPages<T>(path: string, first: readonly T[], second: (query: URLSearchParams) => readonly T[]) {
  const requests: URLSearchParams[] = [];
  server.use(
    http.get(path, ({ request }) => {
      const query = new URL(request.url).searchParams;
      requests.push(query);
      return query.get('cursor') === 'p2'
        ? HttpResponse.json({ data: second(query), next_cursor: null })
        : HttpResponse.json({ data: first, next_cursor: 'p2' });
    }),
  );
  return requests;
}

const params = (requests: URLSearchParams[], ...names: string[]) => requests.map((q) => names.map((n) => q.get(n)));

describe('listagens do catálogo por cursor', () => {
  it.each([
    ['produtos', { path: '/api/v1/products', Page: ProductsPage, first: [product(1, 'Produto da primeira página')], second: [product(2, 'Produto da segunda página')] }],
    ['métricas', { path: '/api/v1/metrics', Page: MetricsPage, first: [metric(1, 'chamadas.api')], second: [metric(2, 'gb.armazenado')] }],
    ['cupons', { path: '/api/v1/coupons', Page: CouponsPage, first: [coupon(1, 'PRIMEIRA')], second: [coupon(2, 'SEGUNDA')] }],
  ] as const)('a lista de %s segue o next_cursor, 25 itens por página', async (_name, { path, Page, first, second }) => {
    server.use(http.get('/api/v1/plans', () => HttpResponse.json({ data: [], next_cursor: null })));
    const requests = twoPages<unknown>(path, first, () => second);
    const { user } = renderPage(<Page />);
    const label = (item: unknown) => (item as { name: string }).name;
    expect(await screen.findByText(label(first[0]))).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Próxima' }));
    expect(await screen.findByText(label(second[0]))).toBeInTheDocument();
    expect(screen.getByText('Página 2')).toBeInTheDocument();
    expect(screen.queryByText(label(first[0]))).not.toBeInTheDocument();
    expect(params(requests, 'limit', 'cursor')).toEqual([
      ['25', null],
      ['25', 'p2'],
    ]);
    await user.click(screen.getByRole('button', { name: 'Anterior' }));
    expect(await screen.findByText(label(first[0]))).toBeInTheDocument();
  });

  it('a página de produtos ativos que fica vazia depois do arquivamento volta à anterior', async () => {
    let lastArchived = false;
    const last = product(2, 'Último produto ativo');
    twoPages('/api/v1/products', [product(1, 'Produto da primeira página')], () => (lastArchived ? [] : [last]));
    server.use(
      http.patch(`/api/v1/products/${last.id}`, async ({ request }) => {
        lastArchived = true;
        const body = (await request.json()) as Partial<Product>;
        return HttpResponse.json({ ...last, ...body, version: 2 }, { headers: { ETag: '"2"' } });
      }),
    );
    const { user } = renderPage(<ProductsPage />);
    await screen.findByText('Produto da primeira página');
    await user.click(screen.getByRole('button', { name: 'Próxima' }));
    await user.click(await screen.findByRole('button', { name: 'Editar produto Último produto ativo' }));
    const dialog = await screen.findByRole('dialog', { name: 'Editar produto' });
    await user.click(within(dialog).getByRole('textbox', { name: 'Situação' }));
    await user.click(await screen.findByRole('option', { name: 'Arquivado' }));
    await user.click(within(dialog).getByRole('button', { name: 'Salvar' }));
    expect(await screen.findByText('Produto da primeira página')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('Página 1')).toBeInTheDocument());
    expect(screen.queryByText('Nenhum produto')).not.toBeInTheDocument();
  });
});

describe('campos de seleção com todas as páginas', () => {
  it('o produto do novo plano oferece também os produtos da segunda página', async () => {
    server.use(
      http.get('/api/v1/metrics', () => HttpResponse.json({ data: [], next_cursor: null })),
      http.get('/api/v1/plans', () => HttpResponse.json({ data: [], next_cursor: null })),
    );
    const requests = twoPages('/api/v1/products', [product(1, 'Produto Zeta')], () => [product(2, 'Produto Alfa da segunda página')]);
    const { user } = renderPage(<PlansPage />);
    await user.click(await screen.findByRole('button', { name: 'Novo plano' }));
    const dialog = await screen.findByRole('dialog', { name: 'Novo plano' });
    await user.click(within(dialog).getByRole('textbox', { name: /Produto/ }));
    const options = await screen.findAllByRole('option');
    // Os itens das duas páginas, pelo nome.
    expect(options.map((o) => o.textContent)).toEqual(['Produto Alfa da segunda página', 'Produto Zeta']);
    expect(params(requests, 'status', 'limit', 'cursor')).toEqual(
      expect.arrayContaining([
        ['active', '100', null],
        ['active', '100', 'p2'],
      ]),
    );
  });

  it('o plano da nova assinatura oferece também os planos da segunda página', async () => {
    server.use(
      http.get('/api/v1/metrics', () => HttpResponse.json({ data: [], next_cursor: null })),
      http.get('/api/v1/customers', () => HttpResponse.json({ data: [], next_cursor: null })),
    );
    const requests = twoPages('/api/v1/plans', [plan(1, 'Básico')], () => [plan(2, 'Profissional da segunda página')]);
    const { user } = renderPage(<SubscriptionCreateModal opened onClose={() => {}} />);
    await user.click(screen.getByRole('textbox', { name: /Plano/ }));
    expect(await screen.findByRole('option', { name: 'Profissional da segunda página' })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: 'Básico' })).toBeInTheDocument();
    expect(params(requests, 'status', 'limit', 'cursor')).toEqual([
      ['active', '100', null],
      ['active', '100', 'p2'],
    ]);
  });
});
