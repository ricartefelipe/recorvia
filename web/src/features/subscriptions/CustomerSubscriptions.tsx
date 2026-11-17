import { Button, Group, Stack } from '@mantine/core';
import { IconPlus } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import type { CustomerDetails } from '../../api/types';
import { EmptyState, QueryContent } from '../../components/states';
import { Can, useTenant } from '../../session/SessionContext';
import { subscriptionsApi } from './api';
import { SubscriptionCreateModal } from './SubscriptionCreateModal';
import { SubscriptionsTable } from './SubscriptionsPage';

/** Assinaturas de um cliente (aba do detalhe do cliente). */
export function CustomerSubscriptions({ customer }: { customer: CustomerDetails }) {
  const { keys } = useTenant();
  const [creating, setCreating] = useState(false);
  const query = useQuery({
    queryKey: keys('subscriptions', 'list', { customer_id: customer.id, limit: 100 }),
    queryFn: ({ signal }) => subscriptionsApi.list({ customer_id: customer.id, limit: 100 }, signal),
  });
  return (
    <Stack>
      <Can permission="subscriptions.write">
        {customer.status === 'active' && (
          <Group justify="flex-end">
            <Button leftSection={<IconPlus size={16} />} onClick={() => setCreating(true)}>
              Nova assinatura
            </Button>
          </Group>
        )}
      </Can>
      <QueryContent query={query}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhuma assinatura" description="Este cliente ainda não tem assinaturas." />
          ) : (
            <SubscriptionsTable subscriptions={page.data} showCustomer={false} />
          )
        }
      </QueryContent>
      <SubscriptionCreateModal opened={creating} onClose={() => setCreating(false)} customerId={customer.id} />
    </Stack>
  );
}
