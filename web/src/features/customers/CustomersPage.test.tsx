import { screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { visibleNavigation } from '../../app/navigation';
import { buildSessionValue } from '../../session/SessionContext';
import { AUDITOR_PERMISSIONS, makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { CustomersPage } from './CustomersPage';

const customer = {
  id: '0195f1a0-0000-7000-8000-00000000d001',
  kind: 'company',
  name: 'Cliente Exemplo Ltda',
  document_type: 'cnpj',
  document_number: '11222333000181',
  email: 'financeiro@exemplo.test',
  status: 'active',
  created_at: '2026-11-01T12:00:00Z',
  updated_at: '2026-11-01T12:00:00Z',
  version: 1,
};

describe('visibilidade de ações por permissão', () => {
  it('quem tem customers.write vê "Novo cliente"', async () => {
    const requests: URL[] = [];
    server.use(
      http.get('/api/v1/customers', ({ request }) => {
        requests.push(new URL(request.url));
        return HttpResponse.json({ data: [customer], next_cursor: 'c2' });
      }),
    );
    renderPage(<CustomersPage />, { path: '/clientes' });
    expect(await screen.findByText('Cliente Exemplo Ltda')).toBeInTheDocument();
    expect(screen.getByText('11.222.333/0001-81')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Novo cliente' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Próxima/ })).toBeEnabled();
    expect(requests[0].searchParams.get('status')).toBe('active');
    expect(requests[0].searchParams.get('sort')).toBe('name');
  });

  it('auditor não vê botões de escrita', async () => {
    server.use(http.get('/api/v1/customers', () => HttpResponse.json({ data: [customer], next_cursor: null })));
    renderPage(<CustomersPage />, { path: '/clientes', session: makeSession({ permissions: AUDITOR_PERMISSIONS }) });
    expect(await screen.findByText('Cliente Exemplo Ltda')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Novo cliente' })).not.toBeInTheDocument();
  });

  it('lista vazia orienta o cadastro', async () => {
    server.use(http.get('/api/v1/customers', () => HttpResponse.json({ data: [], next_cursor: null })));
    renderPage(<CustomersPage />, { path: '/clientes' });
    expect(await screen.findByText('Nenhum cliente cadastrado')).toBeInTheDocument();
  });

  it('erro 403 mostra acesso negado', async () => {
    server.use(
      http.get('/api/v1/customers', () =>
        HttpResponse.json({ type: 'about:blank', title: 'Proibido', status: 403, code: 'forbidden' }, { status: 403 }),
      ),
    );
    renderPage(<CustomersPage />, { path: '/clientes' });
    expect(await screen.findByText('Acesso negado')).toBeInTheDocument();
  });

  it('navegação lateral esconde seções sem permissão de leitura', () => {
    const operator = buildSessionValue(
      makeSession({ permissions: ['customers.read', 'customers.write', 'dashboard.read'] }),
    );
    const labels = visibleNavigation(operator.can).map((e) => e.label);
    expect(labels).toContain('Clientes');
    expect(labels).not.toContain('Auditoria');
  });
});
