import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { CollectionPolicy, Notification, NotificationEvent, NotificationTemplate } from '../../api/types';
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
  version: 0,
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
  status_reason: 'superseded',
  send_after: '2026-11-13T11:00:00Z',
  attempts: 0,
  last_error: '',
  sent_at: null,
  created_at: '2026-11-13T09:00:00Z',
};

describe('templates de e-mail', () => {
  it('mostra as variáveis com a marcação e insere {{nome}} no corpo', async () => {
    const bodies: unknown[] = [];
    const versions: (string | null)[] = [];
    server.use(
      http.get('/api/v1/notification-templates', () => HttpResponse.json({ data: [template] })),
      http.put('/api/v1/notification-templates/:key', async ({ request, params }) => {
        const body = (await request.json()) as { subject: string; body: string };
        bodies.push({ key: params.key, ...body });
        versions.push(request.headers.get('If-Match'));
        return HttpResponse.json({ ...template, ...body, is_default: false, updated_at: '2026-11-17T12:00:00Z', version: 1 }, { headers: { ETag: '"1"' } });
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
    // Template padrão, ainda não personalizado: versão 0.
    expect(versions).toEqual(['"0"']);
  });

  it('template personalizado envia If-Match com a versão da personalização', async () => {
    const versions: (string | null)[] = [];
    const custom = { ...template, is_default: false, updated_at: '2026-11-17T12:00:00Z', version: 3 };
    server.use(
      http.get('/api/v1/notification-templates', () => HttpResponse.json({ data: [custom] })),
      http.put('/api/v1/notification-templates/:key', async ({ request }) => {
        versions.push(request.headers.get('If-Match'));
        const body = (await request.json()) as { subject: string; body: string };
        return HttpResponse.json({ ...custom, ...body, version: 4 }, { headers: { ETag: '"4"' } });
      }),
    );
    const { user } = renderPage(<CollectionsPage />, { route: '/?aba=templates' });
    await user.click(await screen.findByRole('button', { name: 'Editar' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Salvar template' }));
    await waitFor(() => expect(versions).toEqual(['"3"']));
  });

  it('depois do 412, recarregar fecha o modal e a nova edição usa a versão atual', async () => {
    const versions: (string | null)[] = [];
    let current: NotificationTemplate = { ...template, is_default: true, version: 0 };
    server.use(
      http.get('/api/v1/notification-templates', () => HttpResponse.json({ data: [current] })),
      http.put('/api/v1/notification-templates/:key', async ({ request }) => {
        const ifMatch = request.headers.get('If-Match');
        versions.push(ifMatch);
        if (versions.length === 1) {
          // Outra pessoa personalizou o template enquanto o modal estava aberto.
          current = { ...template, is_default: false, version: 1 };
          return HttpResponse.json(
            { type: 'about:blank', title: 'Versão divergente', status: 412, code: 'version_mismatch' },
            { status: 412, headers: { 'Content-Type': 'application/problem+json' } },
          );
        }
        const body = (await request.json()) as { subject: string; body: string };
        return HttpResponse.json({ ...current, ...body, version: 2 }, { headers: { ETag: '"2"' } });
      }),
    );
    const { user } = renderPage(<CollectionsPage />, { route: '/?aba=templates' });
    await user.click(await screen.findByRole('button', { name: 'Editar' }));
    let dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Salvar template' }));
    await user.click(await screen.findByRole('button', { name: 'Recarregar dados' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await user.click(await screen.findByRole('button', { name: 'Editar' }));
    dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Salvar template' }));
    await waitFor(() => expect(versions).toEqual(['"0"', '"1"']));
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

  it('mostra o motivo da situação em português', async () => {
    const reasons: [Notification['status'], string, string][] = [
      ['canceled', 'invoice_paid', 'Fatura paga'],
      ['scheduled', 'frequency_limit', 'Limite diário de mensagens atingido'],
      ['scheduled', 'outside_send_window', 'Fora da janela de envio'],
      ['suppressed', 'no_recipient', 'Sem destinatário'],
    ];
    server.use(
      http.get('/api/v1/notifications', () =>
        HttpResponse.json({
          data: [
            notification,
            ...reasons.map(([status, status_reason], i) => ({ ...notification, id: `0195f1a0-0000-7000-8000-0000000a010${i}`, status, status_reason })),
            { ...notification, id: '0195f1a0-0000-7000-8000-0000000a0199', status: 'sent', status_reason: '', sent_at: '2026-11-13T11:00:05Z' },
          ],
          next_cursor: null,
        }),
      ),
    );
    renderPage(<CollectionsPage />, { route: '/?aba=historico' });
    const table = await screen.findByRole('table', { name: 'Histórico de notificações' });
    expect(within(table).getByText('Substituída por um passo posterior da régua')).toBeInTheDocument();
    for (const [, , label] of reasons) expect(within(table).getByText(label)).toBeInTheDocument();
    expect(within(table).queryByText('superseded')).not.toBeInTheDocument();
  });

  it('abre a linha do tempo da notificação com os eventos gravados', async () => {
    const events: NotificationEvent[] = [
      { status: 'scheduled', reason: 'scheduled', attempt: 0, detail: '', occurred_at: '2026-11-13T09:00:00Z' },
      { status: 'scheduled', reason: 'outside_send_window', attempt: 0, detail: '2026-11-13T11:00:00Z', occurred_at: '2026-11-13T09:00:01Z' },
      { status: 'sending', reason: 'attempt', attempt: 1, detail: '', occurred_at: '2026-11-13T11:00:00Z' },
      { status: 'scheduled', reason: 'retry_scheduled', attempt: 1, detail: '421 serviço indisponível', occurred_at: '2026-11-13T11:00:02Z' },
      { status: 'canceled', reason: 'superseded', attempt: 1, detail: '', occurred_at: '2026-11-13T11:30:00Z' },
    ];
    const requested: string[] = [];
    server.use(
      http.get('/api/v1/notifications', () => HttpResponse.json({ data: [notification], next_cursor: null })),
      http.get('/api/v1/notifications/:id/events', ({ params }) => {
        requested.push(String(params.id));
        return HttpResponse.json({ data: events });
      }),
    );
    const { user } = renderPage(<CollectionsPage />, { route: '/?aba=historico', session: makeSession({ permissions: AUDITOR_PERMISSIONS }) });
    const table = await screen.findByRole('table', { name: 'Histórico de notificações' });
    await user.click(within(table).getByRole('button', { name: `Linha do tempo: ${notification.subject}` }));
    const timeline = await screen.findByRole('table', { name: 'Linha do tempo da notificação' });
    expect(requested).toEqual([notification.id]);
    const rows = within(timeline).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(5);
    expect(within(rows[1]).getByText('Fora da janela de envio')).toBeInTheDocument();
    expect(within(rows[1]).getByText(/Adiada para/)).toBeInTheDocument();
    expect(within(rows[2]).getByText('Tentativa de envio')).toBeInTheDocument();
    expect(within(rows[3]).getByText('Nova tentativa agendada')).toBeInTheDocument();
    expect(within(rows[3]).getByText('421 serviço indisponível')).toBeInTheDocument();
    expect(within(rows[4]).getByText('Substituída por um passo posterior da régua')).toBeInTheDocument();
  });
});

// Formulário com vários campos numéricos do Mantine: sob carga, a digitação
// passa do limite padrão de 15 s.
const FORM_TEST_TIMEOUT = 40_000;

const policy: CollectionPolicy = {
  id: '0195f1a0-0000-7000-8000-00000000b001',
  name: 'Régua padrão',
  is_default: true,
  reminder_offsets_days: [-3, 0, 3],
  send_window_start_hour: 8,
  send_window_end_hour: 20,
  max_messages_per_day: 1,
  suspend_after_days: 30,
  reactivate_on_payment: true,
  created_at: '2026-11-01T12:00:00Z',
  version: 2,
};

const lenient: CollectionPolicy = {
  ...policy,
  id: '0195f1a0-0000-7000-8000-00000000b002',
  name: 'Clientes estratégicos',
  is_default: false,
  reminder_offsets_days: [7, -1],
  send_window_start_hour: 9,
  send_window_end_hour: 18,
  max_messages_per_day: 2,
  suspend_after_days: null,
  reactivate_on_payment: false,
  version: 1,
};

function servePolicies(data: CollectionPolicy[] = [policy, lenient]) {
  server.use(http.get('/api/v1/collection-policies', () => HttpResponse.json({ data })));
}

describe('políticas de cobrança', () => {
  it('lista lembretes, janela, limite, suspensão e a política padrão', async () => {
    servePolicies();
    renderPage(<CollectionsPage />);
    const table = await screen.findByRole('table', { name: 'Políticas de cobrança' });
    const standard = within(table).getByRole('row', { name: /Régua padrão/ });
    expect(within(standard).getByText('Padrão')).toBeInTheDocument();
    expect(standard).toHaveTextContent('3 dia(s) antes, no vencimento, 3 dia(s) depois');
    expect(standard).toHaveTextContent('8h–20h');
    expect(standard).toHaveTextContent('1 e-mail(s)');
    expect(standard).toHaveTextContent('Após 30 dia(s)');
    expect(standard).toHaveTextContent('Sim');

    const other = within(table).getByRole('row', { name: /Clientes estratégicos/ });
    expect(within(other).queryByText('Padrão')).not.toBeInTheDocument();
    // Os lembretes aparecem em ordem, do primeiro ao último.
    expect(other).toHaveTextContent('1 dia(s) antes, 7 dia(s) depois');
    expect(other).toHaveTextContent('9h–18h');
    expect(other).toHaveTextContent('2 e-mail(s)');
    expect(other).toHaveTextContent('Não suspende');
    expect(other).toHaveTextContent('Não');
  });

  it(
    'cria a política com os lembretes em ordem e sem suspensão quando desligada',
    async () => {
      const bodies: unknown[] = [];
      servePolicies([]);
      server.use(
        http.post('/api/v1/collection-policies', async ({ request }) => {
          const body = (await request.json()) as Record<string, unknown>;
          bodies.push(body);
          return HttpResponse.json({ ...policy, ...body, id: '0195f1a0-0000-7000-8000-00000000b003', version: 1 }, { status: 201 });
        }),
      );
      const { user } = renderPage(<CollectionsPage />);
      expect(await screen.findByText('Nenhuma política')).toBeInTheDocument();
      await user.click(screen.getByRole('button', { name: 'Nova política' }));
      const dialog = await screen.findByRole('dialog', { name: 'Nova política de cobrança' });

      await user.type(within(dialog).getByLabelText(/^Nome/), 'Régua de varejo');
      await user.click(within(dialog).getByRole('button', { name: 'Adicionar lembrete' }));
      await user.type(within(dialog).getByLabelText('Lembrete 5 (dias)'), '-7');
      const maxPerDay = within(dialog).getByLabelText('Máximo de e-mails por dia');
      await user.clear(maxPerDay);
      await user.type(maxPerDay, '2');
      expect(within(dialog).getByRole('switch', { name: 'Suspender a assinatura após atraso' })).not.toBeChecked();
      await user.click(within(dialog).getByRole('button', { name: 'Salvar política' }));

      await waitFor(() => expect(bodies).toHaveLength(1));
      expect(bodies[0]).toEqual({
        name: 'Régua de varejo',
        is_default: false,
        reminder_offsets_days: [-7, -3, 0, 3, 7],
        send_window_start_hour: 8,
        send_window_end_hour: 20,
        max_messages_per_day: 2,
        suspend_after_days: null,
        reactivate_on_payment: true,
      });
      expect(await screen.findByText('Política criada.')).toBeInTheDocument();
    },
    FORM_TEST_TIMEOUT,
  );

  it(
    'edita a política com If-Match da versão e desliga a suspensão',
    async () => {
      const requests: { ifMatch: string | null; id: string; body: Record<string, unknown> }[] = [];
      servePolicies();
      server.use(
        http.patch('/api/v1/collection-policies/:id', async ({ request, params }) => {
          const body = (await request.json()) as Record<string, unknown>;
          requests.push({ ifMatch: request.headers.get('If-Match'), id: String(params.id), body });
          return HttpResponse.json({ ...policy, ...body, version: 3 });
        }),
      );
      const { user } = renderPage(<CollectionsPage />);
      await user.click(await screen.findByRole('button', { name: 'Editar política Régua padrão' }));
      const dialog = await screen.findByRole('dialog', { name: 'Editar política de cobrança' });
      const name = within(dialog).getByLabelText(/^Nome/);
      expect(name).toHaveValue('Régua padrão');
      expect(within(dialog).getByLabelText('Lembrete 3 (dias)')).toHaveValue('3');
      expect(within(dialog).getByLabelText('Suspender após quantos dias de atraso')).toHaveValue('30');

      await user.clear(name);
      await user.type(name, 'Régua padrão revisada');
      await user.click(within(dialog).getByRole('switch', { name: 'Suspender a assinatura após atraso' }));
      expect(within(dialog).queryByLabelText('Suspender após quantos dias de atraso')).not.toBeInTheDocument();
      await user.click(within(dialog).getByRole('button', { name: 'Salvar política' }));

      await waitFor(() => expect(requests).toHaveLength(1));
      expect(requests[0].ifMatch).toBe('"2"');
      expect(requests[0].id).toBe(policy.id);
      expect(requests[0].body).toEqual({
        name: 'Régua padrão revisada',
        is_default: true,
        reminder_offsets_days: [-3, 0, 3],
        send_window_start_hour: 8,
        send_window_end_hour: 20,
        max_messages_per_day: 1,
        suspend_after_days: null,
        reactivate_on_payment: true,
      });
      expect(await screen.findByText('Política atualizada.')).toBeInTheDocument();
    },
    FORM_TEST_TIMEOUT,
  );

  /** Abre o modal de nova política com o nome preenchido e conta os POST recebidos. */
  async function openNewPolicy() {
    const calls = { count: 0 };
    servePolicies([]);
    server.use(
      http.post('/api/v1/collection-policies', () => {
        calls.count++;
        return HttpResponse.json(policy, { status: 201 });
      }),
    );
    const { user } = renderPage(<CollectionsPage />);
    await user.click(await screen.findByRole('button', { name: 'Nova política' }));
    const dialog = await screen.findByRole('dialog', { name: 'Nova política de cobrança' });
    await user.type(within(dialog).getByLabelText(/^Nome/), 'Régua inválida');
    return { user, dialog, calls };
  }

  it(
    'não envia a janela de envio que termina antes de começar',
    async () => {
      const { user, dialog, calls } = await openNewPolicy();
      const end = within(dialog).getByLabelText('Envio até as (h)');
      await user.clear(end);
      await user.type(end, '6');
      await user.click(within(dialog).getByRole('button', { name: 'Salvar política' }));
      expect(await within(dialog).findByText('O fim da janela deve ser depois do início.')).toBeInTheDocument();
      expect(calls.count).toBe(0);
    },
    FORM_TEST_TIMEOUT,
  );

  it(
    'não envia lembretes repetidos e explica o motivo',
    async () => {
      const { user, dialog, calls } = await openNewPolicy();
      const second = within(dialog).getByLabelText('Lembrete 2 (dias)');
      await user.clear(second);
      await user.type(second, '-3');
      await user.click(within(dialog).getByRole('button', { name: 'Salvar política' }));
      // O erro da lista de lembretes fica na raiz do campo (offsets.root).
      expect(await within(dialog).findByRole('alert')).toHaveTextContent('Há lembretes repetidos.');
      expect(calls.count).toBe(0);
      // Corrigido o valor repetido, o aviso some sem novo envio.
      await user.clear(second);
      await user.type(second, '5');
      await waitFor(() => expect(within(dialog).queryByText('Há lembretes repetidos.')).not.toBeInTheDocument());
    },
    FORM_TEST_TIMEOUT,
  );

  it('auditor consulta as políticas sem criar nem editar', async () => {
    servePolicies();
    renderPage(<CollectionsPage />, { session: makeSession({ permissions: AUDITOR_PERMISSIONS }) });
    const table = await screen.findByRole('table', { name: 'Políticas de cobrança' });
    expect(within(table).getByRole('row', { name: /Régua padrão/ })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Nova política' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^Editar política/ })).not.toBeInTheDocument();
  });
});
