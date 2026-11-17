import { Anchor, Button, Group, Modal, Select, Table, TextInput } from '@mantine/core';
import { useDebouncedValue } from '@mantine/hooks';
import { IconPlus, IconSearch } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { Link, useNavigate } from 'react-router';
import { DateTime, StatusBadge } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { CursorPager, LabeledSegmented, PageHeader } from '../../components/ui';
import { formatDocument } from '../../lib/documents';
import { useCursorPagination, useUrlFilters } from '../../lib/hooks';
import { stableStringify } from '../../lib/idempotency';
import { customerKindLabels, customerStatusLabels, labelOf } from '../../lib/labels';
import { Can, useTenant } from '../../session/SessionContext';
import { customersApi, type CustomerListParams } from './api';
import { CustomerForm } from './CustomerForm';

const SORT_OPTIONS = [
  { value: 'name', label: 'Nome (A–Z)' },
  { value: '-name', label: 'Nome (Z–A)' },
  { value: '-created_at', label: 'Cadastro mais recente' },
  { value: 'created_at', label: 'Cadastro mais antigo' },
];

export function CustomersPage() {
  const { keys } = useTenant();
  const navigate = useNavigate();
  const [filters, setFilters] = useUrlFilters(['q', 'situacao', 'ordem'] as const);
  const [search, setSearch] = useState(filters.q);
  const [debouncedSearch] = useDebouncedValue(search.trim(), 350);
  const [creating, setCreating] = useState(false);

  useEffect(() => {
    if (debouncedSearch !== filters.q) setFilters({ q: debouncedSearch });
  }, [debouncedSearch, filters.q, setFilters]);

  const statusFilter = filters.situacao || 'active';
  const params: CustomerListParams = {
    q: filters.q || undefined,
    status: statusFilter === 'all' ? undefined : (statusFilter as 'active' | 'archived'),
    sort: (filters.ordem || 'name') as CustomerListParams['sort'],
    limit: 25,
  };
  const pagination = useCursorPagination(stableStringify(params));
  const query = useQuery({
    queryKey: keys('customers', 'list', { ...params, cursor: pagination.cursor }),
    queryFn: ({ signal }) => customersApi.list({ ...params, cursor: pagination.cursor }, signal),
    placeholderData: keepPreviousData,
  });
  const filtered = !!params.q || statusFilter !== 'active';

  return (
    <>
      <PageHeader
        title="Clientes"
        description="Pessoas físicas e jurídicas que você cobra. Documentos são validados apenas estruturalmente."
        actions={
          <Can permission="customers.write">
            <Button leftSection={<IconPlus size={16} />} onClick={() => setCreating(true)}>
              Novo cliente
            </Button>
          </Can>
        }
      />
      <Group mb="md" align="flex-end" wrap="wrap">
        <TextInput
          label="Buscar"
          placeholder="Nome, documento ou identificador externo"
          leftSection={<IconSearch size={16} />}
          value={search}
          onChange={(e) => setSearch(e.currentTarget.value)}
          maxLength={100}
          w={{ base: '100%', sm: 340 }}
        />
        <LabeledSegmented
          label="Situação"
          value={statusFilter}
          onChange={(v) => setFilters({ situacao: v === 'active' ? null : v })}
          data={[
            { value: 'active', label: 'Ativos' },
            { value: 'archived', label: 'Arquivados' },
            { value: 'all', label: 'Todos' },
          ]}
        />
        <Select
          label="Ordenar por"
          data={SORT_OPTIONS}
          value={params.sort}
          onChange={(v) => setFilters({ ordem: v === 'name' ? null : v })}
          allowDeselect={false}
          w={220}
        />
      </Group>
      <QueryContent query={query} loading={<LoadingState />}>
        {(page) =>
          page.data.length === 0 ? (
            filtered ? (
              <EmptyState title="Nenhum cliente encontrado" description="Revise a busca ou os filtros aplicados." />
            ) : (
              <EmptyState
                title="Nenhum cliente cadastrado"
                description="Cadastre o primeiro cliente para criar assinaturas e emitir faturas."
                action={
                  <Can permission="customers.write">
                    <Button leftSection={<IconPlus size={16} />} onClick={() => setCreating(true)}>
                      Novo cliente
                    </Button>
                  </Can>
                }
              />
            )
          ) : (
            <>
              <Table.ScrollContainer minWidth={760}>
                <Table aria-label="Clientes">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Nome</Table.Th>
                      <Table.Th>Documento</Table.Th>
                      <Table.Th>Tipo</Table.Th>
                      <Table.Th>E-mail</Table.Th>
                      <Table.Th>Situação</Table.Th>
                      <Table.Th>Cadastro</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.data.map((c) => (
                      <Table.Tr key={c.id}>
                        <Table.Td>
                          <Anchor component={Link} to={`/clientes/${c.id}`} fw={500}>
                            {c.name}
                          </Anchor>
                        </Table.Td>
                        <Table.Td className="numeric">{formatDocument(c.document_number, c.document_type)}</Table.Td>
                        <Table.Td>{labelOf(customerKindLabels, c.kind)}</Table.Td>
                        <Table.Td>{c.email || '—'}</Table.Td>
                        <Table.Td>
                          <StatusBadge status={c.status} labels={customerStatusLabels} />
                        </Table.Td>
                        <Table.Td>
                          <DateTime value={c.created_at} dateOnly />
                        </Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
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
      <Modal opened={creating} onClose={() => setCreating(false)} title="Novo cliente" size="lg">
        <CustomerForm
          onCancel={() => setCreating(false)}
          onSaved={(customer) => {
            setCreating(false);
            navigate(`/clientes/${customer.id}`);
          }}
        />
      </Modal>
    </>
  );
}
