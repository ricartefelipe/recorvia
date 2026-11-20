import { zodResolver } from '@hookform/resolvers/zod';
import {
  Alert,
  Anchor,
  Badge,
  Breadcrumbs,
  Button,
  Code,
  Group,
  Modal,
  NumberInput,
  Paper,
  Select,
  SimpleGrid,
  Stack,
  Table,
  Text,
  Textarea,
  TextInput,
  Title,
} from '@mantine/core';
import { IconInfoCircle, IconPencil, IconPlus } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Controller, FormProvider, useForm } from 'react-hook-form';
import { Link, useParams } from 'react-router';
import { z } from 'zod';
import type { PlanDetails, PlanUpdate, PriceInput, PriceVersion } from '../../api/types';
import { DateTime, StatusBadge } from '../../components/format';
import { LoadingState, QueryContent } from '../../components/states';
import { KeyValue, PageHeader } from '../../components/ui';
import { useApiMutation } from '../../lib/hooks';
import { billingIntervalLabels, catalogStatusLabels, labelOf, pricingModelLabels } from '../../lib/labels';
import { optionalText, requiredInt, requiredText } from '../../lib/validation';
import { Can, useTenant } from '../../session/SessionContext';
import { catalogApi, useMetrics, useProducts } from './api';
import { describePrice, PriceFields, priceSchema, priceToForm, type PriceFormValues } from './PriceFields';

const planUpdateSchema = z.object({
  name: requiredText(2, 200, 'O nome'),
  description: optionalText(2000),
  trial_days: requiredInt(0, 365),
  status: z.enum(['active', 'archived']),
});
type PlanUpdateForm = z.input<typeof planUpdateSchema>;

function EditPlanModal({ plan, opened, onClose }: { plan: PlanDetails; opened: boolean; onClose: () => void }) {
  const form = useForm<PlanUpdateForm, unknown, PlanUpdate>({
    resolver: zodResolver(planUpdateSchema),
    values: { name: plan.name, description: plan.description ?? '', trial_days: plan.trial_days, status: plan.status },
  });
  const mutation = useApiMutation<PlanUpdate, PlanDetails, PlanUpdateForm>({
    mutationFn: (input) => catalogApi.updatePlan(plan.id, plan.version, { ...input, description: input.description ?? '' }),
    invalidate: ['plans'],
    successMessage: 'Plano atualizado.',
    form,
    onSuccess: onClose,
  });
  return (
    <Modal opened={opened} onClose={onClose} title="Editar plano">
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
        <Stack>
          <TextInput label="Nome" required {...form.register('name')} error={form.formState.errors.name?.message} />
          <Textarea label="Descrição" autosize minRows={2} {...form.register('description')} error={form.formState.errors.description?.message} />
          <Controller
            control={form.control}
            name="trial_days"
            render={({ field, fieldState }) => (
              <NumberInput
                label="Dias de avaliação (trial)"
                description="Vale para novas assinaturas."
                min={0}
                max={365}
                allowDecimal={false}
                value={field.value}
                onChange={(v) => field.onChange(v === '' ? '' : Number(v))}
                error={fieldState.error?.message}
              />
            )}
          />
          <Controller
            control={form.control}
            name="status"
            render={({ field }) => (
              <Select
                label="Situação"
                description="Planos arquivados não aceitam novas assinaturas."
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

const newPriceSchema = z.object({ price: priceSchema });
type NewPriceForm = { price: PriceFormValues };

function NewPriceVersionModal({ plan, opened, onClose }: { plan: PlanDetails; opened: boolean; onClose: () => void }) {
  const metrics = useMetrics();
  const form = useForm<NewPriceForm, unknown, { price: PriceInput }>({
    resolver: zodResolver(newPriceSchema),
    values: { price: priceToForm(plan.current_price) },
  });
  const mutation = useApiMutation<PriceInput, PriceVersion, NewPriceForm>({
    mutationFn: (input) => catalogApi.addPriceVersion(plan.id, input),
    invalidate: ['plans'],
    successMessage: (v) => `Versão ${v.version} do preço criada.`,
    form,
    fieldMap: {
      pricing_model: 'price.pricing_model',
      flat_amount: 'price.flat_amount',
      unit_amount_decimal: 'price.unit_amount',
      metric_id: 'price.metric_id',
      included_quantity: 'price.included_quantity',
    },
    onSuccess: onClose,
  });
  return (
    <Modal opened={opened} onClose={onClose} title="Nova versão de preço" size="lg">
      <FormProvider {...form}>
        <form onSubmit={form.handleSubmit((v) => mutation.mutate(v.price))} noValidate>
          <Stack>
            <Alert color="blue" icon={<IconInfoCircle />} title="Assinaturas existentes não mudam">
              Versões de preço são imutáveis. A nova versão vale para novas assinaturas e trocas de plano; as assinaturas
              atuais continuam com a versão contratada e faturas emitidas mantêm o preço registrado. Para alterar o valor
              de uma assinatura existente, use um reajuste programado.
            </Alert>
            <PriceFields metrics={metrics.data?.data ?? []} />
            <Group justify="flex-end">
              <Button variant="default" onClick={onClose}>
                Cancelar
              </Button>
              <Button type="submit" loading={mutation.isPending}>
                Criar versão {(plan.prices.reduce((max, p) => Math.max(max, p.version), 0) || 0) + 1}
              </Button>
            </Group>
          </Stack>
        </form>
      </FormProvider>
    </Modal>
  );
}

export function PlanDetailPage() {
  const { id = '' } = useParams();
  const { keys } = useTenant();
  const [editing, setEditing] = useState(false);
  const [pricing, setPricing] = useState(false);
  const metrics = useMetrics();
  const products = useProducts();
  const query = useQuery({ queryKey: keys('plans', 'detail', id), queryFn: ({ signal }) => catalogApi.getPlan(id, signal) });

  return (
    <Stack>
      <Breadcrumbs>
        <Anchor component={Link} to="/catalogo/planos">
          Planos
        </Anchor>
        <Text>{query.data?.name ?? 'Plano'}</Text>
      </Breadcrumbs>
      <QueryContent query={query} loading={<LoadingState variant="detail" />}>
        {(plan) => {
          const product = products.data?.data.find((p) => p.id === plan.product_id);
          const versions = [...plan.prices].sort((a, b) => b.version - a.version);
          return (
            <>
              <PageHeader
                title={plan.name}
                badge={<StatusBadge status={plan.status} labels={catalogStatusLabels} />}
                description={plan.description || undefined}
                actions={
                  <Can permission="catalog.write">
                    <Button variant="default" leftSection={<IconPencil size={16} />} onClick={() => setEditing(true)}>
                      Editar plano
                    </Button>
                    <Button leftSection={<IconPlus size={16} />} onClick={() => setPricing(true)}>
                      Nova versão de preço
                    </Button>
                  </Can>
                }
              />
              <Paper withBorder p="md" radius="md">
                <SimpleGrid cols={{ base: 1, xs: 2, md: 4 }}>
                  <KeyValue label="Código">
                    <Code>{plan.code}</Code>
                  </KeyValue>
                  <KeyValue label="Produto">{product?.name ?? '—'}</KeyValue>
                  <KeyValue label="Periodicidade">{labelOf(billingIntervalLabels, plan.billing_interval)}</KeyValue>
                  <KeyValue label="Avaliação">{plan.trial_days ? `${plan.trial_days} dias` : 'Sem avaliação'}</KeyValue>
                  <KeyValue label="Moeda">{plan.currency}</KeyValue>
                  <KeyValue label="Preço atual">{describePrice(plan.current_price, metrics.data?.data, plan.billing_interval)}</KeyValue>
                  <KeyValue label="Criado em">
                    <DateTime value={plan.created_at} />
                  </KeyValue>
                </SimpleGrid>
              </Paper>
              <Title order={2} size="h4">
                Versões de preço
              </Title>
              <Text size="sm" c="dimmed">
                Cada assinatura guarda a versão contratada. Criar uma versão não altera assinaturas nem faturas existentes.
              </Text>
              <Table.ScrollContainer minWidth={720}>
                <Table aria-label="Versões de preço">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Versão</Table.Th>
                      <Table.Th>Modelo</Table.Th>
                      <Table.Th>Valores</Table.Th>
                      <Table.Th>Criada em</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {versions.map((v) => (
                      <Table.Tr key={v.id}>
                        <Table.Td>
                          <Group gap={6}>
                            <Text fw={600}>v{v.version}</Text>
                            {plan.current_price?.id === v.id && (
                              <Badge size="sm" variant="filled">
                                Atual
                              </Badge>
                            )}
                          </Group>
                        </Table.Td>
                        <Table.Td>{labelOf(pricingModelLabels, v.pricing_model)}</Table.Td>
                        <Table.Td>{describePrice(v, metrics.data?.data, plan.billing_interval)}</Table.Td>
                        <Table.Td>
                          <DateTime value={v.created_at} />
                        </Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
              <EditPlanModal plan={plan} opened={editing} onClose={() => setEditing(false)} />
              <NewPriceVersionModal plan={plan} opened={pricing} onClose={() => setPricing(false)} />
            </>
          );
        }}
      </QueryContent>
    </Stack>
  );
}
