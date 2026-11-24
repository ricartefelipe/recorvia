import { zodResolver } from '@hookform/resolvers/zod';
import { Anchor, Button, Code, Group, Modal, NumberInput, Select, SimpleGrid, Stack, Table, Text, Textarea, TextInput } from '@mantine/core';
import { IconPlus } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Controller, FormProvider, useForm } from 'react-hook-form';
import { Link, useNavigate } from 'react-router';
import { z } from 'zod';
import type { PlanDetails, PlanInput } from '../../api/types';
import { StatusBadge } from '../../components/format';
import { EmptyState, QueryContent } from '../../components/states';
import { CursorPager, LabeledSegmented } from '../../components/ui';
import { useApiMutation, useCursorPagination, useReturnFromEmptyPage } from '../../lib/hooks';
import { billingIntervalLabels, catalogStatusLabels, labelOf } from '../../lib/labels';
import { optionalInt, optionalText, requiredText } from '../../lib/validation';
import { Can, useTenant } from '../../session/SessionContext';
import { catalogApi, useMetrics, useProducts } from './api';
import { describePrice, emptyPrice, PriceFields, priceSchema } from './PriceFields';
import { ResponsiveTable } from '../../components/ResponsiveTable';

export const planSchema = z.object({
  product_id: z.string().min(1, 'Selecione o produto.'),
  code: z
    .string()
    .trim()
    .regex(/^[a-z0-9][a-z0-9_.-]{1,63}$/, 'Use de 2 a 64 caracteres: letras minúsculas, números, ponto, hífen ou sublinhado.'),
  name: requiredText(2, 200, 'O nome'),
  description: optionalText(2000),
  billing_interval: z.enum(['month', 'year']),
  trial_days: optionalInt(0, 365),
  price: priceSchema,
});
type PlanForm = z.input<typeof planSchema>;

export function PlanCreateModal({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const navigate = useNavigate();
  const products = useProducts('active');
  const metrics = useMetrics();
  const form = useForm<PlanForm, unknown, PlanInput>({
    resolver: zodResolver(planSchema),
    defaultValues: { product_id: '', code: '', name: '', description: '', billing_interval: 'month', trial_days: '', price: emptyPrice },
  });
  const mutation = useApiMutation<PlanInput, PlanDetails, PlanForm>({
    mutationFn: catalogApi.createPlan,
    invalidate: ['plans'],
    successMessage: 'Plano criado com a versão 1 do preço.',
    form,
    fieldMap: { 'price.unit_amount_decimal': 'price.unit_amount' },
    onSuccess: (plan) => {
      form.reset();
      onClose();
      navigate(`/catalogo/planos/${plan.id}`);
    },
  });
  const { register, control, formState } = form;
  return (
    <Modal opened={opened} onClose={onClose} title="Novo plano" size="xl">
      <FormProvider {...form}>
        <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
          <Stack>
            <SimpleGrid cols={{ base: 1, sm: 2 }}>
              <Controller
                control={control}
                name="product_id"
                render={({ field, fieldState }) => (
                  <Select
                    label="Produto"
                    required
                    data={(products.data?.data ?? []).map((p) => ({ value: p.id, label: p.name }))}
                    value={field.value || null}
                    onChange={(v) => field.onChange(v ?? '')}
                    onBlur={field.onBlur}
                    error={fieldState.error?.message}
                    placeholder={products.isPending ? 'Carregando…' : 'Selecione'}
                    nothingFoundMessage="Cadastre um produto primeiro"
                    searchable
                  />
                )}
              />
              <TextInput label="Código" placeholder="profissional-mensal" required {...register('code')} error={formState.errors.code?.message} />
              <TextInput label="Nome" required {...register('name')} error={formState.errors.name?.message} />
              <Controller
                control={control}
                name="billing_interval"
                render={({ field }) => (
                  <LabeledSegmented
                    label="Periodicidade"
                    value={field.value}
                    onChange={field.onChange}
                    data={[
                      { value: 'month', label: 'Mensal' },
                      { value: 'year', label: 'Anual' },
                    ]}
                  />
                )}
              />
              <Controller
                control={control}
                name="trial_days"
                render={({ field, fieldState }) => (
                  <NumberInput
                    label="Dias de avaliação (trial)"
                    min={0}
                    max={365}
                    allowDecimal={false}
                    value={field.value}
                    onChange={(v) => field.onChange(v === '' ? '' : Number(v))}
                    error={fieldState.error?.message}
                  />
                )}
              />
            </SimpleGrid>
            <Textarea label="Descrição" autosize minRows={2} {...register('description')} error={formState.errors.description?.message} />
            <Text fw={600} size="sm">
              Preço (versão 1)
            </Text>
            <PriceFields metrics={metrics.data?.data ?? []} />
            <Group justify="flex-end">
              <Button variant="default" onClick={onClose}>
                Cancelar
              </Button>
              <Button type="submit" loading={mutation.isPending}>
                Criar plano
              </Button>
            </Group>
          </Stack>
        </form>
      </FormProvider>
    </Modal>
  );
}

export function PlansPage() {
  const [status, setStatus] = useState<'active' | 'archived' | 'all'>('active');
  const [productId, setProductId] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const { keys } = useTenant();
  const products = useProducts();
  const metrics = useMetrics();
  const filters = { status: status === 'all' ? undefined : status, product_id: productId ?? undefined };
  const pagination = useCursorPagination(`plans:${status}:${productId ?? ''}`);
  const query = useQuery({
    queryKey: keys('plans', 'page', filters, pagination.cursor),
    queryFn: ({ signal }) => catalogApi.listPlans({ ...filters, cursor: pagination.cursor, limit: 25 }, signal),
    placeholderData: keepPreviousData,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);
  const productName = new Map((products.data?.data ?? []).map((p) => [p.id, p.name]));

  return (
    <Stack>
      <Group justify="space-between" align="flex-end">
        <Group align="flex-end">
          <LabeledSegmented
            label="Situação"
            value={status}
            onChange={(v) => setStatus(v as typeof status)}
            data={[
              { value: 'active', label: 'Ativos' },
              { value: 'archived', label: 'Arquivados' },
              { value: 'all', label: 'Todos' },
            ]}
          />
          <Select
            label="Produto"
            placeholder="Todos"
            clearable
            data={(products.data?.data ?? []).map((p) => ({ value: p.id, label: p.name }))}
            value={productId}
            onChange={setProductId}
            w={240}
          />
        </Group>
        <Can permission="catalog.write">
          <Button leftSection={<IconPlus size={16} />} onClick={() => setCreating(true)}>
            Novo plano
          </Button>
        </Can>
      </Group>
      <QueryContent query={query}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhum plano" description="Crie um plano com seu preço para começar a vender assinaturas." />
          ) : (
            <>
              <ResponsiveTable minWidth={820}>
                <Table aria-label="Planos">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Plano</Table.Th>
                      <Table.Th>Código</Table.Th>
                      <Table.Th>Produto</Table.Th>
                      <Table.Th>Periodicidade</Table.Th>
                      <Table.Th>Preço atual</Table.Th>
                      <Table.Th>Situação</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.data.map((p) => (
                      <Table.Tr key={p.id}>
                        <Table.Td>
                          <Anchor component={Link} to={`/catalogo/planos/${p.id}`} fw={500}>
                            {p.name}
                          </Anchor>
                        </Table.Td>
                        <Table.Td>
                          <Code>{p.code}</Code>
                        </Table.Td>
                        <Table.Td>{productName.get(p.product_id) ?? '—'}</Table.Td>
                        <Table.Td>{labelOf(billingIntervalLabels, p.billing_interval)}</Table.Td>
                        <Table.Td>
                          <Text size="sm">{describePrice(p.current_price, metrics.data?.data, p.billing_interval)}</Text>
                          {p.current_price && (
                            <Text size="xs" c="dimmed">
                              Versão {p.current_price.version}
                            </Text>
                          )}
                        </Table.Td>
                        <Table.Td>
                          <StatusBadge status={p.status} labels={catalogStatusLabels} />
                        </Table.Td>
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
      <PlanCreateModal opened={creating} onClose={() => setCreating(false)} />
    </Stack>
  );
}
