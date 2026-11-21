import { zodResolver } from '@hookform/resolvers/zod';
import { Alert, Badge, Button, Card, Group, List, Modal, Paper, Progress, SimpleGrid, Stack, Table, Text, Textarea, Title } from '@mantine/core';
import { IconBuildingStore, IconCheck, IconInfoCircle } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useForm } from 'react-hook-form';
import { z } from 'zod';
import { api } from '../../api/client';
import type { AddonOrder, DataList, PlatformAddon, PlatformOverview, PlatformPlan, PlatformStatement } from '../../api/types';
import { DateTime, Money, StatusBadge } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { confirmAction, KeyValue, PageHeader } from '../../components/ui';
import { useApiMutation } from '../../lib/hooks';
import { addonOrderStatusLabels, labelOf, platformSubscriptionStatusLabels } from '../../lib/labels';
import { formatMoney } from '../../lib/money';
import { optionalText } from '../../lib/validation';
import { Can, useTenant } from '../../session/SessionContext';

const platformApi = {
  overview: (signal?: AbortSignal) => api.get<PlatformOverview>('/api/v1/platform/overview', { signal }),
  plans: (signal?: AbortSignal) => api.get<DataList<PlatformPlan>>('/api/v1/platform/plans', { signal }),
  changePlan: (planCode: string) => api.put<PlatformOverview>('/api/v1/platform/plan', { plan_code: planCode }),
  addons: (signal?: AbortSignal) => api.get<DataList<PlatformAddon>>('/api/v1/platform/addons', { signal }),
  orders: (signal?: AbortSignal) => api.get<DataList<AddonOrder>>('/api/v1/platform/addon-orders', { signal }),
  order: (input: { addon_code: string; notes?: string }) => api.post<AddonOrder>('/api/v1/platform/addon-orders', input),
  statements: (signal?: AbortSignal) => api.get<DataList<PlatformStatement>>('/api/v1/platform/statements', { signal }),
};

function UsageBar({ label, used, max, detail }: { label: string; used: number; max: number | null | undefined; detail?: string }) {
  const unlimited = max === null || max === undefined;
  const pct = unlimited ? 0 : Math.min(100, Math.round((used / Math.max(1, max)) * 100));
  return (
    <Stack gap={4}>
      <Group justify="space-between">
        <Text size="sm">{label}</Text>
        <Text size="sm" fw={600}>
          {detail ?? used} {unlimited ? '(sem limite)' : `de ${max}`}
        </Text>
      </Group>
      {!unlimited && <Progress value={pct} color={pct >= 90 ? 'red' : pct >= 75 ? 'orange' : 'blue'} aria-label={`${label}: ${pct}% do limite`} />}
    </Stack>
  );
}

function usersDetail(active: number, pending: number): string {
  const activeText = `${active} ${active === 1 ? 'ativo' : 'ativos'}`;
  if (pending === 0) return activeText;
  return `${activeText} + ${pending} ${pending === 1 ? 'convite pendente' : 'convites pendentes'}`;
}

function OrderProgress({ order }: { order: AddonOrder }) {
  switch (order.status) {
    case 'scheduled':
      return (
        <Text size="xs" c="dimmed">
          Agendado para <DateTime value={order.scheduled_for} />
        </Text>
      );
    case 'completed':
      return (
        <Text size="xs" c="dimmed">
          Concluído em <DateTime value={order.completed_at} />
          {order.billed ? ' · cobrado no demonstrativo' : ' · entra no próximo demonstrativo'}
        </Text>
      );
    case 'canceled':
      return order.cancel_reason ? (
        <Text size="xs" c="dimmed">
          Motivo: {order.cancel_reason}
        </Text>
      ) : null;
    default:
      return null;
  }
}

function Overview({ overview }: { overview: PlatformOverview }) {
  const { plan, subscription, usage } = overview;
  return (
    <SimpleGrid cols={{ base: 1, md: 2 }}>
      <Paper withBorder p="md" radius="md">
        <Stack gap="sm">
          <Group justify="space-between">
            <Title order={2} size="h4">
              Plano {plan.name}
            </Title>
            <StatusBadge status={subscription.status} labels={platformSubscriptionStatusLabels} />
          </Group>
          <Text size="sm" c="dimmed">
            {plan.description}
          </Text>
          <SimpleGrid cols={2}>
            <KeyValue label="Mensalidade">
              <Money value={plan.monthly_price} currency={plan.currency} />
            </KeyValue>
            <KeyValue label="Período atual">
              <DateTime value={subscription.current_period_start} dateOnly /> – <DateTime value={subscription.current_period_end} dateOnly />
            </KeyValue>
            {subscription.trial_ends_at && (
              <KeyValue label="Avaliação até">
                <DateTime value={subscription.trial_ends_at} />
              </KeyValue>
            )}
          </SimpleGrid>
          {plan.features.length > 0 && (
            <List size="sm" spacing={2} icon={<IconCheck size={14} />}>
              {plan.features.map((f) => (
                <List.Item key={f}>{f}</List.Item>
              ))}
            </List>
          )}
        </Stack>
      </Paper>
      <Paper withBorder p="md" radius="md">
        <Stack gap="md">
          <Title order={2} size="h4">
            Limites e uso
          </Title>
          <UsageBar
            label="Usuários (ativos e convites pendentes)"
            used={usage.active_users + usage.pending_invitations}
            max={plan.max_users}
            detail={usersDetail(usage.active_users, usage.pending_invitations)}
          />
          <UsageBar label="Assinaturas ativas dos seus clientes" used={usage.active_subscriptions} max={plan.max_active_subscriptions} />
          <Text size="xs" c="dimmed">
            Ao atingir um limite, novos convites ou assinaturas são recusados até a troca de plano. Convites pendentes
            reservam vagas de usuário e contam na troca para um plano menor.
          </Text>
        </Stack>
      </Paper>
    </SimpleGrid>
  );
}

function PlansSection({ currentCode }: { currentCode: string }) {
  const { keys } = useTenant();
  const query = useQuery({ queryKey: keys('platform', 'plans'), queryFn: ({ signal }) => platformApi.plans(signal) });
  const change = useApiMutation<PlatformPlan, PlatformOverview>({
    mutationFn: (p) => platformApi.changePlan(p.code),
    invalidate: ['platform'],
    successMessage: (o) => `Plano do Recorvia alterado para ${o.plan.name}.`,
  });
  const onChange = async (p: PlatformPlan) => {
    const ok = await confirmAction({
      title: 'Trocar plano do Recorvia',
      message: `Mudar a assinatura da sua organização no Recorvia para o plano ${p.name} (${formatMoney(p.monthly_price, p.currency)} por mês)? Isso não altera nada nas assinaturas dos seus clientes.`,
      confirmLabel: 'Trocar plano',
    });
    if (ok) change.mutate(p);
  };
  return (
    <QueryContent query={query} loading={<LoadingState variant="cards" />}>
      {({ data }) => (
        <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }}>
          {data.map((p) => {
            const current = p.code === currentCode;
            return (
              <Card key={p.code} withBorder radius="md" style={current ? { borderColor: 'var(--mantine-color-blue-5)' } : undefined}>
                <Stack gap="xs" h="100%">
                  <Group justify="space-between">
                    <Text fw={700}>{p.name}</Text>
                    {current && <Badge>Plano atual</Badge>}
                  </Group>
                  <Text fz={22} fw={700}>
                    {formatMoney(p.monthly_price, p.currency)}
                    <Text span size="sm" c="dimmed" fw={400}>
                      {' '}
                      / mês
                    </Text>
                  </Text>
                  <Text size="sm" c="dimmed">
                    {p.description}
                  </Text>
                  <Text size="sm">
                    {p.max_users ?? 'Usuários ilimitados'}
                    {p.max_users ? ' usuários' : ''} · {p.max_active_subscriptions ?? 'Assinaturas ilimitadas'}
                    {p.max_active_subscriptions ? ' assinaturas ativas' : ''}
                  </Text>
                  {p.trial_days > 0 && <Text size="xs" c="dimmed">{p.trial_days} dias de avaliação</Text>}
                  <List size="xs" spacing={2} icon={<IconCheck size={12} />} style={{ flex: 1 }}>
                    {p.features.map((f) => (
                      <List.Item key={f}>{f}</List.Item>
                    ))}
                  </List>
                  <Can permission="platform_billing.manage">
                    {!current && (
                      <Button variant="light" onClick={() => void onChange(p)} loading={change.isPending && change.variables?.code === p.code}>
                        Mudar para este plano
                      </Button>
                    )}
                  </Can>
                </Stack>
              </Card>
            );
          })}
        </SimpleGrid>
      )}
    </QueryContent>
  );
}

const orderSchema = z.object({ notes: optionalText(2000) });

function OrderAddonModal({ addon, onClose }: { addon: PlatformAddon; onClose: () => void }) {
  const form = useForm<z.input<typeof orderSchema>, unknown, z.output<typeof orderSchema>>({ resolver: zodResolver(orderSchema), defaultValues: { notes: '' } });
  const mutation = useApiMutation<z.output<typeof orderSchema>, AddonOrder>({
    mutationFn: (v) => platformApi.order({ addon_code: addon.code, notes: v.notes }),
    invalidate: ['platform'],
    successMessage: 'Pedido registrado. Nossa equipe entrará em contato.',
    onSuccess: onClose,
  });
  return (
    <Modal opened onClose={onClose} title={`Solicitar: ${addon.name}`}>
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
        <Stack>
          <Text size="sm">{addon.description}</Text>
          <Text size="sm">
            Valor: <strong>{formatMoney(addon.price, addon.currency)}</strong>
          </Text>
          <Textarea label="Observações" description="Conte o que você precisa e a melhor forma de contato." autosize minRows={3} maxLength={2000} {...form.register('notes')} error={form.formState.errors.notes?.message} />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Enviar pedido
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

function AddonsSection() {
  const { keys } = useTenant();
  const [ordering, setOrdering] = useState<PlatformAddon | null>(null);
  const addons = useQuery({ queryKey: keys('platform', 'addons'), queryFn: ({ signal }) => platformApi.addons(signal) });
  const orders = useQuery({ queryKey: keys('platform', 'orders'), queryFn: ({ signal }) => platformApi.orders(signal) });
  const addonName = new Map((addons.data?.data ?? []).map((a) => [a.code, a.name]));
  return (
    <Stack>
      <QueryContent query={addons}>
        {({ data }) =>
          data.length === 0 ? (
            <EmptyState title="Nenhum serviço adicional disponível" />
          ) : (
            <SimpleGrid cols={{ base: 1, sm: 2 }}>
              {data.map((a) => (
                <Card key={a.code} withBorder radius="md">
                  <Stack gap="xs">
                    <Group justify="space-between">
                      <Text fw={600}>{a.name}</Text>
                      <Text fw={600}>{formatMoney(a.price, a.currency)}</Text>
                    </Group>
                    <Text size="sm" c="dimmed">
                      {a.description}
                    </Text>
                    <Can permission="platform_billing.manage">
                      <Button variant="light" size="xs" w="fit-content" onClick={() => setOrdering(a)}>
                        Solicitar
                      </Button>
                    </Can>
                  </Stack>
                </Card>
              ))}
            </SimpleGrid>
          )
        }
      </QueryContent>
      <Title order={3} size="h5">
        Pedidos
      </Title>
      <QueryContent query={orders}>
        {({ data }) =>
          data.length === 0 ? (
            <Text size="sm" c="dimmed">
              Nenhum pedido.
            </Text>
          ) : (
            <Table aria-label="Pedidos de serviços adicionais">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Serviço</Table.Th>
                  <Table.Th>Situação</Table.Th>
                  <Table.Th ta="right">Valor</Table.Th>
                  <Table.Th>Observações</Table.Th>
                  <Table.Th>Pedido em</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {data.map((o) => (
                  <Table.Tr key={o.id}>
                    <Table.Td>{addonName.get(o.addon_code) ?? o.addon_code}</Table.Td>
                    <Table.Td>
                      <StatusBadge status={o.status} labels={addonOrderStatusLabels} />
                      <OrderProgress order={o} />
                    </Table.Td>
                    <Table.Td ta="right">
                      <Money value={o.price} currency={o.currency} />
                    </Table.Td>
                    <Table.Td>{o.notes || '—'}</Table.Td>
                    <Table.Td>
                      <DateTime value={o.created_at} />
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )
        }
      </QueryContent>
      {ordering && <OrderAddonModal addon={ordering} onClose={() => setOrdering(null)} />}
    </Stack>
  );
}

function StatementsSection() {
  const { keys } = useTenant();
  const query = useQuery({ queryKey: keys('platform', 'statements'), queryFn: ({ signal }) => platformApi.statements(signal) });
  return (
    <QueryContent query={query}>
      {({ data }) =>
        data.length === 0 ? (
          <EmptyState title="Nenhum demonstrativo" description="Os demonstrativos mensais do Recorvia aparecem após o fechamento de cada período." />
        ) : (
          <Table.ScrollContainer minWidth={700}>
            <Table aria-label="Demonstrativos do Recorvia">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Período</Table.Th>
                  <Table.Th>Plano</Table.Th>
                  <Table.Th ta="right">Assinatura</Table.Th>
                  <Table.Th ta="right">Adicionais</Table.Th>
                  <Table.Th ta="right">Total</Table.Th>
                  <Table.Th>Situação</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {data.map((s) => (
                  <Table.Tr key={s.id}>
                    <Table.Td>
                      <DateTime value={s.period_start} dateOnly /> – <DateTime value={s.period_end} dateOnly />
                    </Table.Td>
                    <Table.Td>{s.plan_code}</Table.Td>
                    <Table.Td ta="right">
                      <Money value={s.subscription_amount} currency={s.currency} />
                    </Table.Td>
                    <Table.Td ta="right">
                      <Money value={s.addons_amount} currency={s.currency} />
                    </Table.Td>
                    <Table.Td ta="right">
                      <Money value={s.total} currency={s.currency} strong />
                    </Table.Td>
                    <Table.Td>{labelOf(platformSubscriptionStatusLabels, s.status)}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )
      }
    </QueryContent>
  );
}

export function PlatformPage() {
  const { keys } = useTenant();
  const overview = useQuery({ queryKey: keys('platform', 'overview'), queryFn: ({ signal }) => platformApi.overview(signal) });
  return (
    <Stack gap="lg">
      <PageHeader
        title="Plano Recorvia"
        description="A assinatura da sua organização junto ao Recorvia: plano, limites, serviços adicionais e demonstrativos."
      />
      <Alert icon={<IconBuildingStore />} color="violet" variant="light" title="Esta é a sua assinatura do Recorvia">
        Aqui você gerencia o que a sua empresa paga ao Recorvia pelo uso da plataforma. Isso não se confunde com as
        assinaturas, faturas e cobranças dos seus clientes, que ficam nas outras seções.
      </Alert>
      <QueryContent query={overview} loading={<LoadingState variant="cards" />}>
        {(o) => (
          <>
            <Overview overview={o} />
            <Stack gap="xs">
              <Title order={2} size="h4">
                Planos do Recorvia
              </Title>
              <PlansSection currentCode={o.subscription.plan_code} />
            </Stack>
          </>
        )}
      </QueryContent>
      <Stack gap="xs">
        <Title order={2} size="h4">
          Implantação assistida e serviços adicionais
        </Title>
        <Group gap={6}>
          <IconInfoCircle size={16} aria-hidden />
          <Text size="sm" c="dimmed">
            Nossa equipe pode configurar planos, provedores e importar seus clientes.
          </Text>
        </Group>
        <AddonsSection />
      </Stack>
      <Stack gap="xs">
        <Title order={2} size="h4">
          Demonstrativos
        </Title>
        <StatementsSection />
      </Stack>
    </Stack>
  );
}
