import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { AUDITOR_PERMISSIONS, makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { AuditPage } from '../audit/AuditPage';
import { OperationsPage } from './OperationsPage';

const letter = {
  id: '0195f1a0-0000-7000-8000-0000000dead1',
  consumer: 'billing.invoice-issued',
  message_id: 'msg-1',
  event_type: 'invoice.issued',
  error: 'timeout ao chamar o provedor',
  attempts: 8,
  status: 'open',
  failed_at: '2026-11-16T12:00:00Z',
};

describe('operações', () => {
  it('reprocessar exige justificativa e confirmação', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.get('/api/v1/dead-letters', () => HttpResponse.json({ data: [letter] })),
      http.post('/api/v1/dead-letters/:id/replay', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ ...letter, status: 'replayed' });
      }),
    );
    const { user } = renderPage(<OperationsPage />, { path: '/operacoes' });
    await user.click(await screen.findByRole('button', { name: 'Reprocessar' }));
    await user.click(screen.getByRole('button', { name: 'Continuar' }));
    expect(await screen.findByText('O motivo deve ter pelo menos 3 caracteres.')).toBeInTheDocument();
    await user.type(screen.getByLabelText(/Justificativa/), 'Provedor normalizado');
    await user.click(screen.getByRole('button', { name: 'Continuar' }));
    const confirmText = await screen.findByText(/A mensagem volta para a fila/);
    const dialog = confirmText.closest('[role="dialog"]') as HTMLElement;
    await user.click(within(dialog).getByRole('button', { name: 'Reprocessar' }));
    await waitFor(() => expect(bodies).toEqual([{ reason: 'Provedor normalizado' }]));
  });

  it('sem operations.reprocess não há ações', async () => {
    server.use(http.get('/api/v1/dead-letters', () => HttpResponse.json({ data: [letter] })));
    renderPage(<OperationsPage />, { path: '/operacoes', session: makeSession({ permissions: AUDITOR_PERMISSIONS }) });
    expect(await screen.findByText('timeout ao chamar o provedor')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Reprocessar' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Descartar' })).not.toBeInTheDocument();
  });

  it('auditoria sem audit.read mostra acesso negado', () => {
    renderPage(<AuditPage />, { path: '/auditoria', session: makeSession({ permissions: ['customers.read'] }) });
    expect(screen.getByText('Acesso negado')).toBeInTheDocument();
  });
});
