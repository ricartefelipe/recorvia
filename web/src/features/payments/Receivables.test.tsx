import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { Dispute, Refund } from '../../api/types';
import { makeCharge } from '../../test/fixtures';
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

describe('disputas', () => {
  it('lista disputas abertas e encerradas como devolvidas pela API', async () => {
    const disputes: Dispute[] = [
      { id: '0195f1a0-0000-7000-8000-0000000d1001', charge_id: CHARGE_ID, status: 'opened', amount: 5000, currency: 'BRL', opened_at: '2026-11-17T12:00:00Z', closed_at: null },
      { id: '0195f1a0-0000-7000-8000-0000000d1002', charge_id: CHARGE_ID, status: 'lost', amount: 6000, currency: 'BRL', opened_at: '2026-11-10T12:00:00Z', closed_at: '2026-11-15T12:00:00Z' },
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
  });
});
