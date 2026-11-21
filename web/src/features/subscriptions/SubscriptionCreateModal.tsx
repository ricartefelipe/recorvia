import { zodResolver } from '@hookform/resolvers/zod';
import { Button, Group, Modal, NumberInput, Select, SimpleGrid, Stack, Text } from '@mantine/core';
import { DatePickerInput } from '@mantine/dates';
import { Controller, useForm, useWatch } from 'react-hook-form';
import { useNavigate } from 'react-router';
import { z } from 'zod';
import type { Subscription, SubscriptionInput } from '../../api/types';
import { addDaysToCivilDate, todayInTimeZone } from '../../lib/dates';
import { useApiMutation } from '../../lib/hooks';
import { useIdempotencyKey } from '../../lib/idempotency';
import { optionalInt, requiredInt } from '../../lib/validation';
import { useTenant } from '../../session/SessionContext';
import { useMetrics, usePlans } from '../catalog/api';
import { describePrice } from '../catalog/PriceFields';
import { CustomerSelect } from '../customers/CustomerSelect';
import { subscriptionsApi } from './api';

const schema = z.object({
  customer_id: z.string().min(1, 'Selecione o cliente.'),
  plan_id: z.string().min(1, 'Selecione o plano.'),
  quantity: requiredInt(1, 1_000_000, 'Informe uma quantidade entre 1 e 1.000.000.'),
  start_date: z.string().nullable(),
  trial_days: optionalInt(0, 365),
  payment_terms_days: optionalInt(0, 120),
});
type FormValues = z.input<typeof schema>;
type Output = z.output<typeof schema>;

export function SubscriptionCreateModal({
  opened,
  onClose,
  customerId,
}: {
  opened: boolean;
  onClose: () => void;
  customerId?: string;
}) {
  const { timeZone } = useTenant();
  const navigate = useNavigate();
  const plans = usePlans({ status: 'active' });
  const metrics = useMetrics();
  const today = todayInTimeZone(timeZone);
  const form = useForm<FormValues, unknown, Output>({
    resolver: zodResolver(schema),
    values: { customer_id: customerId ?? '', plan_id: '', quantity: 1, start_date: null, trial_days: '', payment_terms_days: '' },
  });
  const idempotency = useIdempotencyKey();
  const mutation = useApiMutation<SubscriptionInput, Subscription, FormValues>({
    mutationFn: (input) => subscriptionsApi.create(input, idempotency.keyFor(input)),
    idempotency,
    invalidate: ['subscriptions', 'customers', 'dashboard'],
    successMessage: 'Assinatura criada.',
    form,
    onSuccess: (s) => {
      onClose();
      navigate(`/assinaturas/${s.id}`);
    },
  });
  const planId = useWatch({ control: form.control, name: 'plan_id' });
  const plan = plans.data?.data.find((p) => p.id === planId);

  const submit = form.handleSubmit((v) => {
    const input: SubscriptionInput = { customer_id: v.customer_id, plan_id: v.plan_id, quantity: v.quantity };
    if (v.start_date) input.start_date = v.start_date;
    if (v.trial_days !== undefined) input.trial_days = v.trial_days;
    if (v.payment_terms_days !== undefined) input.payment_terms_days = v.payment_terms_days;
    mutation.mutate(input);
  });

  const numberField = (name: 'quantity' | 'trial_days' | 'payment_terms_days', label: string, props: { min: number; max: number; description?: string; placeholder?: string; required?: boolean }) => (
    <Controller
      control={form.control}
      name={name}
      render={({ field, fieldState }) => (
        <NumberInput
          label={label}
          allowDecimal={false}
          allowNegative={false}
          {...props}
          value={field.value}
          onChange={(v) => field.onChange(v === '' ? '' : Number(v))}
          onBlur={field.onBlur}
          error={fieldState.error?.message}
        />
      )}
    />
  );

  return (
    <Modal opened={opened} onClose={onClose} title="Nova assinatura" size="lg">
      <form onSubmit={submit} noValidate>
        <Stack>
          <Controller
            control={form.control}
            name="customer_id"
            render={({ field, fieldState }) => (
              <CustomerSelect label="Cliente" required value={field.value || null} onChange={(v) => field.onChange(v ?? '')} error={fieldState.error?.message} />
            )}
          />
          <Controller
            control={form.control}
            name="plan_id"
            render={({ field, fieldState }) => (
              <Select
                label="Plano"
                required
                searchable
                data={(plans.data?.data ?? []).map((p) => ({ value: p.id, label: p.name }))}
                value={field.value || null}
                onChange={(v) => field.onChange(v ?? '')}
                error={fieldState.error?.message}
                nothingFoundMessage="Nenhum plano ativo"
                description={plan ? describePrice(plan.current_price, metrics.data?.data, plan.billing_interval) : undefined}
              />
            )}
          />
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            {numberField('quantity', 'Quantidade', { min: 1, max: 1_000_000, required: true, description: 'Usada nos planos por quantidade.' })}
            <Controller
              control={form.control}
              name="start_date"
              render={({ field }) => (
                <DatePickerInput
                  label="Data de início"
                  description="Hoje, se vazio. Pode ser retroativa em até 366 dias."
                  placeholder="Hoje"
                  clearable
                  value={field.value}
                  onChange={field.onChange}
                  maxDate={today}
                  minDate={addDaysToCivilDate(today, -366)}
                  valueFormat="DD/MM/YYYY"
                />
              )}
            />
            {numberField('trial_days', 'Dias de avaliação', {
              min: 0,
              max: 365,
              description: 'Vazio usa o padrão do plano.',
              placeholder: plan ? String(plan.trial_days) : undefined,
            })}
            {numberField('payment_terms_days', 'Prazo de pagamento (dias)', { min: 0, max: 120, description: 'Vazio usa o prazo do cliente ou da organização.' })}
          </SimpleGrid>
          <Text size="xs" c="dimmed">
            A assinatura guarda a versão de preço atual do plano. Mudanças futuras de preço no catálogo não a afetam.
          </Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Criar assinatura
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
