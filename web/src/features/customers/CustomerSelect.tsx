import { Select, Stack, Text, type SelectProps } from '@mantine/core';
import { useDebouncedValue } from '@mantine/hooks';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { formatDocument } from '../../lib/documents';
import { useTenant } from '../../session/SessionContext';
import { customersApi } from './api';

/** Seleção de cliente com busca no servidor (nome, documento ou identificador externo). */
export function CustomerSelect({
  value,
  onChange,
  includeArchived,
  ...props
}: {
  value: string | null;
  onChange: (value: string | null) => void;
  includeArchived?: boolean;
} & Omit<SelectProps, 'value' | 'onChange' | 'data'>) {
  const { keys } = useTenant();
  const [search, setSearch] = useState('');
  const [debounced] = useDebouncedValue(search.trim(), 300);

  const list = useQuery({
    queryKey: keys('customers', 'select', { q: debounced, includeArchived }),
    queryFn: ({ signal }) =>
      customersApi.list({ q: debounced || undefined, status: includeArchived ? undefined : 'active', sort: 'name', limit: 20 }, signal),
    placeholderData: keepPreviousData,
  });
  const inList = !!value && !!list.data?.data.some((c) => c.id === value);
  const selected = useQuery({
    queryKey: keys('customers', 'detail', value),
    queryFn: ({ signal }) => customersApi.get(value as string, signal),
    enabled: !!value && !inList,
  });

  const { data, documents } = useMemo(() => {
    const items = [...(list.data?.data ?? [])];
    if (selected.data && !items.some((c) => c.id === selected.data.id)) items.unshift(selected.data);
    return {
      data: items.map((c) => ({ value: c.id, label: c.name })),
      documents: new Map(items.map((c) => [c.id, formatDocument(c.document_number, c.document_type)])),
    };
  }, [list.data, selected.data]);

  return (
    <Select
      searchable
      clearable
      placeholder="Busque por nome ou documento"
      nothingFoundMessage={list.isFetching ? 'Buscando…' : 'Nenhum cliente encontrado'}
      {...props}
      data={data}
      value={value}
      onChange={onChange}
      onSearchChange={setSearch}
      filter={({ options }) => options}
      renderOption={({ option }) => (
        <Stack gap={0}>
          <Text size="sm">{option.label}</Text>
          <Text size="xs" c="dimmed">
            {documents.get(option.value)}
          </Text>
        </Stack>
      )}
    />
  );
}
