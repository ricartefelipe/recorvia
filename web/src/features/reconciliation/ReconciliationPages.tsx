import { zodResolver } from '@hookform/resolvers/zod';
import {
  Alert,
  Anchor,
  Breadcrumbs,
  Button,
  Code,
  Group,
  Paper,
  Radio,
  Select,
  SimpleGrid,
  Stack,
  Table,
  Tabs,
  Text,
  Textarea,
  Title,
} from '@mantine/core';
import { IconInfoCircle, IconPlayerPlay, IconScale } from '@tabler/icons-react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Controller, useForm, useWatch } from 'react-hook-form';
import { Link, useParams } from 'react-router';
import { z } from 'zod';
import { api } from '../../api/client';
import { ApiError } from '../../api/http';
import type { DataList, Discrepancy, DiscrepancyKind, ReconciliationRun } from '../../api/types';
import { AuditEventList } from '../../components/AuditEventList';
import { DateTime, ShortId, StatusBadge } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { confirmAction, KeyValue, PageHeader } from '../../components/ui';
import { useApiMutation, useUrlFilters } from '../../lib/hooks';
import { useIdempotencyKey } from '../../lib/idempotency';
import {
  actorTypeLabels,
  discrepancyKindLabels,
  discrepancyStatusLabels,
  labelOf,
  optionsOf,
  providerLabels,
  reconciliationRunStatusLabels,
  reconciliationRunTriggerLabels,
  resolutionLabels,
} from '../../lib/labels';
import { formatMoney } from '../../lib/money';
import { notifyError, notifyInfo, notifySuccess } from '../../lib/notify';
import { Can, usePermission, useSession, useTenant } from '../../session/SessionContext';

const enc = encodeURIComponent;
type Resolution = 'accept_provider' | 'keep_local' | 'adjust_ledger' | 'dismiss';

const reconciliationApi = {
  runs: (signal?: AbortSignal) => api.get<DataList<ReconciliationRun>>('/api/v1/reconciliation/runs', { signal }),
  run: (idempotencyKey: string) => api.post<ReconciliationRun>('/api/v1/reconciliation/runs', undefined, { idempotencyKey }),
  getRun: (id: string, signal?: AbortSignal) => api.get<ReconciliationRun>(`/api/v1/reconciliation/runs/${enc(id)}`, { signal }),
  discrepancies: (params: { status?: string; kind?: DiscrepancyKind }, signal?: AbortSignal) =>
    api.get<DataList<Discrepancy>>('/api/v1/reconciliation/discrepancies', { query: params, signal }),
  discrepancy: (id: string, signal?: AbortSignal) => api.get<Discrepancy>(`/api/v1/reconciliation/discrepancies/${enc(id)}`, { signal }),
  resolve: (id: string, input: { resolution: Resolution; justification: string }) =>
    api.post<Discrepancy>(`/api/v1/reconciliation/discrepancies/${enc(id)}/resolve`, input),
};

const RESOLUTION_HELP: Record<Resolution, string> = {
  accept_provider:
    'Adota o provedor: aplica o pagamento não refletido, sincroniza reembolso ou chargeback, aceita a taxa ou o valor pago com lançamento de ajuste no ledger, reconsulta a operação desconhecida ou cancela a cobrança inexistente no provedor.',
  keep_local: 'O registro local está correto; a diferença é do provedor e fica documentada.',
  adjust_ledger: 'Lança no ledger o ajuste pela diferença de taxa ou de valor pago, sem alterar a cobrança.',
  dismiss: 'A divergência não procede (por exemplo, já resolvida por outro meio).',
};

/**
 * Resoluções aceitas pela API para cada tipo de divergência (ADR-038). O valor
 * divergente de cobrança ainda não paga (`local_value.basis = 'charge'`) só
 * admite manter o registro local ou descartar: o ajuste e a adoção do
 * provedor valem apenas para o valor pago.
 */
export function resolutionsFor(kind: DiscrepancyKind, localValue?: Record<string, unknown>): Resolution[] {
  const all = Object.keys(RESOLUTION_HELP) as Resolution[];
  const unpaidAmount = kind === 'amount_mismatch' && localValue?.basis !== undefined && localValue.basis !== 'paid';
  return all.filter((r) => {
    if (r === 'adjust_ledger') return (kind === 'amount_mismatch' || kind === 'fee_mismatch') && !unpaidAmount;
    if (r === 'accept_provider') return kind !== 'missing_local' && !unpaidAmount;
    return true;
  });
}

/** Intervalo de consulta enquanto há execução de conciliação em andamento. */
export const RUN_POLL_INTERVAL_MS = 2_000;

/** Informa o resultado de uma execução encerrada. */
function announceFinishedRun(r: ReconciliationRun): void {
  // A falha (por exemplo, do provedor) encerra a execução como "failed", com a causa.
  if (r.status === 'failed') {
    notifyError(`A conciliação não foi concluída: ${r.error || 'falha ao consultar o provedor'}.`);
    return;
  }
  const summary = `${r.checked_count} cobrança(s) verificada(s), ${r.discrepancy_count} divergência(s)`;
  if (r.error) {
    notifyInfo(`${summary}. ${r.error}`, 'Conciliação concluída com aviso');
    return;
  }
  notifySuccess(`Conciliação concluída: ${summary}.`);
}

/** Detalhe da execução em andamento: aguardando o processamento ou retomada. */
function runProgress(r: ReconciliationRun): string | null {
  if (r.status !== 'running') return null;
  if (r.attempts === 0) return 'Aguardando o processamento';
  return r.attempts > 1 ? `Retomada depois de interrupção (tentativa ${r.attempts})` : null;
}

/**
 * Execuções de conciliação. O pedido manual é assíncrono: a API registra a
 * execução e responde de imediato (202); a tela acompanha a execução pedida
 * por consulta até ela terminar e então informa o resultado. A chave de
 * idempotência vale só para o pedido: é descartada assim que a API o aceita,
 * e o acompanhamento não repete o pedido.
 */
export function RunsTab({ pollIntervalMs = RUN_POLL_INTERVAL_MS }: { pollIntervalMs?: number }) {
  const { keys } = useTenant();
  const queryClient = useQueryClient();
  const idempotency = useIdempotencyKey();
  const [trackedId, setTrackedId] = useState<string | null>(null);
  const announced = useRef<string | null>(null);
  const query = useQuery({
    queryKey: keys('reconciliation', 'runs'),
    queryFn: ({ signal }) => reconciliationApi.runs(signal),
    // Execução em andamento (pedida aqui, por outra pessoa ou agendada): a
    // lista é atualizada até ela terminar.
    refetchInterval: (q) => (q.state.data?.data.some((r) => r.status === 'running') ? pollIntervalMs : false),
  });
  const tracked = useQuery({
    queryKey: keys('reconciliation', 'run', trackedId),
    queryFn: ({ signal }) => reconciliationApi.getRun(trackedId ?? '', signal),
    enabled: trackedId !== null,
    staleTime: 0,
    retry: false,
    refetchInterval: (q) => (q.state.data && q.state.data.status !== 'running' ? false : pollIntervalMs),
  });
  const finished = tracked.data && tracked.data.status !== 'running' ? tracked.data : null;
  // Recusa definitiva da consulta (por exemplo, sem acesso à execução)
  // encerra o acompanhamento; erro de rede ou 5xx continua tentando.
  const lost = tracked.error instanceof ApiError && tracked.error.status >= 400 && tracked.error.status < 500;
  useEffect(() => {
    if (trackedId === null || announced.current === trackedId) return;
    if (!finished && !lost) return;
    announced.current = trackedId;
    if (finished) announceFinishedRun(finished);
    else notifyError('Não foi possível acompanhar a execução; confira a situação na lista de execuções.');
    setTrackedId(null);
    void Promise.all(
      (['reconciliation', 'dashboard'] as const).map((domain) => queryClient.invalidateQueries({ queryKey: keys(domain) })),
    );
  }, [trackedId, finished, lost, keys, queryClient]);
  const run = useApiMutation<void, ReconciliationRun>({
    mutationFn: () => reconciliationApi.run(idempotency.keyFor({ operation: 'reconciliation-run' })),
    invalidate: ['reconciliation', 'dashboard'],
    // O pedido aceito (ou recusado, por exemplo com outra execução em
    // andamento) encerra a intenção: o próximo clique usa outra chave.
    idempotency,
    onSuccess: (r) => {
      if (r.status !== 'running') {
        announceFinishedRun(r);
        return;
      }
      announced.current = null;
      setTrackedId(r.id);
      notifyInfo('A execução foi registrada; o resultado aparece aqui quando ela terminar.', 'Conciliação em andamento');
    },
  });
  const inProgress = trackedId !== null || (query.data?.data.some((r) => r.status === 'running') ?? false);
  const onRun = async () => {
    const ok = await confirmAction({
      title: 'Executar conciliação agora',
      message: 'Compara as cobranças, pagamentos, taxas, reembolsos e chargebacks locais com o provedor e registra as divergências encontradas.',
      confirmLabel: 'Executar conciliação',
    });
    if (ok) run.mutate();
  };
  return (
    <Stack>
      <Group justify="flex-end">
        <Can permission="reconciliation.run">
          <Button leftSection={<IconPlayerPlay size={16} />} onClick={() => void onRun()} loading={run.isPending || trackedId !== null}>
            Executar conciliação agora
          </Button>
        </Can>
      </Group>
      {inProgress && (
        <Alert icon={<IconInfoCircle size={18} />} color="blue" role="status" title="Conciliação em andamento">
          A execução é feita em segundo plano e pode levar alguns minutos em contas grandes. Esta lista é atualizada até ela terminar; você
          pode sair desta página.
        </Alert>
      )}
      <QueryContent query={query}>
        {({ data }) =>
          data.length === 0 ? (
            <EmptyState title="Nenhuma execução" description="As execuções agendadas e manuais aparecem aqui." />
          ) : (
            <Table.ScrollContainer minWidth={940}>
              <Table aria-label="Execuções de conciliação">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Início</Table.Th>
                    <Table.Th>Origem</Table.Th>
                    <Table.Th>Provedor</Table.Th>
                    <Table.Th>Janela verificada</Table.Th>
                    <Table.Th>Situação</Table.Th>
                    <Table.Th ta="right">Verificadas</Table.Th>
                    <Table.Th ta="right">Divergências</Table.Th>
                    <Table.Th>Término</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {data.map((r) => (
                    <Table.Tr key={r.id}>
                      <Table.Td>
                        <DateTime value={r.started_at} />
                      </Table.Td>
                      <Table.Td>{labelOf(reconciliationRunTriggerLabels, r.trigger)}</Table.Td>
                      <Table.Td>{labelOf(providerLabels, r.provider)}</Table.Td>
                      <Table.Td>
                        <Text size="sm">
                          <DateTime value={r.window_start} /> – <DateTime value={r.window_end} />
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <StatusBadge status={r.status} labels={reconciliationRunStatusLabels} />
                        {runProgress(r) && (
                          <Text size="xs" c="dimmed">
                            {runProgress(r)}
                          </Text>
                        )}
                        {r.error && (
                          <Text size="xs" c={r.status === 'failed' ? 'red' : 'orange'}>
                            {r.error}
                          </Text>
                        )}
                      </Table.Td>
                      <Table.Td ta="right">{r.checked_count}</Table.Td>
                      <Table.Td ta="right">{r.discrepancy_count}</Table.Td>
                      <Table.Td>{r.finished_at ? <DateTime value={r.finished_at} /> : '—'}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )
        }
      </QueryContent>
    </Stack>
  );
}

function DiscrepanciesTab() {
  const { keys } = useTenant();
  const [filters, setFilters] = useUrlFilters(['situacao', 'tipo'] as const);
  const status = filters.situacao === 'todas' ? undefined : filters.situacao || 'open';
  const kind = (filters.tipo || undefined) as DiscrepancyKind | undefined;
  const query = useQuery({
    queryKey: keys('reconciliation', 'discrepancies', { status, kind }),
    queryFn: ({ signal }) => reconciliationApi.discrepancies({ status, kind }, signal),
  });
  return (
    <Stack>
      <Group align="flex-end">
        <Select
          label="Situação"
          data={[...optionsOf(discrepancyStatusLabels), { value: 'todas', label: 'Todas' }]}
          value={filters.situacao || 'open'}
          onChange={(v) => setFilters({ situacao: v === 'open' ? null : v })}
          allowDeselect={false}
          w={200}
        />
        <Select
          label="Tipo"
          placeholder="Todos"
          clearable
          data={optionsOf(discrepancyKindLabels)}
          value={kind ?? null}
          onChange={(v) => setFilters({ tipo: v })}
          w={300}
        />
      </Group>
      <QueryContent query={query}>
        {({ data }) =>
          data.length === 0 ? (
            <EmptyState title="Nenhuma divergência" description={status === 'open' ? 'Tudo conciliado com o provedor.' : 'Nenhuma divergência com esses filtros.'} icon={<IconScale size={26} />} />
          ) : (
            <Table.ScrollContainer minWidth={800}>
              <Table aria-label="Divergências">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Tipo</Table.Th>
                    <Table.Th>Situação</Table.Th>
                    <Table.Th>Cobrança</Table.Th>
                    <Table.Th>Identificador no provedor</Table.Th>
                    <Table.Th>Detectada em</Table.Th>
                    <Table.Th aria-label="Ações" />
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {data.map((d) => (
                    <Table.Tr key={d.id}>
                      <Table.Td>{labelOf(discrepancyKindLabels, d.kind)}</Table.Td>
                      <Table.Td>
                        <StatusBadge status={d.status} labels={discrepancyStatusLabels} />
                      </Table.Td>
                      <Table.Td>
                        {d.charge_id ? (
                          <Anchor component={Link} to={`/cobrancas/${d.charge_id}`} size="sm">
                            {d.charge_id.slice(0, 8)}
                          </Anchor>
                        ) : (
                          '—'
                        )}
                      </Table.Td>
                      <Table.Td>{d.provider_charge_id ? <Code>{d.provider_charge_id}</Code> : '—'}</Table.Td>
                      <Table.Td>
                        <DateTime value={d.created_at} />
                      </Table.Td>
                      <Table.Td>
                        <Anchor component={Link} to={`/conciliacao/divergencias/${d.id}`} size="sm" fw={500}>
                          {d.status === 'open' ? 'Analisar' : 'Ver detalhes'}
                        </Anchor>
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )
        }
      </QueryContent>
    </Stack>
  );
}

export function ReconciliationPage() {
  const [filters, setFilters] = useUrlFilters(['aba'] as const);
  const tab = filters.aba === 'divergencias' ? 'divergencias' : 'execucoes';
  return (
    <Stack gap={0}>
      <PageHeader
        title="Conciliação"
        description="Confronto dos registros locais com o provedor de pagamento: cobranças sem correspondência, valores e taxas divergentes, pagamentos não refletidos e operações com resultado desconhecido."
      />
      <Tabs value={tab} onChange={(v) => setFilters({ aba: v === 'execucoes' ? null : v })} keepMounted={false}>
        <Tabs.List mb="md">
          <Tabs.Tab value="execucoes">Execuções</Tabs.Tab>
          <Tabs.Tab value="divergencias">Divergências</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="execucoes">
          <RunsTab />
        </Tabs.Panel>
        <Tabs.Panel value="divergencias">
          <DiscrepanciesTab />
        </Tabs.Panel>
      </Tabs>
    </Stack>
  );
}

function formatValue(key: string, value: unknown, currency: string): string {
  if (value === undefined) return '—';
  if (typeof value === 'number' && Number.isInteger(value) && /amount|fee|total|value|paid|refund/i.test(key)) {
    return `${formatMoney(value, currency)} (${value})`;
  }
  if (value === null) return 'nulo';
  if (typeof value === 'object') return JSON.stringify(value);
  return String(value);
}

export function ComparisonTable({ discrepancy }: { discrepancy: Discrepancy }) {
  const local = discrepancy.local_value ?? {};
  const provider = discrepancy.provider_value ?? {};
  const currency = String(local.currency ?? provider.currency ?? 'BRL');
  const fields = Array.from(new Set([...Object.keys(local), ...Object.keys(provider)])).sort();
  if (fields.length === 0) return <Text size="sm" c="dimmed">Sem valores para comparar.</Text>;
  return (
    <Table.ScrollContainer minWidth={600}>
      <Table aria-label="Comparação local e provedor">
        <Table.Thead>
          <Table.Tr>
            <Table.Th>Campo</Table.Th>
            <Table.Th>Local (Recorvia)</Table.Th>
            <Table.Th>Provedor</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {fields.map((f) => {
            const differs = JSON.stringify(local[f]) !== JSON.stringify(provider[f]);
            return (
              <Table.Tr key={f} bg={differs ? 'yellow.0' : undefined}>
                <Table.Th scope="row">
                  <Code>{f}</Code>
                  {differs && <span className="visually-hidden"> (diferente)</span>}
                </Table.Th>
                <Table.Td fw={differs ? 600 : undefined}>{formatValue(f, local[f], currency)}</Table.Td>
                <Table.Td fw={differs ? 600 : undefined}>{formatValue(f, provider[f], currency)}</Table.Td>
              </Table.Tr>
            );
          })}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
  );
}

const resolveSchema = z.object({
  resolution: z
    .string()
    .refine((v): v is Resolution => v in RESOLUTION_HELP, 'Escolha como resolver.')
    .transform((v) => v as Resolution),
  justification: z
    .string()
    .trim()
    .min(10, 'A justificativa deve ter pelo menos 10 caracteres.')
    .max(2000, 'A justificativa deve ter no máximo 2000 caracteres.'),
});
type ResolveForm = z.input<typeof resolveSchema>;
type ResolveInput = z.output<typeof resolveSchema>;

export function ResolveDiscrepancyForm({ discrepancy }: { discrepancy: Discrepancy }) {
  const { user } = useSession();
  const form = useForm<ResolveForm, unknown, ResolveInput>({
    resolver: zodResolver(resolveSchema),
    defaultValues: { resolution: '', justification: '' },
  });
  const mutation = useApiMutation<ResolveInput, Discrepancy, ResolveForm>({
    mutationFn: (input) => reconciliationApi.resolve(discrepancy.id, input),
    invalidate: ['reconciliation', 'ledger', 'dashboard', 'charges', 'invoices'],
    successMessage: 'Divergência resolvida.',
    form,
  });
  const justification = useWatch({ control: form.control, name: 'justification' });
  const submit = form.handleSubmit(async (input) => {
    const ok = await confirmAction({
      title: 'Confirmar resolução',
      message: (
        <Stack gap="xs">
          <Text size="sm">
            Resolver como <strong>{labelOf(resolutionLabels, input.resolution)}</strong>? A decisão fica registrada na
            auditoria com seu nome e a justificativa, e não pode ser desfeita.
          </Text>
          <Text size="sm" c="dimmed">
            Responsável: {user.display_name} ({user.email})
          </Text>
        </Stack>
      ),
      confirmLabel: 'Resolver divergência',
      danger: input.resolution === 'accept_provider' || input.resolution === 'adjust_ledger',
    });
    if (ok) mutation.mutate(input);
  });
  return (
    <Paper withBorder p="md" radius="md">
      <form onSubmit={submit} noValidate aria-label="Resolver divergência">
        <Stack>
          <Title order={2} size="h5">
            Resolver divergência
          </Title>
          <Text size="sm">
            Responsável pela resolução: <strong>{user.display_name}</strong> ({user.email})
          </Text>
          <Controller
            control={form.control}
            name="resolution"
            render={({ field, fieldState }) => (
              <Radio.Group label="Resolução" required value={field.value} onChange={field.onChange} error={fieldState.error?.message}>
                <Stack gap="xs" mt="xs">
                  {resolutionsFor(discrepancy.kind, discrepancy.local_value).map((r) => (
                    <Radio key={r} value={r} label={resolutionLabels[r]} description={RESOLUTION_HELP[r]} />
                  ))}
                </Stack>
              </Radio.Group>
            )}
          />
          <Textarea
            label="Justificativa"
            description={`Mínimo de 10 caracteres. ${justification.trim().length}/2000`}
            required
            autosize
            minRows={3}
            maxLength={2000}
            {...form.register('justification')}
            error={form.formState.errors.justification?.message}
          />
          <Group justify="flex-end">
            <Button type="submit" loading={mutation.isPending}>
              Continuar
            </Button>
          </Group>
        </Stack>
      </form>
    </Paper>
  );
}

/**
 * Responsável pela resolução da divergência. A resolução automática é da
 * conciliação; a manual tem o ator no evento de resolução do histórico: o
 * usuário da sessão (também em `resolved_by`) ou a credencial de API, que
 * deixa `resolved_by` nulo.
 */
export function discrepancyResolver(d: Discrepancy): ReactNode {
  if (d.resolution === 'auto_resolved') return 'conciliação automática';
  const event = d.history?.find((e) => e.action === 'reconciliation.discrepancy_resolved');
  if (event?.actor_type === 'user') return event.actor_label || <ShortId id={event.actor_id ?? d.resolved_by} />;
  if (event) {
    const type = labelOf(actorTypeLabels, event.actor_type);
    return event.actor_label ? `${type} “${event.actor_label}”` : type;
  }
  return d.resolved_by ? <ShortId id={d.resolved_by} /> : 'não identificado no histórico';
}

export function DiscrepancyDetailPage() {
  const { id = '' } = useParams();
  const { keys } = useTenant();
  const canResolve = usePermission('reconciliation.resolve');
  const query = useQuery({ queryKey: keys('reconciliation', 'discrepancy', id), queryFn: ({ signal }) => reconciliationApi.discrepancy(id, signal) });
  return (
    <Stack gap="md">
      <Breadcrumbs>
        <Anchor component={Link} to="/conciliacao?aba=divergencias">
          Divergências
        </Anchor>
        <Text>{query.data ? labelOf(discrepancyKindLabels, query.data.kind) : 'Divergência'}</Text>
      </Breadcrumbs>
      <QueryContent query={query} loading={<LoadingState variant="detail" />}>
        {(d) => (
          <>
            <PageHeader title={labelOf(discrepancyKindLabels, d.kind)} badge={<StatusBadge status={d.status} labels={discrepancyStatusLabels} size="lg" />} />
            <Paper withBorder p="md" radius="md">
              <SimpleGrid cols={{ base: 1, xs: 2, md: 4 }}>
                <KeyValue label="Cobrança">
                  {d.charge_id ? (
                    <Anchor component={Link} to={`/cobrancas/${d.charge_id}`}>
                      {d.charge_id.slice(0, 8)}
                    </Anchor>
                  ) : (
                    '—'
                  )}
                </KeyValue>
                <KeyValue label="Identificador no provedor">{d.provider_charge_id ?? '—'}</KeyValue>
                <KeyValue label="Detectada em">
                  <DateTime value={d.created_at} />
                </KeyValue>
                <KeyValue label="Execução">
                  <ShortId id={d.run_id} />
                </KeyValue>
              </SimpleGrid>
            </Paper>
            <Stack gap="xs">
              <Title order={2} size="h4">
                Local × provedor
              </Title>
              <ComparisonTable discrepancy={d} />
            </Stack>
            {d.status !== 'open' ? (
              <Alert color={d.status === 'resolved' ? 'green' : 'gray'} title={labelOf(discrepancyStatusLabels, d.status)}>
                <Stack gap={4}>
                  <Text size="sm">Resolução: {labelOf(resolutionLabels, d.resolution)}</Text>
                  <Text size="sm">Justificativa: {d.justification ?? '—'}</Text>
                  <Text size="sm">
                    Responsável: {discrepancyResolver(d)} em{' '}
                    <DateTime value={d.resolved_at} />
                  </Text>
                </Stack>
              </Alert>
            ) : canResolve ? (
              <ResolveDiscrepancyForm discrepancy={d} />
            ) : (
              <Alert icon={<IconInfoCircle />} color="gray">
                Resolver divergências exige a permissão “Resolver divergências”.
              </Alert>
            )}
            <Stack gap="xs">
              <Title order={2} size="h4">
                Histórico
              </Title>
              <AuditEventList events={d.history ?? []} showResource={false} />
            </Stack>
          </>
        )}
      </QueryContent>
    </Stack>
  );
}
