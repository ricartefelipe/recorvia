import { Button, Group, Paper, SimpleGrid, Stack, TextInput } from '@mantine/core';
import { DatePickerInput } from '@mantine/dates';
import { IconFilter, IconX } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { api } from '../../api/client';
import type { AuditEvent, CursorPage } from '../../api/types';
import { AuditEventList } from '../../components/AuditEventList';
import { AccessDenied, LoadingState, QueryContent } from '../../components/states';
import { CursorPager, PageHeader } from '../../components/ui';
import { useCursorPagination, useUrlFilters } from '../../lib/hooks';
import { stableStringify } from '../../lib/idempotency';
import { useTenant } from '../../session/SessionContext';

interface AuditParams {
  resource_type?: string;
  resource_id?: string;
  action?: string;
  actor_id?: string;
  from?: string;
  to?: string;
  cursor?: string;
  limit?: number;
}

const auditApi = {
  list: (params: AuditParams, signal?: AbortSignal) => api.get<CursorPage<AuditEvent>>('/api/v1/audit-events', { query: { ...params }, signal }),
};

const FILTER_KEYS = ['recurso', 'id', 'acao', 'ator', 'de', 'ate'] as const;

export function AuditPage() {
  const { keys, can } = useTenant();
  const [filters, setFilters] = useUrlFilters(FILTER_KEYS);
  const [draft, setDraft] = useState(filters);
  const params: AuditParams = {
    resource_type: filters.recurso || undefined,
    resource_id: filters.id || undefined,
    action: filters.acao || undefined,
    actor_id: filters.ator || undefined,
    from: filters.de || undefined,
    to: filters.ate || undefined,
    limit: 50,
  };
  const pagination = useCursorPagination(stableStringify(params));
  const query = useQuery({
    queryKey: keys('audit', 'list', { ...params, cursor: pagination.cursor }),
    queryFn: ({ signal }) => auditApi.list({ ...params, cursor: pagination.cursor }, signal),
    placeholderData: keepPreviousData,
    enabled: can('audit.read'),
  });

  if (!can('audit.read')) return <AccessDenied message="A trilha de auditoria exige a permissão “Ver auditoria”." />;

  const text = (key: (typeof FILTER_KEYS)[number], label: string, placeholder: string, maxLength: number) => (
    <TextInput label={label} placeholder={placeholder} maxLength={maxLength} value={draft[key]} onChange={(e) => setDraft({ ...draft, [key]: e.currentTarget.value })} />
  );

  return (
    <Stack gap={0}>
      <PageHeader title="Auditoria" description="Quem fez o quê, quando e em qual recurso. Os registros são imutáveis." />
      <Paper withBorder p="md" radius="md" mb="md">
        <form
          onSubmit={(e) => {
            e.preventDefault();
            setFilters(Object.fromEntries(FILTER_KEYS.map((k) => [k, draft[k].trim() || null])));
          }}
          aria-label="Filtros da auditoria"
        >
          <SimpleGrid cols={{ base: 1, sm: 3, lg: 6 }}>
            {text('recurso', 'Recurso', 'invoice, customer…', 64)}
            {text('id', 'Identificador do recurso', 'UUID', 64)}
            {text('acao', 'Ação', 'invoice.voided', 100)}
            {text('ator', 'Ator', 'Identificador do usuário', 64)}
            <DatePickerInput label="De" clearable value={draft.de || null} onChange={(v) => setDraft({ ...draft, de: v ?? '' })} valueFormat="DD/MM/YYYY" />
            <DatePickerInput
              label="Até"
              clearable
              value={draft.ate || null}
              onChange={(v) => setDraft({ ...draft, ate: v ?? '' })}
              valueFormat="DD/MM/YYYY"
              minDate={draft.de || undefined}
            />
          </SimpleGrid>
          <Group justify="flex-end" mt="sm">
            <Button
              variant="subtle"
              leftSection={<IconX size={16} />}
              onClick={() => {
                const empty = Object.fromEntries(FILTER_KEYS.map((k) => [k, ''])) as typeof draft;
                setDraft(empty);
                setFilters(Object.fromEntries(FILTER_KEYS.map((k) => [k, null])));
              }}
            >
              Limpar
            </Button>
            <Button type="submit" leftSection={<IconFilter size={16} />}>
              Aplicar filtros
            </Button>
          </Group>
        </form>
      </Paper>
      <QueryContent query={query} loading={<LoadingState />}>
        {(page) => (
          <>
            <AuditEventList events={page.data} />
            <CursorPager
              page={pagination.page}
              hasPrevious={pagination.hasPrevious}
              hasNext={!!page.next_cursor}
              onPrevious={pagination.previous}
              onNext={() => page.next_cursor && pagination.next(page.next_cursor)}
              loading={query.isFetching}
            />
          </>
        )}
      </QueryContent>
    </Stack>
  );
}
