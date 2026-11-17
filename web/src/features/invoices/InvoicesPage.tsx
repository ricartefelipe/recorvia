import { Anchor, Button, Group, List, Modal, Select, Stack, Switch, Text, Tooltip } from '@mantine/core';
import { DatePickerInput } from '@mantine/dates';
import { IconFileExport, IconPlayerPlay } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Link } from 'react-router';
import type { BillingRunResult, InvoiceStatus } from '../../api/types';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { confirmAction, CursorPager, PageHeader } from '../../components/ui';
import { todayInTimeZone } from '../../lib/dates';
import { handleMutationError } from '../../lib/errors';
import { useApiMutation, useCursorPagination, useUrlFilters } from '../../lib/hooks';
import { stableStringify, useIdempotencyKey } from '../../lib/idempotency';
import { invoiceStatusLabels, optionsOf } from '../../lib/labels';
import { notifySuccess } from '../../lib/notify';
import { Can, useTenant } from '../../session/SessionContext';
import { CustomerSelect } from '../customers/CustomerSelect';
import { downloadText, invoicesApi, type InvoiceListParams } from './api';
import { InvoicesTable } from './InvoicesTable';

function BillingRunButton() {
  const idempotency = useIdempotencyKey();
  const [result, setResult] = useState<BillingRunResult | null>(null);
  const mutation = useApiMutation<void, BillingRunResult>({
    mutationFn: () => invoicesApi.runBilling(idempotency.keyFor({ operation: 'billing-run' })),
    invalidate: ['invoices', 'subscriptions', 'charges', 'dashboard', 'ledger'],
    onSuccess: (r) => {
      idempotency.reset();
      setResult(r);
    },
  });
  const run = async () => {
    const ok = await confirmAction({
      title: 'Executar faturamento agora',
      message:
        'O motor de recorrência gera as faturas devidas de todas as assinaturas da organização e solicita as cobranças ao provedor. Faturas já geradas para o mesmo período não são duplicadas.',
      confirmLabel: 'Executar faturamento',
    });
    if (ok) mutation.mutate();
  };
  return (
    <>
      <Button leftSection={<IconPlayerPlay size={16} />} onClick={() => void run()} loading={mutation.isPending}>
        Executar faturamento agora
      </Button>
      <Modal opened={!!result} onClose={() => setResult(null)} title="Faturamento executado">
        {result && (
          <Stack>
            <Text>
              {result.subscriptions_processed} assinatura(s) processada(s) e {result.invoices_created} fatura(s) criada(s).
            </Text>
            {result.invoice_ids.length > 0 && (
              <List size="sm">
                {result.invoice_ids.slice(0, 20).map((id) => (
                  <List.Item key={id}>
                    <Anchor component={Link} to={`/faturas/${id}`} onClick={() => setResult(null)}>
                      Fatura {id.slice(0, 8)}
                    </Anchor>
                  </List.Item>
                ))}
              </List>
            )}
            <Group justify="flex-end">
              <Button onClick={() => setResult(null)}>Fechar</Button>
            </Group>
          </Stack>
        )}
      </Modal>
    </>
  );
}

export function InvoicesPage() {
  const { keys, timeZone } = useTenant();
  const [filters, setFilters] = useUrlFilters(['status', 'cliente', 'de', 'ate', 'overdue'] as const);
  const [exporting, setExporting] = useState(false);
  const params: InvoiceListParams = {
    status: (filters.status || undefined) as InvoiceStatus | undefined,
    customer_id: filters.cliente || undefined,
    issued_from: filters.de || undefined,
    issued_to: filters.ate || undefined,
    overdue: filters.overdue === 'true' ? true : undefined,
    limit: 25,
  };
  const pagination = useCursorPagination(stableStringify(params));
  const query = useQuery({
    queryKey: keys('invoices', 'list', { ...params, cursor: pagination.cursor }),
    queryFn: ({ signal }) => invoicesApi.list({ ...params, cursor: pagination.cursor }, signal),
    placeholderData: keepPreviousData,
  });
  const filtered = Object.entries(params).some(([k, v]) => k !== 'limit' && v !== undefined);

  const exportCsv = async () => {
    setExporting(true);
    try {
      const csv = await invoicesApi.exportCsv({ status: params.status, issued_from: params.issued_from, issued_to: params.issued_to });
      downloadText(csv, `faturas-${todayInTimeZone(timeZone)}.csv`, 'text/csv;charset=utf-8');
      notifySuccess('Arquivo CSV gerado.');
    } catch (error) {
      handleMutationError(error);
    } finally {
      setExporting(false);
    }
  };

  return (
    <Stack gap={0}>
      <PageHeader
        title="Faturas"
        description="Faturas emitidas não são reescritas: correções usam nota de crédito, cancelamento ou baixa, sempre com histórico."
        actions={
          <>
            <Can permission="invoices.export">
              <Tooltip label="Exporta conforme situação e período de emissão filtrados">
                <Button variant="default" leftSection={<IconFileExport size={16} />} onClick={() => void exportCsv()} loading={exporting}>
                  Exportar CSV
                </Button>
              </Tooltip>
            </Can>
            <Can permission="billing.run">
              <BillingRunButton />
            </Can>
          </>
        }
      />
      <Group mb="md" align="flex-end" wrap="wrap">
        <Select
          label="Situação"
          placeholder="Todas"
          clearable
          data={optionsOf(invoiceStatusLabels)}
          value={filters.status || null}
          onChange={(v) => setFilters({ status: v })}
          w={200}
        />
        <CustomerSelect label="Cliente" includeArchived value={filters.cliente || null} onChange={(v) => setFilters({ cliente: v })} w={300} />
        <DatePickerInput
          label="Emitidas de"
          clearable
          value={filters.de || null}
          onChange={(v) => setFilters({ de: v })}
          valueFormat="DD/MM/YYYY"
          w={160}
        />
        <DatePickerInput
          label="até"
          clearable
          value={filters.ate || null}
          onChange={(v) => setFilters({ ate: v })}
          valueFormat="DD/MM/YYYY"
          minDate={filters.de || undefined}
          w={160}
        />
        <Switch
          label="Somente vencidas"
          checked={filters.overdue === 'true'}
          onChange={(e) => setFilters({ overdue: e.currentTarget.checked ? 'true' : null })}
          mb={8}
        />
      </Group>
      <QueryContent query={query} loading={<LoadingState />}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState
              title={filtered ? 'Nenhuma fatura encontrada' : 'Nenhuma fatura emitida'}
              description={
                filtered
                  ? 'Revise os filtros aplicados.'
                  : 'As faturas são geradas pelo motor de recorrência a partir das assinaturas. Execute o faturamento para gerar as devidas agora.'
              }
            />
          ) : (
            <>
              <InvoicesTable invoices={page.data} />
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
