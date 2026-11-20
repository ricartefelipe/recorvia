import { Anchor, Breadcrumbs, Button, Group, Paper, Select, SimpleGrid, Stack, Text } from '@mantine/core';
import { IconReceiptRefund } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Link, useParams } from 'react-router';
import type { ChargeStatus } from '../../api/types';
import { CivilDate, DateTime, Money, StatusBadge } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { CursorPager, KeyValue, PageHeader } from '../../components/ui';
import { useCursorPagination, useUrlFilters } from '../../lib/hooks';
import { stableStringify } from '../../lib/idempotency';
import { chargeStatusLabels, labelOf, optionsOf, providerLabels } from '../../lib/labels';
import { Can, useTenant } from '../../session/SessionContext';
import { canRequestRefund, paymentsApi, refundableAmount } from './api';
import { ChargesTable, CheckoutLink } from './ChargesTable';
import { RefundModal } from './RefundModal';

export function ChargesPage() {
  const { keys } = useTenant();
  const [filters, setFilters] = useUrlFilters(['status', 'fatura'] as const);
  const params = { status: (filters.status || undefined) as ChargeStatus | undefined, invoice_id: filters.fatura || undefined, limit: 25 };
  const pagination = useCursorPagination(stableStringify(params));
  const query = useQuery({
    queryKey: keys('charges', 'list', { ...params, cursor: pagination.cursor }),
    queryFn: ({ signal }) => paymentsApi.listCharges({ ...params, cursor: pagination.cursor }, signal),
    placeholderData: keepPreviousData,
  });
  return (
    <Stack gap={0}>
      <PageHeader
        title="Cobranças"
        description="Cobranças avulsas solicitadas ao provedor para cada fatura. Reembolsos respeitam o valor reembolsável de cada cobrança."
      />
      <Group mb="md" align="flex-end">
        <Select
          label="Situação"
          placeholder="Todas"
          clearable
          data={optionsOf(chargeStatusLabels)}
          value={filters.status || null}
          onChange={(v) => setFilters({ status: v })}
          w={240}
        />
        {filters.fatura && (
          <Button variant="subtle" onClick={() => setFilters({ fatura: null })}>
            Remover filtro de fatura
          </Button>
        )}
      </Group>
      <QueryContent query={query} loading={<LoadingState />}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhuma cobrança" description="As cobranças aparecem quando faturas são emitidas e enviadas ao provedor." />
          ) : (
            <>
              <ChargesTable charges={page.data} showInvoice />
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
    </Stack>
  );
}

export function ChargeDetailPage() {
  const { id = '' } = useParams();
  const { keys } = useTenant();
  const [refunding, setRefunding] = useState(false);
  const query = useQuery({ queryKey: keys('charges', 'detail', id), queryFn: ({ signal }) => paymentsApi.getCharge(id, signal) });
  return (
    <Stack gap="md">
      <Breadcrumbs>
        <Anchor component={Link} to="/cobrancas">
          Cobranças
        </Anchor>
        <Text>Tentativa {query.data?.attempt ?? ''}</Text>
      </Breadcrumbs>
      <QueryContent query={query} loading={<LoadingState variant="detail" />}>
        {(c) => (
          <>
            <PageHeader
              title={`Cobrança #${c.attempt}`}
              badge={<StatusBadge status={c.status} labels={chargeStatusLabels} size="lg" />}
              description={labelOf(providerLabels, c.provider)}
              actions={
                <Can permission="payments.refund">
                  {canRequestRefund(c) && (
                    <Button color="red" variant="light" leftSection={<IconReceiptRefund size={16} />} onClick={() => setRefunding(true)}>
                      Reembolsar
                    </Button>
                  )}
                </Can>
              }
            />
            <Paper withBorder p="md" radius="md">
              <SimpleGrid cols={{ base: 1, xs: 2, md: 4 }} spacing="lg">
                <KeyValue label="Fatura">
                  <Anchor component={Link} to={`/faturas/${c.invoice_id}`}>
                    Ver fatura
                  </Anchor>
                </KeyValue>
                <KeyValue label="Cliente">
                  <Anchor component={Link} to={`/clientes/${c.customer_id}`}>
                    Ver cliente
                  </Anchor>
                </KeyValue>
                <KeyValue label="Valor">
                  <Money value={c.amount} currency={c.currency} />
                </KeyValue>
                <KeyValue label="Pago">
                  <Money value={c.amount_paid} currency={c.currency} />
                </KeyValue>
                <KeyValue label="Taxa do provedor">
                  <Money value={c.fee_amount} currency={c.currency} />
                </KeyValue>
                <KeyValue label="Reembolsado">
                  <Money value={c.amount_refunded} currency={c.currency} />
                </KeyValue>
                <KeyValue label="Chargeback">
                  <Money value={c.amount_charged_back} currency={c.currency} />
                </KeyValue>
                <KeyValue label="Reembolsável">
                  <Money value={refundableAmount(c)} currency={c.currency} strong />
                </KeyValue>
                <KeyValue label="Vencimento">
                  <CivilDate value={c.due_date} />
                </KeyValue>
                <KeyValue label="Pago em">{c.paid_at ? <DateTime value={c.paid_at} /> : '—'}</KeyValue>
                <KeyValue label="Identificador no provedor">{c.provider_charge_id ?? '—'}</KeyValue>
                <KeyValue label="Situação no provedor">{c.provider_status || '—'}</KeyValue>
                <KeyValue label="Checkout hospedado">
                  <CheckoutLink url={c.checkout_url} />
                </KeyValue>
                {c.failure_reason && <KeyValue label="Motivo da falha">{c.failure_reason}</KeyValue>}
                <KeyValue label="Criada em">
                  <DateTime value={c.created_at} />
                </KeyValue>
              </SimpleGrid>
            </Paper>
            {refunding && <RefundModal charge={c} opened onClose={() => setRefunding(false)} />}
          </>
        )}
      </QueryContent>
    </Stack>
  );
}
