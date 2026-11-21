import { zodResolver } from '@hookform/resolvers/zod';
import {
  ActionIcon,
  Anchor,
  Badge,
  Breadcrumbs,
  Button,
  Group,
  Modal,
  MultiSelect,
  NumberInput,
  Select,
  Stack,
  Table,
  Text,
  TextInput,
  Tooltip,
} from '@mantine/core';
import { IconListDetails, IconPencil, IconPlus, IconRefresh, IconRotate, IconTrash, IconWebhook } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { Link, useParams } from 'react-router';
import { z } from 'zod';
import type { WebhookDelivery, WebhookEndpoint, WebhookEndpointInput, WebhookEndpointWithSecret } from '../../api/types';
import { DateTime, StatusBadge } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { confirmAction, CursorPager, PageHeader, SecretRevealModal } from '../../components/ui';
import { useApiMutation, useCursorPagination } from '../../lib/hooks';
import { deliveryStatusLabels, webhookStatusLabels } from '../../lib/labels';
import { optionalText } from '../../lib/validation';
import { Can, usePermission, useTenant } from '../../session/SessionContext';
import { integrationsApi } from './api';

const endpointSchema = z.object({
  url: z
    .string()
    .trim()
    .max(2000, 'Use no máximo 2000 caracteres.')
    .refine((v) => {
      try {
        const u = new URL(v);
        return u.protocol === 'https:' || u.protocol === 'http:';
      } catch {
        return false;
      }
    }, 'Informe uma URL completa, por exemplo https://erp.exemplo.com/webhooks/recorvia.'),
  description: optionalText(200),
  event_types: z.array(z.string()).min(1, 'Escolha pelo menos um tipo de evento.'),
  status: z.enum(['active', 'disabled']),
});
type EndpointForm = z.input<typeof endpointSchema>;
type EndpointOutput = z.output<typeof endpointSchema>;

function EndpointModal({
  endpoint,
  onClose,
  onCreated,
}: {
  endpoint: WebhookEndpoint | null;
  onClose: () => void;
  onCreated: (r: WebhookEndpointWithSecret) => void;
}) {
  const { keys } = useTenant();
  const eventTypes = useQuery({ queryKey: keys('webhooks', 'event-types'), queryFn: ({ signal }) => integrationsApi.eventTypes(signal) });
  const form = useForm<EndpointForm, unknown, EndpointOutput>({
    resolver: zodResolver(endpointSchema),
    defaultValues: {
      url: endpoint?.url ?? '',
      description: endpoint?.description ?? '',
      event_types: endpoint?.event_types ?? [],
      status: endpoint?.status ?? 'active',
    },
  });
  const mutation = useApiMutation<EndpointOutput, WebhookEndpoint | WebhookEndpointWithSecret, EndpointForm>({
    mutationFn: ({ status, ...input }) => {
      const body: WebhookEndpointInput = { ...input, description: input.description ?? '' };
      return endpoint ? integrationsApi.updateEndpoint(endpoint, { ...body, status }) : integrationsApi.createEndpoint(body);
    },
    invalidate: ['webhooks'],
    successMessage: endpoint ? 'Endpoint atualizado.' : 'Endpoint criado.',
    form,
    onSuccess: (result) => {
      onClose();
      if (!endpoint && 'secret' in result) onCreated(result);
    },
  });
  return (
    <Modal opened onClose={onClose} title={endpoint ? 'Editar endpoint' : 'Novo endpoint de webhook'} size="lg">
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
        <Stack>
          <TextInput label="URL" required placeholder="https://" {...form.register('url')} error={form.formState.errors.url?.message} />
          <TextInput label="Descrição" {...form.register('description')} error={form.formState.errors.description?.message} />
          <Controller
            control={form.control}
            name="event_types"
            render={({ field, fieldState }) => (
              <MultiSelect
                label="Tipos de evento"
                required
                searchable
                data={eventTypes.data?.data ?? []}
                value={field.value}
                onChange={field.onChange}
                error={fieldState.error?.message}
                placeholder={eventTypes.isPending ? 'Carregando…' : 'Selecione'}
              />
            )}
          />
          {endpoint && (
            <Controller
              control={form.control}
              name="status"
              render={({ field }) => (
                <Select
                  label="Situação"
                  data={[
                    { value: 'active', label: 'Ativo' },
                    { value: 'disabled', label: 'Desativado' },
                  ]}
                  value={field.value}
                  onChange={(v) => v && field.onChange(v)}
                  allowDeselect={false}
                />
              )}
            />
          )}
          <Text size="xs" c="dimmed">
            Cada entrega é assinada com HMAC e carimbo de tempo. Endereços internos ou privados são recusados.
          </Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Salvar
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

function RotateSecretModal({ endpoint, onClose, onRotated }: { endpoint: WebhookEndpoint; onClose: () => void; onRotated: (r: WebhookEndpointWithSecret) => void }) {
  const [hours, setHours] = useState<number | string>(24);
  const valid = typeof hours === 'number' && Number.isInteger(hours) && hours >= 0 && hours <= 168;
  const mutation = useApiMutation<number, WebhookEndpointWithSecret>({
    mutationFn: (h) => integrationsApi.rotateSecret(endpoint, h),
    invalidate: ['webhooks'],
    successMessage: 'Segredo rotacionado.',
    onSuccess: (r) => {
      onClose();
      onRotated(r);
    },
  });
  return (
    <Modal opened onClose={onClose} title="Rotacionar segredo">
      <Stack>
        <Text size="sm">Um novo segredo será gerado e exibido uma única vez. Durante a transição, as entregas são assinadas com os dois.</Text>
        <NumberInput
          label="Manter o segredo anterior válido por (horas)"
          min={0}
          max={168}
          allowDecimal={false}
          value={hours}
          onChange={setHours}
          error={valid ? undefined : 'Informe de 0 a 168 horas.'}
        />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            Cancelar
          </Button>
          <Button onClick={() => valid && mutation.mutate(hours as number)} loading={mutation.isPending} disabled={!valid}>
            Rotacionar
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}

export function WebhooksTab() {
  const { keys } = useTenant();
  const canWrite = usePermission('webhooks.write');
  const [editing, setEditing] = useState<WebhookEndpoint | null | undefined>(undefined);
  const [rotating, setRotating] = useState<WebhookEndpoint | null>(null);
  const [secret, setSecret] = useState<WebhookEndpointWithSecret | null>(null);
  const query = useQuery({ queryKey: keys('webhooks', 'endpoints'), queryFn: ({ signal }) => integrationsApi.endpoints(signal) });
  const remove = useApiMutation<WebhookEndpoint, unknown>({
    mutationFn: (e) => integrationsApi.deleteEndpoint(e),
    invalidate: ['webhooks'],
    successMessage: 'Endpoint removido.',
  });
  return (
    <Stack>
      <Group justify="space-between">
        <Text size="sm" c="dimmed" maw={640}>
          Webhooks enviados pelo Recorvia para os seus sistemas, com identificador estável por evento e novas tentativas
          automáticas.
        </Text>
        <Can permission="webhooks.write">
          <Button leftSection={<IconPlus size={16} />} onClick={() => setEditing(null)}>
            Novo endpoint
          </Button>
        </Can>
      </Group>
      <QueryContent query={query}>
        {({ data }) =>
          data.length === 0 ? (
            <EmptyState title="Nenhum endpoint" description="Cadastre uma URL para receber eventos como fatura emitida e pagamento confirmado." icon={<IconWebhook size={26} />} />
          ) : (
            <Table.ScrollContainer minWidth={900}>
              <Table aria-label="Endpoints de webhook">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>URL</Table.Th>
                    <Table.Th>Eventos</Table.Th>
                    <Table.Th>Situação</Table.Th>
                    <Table.Th>Segredos</Table.Th>
                    <Table.Th>Criado em</Table.Th>
                    <Table.Th aria-label="Ações" />
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {data.map((e) => (
                    <Table.Tr key={e.id}>
                      <Table.Td>
                        <Text size="sm" ff="monospace" style={{ wordBreak: 'break-all' }}>
                          {e.url}
                        </Text>
                        {e.description && (
                          <Text size="xs" c="dimmed">
                            {e.description}
                          </Text>
                        )}
                      </Table.Td>
                      <Table.Td>
                        <Tooltip label={e.event_types.join(', ')} multiline w={320}>
                          <Badge variant="light" tabIndex={0}>
                            {e.event_types.length} tipo(s)
                          </Badge>
                        </Tooltip>
                      </Table.Td>
                      <Table.Td>
                        <StatusBadge status={e.status} labels={webhookStatusLabels} />
                      </Table.Td>
                      <Table.Td>
                        <Text size="sm">{e.secrets.length} ativo(s)</Text>
                        {e.secrets.some((s) => s.expires_at) && (
                          <Text size="xs" c="dimmed">
                            Anterior expira em <DateTime value={e.secrets.find((s) => s.expires_at)?.expires_at} />
                          </Text>
                        )}
                      </Table.Td>
                      <Table.Td>
                        <DateTime value={e.created_at} dateOnly />
                      </Table.Td>
                      <Table.Td>
                        <Group gap={4} wrap="nowrap">
                          <Tooltip label="Entregas">
                            <ActionIcon component={Link} to={`/integracoes/webhooks/${e.id}`} variant="subtle" aria-label={`Ver entregas de ${e.url}`}>
                              <IconListDetails size={16} />
                            </ActionIcon>
                          </Tooltip>
                          {canWrite && (
                            <>
                              <Tooltip label="Editar">
                                <ActionIcon variant="subtle" aria-label={`Editar endpoint ${e.url}`} onClick={() => setEditing(e)}>
                                  <IconPencil size={16} />
                                </ActionIcon>
                              </Tooltip>
                              <Tooltip label="Rotacionar segredo">
                                <ActionIcon variant="subtle" aria-label={`Rotacionar segredo de ${e.url}`} onClick={() => setRotating(e)}>
                                  <IconRotate size={16} />
                                </ActionIcon>
                              </Tooltip>
                              <Tooltip label="Remover">
                                <ActionIcon
                                  variant="subtle"
                                  color="red"
                                  aria-label={`Remover endpoint ${e.url}`}
                                  onClick={async () => {
                                    const ok = await confirmAction({
                                      title: 'Remover endpoint',
                                      message: `Desativar e remover ${e.url}? Entregas pendentes não serão mais enviadas.`,
                                      confirmLabel: 'Remover',
                                      danger: true,
                                    });
                                    if (ok) remove.mutate(e);
                                  }}
                                >
                                  <IconTrash size={16} />
                                </ActionIcon>
                              </Tooltip>
                            </>
                          )}
                        </Group>
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )
        }
      </QueryContent>
      {editing !== undefined && <EndpointModal endpoint={editing} onClose={() => setEditing(undefined)} onCreated={setSecret} />}
      {rotating && <RotateSecretModal endpoint={rotating} onClose={() => setRotating(null)} onRotated={setSecret} />}
      <SecretRevealModal
        opened={!!secret}
        title="Segredo de assinatura do webhook"
        description={secret ? `Use este segredo para validar a assinatura HMAC das entregas para ${secret.endpoint.url}.` : undefined}
        value={secret?.secret ?? null}
        valueLabel="Segredo"
        onClose={() => setSecret(null)}
      />
    </Stack>
  );
}

/** Tentativas individuais de uma entrega, da primeira à mais recente. */
function DeliveryAttemptsModal({ delivery, onClose }: { delivery: WebhookDelivery; onClose: () => void }) {
  const { keys } = useTenant();
  const query = useQuery({
    queryKey: keys('webhooks', 'attempts', delivery.id),
    queryFn: ({ signal }) => integrationsApi.deliveryAttempts(delivery.id, signal),
  });
  return (
    <Modal opened onClose={onClose} title="Tentativas da entrega" size="xl">
      <Stack gap="sm">
        <Text size="sm" ff="monospace">
          {delivery.event_type} · {delivery.event_id}
        </Text>
        <QueryContent query={query} loading={<LoadingState />}>
          {(list) =>
            list.data.length === 0 ? (
              <EmptyState title="Nenhuma tentativa" description="A entrega ainda não foi tentada." />
            ) : (
              <Table aria-label="Tentativas da entrega">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th ta="right">Nº</Table.Th>
                    <Table.Th>Início</Table.Th>
                    <Table.Th ta="right">Duração</Table.Th>
                    <Table.Th>Código HTTP</Table.Th>
                    <Table.Th>Erro</Table.Th>
                    <Table.Th ta="right">Assinaturas</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {list.data.map((a) => (
                    <Table.Tr key={a.attempt}>
                      <Table.Td ta="right">{a.attempt}</Table.Td>
                      <Table.Td>
                        <DateTime value={a.started_at} />
                      </Table.Td>
                      <Table.Td ta="right">{a.duration_ms} ms</Table.Td>
                      <Table.Td>{a.status_code ?? 'Sem resposta'}</Table.Td>
                      <Table.Td>
                        {a.error ? (
                          <Text size="xs" c="red" lineClamp={3} title={a.error}>
                            {a.error}
                          </Text>
                        ) : (
                          '—'
                        )}
                      </Table.Td>
                      <Table.Td ta="right">{a.signature_count}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            )
          }
        </QueryContent>
      </Stack>
    </Modal>
  );
}

export function WebhookDeliveriesPage() {
  const { id = '' } = useParams();
  const [attempts, setAttempts] = useState<WebhookDelivery | null>(null);
  const { keys } = useTenant();
  const canWrite = usePermission('webhooks.write');
  const pagination = useCursorPagination(id);
  const endpoints = useQuery({ queryKey: keys('webhooks', 'endpoints'), queryFn: ({ signal }) => integrationsApi.endpoints(signal) });
  const endpoint = endpoints.data?.data.find((e) => e.id === id);
  const query = useQuery({
    queryKey: keys('webhooks', 'deliveries', id, pagination.cursor),
    queryFn: ({ signal }) => integrationsApi.deliveries(id, { cursor: pagination.cursor, limit: 25 }, signal),
    placeholderData: keepPreviousData,
  });
  const retry = useApiMutation<WebhookDelivery, WebhookDelivery>({
    mutationFn: (d) => integrationsApi.retryDelivery(d.id),
    invalidate: ['webhooks'],
    successMessage: 'Entrega reagendada.',
  });
  return (
    <Stack gap="md">
      <Breadcrumbs>
        <Anchor component={Link} to="/integracoes?aba=webhooks">
          Webhooks
        </Anchor>
        <Text>Entregas</Text>
      </Breadcrumbs>
      <PageHeader title="Entregas do webhook" description={endpoint?.url} />
      <QueryContent query={query} loading={<LoadingState />}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhuma entrega" description="Entregas aparecem quando eventos assinados por este endpoint acontecem." />
          ) : (
            <>
              <Table.ScrollContainer minWidth={900}>
                <Table aria-label="Entregas">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Criada em</Table.Th>
                      <Table.Th>Evento</Table.Th>
                      <Table.Th>Situação</Table.Th>
                      <Table.Th ta="right">Tentativas</Table.Th>
                      <Table.Th>Último código HTTP</Table.Th>
                      <Table.Th>Próxima tentativa</Table.Th>
                      <Table.Th>Entregue em</Table.Th>
                      <Table.Th aria-label="Ações" />
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.data.map((d) => (
                      <Table.Tr key={d.id}>
                        <Table.Td>
                          <DateTime value={d.created_at} />
                        </Table.Td>
                        <Table.Td>
                          <Text size="sm" ff="monospace">
                            {d.event_type}
                          </Text>
                          <Text size="xs" c="dimmed" ff="monospace">
                            {d.event_id}
                          </Text>
                        </Table.Td>
                        <Table.Td>
                          <StatusBadge status={d.status} labels={deliveryStatusLabels} />
                          {d.last_error && (
                            <Text size="xs" c="red" lineClamp={2} title={d.last_error}>
                              {d.last_error}
                            </Text>
                          )}
                        </Table.Td>
                        <Table.Td ta="right">{d.attempts}</Table.Td>
                        <Table.Td>{d.last_status_code ?? '—'}</Table.Td>
                        <Table.Td>{d.next_attempt_at ? <DateTime value={d.next_attempt_at} /> : '—'}</Table.Td>
                        <Table.Td>{d.delivered_at ? <DateTime value={d.delivered_at} /> : '—'}</Table.Td>
                        <Table.Td>
                          <Group gap="xs" wrap="nowrap">
                            {d.attempts > 0 && (
                              <Button size="xs" variant="subtle" leftSection={<IconListDetails size={14} />} onClick={() => setAttempts(d)}>
                                Tentativas
                              </Button>
                            )}
                            {canWrite && (d.status === 'exhausted' || d.status === 'retrying' || d.status === 'succeeded') && (
                              <Button
                                size="xs"
                                variant="light"
                                leftSection={<IconRefresh size={14} />}
                                onClick={() => retry.mutate(d)}
                                loading={retry.isPending && retry.variables?.id === d.id}
                              >
                                Reenviar
                              </Button>
                            )}
                          </Group>
                        </Table.Td>
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
      {attempts && <DeliveryAttemptsModal delivery={attempts} onClose={() => setAttempts(null)} />}
    </Stack>
  );
}
