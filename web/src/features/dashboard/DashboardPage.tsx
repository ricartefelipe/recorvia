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
import { currentMonthInTimeZone, formatMonth } from '../../lib/dates';
import { useUrlFilters } from '../../lib/hooks';
import { formatBasisPoints, formatMoney } from '../../lib/money';
import { useTenant } from '../../session/SessionContext';

export function fetchDashboard(month: string, signal?: AbortSignal): Promise<Dashboard> {
  return api.get<Dashboard>('/api/v1/dashboard', { query: { month }, signal });
}

const DEFINITION_LABELS: Record<string, string> = {
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
  new_subscriptions: 'Novas assinaturas',
  canceled_subscriptions: 'Assinaturas canceladas',
  refunds: 'Reembolsos',
  chargebacks: 'Chargebacks',
  recoverables: 'Valores a recuperar de clientes',
  pending_receipt: 'A receber do provedor',
  open_discrepancies: 'Divergências abertas',
  unknown_operations: 'Operações com resultado desconhecido',
};

function StatCard({
  label,
  value,
  hint,
  definitionKey,
  definitions,
  tone,
  href,
  badge,
}: {
  label: string;
  value: ReactNode;
  hint?: ReactNode;
  definitionKey: string;
  definitions: Record<string, string>;
  tone?: 'danger' | 'warning';
  href?: string;
  badge?: ReactNode;
}) {
  const definition = definitions[definitionKey];
  const color = tone === 'danger' ? 'red.7' : tone === 'warning' ? 'orange.7' : undefined;
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
        {badge}
        {hint && (
          <Text size="xs" c="dimmed">
            {hint}
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

function DashboardContent({ data }: { data: Dashboard }) {
  const money = (v: number | undefined) => formatMoney(v ?? 0, data.currency);
  const defs = data.definitions ?? {};
  const unknownOps = data.unknown_operations ?? 0;
  const openDiscrepancies = data.open_discrepancies ?? 0;

  return (
    <Stack gap="xl">
      <Section
        title="Receita do mês"
        description="Faturado e recebido são medidas diferentes: uma fatura emitida no mês pode ser paga em outro mês."
      >
        <SimpleGrid cols={{ base: 1, sm: 3 }}>
          <StatCard
            label="Receita faturada"
            value={money(data.billed)}
            definitionKey="billed"
            definitions={defs}
            hint={
              data.billed_recurring !== undefined || data.billed_usage !== undefined
                ? `Recorrente ${money(data.billed_recurring)} · Consumo ${money(data.billed_usage)}`
                : 'Total das faturas emitidas no mês'
            }
          />
          <StatCard
            label="Receita recebida"
            value={money(data.received)}
            definitionKey="received"
            definitions={defs}
            hint="Pagamentos confirmados no mês"
          />
          <StatCard
            label="A receber do provedor"
            value={money(data.pending_provider_receipt)}
            definitionKey="pending_receipt"
            definitions={defs}
            hint="Confirmado e ainda não creditado; saldo atual, sem relação com o mês"
            href="/ledger"
          />
        </SimpleGrid>
      </Section>

      <Section title="Recorrência">
        <SimpleGrid cols={{ base: 1, xs: 2, lg: 5 }}>
          <StatCard
            label="MRR"
            value={money(data.mrr)}
            definitionKey="mrr"
            definitions={defs}
            badge={
              <Badge variant="light" color="gray" size="sm">
                Exclui consumo variável
              </Badge>
            }
          />
          <StatCard
            label="ARR"
            value={money(data.arr)}
            definitionKey="arr"
            definitions={defs}
            badge={
              <Badge variant="light" color="gray" size="sm">
                Exclui consumo variável
              </Badge>
            }
          />
          <StatCard label="Assinaturas ativas" value={data.active_subscriptions} definitionKey="active_subscriptions" definitions={defs} href="/assinaturas?status=active" />
          <StatCard label="Novas no mês" value={data.new_subscriptions} definitionKey="new_subscriptions" definitions={defs} />
          <StatCard label="Canceladas no mês" value={data.canceled_subscriptions} definitionKey="canceled_subscriptions" definitions={defs} />
        </SimpleGrid>
      </Section>

      <Section title="Recebíveis e inadimplência">
        <SimpleGrid cols={{ base: 1, xs: 2, lg: 4 }}>
          <StatCard label="Recebíveis em aberto" value={money(data.open_receivables)} definitionKey="open_receivables" definitions={defs} href="/faturas?status=open" />
          <StatCard
            label="Recebíveis vencidos"
            value={money(data.overdue_receivables)}
            definitionKey="overdue_receivables"
            definitions={defs}
            tone={data.overdue_receivables > 0 ? 'warning' : undefined}
            href="/faturas?overdue=true"
          />
          <StatCard label="Taxa de inadimplência" value={formatBasisPoints(data.delinquency_rate_bp)} definitionKey="delinquency_rate_bp" definitions={defs} />
          <StatCard label="Churn" value={formatBasisPoints(data.churn_rate_bp)} definitionKey="churn_rate_bp" definitions={defs} />
        </SimpleGrid>
      </Section>

      <Section title="Estornos e exceções">
        <SimpleGrid cols={{ base: 1, xs: 2, lg: 5 }}>
          <StatCard label="Reembolsos" value={money(data.refunds)} definitionKey="refunds" definitions={defs} href="/reembolsos" />
          <StatCard label="Chargebacks" value={money(data.chargebacks)} definitionKey="chargebacks" definitions={defs} href="/disputas" />
          <StatCard
            label="Valores a recuperar de clientes"
            value={money(data.open_recoverables)}
            definitionKey="recoverables"
            definitions={defs}
            tone={(data.open_recoverables ?? 0) > 0 ? 'warning' : undefined}
            hint="Saldo atual, sem relação com o mês"
            href="/disputas"
          />
          <StatCard
            label="Divergências abertas"
            value={openDiscrepancies}
            definitionKey="open_discrepancies"
            definitions={defs}
            tone={openDiscrepancies > 0 ? 'warning' : undefined}
            href="/conciliacao?aba=divergencias"
          />
          <StatCard
            label="Operações com resultado desconhecido"
            value={unknownOps}
            definitionKey="unknown_operations"
            definitions={defs}
            tone={unknownOps > 0 ? 'danger' : undefined}
            hint={unknownOps > 0 ? 'Reconsulte o provedor antes de cobrar novamente.' : undefined}
            href="/operacoes-pagamento?status=unknown"
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
                    <Table.Th>Fórmula</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {Object.entries(defs).map(([key, text]) => (
                    <Table.Tr key={key}>
                      <Table.Td fw={500}>{DEFINITION_LABELS[key] ?? key}</Table.Td>
                      <Table.Td>{text}</Table.Td>
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
        description={`Indicadores de ${formatMonth(month)} no fuso ${timeZone}.`}
        actions={
          <MonthPickerInput
            aria-label="Mês de referência"
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
