import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { WebhookDelivery, WebhookEndpoint } from '../../api/types';
import { AUDITOR_PERMISSIONS, makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { WebhookDeliveriesPage, WebhooksTab } from './WebhooksTab';

const endpoint: WebhookEndpoint = {
  id: '0195f1a0-0000-7000-8000-0000000e0001',
  url: 'https://erp.cliente.test/webhooks/recorvia',
  description: 'ERP financeiro',
  event_types: ['invoice.issued', 'invoice.paid'],
  status: 'active',
  created_at: '2026-11-10T12:00:00Z',
  version: 3,
  secrets: [
    { id: '0195f1a0-0000-7000-8000-0000000e5002', created_at: '2026-11-16T12:00:00Z', expires_at: null },
    { id: '0195f1a0-0000-7000-8000-0000000e5001', created_at: '2026-11-10T12:00:00Z', expires_at: '2026-11-17T12:00:00Z' },
  ],
};

function delivery(id: string, status: WebhookDelivery['status'], extra: Partial<WebhookDelivery> = {}): WebhookDelivery {
  return {
    id,
    endpoint_id: endpoint.id,
    event_id: '0195f1a0-0000-7000-8000-0000000ee001',
    event_type: 'invoice.paid',
    status,
    attempts: 1,
    next_attempt_at: null,
    last_status_code: null,
    last_error: '',
    delivered_at: null,
    created_at: '2026-11-16T12:00:00Z',
    ...extra,
  };
}

describe('webhooks enviados', () => {
  it('exibe o segredo somente na criação e envia o corpo do contrato', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.get('/api/v1/webhook-endpoints', () => HttpResponse.json({ data: [endpoint] })),
      http.get('/api/v1/webhook-event-types', () => HttpResponse.json({ data: ['invoice.issued', 'invoice.overdue', 'invoice.paid'] })),
      http.post('/api/v1/webhook-endpoints', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(
          { endpoint: { ...endpoint, id: '0195f1a0-0000-7000-8000-0000000e0002', url: 'https://novo.cliente.test/hook', version: 1, secrets: [endpoint.secrets[0]] }, secret: 'whsec_segredo-exibido-uma-vez' },
          { status: 201 },
        );
      }),
    );
    const { user } = renderPage(<WebhooksTab />);
    const table = await screen.findByRole('table', { name: 'Endpoints de webhook' });
    expect(within(table).getByText('2 ativo(s)')).toBeInTheDocument();
    expect(screen.queryByText(/whsec_/)).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Novo endpoint' }));
    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByLabelText(/URL/), 'https://novo.cliente.test/hook');
    await user.click(within(dialog).getByRole('textbox', { name: /Tipos de evento/ }));
    await user.click(await screen.findByRole('option', { name: 'invoice.overdue' }));
    await user.click(within(dialog).getByRole('button', { name: 'Salvar' }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ url: 'https://novo.cliente.test/hook', description: '', event_types: ['invoice.overdue'] });
    expect(await screen.findByDisplayValue('whsec_segredo-exibido-uma-vez')).toBeInTheDocument();
  });

  it('reenvia entregas concluídas ou em retentativa, nunca as pendentes', async () => {
    const retried: string[] = [];
    server.use(
      http.get('/api/v1/webhook-endpoints', () => HttpResponse.json({ data: [endpoint] })),
      http.get('/api/v1/webhook-endpoints/:id/deliveries', () =>
        HttpResponse.json({
          data: [
            delivery('0195f1a0-0000-7000-8000-0000000d0001', 'succeeded', { last_status_code: 204, delivered_at: '2026-11-16T12:00:01Z' }),
            delivery('0195f1a0-0000-7000-8000-0000000d0002', 'exhausted', { attempts: 10, last_status_code: 500, last_error: 'resposta sem sucesso do destino' }),
            delivery('0195f1a0-0000-7000-8000-0000000d0003', 'pending', { attempts: 0, next_attempt_at: '2026-11-16T12:05:00Z' }),
          ],
          next_cursor: null,
        }),
      ),
      http.post('/api/v1/webhook-deliveries/:id/retry', ({ params }) => {
        retried.push(String(params.id));
        return HttpResponse.json(delivery(String(params.id), 'pending'));
      }),
    );
    const { user } = renderPage(<WebhookDeliveriesPage />, { path: '/integracoes/webhooks/:id', route: `/integracoes/webhooks/${endpoint.id}` });
    const table = await screen.findByRole('table', { name: 'Entregas' });
    expect(within(table).getAllByRole('button', { name: 'Reenviar' })).toHaveLength(2);
    expect(within(table).getByText('resposta sem sucesso do destino')).toBeInTheDocument();
    await user.click(within(table).getAllByRole('button', { name: 'Reenviar' })[0]);
    await waitFor(() => expect(retried).toEqual(['0195f1a0-0000-7000-8000-0000000d0001']));
  });

  it('auditor consulta as entregas sem reenviar', async () => {
    server.use(
      http.get('/api/v1/webhook-endpoints', () => HttpResponse.json({ data: [endpoint] })),
      http.get('/api/v1/webhook-endpoints/:id/deliveries', () =>
        HttpResponse.json({ data: [delivery('0195f1a0-0000-7000-8000-0000000d0002', 'exhausted')], next_cursor: null }),
      ),
    );
    renderPage(<WebhookDeliveriesPage />, {
      path: '/integracoes/webhooks/:id',
      route: `/integracoes/webhooks/${endpoint.id}`,
      session: makeSession({ permissions: AUDITOR_PERMISSIONS }),
    });
    const table = await screen.findByRole('table', { name: 'Entregas' });
    expect(within(table).getByText('Tentativas esgotadas')).toBeInTheDocument();
    expect(within(table).queryByRole('button', { name: 'Reenviar' })).not.toBeInTheDocument();
  });
});
