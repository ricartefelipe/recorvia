import { Stack, Tabs } from '@mantine/core';
import { PageHeader } from '../../components/ui';
import { useUrlFilters } from '../../lib/hooks';
import { CredentialsTab } from './CredentialsTab';
import { ProvidersTab } from './ProvidersTab';
import { WebhooksTab } from './WebhooksTab';

export function IntegrationsPage() {
  const [filters, setFilters] = useUrlFilters(['aba'] as const);
  const tab = ['credenciais', 'webhooks'].includes(filters.aba) ? filters.aba : 'provedores';
  return (
    <Stack gap={0}>
      <PageHeader
        title="Integrações e webhooks"
        description="Provedores de pagamento, credenciais de API para os seus sistemas e webhooks enviados pelo Recorvia."
      />
      <Tabs value={tab} onChange={(v) => setFilters({ aba: v === 'provedores' ? null : v })} keepMounted={false}>
        <Tabs.List mb="md">
          <Tabs.Tab value="provedores">Provedores de pagamento</Tabs.Tab>
          <Tabs.Tab value="credenciais">Credenciais de API</Tabs.Tab>
          <Tabs.Tab value="webhooks">Webhooks enviados</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="provedores">
          <ProvidersTab />
        </Tabs.Panel>
        <Tabs.Panel value="credenciais">
          <CredentialsTab />
        </Tabs.Panel>
        <Tabs.Panel value="webhooks">
          <WebhooksTab />
        </Tabs.Panel>
      </Tabs>
    </Stack>
  );
}
