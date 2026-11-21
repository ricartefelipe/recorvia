import { zodResolver } from '@hookform/resolvers/zod';
import { ActionIcon, Button, Code, Group, Modal, MultiSelect, NumberInput, Select, SimpleGrid, Stack, Table, Text, TextInput, Tooltip } from '@mantine/core';
import { DateTimePicker } from '@mantine/dates';
import { IconArchive, IconPlus } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Controller, useForm, useWatch } from 'react-hook-form';
import { z } from 'zod';
import type { Coupon, CouponInput } from '../../api/types';
import { DateTime, StatusBadge } from '../../components/format';
import { MoneyInput } from '../../components/inputs';
import { EmptyState, QueryContent } from '../../components/states';
import { confirmAction, LabeledSegmented } from '../../components/ui';
import { zonedWallTimeToUtc } from '../../lib/dates';
import { useApiMutation } from '../../lib/hooks';
import { catalogStatusLabels, couponDurationLabels, labelOf } from '../../lib/labels';
import { formatBasisPoints, formatMoney, parseMoneyToNumber, parsePercentToBasisPoints } from '../../lib/money';
import { optionalInt, requiredText } from '../../lib/validation';
import { Can, usePermission, useTenant } from '../../session/SessionContext';
import { catalogApi, usePlans } from './api';

const couponSchema = z
  .object({
    code: z.string().trim().regex(/^[A-Z0-9_-]{3,40}$/, 'Use de 3 a 40 letras maiúsculas, números, hífen ou sublinhado.'),
    name: requiredText(1, 200, 'O nome'),
    discount_type: z.enum(['percent', 'amount']),
    percent: z.string(),
    amount: z.string(),
    duration: z.enum(['once', 'repeating', 'forever']),
    duration_cycles: optionalInt(1, 120),
    max_redemptions: optionalInt(1, 1_000_000_000),
    valid_from: z.string().nullable(),
    valid_until: z.string().nullable(),
    applies_to_plan_ids: z.array(z.string()),
  })
  .superRefine((v, ctx) => {
    if (v.discount_type === 'percent') {
      const r = parsePercentToBasisPoints(v.percent);
      if (!r.ok) ctx.addIssue({ code: 'custom', path: ['percent'], message: r.error });
      else if (r.value < 1 || r.value > 10000) ctx.addIssue({ code: 'custom', path: ['percent'], message: 'Informe um percentual entre 0,01% e 100%.' });
    } else {
      const r = parseMoneyToNumber(v.amount);
      if (!r.ok) ctx.addIssue({ code: 'custom', path: ['amount'], message: r.error });
      else if (r.value < 1) ctx.addIssue({ code: 'custom', path: ['amount'], message: 'Informe um valor maior que zero.' });
    }
    if (v.duration === 'repeating' && v.duration_cycles === undefined) {
      ctx.addIssue({ code: 'custom', path: ['duration_cycles'], message: 'Informe por quantos ciclos o desconto vale.' });
    }
    if (v.valid_from && v.valid_until && v.valid_until <= v.valid_from) {
      ctx.addIssue({ code: 'custom', path: ['valid_until'], message: 'O fim da validade deve ser posterior ao início.' });
    }
  });
type CouponForm = z.input<typeof couponSchema>;
type CouponValues = z.output<typeof couponSchema>;

function toCouponInput(v: CouponValues, timeZone: string): CouponInput {
  const input: CouponInput = { code: v.code, name: v.name, discount_type: v.discount_type, duration: v.duration };
  if (v.discount_type === 'percent') {
    const r = parsePercentToBasisPoints(v.percent);
    if (r.ok) input.percent_off_bp = r.value;
  } else {
    const r = parseMoneyToNumber(v.amount);
    if (r.ok) input.amount_off = r.value;
  }
  if (v.duration === 'repeating') input.duration_cycles = v.duration_cycles;
  if (v.max_redemptions !== undefined) input.max_redemptions = v.max_redemptions;
  if (v.valid_from) input.valid_from = zonedWallTimeToUtc(v.valid_from, timeZone);
  if (v.valid_until) input.valid_until = zonedWallTimeToUtc(v.valid_until, timeZone);
  if (v.applies_to_plan_ids.length) input.applies_to_plan_ids = v.applies_to_plan_ids;
  return input;
}

function CouponModal({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const { timeZone } = useTenant();
  const plans = usePlans({ status: 'active' });
  const form = useForm<CouponForm, unknown, CouponValues>({
    resolver: zodResolver(couponSchema),
    defaultValues: {
      code: '',
      name: '',
      discount_type: 'percent',
      percent: '',
      amount: '',
      duration: 'once',
      duration_cycles: '',
      max_redemptions: '',
      valid_from: null,
      valid_until: null,
      applies_to_plan_ids: [],
    },
  });
  const mutation = useApiMutation<CouponInput, Coupon, CouponForm>({
    mutationFn: catalogApi.createCoupon,
    invalidate: ['coupons'],
    successMessage: 'Cupom criado.',
    form,
    fieldMap: { percent_off_bp: 'percent', amount_off: 'amount' },
    onSuccess: () => {
      form.reset();
      onClose();
    },
  });
  const { register, control, formState, setValue } = form;
  const discountType = useWatch({ control, name: 'discount_type' });
  const duration = useWatch({ control, name: 'duration' });
  const intNumber = (name: 'duration_cycles' | 'max_redemptions', label: string, description?: string) => (
    <Controller
      control={control}
      name={name}
      render={({ field, fieldState }) => (
        <NumberInput
          label={label}
          description={description}
          min={1}
          allowDecimal={false}
          value={field.value}
          onChange={(v) => field.onChange(v === '' ? '' : Number(v))}
          error={fieldState.error?.message}
        />
      )}
    />
  );
  return (
    <Modal opened={opened} onClose={onClose} title="Novo cupom" size="lg">
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(toCouponInput(v, timeZone)))} noValidate>
        <Stack>
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <TextInput
              label="Código"
              placeholder="BEMVINDO10"
              required
              {...register('code', { onChange: (e) => setValue('code', String(e.target.value).toUpperCase()) })}
              error={formState.errors.code?.message}
            />
            <TextInput label="Nome" required {...register('name')} error={formState.errors.name?.message} />
          </SimpleGrid>
          <Controller
            control={control}
            name="discount_type"
            render={({ field }) => (
              <LabeledSegmented
                label="Tipo de desconto"
                value={field.value}
                onChange={field.onChange}
                data={[
                  { value: 'percent', label: 'Percentual' },
                  { value: 'amount', label: 'Valor fixo' },
                ]}
              />
            )}
          />
          {discountType === 'percent' ? (
            <TextInput
              label="Percentual de desconto"
              placeholder="10"
              rightSection={<Text size="sm" c="dimmed">%</Text>}
              description="Até duas casas decimais, por exemplo 12,5."
              inputMode="decimal"
              required
              {...register('percent')}
              error={formState.errors.percent?.message}
            />
          ) : (
            <MoneyInput label="Valor do desconto" required {...register('amount')} error={formState.errors.amount?.message} />
          )}
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <Controller
              control={control}
              name="duration"
              render={({ field }) => (
                <Select
                  label="Duração"
                  data={Object.entries(couponDurationLabels).map(([value, label]) => ({ value, label }))}
                  value={field.value}
                  onChange={(v) => v && field.onChange(v)}
                  allowDeselect={false}
                />
              )}
            />
            {duration === 'repeating' && intNumber('duration_cycles', 'Número de ciclos')}
            {intNumber('max_redemptions', 'Limite de resgates', 'Vazio para ilimitado.')}
          </SimpleGrid>
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <Controller
              control={control}
              name="valid_from"
              render={({ field }) => (
                <DateTimePicker label="Válido a partir de" description={`Horário de ${timeZone}`} clearable value={field.value} onChange={field.onChange} valueFormat="DD/MM/YYYY HH:mm" />
              )}
            />
            <Controller
              control={control}
              name="valid_until"
              render={({ field, fieldState }) => (
                <DateTimePicker label="Válido até" description={`Horário de ${timeZone}`} clearable value={field.value} onChange={field.onChange} valueFormat="DD/MM/YYYY HH:mm" error={fieldState.error?.message} />
              )}
            />
          </SimpleGrid>
          <Controller
            control={control}
            name="applies_to_plan_ids"
            render={({ field }) => (
              <MultiSelect
                label="Planos elegíveis"
                description="Vazio permite aplicar em qualquer plano."
                data={(plans.data?.data ?? []).map((p) => ({ value: p.id, label: p.name }))}
                value={field.value}
                onChange={field.onChange}
                searchable
                clearable
              />
            )}
          />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Criar cupom
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

function discountText(c: Coupon): string {
  if (c.discount_type === 'percent') return `${formatBasisPoints(c.percent_off_bp)} de desconto`;
  return `${formatMoney(c.amount_off, c.currency)} de desconto`;
}

function durationText(c: Coupon): string {
  if (c.duration === 'repeating') return `Por ${c.duration_cycles ?? '?'} ciclos`;
  return labelOf(couponDurationLabels, c.duration);
}

export function CouponsPage() {
  const { keys } = useTenant();
  const [creating, setCreating] = useState(false);
  const canWrite = usePermission('catalog.write');
  const query = useQuery({ queryKey: keys('coupons', 'list'), queryFn: ({ signal }) => catalogApi.listCoupons(signal) });
  const archive = useApiMutation<Coupon, Coupon>({
    mutationFn: (c) => catalogApi.archiveCoupon(c.id, c.version),
    invalidate: ['coupons'],
    successMessage: 'Cupom arquivado.',
  });
  const onArchive = async (c: Coupon) => {
    const ok = await confirmAction({
      title: 'Arquivar cupom',
      message: `Arquivar o cupom ${c.code}? Ele não poderá ser aplicado em novas assinaturas; descontos já aplicados continuam valendo.`,
      confirmLabel: 'Arquivar',
      danger: true,
    });
    if (ok) archive.mutate(c);
  };

  return (
    <Stack>
      <Group justify="flex-end">
        <Can permission="catalog.write">
          <Button leftSection={<IconPlus size={16} />} onClick={() => setCreating(true)}>
            Novo cupom
          </Button>
        </Can>
      </Group>
      <QueryContent query={query}>
        {({ data }) =>
          data.length === 0 ? (
            <EmptyState title="Nenhum cupom" description="Cupons concedem descontos percentuais ou de valor fixo nas assinaturas." />
          ) : (
            <Table.ScrollContainer minWidth={880}>
              <Table aria-label="Cupons">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Código</Table.Th>
                    <Table.Th>Nome</Table.Th>
                    <Table.Th>Desconto</Table.Th>
                    <Table.Th>Duração</Table.Th>
                    <Table.Th>Resgates</Table.Th>
                    <Table.Th>Validade</Table.Th>
                    <Table.Th>Situação</Table.Th>
                    {canWrite && <Table.Th aria-label="Ações" />}
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {data.map((c) => (
                    <Table.Tr key={c.id}>
                      <Table.Td>
                        <Code>{c.code}</Code>
                      </Table.Td>
                      <Table.Td>{c.name}</Table.Td>
                      <Table.Td>{discountText(c)}</Table.Td>
                      <Table.Td>{durationText(c)}</Table.Td>
                      <Table.Td>
                        {c.redemptions_count}
                        {c.max_redemptions ? ` de ${c.max_redemptions}` : ''}
                      </Table.Td>
                      <Table.Td>
                        <Text size="sm">
                          {c.valid_from ? <DateTime value={c.valid_from} dateOnly /> : 'Desde a criação'} –{' '}
                          {c.valid_until ? <DateTime value={c.valid_until} dateOnly /> : 'sem fim'}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <StatusBadge status={c.status} labels={catalogStatusLabels} />
                      </Table.Td>
                      {canWrite && (
                        <Table.Td>
                          {c.status === 'active' && (
                            <Tooltip label="Arquivar cupom">
                              <ActionIcon variant="subtle" color="red" aria-label={`Arquivar cupom ${c.code}`} onClick={() => void onArchive(c)}>
                                <IconArchive size={16} />
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
          )
        }
      </QueryContent>
      <CouponModal opened={creating} onClose={() => setCreating(false)} />
    </Stack>
  );
}
