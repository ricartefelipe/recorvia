import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { CustomerDetailPage } from './CustomerDetailPage';

const ID = '0195f1a0-0000-7000-8000-00000000d001';

const address = {
  street: 'Avenida Paulista',
  number: '1000',
  complement: '',
  district: 'Bela Vista',
  city: 'São Paulo',
  state: 'SP',
  postal_code: '01310100',
  country: 'BR',
};

const customer = {
  id: ID,
  kind: 'company',
  name: 'Cliente Exemplo Ltda',
  legal_name: '',
  document_type: 'cnpj',
  document_number: '11222333000181',
  email: 'financeiro@exemplo.test',
  phone: '',
  external_id: '',
  status: 'active',
  payment_terms_days: null,
  anonymized_at: null,
  created_at: '2026-11-01T12:00:00Z',
  updated_at: '2026-11-01T12:00:00Z',
  version: 3,
  contacts: [],
  billing_address: address,
};

/** A ficha devolve a versão corrente do cliente no servidor simulado. */
function renderCustomer(tab?: string) {
  const backend = { current: customer };
  server.use(http.get(`/api/v1/customers/${ID}`, () => HttpResponse.json(backend.current)));
  const route = tab ? `/clientes/${ID}?aba=${tab}` : `/clientes/${ID}`;
  return { backend, ...renderPage(<CustomerDetailPage />, { path: '/clientes/:id', route }) };
}

describe('versão do cliente nas alterações', () => {
  it('arquivar envia If-Match com a versão do cliente', async () => {
    const requests: Request[] = [];
    const { backend, user } = renderCustomer();
    server.use(
      http.post(`/api/v1/customers/${ID}/archive`, ({ request }) => {
        requests.push(request.clone());
        backend.current = { ...customer, status: 'archived', version: 4 };
        return HttpResponse.json(backend.current, { headers: { ETag: '"4"' } });
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Arquivar' }));
    const dialog = (await screen.findByText(/Arquivar Cliente Exemplo Ltda\?/)).closest('[role="dialog"]') as HTMLElement;
    await user.click(within(dialog).getByRole('button', { name: 'Arquivar' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0].headers.get('If-Match')).toBe('"3"');
  });

  it('endereço envia If-Match com a versão do cliente e a próxima gravação usa a versão nova', async () => {
    const versions: (string | null)[] = [];
    const { backend, user } = renderCustomer();
    server.use(
      http.put(`/api/v1/customers/${ID}/billing-address`, async ({ request }) => {
        versions.push(request.headers.get('If-Match'));
        const body = (await request.json()) as typeof address;
        const next = backend.current.version + 1;
        backend.current = { ...backend.current, billing_address: body, version: next };
        return HttpResponse.json(body, { headers: { ETag: `"${next}"` } });
      }),
    );
    const city = await screen.findByLabelText(/Cidade/);
    await user.clear(city);
    await user.type(city, 'Campinas');
    await user.click(screen.getByRole('button', { name: 'Salvar endereço' }));
    await waitFor(() => expect(versions).toEqual(['"3"']));
    // A ficha é recarregada com a versão gerada pelo endereço.
    await waitFor(() => expect(screen.getByLabelText(/Cidade/)).toHaveValue('Campinas'));
    const number = screen.getByLabelText(/Número/);
    await user.clear(number);
    await user.type(number, '1001');
    await user.click(screen.getByRole('button', { name: 'Salvar endereço' }));
    await waitFor(() => expect(versions).toEqual(['"3"', '"4"']));
  });

  it('movimentos de crédito seguem o next_cursor', async () => {
    const entry = (id: string, reason: string) => ({ id, amount: 1500, kind: 'grant', invoice_id: null, reason, created_at: '2026-11-10T12:00:00Z' });
    const cursors: (string | null)[] = [];
    server.use(
      http.get(`/api/v1/customers/${ID}/credits`, ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('cursor');
        cursors.push(cursor);
        return HttpResponse.json(
          cursor
            ? { balance: 3000, currency: 'BRL', entries: [entry('e2', 'Cortesia antiga')], next_cursor: null }
            : { balance: 3000, currency: 'BRL', entries: [entry('e1', 'Cortesia recente')], next_cursor: 'cred-2' },
        );
      }),
    );
    const { user } = renderCustomer('creditos');
    expect(await screen.findByText('Cortesia recente')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /Próxima/ }));
    expect(await screen.findByText('Cortesia antiga')).toBeInTheDocument();
    expect(cursors).toEqual([null, 'cred-2']);
  });
});
