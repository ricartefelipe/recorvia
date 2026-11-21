import { Anchor, Badge, Button, Group, Select, Stack, Switch, Table, Text } from '@mantine/core';
import { IconPlus } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Link } from 'react-router';
import type { Subscription, SubscriptionStatus } from '../../api/types';
import { DateTime, StatusBadge } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { CursorPager, PageHeader } from '../../components/ui';
import { useCursorPagination, useReturnFromEmptyPage, useUrlFilters } from '../../lib/hooks';
import { stableStringify } from '../../lib/idempotency';
import { optionsOf, subscriptionStatusLabels } from '../../lib/labels';
import { Can, useTenant } from '../../session/SessionContext';
import { CustomerSelect } from '../customers/CustomerSelect';
import { subscriptionsApi, type SubscriptionListParams } from './api';
import { SubscriptionCreateModal } from './SubscriptionCreateModal';

export function SubscriptionsTable({ subscriptions, showCustomer = true }: { subscriptions: Subscription[]; showCustomer?: boolean }) {
  return (
    <Table.ScrollContainer minWidth={880}>
      <Table aria-label="Assinaturas">
        <Table.Thead>
          <Table.Tr>
            {showCustomer && <Table.Th>Cliente</Table.Th>}
            <Table.Th>Plano</Table.Th>
            <Table.Th>Situação</Table.Th>
            <Table.Th ta="right">Qtd.</Table.Th>
            <Table.Th>Período atual</Table.Th>
            <Table.Th>Próxima cobrança</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {subscriptions.map((s) => (
            <Table.Tr key={s.id}>
              {showCustomer && (
                <Table.Td>
                  <Anchor component={Link} to={`/assinaturas/${s.id}`} fw={500}>
                    {s.customer_name || 'Cliente'}
                  </Anchor>
                </Table.Td>
              )}
              <Table.Td>
                {showCustomer ? (
                  s.plan_name || '—'
                ) : (
                  <Anchor component={Link} to={`/assinaturas/${s.id}`} fw={500}>
                    {s.plan_name || 'Assinatura'}
                  </Anchor>
                )}
              </Table.Td>
              <Table.Td>
                <Group gap={4}>
                  <StatusBadge status={s.status} labels={subscriptionStatusLabels} />
                  {s.cancel_at_period_end && s.status !== 'canceled' && (
                    <Badge variant="outline" color="orange" size="sm">
                      Cancela no fim do período
                    </Badge>
                  )}
                </Group>
              </Table.Td>
              <Table.Td ta="right">{s.quantity}</Table.Td>
              <Table.Td>
                <Text size="sm">
                  <DateTime value={s.current_period_start} dateOnly /> – <DateTime value={s.current_period_end} dateOnly />
                </Text>
              </Table.Td>
              <Table.Td>{s.next_billing_at ? <DateTime value={s.next_billing_at} dateOnly /> : '—'}</Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
  );
}

export function SubscriptionsPage() {
  const { keys } = useTenant();
  const [filters, setFilters] = useUrlFilters(['status', 'cliente', 'in_force'] as const);
  const [creating, setCreating] = useState(false);
  const params: SubscriptionListParams = {
    status: (filters.status || undefined) as SubscriptionStatus | undefined,
    in_force: filters.in_force === 'true' ? true : undefined,
    customer_id: filters.cliente || undefined,
    limit: 25,
  };
  const pagination = useCursorPagination(stableStringify(params));
  const query = useQuery({
    queryKey: keys('subscriptions', 'list', { ...params, cursor: pagination.cursor }),
    queryFn: ({ signal }) => subscriptionsApi.list({ ...params, cursor: pagination.cursor }, signal),
    placeholderData: keepPreviousData,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);
  const filtered = !!params.status || !!params.customer_id || !!params.in_force;

  return (
    <Stack gap={0}>
      <PageHeader
        title="Assinaturas"
        description="Contratos recorrentes dos seus clientes, com ciclo, trial, mudanças de plano e cancelamentos."
        actions={
          <Can permission="subscriptions.write">
            <Button leftSection={<IconPlus size={16} />} onClick={() => setCreating(true)}>
              Nova assinatura
            </Button>
          </Can>
        }
      />
      <Group mb="md" align="flex-end">
        <Select
          label="Situação"
          placeholder="Todas"
          clearable
          data={optionsOf(subscriptionStatusLabels)}
          value={filters.status || null}
          onChange={(v) => setFilters({ status: v })}
          w={200}
        />
        <CustomerSelect label="Cliente" includeArchived value={filters.cliente || null} onChange={(v) => setFilters({ cliente: v })} w={320} />
        <Switch
          label="Somente em vigor (trial, ativas e em atraso)"
          checked={filters.in_force === 'true'}
          onChange={(e) => setFilters({ in_force: e.currentTarget.checked ? 'true' : null })}
          mb={8}
        />
      </Group>
      <QueryContent query={query} loading={<LoadingState />}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState
              title={filtered ? 'Nenhuma assinatura encontrada' : 'Nenhuma assinatura'}
              description={filtered ? 'Revise os filtros aplicados.' : 'Crie uma assinatura escolhendo um cliente e um plano do catálogo.'}
            />
          ) : (
            <>
              <SubscriptionsTable subscriptions={page.data} />
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
      <SubscriptionCreateModal opened={creating} onClose={() => setCreating(false)} customerId={filters.cliente || undefined} />
    </Stack>
  );
}
