import { zodResolver } from '@hookform/resolvers/zod';
import { Alert, Button, Group, Modal, SimpleGrid, Stack, Table, Text, Textarea } from '@mantine/core';
import { DatePickerInput } from '@mantine/dates';
import { IconInfoCircle } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useMemo } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { z } from 'zod';
import type { Dispute, RecoverySettlement, RecoverySettlementInput } from '../../api/types';
import { CivilDate, DateTime, Money, StatusBadge } from '../../components/format';
import { MoneyInput } from '../../components/inputs';
import { EmptyState, QueryContent } from '../../components/states';
import { confirmAction, LabeledSegmented } from '../../components/ui';
import { formatCivilDate, todayInTimeZone } from '../../lib/dates';
import { useApiMutation } from '../../lib/hooks';
import { useIdempotencyKey } from '../../lib/idempotency';
import { recoveryOutcomeLabels } from '../../lib/labels';
import { centsToInput, formatMoney } from '../../lib/money';
import { moneyField, requiredText } from '../../lib/validation';
import { usePermission, useTenant } from '../../session/SessionContext';
import { paymentsApi } from './api';
import { ResponsiveTable } from '../../components/ResponsiveTable';

type Outcome = RecoverySettlementInput['outcome'];

export function recoverySettlementSchema(max: number, currency: string) {
  return z.object({
    outcome: z.enum(['received', 'written_off']),
    amount: moneyField({ max, maxMessage: `O valor excede o saldo a recuperar (${formatMoney(max, currency)}).` }),
    effective_date: z.string().nullable().refine((v) => !!v, 'Informe a data efetiva.'),
    justification: requiredText(10, 2000, 'A justificativa'),
  });
}
type FormValues = z.input<ReturnType<typeof recoverySettlementSchema>>;
type Output = z.output<ReturnType<typeof recoverySettlementSchema>>;

function toInput(v: Output): RecoverySettlementInput {
  return { outcome: v.outcome as Outcome, amount: v.amount, effective_date: v.effective_date as string, justification: v.justification };
}

const confirmMessages: Record<Outcome, (amount: string) => string> = {
  received: (amount) =>
    `Registrar ${amount} como recebido do cliente fora do provedor? O valor sai de "valores a recuperar" e entra no disponível liquidado. A baixa não pode ser desfeita.`,
  written_off: (amount) =>
    `Baixar ${amount} como perdido? O valor deixa de ser cobrado do cliente e é reconhecido como perda com inadimplência. A baixa não pode ser desfeita.`,
};

function SettlementList({ dispute }: { dispute: Dispute }) {
  const { keys } = useTenant();
  const query = useQuery({
    queryKey: keys('disputes', 'recovery-settlements', dispute.id),
    queryFn: ({ signal }) => paymentsApi.listRecoverySettlements(dispute.id, signal),
  });
  return (
    <QueryContent query={query}>
      {({ data }) =>
        data.length === 0 ? (
          <EmptyState title="Nenhuma baixa" description="As baixas registradas aparecem aqui, com a justificativa." />
        ) : (
          <ResponsiveTable minWidth={560}>
            <Table aria-label="Baixas do valor a recuperar">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Data efetiva</Table.Th>
                  <Table.Th>Desfecho</Table.Th>
                  <Table.Th ta="right">Valor</Table.Th>
                  <Table.Th>Justificativa</Table.Th>
                  <Table.Th>Registrada em</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {data.map((s) => (
                  <Table.Tr key={s.id}>
                    <Table.Td>
                      <CivilDate value={s.effective_date} />
                    </Table.Td>
                    <Table.Td>
                      <StatusBadge status={s.outcome} labels={recoveryOutcomeLabels} />
                    </Table.Td>
                    <Table.Td ta="right">
                      <Money value={s.amount} currency={s.currency} />
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm" lineClamp={3} title={s.justification}>
                        {s.justification}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <DateTime value={s.created_at} />
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </ResponsiveTable>
        )
      }
    </QueryContent>
  );
}

function SettlementForm({ dispute, outstanding, onDone }: { dispute: Dispute; outstanding: number; onDone: () => void }) {
  const { timeZone } = useTenant();
  const today = todayInTimeZone(timeZone);
  const closedOn = dispute.closed_at ? todayInTimeZone(timeZone, new Date(dispute.closed_at)) : undefined;
  const idempotency = useIdempotencyKey();
  const schema = useMemo(() => recoverySettlementSchema(outstanding, dispute.currency), [outstanding, dispute.currency]);
  const form = useForm<FormValues, unknown, Output>({
    resolver: zodResolver(schema),
    defaultValues: { outcome: 'received', amount: centsToInput(outstanding, dispute.currency), effective_date: today, justification: '' },
  });
  const mutation = useApiMutation<RecoverySettlementInput, RecoverySettlement, FormValues>({
    mutationFn: (input) => paymentsApi.settleRecovery(dispute.id, input, idempotency.keyFor({ dispute: dispute.id, ...input })),
    invalidate: ['disputes', 'dashboard', 'ledger'],
    form,
    // Recusa da API (por exemplo, saldo baixado por outra pessoa) encerra a
    // intenção: o próximo envio usa outra chave.
    idempotency,
    successMessage: (s) =>
      s.outcome === 'received' ? 'Recebimento do valor a recuperar registrado.' : 'Valor a recuperar baixado como perdido.',
    onSuccess: () => {
      form.reset();
      onDone();
    },
  });
  const submit = form.handleSubmit(async (values) => {
    const input = toInput(values);
    const ok = await confirmAction({
      title: input.outcome === 'received' ? 'Confirmar recebimento' : 'Confirmar perda',
      message: confirmMessages[input.outcome](formatMoney(input.amount, dispute.currency)),
      confirmLabel: input.outcome === 'received' ? 'Registrar recebimento' : 'Baixar como perdido',
      danger: input.outcome === 'written_off',
    });
    if (ok) mutation.mutate(input);
  });
  const { register, control, formState } = form;
  return (
    <form onSubmit={submit} noValidate>
      <Stack>
        <Controller
          control={control}
          name="outcome"
          render={({ field }) => (
            <LabeledSegmented
              label="Desfecho"
              value={field.value}
              onChange={field.onChange}
              data={[
                { value: 'received', label: 'Recebido' },
                { value: 'written_off', label: 'Perdido' },
              ]}
            />
          )}
        />
        <MoneyInput
          label="Valor da baixa"
          description={`Saldo a recuperar: ${formatMoney(outstanding, dispute.currency)}. A baixa pode ser parcial.`}
          required
          {...register('amount')}
          error={formState.errors.amount?.message}
        />
        <Controller
          control={control}
          name="effective_date"
          render={({ field, fieldState }) => (
            <DatePickerInput
              label="Data efetiva"
              description={
                closedOn
                  ? `Data do recebimento ou da decisão, entre ${formatCivilDate(closedOn)} (encerramento da disputa) e hoje.`
                  : 'Data do recebimento ou da decisão, até hoje.'
              }
              required
              value={field.value}
              onChange={field.onChange}
              minDate={closedOn}
              maxDate={today}
              valueFormat="DD/MM/YYYY"
              error={fieldState.error?.message}
            />
          )}
        />
        <Textarea
          label="Justificativa"
          description="Registrada na auditoria e no histórico da baixa; não inclua dados pessoais do cliente."
          required
          autosize
          minRows={2}
          maxLength={2000}
          {...register('justification')}
          error={formState.errors.justification?.message}
        />
        <Group justify="flex-end">
          <Button type="submit" loading={mutation.isPending}>
            Continuar
          </Button>
        </Group>
      </Stack>
    </form>
  );
}

/**
 * Valor a recuperar do cliente na disputa perdida (ADR-070 e ADR-073): saldo,
 * baixas registradas e, com a permissão, o registro de uma nova baixa
 * (recebida ou perdida) com confirmação.
 */
export function RecoverySettlementModal({ dispute, opened, onClose }: { dispute: Dispute; opened: boolean; onClose: () => void }) {
  const canSettle = usePermission('payments.settle_recovery');
  const outstanding = dispute.recovery_outstanding_amount;
  return (
    <Modal opened={opened} onClose={onClose} title="Valor a recuperar do cliente" size="xl">
      <Stack>
        <SimpleGrid cols={{ base: 2, sm: 4 }}>
          <Summary label="A recuperar" value={dispute.recovery_amount} currency={dispute.currency} />
          <Summary label="Recebido" value={dispute.recovery_received_amount} currency={dispute.currency} />
          <Summary label="Perdido" value={dispute.recovery_written_off_amount} currency={dispute.currency} />
          <Summary label="Saldo a recuperar" value={outstanding} currency={dispute.currency} strong />
        </SimpleGrid>
        <SettlementList dispute={dispute} />
        {outstanding <= 0 ? (
          <Alert color="green" icon={<IconInfoCircle />}>
            O valor a recuperar desta disputa já foi baixado por completo.
          </Alert>
        ) : canSettle ? (
          <SettlementForm dispute={dispute} outstanding={outstanding} onDone={onClose} />
        ) : (
          <Alert color="gray" icon={<IconInfoCircle />}>
            Você pode consultar o saldo e as baixas, mas não tem permissão para dar baixa.
          </Alert>
        )}
      </Stack>
    </Modal>
  );
}

function Summary({ label, value, currency, strong }: { label: string; value: number; currency: string; strong?: boolean }) {
  return (
    <Stack gap={2}>
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Money value={value} currency={currency} strong={strong} />
    </Stack>
  );
}
