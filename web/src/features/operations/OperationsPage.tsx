import { zodResolver } from '@hookform/resolvers/zod';
import { Alert, Button, Code, Group, Modal, Select, Stack, Table, Text, Textarea } from '@mantine/core';
import { IconInfoCircle, IconPlayerPlay, IconTrash } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useForm } from 'react-hook-form';
import { z } from 'zod';
import { api } from '../../api/client';
import type { DataList, DeadLetter } from '../../api/types';
import { DateTime, StatusBadge } from '../../components/format';
import { EmptyState, QueryContent } from '../../components/states';
import { confirmAction, PageHeader } from '../../components/ui';
import { useApiMutation, useUrlFilters } from '../../lib/hooks';
import { deadLetterStatusLabels, optionsOf } from '../../lib/labels';
import { reasonField } from '../../lib/validation';
import { usePermission, useTenant } from '../../session/SessionContext';

const enc = encodeURIComponent;
type Decision = 'replay' | 'discard';

const operationsApi = {
  list: (status: string | undefined, signal?: AbortSignal) => api.get<DataList<DeadLetter>>('/api/v1/dead-letters', { query: { status }, signal }),
  decide: (id: string, decision: Decision, reason: string) => api.post<DeadLetter>(`/api/v1/dead-letters/${enc(id)}/${decision}`, { reason }),
};

const reasonSchema = z.object({ reason: reasonField });
type ReasonForm = z.input<typeof reasonSchema>;

function DecisionModal({ letter, decision, onClose }: { letter: DeadLetter; decision: Decision; onClose: () => void }) {
  const form = useForm<ReasonForm>({ resolver: zodResolver(reasonSchema), defaultValues: { reason: '' } });
  const mutation = useApiMutation<string, DeadLetter, ReasonForm>({
    mutationFn: (reason) => operationsApi.decide(letter.id, decision, reason),
    invalidate: ['dead-letters'],
    successMessage: decision === 'replay' ? 'Mensagem reenfileirada para processamento.' : 'Mensagem descartada.',
    form,
    onSuccess: onClose,
  });
  const submit = form.handleSubmit(async ({ reason }) => {
    const ok = await confirmAction({
      title: decision === 'replay' ? 'Reprocessar mensagem' : 'Descartar mensagem',
      message:
        decision === 'replay'
          ? 'A mensagem volta para a fila do consumidor. O processamento é idempotente, mas confirme que a causa da falha foi corrigida.'
          : 'A mensagem não será processada. A decisão e a justificativa ficam registradas na auditoria.',
      confirmLabel: decision === 'replay' ? 'Reprocessar' : 'Descartar',
      danger: decision === 'discard',
    });
    if (ok) mutation.mutate(reason);
  });
  return (
    <Modal opened onClose={onClose} title={decision === 'replay' ? 'Reprocessar mensagem' : 'Descartar mensagem'} size="lg">
      <form onSubmit={submit} noValidate>
        <Stack>
          <Text size="sm">
            Consumidor <Code>{letter.consumer}</Code>, evento <Code>{letter.event_type ?? '—'}</Code>
          </Text>
          <Alert color="red" variant="light" title="Erro registrado">
            <Text size="sm" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
              {letter.error}
            </Text>
          </Alert>
          <Textarea label="Justificativa" required autosize minRows={2} maxLength={500} {...form.register('reason')} error={form.formState.errors.reason?.message} />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" color={decision === 'discard' ? 'red' : 'blue'} loading={mutation.isPending}>
              Continuar
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

export function OperationsPage() {
  const { keys } = useTenant();
  const canReprocess = usePermission('operations.reprocess');
  const [filters, setFilters] = useUrlFilters(['situacao'] as const);
  const status = filters.situacao === 'todas' ? undefined : filters.situacao || 'open';
  const [deciding, setDeciding] = useState<{ letter: DeadLetter; decision: Decision } | null>(null);
  const query = useQuery({ queryKey: keys('dead-letters', 'list', status ?? 'all'), queryFn: ({ signal }) => operationsApi.list(status, signal) });
  return (
    <Stack gap={0}>
      <PageHeader
        title="Operações"
        description="Mensagens que esgotaram as tentativas de processamento (dead letters). Reprocessar e descartar exigem justificativa e são auditados."
      />
      {!canReprocess && (
        <Alert icon={<IconInfoCircle />} color="gray" mb="md">
          Você pode consultar as mensagens. Reprocessar ou descartar exige a permissão “Reprocessar ou descartar mensagens”.
        </Alert>
      )}
      <Group mb="md">
        <Select
          label="Situação"
          data={[...optionsOf(deadLetterStatusLabels), { value: 'todas', label: 'Todas' }]}
          value={filters.situacao || 'open'}
          onChange={(v) => setFilters({ situacao: v === 'open' ? null : v })}
          allowDeselect={false}
          w={200}
        />
      </Group>
      <QueryContent query={query}>
        {({ data }) =>
          data.length === 0 ? (
            <EmptyState title="Nenhuma mensagem" description={status === 'open' ? 'Nenhuma mensagem aguardando decisão.' : 'Nenhuma mensagem nesta situação.'} />
          ) : (
            <Table.ScrollContainer minWidth={1000}>
              <Table aria-label="Mensagens não processadas">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Falhou em</Table.Th>
                    <Table.Th>Consumidor</Table.Th>
                    <Table.Th>Evento</Table.Th>
                    <Table.Th>Erro</Table.Th>
                    <Table.Th ta="right">Tentativas</Table.Th>
                    <Table.Th>Situação</Table.Th>
                    {canReprocess && <Table.Th aria-label="Ações" />}
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {data.map((d) => (
                    <Table.Tr key={d.id}>
                      <Table.Td>
                        <DateTime value={d.failed_at} />
                      </Table.Td>
                      <Table.Td>
                        <Code>{d.consumer}</Code>
                      </Table.Td>
                      <Table.Td>
                        <Text size="sm" ff="monospace">
                          {d.event_type ?? '—'}
                        </Text>
                        <Text size="xs" c="dimmed" ff="monospace">
                          {d.message_id}
                        </Text>
                      </Table.Td>
                      <Table.Td maw={320}>
                        <Text size="xs" lineClamp={3} title={d.error}>
                          {d.error}
                        </Text>
                      </Table.Td>
                      <Table.Td ta="right">{d.attempts}</Table.Td>
                      <Table.Td>
                        <StatusBadge status={d.status} labels={deadLetterStatusLabels} />
                        {d.resolution_note && (
                          <Text size="xs" c="dimmed">
                            {d.resolution_note}
                          </Text>
                        )}
                      </Table.Td>
                      {canReprocess && (
                        <Table.Td>
                          {d.status === 'open' && (
                            <Group gap={4} wrap="nowrap">
                              <Button size="xs" variant="light" leftSection={<IconPlayerPlay size={14} />} onClick={() => setDeciding({ letter: d, decision: 'replay' })}>
                                Reprocessar
                              </Button>
                              <Button size="xs" variant="light" color="red" leftSection={<IconTrash size={14} />} onClick={() => setDeciding({ letter: d, decision: 'discard' })}>
                                Descartar
                              </Button>
                            </Group>
                          )}
                        </Table.Td>
                      )}
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )
        }
      </QueryContent>
      {deciding && <DecisionModal letter={deciding.letter} decision={deciding.decision} onClose={() => setDeciding(null)} />}
    </Stack>
  );
}
