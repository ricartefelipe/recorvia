import { screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { Discrepancy } from '../../api/types';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { ComparisonTable, ResolveDiscrepancyForm } from './ReconciliationPages';

const discrepancy: Discrepancy = {
  id: '0195f1a0-0000-7000-8000-0000000d1501',
  kind: 'amount_mismatch',
  status: 'open',
  created_at: '2026-11-16T12:00:00Z',
  charge_id: '0195f1a0-0000-7000-8000-00000000c001',
  local_value: { amount: 15990, currency: 'BRL', status: 'paid' },
  provider_value: { amount: 15000, currency: 'BRL', status: 'paid' },
};

describe('resolução de divergência', () => {
  it('compara valores locais e do provedor destacando a diferença', () => {
    renderPage(<ComparisonTable discrepancy={discrepancy} />);
    const rows = screen.getAllByRole('row');
    const amount = rows.find((r) => r.textContent?.startsWith('amount'));
    const status = rows.find((r) => r.textContent?.startsWith('status'));
    expect(amount).toHaveTextContent('(diferente)');
    expect(amount).toHaveTextContent('R$ 159,90');
    expect(amount).toHaveTextContent('R$ 150,00');
    expect(status).not.toHaveTextContent('diferente');
  });

  it('exige justificativa de 10 caracteres e confirmação antes de enviar', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.post('/api/v1/reconciliation/discrepancies/:id/resolve', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ ...discrepancy, status: 'resolved' });
      }),
    );
    const { user } = renderPage(<ResolveDiscrepancyForm discrepancy={discrepancy} />);
    expect(screen.getByText(/Responsável pela resolução/)).toHaveTextContent('Ana Owner');
    await user.click(screen.getByLabelText('Aceitar o valor do provedor'));
    await user.type(screen.getByLabelText(/Justificativa/), 'curta');
    await user.click(screen.getByRole('button', { name: 'Continuar' }));
    expect(await screen.findByText('A justificativa deve ter pelo menos 10 caracteres.')).toBeInTheDocument();

    await user.type(screen.getByLabelText(/Justificativa/), ' demais: taxa conferida no extrato');
    await user.click(screen.getByRole('button', { name: 'Continuar' }));
    await user.click(await screen.findByRole('button', { name: 'Resolver divergência' }));
    await waitFor(() =>
      expect(bodies).toEqual([{ resolution: 'accept_provider', justification: 'curta demais: taxa conferida no extrato' }]),
    );
  });
});
