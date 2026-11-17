import { zodResolver } from '@hookform/resolvers/zod';
import { Alert, Badge, Button, Code, Group, Modal, NumberInput, Radio, SimpleGrid, Stack, Table, Text, TextInput, Tooltip } from '@mantine/core';
import { IconInfoCircle, IconPlus } from '@tabler/icons-react';
import { useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { z } from 'zod';
import type { Aggregation, Metric, MetricInput } from '../../api/types';
import { StatusBadge } from '../../components/format';
import { EmptyState, QueryContent } from '../../components/states';
import { useApiMutation } from '../../lib/hooks';
import { aggregationHelp, aggregationLabels, catalogStatusLabels, labelOf } from '../../lib/labels';
import { formatQuantity } from '../../lib/money';
import { optionalInt, quantityField, requiredText } from '../../lib/validation';
import { Can } from '../../session/SessionContext';
import { catalogApi, useMetrics } from './api';

const AGGREGATIONS: Aggregation[] = ['sum', 'max', 'unique_count', 'last'];

const metricSchema = z.object({
  code: z
    .string()
    .trim()
    .regex(/^[a-z0-9][a-z0-9_.-]{1,63}$/, 'Use de 2 a 64 caracteres: letras minúsculas, números, ponto, hífen ou sublinhado.'),
  name: requiredText(1, 200, 'O nome'),
  unit: z.string().trim().regex(/^[a-z0-9_]{1,32}$/, 'Use até 32 letras minúsculas, números ou sublinhado (ex.: api_calls).'),
  aggregation: z.enum(['sum', 'max', 'unique_count', 'last']),
  max_quantity_per_event: quantityField(),
  late_grace_hours: optionalInt(0, 720),
});
type MetricForm = z.input<typeof metricSchema>;

function MetricModal({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const form = useForm<MetricForm, unknown, MetricInput>({
    resolver: zodResolver(metricSchema),
    defaultValues: { code: '', name: '', unit: '', aggregation: 'sum', max_quantity_per_event: '', late_grace_hours: '' },
  });
  const mutation = useApiMutation<MetricInput, Metric, MetricForm>({
    mutationFn: catalogApi.createMetric,
    invalidate: ['metrics'],
    successMessage: 'Métrica criada.',
    form,
    onSuccess: () => {
      form.reset();
      onClose();
    },
  });
  const { register, control, formState } = form;
  return (
    <Modal opened={opened} onClose={onClose} title="Nova métrica de consumo" size="lg">
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
        <Stack>
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <TextInput label="Código" description="Usado na ingestão de eventos (metric_code)." placeholder="api.calls" required {...register('code')} error={formState.errors.code?.message} />
            <TextInput label="Nome" required {...register('name')} error={formState.errors.name?.message} />
            <TextInput label="Unidade" placeholder="api_calls" required {...register('unit')} error={formState.errors.unit?.message} />
            <TextInput
              label="Quantidade máxima por evento"
              description="Eventos acima deste valor são rejeitados."
              inputMode="decimal"
              required
              {...register('max_quantity_per_event')}
              error={formState.errors.max_quantity_per_event?.message}
            />
            <Controller
              control={control}
              name="late_grace_hours"
              render={({ field, fieldState }) => (
                <NumberInput
                  label="Carência para eventos atrasados (horas)"
                  description="Após o fechamento do período, eventos dentro da carência viram ajuste na próxima fatura."
                  min={0}
                  max={720}
                  allowDecimal={false}
                  value={field.value}
                  onChange={(v) => field.onChange(v === '' ? '' : Number(v))}
                  error={fieldState.error?.message}
                />
              )}
            />
          </SimpleGrid>
          <Controller
            control={control}
            name="aggregation"
            render={({ field }) => (
              <Radio.Group label="Agregação no período" required value={field.value} onChange={field.onChange}>
                <Stack gap="xs" mt="xs">
                  {AGGREGATIONS.map((a) => (
                    <Radio key={a} value={a} label={aggregationLabels[a]} description={aggregationHelp[a]} />
                  ))}
                </Stack>
              </Radio.Group>
            )}
          />
          <Alert variant="light" icon={<IconInfoCircle />}>
            Métricas não podem ser editadas depois de criadas, pois definem como o consumo já registrado é agregado.
          </Alert>
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Criar métrica
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

export function MetricsPage() {
  const query = useMetrics();
  const [creating, setCreating] = useState(false);
  return (
    <Stack>
      <Group justify="space-between">
        <Text size="sm" c="dimmed" maw={720}>
          Métricas definem o que é medido nos planos por consumo. A agregação determina como os eventos do período viram
          a quantidade faturada.
        </Text>
        <Can permission="catalog.write">
          <Button leftSection={<IconPlus size={16} />} onClick={() => setCreating(true)}>
            Nova métrica
          </Button>
        </Can>
      </Group>
      <QueryContent query={query}>
        {({ data }) =>
          data.length === 0 ? (
            <EmptyState title="Nenhuma métrica" description="Crie uma métrica para cobrar por consumo, como chamadas de API ou armazenamento." />
          ) : (
            <Table.ScrollContainer minWidth={760}>
              <Table aria-label="Métricas de consumo">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Código</Table.Th>
                    <Table.Th>Nome</Table.Th>
                    <Table.Th>Unidade</Table.Th>
                    <Table.Th>Agregação</Table.Th>
                    <Table.Th ta="right">Máximo por evento</Table.Th>
                    <Table.Th ta="right">Carência</Table.Th>
                    <Table.Th>Situação</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {data.map((m) => (
                    <Table.Tr key={m.id}>
                      <Table.Td>
                        <Code>{m.code}</Code>
                      </Table.Td>
                      <Table.Td>{m.name}</Table.Td>
                      <Table.Td>{m.unit}</Table.Td>
                      <Table.Td>
                        <Tooltip label={aggregationHelp[m.aggregation]} multiline w={300} events={{ hover: true, focus: true, touch: true }}>
                          <Badge variant="light" tabIndex={0}>
                            {labelOf(aggregationLabels, m.aggregation)}
                          </Badge>
                        </Tooltip>
                      </Table.Td>
                      <Table.Td ta="right" className="numeric">
                        {formatQuantity(m.max_quantity_per_event)}
                      </Table.Td>
                      <Table.Td ta="right">{m.late_grace_hours} h</Table.Td>
                      <Table.Td>
                        <StatusBadge status={m.status} labels={catalogStatusLabels} />
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )
        }
      </QueryContent>
      <MetricModal opened={creating} onClose={() => setCreating(false)} />
    </Stack>
  );
}
