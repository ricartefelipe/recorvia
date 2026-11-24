import { zodResolver } from '@hookform/resolvers/zod';
import {
  ActionIcon,
  Anchor,
  Badge,
  Button,
  Card,
  Code,
  Group,
  Modal,
  NumberInput,
  Select,
  SimpleGrid,
  Stack,
  Switch,
  Table,
  Tabs,
  Text,
  Textarea,
  TextInput,
  Tooltip,
} from '@mantine/core';
import { IconListDetails, IconMail, IconPencil, IconPlus, IconTrash } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useRef, useState } from 'react';
import { Controller, useFieldArray, useForm, useWatch, type Control } from 'react-hook-form';
import { Link } from 'react-router';
import { z } from 'zod';
import { api } from '../../api/client';
import type {
  CollectionPolicy,
  CollectionPolicyInput,
  CursorPage,
  DataList,
  Notification,
  NotificationEvent,
  NotificationTemplate,
} from '../../api/types';
import { DateTime, StatusBadge } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { CursorPager, PageHeader } from '../../components/ui';
import { useApiMutation, useCursorPagination, useReturnFromEmptyPage, useUrlFilters } from '../../lib/hooks';
import { stableStringify } from '../../lib/idempotency';
import { channelLabels, labelOf, notificationReasonLabels, notificationStatusLabels, optionsOf, templateKeyLabels } from '../../lib/labels';
import { requiredText } from '../../lib/validation';
import { Can, usePermission, useTenant } from '../../session/SessionContext';
import { ResponsiveTable } from '../../components/ResponsiveTable';

const enc = encodeURIComponent;

const collectionsApi = {
  policies: (signal?: AbortSignal) => api.get<DataList<CollectionPolicy>>('/api/v1/collection-policies', { signal }),
  createPolicy: (input: CollectionPolicyInput) => api.post<CollectionPolicy>('/api/v1/collection-policies', input),
  updatePolicy: (p: CollectionPolicy, input: CollectionPolicyInput) =>
    api.patch<CollectionPolicy>(`/api/v1/collection-policies/${enc(p.id)}`, input, { ifMatch: p.version }),
  templates: (signal?: AbortSignal) => api.get<DataList<NotificationTemplate>>('/api/v1/notification-templates', { signal }),
  /** If-Match é a versão da personalização; 0 no template padrão, ainda não personalizado. */
  updateTemplate: (t: NotificationTemplate, input: { subject: string; body: string }) =>
    api.put<NotificationTemplate>(`/api/v1/notification-templates/${enc(t.key)}`, input, { ifMatch: t.version }),
  notifications: (params: { status?: string; invoice_id?: string; cursor?: string; limit?: number }, signal?: AbortSignal) =>
    api.get<CursorPage<Notification>>('/api/v1/notifications', { query: { ...params }, signal }),
  notificationEvents: (id: string, signal?: AbortSignal) =>
    api.get<DataList<NotificationEvent>>(`/api/v1/notifications/${enc(id)}/events`, { signal }),
};

export function formatOffsets(offsets: number[]): string {
  if (offsets.length === 0) return 'Sem lembretes';
  return [...offsets]
    .sort((a, b) => a - b)
    .map((d) => (d === 0 ? 'no vencimento' : d < 0 ? `${-d} dia(s) antes` : `${d} dia(s) depois`))
    .join(', ');
}

const intIn = (min: number, max: number, msg: string) =>
  z.union([z.number(), z.literal('')]).refine((v) => v !== '' && Number.isInteger(v) && v >= min && v <= max, msg).transform((v) => v as number);

export const policySchema = z
  .object({
    name: requiredText(2, 100, 'O nome'),
    is_default: z.boolean(),
    offsets: z
      .array(z.object({ value: intIn(-30, 90, 'Use um número entre -30 e 90.') }))
      .max(10, 'Use no máximo 10 lembretes.'),
    send_window_start_hour: intIn(0, 23, 'Use uma hora entre 0 e 23.'),
    send_window_end_hour: intIn(1, 24, 'Use uma hora entre 1 e 24.'),
    max_messages_per_day: intIn(1, 5, 'Use um limite entre 1 e 5.'),
    suspend: z.boolean(),
    suspend_after_days: z.union([z.number(), z.literal('')]),
    reactivate_on_payment: z.boolean(),
  })
  .refine((v) => v.send_window_end_hour > v.send_window_start_hour, {
    path: ['send_window_end_hour'],
    message: 'O fim da janela deve ser depois do início.',
  })
  .refine((v) => !v.suspend || (typeof v.suspend_after_days === 'number' && Number.isInteger(v.suspend_after_days) && v.suspend_after_days >= 1 && v.suspend_after_days <= 180), {
    path: ['suspend_after_days'],
    message: 'Informe de 1 a 180 dias.',
  })
  .refine((v) => new Set(v.offsets.map((o) => o.value)).size === v.offsets.length, {
    path: ['offsets'],
    message: 'Há lembretes repetidos.',
  })
  .transform(
    (v): CollectionPolicyInput => ({
      name: v.name,
      is_default: v.is_default,
      reminder_offsets_days: v.offsets.map((o) => o.value).sort((a, b) => a - b),
      send_window_start_hour: v.send_window_start_hour,
      send_window_end_hour: v.send_window_end_hour,
      max_messages_per_day: v.max_messages_per_day,
      suspend_after_days: v.suspend ? (v.suspend_after_days as number) : null,
      reactivate_on_payment: v.reactivate_on_payment,
    }),
  );
type PolicyForm = z.input<typeof policySchema>;

function NumberField({
  name,
  control,
  label,
  min,
  max,
  description,
}: {
  name: 'send_window_start_hour' | 'send_window_end_hour' | 'max_messages_per_day' | 'suspend_after_days';
  control: Control<PolicyForm, unknown, CollectionPolicyInput>;
  label: string;
  min: number;
  max: number;
  description?: string;
}) {
  return (
    <Controller
      control={control}
      name={name}
      render={({ field, fieldState }) => (
        <NumberInput
          label={label}
          description={description}
          min={min}
          max={max}
          allowDecimal={false}
          value={field.value}
          onChange={(v) => field.onChange(v === '' ? '' : Number(v))}
          onBlur={field.onBlur}
          error={fieldState.error?.message}
        />
      )}
    />
  );
}

function PolicyModal({ policy, opened, onClose }: { policy: CollectionPolicy | null; opened: boolean; onClose: () => void }) {
  const form = useForm<PolicyForm, unknown, CollectionPolicyInput>({
    resolver: zodResolver(policySchema),
    values: {
      name: policy?.name ?? '',
      is_default: policy?.is_default ?? false,
      offsets: (policy?.reminder_offsets_days ?? [-3, 0, 3, 7]).map((value) => ({ value })),
      send_window_start_hour: policy?.send_window_start_hour ?? 8,
      send_window_end_hour: policy?.send_window_end_hour ?? 20,
      max_messages_per_day: policy?.max_messages_per_day ?? 1,
      suspend: policy ? policy.suspend_after_days != null : false,
      suspend_after_days: policy?.suspend_after_days ?? 30,
      reactivate_on_payment: policy?.reactivate_on_payment ?? true,
    },
  });
  const offsets = useFieldArray({ control: form.control, name: 'offsets' });
  const suspend = useWatch({ control: form.control, name: 'suspend' });
  const mutation = useApiMutation<CollectionPolicyInput, CollectionPolicy, PolicyForm>({
    mutationFn: (input) => (policy ? collectionsApi.updatePolicy(policy, input) : collectionsApi.createPolicy(input)),
    invalidate: ['collection-policies'],
    successMessage: policy ? 'Política atualizada.' : 'Política criada.',
    form,
    fieldMap: { reminder_offsets_days: 'offsets' },
    onSuccess: onClose,
    onConcurrentReload: onClose,
  });
  // Erros da lista inteira (máximo, repetidos) chegam em root, como no
  // resolver do zod para arrays; mensagens do servidor, em message.
  const offsetsError = form.formState.errors.offsets;
  const offsetsMessage = offsetsError?.message ?? offsetsError?.root?.message;
  return (
    <Modal opened={opened} onClose={onClose} title={policy ? 'Editar política de cobrança' : 'Nova política de cobrança'} size="lg">
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
        <Stack>
          <TextInput label="Nome" required {...form.register('name')} error={form.formState.errors.name?.message} />
          <Group gap="xs">
            <IconMail size={16} aria-hidden />
            <Text size="sm">Canal: {channelLabels.email}</Text>
          </Group>
          <Stack gap={6}>
            <Text size="sm" fw={500}>
              Lembretes (dias relativos ao vencimento)
            </Text>
            <Text size="xs" c="dimmed">
              Negativo envia antes do vencimento, 0 no dia e positivo depois. Lembretes param quando a fatura é paga.
            </Text>
            {offsets.fields.map((f, index) => (
              <Group key={f.id} gap="xs" align="flex-start">
                <Controller
                  control={form.control}
                  name={`offsets.${index}.value`}
                  render={({ field, fieldState }) => (
                    <NumberInput
                      aria-label={`Lembrete ${index + 1} (dias)`}
                      min={-30}
                      max={90}
                      allowDecimal={false}
                      value={field.value}
                      onChange={(v) => {
                        field.onChange(v === '' ? '' : Number(v));
                        // A repetição é uma regra da lista inteira (offsets.root):
                        // depois do primeiro envio, a lista é revalidada a cada
                        // mudança para que o aviso suma assim que for corrigido.
                        if (form.formState.isSubmitted) void form.trigger('offsets');
                      }}
                      error={fieldState.error?.message}
                      w={140}
                    />
                  )}
                />
                <ActionIcon variant="subtle" color="red" mt={6} aria-label={`Remover lembrete ${index + 1}`} onClick={() => offsets.remove(index)}>
                  <IconTrash size={16} />
                </ActionIcon>
              </Group>
            ))}
            {offsetsMessage && (
              <Text size="xs" c="red" role="alert">
                {offsetsMessage}
              </Text>
            )}
            <Button
              variant="light"
              size="xs"
              w="fit-content"
              leftSection={<IconPlus size={14} />}
              onClick={() => offsets.append({ value: '' })}
              disabled={offsets.fields.length >= 10}
            >
              Adicionar lembrete
            </Button>
          </Stack>
          <SimpleGrid cols={{ base: 1, sm: 3 }}>
            <NumberField control={form.control} name="send_window_start_hour" label="Envio a partir das (h)" min={0} max={23} />
            <NumberField control={form.control} name="send_window_end_hour" label="Envio até as (h)" min={1} max={24} />
            <NumberField control={form.control} name="max_messages_per_day" label="Máximo de e-mails por dia" min={1} max={5} />
          </SimpleGrid>
          <Text size="xs" c="dimmed">
            A janela de envio usa o fuso da organização.
          </Text>
          <Controller
            control={form.control}
            name="suspend"
            render={({ field }) => (
              <Switch label="Suspender a assinatura após atraso" checked={field.value} onChange={(e) => field.onChange(e.currentTarget.checked)} />
            )}
          />
          {suspend && <NumberField control={form.control} name="suspend_after_days" label="Suspender após quantos dias de atraso" min={1} max={180} />}
          <Controller
            control={form.control}
            name="reactivate_on_payment"
            render={({ field }) => (
              <Switch label="Reativar automaticamente quando a fatura for paga" checked={field.value} onChange={(e) => field.onChange(e.currentTarget.checked)} />
            )}
          />
          <Controller
            control={form.control}
            name="is_default"
            render={({ field }) => (
              <Switch label="Política padrão da organização" checked={field.value} onChange={(e) => field.onChange(e.currentTarget.checked)} />
            )}
          />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Salvar política
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

function PoliciesTab() {
  const { keys } = useTenant();
  const canWrite = usePermission('collections.write');
  const [editing, setEditing] = useState<CollectionPolicy | null | undefined>(undefined);
  const query = useQuery({ queryKey: keys('collection-policies', 'list'), queryFn: ({ signal }) => collectionsApi.policies(signal) });
  return (
    <Stack>
      <Group justify="flex-end">
        <Can permission="collections.write">
          <Button leftSection={<IconPlus size={16} />} onClick={() => setEditing(null)}>
            Nova política
          </Button>
        </Can>
      </Group>
      <QueryContent query={query}>
        {({ data }) =>
          data.length === 0 ? (
            <EmptyState title="Nenhuma política" description="Defina quando enviar lembretes antes e depois do vencimento e quando suspender assinaturas em atraso." />
          ) : (
            <ResponsiveTable minWidth={900}>
              <Table aria-label="Políticas de cobrança">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Nome</Table.Th>
                    <Table.Th>Lembretes</Table.Th>
                    <Table.Th>Janela de envio</Table.Th>
                    <Table.Th>Limite diário</Table.Th>
                    <Table.Th>Suspensão</Table.Th>
                    <Table.Th>Reativa ao pagar</Table.Th>
                    {canWrite && <Table.Th aria-label="Ações" />}
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {data.map((p) => (
                    <Table.Tr key={p.id}>
                      <Table.Td>
                        <Group gap={6}>
                          <Text fw={500} size="sm">
                            {p.name}
                          </Text>
                          {p.is_default && <Badge size="sm">Padrão</Badge>}
                        </Group>
                      </Table.Td>
                      <Table.Td>
                        <Text size="sm">{formatOffsets(p.reminder_offsets_days)}</Text>
                      </Table.Td>
                      <Table.Td>
                        {p.send_window_start_hour}h–{p.send_window_end_hour}h
                      </Table.Td>
                      <Table.Td>{p.max_messages_per_day} e-mail(s)</Table.Td>
                      <Table.Td>{p.suspend_after_days != null ? `Após ${p.suspend_after_days} dia(s)` : 'Não suspende'}</Table.Td>
                      <Table.Td>{p.reactivate_on_payment ? 'Sim' : 'Não'}</Table.Td>
                      {canWrite && (
                        <Table.Td>
                          <ActionIcon variant="subtle" aria-label={`Editar política ${p.name}`} onClick={() => setEditing(p)}>
                            <IconPencil size={16} />
                          </ActionIcon>
                        </Table.Td>
                      )}
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </ResponsiveTable>
          )
        }
      </QueryContent>
      <PolicyModal policy={editing ?? null} opened={editing !== undefined} onClose={() => setEditing(undefined)} />
    </Stack>
  );
}

/** Marcação de uma variável no assunto e no corpo do template ({{nome}}). */
export function placeholder(name: string): string {
  return `{{${name}}}`;
}

const templateSchema = z.object({
  subject: requiredText(3, 200, 'O assunto'),
  body: z.string().trim().min(10, 'O corpo deve ter pelo menos 10 caracteres.').max(10000, 'O corpo deve ter no máximo 10.000 caracteres.'),
});
type TemplateForm = z.input<typeof templateSchema>;

function TemplateModal({ template, onClose }: { template: NotificationTemplate; onClose: () => void }) {
  const bodyRef = useRef<HTMLTextAreaElement | null>(null);
  const form = useForm<TemplateForm>({ resolver: zodResolver(templateSchema), defaultValues: { subject: template.subject, body: template.body } });
  const mutation = useApiMutation<TemplateForm, NotificationTemplate, TemplateForm>({
    mutationFn: (input) => collectionsApi.updateTemplate(template, input),
    invalidate: ['notification-templates'],
    successMessage: 'Template gravado.',
    form,
    onSuccess: onClose,
    onConcurrentReload: onClose,
  });
  const { ref, ...bodyField } = form.register('body');
  const insert = (name: string) => {
    const variable = placeholder(name);
    const el = bodyRef.current;
    const current = form.getValues('body');
    const start = el?.selectionStart ?? current.length;
    const end = el?.selectionEnd ?? current.length;
    form.setValue('body', current.slice(0, start) + variable + current.slice(end), { shouldDirty: true });
    requestAnimationFrame(() => {
      el?.focus();
      el?.setSelectionRange(start + variable.length, start + variable.length);
    });
  };
  return (
    <Modal opened onClose={onClose} title={`Template: ${labelOf(templateKeyLabels, template.key)}`} size="xl">
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
        <Stack>
          <Text size="sm">Canal: {channelLabels.email}</Text>
          <TextInput label="Assunto" required {...form.register('subject')} error={form.formState.errors.subject?.message} />
          <Textarea
            label="Corpo do e-mail"
            required
            autosize
            minRows={8}
            maxRows={20}
            {...bodyField}
            ref={(el) => {
              ref(el);
              bodyRef.current = el;
            }}
            error={form.formState.errors.body?.message}
          />
          {template.variables.length > 0 && (
            <Stack gap={4}>
              <Text size="sm" fw={500}>
                Variáveis disponíveis
              </Text>
              <Text size="xs" c="dimmed">
                Clique para inserir na posição do cursor.
              </Text>
              <Group gap={6}>
                {template.variables.map((v) => (
                  <Button key={v} size="compact-xs" variant="light" onClick={() => insert(v)} aria-label={`Inserir variável ${v}`}>
                    {placeholder(v)}
                  </Button>
                ))}
              </Group>
            </Stack>
          )}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Salvar template
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

function TemplatesTab() {
  const { keys } = useTenant();
  const [editing, setEditing] = useState<NotificationTemplate | null>(null);
  const query = useQuery({ queryKey: keys('notification-templates', 'list'), queryFn: ({ signal }) => collectionsApi.templates(signal) });
  return (
    <QueryContent query={query}>
      {({ data }) => (
        <>
          {data.length === 0 ? (
            <EmptyState title="Nenhum template" />
          ) : (
            <SimpleGrid cols={{ base: 1, md: 2 }}>
              {data.map((t) => (
                <Card key={t.key} withBorder radius="md">
                  <Stack gap="xs">
                    <Group justify="space-between">
                      <Text fw={600}>{labelOf(templateKeyLabels, t.key)}</Text>
                      <Group gap={6}>
                        <Badge variant="light" color="gray" leftSection={<IconMail size={12} />}>
                          {labelOf(channelLabels, t.channel)}
                        </Badge>
                        <Badge variant="light" color={t.is_default ? 'gray' : 'blue'}>
                          {t.is_default ? 'Padrão do sistema' : 'Personalizado'}
                        </Badge>
                      </Group>
                    </Group>
                    <Text size="sm">
                      <Text span c="dimmed">
                        Assunto:{' '}
                      </Text>
                      {t.subject}
                    </Text>
                    <Text size="xs" c="dimmed" lineClamp={3} style={{ whiteSpace: 'pre-wrap' }}>
                      {t.body}
                    </Text>
                    <Group gap={4}>
                      {t.variables.map((v) => (
                        <Code key={v}>{placeholder(v)}</Code>
                      ))}
                    </Group>
                    <Group justify="space-between">
                      <Text size="xs" c="dimmed">
                        {t.updated_at ? (
                          <>
                            Atualizado em <DateTime value={t.updated_at} />
                          </>
                        ) : (
                          'Nunca alterado'
                        )}
                      </Text>
                      <Can permission="collections.write">
                        <Button size="xs" variant="light" leftSection={<IconPencil size={14} />} onClick={() => setEditing(t)}>
                          Editar
                        </Button>
                      </Can>
                    </Group>
                  </Stack>
                </Card>
              ))}
            </SimpleGrid>
          )}
          {editing && <TemplateModal template={editing} onClose={() => setEditing(null)} />}
        </>
      )}
    </QueryContent>
  );
}

/** Linha do tempo de uma notificação: agendamento, adiamentos, tentativas e desfecho. */
function NotificationEventsModal({ notification, onClose }: { notification: Notification; onClose: () => void }) {
  const { keys } = useTenant();
  const query = useQuery({
    queryKey: keys('notifications', 'events', notification.id),
    queryFn: ({ signal }) => collectionsApi.notificationEvents(notification.id, signal),
  });
  return (
    <Modal opened onClose={onClose} title="Linha do tempo da notificação" size="xl">
      <Stack gap="sm">
        <Text size="sm" c="dimmed">
          {labelOf(templateKeyLabels, notification.template_key)} · {notification.subject}
        </Text>
        <QueryContent query={query} loading={<LoadingState />}>
          {(list) =>
            list.data.length === 0 ? (
              <EmptyState title="Sem eventos" description="Nenhum evento registrado para esta notificação." />
            ) : (
              <Table aria-label="Linha do tempo da notificação">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Quando</Table.Th>
                    <Table.Th>Evento</Table.Th>
                    <Table.Th>Situação</Table.Th>
                    <Table.Th ta="right">Tentativa</Table.Th>
                    <Table.Th>Detalhe</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {list.data.map((e, i) => (
                    <Table.Tr key={`${e.occurred_at}-${i}`}>
                      <Table.Td>
                        <DateTime value={e.occurred_at} />
                      </Table.Td>
                      <Table.Td>{labelOf(notificationReasonLabels, e.reason)}</Table.Td>
                      <Table.Td>
                        <StatusBadge status={e.status} labels={notificationStatusLabels} />
                      </Table.Td>
                      <Table.Td ta="right">{e.attempt > 0 ? e.attempt : '—'}</Table.Td>
                      <Table.Td>
                        {e.detail ? (
                          <Text size="xs" lineClamp={3} title={e.detail}>
                            {e.reason === 'outside_send_window' || e.reason === 'frequency_limit' ? (
                              <>
                                Adiada para <DateTime value={e.detail} />
                              </>
                            ) : (
                              e.detail
                            )}
                          </Text>
                        ) : (
                          '—'
                        )}
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            )
          }
        </QueryContent>
      </Stack>
    </Modal>
  );
}

function NotificationsTab() {
  const { keys } = useTenant();
  const [timeline, setTimeline] = useState<Notification | null>(null);
  const [filters, setFilters] = useUrlFilters(['situacao', 'fatura'] as const);
  const params = { status: filters.situacao || undefined, invoice_id: filters.fatura || undefined, limit: 25 };
  const pagination = useCursorPagination(stableStringify(params));
  const query = useQuery({
    queryKey: keys('notifications', 'list', { ...params, cursor: pagination.cursor }),
    queryFn: ({ signal }) => collectionsApi.notifications({ ...params, cursor: pagination.cursor }, signal),
    placeholderData: keepPreviousData,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);
  return (
    <Stack>
      <Group align="flex-end">
        <Select
          label="Situação"
          placeholder="Todas"
          clearable
          data={optionsOf(notificationStatusLabels)}
          value={filters.situacao || null}
          onChange={(v) => setFilters({ situacao: v })}
          w={200}
        />
        {filters.fatura && (
          <Button variant="subtle" onClick={() => setFilters({ fatura: null })}>
            Remover filtro de fatura
          </Button>
        )}
      </Group>
      <QueryContent query={query} loading={<LoadingState />}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhuma notificação" description="E-mails de cobrança agendados e enviados aparecem aqui." />
          ) : (
            <>
              <ResponsiveTable minWidth={1000}>
                <Table aria-label="Histórico de notificações">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Agendada para</Table.Th>
                      <Table.Th>Mensagem</Table.Th>
                      <Table.Th>Destinatário</Table.Th>
                      <Table.Th>Assunto</Table.Th>
                      <Table.Th>Situação</Table.Th>
                      <Table.Th ta="right">Tentativas</Table.Th>
                      <Table.Th>Enviada em</Table.Th>
                      <Table.Th>Fatura</Table.Th>
                      <Table.Th aria-label="Ações" />
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.data.map((n) => (
                      <Table.Tr key={n.id}>
                        <Table.Td>
                          <DateTime value={n.send_after} />
                        </Table.Td>
                        <Table.Td>{labelOf(templateKeyLabels, n.template_key)}</Table.Td>
                        <Table.Td>{n.recipient}</Table.Td>
                        <Table.Td>
                          <Text size="sm" lineClamp={1} title={n.subject}>
                            {n.subject}
                          </Text>
                        </Table.Td>
                        <Table.Td>
                          <StatusBadge status={n.status} labels={notificationStatusLabels} />
                          {n.status_reason && (
                            <Text size="xs" c="dimmed">
                              {labelOf(notificationReasonLabels, n.status_reason)}
                            </Text>
                          )}
                          {n.last_error && (
                            <Text size="xs" c="red" lineClamp={2} title={n.last_error}>
                              {n.last_error}
                            </Text>
                          )}
                        </Table.Td>
                        <Table.Td ta="right">{n.attempts}</Table.Td>
                        <Table.Td>{n.sent_at ? <DateTime value={n.sent_at} /> : '—'}</Table.Td>
                        <Table.Td>
                          {n.invoice_id ? (
                            <Anchor component={Link} to={`/faturas/${n.invoice_id}`} size="sm">
                              Ver fatura
                            </Anchor>
                          ) : (
                            '—'
                          )}
                        </Table.Td>
                        <Table.Td>
                          <Tooltip label="Linha do tempo">
                            <ActionIcon variant="subtle" aria-label={`Linha do tempo: ${n.subject}`} onClick={() => setTimeline(n)}>
                              <IconListDetails size={16} />
                            </ActionIcon>
                          </Tooltip>
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
      {timeline && <NotificationEventsModal notification={timeline} onClose={() => setTimeline(null)} />}
    </Stack>
  );
}

export function CollectionsPage() {
  const [filters, setFilters] = useUrlFilters(['aba'] as const);
  const tab = ['templates', 'historico'].includes(filters.aba) ? filters.aba : 'politicas';
  return (
    <Stack gap={0}>
      <PageHeader
        title="Políticas de cobrança"
        description="Lembretes por e-mail antes e depois do vencimento, janela de envio no fuso da organização, limite de frequência e suspensão por atraso."
      />
      <Tabs value={tab} onChange={(v) => setFilters({ aba: v === 'politicas' ? null : v })} keepMounted={false}>
        <Tabs.List mb="md">
          <Tabs.Tab value="politicas">Políticas</Tabs.Tab>
          <Tabs.Tab value="templates">Templates de e-mail</Tabs.Tab>
          <Tabs.Tab value="historico">Histórico de envios</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="politicas">
          <PoliciesTab />
        </Tabs.Panel>
        <Tabs.Panel value="templates">
          <TemplatesTab />
        </Tabs.Panel>
        <Tabs.Panel value="historico">
          <NotificationsTab />
        </Tabs.Panel>
      </Tabs>
    </Stack>
  );
}
