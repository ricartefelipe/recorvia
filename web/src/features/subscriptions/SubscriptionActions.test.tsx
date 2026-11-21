import { screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { Subscription } from '../../api/types';
import type { Permission } from '../../session/permissions';
import { AUDITOR_PERMISSIONS, makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { SubscriptionActions } from './SubscriptionActions';

const subscription: Subscription = {
  id: '0195f1a0-0000-7000-8000-00000000e001',
  customer_id: '0195f1a0-0000-7000-8000-00000000d001',
  customer_name: 'Cliente Exemplo Ltda',
  plan_id: '0195f1a0-0000-7000-8000-00000000b101',
  plan_name: 'Profissional',
  price_version_id: '0195f1a0-0000-7000-8000-00000000b001',
  status: 'active',
  quantity: 3,
  billing_interval: 'month',
  timezone: 'America/Sao_Paulo',
  anchor_date: '2026-11-01',
  started_at: '2026-11-01T03:00:00Z',
  current_period_start: '2026-11-01T03:00:00Z',
  current_period_end: '2026-12-01T03:00:00Z',
  cancel_at_period_end: true,
  created_at: '2026-11-01T03:00:00Z',
  version: 2,
};

function render(permissions: readonly Permission[]) {
  server.use(
    http.get('/api/v1/plans', () => HttpResponse.json({ data: [] })),
    http.get('/api/v1/metrics', () => HttpResponse.json({ data: [] })),
  );
  return renderPage(<SubscriptionActions subscription={subscription} />, { session: makeSession({ permissions }) });
}

describe('ações da assinatura por permissão', () => {
  it('operador altera a assinatura, mas não cancela', async () => {
    const { user } = render(['subscriptions.read', 'subscriptions.write']);
    await user.click(screen.getByRole('button', { name: 'Ações' }));
    expect(await screen.findByRole('menuitem', { name: 'Trocar plano' })).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'Suspender' })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: 'Cancelar assinatura' })).not.toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: 'Desfazer cancelamento agendado' })).not.toBeInTheDocument();
  });

  it('financeiro cancela ou desfaz o cancelamento agendado', async () => {
    const { user } = render(['subscriptions.read', 'subscriptions.cancel']);
    await user.click(screen.getByRole('button', { name: 'Ações' }));
    expect(await screen.findByRole('menuitem', { name: 'Cancelar assinatura' })).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'Desfazer cancelamento agendado' })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: 'Trocar plano' })).not.toBeInTheDocument();
  });

  it('auditor não vê o menu de ações', () => {
    render(AUDITOR_PERMISSIONS);
    expect(screen.queryByRole('button', { name: 'Ações' })).not.toBeInTheDocument();
  });

  it('cancelamento envia If-Match com a versão e exige confirmação', async () => {
    const requests: Request[] = [];
    server.use(
      http.post('/api/v1/subscriptions/:id/cancel', ({ request }) => {
        requests.push(request.clone());
        return HttpResponse.json({ ...subscription, status: 'canceled', version: 3 });
      }),
    );
    const { user } = render(['subscriptions.read', 'subscriptions.cancel']);
    await user.click(screen.getByRole('button', { name: 'Ações' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Cancelar assinatura' }));
    await user.click(screen.getByLabelText('Imediatamente'));
    await user.click(screen.getByRole('button', { name: 'Continuar' }));
    expect(await screen.findByText(/não pode ser desfeita/)).toBeInTheDocument();
    expect(requests).toHaveLength(0);
    await user.click(screen.getByRole('button', { name: 'Cancelar agora' }));
    await screen.findByText('Assinatura cancelada.');
    expect(requests[0].headers.get('If-Match')).toBe('"2"');
    expect(await requests[0].json()).toEqual({ mode: 'immediate' });
  });

  it('mudança imediata explica a fatura no aumento e o crédito na redução', async () => {
    const { user } = render(['subscriptions.read', 'subscriptions.write']);
    await user.click(screen.getByRole('button', { name: 'Ações' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Alterar quantidade' }));
    expect(screen.getByRole('radio', { name: /Imediatamente/ })).toBeInTheDocument();
    expect(
      screen.getByText(
        'Vale agora, com prorrata do período corrente: no aumento, gera fatura pela diferença; na redução, a diferença vira crédito do cliente, abatido nas próximas faturas.',
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Gera fatura de prorrata pela diferença/)).not.toBeInTheDocument();
  });

  it('suspensão envia If-Match, o motivo e confirma', async () => {
    const requests: Request[] = [];
    server.use(
      http.post('/api/v1/subscriptions/:id/suspend', ({ request }) => {
        requests.push(request.clone());
        return HttpResponse.json({ ...subscription, status: 'suspended', version: 3 });
      }),
    );
    const { user } = render(['subscriptions.read', 'subscriptions.write']);
    await user.click(screen.getByRole('button', { name: 'Ações' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Suspender' }));
    expect(screen.getByText('A assinatura deixa de gerar novas faturas até ser reativada.')).toBeInTheDocument();
    await user.type(screen.getByLabelText('Motivo'), 'Inadimplência em negociação');
    await user.click(screen.getByRole('button', { name: 'Suspender' }));
    await screen.findByText('Assinatura suspensa.');
    expect(requests).toHaveLength(1);
    expect(requests[0].headers.get('If-Match')).toBe('"2"');
    expect(await requests[0].json()).toEqual({ reason: 'Inadimplência em negociação' });
  });

  it('reativação pede confirmação, explica a cobrança e envia If-Match', async () => {
    const requests: Request[] = [];
    server.use(
      http.post('/api/v1/subscriptions/:id/reactivate', ({ request }) => {
        requests.push(request.clone());
        return HttpResponse.json({ ...subscription, status: 'active', version: 4 });
      }),
    );
    const suspended: Subscription = { ...subscription, status: 'suspended', cancel_at_period_end: false, version: 3 };
    server.use(
      http.get('/api/v1/plans', () => HttpResponse.json({ data: [] })),
      http.get('/api/v1/metrics', () => HttpResponse.json({ data: [] })),
    );
    const { user } = renderPage(<SubscriptionActions subscription={suspended} />, {
      session: makeSession({ permissions: ['subscriptions.read', 'subscriptions.write'] }),
    });
    await user.click(screen.getByRole('button', { name: 'Ações' }));
    expect(screen.queryByRole('menuitem', { name: 'Suspender' })).not.toBeInTheDocument();
    await user.click(await screen.findByRole('menuitem', { name: 'Reativar' }));
    expect(
      await screen.findByText(
        'A assinatura volta a ser faturada a partir do período atual, se ele ainda não foi faturado. Os períodos inteiros em que ficou suspensa não são cobrados.',
      ),
    ).toBeInTheDocument();
    expect(requests).toHaveLength(0);
    await user.click(screen.getByRole('button', { name: 'Reativar' }));
    await screen.findByText('Assinatura reativada.');
    expect(requests).toHaveLength(1);
    expect(requests[0].headers.get('If-Match')).toBe('"3"');
  });
});
