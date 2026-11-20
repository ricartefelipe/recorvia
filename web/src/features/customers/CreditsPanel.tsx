import { zodResolver } from '@hookform/resolvers/zod';
import { Anchor, Button, Group, Modal, Paper, Stack, Table, Text, Textarea } from '@mantine/core';
import { IconPlus } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useForm } from 'react-hook-form';
import { Link } from 'react-router';
import { z } from 'zod';
import type { CreditBalance, CustomerDetails } from '../../api/types';
import { DateTime, Money, StatusBadge } from '../../components/format';
import { MoneyInput } from '../../components/inputs';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { confirmAction } from '../../components/ui';
import { useApiMutation } from '../../lib/hooks';
import { useIdempotencyKey } from '../../lib/idempotency';
import { creditKindLabels } from '../../lib/labels';
import { formatMoney } from '../../lib/money';
import { moneyField, reasonField } from '../../lib/validation';
import { Can, useTenant } from '../../session/SessionContext';
import { customersApi } from './api';

const grantSchema = z.object({ amount: moneyField(), reason: reasonField });
type GrantForm = z.input<typeof grantSchema>;
type GrantInput = z.output<typeof grantSchema>;

function GrantCreditModal({ customer, currency, opened, onClose }: { customer: CustomerDetails; currency: string; opened: boolean; onClose: () => void }) {
  const idempotency = useIdempotencyKey();
  const form = useForm<GrantForm, unknown, GrantInput>({ resolver: zodResolver(grantSchema), defaultValues: { amount: '', reason: '' } });
  const mutation = useApiMutation<GrantInput, CreditBalance, GrantForm>({
    mutationFn: (input) => customersApi.grantCredit(customer.id, input, idempotency.keyFor(input)),
    invalidate: ['customers'],
    successMessage: 'Crédito concedido.',
    form,
    idempotency,
    onSuccess: () => {
      form.reset();
      onClose();
    },
  });
  const onSubmit = form.handleSubmit(async (input) => {
    const ok = await confirmAction({
      title: 'Confirmar concessão de crédito',
      message: `Conceder ${formatMoney(input.amount, currency)} de crédito para ${customer.name}? O valor será abatido das próximas faturas.`,
      confirmLabel: 'Conceder crédito',
    });
    if (ok) mutation.mutate(input);
  });
  return (
    <Modal opened={opened} onClose={onClose} title="Conceder crédito">
      <form onSubmit={onSubmit} noValidate>
        <Stack>
          <MoneyInput label="Valor" required {...form.register('amount')} error={form.formState.errors.amount?.message} />
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
    </Modal>
  );
}

export function CreditsPanel({ customer }: { customer: CustomerDetails }) {
  const { keys } = useTenant();
  const [granting, setGranting] = useState(false);
  const query = useQuery({
    queryKey: keys('customers', 'credits', customer.id),
    queryFn: ({ signal }) => customersApi.credits(customer.id, signal),
  });
  return (
    <QueryContent query={query} loading={<LoadingState />}>
      {(credits) => (
        <Stack>
          <Paper withBorder p="md" radius="md">
            <Group justify="space-between">
              <div>
                <Text size="sm" c="dimmed">
                  Saldo de crédito disponível
                </Text>
                <Text fz={28} fw={700}>
                  <Money value={credits.balance} currency={credits.currency} />
                </Text>
              </div>
              <Can permission="credits.write">
                <Button leftSection={<IconPlus size={16} />} onClick={() => setGranting(true)} disabled={customer.status !== 'active'}>
                  Conceder crédito
                </Button>
              </Can>
            </Group>
          </Paper>
          {credits.entries.length === 0 ? (
            <EmptyState title="Nenhum movimento de crédito" description="Concessões e usos de crédito em faturas aparecerão aqui." />
          ) : (
            <Table.ScrollContainer minWidth={640}>
              <Table aria-label="Movimentos de crédito">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Data</Table.Th>
                    <Table.Th>Tipo</Table.Th>
                    <Table.Th ta="right">Valor</Table.Th>
                    <Table.Th>Motivo</Table.Th>
                    <Table.Th>Fatura</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {credits.entries.map((e) => (
                    <Table.Tr key={e.id}>
                      <Table.Td>
                        <DateTime value={e.created_at} />
                      </Table.Td>
                      <Table.Td>
                        <StatusBadge status={e.kind} labels={creditKindLabels} />
                      </Table.Td>
                      <Table.Td ta="right">
                        <Money value={e.amount} currency={credits.currency} />
                      </Table.Td>
                      <Table.Td>{e.reason}</Table.Td>
                      <Table.Td>
                        {e.invoice_id ? (
                          <Anchor component={Link} to={`/faturas/${e.invoice_id}`}>
                            Ver fatura
                          </Anchor>
                        ) : (
                          '—'
                        )}
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )}
          <GrantCreditModal customer={customer} currency={credits.currency} opened={granting} onClose={() => setGranting(false)} />
        </Stack>
      )}
    </QueryContent>
  );
}
