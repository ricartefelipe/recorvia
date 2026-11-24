import { ActionIcon, Anchor, Breadcrumbs, Code, Collapse, Group, Paper, SimpleGrid, Stack, Table, Tabs, Text, Title } from '@mantine/core';
import { IconChevronDown, IconChevronRight } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { Fragment, useState } from 'react';
import { Link, useParams } from 'react-router';
import type { Subscription } from '../../api/types';
import { CivilDate, DateTime, JsonView, ShortId, StatusBadge } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { KeyValue, PageHeader } from '../../components/ui';
import { useUrlFilters } from '../../lib/hooks';
import {
  actorTypeLabels,
  billingIntervalLabels,
  discountStatusLabels,
  labelOf,
  scheduledChangeKindLabels,
  scheduledChangeStatusLabels,
  subscriptionEventLabels,
  subscriptionStatusLabels,
  systemActorLabels,
} from '../../lib/labels';
import { timeZoneLabel } from '../../lib/dates';
import { useTenant } from '../../session/SessionContext';
import { catalogApi, usePlans } from '../catalog/api';
import { RelatedInvoices } from '../invoices/InvoicesTable';
import { subscriptionsApi } from './api';
import { PriceAdjustmentsPanel } from './PriceAdjustmentsPanel';
import { CancelScheduledAlert, SubscriptionActions } from './SubscriptionActions';
import { UsagePanel } from './UsagePanel';
import { ResponsiveTable } from '../../components/ResponsiveTable';

function HistoryPanel({ subscriptionId }: { subscriptionId: string }) {
  const { keys } = useTenant();
  const [open, setOpen] = useState<string | null>(null);
  const query = useQuery({
    queryKey: keys('subscriptions', 'history', subscriptionId),
    queryFn: ({ signal }) => subscriptionsApi.history(subscriptionId, signal),
  });
  return (
    <QueryContent query={query}>
      {({ data }) =>
        data.length === 0 ? (
          <EmptyState title="Sem transições registradas" />
        ) : (
          <ResponsiveTable minWidth={720}>
            <Table aria-label="Histórico de transições">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th w={40}>
                    <span className="visually-hidden">Detalhes</span>
                  </Table.Th>
                  <Table.Th>Quando</Table.Th>
                  <Table.Th>Evento</Table.Th>
                  <Table.Th>Transição</Table.Th>
                  <Table.Th>Ator</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {data.map((h) => {
                  const expanded = open === h.id;
                  const hasDetails = h.details && Object.keys(h.details).length > 0;
                  return (
                    <Fragment key={h.id}>
                      <Table.Tr>
                        <Table.Td>
                          {hasDetails && (
                            <ActionIcon
                              variant="subtle"
                              size="sm"
                              aria-expanded={expanded}
                              aria-label={expanded ? 'Ocultar detalhes' : 'Mostrar detalhes'}
                              onClick={() => setOpen(expanded ? null : h.id)}
                            >
                              {expanded ? <IconChevronDown size={16} /> : <IconChevronRight size={16} />}
                            </ActionIcon>
                          )}
                        </Table.Td>
                        <Table.Td>
                          <DateTime value={h.occurred_at} />
                        </Table.Td>
                        <Table.Td>
                          <Text size="sm" span title={h.event}>
                            {labelOf(subscriptionEventLabels, h.event)}
                          </Text>
                        </Table.Td>
                        <Table.Td>
                          <Group gap={4} wrap="nowrap">
                            {h.from_status ? <StatusBadge status={h.from_status} labels={subscriptionStatusLabels} /> : <Text size="sm">—</Text>}
                            <Text size="sm">→</Text>
                            <StatusBadge status={h.to_status} labels={subscriptionStatusLabels} />
                          </Group>
                        </Table.Td>
                        <Table.Td>
                          <ActorCell type={h.actor_type} id={h.actor_id} />
                        </Table.Td>
                      </Table.Tr>
                      {hasDetails && (
                        <Table.Tr style={{ display: expanded ? undefined : 'none' }}>
                          <Table.Td colSpan={5} bg="gray.0">
                            <Collapse in={expanded}>
                              <Text size="xs" c="dimmed" mb={4}>
                                Dados técnicos registrados com o evento.
                              </Text>
                              <JsonView value={h.details} />
                            </Collapse>
                          </Table.Td>
                        </Table.Tr>
                      )}
                    </Fragment>
                  );
                })}
              </Table.Tbody>
            </Table>
          </ResponsiveTable>
        )
      }
    </QueryContent>
  );
}

/** Versão de preço contratada, pelo número e pela data da versão no catálogo. */
function PriceVersionText({ planId, priceVersionId }: { planId: string; priceVersionId: string }) {
  const { keys, can } = useTenant();
  const plan = useQuery({
    queryKey: keys('plans', 'detail', planId),
    queryFn: ({ signal }) => catalogApi.getPlan(planId, signal),
    enabled: can('catalog.read'),
  });
  const version = plan.data?.prices.find((p) => p.id === priceVersionId);
  if (!version) return <span title={priceVersionId}>Versão contratada do plano</span>;
  return (
    <span title={priceVersionId}>
      Versão {version.version}, de <DateTime value={version.created_at} dateOnly />
      {plan.data?.current_price?.id === priceVersionId ? ' (atual)' : ''}
    </span>
  );
}

/** Ator de uma transição: rotinas automáticas pelo nome; pessoas pelo tipo. */
function ActorCell({ type, id }: { type: string; id?: string | null }) {
  const routine = type === 'system' || type === 'provider' ? systemActorLabels[id ?? ''] : undefined;
  if (routine) return <>{routine}</>;
  return (
    <>
      {labelOf(actorTypeLabels, type)} {id && <ShortId id={id} />}
    </>
  );
}

function Overview({ subscription: s }: { subscription: Subscription }) {
  const plans = usePlans();
  const planName = (id: string | null | undefined) => plans.data?.data.find((p) => p.id === id)?.name ?? (id ? id.slice(0, 8) : '—');
  const pending = (s.scheduled_changes ?? []).filter((c) => c.status === 'pending');
  return (
    <Stack>
      <Paper withBorder p="md" radius="md">
        <SimpleGrid cols={{ base: 1, xs: 2, md: 4 }} spacing="lg">
          <KeyValue label="Cliente">
            <Anchor component={Link} to={`/clientes/${s.customer_id}`}>
              {s.customer_name || 'Ver cliente'}
            </Anchor>
          </KeyValue>
          <KeyValue label="Plano">
            <Anchor component={Link} to={`/catalogo/planos/${s.plan_id}`}>
              {s.plan_name || planName(s.plan_id)}
            </Anchor>
          </KeyValue>
          <KeyValue label="Quantidade">{s.quantity}</KeyValue>
          <KeyValue label="Periodicidade">{labelOf(billingIntervalLabels, s.billing_interval)}</KeyValue>
          <KeyValue label="Período atual">
            <DateTime value={s.current_period_start} dateOnly /> – <DateTime value={s.current_period_end} dateOnly />
          </KeyValue>
          <KeyValue label="Próxima cobrança">{s.next_billing_at ? <DateTime value={s.next_billing_at} /> : '—'}</KeyValue>
          {s.trial_ends_at && (
            <KeyValue label="Fim do período de teste">
              <DateTime value={s.trial_ends_at} />
            </KeyValue>
          )}
          <KeyValue label="Início">
            <DateTime value={s.started_at} />
          </KeyValue>
          <KeyValue label="Data-base do ciclo">
            <CivilDate value={s.anchor_date} />
          </KeyValue>
          <KeyValue label="Fuso do calendário">
            <span title={s.timezone}>{timeZoneLabel(s.timezone)}</span>
          </KeyValue>
          <KeyValue label="Prazo de pagamento">{s.payment_terms_days != null ? `${s.payment_terms_days} dias` : 'Padrão'}</KeyValue>
          <KeyValue label="Preço contratado">
            <PriceVersionText planId={s.plan_id} priceVersionId={s.price_version_id} />
          </KeyValue>
          {s.suspended_at && (
            <KeyValue label="Suspensa em">
              <DateTime value={s.suspended_at} />
            </KeyValue>
          )}
          {s.canceled_at && (
            <KeyValue label="Cancelada em">
              <DateTime value={s.canceled_at} />
            </KeyValue>
          )}
          {s.ended_at && (
            <KeyValue label="Encerrada em">
              <DateTime value={s.ended_at} />
            </KeyValue>
          )}
          {s.cancel_reason && <KeyValue label="Motivo do cancelamento">{s.cancel_reason}</KeyValue>}
        </SimpleGrid>
      </Paper>
      <Paper withBorder p="md" radius="md">
        <Title order={3} size="h5" mb="xs">
          Desconto
        </Title>
        {s.discount ? (
          <Group gap="sm">
            <Code>{s.discount.coupon_code}</Code>
            <StatusBadge status={s.discount.status} labels={discountStatusLabels} />
            <Text size="sm" c="dimmed">
              {s.discount.remaining_cycles != null ? `${s.discount.remaining_cycles} ciclo(s) restante(s)` : 'Sem limite de ciclos'}
            </Text>
          </Group>
        ) : (
          <Text size="sm" c="dimmed">
            Nenhum cupom aplicado.
          </Text>
        )}
      </Paper>
      <Paper withBorder p="md" radius="md">
        <Title order={3} size="h5" mb="xs">
          Mudanças agendadas
        </Title>
        {pending.length === 0 && (s.scheduled_changes ?? []).length === 0 ? (
          <Text size="sm" c="dimmed">
            Nenhuma mudança agendada.
          </Text>
        ) : (
          <Table aria-label="Mudanças agendadas">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Tipo</Table.Th>
                <Table.Th>Novo valor</Table.Th>
                <Table.Th>Vigência</Table.Th>
                <Table.Th>Situação</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {(s.scheduled_changes ?? []).map((c) => (
                <Table.Tr key={c.id}>
                  <Table.Td>{labelOf(scheduledChangeKindLabels, c.kind)}</Table.Td>
                  <Table.Td>{c.kind === 'plan_change' ? planName(c.new_plan_id) : c.new_quantity}</Table.Td>
                  <Table.Td>
                    <DateTime value={c.effective_at} />
                  </Table.Td>
                  <Table.Td>
                    <StatusBadge status={c.status} labels={scheduledChangeStatusLabels} />
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}
      </Paper>
    </Stack>
  );
}

const TABS = ['geral', 'faturas', 'historico', 'consumo', 'reajustes'] as const;

export function SubscriptionDetailPage() {
  const { id = '' } = useParams();
  const { keys } = useTenant();
  const [filters, setFilters] = useUrlFilters(['aba'] as const);
  const tab = (TABS as readonly string[]).includes(filters.aba) ? filters.aba : 'geral';
  const query = useQuery({ queryKey: keys('subscriptions', 'detail', id), queryFn: ({ signal }) => subscriptionsApi.get(id, signal) });

  return (
    <Stack gap="md">
      <Breadcrumbs>
        <Anchor component={Link} to="/assinaturas">
          Assinaturas
        </Anchor>
        <Text>{query.data ? `${query.data.customer_name ?? ''} · ${query.data.plan_name ?? ''}` : 'Assinatura'}</Text>
      </Breadcrumbs>
      <QueryContent query={query} loading={<LoadingState variant="detail" />}>
        {(s) => (
          <>
            <PageHeader
              title={`${s.plan_name ?? 'Assinatura'} — ${s.customer_name ?? ''}`}
              badge={<StatusBadge status={s.status} labels={subscriptionStatusLabels} size="lg" />}
              actions={<SubscriptionActions subscription={s} />}
            />
            <CancelScheduledAlert subscription={s} />
            <Tabs value={tab} onChange={(v) => setFilters({ aba: v === 'geral' ? null : v })} keepMounted={false}>
              <Tabs.List mb="md">
                <Tabs.Tab value="geral">Visão geral</Tabs.Tab>
                <Tabs.Tab value="faturas">Faturas</Tabs.Tab>
                <Tabs.Tab value="historico">Histórico</Tabs.Tab>
                <Tabs.Tab value="consumo">Consumo</Tabs.Tab>
                <Tabs.Tab value="reajustes">Reajustes</Tabs.Tab>
              </Tabs.List>
              <Tabs.Panel value="geral">
                <Overview subscription={s} />
              </Tabs.Panel>
              <Tabs.Panel value="faturas">
                <RelatedInvoices params={{ subscription_id: s.id }} emptyText="Nenhuma fatura gerada para esta assinatura ainda." />
              </Tabs.Panel>
              <Tabs.Panel value="historico">
                <HistoryPanel subscriptionId={s.id} />
              </Tabs.Panel>
              <Tabs.Panel value="consumo">
                <UsagePanel subscription={s} />
              </Tabs.Panel>
              <Tabs.Panel value="reajustes">
                <PriceAdjustmentsPanel subscription={s} />
              </Tabs.Panel>
            </Tabs>
          </>
        )}
      </QueryContent>
    </Stack>
  );
}
