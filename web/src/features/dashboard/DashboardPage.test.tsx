import { screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { Dashboard } from '../../api/types';
import { currentMonthInTimeZone } from '../../lib/dates';
import type { Permission } from '../../session/permissions';
import { makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { BASIS_LABELS, DashboardPage, INDICATOR_LABELS, INDICATOR_LISTS, type IndicatorKey } from './DashboardPage';

const keys = Object.keys(INDICATOR_LABELS) as IndicatorKey[];

const bases: Dashboard['bases'] = {
  mrr: 'current',
  arr: 'current',
  billed: 'month',
  billed_recurring: 'month',
  billed_usage: 'month',
  received: 'month',
  open_receivables: 'month_end',
  overdue_receivables: 'month_end',
  delinquency_rate_bp: 'month_end',
  churn_rate_bp: 'month',
  active_subscriptions: 'month_end',
  new_subscriptions: 'month',
  canceled_subscriptions: 'month',
  refunds: 'month',
  chargebacks: 'month',
  open_recoverables: 'month_end',
  pending_provider_receipt: 'month_end',
  open_discrepancies: 'current',
  unknown_operations: 'current',
};

// Resposta no formato da API: uma fórmula e uma base por indicador, com a
// chave igual ao nome do campo.
function makeDashboard(overrides: Partial<Dashboard> = {}): Dashboard {
  const definitions = Object.fromEntries(keys.map((k) => [k, `Fórmula de ${k}.`])) as Dashboard['definitions'];
  return {
    month: '2025-01',
    currency: 'BRL',
    position_at: '2025-02-01T02:59:59.999999Z',
    mrr: 1500000,
    arr: 18000000,
    billed: 900000,
    billed_recurring: 700000,
    billed_usage: 200000,
    received: 800000,
    open_receivables: 300000,
    overdue_receivables: 120000,
    delinquency_rate_bp: 1250,
    churn_rate_bp: 500,
    active_subscriptions: 42,
    new_subscriptions: 3,
    canceled_subscriptions: 2,
    refunds: 1000,
    chargebacks: 0,
    open_recoverables: 0,
    pending_provider_receipt: 35000,
    open_discrepancies: 4,
    unknown_operations: 1,
    definitions,
    bases,
    ...overrides,
  };
}

const CARDS: [string, IndicatorKey][] = [
  ['Receita faturada', 'billed'],
  ['Receita recebida', 'received'],
  ['A receber do provedor', 'pending_provider_receipt'],
  ['MRR', 'mrr'],
  ['ARR', 'arr'],
  ['Assinaturas ativas', 'active_subscriptions'],
  ['Novas no mês', 'new_subscriptions'],
  ['Canceladas no mês', 'canceled_subscriptions'],
  ['Recebíveis em aberto', 'open_receivables'],
  ['Recebíveis vencidos', 'overdue_receivables'],
  ['Taxa de inadimplência', 'delinquency_rate_bp'],
  ['Churn', 'churn_rate_bp'],
  ['Reembolsos', 'refunds'],
  ['Chargebacks', 'chargebacks'],
  ['Valores a recuperar de clientes', 'open_recoverables'],
  ['Divergências abertas', 'open_discrepancies'],
  ['Operações com resultado desconhecido', 'unknown_operations'],
];

function serve(data: Dashboard, months: string[] = []) {
  server.use(
    http.get('/api/v1/dashboard', ({ request }) => {
      months.push(new URL(request.url).searchParams.get('month') ?? '');
      return HttpResponse.json(data);
    }),
  );
}

describe('painel financeiro', () => {
  it('cada card tem a dica com a fórmula devolvida pela API', async () => {
    serve(makeDashboard());
    renderPage(<DashboardPage />, { route: '/?mes=2025-01' });
    await screen.findByRole('region', { name: 'MRR' });
    for (const [label] of CARDS) {
      const card = screen.getByRole('region', { name: label });
      expect(within(card).getByRole('button', { name: `Como calculamos: ${label}` })).toBeInTheDocument();
    }
    // O faturado detalha recorrente e consumo (definições próprias na tabela).
    expect(within(screen.getByRole('region', { name: 'Receita faturada' })).getByText(/Recorrente R\$\s7\.000,00 · Consumo R\$\s2\.000,00/)).toBeInTheDocument();
  });

  it('consulta o mês escolhido e rotula a base de cada indicador', async () => {
    const months: string[] = [];
    serve(makeDashboard(), months);
    renderPage(<DashboardPage />, { route: '/?mes=2025-01' });
    const mrr = await screen.findByRole('region', { name: 'MRR' });
    expect(months).toEqual(['2025-01']);
    expect(screen.getByText(/Indicadores de janeiro de 2025/)).toBeInTheDocument();

    for (const [label, key] of CARDS) {
      const card = screen.getByRole('region', { name: label });
      const current = within(card).queryByText('Posição atual');
      const position = within(card).queryByText(/Posição em 31\/01\/2025,? 23:59/);
      expect(Boolean(current), `${label}: posição atual`).toBe(bases[key] === 'current');
      expect(Boolean(position), `${label}: posição no fim do mês`).toBe(bases[key] === 'month_end');
    }
    expect(within(mrr).getByText('Exclui consumo variável')).toBeInTheDocument();

    const summary = screen.getByRole('region', { name: 'Base dos indicadores' });
    expect(within(summary).getByText(/Posição em 31\/01\/2025,? 23:59: Recebíveis em aberto, Recebíveis vencidos/)).toBeInTheDocument();
    expect(within(summary).getByText(/Posição atual, seja qual for o mês: MRR, ARR, Divergências abertas/)).toBeInTheDocument();
  });

  it('"Como calculamos" lista todos os indicadores com rótulo, base e fórmula', async () => {
    serve(makeDashboard());
    const { user } = renderPage(<DashboardPage />);
    await user.click(await screen.findByRole('button', { name: 'Como calculamos' }));
    const table = await screen.findByRole('table');
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(keys.length);
    keys.forEach((key, i) => {
      const cells = within(rows[i]).getAllByRole('cell');
      expect(cells[0]).toHaveTextContent(INDICATOR_LABELS[key]);
      expect(cells[1]).toHaveTextContent(BASIS_LABELS[bases[key]]);
      expect(cells[2]).toHaveTextContent(`Fórmula de ${key}.`);
      // Nenhuma chave crua em inglês no lugar do rótulo.
      expect(within(table).queryByText(key)).not.toBeInTheDocument();
    });
  });

  it('sem fórmulas do servidor, avisa e não mostra dicas', async () => {
    serve(makeDashboard({ definitions: undefined as unknown as Dashboard['definitions'] }));
    const { user } = renderPage(<DashboardPage />);
    const card = await screen.findByRole('region', { name: 'MRR' });
    expect(within(card).queryByRole('button', { name: /Como calculamos/ })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Como calculamos' }));
    expect(await screen.findByText('O servidor não informou as fórmulas para este mês.')).toBeInTheDocument();
  });

  it('destaca vencidos e operações desconhecidas', async () => {
    serve(makeDashboard());
    renderPage(<DashboardPage />);
    const unknown = await screen.findByRole('region', { name: 'Operações com resultado desconhecido' });
    expect(within(unknown).getByText('Reconsulte o provedor antes de cobrar novamente.')).toBeInTheDocument();
    expect(within(unknown).getByRole('link', { name: '1' })).toHaveAttribute('href', '/operacoes-pagamento?status=unknown');
    const discrepancies = screen.getByRole('region', { name: 'Divergências abertas' });
    expect(within(discrepancies).getByRole('link', { name: '4' })).toHaveAttribute('href', '/conciliacao?aba=divergencias');
  });

  it('cada card com lista correspondente leva à lista com o filtro do número', async () => {
    serve(makeDashboard());
    renderPage(<DashboardPage />, { route: '/?mes=2025-01' });
    await screen.findByRole('region', { name: 'MRR' });
    const expected: Partial<Record<IndicatorKey, string>> = {
      open_receivables: '/faturas?open_balance=true',
      overdue_receivables: '/faturas?overdue=true',
      active_subscriptions: '/assinaturas?in_force=true',
      open_discrepancies: '/conciliacao?aba=divergencias',
      unknown_operations: '/operacoes-pagamento?status=unknown',
    };
    for (const [label, key] of CARDS) {
      const link = within(screen.getByRole('region', { name: label })).queryByRole('link');
      if (expected[key]) {
        expect(link, label).toHaveAttribute('href', expected[key]);
      } else {
        // Somas do mês e disputas com valor a recuperar não têm lista filtrada correspondente.
        expect(link, label).toBeNull();
      }
    }
  });

  it('sem a permissão de leitura da lista, o card não tem link', async () => {
    const permissions: Permission[] = ['dashboard.read', 'invoices.read'];
    serve(makeDashboard());
    renderPage(<DashboardPage />, { route: '/?mes=2025-01', session: makeSession({ permissions }) });
    await screen.findByRole('region', { name: 'MRR' });
    for (const [label, key] of CARDS) {
      const list = INDICATOR_LISTS[key];
      const link = within(screen.getByRole('region', { name: label })).queryByRole('link');
      expect(Boolean(link), label).toBe(!!list && permissions.includes(list.permission));
    }
    // O número continua visível, só sem o link.
    expect(within(screen.getByRole('region', { name: 'Assinaturas ativas' })).getByText('42')).toBeInTheDocument();
    expect(within(screen.getByRole('region', { name: 'Divergências abertas' })).getByText('4')).toBeInTheDocument();
  });

  it('num mês encerrado, avisa nos cards de posição que a lista mostra a posição de hoje', async () => {
    serve(makeDashboard());
    renderPage(<DashboardPage />, { route: '/?mes=2025-01' });
    await screen.findByRole('region', { name: 'MRR' });
    for (const [label, key] of CARDS) {
      const note = within(screen.getByRole('region', { name: label })).queryByText('A lista mostra a posição de hoje.');
      const linkedPosition = !!INDICATOR_LISTS[key] && bases[key] !== 'month';
      expect(Boolean(note), label).toBe(linkedPosition);
    }
  });

  it('no mês corrente, a lista e o número têm a mesma posição e não há aviso', async () => {
    const month = currentMonthInTimeZone('America/Sao_Paulo');
    serve(makeDashboard({ month }));
    renderPage(<DashboardPage />);
    await screen.findByRole('region', { name: 'MRR' });
    expect(screen.queryByText('A lista mostra a posição de hoje.')).not.toBeInTheDocument();
    expect(within(screen.getByRole('region', { name: 'Recebíveis em aberto' })).getByRole('link')).toHaveAttribute('href', '/faturas?open_balance=true');
  });

  it('sem permissão, não consulta o painel', async () => {
    const months: string[] = [];
    serve(makeDashboard(), months);
    renderPage(<DashboardPage />, { session: makeSession({ permissions: [] }) });
    expect(await screen.findByText(/acesso/i)).toBeInTheDocument();
    expect(months).toEqual([]);
  });
});
