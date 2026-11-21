import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { CouponsPage } from './CouponsPage';
import { PlanDetailPage } from './PlanDetailPage';

const PLAN_ID = '0195f1a0-0000-7000-8000-0000000aa001';
const PRODUCT_ID = '0195f1a0-0000-7000-8000-0000000bb001';

const price = (version: number, amount: number) => ({
  id: `0195f1a0-0000-7000-8000-00000000c00${version}`,
  version,
  pricing_model: 'flat',
  flat_amount: amount,
  unit_amount_decimal: null,
  metric_id: null,
  included_quantity: null,
  currency: 'BRL',
  created_at: '2026-11-01T12:00:00Z',
});

const plan = {
  id: PLAN_ID,
  product_id: PRODUCT_ID,
  code: 'pro',
  name: 'Profissional',
  description: '',
  billing_interval: 'month',
  currency: 'BRL',
  trial_days: 0,
  status: 'active',
  current_price: price(1, 9990),
  created_at: '2026-11-01T12:00:00Z',
  version: 6,
  prices: [price(1, 9990)],
};

const coupon = {
  id: '0195f1a0-0000-7000-8000-0000000cc001',
  code: 'BEMVINDO',
  name: 'Boas-vindas',
  discount_type: 'percent',
  percent_off_bp: 1000,
  duration: 'once',
  applies_to_plan_ids: [],
  redemptions_count: 0,
  status: 'active',
  currency: 'BRL',
  created_at: '2026-11-01T12:00:00Z',
  version: 2,
};

describe('versão no catálogo', () => {
  it('nova versão de preço envia If-Match com a versão do plano e recarrega o plano', async () => {
    const backend = { current: plan, reads: 0 };
    const requests: Request[] = [];
    server.use(
      http.get(`/api/v1/plans/${PLAN_ID}`, () => {
        backend.reads += 1;
        return HttpResponse.json(backend.current);
      }),
      http.get('/api/v1/products', () => HttpResponse.json({ data: [] })),
      http.get('/api/v1/metrics', () => HttpResponse.json({ data: [] })),
      http.post(`/api/v1/plans/${PLAN_ID}/prices`, ({ request }) => {
        requests.push(request.clone());
        const created = price(2, 12990);
        backend.current = { ...plan, current_price: created, prices: [price(1, 9990), created], version: 7 };
        return HttpResponse.json(created, { status: 201, headers: { ETag: '"7"' } });
      }),
    );
    const { user } = renderPage(<PlanDetailPage />, { path: '/catalogo/planos/:id', route: `/catalogo/planos/${PLAN_ID}` });
    await user.click(await screen.findByRole('button', { name: 'Nova versão de preço' }));
    await user.click(await screen.findByRole('button', { name: 'Criar versão 2' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0].headers.get('If-Match')).toBe('"6"');
    // O plano é recarregado: a próxima alteração usa a versão 7.
    await waitFor(() => expect(backend.reads).toBe(2));
  });

  it('arquivar cupom envia If-Match com a versão do cupom', async () => {
    const requests: Request[] = [];
    server.use(
      http.get('/api/v1/coupons', () => HttpResponse.json({ data: [coupon] })),
      http.get('/api/v1/plans', () => HttpResponse.json({ data: [] })),
      http.post(`/api/v1/coupons/${coupon.id}/archive`, ({ request }) => {
        requests.push(request.clone());
        return HttpResponse.json({ ...coupon, status: 'archived', version: 3 }, { headers: { ETag: '"3"' } });
      }),
    );
    const { user } = renderPage(<CouponsPage />, { path: '/catalogo/cupons' });
    await user.click(await screen.findByRole('button', { name: 'Arquivar cupom BEMVINDO' }));
    const dialog = (await screen.findByText(/Arquivar o cupom BEMVINDO\?/)).closest('[role="dialog"]') as HTMLElement;
    await user.click(within(dialog).getByRole('button', { name: 'Arquivar' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0].headers.get('If-Match')).toBe('"2"');
  });
});
