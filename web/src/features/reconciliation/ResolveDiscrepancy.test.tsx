import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { AuditEvent, Discrepancy, ReconciliationRun } from '../../api/types';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import {
  ComparisonTable,
  DiscrepancyDetailPage,
  discrepancyResolver,
  ResolveDiscrepancyForm,
  resolutionsFor,
  RunsTab,
} from './ReconciliationPages';

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
    // A digitação caractere a caractere da justificativa é lenta sob carga.
  }, 30_000);
});

describe('opções de resolução', () => {
  it('oferece somente as resoluções aceitas para o tipo', () => {
    expect(resolutionsFor('fee_mismatch')).toEqual(['accept_provider', 'keep_local', 'adjust_ledger', 'dismiss']);
    expect(resolutionsFor('payment_not_reflected')).toEqual(['accept_provider', 'keep_local', 'dismiss']);
    expect(resolutionsFor('missing_local')).toEqual(['keep_local', 'dismiss']);
    expect(resolutionsFor('amount_mismatch', { basis: 'paid' })).toEqual(['accept_provider', 'keep_local', 'adjust_ledger', 'dismiss']);
    expect(resolutionsFor('amount_mismatch', { basis: 'charge' })).toEqual(['keep_local', 'dismiss']);
  });

  it('pagamento não refletido não oferece ajuste no ledger', () => {
    renderPage(<ResolveDiscrepancyForm discrepancy={{ ...discrepancy, kind: 'payment_not_reflected' }} />);
    expect(screen.getByLabelText('Aceitar o valor do provedor')).toBeInTheDocument();
    expect(screen.queryByLabelText('Ajustar o ledger com lançamento compensatório')).not.toBeInTheDocument();
  });

  it('divergência resolvida automaticamente mostra a resolução sem responsável', async () => {
    const auto: Discrepancy = {
      ...discrepancy,
      status: 'resolved',
      resolution: 'auto_resolved',
      justification: 'Divergência não constatada na execução de conciliação.',
      resolved_by: null,
      resolved_at: '2026-11-17T12:00:00Z',
      history: [],
    };
    server.use(http.get('/api/v1/reconciliation/discrepancies/:id', () => HttpResponse.json(auto)));
    renderPage(<DiscrepancyDetailPage />, { path: '/conciliacao/divergencias/:id', route: `/conciliacao/divergencias/${auto.id}` });
    expect(await screen.findByText(/Resolvida automaticamente/)).toBeInTheDocument();
    expect(screen.getByText(/conciliação automática/)).toBeInTheDocument();
    expect(screen.queryByRole('form', { name: 'Resolver divergência' })).not.toBeInTheDocument();
  });
});

describe('responsável pela resolução', () => {
  const resolvedEvent = (actor: Pick<AuditEvent, 'actor_type' | 'actor_id' | 'actor_label'>): AuditEvent => ({
    id: '0195f1a0-0000-7000-8000-0000000a0001',
    occurred_at: '2026-11-17T12:00:00Z',
    action: 'reconciliation.discrepancy_resolved',
    resource_type: 'reconciliation_discrepancy',
    resource_id: discrepancy.id,
    details: { resolution: 'keep_local' },
    ...actor,
  });
  const detected: AuditEvent = {
    id: '0195f1a0-0000-7000-8000-0000000a0000',
    occurred_at: '2026-11-16T12:00:00Z',
    action: 'reconciliation.discrepancy_detected',
    resource_type: 'reconciliation_discrepancy',
    resource_id: discrepancy.id,
    actor_type: 'system',
    actor_id: 'reconciliation',
    actor_label: '',
    details: {},
  };
  const resolved = (overrides: Partial<Discrepancy>): Discrepancy => ({
    ...discrepancy,
    kind: 'fee_mismatch',
    status: 'resolved',
    resolution: 'keep_local',
    justification: 'Taxa local conferida no contrato com o provedor.',
    resolved_at: '2026-11-17T12:00:00Z',
    ...overrides,
  });

  it.each([
    [
      'credencial de API (sem resolved_by) não aparece como conciliação automática',
      resolved({
        resolved_by: null,
        history: [resolvedEvent({ actor_type: 'api_credential', actor_id: '0195f1a0-0000-7000-8000-0000000c7001', actor_label: 'ERP' }), detected],
      }),
      'Responsável: Credencial de API “ERP”',
    ],
    [
      'usuário da sessão aparece pelo nome registrado no histórico',
      resolved({
        resolved_by: '0195f1a0-0000-7000-8000-0000000b0001',
        history: [resolvedEvent({ actor_type: 'user', actor_id: '0195f1a0-0000-7000-8000-0000000b0001', actor_label: 'Ana Owner' }), detected],
      }),
      'Responsável: Ana Owner',
    ],
    [
      'resolução automática é da conciliação',
      resolved({ resolution: 'auto_resolved', resolved_by: null, history: [detected] }),
      'Responsável: conciliação automática',
    ],
  ])('%s', async (_name, d, expected) => {
    server.use(http.get('/api/v1/reconciliation/discrepancies/:id', () => HttpResponse.json(d)));
    renderPage(<DiscrepancyDetailPage />, { path: '/conciliacao/divergencias/:id', route: `/conciliacao/divergencias/${d.id}` });
    expect(await screen.findByText(/^Responsável:/)).toHaveTextContent(expected);
  });

  it('sem o evento de resolução no histórico, usa resolved_by', () => {
    const d = resolved({ resolved_by: '0195f1a0-0000-7000-8000-0000000b0001', history: [] });
    expect(discrepancyResolver(resolved({ resolved_by: null, history: [] }))).toBe('não identificado no histórico');
    renderPage(<>{discrepancyResolver(d)}</>);
    expect(screen.getByText('0195f1a0')).toHaveAttribute('title', d.resolved_by);
  });
});

describe('execuções de conciliação', () => {
  const run = (overrides: Partial<ReconciliationRun> = {}): ReconciliationRun => ({
    id: '0195f1a0-0000-7000-8000-0000000e0001',
    provider: 'simulator',
    status: 'completed',
    window_start: '2026-10-18T12:00:00Z',
    window_end: '2026-11-17T12:00:00Z',
    checked_count: 4,
    discrepancy_count: 1,
    error: '',
    started_at: '2026-11-17T12:00:00Z',
    finished_at: '2026-11-17T12:00:05Z',
    ...overrides,
  });

  async function execute(response: ReconciliationRun) {
    const keys: (string | null)[] = [];
    server.use(
      http.get('/api/v1/reconciliation/runs', () => HttpResponse.json({ data: [] })),
      http.post('/api/v1/reconciliation/runs', ({ request }) => {
        keys.push(request.headers.get('Idempotency-Key'));
        return HttpResponse.json(response);
      }),
    );
    const { user } = renderPage(<RunsTab />);
    await user.click(await screen.findByRole('button', { name: 'Executar conciliação agora' }));
    const dialog = (await screen.findByText(/Compara as cobranças/)).closest('[role="dialog"]') as HTMLElement;
    await user.click(within(dialog).getByRole('button', { name: 'Executar conciliação' }));
    await waitFor(() => expect(keys).toHaveLength(1));
    expect(keys[0]).toBeTruthy();
  }

  it('execução concluída informa verificadas e divergências', async () => {
    await execute(run());
    expect(await screen.findByText('Conciliação concluída: 4 cobrança(s) verificada(s), 1 divergência(s).')).toBeInTheDocument();
  });

  it('recusa por outra execução em andamento não prende a chave do próximo clique', async () => {
    // Como o middleware de idempotência: o resultado fica registrado na chave.
    const stored = new Map<string, { status: number; body: Record<string, unknown> }>();
    const keys: string[] = [];
    let running = true;
    server.use(
      http.get('/api/v1/reconciliation/runs', () => HttpResponse.json({ data: [] })),
      http.post('/api/v1/reconciliation/runs', ({ request }) => {
        const key = request.headers.get('Idempotency-Key') ?? '';
        keys.push(key);
        const previous = stored.get(key);
        if (previous) return HttpResponse.json(previous.body, { status: previous.status });
        const result = running
          ? { status: 409, body: { status: 409, code: 'reconciliation_running', title: 'Conflito', detail: 'Já há uma conciliação em andamento.' } }
          : { status: 200, body: { ...run() } };
        stored.set(key, result);
        return HttpResponse.json(result.body, { status: result.status });
      }),
    );
    const { user } = renderPage(<RunsTab />);
    const runOnce = async () => {
      await user.click(await screen.findByRole('button', { name: 'Executar conciliação agora' }));
      const dialog = (await screen.findByText(/Compara as cobranças/)).closest('[role="dialog"]') as HTMLElement;
      await user.click(within(dialog).getByRole('button', { name: 'Executar conciliação' }));
    };
    await runOnce();
    await waitFor(() => expect(screen.getByTestId('live-assertive')).toHaveTextContent('Já há uma conciliação em andamento.'));
    running = false;
    await runOnce();
    await waitFor(() => expect(keys).toHaveLength(2));
    expect(keys[1]).not.toBe(keys[0]);
    expect(
      await screen.findByText('Conciliação concluída: 4 cobrança(s) verificada(s), 1 divergência(s).', {}, { timeout: 5_000 }),
    ).toBeInTheDocument();
    // Os dois envios com confirmação são lentos sob carga.
  }, 30_000);

  it('execução que falhou no provedor informa a causa como erro', async () => {
    await execute(run({ status: 'failed', checked_count: 0, discrepancy_count: 0, error: 'listagem do provedor: indisponível' }));
    await waitFor(() =>
      expect(screen.getByTestId('live-assertive')).toHaveTextContent('A conciliação não foi concluída: listagem do provedor: indisponível.'),
    );
  });
});
