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
  ReconciliationPage,
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
  version: 2,
};

describe('resolução de divergência', () => {
  it('compara valores locais e do provedor destacando a diferença', () => {
    renderPage(<ComparisonTable discrepancy={discrepancy} />);
    const rows = screen.getAllByRole('row');
    const amount = rows.find((r) => r.textContent?.startsWith('Valor'));
    const status = rows.find((r) => r.textContent?.startsWith('Situação'));
    // Campos e situações aparecem em português, não com o código do contrato.
    expect(status).toHaveTextContent('Paga');
    expect(screen.queryByText('amount')).not.toBeInTheDocument();
    expect(amount).toHaveTextContent('(diferente)');
    expect(amount).toHaveTextContent('R$ 159,90');
    expect(amount).toHaveTextContent('R$ 150,00');
    expect(status).not.toHaveTextContent('diferente');
  });

  it('exige justificativa de 10 caracteres e confirmação antes de enviar', async () => {
    const bodies: unknown[] = [];
    const versions: (string | null)[] = [];
    server.use(
      http.post('/api/v1/reconciliation/discrepancies/:id/resolve', async ({ request }) => {
        bodies.push(await request.json());
        versions.push(request.headers.get('If-Match'));
        return HttpResponse.json({ ...discrepancy, status: 'resolved', version: 3 }, { headers: { ETag: '"3"' } });
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
    // A resolução leva a versão da divergência consultada.
    expect(versions).toEqual(['"2"']);
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

describe('listagens paginadas da conciliação', () => {
  it('divergências seguem o next_cursor', async () => {
    const cursors: (string | null)[] = [];
    server.use(
      http.get('/api/v1/reconciliation/discrepancies', ({ request }) => {
        const url = new URL(request.url);
        const cursor = url.searchParams.get('cursor');
        cursors.push(cursor);
        return cursor
          ? HttpResponse.json({ data: [{ ...discrepancy, id: 'd2', provider_charge_id: 'pay_segunda' }], next_cursor: null })
          : HttpResponse.json({ data: [{ ...discrepancy, provider_charge_id: 'pay_primeira' }], next_cursor: 'div-2' });
      }),
    );
    const { user } = renderPage(<ReconciliationPage />, { route: '/?aba=divergencias' });
    expect(await screen.findByText('pay_primeira')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /Próxima/ }));
    expect(await screen.findByText('pay_segunda')).toBeInTheDocument();
    expect(cursors).toEqual([null, 'div-2']);
  });

  it('execuções seguem o next_cursor', async () => {
    const run = (id: string, checked: number): ReconciliationRun => ({
      id,
      provider: 'simulator',
      trigger: 'scheduled',
      status: 'completed',
      window_start: '2026-10-18T12:00:00Z',
      window_end: '2026-11-17T12:00:00Z',
      checked_count: checked,
      discrepancy_count: 0,
      error: '',
      attempts: 1,
      started_at: '2026-11-17T12:00:00Z',
      finished_at: '2026-11-17T12:00:05Z',
    });
    const cursors: (string | null)[] = [];
    server.use(
      http.get('/api/v1/reconciliation/runs', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('cursor');
        cursors.push(cursor);
        return cursor ? HttpResponse.json({ data: [run('r2', 777)], next_cursor: null }) : HttpResponse.json({ data: [run('r1', 555)], next_cursor: 'exec-2' });
      }),
    );
    const { user } = renderPage(<RunsTab />);
    expect(await screen.findByText('555')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /Próxima/ }));
    expect(await screen.findByText('777')).toBeInTheDocument();
    expect(cursors).toEqual([null, 'exec-2']);
  });
});

describe('execuções de conciliação', () => {
  const RUN_ID = '0195f1a0-0000-7000-8000-0000000e0001';
  const run = (overrides: Partial<ReconciliationRun> = {}): ReconciliationRun => ({
    id: RUN_ID,
    provider: 'simulator',
    trigger: 'manual',
    status: 'completed',
    window_start: '2026-10-18T12:00:00Z',
    window_end: '2026-11-17T12:00:00Z',
    checked_count: 4,
    discrepancy_count: 1,
    error: '',
    attempts: 1,
    started_at: '2026-11-17T12:00:00Z',
    finished_at: '2026-11-17T12:00:05Z',
    ...overrides,
  });
  const queued = (overrides: Partial<ReconciliationRun> = {}): ReconciliationRun =>
    run({ status: 'running', checked_count: 0, discrepancy_count: 0, attempts: 0, finished_at: null, ...overrides });

  /** Intervalo curto de acompanhamento, para os testes não esperarem 2 s. */
  const POLL_MS = 20;

  async function confirmRun(user: ReturnType<typeof renderPage>['user']) {
    await user.click(await screen.findByRole('button', { name: 'Executar conciliação agora' }));
    const dialog = (await screen.findByText(/Compara as cobranças/)).closest('[role="dialog"]') as HTMLElement;
    await user.click(within(dialog).getByRole('button', { name: 'Executar conciliação' }));
  }

  /**
   * API falsa da conciliação assíncrona: o POST registra a execução e responde
   * 202 com ela em andamento; as consultas seguintes devolvem `states` em
   * ordem (a última se repete), como o worker avançando a execução.
   */
  function asyncApi(states: ReconciliationRun[]) {
    const calls = { post: [] as (string | null)[], get: 0, list: 0 };
    let current: ReconciliationRun | null = null;
    server.use(
      http.get('/api/v1/reconciliation/runs', () => {
        calls.list += 1;
        return HttpResponse.json({ data: current ? [current] : [] });
      }),
      http.post('/api/v1/reconciliation/runs', ({ request }) => {
        calls.post.push(request.headers.get('Idempotency-Key'));
        current = queued();
        return HttpResponse.json(current, { status: 202, headers: { Location: `/api/v1/reconciliation/runs/${RUN_ID}` } });
      }),
      http.get('/api/v1/reconciliation/runs/:id', () => {
        current = states[Math.min(calls.get, states.length - 1)];
        calls.get += 1;
        return HttpResponse.json(current);
      }),
    );
    return calls;
  }

  it('registra a execução, acompanha até terminar e informa verificadas e divergências', async () => {
    const calls = asyncApi([queued(), queued({ attempts: 1 }), run()]);
    const { user } = renderPage(<RunsTab pollIntervalMs={POLL_MS} />);
    await confirmRun(user);
    // O pedido é aceito de imediato: a tela mostra a execução em andamento.
    expect(await screen.findByRole('status')).toHaveTextContent('Conciliação em andamento');
    const table = await screen.findByRole('table', { name: 'Execuções de conciliação' });
    expect(within(table).getAllByRole('row')[1]).toHaveTextContent('Em execução');
    expect(await screen.findByText('Conciliação concluída: 4 cobrança(s) verificada(s), 1 divergência(s).', {}, { timeout: 5_000 })).toBeInTheDocument();
    // Um único pedido; o acompanhamento é por consulta, sem repetir o POST.
    expect(calls.post).toHaveLength(1);
    expect(calls.post[0]).toBeTruthy();
    expect(calls.get).toBeGreaterThanOrEqual(3);
    await waitFor(() => expect(within(screen.getByRole('table', { name: 'Execuções de conciliação' })).getAllByRole('row')[1]).toHaveTextContent('Concluída'));
    await waitFor(() => expect(screen.queryByRole('status')).not.toBeInTheDocument());
    // Depois de encerrada, o acompanhamento para.
    const polls = calls.get;
    await new Promise((resolve) => setTimeout(resolve, POLL_MS * 5));
    expect(calls.get).toBe(polls);
  }, 30_000);

  it('a chave de idempotência vale só para o pedido: a execução seguinte usa outra', async () => {
    const calls = asyncApi([run()]);
    const { user } = renderPage(<RunsTab pollIntervalMs={POLL_MS} />);
    await confirmRun(user);
    await screen.findByText('Conciliação concluída: 4 cobrança(s) verificada(s), 1 divergência(s).', {}, { timeout: 5_000 });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Executar conciliação agora' })).toBeEnabled());
    await confirmRun(user);
    await waitFor(() => expect(calls.post).toHaveLength(2));
    expect(calls.post[1]).toBeTruthy();
    expect(calls.post[1]).not.toBe(calls.post[0]);
    // Os dois envios com confirmação são lentos sob carga.
  }, 30_000);

  it('recusa por outra execução em andamento não prende a chave do próximo clique', async () => {
    // Como a API: a recusa 409 com Retry-After não fica registrada na chave.
    const keys: string[] = [];
    let running = true;
    server.use(
      http.get('/api/v1/reconciliation/runs', () => HttpResponse.json({ data: [] })),
      http.get('/api/v1/reconciliation/runs/:id', () => HttpResponse.json(run())),
      http.post('/api/v1/reconciliation/runs', ({ request }) => {
        keys.push(request.headers.get('Idempotency-Key') ?? '');
        return running
          ? HttpResponse.json(
              { status: 409, code: 'reconciliation_running', title: 'Conflito', detail: 'Já há uma conciliação em andamento.' },
              { status: 409, headers: { 'Retry-After': '60' } },
            )
          : HttpResponse.json(queued(), { status: 202 });
      }),
    );
    const { user } = renderPage(<RunsTab pollIntervalMs={POLL_MS} />);
    await confirmRun(user);
    await waitFor(() => expect(screen.getByTestId('live-assertive')).toHaveTextContent('Já há uma conciliação em andamento.'));
    running = false;
    await confirmRun(user);
    await waitFor(() => expect(keys).toHaveLength(2));
    expect(keys[1]).not.toBe(keys[0]);
    expect(
      await screen.findByText('Conciliação concluída: 4 cobrança(s) verificada(s), 1 divergência(s).', {}, { timeout: 5_000 }),
    ).toBeInTheDocument();
    // Os dois envios com confirmação são lentos sob carga.
  }, 30_000);

  it('execução que falhou no provedor informa a causa como erro', async () => {
    asyncApi([queued({ attempts: 1 }), run({ status: 'failed', checked_count: 0, discrepancy_count: 0, error: 'listagem do provedor: indisponível' })]);
    const { user } = renderPage(<RunsTab pollIntervalMs={POLL_MS} />);
    await confirmRun(user);
    await waitFor(
      () => expect(screen.getByTestId('live-assertive')).toHaveTextContent('A conciliação não foi concluída: listagem do provedor: indisponível.'),
      { timeout: 5_000 },
    );
  }, 30_000);

  it('execução parcial informa o aviso com a causa', async () => {
    const warning = 'Execução parcial (prazo da coleta esgotado): nem todas as cobranças e operações do escopo da execução foram verificadas.';
    asyncApi([run({ error: warning })]);
    const { user } = renderPage(<RunsTab pollIntervalMs={POLL_MS} />);
    await confirmRun(user);
    await waitFor(() => expect(screen.getByTestId('live-polite')).toHaveTextContent(`Conciliação concluída com aviso. 4 cobrança(s) verificada(s), 1 divergência(s). ${warning}`), {
      timeout: 5_000,
    });
  }, 30_000);

  it('repetição de pedido já encerrado (200) informa o resultado sem acompanhar', async () => {
    let gets = 0;
    server.use(
      http.get('/api/v1/reconciliation/runs', () => HttpResponse.json({ data: [] })),
      http.get('/api/v1/reconciliation/runs/:id', () => {
        gets += 1;
        return HttpResponse.json(run());
      }),
      http.post('/api/v1/reconciliation/runs', () => HttpResponse.json(run())),
    );
    const { user } = renderPage(<RunsTab pollIntervalMs={POLL_MS} />);
    await confirmRun(user);
    expect(await screen.findByText('Conciliação concluída: 4 cobrança(s) verificada(s), 1 divergência(s).')).toBeInTheDocument();
    expect(gets).toBe(0);
  }, 30_000);

  it('acompanha a execução em andamento da lista (agendada ou pedida por outra pessoa) e mostra a origem', async () => {
    let lists = 0;
    server.use(
      http.get('/api/v1/reconciliation/runs', () => {
        lists += 1;
        const current = lists < 3 ? queued({ trigger: 'scheduled', attempts: 2 }) : run({ trigger: 'scheduled', attempts: 2 });
        return HttpResponse.json({ data: [current] });
      }),
    );
    renderPage(<RunsTab pollIntervalMs={POLL_MS} />);
    const table = await screen.findByRole('table', { name: 'Execuções de conciliação' });
    const row = () => within(table).getAllByRole('row')[1];
    expect(row()).toHaveTextContent('Agendada');
    expect(row()).toHaveTextContent('Retomada depois de interrupção (tentativa 2)');
    expect(screen.getByRole('status')).toHaveTextContent('Conciliação em andamento');
    await waitFor(() => expect(row()).toHaveTextContent('Concluída'), { timeout: 5_000 });
    await waitFor(() => expect(screen.queryByRole('status')).not.toBeInTheDocument());
    const settled = lists;
    await new Promise((resolve) => setTimeout(resolve, POLL_MS * 5));
    expect(lists).toBe(settled);
  }, 30_000);

  it('consulta recusada encerra o acompanhamento com aviso', async () => {
    server.use(
      http.get('/api/v1/reconciliation/runs', () => HttpResponse.json({ data: [] })),
      http.post('/api/v1/reconciliation/runs', () => HttpResponse.json(queued(), { status: 202 })),
      http.get('/api/v1/reconciliation/runs/:id', () =>
        HttpResponse.json({ status: 404, code: 'reconciliation_run_not_found', title: 'Não encontrado', detail: 'Execução de conciliação não encontrada.' }, { status: 404 }),
      ),
    );
    const { user } = renderPage(<RunsTab pollIntervalMs={POLL_MS} />);
    await confirmRun(user);
    await waitFor(() => expect(screen.getByTestId('live-assertive')).toHaveTextContent('Não foi possível acompanhar a execução'), { timeout: 5_000 });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Executar conciliação agora' })).toBeEnabled());
  }, 30_000);
});
