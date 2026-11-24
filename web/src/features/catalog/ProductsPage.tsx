import { zodResolver } from '@hookform/resolvers/zod';
import { ActionIcon, Button, Group, Modal, Select, Stack, Table, Text, Textarea, TextInput } from '@mantine/core';
import { IconPencil, IconPlus } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { z } from 'zod';
import type { Product, ProductInput } from '../../api/types';
import { DateTime, StatusBadge } from '../../components/format';
import { EmptyState, QueryContent } from '../../components/states';
import { CursorPager, LabeledSegmented } from '../../components/ui';
import { useApiMutation, useCursorPagination, useReturnFromEmptyPage } from '../../lib/hooks';
import { catalogStatusLabels } from '../../lib/labels';
import { optionalText, requiredText } from '../../lib/validation';
import { Can, usePermission, useTenant } from '../../session/SessionContext';
import { catalogApi } from './api';
import { ResponsiveTable } from '../../components/ResponsiveTable';

const productSchema = z.object({
  name: requiredText(2, 200, 'O nome'),
  description: optionalText(2000),
  status: z.enum(['active', 'archived']),
});
type ProductForm = z.input<typeof productSchema>;
type ProductOutput = z.output<typeof productSchema>;

function ProductModal({ product, opened, onClose }: { product: Product | null; opened: boolean; onClose: () => void }) {
  const form = useForm<ProductForm, unknown, ProductOutput>({
    resolver: zodResolver(productSchema),
    values: { name: product?.name ?? '', description: product?.description ?? '', status: product?.status ?? 'active' },
  });
  const mutation = useApiMutation<ProductOutput, Product, ProductForm>({
    mutationFn: ({ status, ...input }) =>
      product
        ? catalogApi.updateProduct(product.id, product.version, { ...input, description: input.description ?? '', status })
        : catalogApi.createProduct(input as ProductInput),
    invalidate: ['products', 'plans'],
    successMessage: product ? 'Produto atualizado.' : 'Produto criado.',
    form,
    onSuccess: onClose,
    onConcurrentReload: onClose,
  });
  return (
    <Modal opened={opened} onClose={onClose} title={product ? 'Editar produto' : 'Novo produto'}>
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
        <Stack>
          <TextInput label="Nome" required {...form.register('name')} error={form.formState.errors.name?.message} />
          <Textarea label="Descrição" autosize minRows={2} {...form.register('description')} error={form.formState.errors.description?.message} />
          {product && (
            <Controller
              control={form.control}
              name="status"
              render={({ field }) => (
                <Select
                  label="Situação"
                  data={[
                    { value: 'active', label: 'Ativo' },
                    { value: 'archived', label: 'Arquivado' },
                  ]}
                  value={field.value}
                  onChange={(v) => v && field.onChange(v)}
                  allowDeselect={false}
                />
              )}
            />
          )}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Salvar
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

export function ProductsPage() {
  const { keys } = useTenant();
  const [status, setStatus] = useState<'active' | 'archived' | 'all'>('active');
  const [editing, setEditing] = useState<Product | null | undefined>(undefined);
  const canWrite = usePermission('catalog.write');
  const pagination = useCursorPagination(`products:${status}`);
  const query = useQuery({
    queryKey: keys('products', 'page', status, pagination.cursor),
    queryFn: ({ signal }) =>
      catalogApi.listProducts({ status: status === 'all' ? undefined : status, cursor: pagination.cursor, limit: 25 }, signal),
    placeholderData: keepPreviousData,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);

  return (
    <Stack>
      <Group justify="space-between" align="flex-end">
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
        <Can permission="catalog.write">
          <Button leftSection={<IconPlus size={16} />} onClick={() => setEditing(null)}>
            Novo produto
          </Button>
        </Can>
      </Group>
      <QueryContent query={query}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhum produto" description="Produtos agrupam planos. Crie um produto e depois os planos com seus preços." />
          ) : (
            <>
              <ResponsiveTable minWidth={600}>
                <Table aria-label="Produtos">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Nome</Table.Th>
                      <Table.Th>Descrição</Table.Th>
                      <Table.Th>Situação</Table.Th>
                      <Table.Th>Criado em</Table.Th>
                      {canWrite && <Table.Th aria-label="Ações" />}
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.data.map((p) => (
                      <Table.Tr key={p.id}>
                        <Table.Td fw={500}>{p.name}</Table.Td>
                        <Table.Td>
                          <Text size="sm" lineClamp={2}>
                            {p.description || '—'}
                          </Text>
                        </Table.Td>
                        <Table.Td>
                          <StatusBadge status={p.status} labels={catalogStatusLabels} />
                        </Table.Td>
                        <Table.Td>
                          <DateTime value={p.created_at} dateOnly />
                        </Table.Td>
                        {canWrite && (
                          <Table.Td>
                            <ActionIcon variant="subtle" aria-label={`Editar produto ${p.name}`} onClick={() => setEditing(p)}>
                              <IconPencil size={16} />
                            </ActionIcon>
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
      <ProductModal product={editing ?? null} opened={editing !== undefined} onClose={() => setEditing(undefined)} />
    </Stack>
  );
}
