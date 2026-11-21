import { screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { AddonOrder, PlatformAddon, Subscription } from '../../api/types';
import { makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { OrderAddonModal } from '../platform/PlatformPage';
import { subscriptionsApi } from './api';
import { SubscriptionActions } from './SubscriptionActions';
import { SubscriptionCreateModal } from './SubscriptionCreateModal';

// RF-09 e ADR-075: criação de assinatura, aplicação de cupom, reajuste
// programado e pedido de serviço adicional enviam Idempotency-Key. Como nas
// demais operações financeiras de criação, a chave é mantida depois de erro
// 5xx (a repetição do mesmo envio não duplica o efeito) e descartada depois
// do sucesso.

const CUSTOMER_ID = '0195f1a0-0000-7000-8000-00000000d001';
const PLAN_ID = '0195f1a0-0000-7000-8000-00000000b101';

const subscription: Subscription = {
  id: '0195f1a0-0000-7000-8000-00000000e001',
  customer_id: CUSTOMER_ID,
  customer_name: 'Cliente Exemplo Ltda',
  plan_id: PLAN_ID,
  plan_name: 'Profissional',
  price_version_id: '0195f1a0-0000-7000-8000-00000000b001',
  status: 'active',
  quantity: 1,
  billing_interval: 'month',
  timezone: 'America/Sao_Paulo',
  anchor_date: '2026-11-01',
  started_at: '2026-11-01T03:00:00Z',
  current_period_start: '2026-11-01T03:00:00Z',
  current_period_end: '2026-12-01T03:00:00Z',
  cancel_at_period_end: false,
  created_at: '2026-11-01T03:00:00Z',
  version: 1,
};

/**
 * Responde como o middleware de idempotência: o primeiro pedido falha com
 * 503 (não registrado); os seguintes são atendidos. Devolve as chaves e os
 * corpos recebidos.
 */
function failOnceThen(path: string, ok: () => Response) {
  const requests: { key: string | null; body: unknown }[] = [];
  server.use(
    http.post(path, async ({ request }) => {
      requests.push({ key: request.headers.get('Idempotency-Key'), body: await request.json() });
      if (requests.length === 1) {
        return HttpResponse.json({ status: 503, code: 'unavailable', title: 'Indisponível', detail: 'Tente novamente.' }, { status: 503 });
      }
      return ok();
    }),
  );
  return requests;
}

describe('Idempotency-Key nas operações financeiras de criação', () => {
  it('criação de assinatura envia a chave e a repete depois de 5xx', async () => {
    server.use(
      http.get('/api/v1/plans', () =>
        HttpResponse.json({
          data: [{ id: PLAN_ID, name: 'Profissional', status: 'active', billing_interval: 'month', trial_days: 0, current_price: null }],
        }),
      ),
      http.get('/api/v1/metrics', () => HttpResponse.json({ data: [] })),
      http.get('/api/v1/customers', () => HttpResponse.json({ data: [{ id: CUSTOMER_ID, name: 'Cliente Exemplo Ltda', status: 'active' }] })),
      http.get('/api/v1/customers/:id', () => HttpResponse.json({ id: CUSTOMER_ID, name: 'Cliente Exemplo Ltda', status: 'active' })),
    );
    const requests = failOnceThen('/api/v1/subscriptions', () => HttpResponse.json(subscription, { status: 201 }));
    const { user } = renderPage(<SubscriptionCreateModal opened onClose={() => {}} customerId={CUSTOMER_ID} />);
    await user.click(screen.getByRole('textbox', { name: /Plano/ }));
    await user.click(await screen.findByRole('option', { name: 'Profissional' }));
    await user.click(screen.getByRole('button', { name: 'Criar assinatura' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    await user.click(screen.getByRole('button', { name: 'Criar assinatura' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(requests[0].key).toBeTruthy();
    expect(requests[1].key).toBe(requests[0].key);
    expect(requests[1].body).toEqual({ customer_id: CUSTOMER_ID, plan_id: PLAN_ID, quantity: 1 });
  });

  it('aplicação de cupom envia a chave, a repete depois de 5xx e usa outra no próximo envio', async () => {
    server.use(
      http.get('/api/v1/plans', () => HttpResponse.json({ data: [] })),
      http.get('/api/v1/metrics', () => HttpResponse.json({ data: [] })),
    );
    const requests = failOnceThen('/api/v1/subscriptions/:id/discounts', () =>
      HttpResponse.json(
        { id: '0195f1a0-0000-7000-8000-00000000f001', coupon_id: '0195f1a0-0000-7000-8000-00000000f101', coupon_code: 'FIDELIDADE', status: 'active' },
        { status: 201 },
      ),
    );
    const { user } = renderPage(<SubscriptionActions subscription={subscription} />, {
      session: makeSession({ permissions: ['subscriptions.read', 'subscriptions.write'] }),
    });
    const apply = async () => {
      await user.click(screen.getByRole('button', { name: 'Ações' }));
      await user.click(await screen.findByRole('menuitem', { name: 'Aplicar cupom' }));
      await user.type(screen.getByLabelText(/Código do cupom/), 'FIDELIDADE');
      await user.click(screen.getByRole('button', { name: 'Aplicar cupom' }));
    };
    await apply();
    await waitFor(() => expect(requests).toHaveLength(1));
    await user.click(screen.getByRole('button', { name: 'Aplicar cupom' }));
    expect(await screen.findByText('Cupom FIDELIDADE aplicado.')).toBeInTheDocument();
    expect(requests[0].key).toBeTruthy();
    expect(requests[1].key).toBe(requests[0].key);
    expect(requests[1].body).toEqual({ coupon_code: 'FIDELIDADE' });

    await apply();
    await waitFor(() => expect(requests).toHaveLength(3));
    expect(requests[2].key).not.toBe(requests[0].key);
  });

  it('pedido de serviço adicional envia a chave e a repete depois de 5xx', async () => {
    const addon: PlatformAddon = { code: 'implantacao_assistida', name: 'Implantação assistida', description: 'Apoio na implantação.', price: 150000, currency: 'BRL' } as PlatformAddon;
    const order = { id: '0195f1a0-0000-7000-8000-00000000a001', addon_code: addon.code, status: 'requested' } as AddonOrder;
    const requests = failOnceThen('/api/v1/platform/addon-orders', () => HttpResponse.json(order, { status: 201 }));
    const { user } = renderPage(<OrderAddonModal addon={addon} onClose={() => {}} />);
    await user.type(screen.getByLabelText(/Observações/), 'Ligar pela manhã');
    await user.click(screen.getByRole('button', { name: 'Enviar pedido' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    await user.click(screen.getByRole('button', { name: 'Enviar pedido' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(requests[0].key).toBeTruthy();
    expect(requests[1].key).toBe(requests[0].key);
    expect(requests[1].body).toEqual({ addon_code: addon.code, notes: 'Ligar pela manhã' });
  });

  it('reajuste programado envia a chave informada', async () => {
    const keys: (string | null)[] = [];
    server.use(
      http.post('/api/v1/subscriptions/:id/price-adjustments', ({ request }) => {
        keys.push(request.headers.get('Idempotency-Key'));
        return HttpResponse.json({ id: '0195f1a0-0000-7000-8000-00000000c001' }, { status: 201 });
      }),
    );
    await subscriptionsApi.scheduleAdjustment(subscription.id, { effective_date: '2026-12-01', kind: 'percent', percent_bp: 450, reason: 'Reajuste anual' }, 'chave-reajuste-1');
    expect(keys).toEqual(['chave-reajuste-1']);
  });
});
