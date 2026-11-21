import { Badge, Code, Paper, Stack, Table, Text, Title, Tooltip } from '@mantine/core';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import type { Subscription } from '../../api/types';
import { DateTime, Money, StatusBadge } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { CursorPager } from '../../components/ui';
import { useCursorPagination, useReturnFromEmptyPage } from '../../lib/hooks';
import { aggregationHelp, aggregationLabels, labelOf, usageEventStatusLabels } from '../../lib/labels';
import { formatQuantity } from '../../lib/money';
import { useTenant } from '../../session/SessionContext';
import { useMetrics } from '../catalog/api';
import { subscriptionsApi } from './api';

function UsageEvents({ subscription }: { subscription: Subscription }) {
  const { keys } = useTenant();
  const metrics = useMetrics();
  const pagination = useCursorPagination(subscription.id);
  const query = useQuery({
    queryKey: keys('usage', 'events', subscription.id, pagination.cursor),
    queryFn: ({ signal }) => subscriptionsApi.usageEvents({ subscription_id: subscription.id, cursor: pagination.cursor, limit: 25 }, signal),
    placeholderData: keepPreviousData,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);
  const metricName = new Map((metrics.data?.data ?? []).map((m) => [m.id, m.name]));
  return (
    <QueryContent query={query} loading={<LoadingState />}>
      {(page) =>
        page.data.length === 0 ? (
          <EmptyState title="Nenhum evento de consumo" description="Eventos enviados pela integração aparecem aqui, deduplicados pelo identificador externo." />
        ) : (
          <>
            <Table.ScrollContainer minWidth={820}>
              <Table aria-label="Eventos de consumo">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Identificador externo</Table.Th>
                    <Table.Th>Métrica</Table.Th>
                    <Table.Th ta="right">Quantidade</Table.Th>
                    <Table.Th>Ocorrido em</Table.Th>
                    <Table.Th>Recebido em</Table.Th>
                    <Table.Th>Situação</Table.Th>
                    <Table.Th>Faturamento</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {page.data.map((e) => (
                    <Table.Tr key={e.id}>
                      <Table.Td>
                        <Code>{e.external_id}</Code>
                      </Table.Td>
                      <Table.Td>{metricName.get(e.metric_id) ?? '—'}</Table.Td>
                      <Table.Td ta="right" className="numeric">
                        {formatQuantity(e.quantity)}
                      </Table.Td>
                      <Table.Td>
                        <DateTime value={e.occurred_at} />
                      </Table.Td>
                      <Table.Td>
                        <DateTime value={e.received_at} />
                      </Table.Td>
                      <Table.Td>
                        <StatusBadge status={e.status} labels={usageEventStatusLabels} />
                      </Table.Td>
                      <Table.Td>{e.invoice_item_id ? <Badge variant="light" color="green">Faturado</Badge> : <Text size="sm" c="dimmed">Período aberto</Text>}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
            <CursorPager
              page={pagination.page}
              hasPrevious={pagination.hasPrevious}
              hasNext={!!page.next_cursor}
              onPrevious={pagination.previous}
              onNext={() => page.next_cursor && pagination.next(page.next_cursor)}
              loading={query.isFetching}
            />
          </>
        )
      }
    </QueryContent>
  );
}

export function UsagePanel({ subscription }: { subscription: Subscription }) {
  const { keys } = useTenant();
  const query = useQuery({
    queryKey: keys('usage', 'subscription', subscription.id),
    queryFn: ({ signal }) => subscriptionsApi.usage(subscription.id, signal),
  });
  return (
    <Stack>
      <QueryContent query={query} loading={<LoadingState />}>
        {(usage) => (
          <Paper withBorder p="md" radius="md">
            <Stack gap="sm">
              <div>
                <Title order={3} size="h5">
                  Consumo do período corrente
                </Title>
                <Text size="sm" c="dimmed">
                  <DateTime value={usage.period_start} /> até <DateTime value={usage.period_end} />. O valor estimado
                  pode mudar até o fechamento.
                </Text>
              </div>
              {usage.metrics.length === 0 ? (
                <Text size="sm" c="dimmed">
                  O plano desta assinatura não cobra por consumo.
                </Text>
              ) : (
                <Table.ScrollContainer minWidth={680}>
                  <Table aria-label="Consumo agregado">
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th>Métrica</Table.Th>
                        <Table.Th>Agregação</Table.Th>
                        <Table.Th ta="right">Quantidade</Table.Th>
                        <Table.Th ta="right">Eventos</Table.Th>
                        <Table.Th ta="right">Valor estimado</Table.Th>
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {usage.metrics.map((m) => (
                        <Table.Tr key={m.metric_id}>
                          <Table.Td>
                            {m.name} <Code>{m.code}</Code>
                          </Table.Td>
                          <Table.Td>
                            <Tooltip label={aggregationHelp[m.aggregation]} multiline w={300}>
                              <Badge variant="light" tabIndex={0}>
                                {labelOf(aggregationLabels, m.aggregation)}
                              </Badge>
                            </Tooltip>
                          </Table.Td>
                          <Table.Td ta="right" className="numeric">
                            {formatQuantity(m.quantity)} {m.unit}
                          </Table.Td>
                          <Table.Td ta="right">{m.event_count}</Table.Td>
                          <Table.Td ta="right">{m.estimated_amount !== undefined ? <Money value={m.estimated_amount} /> : '—'}</Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                </Table.ScrollContainer>
              )}
            </Stack>
          </Paper>
        )}
      </QueryContent>
      <Title order={3} size="h5">
        Eventos recebidos
      </Title>
      <UsageEvents subscription={subscription} />
    </Stack>
  );
}
