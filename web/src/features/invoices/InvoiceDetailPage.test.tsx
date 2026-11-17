import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { AUDITOR_PERMISSIONS, makeInvoice, makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { InvoiceDetailPage } from './InvoiceDetailPage';

const invoice = makeInvoice();
const route = `/faturas/${invoice.id}`;

function serveInvoice(body: unknown = invoice, status = 200) {
  server.use(http.get('/api/v1/invoices/:id', () => HttpResponse.json(body as object, { status })));
}

describe('detalhe da fatura', () => {
  it('renderiza os totais, itens com preço registrado e a cobrança com checkout', async () => {
    serveInvoice();
    renderPage(<InvoiceDetailPage />, { path: '/faturas/:id', route });

    const totals = await screen.findByRole('table', { name: 'Totais' });
    const row = (label: string) => within(totals).getByRole('row', { name: new RegExp(`^${label}`) });
    expect(row('Subtotal')).toHaveTextContent('R$ 1.234,56');
    expect(row('Desconto')).toHaveTextContent('R$ 34,56');
    expect(row('Total')).toHaveTextContent('R$ 1.200,00');
    expect(row('Crédito aplicado')).toHaveTextContent('R$ 50,00');
    expect(row('Pago')).toHaveTextContent('R$ 500,00');
    expect(row('Creditado')).toHaveTextContent('R$ 100,00');
    expect(row('Saldo devedor')).toHaveTextContent('R$ 550,00');

    expect(screen.getByRole('heading', { name: /Fatura ACME-000123/ })).toBeInTheDocument();
    expect(screen.getByText('Vencida')).toBeInTheDocument();
    expect(screen.getByText('10/11/2026')).toBeInTheDocument();
    const items = screen.getByRole('table', { name: 'Itens da fatura' });
    expect(within(items).getByText('Plano Profissional (novembro)')).toBeInTheDocument();
    expect(within(items).getByText('R$ 411,52')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Abrir checkout/ })).toHaveAttribute('href', 'https://pagamentos.exemplo.test/checkout/abc');
  });

  it('mostra as ações financeiras permitidas para a situação', async () => {
    serveInvoice();
    const { user } = renderPage(<InvoiceDetailPage />, { path: '/faturas/:id', route });
    await user.click(await screen.findByRole('button', { name: 'Ações' }));
    expect(await screen.findByRole('menuitem', { name: 'Registrar pagamento manual' })).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'Nova tentativa de cobrança' })).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'Emitir nota de crédito' })).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'Baixar como incobrável' })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: 'Cancelar fatura' })).not.toBeInTheDocument();
  });

  it('auditor vê a fatura e o documento, mas nenhuma ação de escrita', async () => {
    serveInvoice({ ...invoice, charges: [{ ...invoice.charges[0], status: 'paid', amount_paid: 15990, refundable_amount: 15990 }] });
    renderPage(<InvoiceDetailPage />, { path: '/faturas/:id', route, session: makeSession({ permissions: AUDITOR_PERMISSIONS }) });
    expect(await screen.findByRole('table', { name: 'Totais' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Abrir documento' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Ações' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Reembolsar' })).not.toBeInTheDocument();
  });

  it('fatura de outra organização mostra "não encontrado"', async () => {
    serveInvoice({ type: 'about:blank', title: 'Não encontrado', status: 404, code: 'not_found' }, 404);
    renderPage(<InvoiceDetailPage />, { path: '/faturas/:id', route });
    expect(await screen.findByText('Registro não encontrado')).toBeInTheDocument();
  });

  it('pagamento manual reutiliza a chave de idempotência ao repetir após falha de rede', async () => {
    serveInvoice();
    const keys: (string | null)[] = [];
    let calls = 0;
    server.use(
      http.post('/api/v1/invoices/:id/payments', ({ request }) => {
        keys.push(request.headers.get('Idempotency-Key'));
        calls += 1;
        if (calls === 1) return HttpResponse.error();
        return HttpResponse.json({ id: 'p1' }, { status: 201 });
      }),
    );
    const { user } = renderPage(<InvoiceDetailPage />, { path: '/faturas/:id', route });
    await user.click(await screen.findByRole('button', { name: 'Ações' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Registrar pagamento manual' }));
    expect(screen.getByLabelText(/Valor recebido/)).toHaveValue('550,00');

    for (let attempt = 0; attempt < 2; attempt++) {
      await user.click(screen.getByRole('button', { name: 'Continuar' }));
      await user.click(await screen.findByRole('button', { name: 'Registrar pagamento' }));
      await waitFor(() => expect(keys).toHaveLength(attempt + 1));
    }
    expect(keys[0]).toBeTruthy();
    expect(keys[1]).toBe(keys[0]);
    expect(await screen.findByText('Pagamento registrado.')).toBeInTheDocument();
  });
});
