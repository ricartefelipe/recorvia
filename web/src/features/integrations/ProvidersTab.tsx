import { zodResolver } from '@hookform/resolvers/zod';
import { Alert, Badge, Button, Card, Checkbox, Code, Group, Modal, PasswordInput, Select, SimpleGrid, Stack, Table, Text } from '@mantine/core';
import { IconKey, IconPlugConnected, IconShieldLock } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { z } from 'zod';
import type { ProviderAccountConfigured, ProviderAccountInput } from '../../api/types';
import { DateTime } from '../../components/format';
import { EmptyState, QueryContent } from '../../components/states';
import { SecretRevealModal } from '../../components/ui';
import { useApiMutation } from '../../lib/hooks';
import { labelOf, optionsOf, providerEnvironmentLabels, providerLabels } from '../../lib/labels';
import { Can, useTenant } from '../../session/SessionContext';
import { generateSecret, integrationsApi, type ProviderCode } from './api';

const providerSchema = z.object({
  environment: z.enum(['local', 'sandbox', 'production']),
  api_key: z.string().min(8, 'A chave de API deve ter pelo menos 8 caracteres.').max(512, 'Use no máximo 512 caracteres.'),
  webhook_secret: z.string().min(32, 'O segredo deve ter pelo menos 32 caracteres.').max(255, 'Use no máximo 255 caracteres.'),
  make_default: z.boolean(),
});
type ProviderForm = z.input<typeof providerSchema>;

function ConfigureProviderModal({
  provider,
  version,
  onClose,
  onConfigured,
}: {
  provider: ProviderCode;
  /** Versão da conta do provedor (If-Match); 0 se ainda não configurado. */
  version: number;
  onClose: () => void;
  onConfigured: (result: ProviderAccountConfigured) => void;
}) {
  const form = useForm<ProviderForm, unknown, ProviderAccountInput>({
    resolver: zodResolver(providerSchema),
    defaultValues: { environment: provider === 'simulator' ? 'local' : 'sandbox', api_key: '', webhook_secret: '', make_default: true },
  });
  const mutation = useApiMutation<ProviderAccountInput, ProviderAccountConfigured, ProviderForm>({
    mutationFn: (input) => integrationsApi.configureProvider(provider, version, input),
    invalidate: ['payment-providers'],
    successMessage: `${labelOf(providerLabels, provider)} configurado.`,
    form,
    onSuccess: (result) => {
      onClose();
      onConfigured(result);
    },
  });
  return (
    <Modal opened onClose={onClose} title={`Configurar ${labelOf(providerLabels, provider)}`} size="lg">
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate autoComplete="off">
        <Stack>
          <Alert variant="light" icon={<IconShieldLock />}>
            A chave de API e o segredo do webhook são cifrados e nunca voltam pela API. Depois de salvar, apenas os
            últimos 4 caracteres da chave ficam visíveis. Pagamentos acontecem no checkout hospedado do provedor; o Recorvia
            não recebe dados de cartão.
          </Alert>
          <Controller
            control={form.control}
            name="environment"
            render={({ field }) => (
              <Select label="Ambiente" required data={optionsOf(providerEnvironmentLabels)} value={field.value} onChange={(v) => v && field.onChange(v)} allowDeselect={false} />
            )}
          />
          <PasswordInput label="Chave de API" required autoComplete="new-password" {...form.register('api_key')} error={form.formState.errors.api_key?.message} />
          <Group align="flex-end" gap="xs" wrap="nowrap">
            <PasswordInput
              label="Segredo do webhook"
              description="Usado para autenticar as notificações do provedor. Mínimo de 32 caracteres."
              required
              autoComplete="new-password"
              style={{ flex: 1 }}
              {...form.register('webhook_secret')}
              error={form.formState.errors.webhook_secret?.message}
            />
            <Button variant="default" onClick={() => form.setValue('webhook_secret', generateSecret(), { shouldValidate: true })} mb={form.formState.errors.webhook_secret ? 22 : 0}>
              Gerar segredo
            </Button>
          </Group>
          <Controller
            control={form.control}
            name="make_default"
            render={({ field }) => (
              <Checkbox label="Usar como provedor padrão para novas cobranças" checked={field.value} onChange={(e) => field.onChange(e.currentTarget.checked)} />
            )}
          />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Salvar configuração
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

export function ProvidersTab() {
  const { keys } = useTenant();
  const [configuring, setConfiguring] = useState<{ provider: ProviderCode; version: number } | null>(null);
  const [configured, setConfigured] = useState<ProviderAccountConfigured | null>(null);
  const query = useQuery({ queryKey: keys('payment-providers', 'list'), queryFn: ({ signal }) => integrationsApi.providers(signal) });
  return (
    <QueryContent query={query}>
      {({ data, available }) => (
        <Stack>
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            {available.map((p) => {
              const accounts = data.filter((a) => a.provider === p);
              return (
                <Card key={p} withBorder radius="md">
                  <Group justify="space-between" mb="xs">
                    <Group gap="xs">
                      <IconPlugConnected size={20} aria-hidden />
                      <Text fw={600}>{labelOf(providerLabels, p)}</Text>
                    </Group>
                    {accounts.length > 0 ? <Badge color="green">Configurado</Badge> : <Badge color="gray">Não configurado</Badge>}
                  </Group>
                  <Text size="sm" c="dimmed" mb="sm">
                    {p === 'simulator'
                      ? 'Simulador local determinístico: confirma, falha, atrasa e gera divergências para testes.'
                      : 'Integração com o Asaas por cobranças avulsas e checkout hospedado.'}
                  </Text>
                  <Can permission="integrations.write">
                    {(p === 'simulator' || p === 'asaas') && (
                      <Button
                        size="xs"
                        variant="light"
                        leftSection={<IconKey size={14} />}
                        onClick={() => setConfiguring({ provider: p, version: accounts[0]?.version ?? 0 })}
                      >
                        {accounts.length > 0 ? 'Reconfigurar credenciais' : 'Configurar'}
                      </Button>
                    )}
                  </Can>
                </Card>
              );
            })}
          </SimpleGrid>
          {data.length === 0 ? (
            <EmptyState title="Nenhum provedor configurado" description="Configure um provedor para gerar cobranças com checkout hospedado." />
          ) : (
            <Table.ScrollContainer minWidth={700}>
              <Table aria-label="Contas de provedor">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Provedor</Table.Th>
                    <Table.Th>Ambiente</Table.Th>
                    <Table.Th>Situação</Table.Th>
                    <Table.Th>Chave</Table.Th>
                    <Table.Th>Padrão</Table.Th>
                    <Table.Th>Configurado em</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {data.map((a) => (
                    <Table.Tr key={a.id}>
                      <Table.Td>{labelOf(providerLabels, a.provider)}</Table.Td>
                      <Table.Td>{labelOf(providerEnvironmentLabels, a.environment)}</Table.Td>
                      <Table.Td>{a.status}</Table.Td>
                      <Table.Td>{a.api_key_hint ? <Code>••••{a.api_key_hint}</Code> : '—'}</Table.Td>
                      <Table.Td>{a.is_default ? <Badge size="sm">Padrão</Badge> : '—'}</Table.Td>
                      <Table.Td>
                        <DateTime value={a.created_at} />
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )}
          {configuring && (
            <ConfigureProviderModal
              provider={configuring.provider}
              version={configuring.version}
              onClose={() => setConfiguring(null)}
              onConfigured={setConfigured}
            />
          )}
          <SecretRevealModal
            opened={!!configured}
            title="Endpoint de webhook do provedor"
            description="Cadastre esta URL no painel do provedor para receber as notificações de pagamento. Ela contém um identificador secreto."
            value={configured?.webhook_endpoint_url ?? null}
            valueLabel="URL do endpoint"
            onClose={() => setConfigured(null)}
          />
        </Stack>
      )}
    </QueryContent>
  );
}
