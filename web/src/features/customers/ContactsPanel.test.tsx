import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { AuditEvent, Contact } from '../../api/types';
import { AUDITOR_PERMISSIONS, makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { CustomerDetailPage } from './CustomerDetailPage';

const CUSTOMER_ID = '0195f1a0-0000-7000-8000-00000000d001';
const contact: Contact = {
  id: '0195f1a0-0000-7000-8000-00000000f001',
  name: 'Ana Lima',
  email: 'ana@cliente.test',
  phone: '',
  role: 'Financeiro',
  receives_billing_email: true,
  created_at: '2026-11-01T12:00:00Z',
};
const customer = {
  id: CUSTOMER_ID,
  kind: 'company',
  name: 'Cliente Exemplo Ltda',
  document_type: 'cnpj',
  document_number: '11222333000181',
  email: 'financeiro@exemplo.test',
  status: 'active',
  created_at: '2026-11-01T12:00:00Z',
  updated_at: '2026-11-01T12:00:00Z',
  version: 1,
  contacts: [contact],
  billing_address: null,
};

function renderCustomer(tab: string, session = makeSession()) {
  server.use(http.get(`/api/v1/customers/${CUSTOMER_ID}`, () => HttpResponse.json(customer)));
  return renderPage(<CustomerDetailPage />, { path: '/clientes/:id', route: `/clientes/${CUSTOMER_ID}?aba=${tab}`, session });
}

describe('contatos do cliente', () => {
  it('edita o contato com os dados atuais preenchidos e envia a alteração', async () => {
    const requests: Request[] = [];
    server.use(
      http.patch(`/api/v1/customers/${CUSTOMER_ID}/contacts/:contactId`, ({ request }) => {
        requests.push(request.clone());
        return HttpResponse.json({ ...contact, email: 'ana.lima@cliente.test', receives_billing_email: false });
      }),
    );
    const { user } = renderCustomer('contatos');
    await user.click(await screen.findByRole('button', { name: 'Editar contato Ana Lima' }));
    const dialog = await screen.findByRole('dialog', { name: 'Editar contato' });
    expect(within(dialog).getByLabelText(/Nome/)).toHaveValue('Ana Lima');
    expect(within(dialog).getByLabelText('Função')).toHaveValue('Financeiro');
    const email = within(dialog).getByLabelText(/E-mail/, { selector: 'input[type="email"]' });
    expect(email).toHaveValue('ana@cliente.test');
    await user.clear(email);
    await user.type(email, 'ana.lima@cliente.test');
    await user.click(within(dialog).getByLabelText('Recebe e-mails de cobrança (faturas e lembretes)'));
    await user.click(within(dialog).getByRole('button', { name: 'Salvar contato' }));

    expect(await screen.findByText('Contato alterado.')).toBeInTheDocument();
    expect(requests).toHaveLength(1);
    expect(new URL(requests[0].url).pathname).toBe(`/api/v1/customers/${CUSTOMER_ID}/contacts/${contact.id}`);
    expect(requests[0].headers.get('X-CSRF-Token')).toBeTruthy();
    expect(await requests[0].json()).toEqual({
      name: 'Ana Lima',
      email: 'ana.lima@cliente.test',
      role: 'Financeiro',
      receives_billing_email: false,
    });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Editar contato' })).not.toBeInTheDocument());
  });

  it('erro de validação da API fica no campo e o modal continua aberto', async () => {
    server.use(
      http.patch(`/api/v1/customers/${CUSTOMER_ID}/contacts/:contactId`, () =>
        HttpResponse.json(
          { status: 422, code: 'invalid_contact', title: 'Dados do contato inválidos.', errors: [{ field: 'email', message: 'Endereço de e-mail inválido.' }] },
          { status: 422, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    );
    const { user } = renderCustomer('contatos');
    await user.click(await screen.findByRole('button', { name: 'Editar contato Ana Lima' }));
    const dialog = await screen.findByRole('dialog', { name: 'Editar contato' });
    await user.click(within(dialog).getByRole('button', { name: 'Salvar contato' }));
    expect(await within(dialog).findByText('Endereço de e-mail inválido.')).toBeInTheDocument();
  });

  it('auditor vê os contatos sem as ações de edição e remoção', async () => {
    renderCustomer('contatos', makeSession({ permissions: AUDITOR_PERMISSIONS }));
    expect(await screen.findByRole('table', { name: 'Contatos' })).toHaveTextContent('Ana Lima');
    expect(screen.queryByRole('button', { name: 'Editar contato Ana Lima' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Remover contato Ana Lima' })).not.toBeInTheDocument();
  });
});

const event = (id: string, action: string): AuditEvent => ({
  id,
  occurred_at: '2026-11-01T12:00:00Z',
  actor_type: 'user',
  actor_id: '0195f1a0-0000-7000-8000-000000000001',
  actor_label: 'Ana Owner',
  action,
  resource_type: 'customer',
  resource_id: CUSTOMER_ID,
  details: {},
});

describe('histórico do cliente', () => {
  it('pagina por cursor e volta para a página anterior', async () => {
    const cursors: (string | null)[] = [];
    server.use(
      http.get(`/api/v1/customers/${CUSTOMER_ID}/history`, ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('cursor');
        cursors.push(cursor);
        return cursor === 'pagina-2'
          ? HttpResponse.json({ data: [event('0195f1a0-0000-7000-8000-0000000000a1', 'customer.created')], next_cursor: null })
          : HttpResponse.json({ data: [event('0195f1a0-0000-7000-8000-0000000000a2', 'customer.address_updated')], next_cursor: 'pagina-2' });
      }),
    );
    const { user } = renderCustomer('historico');
    expect(await screen.findByText('customer.address_updated')).toBeInTheDocument();
    expect(screen.getByText('Página 1')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /Próxima/ }));
    expect(await screen.findByText('customer.created')).toBeInTheDocument();
    expect(screen.queryByText('customer.address_updated')).not.toBeInTheDocument();
    expect(screen.getByText('Página 2')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Próxima/ })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: /Anterior/ }));
    expect(await screen.findByText('customer.address_updated')).toBeInTheDocument();
    expect(cursors.slice(0, 2)).toEqual([null, 'pagina-2']);
  });
});
