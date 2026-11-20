import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { Notification, NotificationTemplate } from '../../api/types';
import { AUDITOR_PERMISSIONS, makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { CollectionsPage, placeholder } from './CollectionsPage';

// Respostas no formato devolvido pela API (variáveis pelo nome, sem a marcação).
const template: NotificationTemplate = {
  key: 'reminder_after_due',
  channel: 'email',
  subject: 'Fatura {{invoice_number}} vencida em {{due_date}}',
  body: 'Olá, {{customer_name}}.\n\nA fatura {{invoice_number}} está vencida.',
  is_default: true,
  variables: ['organization_name', 'customer_name', 'invoice_number', 'invoice_total', 'amount_due', 'due_date', 'days_overdue'],
  updated_at: null,
};

const notification: Notification = {
  id: '0195f1a0-0000-7000-8000-0000000a0001',
  business_key: 'reminder:0195f1a0-0000-7000-8000-00000000f001:3',
  customer_id: '0195f1a0-0000-7000-8000-00000000d001',
  invoice_id: '0195f1a0-0000-7000-8000-00000000f001',
  template_key: 'reminder_after_due',
  channel: 'email',
  recipient: 'financeiro@cliente.test, contas@cliente.test',
  subject: 'Fatura ACME-000123 vencida em 10/11/2026',
  status: 'canceled',
  send_after: '2026-11-13T11:00:00Z',
  attempts: 0,
  last_error: '',
  sent_at: null,
  created_at: '2026-11-13T09:00:00Z',
};

describe('templates de e-mail', () => {
  it('mostra as variáveis com a marcação e insere {{nome}} no corpo', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.get('/api/v1/notification-templates', () => HttpResponse.json({ data: [template] })),
      http.put('/api/v1/notification-templates/:key', async ({ request, params }) => {
        const body = (await request.json()) as { subject: string; body: string };
        bodies.push({ key: params.key, ...body });
        return HttpResponse.json({ ...template, ...body, is_default: false, updated_at: '2026-11-17T12:00:00Z' });
      }),
    );
    const { user } = renderPage(<CollectionsPage />, { route: '/?aba=templates' });
    expect(await screen.findByText('Lembrete após o vencimento')).toBeInTheDocument();
    expect(screen.getByText(placeholder('days_overdue'))).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Editar' }));
    const dialog = await screen.findByRole('dialog');
    const body = within(dialog).getByLabelText(/Corpo do e-mail/);
    await user.clear(body);
    await user.type(body, 'Prezado cliente, atraso de ');
    await user.click(within(dialog).getByRole('button', { name: 'Inserir variável days_overdue' }));
    expect(body).toHaveValue('Prezado cliente, atraso de {{days_overdue}}');
    await user.click(within(dialog).getByRole('button', { name: 'Salvar template' }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ key: 'reminder_after_due', subject: template.subject, body: 'Prezado cliente, atraso de {{days_overdue}}' });
  });

  it('auditor consulta sem editar', async () => {
    server.use(http.get('/api/v1/notification-templates', () => HttpResponse.json({ data: [template] })));
    renderPage(<CollectionsPage />, { route: '/?aba=templates', session: makeSession({ permissions: AUDITOR_PERMISSIONS }) });
    expect(await screen.findByText('Lembrete após o vencimento')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Editar' })).not.toBeInTheDocument();
  });
});

describe('histórico de envios', () => {
  it('lista destinatários, situação e filtra pela situação', async () => {
    const queries: string[] = [];
    server.use(
      http.get('/api/v1/notifications', ({ request }) => {
        queries.push(new URL(request.url).search);
        return HttpResponse.json({ data: [notification], next_cursor: null });
      }),
    );
    renderPage(<CollectionsPage />, { route: '/?aba=historico&situacao=canceled' });
    const table = await screen.findByRole('table', { name: 'Histórico de notificações' });
    expect(within(table).getByText('financeiro@cliente.test, contas@cliente.test')).toBeInTheDocument();
    expect(within(table).getByText('Cancelada')).toBeInTheDocument();
    expect(within(table).getByText('Lembrete após o vencimento')).toBeInTheDocument();
    expect(within(table).getByRole('link', { name: 'Ver fatura' })).toHaveAttribute('href', `/faturas/${notification.invoice_id}`);
    expect(queries[0]).toContain('status=canceled');
  });
});
