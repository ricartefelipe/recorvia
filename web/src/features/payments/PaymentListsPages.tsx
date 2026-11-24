import { Alert, Anchor, Badge, Button, Code, Group, Select, Stack, Table, Text } from '@mantine/core';
import { IconAlertTriangle, IconReceiptRefund, IconRefreshDot } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Link } from 'react-router';
import type { Dispute, PaymentOperation } from '../../api/types';
import { DateTime, Money, StatusBadge } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { confirmAction, CursorPager, PageHeader } from '../../components/ui';
import { useApiMutation, useCursorPagination, useReturnFromEmptyPage, useUrlFilters } from '../../lib/hooks';
import {
  disputeStatusLabels,
  labelOf,
  optionsOf,
  paymentMethodLabels,
  paymentOperationKindLabels,
  paymentOperationStatusLabels,
  paymentSourceLabels,
  refundStatusLabels,
} from '../../lib/labels';
import { usePermission, useTenant } from '../../session/SessionContext';
import { paymentsApi, type PaymentOperationStatus } from './api';
import { RecoverySettlementModal } from './RecoverySettlementModal';
import { ResponsiveTable } from '../../components/ResponsiveTable';

function ChargeLink({ id }: { id: string }) {
  return (
    <Anchor component={Link} to={`/cobrancas/${id}`} size="sm">
      Cobrança {id.slice(0, 8)}
    </Anchor>
  );
}

export function PaymentsPage() {
  const { keys } = useTenant();
  const pagination = useCursorPagination('payments');
  const query = useQuery({
    queryKey: keys('payments', 'list', pagination.cursor),
    queryFn: ({ signal }) => paymentsApi.listPayments({ cursor: pagination.cursor, limit: 25 }, signal),
    placeholderData: keepPreviousData,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);
  return (
    <Stack gap={0}>
      <PageHeader title="Recebimentos" description="Pagamentos confirmados pelo provedor ou registrados manualmente, com as taxas descontadas." />
      <QueryContent query={query} loading={<LoadingState />}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhum recebimento" description="Pagamentos confirmados aparecem aqui." />
          ) : (
            <>
              <ResponsiveTable minWidth={860}>
                <Table aria-label="Recebimentos">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Pago em</Table.Th>
                      <Table.Th>Fatura</Table.Th>
                      <Table.Th ta="right">Valor</Table.Th>
                      <Table.Th ta="right">Taxa</Table.Th>
                      <Table.Th ta="right">Líquido</Table.Th>
                      <Table.Th>Forma</Table.Th>
                      <Table.Th>Origem</Table.Th>
                      <Table.Th>Observação</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.data.map((p) => (
                      <Table.Tr key={p.id}>
                        <Table.Td>
                          <DateTime value={p.paid_at} />
                        </Table.Td>
                        <Table.Td>
                          <Anchor component={Link} to={`/faturas/${p.invoice_id}`} size="sm">
                            Ver fatura
                          </Anchor>
                        </Table.Td>
                        <Table.Td ta="right">
                          <Money value={p.amount} currency={p.currency} />
                        </Table.Td>
                        <Table.Td ta="right">
                          <Money value={p.fee_amount} currency={p.currency} />
                        </Table.Td>
                        <Table.Td ta="right">
                          <Money value={p.amount - p.fee_amount} currency={p.currency} strong />
                        </Table.Td>
                        <Table.Td>{labelOf(paymentMethodLabels, p.method)}</Table.Td>
                        <Table.Td>{labelOf(paymentSourceLabels, p.source)}</Table.Td>
                        <Table.Td>{p.note || '—'}</Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </ResponsiveTable>
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

export function RefundsPage() {
  const { keys } = useTenant();
  const pagination = useCursorPagination('refunds');
  const query = useQuery({
    queryKey: keys('refunds', 'list', pagination.cursor),
    queryFn: ({ signal }) => paymentsApi.listRefunds({ cursor: pagination.cursor, limit: 25 }, signal),
    placeholderData: keepPreviousData,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);
  return (
    <Stack gap={0}>
      <PageHeader title="Reembolsos" description="Reembolsos solicitados ao provedor. Resultado desconhecido é resolvido por reconsulta ou conciliação." />
      <QueryContent query={query} loading={<LoadingState />}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhum reembolso" description="Reembolsos são feitos a partir de uma cobrança paga." />
          ) : (
            <>
              <ResponsiveTable minWidth={820}>
                <Table aria-label="Reembolsos">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Solicitado em</Table.Th>
                      <Table.Th>Cobrança</Table.Th>
                      <Table.Th ta="right">Valor</Table.Th>
                      <Table.Th>Situação</Table.Th>
                      <Table.Th>Motivo</Table.Th>
                      <Table.Th>Identificador no provedor</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.data.map((r) => (
                      <Table.Tr key={r.id} bg={r.status === 'unknown' ? 'red.0' : undefined}>
                        <Table.Td>
                          <DateTime value={r.created_at} />
                        </Table.Td>
                        <Table.Td>
                          <ChargeLink id={r.charge_id} />
                        </Table.Td>
                        <Table.Td ta="right">
                          <Money value={r.amount} currency={r.currency} />
                        </Table.Td>
                        <Table.Td>
                          <StatusBadge status={r.status} labels={refundStatusLabels} />
                        </Table.Td>
                        <Table.Td>{r.reason}</Table.Td>
                        <Table.Td>{r.provider_refund_id ? <Code>{r.provider_refund_id}</Code> : '—'}</Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </ResponsiveTable>
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

export function DisputesPage() {
  const { keys } = useTenant();
  const canSettle = usePermission('payments.settle_recovery');
  const [selected, setSelected] = useState<Dispute | null>(null);
  const pagination = useCursorPagination('disputes');
  const query = useQuery({
    queryKey: keys('disputes', 'list', pagination.cursor),
    queryFn: ({ signal }) => paymentsApi.listDisputes({ cursor: pagination.cursor, limit: 25 }, signal),
    placeholderData: keepPreviousData,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);
  return (
    <Stack gap={0}>
      <PageHeader
        title="Disputas e chargebacks"
        description="Contestações abertas pelo pagador junto ao provedor, inclusive as que chegam tardiamente. Na disputa perdida de um pagamento com excedente, o crédito gerado pelo excedente é consumido; a parte que o cliente já usou fica como valor a recuperar, baixado como recebido ou perdido."
      />
      <QueryContent query={query}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhuma disputa" description="Contestações informadas pelo provedor aparecem aqui." />
          ) : (
            <>
              <ResponsiveTable minWidth={1100}>
                <Table aria-label="Disputas">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Aberta em</Table.Th>
                      <Table.Th>Cobrança</Table.Th>
                      <Table.Th ta="right">Valor contestado</Table.Th>
                      <Table.Th>Situação</Table.Th>
                      <Table.Th ta="right">Crédito consumido</Table.Th>
                      <Table.Th ta="right">A recuperar do cliente</Table.Th>
                      <Table.Th ta="right">Saldo a recuperar</Table.Th>
                      <Table.Th>Encerrada em</Table.Th>
                      <Table.Th aria-label="Ações" />
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.data.map((d) => {
                      const outstanding = d.recovery_outstanding_amount;
                      return (
                        <Table.Tr key={d.id}>
                          <Table.Td>
                            <DateTime value={d.opened_at} />
                          </Table.Td>
                          <Table.Td>
                            <ChargeLink id={d.charge_id} />
                          </Table.Td>
                          <Table.Td ta="right">
                            <Money value={d.amount} currency={d.currency} />
                          </Table.Td>
                          <Table.Td>
                            <StatusBadge status={d.status} labels={disputeStatusLabels} />
                          </Table.Td>
                          <Table.Td ta="right">{d.credit_amount > 0 ? <Money value={d.credit_amount} currency={d.currency} /> : '—'}</Table.Td>
                          <Table.Td ta="right">{d.recovery_amount > 0 ? <Money value={d.recovery_amount} currency={d.currency} /> : '—'}</Table.Td>
                          <Table.Td ta="right">
                            {d.recovery_amount > 0 ? <Money value={outstanding} currency={d.currency} strong={outstanding > 0} /> : '—'}
                          </Table.Td>
                          <Table.Td>{d.closed_at ? <DateTime value={d.closed_at} /> : '—'}</Table.Td>
                          <Table.Td>
                            {d.recovery_amount > 0 && (
                              <Button
                                size="xs"
                                variant="light"
                                leftSection={<IconReceiptRefund size={14} />}
                                onClick={() => setSelected(d)}
                                aria-label={`${canSettle && outstanding > 0 ? 'Dar baixa' : 'Ver baixas'} da disputa da cobrança ${d.charge_id.slice(0, 8)}`}
                              >
                                {canSettle && outstanding > 0 ? 'Dar baixa' : 'Ver baixas'}
                              </Button>
                            )}
                          </Table.Td>
                        </Table.Tr>
                      );
                    })}
                  </Table.Tbody>
                </Table>
              </ResponsiveTable>
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
      {selected && <RecoverySettlementModal dispute={selected} opened onClose={() => setSelected(null)} />}
    </Stack>
  );
}

export function PaymentOperationsPage() {
  const { keys } = useTenant();
  const canRetry = usePermission('payments.charge');
  const [filters, setFilters] = useUrlFilters(['status'] as const);
  const status = (filters.status || undefined) as PaymentOperationStatus | undefined;
  // O filtro volta à primeira página.
  const pagination = useCursorPagination(`payment-operations:${status ?? 'all'}`);
  const query = useQuery({
    queryKey: keys('payment-operations', 'list', status ?? 'all', pagination.cursor),
    queryFn: ({ signal }) => paymentsApi.listOperations({ status, cursor: pagination.cursor, limit: 25 }, signal),
    placeholderData: keepPreviousData,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);
  // Aviso das operações com resultado desconhecido: a primeira página com o
  // limite máximo; havendo mais, o aviso informa que são pelo menos essas.
  const unknownQuery = useQuery({
    queryKey: keys('payment-operations', 'unknown-alert'),
    queryFn: ({ signal }) => paymentsApi.listOperations({ status: 'unknown', limit: 100 }, signal),
  });
  const unknownCount = unknownQuery.data?.data.length ?? 0;
  const unknownLabel = unknownQuery.data?.next_cursor ? `${unknownCount} ou mais` : String(unknownCount);
  const retry = useApiMutation<PaymentOperation, PaymentOperation>({
    mutationFn: (op) => paymentsApi.retryOperation(op.id),
    invalidate: ['payment-operations', 'charges', 'refunds', 'invoices', 'dashboard'],
    successMessage: (op) => `Reconsulta concluída: ${labelOf(paymentOperationStatusLabels, op.status).toLowerCase()}.`,
  });
  const onRetry = async (op: PaymentOperation) => {
    const ok = await confirmAction({
      title: 'Reconsultar provedor',
      message: `Consultar agora o provedor pela referência ${op.external_reference}, sem esperar a próxima tentativa? Se a operação já tiver sido executada no provedor, o resultado é vinculado; se não tiver, é executada uma única vez com esta referência, desde que a cobrança ainda esteja pendente (cobrança cancelada não é criada no provedor).`,
      confirmLabel: 'Reconsultar',
    });
    if (ok) retry.mutate(op);
  };
  return (
    <Stack gap={0}>
      <PageHeader
        title="Operações de pagamento"
        description="Chamadas externas ao provedor registradas antes do envio. Quando a resposta se perde, o resultado fica desconhecido até ser reconsultado."
      />
      {unknownCount > 0 && (
        <Alert color="red" icon={<IconAlertTriangle />} title={`${unknownLabel} operação(ões) com resultado desconhecido`} mb="md">
          O provedor pode ter executado a operação. Reconsulte antes de qualquer nova tentativa: o Recorvia não cria cobrança às
          cegas.{' '}
          {status !== 'unknown' && (
            <Anchor component="button" type="button" onClick={() => setFilters({ status: 'unknown' })} fw={600}>
              Ver somente estas
            </Anchor>
          )}
        </Alert>
      )}
      <Group mb="md">
        <Select
          label="Situação"
          placeholder="Todas"
          clearable
          data={optionsOf(paymentOperationStatusLabels)}
          value={status ?? null}
          onChange={(v) => setFilters({ status: v })}
          w={240}
        />
      </Group>
      <QueryContent query={query}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhuma operação" description={status ? 'Nenhuma operação nesta situação.' : 'As operações aparecem ao criar cobranças e reembolsos.'} />
          ) : (
            <>
              <ResponsiveTable minWidth={980}>
                <Table aria-label="Operações de pagamento">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Tipo</Table.Th>
                      <Table.Th>Situação</Table.Th>
                      <Table.Th>Referência externa</Table.Th>
                      <Table.Th>Cobrança</Table.Th>
                      <Table.Th ta="right">Tentativas</Table.Th>
                      <Table.Th>Próxima tentativa</Table.Th>
                      <Table.Th>Último erro</Table.Th>
                      <Table.Th>Atualizada em</Table.Th>
                      {canRetry && <Table.Th aria-label="Ações" />}
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.data.map((op) => (
                      <Table.Tr key={op.id} bg={op.status === 'unknown' ? 'red.0' : undefined}>
                        <Table.Td>{labelOf(paymentOperationKindLabels, op.kind)}</Table.Td>
                        <Table.Td>
                          {op.status === 'unknown' ? (
                            <Badge color="red" variant="filled">
                              {labelOf(paymentOperationStatusLabels, op.status)}
                            </Badge>
                          ) : (
                            <StatusBadge status={op.status} labels={paymentOperationStatusLabels} />
                          )}
                        </Table.Td>
                        <Table.Td>
                          <Code>{op.external_reference}</Code>
                        </Table.Td>
                        <Table.Td>
                          <ChargeLink id={op.charge_id} />
                        </Table.Td>
                        <Table.Td ta="right">{op.attempts}</Table.Td>
                        <Table.Td>
                          <DateTime value={op.next_attempt_at} />
                        </Table.Td>
                        <Table.Td>
                          <Text size="xs" lineClamp={2} title={op.last_error}>
                            {op.last_error || '—'}
                          </Text>
                        </Table.Td>
                        <Table.Td>
                          <DateTime value={op.updated_at} />
                        </Table.Td>
                        {canRetry && (
                          <Table.Td>
                            {op.status === 'unknown' && (
                              <Button
                                size="xs"
                                variant="light"
                                leftSection={<IconRefreshDot size={14} />}
                                onClick={() => void onRetry(op)}
                                loading={retry.isPending && retry.variables?.id === op.id}
                              >
                                Reconsultar
                              </Button>
                            )}
                          </Table.Td>
                        )}
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </ResponsiveTable>
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
