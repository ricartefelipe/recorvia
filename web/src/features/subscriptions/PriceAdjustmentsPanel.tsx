import { zodResolver } from '@hookform/resolvers/zod';
import { ActionIcon, Button, Group, Modal, Stack, Table, Text, Textarea, TextInput, Tooltip } from '@mantine/core';
import { DatePickerInput } from '@mantine/dates';
import { IconCalendarPlus, IconX } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Controller, useForm, useWatch } from 'react-hook-form';
import { z } from 'zod';
import type { PriceAdjustment, PriceAdjustmentInput, Subscription } from '../../api/types';
import { CivilDate, Money, StatusBadge } from '../../components/format';
import { MoneyInput } from '../../components/inputs';
import { EmptyState, QueryContent } from '../../components/states';
import { confirmAction, CursorPager, LabeledSegmented } from '../../components/ui';
import { addDaysToCivilDate, todayInTimeZone } from '../../lib/dates';
import { useApiMutation, useCursorPagination, useReturnFromEmptyPage } from '../../lib/hooks';
import { useIdempotencyKey } from '../../lib/idempotency';
import { priceAdjustmentKindLabels, priceAdjustmentStatusLabels, labelOf } from '../../lib/labels';
import { formatBasisPoints, parseMoneyToNumber, parsePercentToBasisPoints } from '../../lib/money';
import { optionalText, reasonField } from '../../lib/validation';
import { Can, usePermission, useTenant } from '../../session/SessionContext';
import { subscriptionsApi } from './api';

const schema = z
  .object({
    effective_date: z.string().nullable().refine((v) => !!v, 'Informe a data de vigência.'),
    kind: z.enum(['percent', 'fixed_amount']),
    percent: z.string(),
    new_amount: z.string(),
    index_name: optionalText(50),
    reason: reasonField,
  })
  .superRefine((v, ctx) => {
    if (v.kind === 'percent') {
      const r = parsePercentToBasisPoints(v.percent, { allowNegative: true });
      if (!r.ok) ctx.addIssue({ code: 'custom', path: ['percent'], message: r.error });
      else if (r.value < -10000 || r.value > 100000 || r.value === 0)
        ctx.addIssue({ code: 'custom', path: ['percent'], message: 'Informe um percentual diferente de zero, entre -100% e 1.000%.' });
    } else {
      const r = parseMoneyToNumber(v.new_amount);
      if (!r.ok) ctx.addIssue({ code: 'custom', path: ['new_amount'], message: r.error });
    }
  });
type FormValues = z.input<typeof schema>;
type Output = z.output<typeof schema>;

function toInput(v: Output): PriceAdjustmentInput {
  const input: PriceAdjustmentInput = { effective_date: v.effective_date as string, kind: v.kind, reason: v.reason };
  if (v.kind === 'percent') {
    const r = parsePercentToBasisPoints(v.percent, { allowNegative: true });
    if (r.ok) input.percent_bp = r.value;
  } else {
    const r = parseMoneyToNumber(v.new_amount);
    if (r.ok) input.new_amount = r.value;
  }
  if (v.index_name) input.index_name = v.index_name;
  return input;
}

function ScheduleModal({ subscription, opened, onClose }: { subscription: Subscription; opened: boolean; onClose: () => void }) {
  const { timeZone } = useTenant();
  const today = todayInTimeZone(timeZone);
  const form = useForm<FormValues, unknown, Output>({
    resolver: zodResolver(schema),
    defaultValues: { effective_date: null, kind: 'percent', percent: '', new_amount: '', index_name: '', reason: '' },
  });
  const idempotency = useIdempotencyKey();
  const mutation = useApiMutation<PriceAdjustmentInput, PriceAdjustment, FormValues>({
    mutationFn: (input) =>
      subscriptionsApi.scheduleAdjustment(subscription.id, input, idempotency.keyFor({ subscription: subscription.id, ...input })),
    idempotency,
    invalidate: ['subscriptions'],
    successMessage: 'Reajuste programado.',
    form,
    fieldMap: { percent_bp: 'percent' },
    onSuccess: () => {
      form.reset();
      onClose();
    },
  });
  const kind = useWatch({ control: form.control, name: 'kind' });
  const { register, control, formState } = form;
  return (
    <Modal opened={opened} onClose={onClose} title="Programar reajuste" size="lg">
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(toInput(v)))} noValidate>
        <Stack>
          <Controller
            control={control}
            name="effective_date"
            render={({ field, fieldState }) => (
              <DatePickerInput
                label="Vigência"
                description="Vale a partir do primeiro ciclo iniciado nesta data ou depois."
                required
                value={field.value}
                onChange={field.onChange}
                minDate={addDaysToCivilDate(today, 1)}
                valueFormat="DD/MM/YYYY"
                error={fieldState.error?.message}
              />
            )}
          />
          <Controller
            control={control}
            name="kind"
            render={({ field }) => (
              <LabeledSegmented
                label="Tipo de reajuste"
                value={field.value}
                onChange={field.onChange}
                data={[
                  { value: 'percent', label: 'Percentual' },
                  { value: 'fixed_amount', label: 'Novo valor' },
                ]}
              />
            )}
          />
          {kind === 'percent' ? (
            <TextInput
              label="Percentual"
              description="Use valor negativo para redução, por exemplo -5."
              placeholder="4,5"
              rightSection={<Text size="sm" c="dimmed">%</Text>}
              inputMode="decimal"
              required
              {...register('percent')}
              error={formState.errors.percent?.message}
            />
          ) : (
            <MoneyInput
              label="Novo valor fixo ou unitário"
              description="Substitui o valor fixo (ou o preço unitário) da versão contratada."
              required
              {...register('new_amount')}
              error={formState.errors.new_amount?.message}
            />
          )}
          <TextInput label="Índice de referência" placeholder="IPCA, IGP-M…" description="Informativo." {...register('index_name')} error={formState.errors.index_name?.message} />
          <Textarea label="Motivo" required autosize minRows={2} maxLength={500} {...register('reason')} error={formState.errors.reason?.message} />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Programar
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

export function PriceAdjustmentsPanel({ subscription }: { subscription: Subscription }) {
  const { keys } = useTenant();
  const [opened, setOpened] = useState(false);
  const canWrite = usePermission('subscriptions.write');
  const pagination = useCursorPagination(`adjustments:${subscription.id}`);
  const query = useQuery({
    queryKey: keys('subscriptions', 'adjustments', subscription.id, pagination.cursor),
    queryFn: ({ signal }) => subscriptionsApi.listAdjustments(subscription.id, { cursor: pagination.cursor, limit: 25 }, signal),
    placeholderData: keepPreviousData,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);
  const cancel = useApiMutation<PriceAdjustment, unknown>({
    mutationFn: (a) => subscriptionsApi.cancelAdjustment(subscription.id, a.id),
    invalidate: ['subscriptions'],
    successMessage: 'Reajuste cancelado.',
  });
  return (
    <Stack>
      <Group justify="space-between">
        <Text size="sm" c="dimmed" maw={640}>
          Reajustes alteram o valor desta assinatura a partir da vigência, sem mudar o catálogo nem faturas já emitidas.
        </Text>
        <Can permission="subscriptions.write">
          {subscription.status !== 'canceled' && (
            <Button leftSection={<IconCalendarPlus size={16} />} onClick={() => setOpened(true)}>
              Programar reajuste
            </Button>
          )}
        </Can>
      </Group>
      <QueryContent query={query}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhum reajuste" description="Programe reajustes anuais por índice ou por novo valor." />
          ) : (
            <>
              <Table.ScrollContainer minWidth={820}>
                <Table aria-label="Reajustes">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Vigência</Table.Th>
                      <Table.Th>Tipo</Table.Th>
                      <Table.Th>Reajuste</Table.Th>
                      <Table.Th>Índice</Table.Th>
                      <Table.Th>Antes → depois</Table.Th>
                      <Table.Th>Situação</Table.Th>
                      <Table.Th>Motivo</Table.Th>
                      {canWrite && <Table.Th aria-label="Ações" />}
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.data.map((a) => (
                      <Table.Tr key={a.id}>
                        <Table.Td>
                          <CivilDate value={a.effective_date} />
                        </Table.Td>
                        <Table.Td>{labelOf(priceAdjustmentKindLabels, a.kind)}</Table.Td>
                        <Table.Td>{a.kind === 'percent' ? formatBasisPoints(a.percent_bp) : <Money value={a.new_amount} />}</Table.Td>
                        <Table.Td>{a.index_name || '—'}</Table.Td>
                        <Table.Td>
                          {a.previous_amount !== null && a.previous_amount !== undefined ? (
                            <>
                              <Money value={a.previous_amount} /> → <Money value={a.resulting_amount} />
                            </>
                          ) : (
                            '—'
                          )}
                        </Table.Td>
                        <Table.Td>
                          <StatusBadge status={a.status} labels={priceAdjustmentStatusLabels} />
                        </Table.Td>
                        <Table.Td>{a.reason}</Table.Td>
                        {canWrite && (
                          <Table.Td>
                            {a.status === 'scheduled' && (
                              <Tooltip label="Cancelar reajuste">
                                <ActionIcon
                                  variant="subtle"
                                  color="red"
                                  aria-label="Cancelar reajuste"
                                  onClick={async () => {
                                    const ok = await confirmAction({ title: 'Cancelar reajuste', message: 'O reajuste programado não será aplicado.', confirmLabel: 'Cancelar reajuste', danger: true });
                                    if (ok) cancel.mutate(a);
                                  }}
                                >
                                  <IconX size={16} />
                                </ActionIcon>
                              </Tooltip>
                            )}
                          </Table.Td>
                        )}
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
      <ScheduleModal subscription={subscription} opened={opened} onClose={() => setOpened(false)} />
    </Stack>
  );
}
