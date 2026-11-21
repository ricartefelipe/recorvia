import { Alert, Anchor, Badge, Button, Group, Paper, Select, Stack, Table, Tabs, Text, TextInput } from '@mantine/core';
import { IconInfoCircle } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { Link } from 'react-router';
import { api } from '../../api/client';
import type { CursorPage, DataList, LedgerAccount, LedgerTransaction } from '../../api/types';
import { DateTime, Money } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { CursorPager, PageHeader } from '../../components/ui';
import { useCursorPagination, useReturnFromEmptyPage, useUrlFilters } from '../../lib/hooks';
import { stableStringify } from '../../lib/idempotency';
import { labelOf, ledgerAccountKindLabels, ledgerReferenceLabels, optionsOf } from '../../lib/labels';
import { useTenant } from '../../session/SessionContext';

const ledgerApi = {
  accounts: (signal?: AbortSignal) => api.get<DataList<LedgerAccount>>('/api/v1/ledger/accounts', { signal }),
  transactions: (params: { reference_type?: string; reference_id?: string; cursor?: string; limit?: number }, signal?: AbortSignal) =>
    api.get<CursorPage<LedgerTransaction>>('/api/v1/ledger/transactions', { query: { ...params }, signal }),
};

const REFERENCE_ROUTES: Record<string, string> = { invoice: '/faturas/', customer_credit: '/clientes/' };

function Accounts() {
  const { keys } = useTenant();
  const query = useQuery({ queryKey: keys('ledger', 'accounts'), queryFn: ({ signal }) => ledgerApi.accounts(signal) });
  return (
    <QueryContent query={query}>
      {({ data }) =>
        data.length === 0 ? (
          <EmptyState title="Nenhuma conta movimentada" />
        ) : (
          <Table.ScrollContainer minWidth={760}>
            <Table aria-label="Contas do ledger">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Código</Table.Th>
                  <Table.Th>Conta</Table.Th>
                  <Table.Th>Natureza</Table.Th>
                  <Table.Th ta="right">Débitos</Table.Th>
                  <Table.Th ta="right">Créditos</Table.Th>
                  <Table.Th ta="right">Saldo</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {data.map((a) => (
                  <Table.Tr key={`${a.code}-${a.currency}`}>
                    <Table.Td ff="monospace">{a.code}</Table.Td>
                    <Table.Td>{a.name}</Table.Td>
                    <Table.Td>{labelOf(ledgerAccountKindLabels, a.kind)}</Table.Td>
                    <Table.Td ta="right">
                      <Money value={a.debits} currency={a.currency} />
                    </Table.Td>
                    <Table.Td ta="right">
                      <Money value={a.credits} currency={a.currency} />
                    </Table.Td>
                    <Table.Td ta="right">
                      <Money value={a.balance} currency={a.currency} strong />
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )
      }
    </QueryContent>
  );
}

function Transactions() {
  const { keys } = useTenant();
  const [filters, setFilters] = useUrlFilters(['tipo', 'ref'] as const);
  const params = { reference_type: filters.tipo || undefined, reference_id: filters.ref || undefined, limit: 25 };
  const pagination = useCursorPagination(stableStringify(params));
  const refValid = !params.reference_id || /^[0-9a-f-]{36}$/i.test(params.reference_id);
  const query = useQuery({
    queryKey: keys('ledger', 'transactions', { ...params, cursor: pagination.cursor }),
    queryFn: ({ signal }) => ledgerApi.transactions({ ...params, cursor: pagination.cursor }, signal),
    placeholderData: keepPreviousData,
    enabled: refValid,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);
  return (
    <Stack>
      <Group align="flex-end">
        <Select
          label="Tipo de referência"
          placeholder="Todos"
          clearable
          data={optionsOf(ledgerReferenceLabels)}
          value={filters.tipo || null}
          onChange={(v) => setFilters({ tipo: v })}
          w={240}
        />
        <TextInput
          label="Identificador da referência"
          placeholder="UUID"
          value={filters.ref}
          onChange={(e) => setFilters({ ref: e.currentTarget.value.trim() })}
          error={refValid ? undefined : 'Informe um UUID válido.'}
          w={340}
        />
        {(filters.tipo || filters.ref) && (
          <Button variant="subtle" onClick={() => setFilters({ tipo: null, ref: null })}>
            Limpar filtros
          </Button>
        )}
      </Group>
      <QueryContent query={query} loading={<LoadingState />}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhum lançamento" description="Lançamentos são gerados por faturas, pagamentos, reembolsos, disputas e ajustes." />
          ) : (
            <>
              <Stack gap="sm">
                {page.data.map((t) => {
                  const route = REFERENCE_ROUTES[t.reference_type];
                  return (
                    <Paper key={t.id} withBorder p="sm" radius="md">
                      <Group justify="space-between" mb="xs" wrap="wrap">
                        <Group gap="xs">
                          <Text fw={600} size="sm">
                            {t.description}
                          </Text>
                          <Badge variant="light">{t.kind}</Badge>
                          {t.reverses_transaction_id && (
                            <Badge variant="outline" color="orange">
                              Estorno
                            </Badge>
                          )}
                        </Group>
                        <Group gap="xs">
                          <Text size="xs" c="dimmed">
                            {labelOf(ledgerReferenceLabels, t.reference_type)}{' '}
                            {route ? (
                              <Anchor component={Link} to={`${route}${t.reference_id}`} size="xs">
                                {t.reference_id.slice(0, 8)}
                              </Anchor>
                            ) : (
                              t.reference_id.slice(0, 8)
                            )}
                          </Text>
                          <Text size="xs" c="dimmed">
                            <DateTime value={t.occurred_at} />
                          </Text>
                        </Group>
                      </Group>
                      <Table aria-label={`Partidas de ${t.description}`} withRowBorders={false} verticalSpacing={2}>
                        <Table.Thead>
                          <Table.Tr>
                            <Table.Th>Conta</Table.Th>
                            <Table.Th ta="right">Débito</Table.Th>
                            <Table.Th ta="right">Crédito</Table.Th>
                          </Table.Tr>
                        </Table.Thead>
                        <Table.Tbody>
                          {t.entries.map((e, i) => (
                            <Table.Tr key={`${t.id}-${i}`}>
                              <Table.Td ff="monospace" fz="sm">
                                {e.account}
                              </Table.Td>
                              <Table.Td ta="right">{e.direction === 'debit' ? <Money value={e.amount} currency={e.currency} /> : ''}</Table.Td>
                              <Table.Td ta="right">{e.direction === 'credit' ? <Money value={e.amount} currency={e.currency} /> : ''}</Table.Td>
                            </Table.Tr>
                          ))}
                        </Table.Tbody>
                      </Table>
                    </Paper>
                  );
                })}
              </Stack>
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

export function LedgerPage() {
  const [filters, setFilters] = useUrlFilters(['aba', 'tipo', 'ref'] as const);
  const tab = filters.aba === 'contas' ? 'contas' : filters.tipo || filters.ref ? 'lancamentos' : filters.aba || 'contas';
  return (
    <Stack gap={0}>
      <PageHeader title="Ledger" description="Registro operacional imutável de partidas dobradas, balanceado por moeda. Correções entram como lançamentos compensatórios." />
      <Alert variant="light" icon={<IconInfoCircle />} mb="md">
        O ledger operacional não substitui a contabilidade fiscal nem o reconhecimento contábil de receita.
      </Alert>
      <Tabs value={tab} onChange={(v) => setFilters({ aba: v })} keepMounted={false}>
        <Tabs.List mb="md">
          <Tabs.Tab value="contas">Contas e saldos</Tabs.Tab>
          <Tabs.Tab value="lancamentos">Lançamentos</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="contas">
          <Accounts />
        </Tabs.Panel>
        <Tabs.Panel value="lancamentos">
          <Transactions />
        </Tabs.Panel>
      </Tabs>
    </Stack>
  );
}
