import { screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { generateSecret } from './api';
import { ProvidersTab } from './ProvidersTab';

describe('provedores de pagamento', () => {
  it('nunca mostra a chave salva; exibe só o final e a URL do endpoint uma vez', async () => {
    const bodies: unknown[] = [];
    const versions: (string | null)[] = [];
    server.use(
      http.get('/api/v1/payment-providers', () =>
        HttpResponse.json({
          data: [{ id: 'a1', provider: 'asaas', environment: 'sandbox', status: 'active', is_default: true, api_key_hint: 'f00d', created_at: '2026-11-01T12:00:00Z', version: 5 }],
          available: ['simulator', 'asaas'],
        }),
      ),
      http.put('/api/v1/payment-providers/:provider', async ({ request }) => {
        versions.push(request.headers.get('If-Match'));
        bodies.push(await request.json());
        return HttpResponse.json({
          account: { id: 'a2', provider: 'simulator', environment: 'local', status: 'active', is_default: true, api_key_hint: '7890', created_at: '2026-11-16T12:00:00Z', version: 1 },
          webhook_endpoint_url: 'https://app.recorvia.test/webhooks/providers/simulator/tok_123',
        });
      }),
    );
    const { user } = renderPage(<ProvidersTab />);
    expect(await screen.findByText('••••f00d')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Configurar' }));
    await user.type(screen.getByLabelText(/Chave de API/), 'sim_key_1234567890');
    await user.click(screen.getByRole('button', { name: 'Gerar segredo' }));
    await user.click(screen.getByRole('button', { name: 'Salvar configuração' }));
    expect(await screen.findByLabelText('URL do endpoint')).toHaveValue('https://app.recorvia.test/webhooks/providers/simulator/tok_123');
    await waitFor(() => expect(bodies).toHaveLength(1));
    const body = bodies[0] as { environment: string; api_key: string; webhook_secret: string; make_default: boolean };
    expect(body.environment).toBe('local');
    expect(body.api_key).toBe('sim_key_1234567890');
    expect(body.webhook_secret.length).toBeGreaterThanOrEqual(32);
    // Provedor ainda não configurado: versão 0.
    expect(versions).toEqual(['"0"']);
  });

  it('reconfiguração envia If-Match com a versão da conta do provedor', async () => {
    const versions: (string | null)[] = [];
    const account = { id: 'a1', provider: 'asaas', environment: 'sandbox', status: 'active', is_default: true, api_key_hint: 'f00d', created_at: '2026-11-01T12:00:00Z', version: 5 };
    server.use(
      http.get('/api/v1/payment-providers', () => HttpResponse.json({ data: [account], available: ['asaas'] })),
      http.put('/api/v1/payment-providers/:provider', ({ request }) => {
        versions.push(request.headers.get('If-Match'));
        return HttpResponse.json(
          { account: { ...account, version: 6 }, webhook_endpoint_url: 'https://app.recorvia.test/webhooks/providers/asaas/tok_456' },
          { headers: { ETag: '"6"' } },
        );
      }),
    );
    const { user } = renderPage(<ProvidersTab />);
    await user.click(await screen.findByRole('button', { name: 'Reconfigurar credenciais' }));
    await user.type(screen.getByLabelText(/Chave de API/), 'aact_chave_1234567890');
    await user.click(screen.getByRole('button', { name: 'Gerar segredo' }));
    await user.click(screen.getByRole('button', { name: 'Salvar configuração' }));
    await waitFor(() => expect(versions).toEqual(['"5"']));
  });

  it('gera segredos aleatórios com tamanho suficiente', () => {
    const a = generateSecret();
    expect(a).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(generateSecret()).not.toBe(a);
  });
});
