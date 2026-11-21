import { screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { SubscriptionsPage } from './SubscriptionsPage';

describe('filtros da lista de assinaturas', () => {
  it('em vigor pela URL (link do painel) consulta in_force e mostra o filtro ligado', async () => {
    const queries: URLSearchParams[] = [];
    server.use(
      http.get('/api/v1/subscriptions', ({ request }) => {
        queries.push(new URL(request.url).searchParams);
        return HttpResponse.json({ data: [], next_cursor: null });
      }),
    );
    const { user } = renderPage(<SubscriptionsPage />, { path: '/assinaturas', route: '/assinaturas?in_force=true' });
    const toggle = await screen.findByRole('switch', { name: 'Somente em vigor (trial, ativas e em atraso)' });
    expect(toggle).toBeChecked();
    await waitFor(() => expect(queries.at(-1)?.get('in_force')).toBe('true'));
    expect(queries.at(-1)?.has('status')).toBe(false);
    expect(await screen.findByText('Nenhuma assinatura encontrada')).toBeInTheDocument();

    await user.click(toggle);
    await waitFor(() => expect(queries.at(-1)?.has('in_force')).toBe(false));
  });
});
