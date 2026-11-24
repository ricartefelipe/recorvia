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
    expect(within(items).getByRole('row', { name: /Plano Profissional \(novembro\)/ })).toHaveTextContent('R$ 411,52');
    // O preço registrado no item aparece descrito, não como JSON.
    expect(within(items).getByText('Por quantidade (unidades ou usuários)')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Abrir checkout/ })).toHaveAttribute('href', 'https://pagamentos.exemplo.test/checkout/abc');
  });

  it('mostra os dados do cliente na emissão como lista descritiva, com documento formatado', async () => {
    serveInvoice(
      makeInvoice({
        customer_snapshot: {
          name: 'Cliente Exemplo Ltda',
          document_type: 'cnpj',
          document_number: '11222333000181',
          email: 'financeiro@exemplo.test',
          address: { street: 'Rua das Flores', number: '100', district: 'Centro', city: 'Curitiba', state: 'PR', postal_code: '80000000', country: 'BR' },
          captured_at: '2026-11-01T12:00:00Z',
        },
      }),
    );
    renderPage(<InvoiceDetailPage />, { path: '/faturas/:id', route });

    expect(await screen.findByText('11.222.333/0001-81')).toBeInTheDocument();
    expect(screen.getByText('CNPJ')).toBeInTheDocument();
    expect(screen.getByText('financeiro@exemplo.test')).toBeInTheDocument();
    expect(screen.getByText('Rua das Flores, 100')).toBeInTheDocument();
    expect(screen.getByText('Centro, Curitiba/PR')).toBeInTheDocument();
    // O backend grava o CEP só com dígitos; a tela mostra 00000-000.
    expect(screen.getByText('CEP 80000-000')).toBeInTheDocument();
    expect(screen.queryByText(/"document_number"/)).not.toBeInTheDocument();
  });

  it('traduz o motivo das transições do histórico', async () => {
    serveInvoice(
      makeInvoice({
        history: [
          { id: 'h1', from_status: 'draft', to_status: 'open', occurred_at: '2026-11-01T12:00:00Z', actor_type: 'system', actor_id: 'billing.engine', reason: 'issued' },
        ],
      }),
    );
    renderPage(<InvoiceDetailPage />, { path: '/faturas/:id', route });

    const history = await screen.findByRole('table', { name: 'Histórico da fatura' });
    const row = within(history).getAllByRole('row')[1];
    expect(row).toHaveTextContent('Emissão');
    expect(row).toHaveTextContent('Faturamento automático');
    expect(row).not.toHaveTextContent('issued');
  });

  it('avisa que a fatura comercial não é documento fiscal', async () => {
    serveInvoice();
    renderPage(<InvoiceDetailPage />, { path: '/faturas/:id', route });

    const notice = await screen.findByRole('note', { name: 'Aviso fiscal' });
    expect(notice).toHaveTextContent('Este documento é uma fatura comercial e não é documento fiscal. Não substitui a nota fiscal.');
  });

  it('lista as notas de crédito emitidas com número, motivo e valor', async () => {
    serveInvoice(
      makeInvoice({
        credit_notes: [
          { id: '0195f1a0-0000-7000-8000-00000000c001', number: 'ACME-000123-NC01', amount: 10000, reason: 'Desconto comercial acordado', created_at: '2026-11-18T13:00:00Z' },
        ],
      }),
    );
    renderPage(<InvoiceDetailPage />, { path: '/faturas/:id', route });

    const notes = await screen.findByRole('table', { name: 'Notas de crédito' });
    const row = within(notes).getByRole('row', { name: /ACME-000123-NC01/ });
    expect(row).toHaveTextContent('Desconto comercial acordado');
    expect(row).toHaveTextContent('R$ 100,00');
  });

  it('informa quando não há notas de crédito', async () => {
    serveInvoice();
    renderPage(<InvoiceDetailPage />, { path: '/faturas/:id', route });

    expect(await screen.findByText('Nenhuma nota de crédito emitida.')).toBeInTheDocument();
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

  it('nova tentativa de cobrança em andamento não bloqueia o botão "Ações"', async () => {
    serveInvoice();
    let release: () => void = () => {};
    const pending = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      http.post('/api/v1/invoices/:id/charges', async () => {
        await pending;
        return HttpResponse.json(invoice.charges[0], { status: 201 });
      }),
    );
    const { user } = renderPage(<InvoiceDetailPage />, { path: '/faturas/:id', route });
    await user.click(await screen.findByRole('button', { name: 'Ações' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Nova tentativa de cobrança' }));
    await user.click(await screen.findByRole('button', { name: 'Solicitar cobrança' }));

    // Enquanto a requisição não termina, o menu abre e só a nova tentativa fica desabilitada.
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Nova tentativa de cobrança' })).not.toBeInTheDocument());
    const actions = screen.getByRole('button', { name: 'Ações' });
    expect(actions).toBeEnabled();
    await user.click(actions);
    expect(await screen.findByRole('menuitem', { name: 'Nova tentativa de cobrança' })).toBeDisabled();
    expect(screen.getByRole('menuitem', { name: 'Emitir nota de crédito' })).toBeEnabled();
    release();
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
    await waitFor(() => expect(screen.getByTestId('live-polite')).toHaveTextContent('Pagamento registrado.'));
    expect(screen.getByTestId('live-assertive')).toHaveTextContent(/Sem conexão/);
  });
});
