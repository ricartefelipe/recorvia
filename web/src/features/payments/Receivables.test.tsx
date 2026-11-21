import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { Dispute, RecoverySettlement, Refund } from '../../api/types';
import { todayInTimeZone } from '../../lib/dates';
import { PERMISSIONS } from '../../session/permissions';
import { makeCharge, makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { canRequestRefund } from './api';
import { ChargesTable } from './ChargesTable';
import { DisputesPage, RefundsPage } from './PaymentListsPages';
import { RefundModal } from './RefundModal';

const CHARGE_ID = '0195f1a0-0000-7000-8000-00000000c001';

// Respostas no formato devolvido pela API (schemas Refund e Dispute).
const refund = (overrides: Partial<Refund> = {}): Refund => ({
  id: '0195f1a0-0000-7000-8000-0000000ae001',
  charge_id: CHARGE_ID,
  amount: 5000,
  currency: 'BRL',
  status: 'succeeded',
  provider_refund_id: 'rf_1',
  reason: 'Cancelamento parcial do contrato',
  created_at: '2026-11-17T12:00:00Z',
  ...overrides,
});

describe('reembolsos', () => {
  it('reembolso parcial envia valor em centavos, motivo e Idempotency-Key após confirmação', async () => {
    const requests: { key: string | null; body: unknown }[] = [];
    server.use(
      http.post('/api/v1/charges/:id/refunds', async ({ request, params }) => {
        expect(params.id).toBe(CHARGE_ID);
        requests.push({ key: request.headers.get('Idempotency-Key'), body: await request.json() });
        return HttpResponse.json(refund(), { status: 201 });
      }),
    );
    const charge = makeCharge({ status: 'paid', amount_paid: 15990, refundable_amount: 15990 });
    const { user } = renderPage(<RefundModal charge={charge} opened onClose={() => {}} />);
    expect(screen.getByLabelText(/Valor do reembolso/)).toHaveValue('159,90');

    await user.clear(screen.getByLabelText(/Valor do reembolso/));
    await user.type(screen.getByLabelText(/Valor do reembolso/), '160,00');
    await user.type(screen.getByLabelText(/Motivo/), 'Cancelamento parcial do contrato');
    await user.click(screen.getByRole('button', { name: 'Continuar' }));
    expect(await screen.findByText(/O valor excede o reembolsável/)).toBeInTheDocument();

    await user.clear(screen.getByLabelText(/Valor do reembolso/));
    await user.type(screen.getByLabelText(/Valor do reembolso/), '50,00');
    await user.click(screen.getByRole('button', { name: 'Continuar' }));
    const confirmText = await screen.findByText(/Reembolsar R\$\s50,00 ao pagador/);
    const dialog = confirmText.closest('[role="dialog"]') as HTMLElement;
    await user.click(within(dialog).getByRole('button', { name: 'Reembolsar' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0].body).toEqual({ amount: 5000, reason: 'Cancelamento parcial do contrato' });
    expect(requests[0].key).toBeTruthy();
    expect(await screen.findByText('Reembolso concluído.')).toBeInTheDocument();
  });

  it('recusa do provedor é informada como erro', async () => {
    server.use(http.post('/api/v1/charges/:id/refunds', () => HttpResponse.json(refund({ status: 'failed', provider_refund_id: null }), { status: 201 })));
    const charge = makeCharge({ status: 'paid', amount_paid: 15990, refundable_amount: 15990 });
    const { user } = renderPage(<RefundModal charge={charge} opened onClose={() => {}} />);
    await user.type(screen.getByLabelText(/Motivo/), 'Cancelamento do contrato');
    await user.click(screen.getByRole('button', { name: 'Continuar' }));
    const dialog = (await screen.findByText(/ao pagador/)).closest('[role="dialog"]') as HTMLElement;
    await user.click(within(dialog).getByRole('button', { name: 'Reembolsar' }));
    await waitFor(() =>
      expect(screen.getByTestId('live-assertive')).toHaveTextContent('O provedor recusou o reembolso; o valor reservado voltou a ficar disponível.'),
    );
  });

  it.each([
    ['recusa registrada na chave (saldo insuficiente) usa outra chave no próximo envio', 409, 'refund_exceeds_balance', true],
    ['erro no servidor mantém a chave para a repetição', 503, 'provider_unavailable', false],
  ])('%s', async (_name, status, code, newKey) => {
    // Como o middleware de idempotência: a recusa 4xx fica registrada na
    // chave e é devolvida de novo com a mesma chave; a resposta 5xx não.
    const stored = new Map<string, { status: number; body: Record<string, unknown> }>();
    const keys: string[] = [];
    let failing = true;
    const detail = `Recusa ${code} na primeira tentativa.`;
    server.use(
      http.post('/api/v1/charges/:id/refunds', ({ request }) => {
        const key = request.headers.get('Idempotency-Key') ?? '';
        keys.push(key);
        const previous = stored.get(key);
        if (previous) return HttpResponse.json(previous.body, { status: previous.status });
        const result = failing ? { status, body: { status, code, title: 'Erro', detail } } : { status: 201, body: { ...refund() } };
        failing = false;
        if (result.status < 500) stored.set(key, result);
        return HttpResponse.json(result.body, { status: result.status });
      }),
    );
    const charge = makeCharge({ status: 'paid', amount_paid: 15990, refundable_amount: 15990 });
    const { user } = renderPage(<RefundModal charge={charge} opened onClose={() => {}} />);
    await user.type(screen.getByLabelText(/Motivo/), 'Cancelamento do contrato');
    const submit = async () => {
      await user.click(screen.getByRole('button', { name: 'Continuar' }));
      const dialog = (await screen.findByText(/ao pagador/)).closest('[role="dialog"]') as HTMLElement;
      await user.click(within(dialog).getByRole('button', { name: 'Reembolsar' }));
    };
    await submit();
    await waitFor(() => expect(screen.getByTestId('live-assertive')).toHaveTextContent(detail));
    await submit();
    await waitFor(() => expect(keys).toHaveLength(2));
    expect(keys[1] !== keys[0]).toBe(newKey);
    expect(await screen.findByText('Reembolso concluído.', {}, { timeout: 5_000 })).toBeInTheDocument();
    // Os dois envios com confirmação são lentos sob carga.
  }, 30_000);

  it('cobrança sem saldo reembolsável não permite o pedido', () => {
    const charge = makeCharge({ status: 'refunded', amount_paid: 15990, amount_refunded: 15990, refundable_amount: 0 });
    renderPage(<RefundModal charge={charge} opened onClose={() => {}} />);
    expect(screen.getByText('Não há valor reembolsável nesta cobrança.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continuar' })).toBeDisabled();
  });

  it('cobrança em disputa mantém o reembolsável, mas não aceita o pedido', () => {
    const disputed = makeCharge({ status: 'disputed', amount_paid: 15990, refundable_amount: 15990 });
    expect(canRequestRefund(disputed)).toBe(false);
    expect(canRequestRefund({ ...disputed, status: 'partially_refunded' })).toBe(true);
    expect(canRequestRefund({ ...disputed, status: 'paid', refundable_amount: 0 })).toBe(false);
    renderPage(<RefundModal charge={disputed} opened onClose={() => {}} />);
    expect(screen.getByText(/não aceita reembolso nesta situação/)).toBeInTheDocument();
    expect(screen.queryByLabelText(/Valor do reembolso/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continuar' })).toBeDisabled();
  });

  it('tabela de cobranças oferece o reembolso somente às cobranças pagas com saldo', () => {
    const charges = [
      makeCharge({ id: '0195f1a0-0000-7000-8000-00000000c101', attempt: 1, status: 'paid', amount_paid: 15990, refundable_amount: 15990 }),
      makeCharge({ id: '0195f1a0-0000-7000-8000-00000000c102', attempt: 2, status: 'disputed', amount_paid: 15990, refundable_amount: 15990 }),
      makeCharge({ id: '0195f1a0-0000-7000-8000-00000000c103', attempt: 3, status: 'refunded', amount_paid: 15990, amount_refunded: 15990 }),
    ];
    renderPage(<ChargesTable charges={charges} />);
    const rows = within(screen.getByRole('table', { name: 'Cobranças' })).getAllByRole('row');
    expect(within(rows[1]).getByRole('button', { name: 'Reembolsar' })).toBeInTheDocument();
    expect(within(rows[2]).queryByRole('button', { name: 'Reembolsar' })).not.toBeInTheDocument();
    expect(within(rows[3]).queryByRole('button', { name: 'Reembolsar' })).not.toBeInTheDocument();
  });

  it('lista reembolsos da API, destacando o resultado desconhecido e o identificador ausente', async () => {
    server.use(
      http.get('/api/v1/refunds', () =>
        HttpResponse.json({
          data: [refund(), refund({ id: '0195f1a0-0000-7000-8000-0000000ae002', status: 'unknown', provider_refund_id: null, amount: 1000 })],
          next_cursor: null,
        }),
      ),
    );
    renderPage(<RefundsPage />, { path: '/reembolsos' });
    const table = await screen.findByRole('table', { name: 'Reembolsos' });
    const rows = within(table).getAllByRole('row');
    expect(rows).toHaveLength(3);
    expect(rows[1]).toHaveTextContent('R$ 50,00');
    expect(rows[1]).toHaveTextContent('Concluído');
    expect(rows[1]).toHaveTextContent('rf_1');
    expect(rows[2]).toHaveTextContent('Resultado desconhecido');
    expect(rows[2]).toHaveTextContent('—');
  });
});

const dispute = (overrides: Partial<Dispute> = {}): Dispute => ({
  id: '0195f1a0-0000-7000-8000-0000000d1001',
  charge_id: CHARGE_ID,
  status: 'lost',
  amount: 12500,
  credit_amount: 0,
  recovery_amount: 0,
  recovery_received_amount: 0,
  recovery_written_off_amount: 0,
  recovery_outstanding_amount: 0,
  currency: 'BRL',
  opened_at: '2026-11-10T12:00:00Z',
  closed_at: '2026-11-15T12:00:00Z',
  ...overrides,
});

// Disputa perdida com 20,00 a recuperar do cliente, ainda sem baixas.
const withRecovery = (overrides: Partial<Dispute> = {}) =>
  dispute({ id: '0195f1a0-0000-7000-8000-0000000d1003', credit_amount: 500, recovery_amount: 2000, recovery_outstanding_amount: 2000, ...overrides });

const settlement = (overrides: Partial<RecoverySettlement> = {}): RecoverySettlement => ({
  id: '0195f1a0-0000-7000-8000-0000000b0001',
  dispute_id: '0195f1a0-0000-7000-8000-0000000d1003',
  outcome: 'received',
  amount: 500,
  currency: 'BRL',
  effective_date: '2026-11-16',
  justification: 'Transferência identificada no extrato.',
  settled_by: null,
  ledger_transaction_id: '0195f1a0-0000-7000-8000-0000000e0001',
  created_at: '2026-11-16T15:00:00Z',
  ...overrides,
});

describe('disputas', () => {
  it('lista disputas abertas e encerradas como devolvidas pela API', async () => {
    const disputes: Dispute[] = [
      dispute({ status: 'opened', amount: 5000, opened_at: '2026-11-17T12:00:00Z', closed_at: null }),
      dispute({ id: '0195f1a0-0000-7000-8000-0000000d1002', amount: 6000 }),
    ];
    server.use(http.get('/api/v1/disputes', () => HttpResponse.json({ data: disputes })));
    renderPage(<DisputesPage />, { path: '/disputas' });
    const table = await screen.findByRole('table', { name: 'Disputas' });
    const rows = within(table).getAllByRole('row');
    expect(rows[1]).toHaveTextContent('Aberta');
    expect(rows[1]).toHaveTextContent('R$ 50,00');
    expect(rows[2]).toHaveTextContent('Perdida');
    expect(rows[2]).toHaveTextContent('R$ 60,00');
    expect(within(rows[2]).getByRole('link', { name: /Cobrança 0195f1a0/ })).toHaveAttribute('href', `/cobrancas/${CHARGE_ID}`);
    // Sem valor a recuperar não há ação de baixa.
    expect(within(table).queryByRole('button', { name: /Dar baixa|Ver baixas/ })).not.toBeInTheDocument();
  });

  it('disputa perdida de pagamento com excedente mostra o crédito consumido, o valor a recuperar e o saldo', async () => {
    const disputes: Dispute[] = [
      withRecovery({ recovery_received_amount: 1500, recovery_outstanding_amount: 500 }),
      dispute({ id: '0195f1a0-0000-7000-8000-0000000d1004', status: 'won', opened_at: '2026-11-09T12:00:00Z', closed_at: '2026-11-12T12:00:00Z' }),
    ];
    server.use(http.get('/api/v1/disputes', () => HttpResponse.json({ data: disputes })));
    renderPage(<DisputesPage />, { path: '/disputas' });
    const table = await screen.findByRole('table', { name: 'Disputas' });
    const headers = within(table).getAllByRole('columnheader').map((h) => h.textContent);
    expect(headers).toEqual([
      'Aberta em',
      'Cobrança',
      'Valor contestado',
      'Situação',
      'Crédito consumido',
      'A recuperar do cliente',
      'Saldo a recuperar',
      'Encerrada em',
      '',
    ]);
    const rows = within(table).getAllByRole('row');
    const lost = within(rows[1]).getAllByRole('cell');
    expect(lost[2]).toHaveTextContent('R$ 125,00');
    expect(lost[4]).toHaveTextContent('R$ 5,00');
    expect(lost[5]).toHaveTextContent('R$ 20,00');
    expect(lost[6]).toHaveTextContent('R$ 5,00');
    expect(within(rows[1]).getByRole('button', { name: /Dar baixa/ })).toBeInTheDocument();
    const won = within(rows[2]).getAllByRole('cell');
    expect(won[4]).toHaveTextContent('—');
    expect(won[5]).toHaveTextContent('—');
    expect(won[6]).toHaveTextContent('—');
  });

  it('baixa parcial como perdido envia desfecho, valor em centavos, data, justificativa e Idempotency-Key após confirmação', async () => {
    const requests: { key: string | null; body: unknown }[] = [];
    server.use(
      http.get('/api/v1/disputes', () => HttpResponse.json({ data: [withRecovery()] })),
      http.get('/api/v1/disputes/:id/recovery-settlements', () => HttpResponse.json({ data: [] })),
      http.post('/api/v1/disputes/:id/recovery-settlements', async ({ request, params }) => {
        expect(params.id).toBe('0195f1a0-0000-7000-8000-0000000d1003');
        requests.push({ key: request.headers.get('Idempotency-Key'), body: await request.json() });
        return HttpResponse.json(settlement({ outcome: 'written_off', amount: 1500 }), { status: 201 });
      }),
    );
    const { user } = renderPage(<DisputesPage />, { path: '/disputas' });
    await user.click(await screen.findByRole('button', { name: /Dar baixa/ }));
    const modal = await screen.findByRole('dialog', { name: 'Valor a recuperar do cliente' });
    expect(await within(modal).findByText('Nenhuma baixa')).toBeInTheDocument();
    const amount = within(modal).getByLabelText(/Valor da baixa/);
    expect(amount).toHaveValue('20,00');

    await user.clear(amount);
    await user.type(amount, '20,01');
    await user.click(within(modal).getByRole('button', { name: 'Continuar' }));
    expect(await within(modal).findByText(/O valor excede o saldo a recuperar/)).toBeInTheDocument();
    expect(await within(modal).findByText(/A justificativa deve ter pelo menos 10 caracteres/)).toBeInTheDocument();

    await user.clear(amount);
    await user.type(amount, '15,00');
    await user.click(within(modal).getByRole('radio', { name: 'Perdido' }));
    await user.type(within(modal).getByLabelText(/Justificativa/), 'Acordo comercial: restante perdoado.');
    await user.click(within(modal).getByRole('button', { name: 'Continuar' }));
    const confirmText = await screen.findByText(/Baixar R\$\s15,00 como perdido/);
    const dialog = confirmText.closest('[role="dialog"]') as HTMLElement;
    await user.click(within(dialog).getByRole('button', { name: 'Baixar como perdido' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0].body).toEqual({
      outcome: 'written_off',
      amount: 1500,
      effective_date: todayInTimeZone('America/Sao_Paulo'),
      justification: 'Acordo comercial: restante perdoado.',
    });
    expect(requests[0].key).toBeTruthy();
    expect(await screen.findByText('Valor a recuperar baixado como perdido.')).toBeInTheDocument();
  }, 30_000);

  it('sem a permissão de baixa, mostra o saldo e as baixas sem o formulário', async () => {
    server.use(
      http.get('/api/v1/disputes', () => HttpResponse.json({ data: [withRecovery({ recovery_received_amount: 500, recovery_outstanding_amount: 1500 })] })),
      http.get('/api/v1/disputes/:id/recovery-settlements', () => HttpResponse.json({ data: [settlement()] })),
    );
    const { user } = renderPage(<DisputesPage />, {
      path: '/disputas',
      session: makeSession({ permissions: PERMISSIONS.filter((p) => p !== 'payments.settle_recovery') }),
    });
    await user.click(await screen.findByRole('button', { name: /Ver baixas/ }));
    const modal = await screen.findByRole('dialog', { name: 'Valor a recuperar do cliente' });
    const table = await within(modal).findByRole('table', { name: 'Baixas do valor a recuperar' });
    const row = within(table).getAllByRole('row')[1];
    expect(row).toHaveTextContent('16/11/2026');
    expect(row).toHaveTextContent('Recebido');
    expect(row).toHaveTextContent('R$ 5,00');
    expect(row).toHaveTextContent('Transferência identificada no extrato.');
    expect(within(modal).getByText(/não tem permissão para dar baixa/)).toBeInTheDocument();
    expect(within(modal).queryByLabelText(/Valor da baixa/)).not.toBeInTheDocument();
  });

  it('saldo já baixado por completo não oferece nova baixa', async () => {
    server.use(
      http.get('/api/v1/disputes', () =>
        HttpResponse.json({ data: [withRecovery({ recovery_received_amount: 1500, recovery_written_off_amount: 500, recovery_outstanding_amount: 0 })] }),
      ),
      http.get('/api/v1/disputes/:id/recovery-settlements', () =>
        HttpResponse.json({ data: [settlement({ amount: 1500 }), settlement({ id: '0195f1a0-0000-7000-8000-0000000b0002', outcome: 'written_off', amount: 500 })] }),
      ),
    );
    const { user } = renderPage(<DisputesPage />, { path: '/disputas' });
    await user.click(await screen.findByRole('button', { name: /Ver baixas/ }));
    const modal = await screen.findByRole('dialog', { name: 'Valor a recuperar do cliente' });
    expect(await within(modal).findByText(/já foi baixado por completo/)).toBeInTheDocument();
    expect(within(modal).getAllByText('Perdido').length).toBeGreaterThan(0);
    expect(within(modal).queryByLabelText(/Valor da baixa/)).not.toBeInTheDocument();
  });
});
