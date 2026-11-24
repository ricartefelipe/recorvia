import { Accordion, Anchor, Badge, Group, Paper, SimpleGrid, Stack, Table, Text, Title } from '@mantine/core';
import { MonthPickerInput } from '@mantine/dates';
import { IconCalendarMonth } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { Link } from 'react-router';
import { api } from '../../api/client';
import type { Dashboard } from '../../api/types';
import { AccessDenied, LoadingState, QueryContent } from '../../components/states';
import { HelpTip, PageHeader } from '../../components/ui';
import { currentMonthInTimeZone, formatDateTime, formatMonth, timeZoneLabel } from '../../lib/dates';
import { useUrlFilters } from '../../lib/hooks';
import { formatBasisPoints, formatMoney } from '../../lib/money';
import type { Permission } from '../../session/permissions';
import { useTenant } from '../../session/SessionContext';

export function fetchDashboard(month: string, signal?: AbortSignal): Promise<Dashboard> {
  return api.get<Dashboard>('/api/v1/dashboard', { query: { month }, signal });
}

/** Chave de um indicador: o nome do campo na resposta, em definitions e em bases. */
export type IndicatorKey = keyof Dashboard['definitions'];
type Basis = Dashboard['bases'][IndicatorKey];

/** Rótulo de cada indicador, na ordem da tabela "Como calculamos". */
export const INDICATOR_LABELS: Record<IndicatorKey, string> = {
  mrr: 'MRR',
  arr: 'ARR',
  billed: 'Receita faturada',
  billed_recurring: 'Faturado recorrente',
  billed_usage: 'Faturado por consumo',
  received: 'Receita recebida',
  open_receivables: 'Recebíveis em aberto',
  overdue_receivables: 'Recebíveis vencidos',
  delinquency_rate_bp: 'Taxa de inadimplência',
  churn_rate_bp: 'Churn',
  active_subscriptions: 'Assinaturas ativas',
  new_subscriptions: 'Novas no mês',
  canceled_subscriptions: 'Canceladas no mês',
  refunds: 'Reembolsos',
  chargebacks: 'Chargebacks',
  open_recoverables: 'Valores a recuperar de clientes',
  pending_provider_receipt: 'A receber do provedor',
  open_discrepancies: 'Divergências abertas',
  unknown_operations: 'Operações com resultado desconhecido',
};

/**
 * Lista que corresponde ao número do card, com o filtro que já existe na API e
 * na tela, e a permissão de leitura da lista: sem ela, o card fica sem link.
 * Indicadores sem lista correspondente (por exemplo, somas do mês ou
 * disputas com valor a recuperar, que a lista não filtra) não têm link.
 */
export const INDICATOR_LISTS: Partial<Record<IndicatorKey, { href: string; permission: Permission }>> = {
  open_receivables: { href: '/faturas?open_balance=true', permission: 'invoices.read' },
  overdue_receivables: { href: '/faturas?overdue=true', permission: 'invoices.read' },
  active_subscriptions: { href: '/assinaturas?in_force=true', permission: 'subscriptions.read' },
  open_discrepancies: { href: '/conciliacao?aba=divergencias', permission: 'reconciliation.read' },
  unknown_operations: { href: '/operacoes-pagamento?status=unknown', permission: 'payments.read' },
};

export const BASIS_LABELS: Record<Basis, string> = {
  month: 'Mês consultado',
  month_end: 'Posição no fim do mês',
  current: 'Posição atual',
};

function StatCard({
  label,
  value,
  hint,
  indicator,
  data,
  tone,
  badge,
}: {
  label: string;
  value: ReactNode;
  hint?: ReactNode;
  indicator: IndicatorKey;
  data: Dashboard;
  tone?: 'danger' | 'warning';
  badge?: ReactNode;
}) {
  const { timeZone, can } = useTenant();
  const definition = data.definitions?.[indicator];
  const basis = data.bases?.[indicator];
  const list = INDICATOR_LISTS[indicator];
  const href = list && can(list.permission) ? list.href : undefined;
  // A lista filtrada mostra a posição de hoje: num mês encerrado, ela difere
  // do número das posições no fim do mês (e o card avisa).
  const todayList = !!href && (basis === 'month_end' || basis === 'current') && data.month !== currentMonthInTimeZone(timeZone);
  const color = tone === 'danger' ? 'red' : tone === 'warning' ? 'orange' : undefined;
  return (
    <Paper withBorder p="md" radius="md" component="section" aria-label={label}>
      <Stack gap={6}>
        <Group justify="space-between" wrap="nowrap" gap={4}>
          <Text size="sm" c="dimmed" fw={500}>
            {label}
          </Text>
          {definition && <HelpTip label={label} text={definition} />}
        </Group>
        <Text fz={26} fw={700} c={color} className="numeric">
          {href ? (
            <Anchor component={Link} to={href} inherit c={color ?? 'inherit'}>
              {value}
            </Anchor>
          ) : (
            value
          )}
        </Text>
        {(badge || basis === 'current') && (
          <Group gap={4}>
            {basis === 'current' && (
              <Badge variant="light" color="blue" size="sm">
                Posição atual
              </Badge>
            )}
            {badge}
          </Group>
        )}
        {basis === 'month_end' && (
          <Text size="xs" c="dimmed">
            Posição em {formatDateTime(data.position_at, timeZone)}
          </Text>
        )}
        {hint && (
          <Text size="xs" c="dimmed">
            {hint}
          </Text>
        )}
        {todayList && (
          <Text size="xs" c="dimmed">
            A lista mostra a posição de hoje.
          </Text>
        )}
      </Stack>
    </Paper>
  );
}

function Section({ title, description, children }: { title: string; description?: string; children: ReactNode }) {
  return (
    <Stack gap="xs">
      <div>
        <Title order={2} size="h4">
          {title}
        </Title>
        {description && (
          <Text size="sm" c="dimmed">
            {description}
          </Text>
        )}
      </div>
      {children}
    </Stack>
  );
}

/**
 * Legenda curta sobre a data de referência; a base de cada indicador aparece
 * no próprio card e na tabela "Como calculamos".
 */
function BasisSummary({ data, timeZone }: { data: Dashboard; timeZone: string }) {
  const bases = Object.values(data.bases ?? {});
  if (!bases.includes('month_end') && !bases.includes('current')) return null;
  return (
    <Text size="sm" c="dimmed" component="p" m={0} aria-label="Base dos indicadores" role="note">
      Posição em {formatDateTime(data.position_at, timeZone)}. A base de cada indicador está em “Como calculamos”, no fim da
      página.
    </Text>
  );
}

function DashboardContent({ data }: { data: Dashboard }) {
  const money = (v: number | undefined) => formatMoney(v ?? 0, data.currency);
  const { timeZone } = useTenant();
  const defs: Partial<Dashboard['definitions']> = data.definitions ?? {};
  const unknownOps = data.unknown_operations ?? 0;
  const openDiscrepancies = data.open_discrepancies ?? 0;

  return (
    <Stack gap="xl">
      <BasisSummary data={data} timeZone={timeZone} />
      <Section
        title="Receita do mês"
        description="Faturado e recebido são medidas diferentes: uma fatura emitida no mês pode ser paga em outro mês."
      >
        <SimpleGrid cols={{ base: 1, sm: 3 }}>
          <StatCard
            label="Receita faturada"
            value={money(data.billed)}
            indicator="billed"
            data={data}
            hint={
              data.billed_recurring !== undefined || data.billed_usage !== undefined
                ? `Recorrente ${money(data.billed_recurring)} · Consumo ${money(data.billed_usage)}`
                : 'Total das faturas emitidas no mês'
            }
          />
          <StatCard
            label="Receita recebida"
            value={money(data.received)}
            indicator="received"
            data={data}
            hint="Pagamentos confirmados no mês"
          />
          <StatCard
            label="A receber do provedor"
            value={money(data.pending_provider_receipt)}
            indicator="pending_provider_receipt"
            data={data}
            hint="Confirmado pelo provedor e ainda não creditado"
          />
        </SimpleGrid>
      </Section>

      <Section title="Recorrência">
        <SimpleGrid cols={{ base: 1, xs: 2, lg: 5 }}>
          <StatCard
            label="MRR"
            value={money(data.mrr)}
            indicator="mrr"
            data={data}
            badge={
              <Badge variant="light" color="gray" size="sm">
                Exclui consumo variável
              </Badge>
            }
          />
          <StatCard
            label="ARR"
            value={money(data.arr)}
            indicator="arr"
            data={data}
            badge={
              <Badge variant="light" color="gray" size="sm">
                Exclui consumo variável
              </Badge>
            }
          />
          <StatCard label="Assinaturas ativas" value={data.active_subscriptions} indicator="active_subscriptions" data={data} />
          <StatCard label="Novas no mês" value={data.new_subscriptions} indicator="new_subscriptions" data={data} />
          <StatCard label="Canceladas no mês" value={data.canceled_subscriptions} indicator="canceled_subscriptions" data={data} />
        </SimpleGrid>
      </Section>

      <Section title="Recebíveis e inadimplência">
        <SimpleGrid cols={{ base: 1, xs: 2, lg: 4 }}>
          <StatCard label="Recebíveis em aberto" value={money(data.open_receivables)} indicator="open_receivables" data={data} />
          <StatCard
            label="Recebíveis vencidos"
            value={money(data.overdue_receivables)}
            indicator="overdue_receivables"
            data={data}
            tone={data.overdue_receivables > 0 ? 'warning' : undefined}
          />
          <StatCard label="Taxa de inadimplência" value={formatBasisPoints(data.delinquency_rate_bp)} indicator="delinquency_rate_bp" data={data} />
          <StatCard label="Churn" value={formatBasisPoints(data.churn_rate_bp)} indicator="churn_rate_bp" data={data} />
        </SimpleGrid>
      </Section>

      <Section title="Estornos e exceções">
        <SimpleGrid cols={{ base: 1, xs: 2, lg: 5 }}>
          <StatCard label="Reembolsos" value={money(data.refunds)} indicator="refunds" data={data} />
          <StatCard label="Chargebacks" value={money(data.chargebacks)} indicator="chargebacks" data={data} />
          <StatCard
            label="Valores a recuperar de clientes"
            value={money(data.open_recoverables)}
            indicator="open_recoverables"
            data={data}
            tone={(data.open_recoverables ?? 0) > 0 ? 'warning' : undefined}
          />
          <StatCard
            label="Divergências abertas"
            value={openDiscrepancies}
            indicator="open_discrepancies"
            data={data}
            tone={openDiscrepancies > 0 ? 'warning' : undefined}
          />
          <StatCard
            label="Operações com resultado desconhecido"
            value={unknownOps}
            indicator="unknown_operations"
            data={data}
            tone={unknownOps > 0 ? 'danger' : undefined}
            hint={unknownOps > 0 ? 'Reconsulte o provedor antes de cobrar novamente.' : undefined}
          />
        </SimpleGrid>
      </Section>

      <Accordion variant="contained" radius="md">
        <Accordion.Item value="definitions">
          <Accordion.Control>Como calculamos</Accordion.Control>
          <Accordion.Panel>
            {Object.keys(defs).length === 0 ? (
              <Text size="sm" c="dimmed">
                O servidor não informou as fórmulas para este mês.
              </Text>
            ) : (
              <Table>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Indicador</Table.Th>
                    <Table.Th>Base</Table.Th>
                    <Table.Th>Fórmula</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {(Object.keys(INDICATOR_LABELS) as IndicatorKey[])
                    .filter((key) => defs[key])
                    .map((key) => (
                      <Table.Tr key={key}>
                        <Table.Td fw={500}>{INDICATOR_LABELS[key]}</Table.Td>
                        <Table.Td>{data.bases?.[key] ? BASIS_LABELS[data.bases[key]] : '—'}</Table.Td>
                        <Table.Td>{defs[key]}</Table.Td>
                      </Table.Tr>
                    ))}
                </Table.Tbody>
              </Table>
            )}
            <Text size="xs" c="dimmed" mt="sm">
              Taxas em pontos-base convertidas para percentual (100 pontos-base = 1%). Valores em {data.currency}.
            </Text>
          </Accordion.Panel>
        </Accordion.Item>
      </Accordion>
    </Stack>
  );
}

const lowerFirst = (text: string) => text.charAt(0).toLowerCase() + text.slice(1);

export function DashboardPage() {
  const { keys, timeZone, can } = useTenant();
  const currentMonth = currentMonthInTimeZone(timeZone);
  const [filters, setFilters] = useUrlFilters(['mes'] as const);
  const month = /^\d{4}-\d{2}$/.test(filters.mes) ? filters.mes : currentMonth;

  const query = useQuery({
    queryKey: keys('dashboard', month),
    queryFn: ({ signal }) => fetchDashboard(month, signal),
    placeholderData: keepPreviousData,
    enabled: can('dashboard.read'),
  });

  if (!can('dashboard.read')) return <AccessDenied />;

  return (
    <>
      <PageHeader
        title="Painel financeiro"
        description={
          <>
            Indicadores de {formatMonth(month)}, no{' '}
            <span title={timeZone}>{lowerFirst(timeZoneLabel(timeZone))}</span>.
          </>
        }
        actions={
          <MonthPickerInput
            aria-label={`Mês de referência: ${formatMonth(month)}`}
            leftSection={<IconCalendarMonth size={16} />}
            value={`${month}-01`}
            onChange={(value) => setFilters({ mes: value ? value.slice(0, 7) : null })}
            maxDate={`${currentMonth}-01`}
            valueFormat="MMMM [de] YYYY"
            w={220}
          />
        }
      />
      <QueryContent query={query} loading={<LoadingState variant="cards" />}>
        {(data) => <DashboardContent data={data} />}
      </QueryContent>
    </>
  );
}
