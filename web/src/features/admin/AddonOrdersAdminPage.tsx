import { zodResolver } from '@hookform/resolvers/zod';
import { Alert, Anchor, Badge, Button, Container, Group, Modal, Stack, Table, Text, Textarea, TextInput } from '@mantine/core';
import { IconArrowLeft, IconInfoCircle } from '@tabler/icons-react';
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useForm, type FieldValues } from 'react-hook-form';
import { Link } from 'react-router';
import { z } from 'zod';
import { api } from '../../api/client';
import type { AddonOrder, CursorPage, StaffAddonOrder } from '../../api/types';
import { UserMenu } from '../../app/AppLayout';
import { Money, StatusBadge } from '../../components/format';
import { AccessDenied, EmptyState, ErrorState, LoadingState } from '../../components/states';
import { confirmAction, LabeledSegmented, PageHeader } from '../../components/ui';
import { formatDateTime, zonedWallTimeToUtc } from '../../lib/dates';
import { handleMutationError, type FormErrorTarget } from '../../lib/errors';
import { addonOrderStatusLabels } from '../../lib/labels';
import { formatMoney } from '../../lib/money';
import { notifySuccess } from '../../lib/notify';
import { reasonField } from '../../lib/validation';
import { useSession } from '../../session/SessionContext';

/** Filtros da fila: sem situação, a API devolve os pedidos em aberto. */
const FILTERS = [
  { value: 'open', label: 'Em aberto' },
  { value: 'scheduled', label: 'Agendados' },
  { value: 'completed', label: 'Concluídos' },
  { value: 'canceled', label: 'Cancelados' },
];

const ADMIN_KEY = ['platform-admin', 'addon-orders'] as const;

const orderPath = (o: StaffAddonOrder, action: 'schedule' | 'complete' | 'cancel') =>
  `/api/v1/admin/tenants/${encodeURIComponent(o.tenant_id)}/addon-orders/${encodeURIComponent(o.id)}/${action}`;

/** Chamadas da equipe da plataforma: sem organização ativa (sem X-Recorvia-Tenant). */
export const adminApi = {
  orders: (status: string, cursor?: string, signal?: AbortSignal) => {
    const params = new URLSearchParams();
    if (status) params.set('status', status);
    if (cursor) params.set('cursor', cursor);
    const qs = params.toString();
    return api.get<CursorPage<StaffAddonOrder>>(`/api/v1/admin/addon-orders${qs ? `?${qs}` : ''}`, { signal, tenant: false });
  },
  schedule: (o: StaffAddonOrder, scheduledFor: string) =>
    api.post<AddonOrder>(orderPath(o, 'schedule'), { scheduled_for: scheduledFor }, { tenant: false }),
  complete: (o: StaffAddonOrder) => api.post<AddonOrder>(orderPath(o, 'complete'), undefined, { tenant: false }),
  cancel: (o: StaffAddonOrder, reason: string) => api.post<AddonOrder>(orderPath(o, 'cancel'), { reason }, { tenant: false }),
};

/** Fuso do navegador de quem atende (a equipe não tem organização ativa). */
function staffTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'America/Sao_Paulo';
  } catch {
    return 'America/Sao_Paulo';
  }
}

function When({ value, timeZone }: { value: string | null | undefined; timeZone: string }) {
  if (!value) return <>—</>;
  return <time dateTime={value}>{formatDateTime(value, timeZone)}</time>;
}

function useAdminMutation<TVars, TForm extends FieldValues = FieldValues>(
  mutationFn: (vars: TVars) => Promise<AddonOrder>,
  successMessage: string,
  options: { onSuccess?: () => void; form?: FormErrorTarget<TForm> } = {},
) {
  const queryClient = useQueryClient();
  return useMutation<AddonOrder, unknown, TVars>({
    mutationFn,
    onSuccess: async () => {
      notifySuccess(successMessage);
      options.onSuccess?.();
      await queryClient.invalidateQueries({ queryKey: ADMIN_KEY });
    },
    onError: (error) => handleMutationError(error, { form: options.form }),
  });
}

const scheduleSchema = z.object({
  scheduled_for: z
    .string()
    .trim()
    .regex(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/, 'Informe a data e a hora do atendimento.'),
});
type ScheduleValues = z.infer<typeof scheduleSchema>;

function ScheduleModal({ order, timeZone, onClose }: { order: StaffAddonOrder; timeZone: string; onClose: () => void }) {
  const form = useForm<ScheduleValues>({ resolver: zodResolver(scheduleSchema), defaultValues: { scheduled_for: '' } });
  const mutation = useAdminMutation<string, ScheduleValues>((at) => adminApi.schedule(order, at), `Atendimento de ${order.tenant_name} agendado.`, {
    onSuccess: onClose,
    form,
  });
  const submit = form.handleSubmit((v) => mutation.mutate(zonedWallTimeToUtc(v.scheduled_for, timeZone)));
  return (
    <Modal opened onClose={onClose} title={`${order.scheduled_for ? 'Reagendar' : 'Agendar'} atendimento: ${order.tenant_name}`}>
      <form onSubmit={submit} noValidate>
        <Stack>
          <Text size="sm">
            {order.addon_name} · {formatMoney(order.price, order.currency)}
          </Text>
          <TextInput
            type="datetime-local"
            label="Data e hora do atendimento"
            description={`Horário de ${timeZone}. Informe um instante futuro, em até um ano.`}
            required
            {...form.register('scheduled_for')}
            error={form.formState.errors.scheduled_for?.message}
          />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Voltar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Salvar agendamento
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

const cancelSchema = z.object({ reason: reasonField });
type CancelValues = z.infer<typeof cancelSchema>;

function CancelModal({ order, onClose }: { order: StaffAddonOrder; onClose: () => void }) {
  const form = useForm<CancelValues>({ resolver: zodResolver(cancelSchema), defaultValues: { reason: '' } });
  const mutation = useAdminMutation<string, CancelValues>((reason) => adminApi.cancel(order, reason), `Pedido de ${order.tenant_name} cancelado.`, {
    onSuccess: onClose,
    form,
  });
  return (
    <Modal opened onClose={onClose} title={`Cancelar pedido: ${order.tenant_name}`}>
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(v.reason))} noValidate>
        <Stack>
          <Text size="sm">O pedido é encerrado sem cobrança. A justificativa fica registrada na auditoria da organização.</Text>
          <Textarea
            label="Justificativa"
            required
            autosize
            minRows={3}
            maxLength={500}
            {...form.register('reason')}
            error={form.formState.errors.reason?.message}
          />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Voltar
            </Button>
            <Button type="submit" color="red" loading={mutation.isPending}>
              Cancelar pedido
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

function OrdersTable({ orders, timeZone }: { orders: StaffAddonOrder[]; timeZone: string }) {
  const [scheduling, setScheduling] = useState<StaffAddonOrder | null>(null);
  const [canceling, setCanceling] = useState<StaffAddonOrder | null>(null);
  const complete = useAdminMutation<StaffAddonOrder>((o) => adminApi.complete(o), 'Atendimento concluído.');
  const onComplete = async (o: StaffAddonOrder) => {
    const ok = await confirmAction({
      title: 'Concluir atendimento',
      message: `Concluir ${o.addon_name} de ${o.tenant_name}? O valor de ${formatMoney(o.price, o.currency)} entra no próximo demonstrativo mensal da organização no Recorvia, uma única vez. A conclusão não pode ser desfeita.`,
      confirmLabel: 'Concluir',
    });
    if (ok) complete.mutate(o);
  };
  return (
    <>
      <Table.ScrollContainer minWidth={1000}>
        <Table aria-label="Pedidos de serviços adicionais" verticalSpacing="sm">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Organização</Table.Th>
              <Table.Th>Serviço</Table.Th>
              <Table.Th ta="right">Valor</Table.Th>
              <Table.Th>Situação</Table.Th>
              <Table.Th>Solicitado em</Table.Th>
              <Table.Th>Agendado para</Table.Th>
              <Table.Th>Concluído em</Table.Th>
              <Table.Th>Observações</Table.Th>
              <Table.Th>Ações</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {orders.map((o) => {
              const open = o.status === 'requested' || o.status === 'scheduled';
              return (
                <Table.Tr key={o.id}>
                  <Table.Td>
                    <Text size="sm" fw={600}>
                      {o.tenant_name}
                    </Text>
                    <Text size="xs" c="dimmed">
                      {o.tenant_slug}
                    </Text>
                  </Table.Td>
                  <Table.Td>{o.addon_name}</Table.Td>
                  <Table.Td ta="right">
                    <Money value={o.price} currency={o.currency} />
                  </Table.Td>
                  <Table.Td>
                    <Group gap={4}>
                      <StatusBadge status={o.status} labels={addonOrderStatusLabels} />
                      {o.status === 'completed' && (
                        <Badge variant="outline" color={o.billed ? 'teal' : 'gray'} size="sm">
                          {o.billed ? 'Cobrado' : 'Aguardando demonstrativo'}
                        </Badge>
                      )}
                    </Group>
                    {o.cancel_reason && (
                      <Text size="xs" c="dimmed">
                        Motivo: {o.cancel_reason}
                      </Text>
                    )}
                  </Table.Td>
                  <Table.Td>
                    <When value={o.created_at} timeZone={timeZone} />
                  </Table.Td>
                  <Table.Td>
                    <When value={o.scheduled_for} timeZone={timeZone} />
                  </Table.Td>
                  <Table.Td>
                    <When value={o.completed_at} timeZone={timeZone} />
                  </Table.Td>
                  <Table.Td maw={260}>
                    <Text size="sm" lineClamp={3} title={o.notes || undefined}>
                      {o.notes || '—'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    {open ? (
                      <Group gap={4} wrap="nowrap">
                        <Button size="xs" variant="light" onClick={() => setScheduling(o)} aria-label={`${o.scheduled_for ? 'Reagendar' : 'Agendar'} ${o.tenant_name}`}>
                          {o.scheduled_for ? 'Reagendar' : 'Agendar'}
                        </Button>
                        <Button
                          size="xs"
                          onClick={() => void onComplete(o)}
                          loading={complete.isPending && complete.variables?.id === o.id}
                          aria-label={`Concluir ${o.tenant_name}`}
                        >
                          Concluir
                        </Button>
                        <Button size="xs" variant="subtle" color="red" onClick={() => setCanceling(o)} aria-label={`Cancelar ${o.tenant_name}`}>
                          Cancelar
                        </Button>
                      </Group>
                    ) : (
                      '—'
                    )}
                  </Table.Td>
                </Table.Tr>
              );
            })}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
      {scheduling && <ScheduleModal order={scheduling} timeZone={timeZone} onClose={() => setScheduling(null)} />}
      {canceling && <CancelModal order={canceling} onClose={() => setCanceling(null)} />}
    </>
  );
}

/**
 * Atendimento dos pedidos de serviço adicional (implantação assistida) pela
 * equipe da plataforma. Fica fora da área da organização: a equipe atende
 * pedidos de todas as organizações sem participar delas.
 */
export function AddonOrdersAdminPage() {
  const { session, tenant } = useSession();
  const [filter, setFilter] = useState('open');
  const timeZone = staffTimeZone();
  const status = filter === 'open' ? '' : filter;
  // Páginas acumuladas: "Carregar mais" busca a seguinte pelo cursor da última.
  const query = useInfiniteQuery({
    queryKey: [...ADMIN_KEY, status],
    queryFn: ({ pageParam, signal }) => adminApi.orders(status, pageParam, signal),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    enabled: session.platform_staff,
  });
  const orders = query.data?.pages.flatMap((page) => page.data) ?? [];
  return (
    <Container size="xl" py="lg" component="main">
      <Group justify="space-between">
        <Anchor component={Link} to={tenant ? '/' : '/organizacoes'} size="sm">
          <Group gap={4}>
            <IconArrowLeft size={14} aria-hidden />
            {tenant ? 'Voltar ao painel' : 'Voltar às organizações'}
          </Group>
        </Anchor>
        <UserMenu />
      </Group>
      <PageHeader
        title="Atendimento de serviços adicionais"
        description="Pedidos de implantação assistida e outros serviços das organizações: agende, conclua ou cancele o atendimento."
      />
      {!session.platform_staff ? (
        <AccessDenied message="Esta área é restrita à equipe da plataforma Recorvia." />
      ) : (
        <Stack>
          <Alert variant="light" icon={<IconInfoCircle />}>
            Pedidos concluídos entram uma única vez no demonstrativo mensal do Recorvia da organização: no do
            período da conclusão ou, se ele já estiver fechado, no seguinte. Cada ação fica registrada na auditoria da organização.
          </Alert>
          <LabeledSegmented label="Situação" data={FILTERS} value={filter} onChange={setFilter} w="fit-content" />
          {query.isPending ? (
            <LoadingState />
          ) : query.isError && orders.length === 0 ? (
            <ErrorState error={query.error} onRetry={() => void query.refetch()} />
          ) : orders.length === 0 ? (
            <EmptyState title="Nenhum pedido" description="Não há pedidos nesta situação." />
          ) : (
            <>
              <OrdersTable orders={orders} timeZone={timeZone} />
              {query.hasNextPage && (
                <Group justify="center">
                  <Button variant="light" onClick={() => void query.fetchNextPage()} loading={query.isFetchingNextPage}>
                    Carregar mais
                  </Button>
                </Group>
              )}
            </>
          )}
        </Stack>
      )}
    </Container>
  );
}
