import { zodResolver } from '@hookform/resolvers/zod';
import { Alert, Button, Group, Menu, Modal, NumberInput, Radio, Select, Stack, Text, Textarea, TextInput } from '@mantine/core';
import {
  IconArrowsExchange,
  IconBan,
  IconChevronDown,
  IconDiscount2,
  IconPlayerPause,
  IconPlayerPlay,
  IconRotateClockwise,
  IconStack2,
} from '@tabler/icons-react';
import { useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { z } from 'zod';
import type { ChangeTiming, Subscription, SubscriptionDiscount } from '../../api/types';
import { confirmAction } from '../../components/ui';
import { formatInstantDate } from '../../lib/dates';
import { useApiMutation } from '../../lib/hooks';
import { changeTimingLabels } from '../../lib/labels';
import { optionalInt, optionalText, requiredInt } from '../../lib/validation';
import { useSession, useTenant } from '../../session/SessionContext';
import { useMetrics, usePlans } from '../catalog/api';
import { describePrice } from '../catalog/PriceFields';
import { subscriptionsApi } from './api';

type ActionKind = 'cancel' | 'suspend' | 'plan' | 'quantity' | 'coupon' | null;
const INVALIDATE = ['subscriptions', 'invoices', 'dashboard'] as const;

function TimingField({ value, onChange }: { value: ChangeTiming; onChange: (v: ChangeTiming) => void }) {
  return (
    <Radio.Group label="Quando aplicar" value={value} onChange={(v) => onChange(v as ChangeTiming)} required>
      <Stack gap="xs" mt="xs">
        <Radio value="immediate" label={changeTimingLabels.immediate} description="Gera fatura de prorrata pela diferença no período corrente." />
        <Radio value="next_cycle" label={changeTimingLabels.next_cycle} description="Vale a partir da próxima renovação, sem prorrata." />
      </Stack>
    </Radio.Group>
  );
}

const cancelSchema = z.object({ mode: z.enum(['immediate', 'period_end']), reason: optionalText(500) });

function CancelModal({ subscription, onClose }: { subscription: Subscription; onClose: () => void }) {
  const { timeZone } = useTenant();
  const form = useForm<z.input<typeof cancelSchema>, unknown, z.output<typeof cancelSchema>>({
    resolver: zodResolver(cancelSchema),
    defaultValues: { mode: 'period_end', reason: '' },
  });
  const mutation = useApiMutation<z.output<typeof cancelSchema>, Subscription>({
    mutationFn: (input) => subscriptionsApi.cancel(subscription, input),
    invalidate: [...INVALIDATE],
    successMessage: (s) => (s.status === 'canceled' ? 'Assinatura cancelada.' : 'Cancelamento agendado para o fim do período.'),
    onSuccess: onClose,
  });
  const submit = form.handleSubmit(async (input) => {
    const ok = await confirmAction({
      title: 'Confirmar cancelamento',
      message:
        input.mode === 'immediate'
          ? 'A assinatura será encerrada agora e não haverá novas cobranças. Esta ação não pode ser desfeita.'
          : `A assinatura continua ativa até ${formatInstantDate(subscription.current_period_end, timeZone)} e não será renovada. Você pode desfazer até lá.`,
      confirmLabel: input.mode === 'immediate' ? 'Cancelar agora' : 'Agendar cancelamento',
      danger: true,
    });
    if (ok) mutation.mutate(input);
  });
  return (
    <form onSubmit={submit} noValidate>
      <Stack>
        <Controller
          control={form.control}
          name="mode"
          render={({ field }) => (
            <Radio.Group label="Modo de cancelamento" value={field.value} onChange={field.onChange} required>
              <Stack gap="xs" mt="xs">
                <Radio value="period_end" label="No fim do período atual" description={`Sem renovação após ${formatInstantDate(subscription.current_period_end, timeZone)}.`} />
                <Radio value="immediate" label="Imediatamente" description="Encerra a assinatura agora." />
              </Stack>
            </Radio.Group>
          )}
        />
        <Textarea label="Motivo" autosize minRows={2} maxLength={500} {...form.register('reason')} error={form.formState.errors.reason?.message} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            Voltar
          </Button>
          <Button type="submit" color="red" loading={mutation.isPending}>
            Continuar
          </Button>
        </Group>
      </Stack>
    </form>
  );
}

const suspendSchema = z.object({ reason: optionalText(500) });

function SuspendModal({ subscription, onClose }: { subscription: Subscription; onClose: () => void }) {
  const form = useForm<z.input<typeof suspendSchema>, unknown, z.output<typeof suspendSchema>>({
    resolver: zodResolver(suspendSchema),
    defaultValues: { reason: '' },
  });
  const mutation = useApiMutation<z.output<typeof suspendSchema>, Subscription>({
    mutationFn: (input) => subscriptionsApi.suspend(subscription, input),
    invalidate: [...INVALIDATE],
    successMessage: 'Assinatura suspensa.',
    onSuccess: onClose,
  });
  return (
    <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
      <Stack>
        <Text size="sm">A assinatura deixa de gerar novas faturas até ser reativada.</Text>
        <Textarea label="Motivo" autosize minRows={2} maxLength={500} {...form.register('reason')} error={form.formState.errors.reason?.message} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            Voltar
          </Button>
          <Button type="submit" color="orange" loading={mutation.isPending}>
            Suspender
          </Button>
        </Group>
      </Stack>
    </form>
  );
}

const planSchema = z.object({
  plan_id: z.string().min(1, 'Selecione o novo plano.'),
  quantity: optionalInt(1, 1_000_000),
  timing: z.enum(['immediate', 'next_cycle']),
});

function ChangePlanModal({ subscription, onClose }: { subscription: Subscription; onClose: () => void }) {
  const plans = usePlans({ status: 'active' });
  const metrics = useMetrics();
  const form = useForm<z.input<typeof planSchema>, unknown, z.output<typeof planSchema>>({
    resolver: zodResolver(planSchema),
    defaultValues: { plan_id: '', quantity: '', timing: 'next_cycle' },
  });
  const mutation = useApiMutation<z.output<typeof planSchema>, Subscription, z.input<typeof planSchema>>({
    mutationFn: (input) => subscriptionsApi.changePlan(subscription, input),
    invalidate: [...INVALIDATE],
    successMessage: (_s, v) => (v.timing === 'immediate' ? 'Plano alterado.' : 'Troca de plano agendada para o próximo ciclo.'),
    form,
    onSuccess: onClose,
  });
  const options = (plans.data?.data ?? [])
    .filter((p) => p.id !== subscription.plan_id)
    .map((p) => ({ value: p.id, label: `${p.name} — ${describePrice(p.current_price, metrics.data?.data, p.billing_interval)}` }));
  return (
    <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
      <Stack>
        <Controller
          control={form.control}
          name="plan_id"
          render={({ field, fieldState }) => (
            <Select label="Novo plano" required searchable data={options} value={field.value || null} onChange={(v) => field.onChange(v ?? '')} error={fieldState.error?.message} />
          )}
        />
        <Controller
          control={form.control}
          name="quantity"
          render={({ field, fieldState }) => (
            <NumberInput
              label="Nova quantidade"
              description={`Vazio mantém a quantidade atual (${subscription.quantity}).`}
              min={1}
              max={1_000_000}
              allowDecimal={false}
              value={field.value}
              onChange={(v) => field.onChange(v === '' ? '' : Number(v))}
              error={fieldState.error?.message}
            />
          )}
        />
        <Controller control={form.control} name="timing" render={({ field }) => <TimingField value={field.value} onChange={field.onChange} />} />
        <Text size="xs" c="dimmed">
          O novo plano entra com a versão de preço vigente dele.
        </Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            Voltar
          </Button>
          <Button type="submit" loading={mutation.isPending}>
            Trocar plano
          </Button>
        </Group>
      </Stack>
    </form>
  );
}

const quantitySchema = z.object({
  quantity: requiredInt(1, 1_000_000, 'Informe uma quantidade entre 1 e 1.000.000.'),
  timing: z.enum(['immediate', 'next_cycle']),
});

function ChangeQuantityModal({ subscription, onClose }: { subscription: Subscription; onClose: () => void }) {
  const form = useForm<z.input<typeof quantitySchema>, unknown, z.output<typeof quantitySchema>>({
    resolver: zodResolver(quantitySchema),
    defaultValues: { quantity: subscription.quantity, timing: 'immediate' },
  });
  const mutation = useApiMutation<z.output<typeof quantitySchema>, Subscription, z.input<typeof quantitySchema>>({
    mutationFn: (input) => subscriptionsApi.changeQuantity(subscription, input),
    invalidate: [...INVALIDATE],
    successMessage: (_s, v) => (v.timing === 'immediate' ? 'Quantidade alterada.' : 'Alteração de quantidade agendada.'),
    form,
    onSuccess: onClose,
  });
  return (
    <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
      <Stack>
        <Controller
          control={form.control}
          name="quantity"
          render={({ field, fieldState }) => (
            <NumberInput
              label="Nova quantidade"
              required
              min={1}
              max={1_000_000}
              allowDecimal={false}
              value={field.value}
              onChange={(v) => field.onChange(v === '' ? '' : Number(v))}
              error={fieldState.error?.message}
            />
          )}
        />
        <Controller control={form.control} name="timing" render={({ field }) => <TimingField value={field.value} onChange={field.onChange} />} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            Voltar
          </Button>
          <Button type="submit" loading={mutation.isPending}>
            Alterar quantidade
          </Button>
        </Group>
      </Stack>
    </form>
  );
}

const couponSchema = z.object({ coupon_code: z.string().trim().min(3, 'Informe o código do cupom.').max(40) });

function ApplyCouponModal({ subscription, onClose }: { subscription: Subscription; onClose: () => void }) {
  const form = useForm<z.input<typeof couponSchema>>({ resolver: zodResolver(couponSchema), defaultValues: { coupon_code: '' } });
  const mutation = useApiMutation<string, SubscriptionDiscount, z.input<typeof couponSchema>>({
    mutationFn: (code) => subscriptionsApi.applyCoupon(subscription.id, code),
    invalidate: ['subscriptions', 'coupons'],
    successMessage: (d) => `Cupom ${d.coupon_code} aplicado.`,
    form,
    onSuccess: onClose,
  });
  return (
    <form onSubmit={form.handleSubmit((v) => mutation.mutate(v.coupon_code))} noValidate>
      <Stack>
        <TextInput
          label="Código do cupom"
          required
          {...form.register('coupon_code', { onChange: (e) => form.setValue('coupon_code', String(e.target.value).toUpperCase()) })}
          error={form.formState.errors.coupon_code?.message}
        />
        <Text size="xs" c="dimmed">
          O desconto vale a partir da próxima fatura, conforme a duração do cupom.
        </Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            Voltar
          </Button>
          <Button type="submit" loading={mutation.isPending}>
            Aplicar cupom
          </Button>
        </Group>
      </Stack>
    </form>
  );
}

const TITLES: Record<Exclude<ActionKind, null>, string> = {
  cancel: 'Cancelar assinatura',
  suspend: 'Suspender assinatura',
  plan: 'Trocar plano',
  quantity: 'Alterar quantidade',
  coupon: 'Aplicar cupom',
};

/** Menu de ações da assinatura conforme a situação e as permissões. */
export function SubscriptionActions({ subscription }: { subscription: Subscription }) {
  const { can } = useSession();
  const [action, setAction] = useState<ActionKind>(null);
  const close = () => setAction(null);
  const s = subscription;
  const live = s.status !== 'canceled';
  const canWrite = can('subscriptions.write');
  const canCancel = can('subscriptions.cancel');

  const resume = useApiMutation<void, Subscription>({
    mutationFn: () => subscriptionsApi.resumeRenewal(s),
    invalidate: [...INVALIDATE],
    successMessage: 'Cancelamento agendado desfeito. A assinatura será renovada.',
  });
  const reactivate = useApiMutation<void, Subscription>({
    mutationFn: () => subscriptionsApi.reactivate(s),
    invalidate: [...INVALIDATE],
    successMessage: 'Assinatura reativada.',
  });
  const removeDiscount = useApiMutation<void, unknown>({
    mutationFn: () => subscriptionsApi.removeDiscount(s.id),
    invalidate: ['subscriptions'],
    successMessage: 'Desconto encerrado.',
  });

  const items = [
    canWrite && (s.status === 'trialing' || s.status === 'active' || s.status === 'past_due') && (
      <Menu.Item key="plan" leftSection={<IconArrowsExchange size={16} />} onClick={() => setAction('plan')}>
        Trocar plano
      </Menu.Item>
    ),
    canWrite && (s.status === 'trialing' || s.status === 'active' || s.status === 'past_due') && (
      <Menu.Item key="quantity" leftSection={<IconStack2 size={16} />} onClick={() => setAction('quantity')}>
        Alterar quantidade
      </Menu.Item>
    ),
    canWrite && live && !s.discount && (
      <Menu.Item key="coupon" leftSection={<IconDiscount2 size={16} />} onClick={() => setAction('coupon')}>
        Aplicar cupom
      </Menu.Item>
    ),
    canWrite && live && s.discount?.status === 'active' && (
      <Menu.Item
        key="remove-discount"
        leftSection={<IconDiscount2 size={16} />}
        onClick={async () => {
          const ok = await confirmAction({
            title: 'Encerrar desconto',
            message: `Encerrar o desconto do cupom ${s.discount?.coupon_code}? As próximas faturas virão sem ele.`,
            confirmLabel: 'Encerrar desconto',
            danger: true,
          });
          if (ok) removeDiscount.mutate();
        }}
      >
        Encerrar desconto
      </Menu.Item>
    ),
    canWrite && (s.status === 'trialing' || s.status === 'active' || s.status === 'past_due') && (
      <Menu.Item key="suspend" color="orange" leftSection={<IconPlayerPause size={16} />} onClick={() => setAction('suspend')}>
        Suspender
      </Menu.Item>
    ),
    canWrite && s.status === 'suspended' && (
      <Menu.Item
        key="reactivate"
        leftSection={<IconPlayerPlay size={16} />}
        onClick={async () => {
          const ok = await confirmAction({ title: 'Reativar assinatura', message: 'A assinatura volta a gerar faturas a partir do próximo ciclo.', confirmLabel: 'Reativar' });
          if (ok) reactivate.mutate();
        }}
      >
        Reativar
      </Menu.Item>
    ),
    canCancel && live && s.cancel_at_period_end && (
      <Menu.Item
        key="resume"
        leftSection={<IconRotateClockwise size={16} />}
        onClick={async () => {
          const ok = await confirmAction({ title: 'Desfazer cancelamento agendado', message: 'A assinatura voltará a ser renovada normalmente.', confirmLabel: 'Desfazer cancelamento' });
          if (ok) resume.mutate();
        }}
      >
        Desfazer cancelamento agendado
      </Menu.Item>
    ),
    canCancel && live && (
      <Menu.Item key="cancel" color="red" leftSection={<IconBan size={16} />} onClick={() => setAction('cancel')}>
        Cancelar assinatura
      </Menu.Item>
    ),
  ].filter(Boolean);

  if (items.length === 0) return null;

  return (
    <>
      <Menu position="bottom-end" withArrow>
        <Menu.Target>
          <Button rightSection={<IconChevronDown size={16} />} loading={resume.isPending || reactivate.isPending || removeDiscount.isPending}>
            Ações
          </Button>
        </Menu.Target>
        <Menu.Dropdown>{items}</Menu.Dropdown>
      </Menu>
      <Modal opened={action !== null} onClose={close} title={action ? TITLES[action] : ''} size="lg">
        {action === 'cancel' && <CancelModal subscription={s} onClose={close} />}
        {action === 'suspend' && <SuspendModal subscription={s} onClose={close} />}
        {action === 'plan' && <ChangePlanModal subscription={s} onClose={close} />}
        {action === 'quantity' && <ChangeQuantityModal subscription={s} onClose={close} />}
        {action === 'coupon' && <ApplyCouponModal subscription={s} onClose={close} />}
      </Modal>
    </>
  );
}

export function CancelScheduledAlert({ subscription }: { subscription: Subscription }) {
  const { timeZone } = useTenant();
  if (!subscription.cancel_at_period_end || subscription.status === 'canceled') return null;
  return (
    <Alert color="orange" title="Cancelamento agendado">
      Esta assinatura não será renovada e termina em {formatInstantDate(subscription.current_period_end, timeZone)}.
      Use “Desfazer cancelamento agendado” no menu de ações para manter a renovação.
    </Alert>
  );
}
