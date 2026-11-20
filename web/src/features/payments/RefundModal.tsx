import { zodResolver } from '@hookform/resolvers/zod';
import { Alert, Button, Group, Modal, Stack, Text, Textarea } from '@mantine/core';
import { IconAlertTriangle } from '@tabler/icons-react';
import { useMemo } from 'react';
import { useForm } from 'react-hook-form';
import { z } from 'zod';
import type { Charge, Refund } from '../../api/types';
import { MoneyInput } from '../../components/inputs';
import { confirmAction } from '../../components/ui';
import { useApiMutation } from '../../lib/hooks';
import { useIdempotencyKey } from '../../lib/idempotency';
import { centsToInput, formatMoney } from '../../lib/money';
import { notifyError, notifySuccess } from '../../lib/notify';
import { moneyField, reasonField } from '../../lib/validation';
import { canRequestRefund, paymentsApi, refundableAmount } from './api';

export function refundSchema(max: number, currency: string) {
  return z.object({
    amount: moneyField({ max, maxMessage: `O valor excede o reembolsável (${formatMoney(max, currency)}).` }),
    reason: reasonField,
  });
}
type FormValues = z.input<ReturnType<typeof refundSchema>>;
type Output = z.output<ReturnType<typeof refundSchema>>;

/** Reembolso com valor limitado ao reembolsável e confirmação explícita. */
export function RefundModal({ charge, opened, onClose }: { charge: Charge; opened: boolean; onClose: () => void }) {
  const max = refundableAmount(charge);
  const allowed = canRequestRefund(charge);
  const idempotency = useIdempotencyKey();
  const schema = useMemo(() => refundSchema(max, charge.currency), [max, charge.currency]);
  const form = useForm<FormValues, unknown, Output>({
    resolver: zodResolver(schema),
    defaultValues: { amount: max > 0 ? centsToInput(max, charge.currency) : '', reason: '' },
  });
  const mutation = useApiMutation<Output, Refund, FormValues>({
    mutationFn: (input) => paymentsApi.refund(charge.id, input, idempotency.keyFor({ charge: charge.id, ...input })),
    invalidate: ['charges', 'refunds', 'invoices', 'payment-operations', 'dashboard', 'ledger'],
    form,
    // Recusa da API (por exemplo, saldo insuficiente) encerra a intenção: o
    // próximo envio usa outra chave e não repete a recusa registrada.
    idempotency,
    onSuccess: (r) => {
      onClose();
      // A API registra o pedido (201) também quando o provedor o recusa.
      if (r.status === 'failed') {
        notifyError('O provedor recusou o reembolso; o valor reservado voltou a ficar disponível.');
        return;
      }
      notifySuccess(
        r.status === 'succeeded'
          ? 'Reembolso concluído.'
          : r.status === 'unknown'
            ? 'Reembolso enviado; o provedor não confirmou o resultado. Acompanhe em Operações de pagamento.'
            : 'Reembolso solicitado ao provedor; a confirmação chega pelo provedor.',
      );
    },
  });
  const submit = form.handleSubmit(async (input) => {
    const ok = await confirmAction({
      title: 'Confirmar reembolso',
      message: `Reembolsar ${formatMoney(input.amount, charge.currency)} ao pagador? O provedor devolve o valor e a operação não pode ser desfeita.`,
      confirmLabel: 'Reembolsar',
      danger: true,
    });
    if (ok) mutation.mutate(input);
  });
  return (
    <Modal opened={opened} onClose={onClose} title="Reembolsar cobrança">
      <form onSubmit={submit} noValidate>
        <Stack>
          <Text size="sm">
            Pago: {formatMoney(charge.amount_paid, charge.currency)} · Já reembolsado: {formatMoney(charge.amount_refunded, charge.currency)} ·
            Reembolsável: <strong>{formatMoney(max, charge.currency)}</strong>
          </Text>
          {!allowed ? (
            <Alert color="orange" icon={<IconAlertTriangle />}>
              {max <= 0
                ? 'Não há valor reembolsável nesta cobrança.'
                : 'A cobrança não aceita reembolso nesta situação (por exemplo, durante uma disputa).'}
            </Alert>
          ) : (
            <>
              <MoneyInput label="Valor do reembolso" required {...form.register('amount')} error={form.formState.errors.amount?.message} />
              <Textarea label="Motivo" required autosize minRows={2} maxLength={500} {...form.register('reason')} error={form.formState.errors.reason?.message} />
            </>
          )}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" color="red" loading={mutation.isPending} disabled={!allowed}>
              Continuar
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
