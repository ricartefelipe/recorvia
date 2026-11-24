import { zodResolver } from '@hookform/resolvers/zod';
import { Button, Group, Loader, Menu, Modal, Select, Stack, Text, Textarea } from '@mantine/core';
import { DateTimePicker } from '@mantine/dates';
import {
  IconBan,
  IconBook,
  IconCash,
  IconChevronDown,
  IconFileText,
  IconReceipt2,
  IconRefresh,
  IconTrashX,
} from '@tabler/icons-react';
import { useMediaQuery } from '@mantine/hooks';
import { useMemo, useState } from 'react';
import { Link } from 'react-router';
import { Controller, useForm } from 'react-hook-form';
import { z } from 'zod';
import type { Charge, InvoiceDetails, ManualPaymentInput, Payment } from '../../api/types';
import { DocumentViewer } from '../../components/DocumentViewer';
import { MoneyInput } from '../../components/inputs';
import { confirmAction } from '../../components/ui';
import { zonedWallTimeToUtc } from '../../lib/dates';
import { useApiMutation } from '../../lib/hooks';
import { useIdempotencyKey } from '../../lib/idempotency';
import { labelOf, optionsOf, paymentMethodLabels } from '../../lib/labels';
import { centsToInput, formatMoney } from '../../lib/money';
import { moneyField, optionalText, reasonField } from '../../lib/validation';
import { useSession, useTenant } from '../../session/SessionContext';
import { invoicesApi } from './api';

const INVALIDATE = ['invoices', 'charges', 'payments', 'customers', 'dashboard', 'ledger'] as const;
type Action = 'payment' | 'credit' | 'void' | 'uncollectible' | null;

const paymentSchema = z.object({
  amount: moneyField(),
  method: z.enum(['pix', 'bank_transfer', 'boleto', 'cash', 'other']),
  paid_at: z.string().nullable(),
  note: optionalText(500),
});

function ManualPaymentForm({ invoice, onClose }: { invoice: InvoiceDetails; onClose: () => void }) {
  const { timeZone } = useTenant();
  const idempotency = useIdempotencyKey();
  const form = useForm<z.input<typeof paymentSchema>, unknown, z.output<typeof paymentSchema>>({
    resolver: zodResolver(paymentSchema),
    defaultValues: { amount: centsToInput(invoice.amount_due, invoice.currency), method: 'pix', paid_at: null, note: '' },
  });
  const mutation = useApiMutation<ManualPaymentInput, Payment, z.input<typeof paymentSchema>>({
    mutationFn: (input) => invoicesApi.recordPayment(invoice.id, input, idempotency.keyFor(input)),
    invalidate: [...INVALIDATE],
    successMessage: 'Pagamento registrado.',
    form,
    idempotency,
    onSuccess: () => onClose(),
  });
  const submit = form.handleSubmit(async (v) => {
    const input: ManualPaymentInput = { amount: v.amount, method: v.method };
    if (v.paid_at) input.paid_at = zonedWallTimeToUtc(v.paid_at, timeZone);
    if (v.note) input.note = v.note;
    const ok = await confirmAction({
      title: 'Confirmar pagamento manual',
      message: `Registrar o recebimento de ${formatMoney(v.amount, invoice.currency)} via ${labelOf(paymentMethodLabels, v.method)} na fatura ${invoice.number ?? ''}?${v.amount > invoice.amount_due ? ' O valor acima do saldo vira crédito do cliente.' : ''}`,
      confirmLabel: 'Registrar pagamento',
    });
    if (ok) mutation.mutate(input);
  });
  return (
    <form onSubmit={submit} noValidate>
      <Stack>
        <Text size="sm">Saldo devedor: {formatMoney(invoice.amount_due, invoice.currency)}</Text>
        <MoneyInput label="Valor recebido" required {...form.register('amount')} error={form.formState.errors.amount?.message} />
        <Controller
          control={form.control}
          name="method"
          render={({ field }) => (
            <Select label="Forma de pagamento" required data={optionsOf(paymentMethodLabels)} value={field.value} onChange={(v) => v && field.onChange(v)} allowDeselect={false} />
          )}
        />
        <Controller
          control={form.control}
          name="paid_at"
          render={({ field }) => (
            <DateTimePicker
              label="Data do recebimento"
              description={`Vazio usa agora. Horário de ${timeZone}.`}
              clearable
              value={field.value}
              onChange={field.onChange}
              valueFormat="DD/MM/YYYY HH:mm"
            />
          )}
        />
        <Textarea label="Observação" autosize minRows={2} maxLength={500} {...form.register('note')} error={form.formState.errors.note?.message} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            Cancelar
          </Button>
          <Button type="submit" loading={mutation.isPending}>
            Continuar
          </Button>
        </Group>
      </Stack>
    </form>
  );
}

function CreditNoteForm({ invoice, onClose }: { invoice: InvoiceDetails; onClose: () => void }) {
  const idempotency = useIdempotencyKey();
  const max = Math.max(0, invoice.total - invoice.amount_credited);
  const schema = useMemo(
    () =>
      z.object({
        amount: moneyField({ max, maxMessage: `O valor excede o máximo creditável (${formatMoney(max, invoice.currency)}).` }),
        reason: reasonField,
      }),
    [max, invoice.currency],
  );
  const form = useForm<z.input<typeof schema>, unknown, z.output<typeof schema>>({
    resolver: zodResolver(schema),
    defaultValues: { amount: '', reason: '' },
  });
  const mutation = useApiMutation<z.output<typeof schema>, InvoiceDetails, z.input<typeof schema>>({
    mutationFn: (input) => invoicesApi.creditNote(invoice.id, input, idempotency.keyFor(input)),
    invalidate: [...INVALIDATE],
    successMessage: 'Nota de crédito emitida.',
    form,
    idempotency,
    onSuccess: () => onClose(),
  });
  const submit = form.handleSubmit(async (input) => {
    const ok = await confirmAction({
      title: 'Confirmar nota de crédito',
      message: `Emitir nota de crédito de ${formatMoney(input.amount, invoice.currency)} para a fatura ${invoice.number ?? ''}? A fatura não é reescrita; o crédito reduz o saldo e gera lançamento compensatório.`,
      confirmLabel: 'Emitir nota de crédito',
    });
    if (ok) mutation.mutate(input);
  });
  return (
    <form onSubmit={submit} noValidate>
      <Stack>
        <Text size="sm">Máximo creditável: {formatMoney(max, invoice.currency)}</Text>
        <MoneyInput label="Valor do crédito" required {...form.register('amount')} error={form.formState.errors.amount?.message} />
        <Textarea label="Motivo" required autosize minRows={2} maxLength={500} {...form.register('reason')} error={form.formState.errors.reason?.message} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            Cancelar
          </Button>
          <Button type="submit" loading={mutation.isPending}>
            Continuar
          </Button>
        </Group>
      </Stack>
    </form>
  );
}

const reasonSchema = z.object({ reason: reasonField });

function ReasonForm({ invoice, kind, onClose }: { invoice: InvoiceDetails; kind: 'void' | 'uncollectible'; onClose: () => void }) {
  const form = useForm<z.input<typeof reasonSchema>>({ resolver: zodResolver(reasonSchema), defaultValues: { reason: '' } });
  const mutation = useApiMutation<string, InvoiceDetails, z.input<typeof reasonSchema>>({
    mutationFn: (reason) => (kind === 'void' ? invoicesApi.void(invoice, reason) : invoicesApi.markUncollectible(invoice, reason)),
    invalidate: [...INVALIDATE],
    successMessage: kind === 'void' ? 'Fatura cancelada.' : 'Fatura baixada como incobrável.',
    form,
    onSuccess: () => onClose(),
  });
  const submit = form.handleSubmit(async ({ reason }) => {
    const ok = await confirmAction({
      title: kind === 'void' ? 'Cancelar fatura' : 'Baixar como incobrável',
      message:
        kind === 'void'
          ? `Cancelar a fatura ${invoice.number ?? ''}? Cobranças pendentes são canceladas e lançamentos compensatórios são registrados. A fatura continua no histórico.`
          : `Baixar o saldo de ${formatMoney(invoice.amount_due, invoice.currency)} como incobrável? O valor sai dos recebíveis e é registrado como perda.`,
      confirmLabel: kind === 'void' ? 'Cancelar fatura' : 'Baixar como incobrável',
      danger: true,
    });
    if (ok) mutation.mutate(reason);
  });
  return (
    <form onSubmit={submit} noValidate>
      <Stack>
        <Textarea label="Motivo" required autosize minRows={2} maxLength={500} {...form.register('reason')} error={form.formState.errors.reason?.message} />
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

const TITLES: Record<Exclude<Action, null>, string> = {
  payment: 'Registrar pagamento manual',
  credit: 'Emitir nota de crédito',
  void: 'Cancelar fatura',
  uncollectible: 'Baixar como incobrável',
};

/** Estado e visualizador do documento HTML da fatura. */
function useInvoiceDocument(invoice: InvoiceDetails) {
  const [state, setState] = useState<{ opened: boolean; html: string | null; loading: boolean; error: unknown }>({
    opened: false,
    html: null,
    loading: false,
    error: null,
  });
  const open = async () => {
    setState({ opened: true, html: null, loading: true, error: null });
    try {
      const html = await invoicesApi.document(invoice.id);
      setState({ opened: true, html, loading: false, error: null });
    } catch (error) {
      setState({ opened: true, html: null, loading: false, error });
    }
  };
  const viewer = (
    <DocumentViewer
      opened={state.opened}
      onClose={() => setState((s) => ({ ...s, opened: false }))}
      title={`Fatura ${invoice.number ?? ''}`}
      html={state.html}
      loading={state.loading}
      error={state.error}
      filename={`fatura-${invoice.number ?? invoice.id}.html`}
    />
  );
  return { open: () => void open(), viewer };
}

/**
 * Ações do cabeçalho da fatura: ledger, documento e ações financeiras,
 * conforme situação e permissões. No celular, tudo fica num único menu
 * "Ações", para o cabeçalho não se desalinhar.
 */
export function InvoiceActions({ invoice }: { invoice: InvoiceDetails }) {
  const { can } = useSession();
  const compact = useMediaQuery('(max-width: 47.99em)', false, { getInitialValueInEffect: false });
  const doc = useInvoiceDocument(invoice);
  const [action, setAction] = useState<Action>(null);
  const idempotency = useIdempotencyKey();
  const close = () => setAction(null);
  const receivable = invoice.status === 'open' || invoice.status === 'partially_paid';

  const retry = useApiMutation<void, Charge>({
    mutationFn: () => invoicesApi.retryCharge(invoice.id, idempotency.keyFor({ invoice: invoice.id, version: invoice.version })),
    invalidate: ['invoices', 'charges', 'payment-operations'],
    successMessage: 'Nova cobrança solicitada ao provedor.',
    idempotency,
  });

  const items = [
    can('payments.record_manual') && receivable && (
      <Menu.Item key="payment" leftSection={<IconCash size={16} />} onClick={() => setAction('payment')}>
        Registrar pagamento manual
      </Menu.Item>
    ),
    can('payments.charge') && receivable && (
      <Menu.Item
        key="retry"
        // O carregamento fica só neste item: o botão "Ações" continua
        // habilitado para abrir o documento e o ledger (menu único no celular).
        disabled={retry.isPending}
        leftSection={retry.isPending ? <Loader size={14} /> : <IconRefresh size={16} />}
        onClick={async () => {
          const ok = await confirmAction({
            title: 'Nova tentativa de cobrança',
            message: `Solicitar ao provedor uma nova cobrança de ${formatMoney(invoice.amount_due, invoice.currency)}? A cobrança anterior pendente é substituída.`,
            confirmLabel: 'Solicitar cobrança',
          });
          if (ok) retry.mutate();
        }}
      >
        Nova tentativa de cobrança
      </Menu.Item>
    ),
    can('invoices.write') && (receivable || invoice.status === 'paid') && (
      <Menu.Item key="credit" leftSection={<IconReceipt2 size={16} />} onClick={() => setAction('credit')}>
        Emitir nota de crédito
      </Menu.Item>
    ),
    can('invoices.void') && receivable && (
      <Menu.Item key="uncollectible" color="orange" leftSection={<IconTrashX size={16} />} onClick={() => setAction('uncollectible')}>
        Baixar como incobrável
      </Menu.Item>
    ),
    can('invoices.void') && (invoice.status === 'draft' || invoice.status === 'open') && (
      <Menu.Item key="void" color="red" leftSection={<IconBan size={16} />} onClick={() => setAction('void')}>
        Cancelar fatura
      </Menu.Item>
    ),
  ].filter(Boolean);

  const ledgerLink = can('ledger.read') ? `/ledger?tipo=invoice&ref=${invoice.id}` : null;
  const secondary = compact
    ? [
        <Menu.Item key="document" leftSection={<IconFileText size={16} />} onClick={doc.open}>
          Abrir documento
        </Menu.Item>,
        ledgerLink && (
          <Menu.Item key="ledger" component={Link} to={ledgerLink} leftSection={<IconBook size={16} />}>
            Ver no ledger
          </Menu.Item>
        ),
      ].filter(Boolean)
    : [];
  const menuItems = secondary.length && items.length ? [...secondary, <Menu.Divider key="divider" />, ...items] : [...secondary, ...items];

  return (
    <>
      {!compact && ledgerLink && (
        <Button component={Link} to={ledgerLink} variant="subtle" leftSection={<IconBook size={16} />}>
          Ver no ledger
        </Button>
      )}
      {!compact && (
        <Button variant="default" leftSection={<IconFileText size={16} />} onClick={doc.open}>
          Abrir documento
        </Button>
      )}
      {menuItems.length > 0 && (
        <Menu position="bottom-end" withArrow>
          <Menu.Target>
            <Button rightSection={<IconChevronDown size={16} />}>Ações</Button>
          </Menu.Target>
          <Menu.Dropdown>{menuItems}</Menu.Dropdown>
        </Menu>
      )}
      {doc.viewer}
      <Modal opened={action !== null} onClose={close} title={action ? TITLES[action] : ''} size="lg">
        {action === 'payment' && <ManualPaymentForm invoice={invoice} onClose={close} />}
        {action === 'credit' && <CreditNoteForm invoice={invoice} onClose={close} />}
        {(action === 'void' || action === 'uncollectible') && <ReasonForm invoice={invoice} kind={action} onClose={close} />}
      </Modal>
    </>
  );
}
