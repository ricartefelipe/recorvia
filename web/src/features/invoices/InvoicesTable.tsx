import { Anchor, Badge, Group, Table } from '@mantine/core';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { Link } from 'react-router';
import type { Invoice } from '../../api/types';
import { CivilDate, DateTime, Money, StatusBadge } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { CursorPager } from '../../components/ui';
import { useCursorPagination, useReturnFromEmptyPage } from '../../lib/hooks';
import { stableStringify } from '../../lib/idempotency';
import { invoiceStatusLabels } from '../../lib/labels';
import { useTenant } from '../../session/SessionContext';
import { invoicesApi, type InvoiceListParams } from './api';

export function InvoiceStatusBadges({ invoice }: { invoice: Pick<Invoice, 'status' | 'overdue'> }) {
  return (
    <Group gap={4} wrap="nowrap">
      <StatusBadge status={invoice.status} labels={invoiceStatusLabels} />
      {invoice.overdue && (
        <Badge color="red" variant="filled" size="sm">
          Vencida
        </Badge>
      )}
    </Group>
  );
}

export function InvoicesTable({ invoices, showCustomer = true }: { invoices: Invoice[]; showCustomer?: boolean }) {
  return (
    <Table.ScrollContainer minWidth={900}>
      <Table aria-label="Faturas">
        <Table.Thead>
          <Table.Tr>
            <Table.Th>Número</Table.Th>
            {showCustomer && <Table.Th>Cliente</Table.Th>}
            <Table.Th>Situação</Table.Th>
            <Table.Th>Emissão</Table.Th>
            <Table.Th>Vencimento</Table.Th>
            <Table.Th ta="right">Total</Table.Th>
            <Table.Th ta="right">Saldo</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {invoices.map((i) => (
            <Table.Tr key={i.id}>
              <Table.Td>
                <Anchor component={Link} to={`/faturas/${i.id}`} fw={500}>
                  {i.number ?? 'Rascunho'}
                </Anchor>
              </Table.Td>
              {showCustomer && <Table.Td>{i.customer_name || '—'}</Table.Td>}
              <Table.Td>
                <InvoiceStatusBadges invoice={i} />
              </Table.Td>
              <Table.Td>{i.issued_at ? <DateTime value={i.issued_at} dateOnly /> : '—'}</Table.Td>
              <Table.Td>
                <CivilDate value={i.due_date} />
              </Table.Td>
              <Table.Td ta="right">
                <Money value={i.total} currency={i.currency} />
              </Table.Td>
              <Table.Td ta="right">
                <Money value={i.amount_due} currency={i.currency} strong={i.amount_due > 0} />
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
  );
}

/** Faturas filtradas por cliente ou assinatura, para abas de detalhe. */
export function RelatedInvoices({ params, emptyText }: { params: Pick<InvoiceListParams, 'customer_id' | 'subscription_id'>; emptyText: string }) {
  const { keys } = useTenant();
  const pagination = useCursorPagination(stableStringify(params));
  const query = useQuery({
    queryKey: keys('invoices', 'list', { ...params, cursor: pagination.cursor }),
    queryFn: ({ signal }) => invoicesApi.list({ ...params, limit: 25, cursor: pagination.cursor }, signal),
    placeholderData: keepPreviousData,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);
  return (
    <QueryContent query={query} loading={<LoadingState />}>
      {(page) =>
        page.data.length === 0 ? (
          <EmptyState title="Nenhuma fatura" description={emptyText} />
        ) : (
          <>
            <InvoicesTable invoices={page.data} showCustomer={!params.customer_id} />
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
  );
}
