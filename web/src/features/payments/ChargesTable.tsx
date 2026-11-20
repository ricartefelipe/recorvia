import { Anchor, Button, Group, Table, Text } from '@mantine/core';
import { IconExternalLink, IconReceiptRefund } from '@tabler/icons-react';
import { useState } from 'react';
import { Link } from 'react-router';
import type { Charge } from '../../api/types';
import { CivilDate, Money, StatusBadge } from '../../components/format';
import { chargeStatusLabels, labelOf, providerLabels } from '../../lib/labels';
import { usePermission } from '../../session/SessionContext';
import { canRequestRefund } from './api';
import { RefundModal } from './RefundModal';

export function CheckoutLink({ url }: { url: string | null | undefined }) {
  if (!url) return <Text size="sm" c="dimmed">—</Text>;
  return (
    <Anchor href={url} target="_blank" rel="noopener noreferrer" size="sm">
      <Group gap={4} wrap="nowrap" component="span">
        Abrir checkout
        <IconExternalLink size={14} aria-hidden />
        <span className="visually-hidden">(abre em nova aba)</span>
      </Group>
    </Anchor>
  );
}

export function ChargesTable({ charges, showInvoice = false }: { charges: Charge[]; showInvoice?: boolean }) {
  const canRefund = usePermission('payments.refund');
  const [refunding, setRefunding] = useState<Charge | null>(null);
  return (
    <>
      <Table.ScrollContainer minWidth={960}>
        <Table aria-label="Cobranças">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Tentativa</Table.Th>
              {showInvoice && <Table.Th>Fatura</Table.Th>}
              <Table.Th>Provedor</Table.Th>
              <Table.Th>Situação</Table.Th>
              <Table.Th ta="right">Valor</Table.Th>
              <Table.Th ta="right">Pago</Table.Th>
              <Table.Th ta="right">Reembolsado</Table.Th>
              <Table.Th>Vencimento</Table.Th>
              <Table.Th>Checkout hospedado</Table.Th>
              <Table.Th aria-label="Ações" />
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {charges.map((c) => (
              <Table.Tr key={c.id}>
                <Table.Td>
                  <Anchor component={Link} to={`/cobrancas/${c.id}`}>
                    #{c.attempt}
                  </Anchor>
                </Table.Td>
                {showInvoice && (
                  <Table.Td>
                    <Anchor component={Link} to={`/faturas/${c.invoice_id}`}>
                      Ver fatura
                    </Anchor>
                  </Table.Td>
                )}
                <Table.Td>{labelOf(providerLabels, c.provider)}</Table.Td>
                <Table.Td>
                  <StatusBadge status={c.status} labels={chargeStatusLabels} />
                  {c.failure_reason && (
                    <Text size="xs" c="red">
                      {c.failure_reason}
                    </Text>
                  )}
                </Table.Td>
                <Table.Td ta="right">
                  <Money value={c.amount} currency={c.currency} />
                </Table.Td>
                <Table.Td ta="right">
                  <Money value={c.amount_paid} currency={c.currency} />
                </Table.Td>
                <Table.Td ta="right">
                  <Money value={c.amount_refunded} currency={c.currency} />
                </Table.Td>
                <Table.Td>
                  <CivilDate value={c.due_date} />
                </Table.Td>
                <Table.Td>{c.status === 'awaiting_payment' || c.status === 'pending' ? <CheckoutLink url={c.checkout_url} /> : '—'}</Table.Td>
                <Table.Td>
                  {canRefund && canRequestRefund(c) && (
                    <Button size="xs" variant="light" color="red" leftSection={<IconReceiptRefund size={14} />} onClick={() => setRefunding(c)}>
                      Reembolsar
                    </Button>
                  )}
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
      {refunding && <RefundModal charge={refunding} opened onClose={() => setRefunding(null)} />}
    </>
  );
}
