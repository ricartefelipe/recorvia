import { Alert, Anchor, Badge, Breadcrumbs, Button, Checkbox, Group, Paper, SimpleGrid, Stack, Table, Text, Title } from '@mantine/core';
import { IconExternalLink, IconFileText, IconInfoCircle } from '@tabler/icons-react';
import { useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link, useParams } from 'react-router';
import { portalApi } from '../../api/client';
import { isApiError } from '../../api/http';
import { PORTAL_SESSION_KEY, portalKey } from '../../api/queryKeys';
import type { PortalInvoice, PortalInvoicePage, PortalSession } from '../../api/types';
import { DocumentViewer } from '../../components/DocumentViewer';
import { FiscalNotice } from '../../components/FiscalNotice';
import { CivilDate, Money, StatusBadge } from '../../components/format';
import { EmptyState, ErrorState, LoadingState, QueryContent } from '../../components/states';
import { KeyValue } from '../../components/ui';
import { formatInstantDate } from '../../lib/dates';
import { handleMutationError } from '../../lib/errors';
import { portalInvoiceStatusLabels } from '../../lib/labels';
import { notifySuccess } from '../../lib/notify';
import { portalTimeZone, usePortalSession } from './PortalLayout';
import { ResponsiveTable } from '../../components/ResponsiveTable';

const enc = encodeURIComponent;
const portalInvoicesApi = {
  list: (cursor: string | undefined, signal?: AbortSignal) =>
    portalApi.get<PortalInvoicePage>('/api/portal/v1/invoices', { query: { cursor }, signal }),
  get: (id: string, signal?: AbortSignal) => portalApi.get<PortalInvoice>(`/api/portal/v1/invoices/${enc(id)}`, { signal }),
  document: (id: string) => portalApi.get<string>(`/api/portal/v1/invoices/${enc(id)}/document`, { responseType: 'text' }),
  updatePreferences: (contacts: { id: string; receives_billing_email: boolean }[]) =>
    portalApi.put<PortalSession>('/api/portal/v1/contact-preferences', { contacts }),
};

function payable(invoice: PortalInvoice): boolean {
  return invoice.amount_due > 0 && !!invoice.checkout_url && (invoice.status === 'open' || invoice.status === 'partially_paid');
}

function PayButton({ invoice, size = 'sm' }: { invoice: PortalInvoice; size?: 'xs' | 'sm' | 'md' }) {
  if (!payable(invoice)) return null;
  return (
    <Button component="a" href={invoice.checkout_url ?? undefined} target="_blank" rel="noopener noreferrer" size={size} rightSection={<IconExternalLink size={14} />}>
      Pagar agora
      <span className="visually-hidden"> (abre o pagamento seguro em nova aba)</span>
    </Button>
  );
}

function StatusBadges({ invoice }: { invoice: PortalInvoice }) {
  return (
    <Group gap={4} wrap="nowrap">
      <StatusBadge status={invoice.status} labels={portalInvoiceStatusLabels} />
      {invoice.overdue && (
        <Badge color="red" variant="filled" size="sm">
          Vencida
        </Badge>
      )}
    </Group>
  );
}

export function PortalInvoicesPage() {
  const session = usePortalSession();
  // Páginas acumuladas: "Carregar mais" busca a seguinte pelo cursor da última.
  const query = useInfiniteQuery({
    queryKey: portalKey(session.customer.id, 'invoices'),
    queryFn: ({ pageParam, signal }) => portalInvoicesApi.list(pageParam, signal),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
  const invoices = query.data?.pages.flatMap((page) => page.data) ?? [];
  const shown = `${invoices.length} ${invoices.length === 1 ? 'fatura exibida' : 'faturas exibidas'}`;
  return (
    <Stack>
      <Title order={1} size="h2">
        Suas faturas
      </Title>
      {query.isPending ? (
        <LoadingState />
      ) : query.isError && invoices.length === 0 ? (
        <ErrorState error={query.error} onRetry={() => void query.refetch()} />
      ) : invoices.length === 0 ? (
        <EmptyState title="Nenhuma fatura" description="Quando houver faturas emitidas para você, elas aparecerão aqui." />
      ) : (
        <>
          <ResponsiveTable minWidth={720}>
            <Table aria-label="Faturas">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Número</Table.Th>
                  <Table.Th>Situação</Table.Th>
                  <Table.Th>Emissão</Table.Th>
                  <Table.Th>Vencimento</Table.Th>
                  <Table.Th ta="right">Total</Table.Th>
                  <Table.Th ta="right">A pagar</Table.Th>
                  <Table.Th aria-label="Ações" />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {invoices.map((i) => (
                  <Table.Tr key={i.id}>
                    <Table.Td>
                      <Anchor component={Link} to={`/portal/faturas/${i.id}`} fw={500}>
                        {i.number}
                      </Anchor>
                    </Table.Td>
                    <Table.Td>
                      <StatusBadges invoice={i} />
                    </Table.Td>
                    <Table.Td>{formatInstantDate(i.issued_at, portalTimeZone)}</Table.Td>
                    <Table.Td>
                      <CivilDate value={i.due_date} />
                    </Table.Td>
                    <Table.Td ta="right">
                      <Money value={i.total} currency={i.currency} />
                    </Table.Td>
                    <Table.Td ta="right">
                      <Money value={i.amount_due} currency={i.currency} strong={i.amount_due > 0} />
                    </Table.Td>
                    <Table.Td>
                      <PayButton invoice={i} size="xs" />
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </ResponsiveTable>
          {query.isFetchNextPageError && <ErrorState error={query.error} onRetry={() => void query.fetchNextPage()} />}
          <Group justify="space-between" component="nav" aria-label="Paginação">
            <Text size="sm" c="dimmed" role="status">
              {query.hasNextPage ? `${shown}; há faturas mais antigas.` : `${shown}; não há mais faturas.`}
            </Text>
            {query.hasNextPage && (
              <Button variant="default" onClick={() => void query.fetchNextPage()} loading={query.isFetchingNextPage}>
                Carregar mais faturas
              </Button>
            )}
          </Group>
        </>
      )}
    </Stack>
  );
}

export function PortalInvoicePage() {
  const { id = '' } = useParams();
  const session = usePortalSession();
  const [doc, setDoc] = useState<{ opened: boolean; html: string | null; loading: boolean; error: unknown }>({ opened: false, html: null, loading: false, error: null });
  const query = useQuery({ queryKey: portalKey(session.customer.id, 'invoice', id), queryFn: ({ signal }) => portalInvoicesApi.get(id, signal) });
  const openDocument = async () => {
    setDoc({ opened: true, html: null, loading: true, error: null });
    try {
      setDoc({ opened: true, html: await portalInvoicesApi.document(id), loading: false, error: null });
    } catch (error) {
      setDoc({ opened: true, html: null, loading: false, error });
    }
  };
  return (
    <Stack>
      <Breadcrumbs>
        <Anchor component={Link} to="/portal">
          Faturas
        </Anchor>
        <Text>{query.data?.number ?? 'Fatura'}</Text>
      </Breadcrumbs>
      {isApiError(query.error, 404) ? (
        <EmptyState
          title="Fatura não encontrada"
          description="A fatura não existe ou não está disponível para você."
          action={
            <Button component={Link} to="/portal" variant="light">
              Voltar às faturas
            </Button>
          }
        />
      ) : (
        <QueryContent query={query} loading={<LoadingState variant="detail" />}>
          {(invoice) => (
            <>
              <Group justify="space-between" align="flex-start">
                <Group gap="sm">
                  <Title order={1} size="h2">
                    Fatura {invoice.number}
                  </Title>
                  <StatusBadges invoice={invoice} />
                </Group>
                <Group gap="xs">
                  <Button variant="default" leftSection={<IconFileText size={16} />} onClick={() => void openDocument()}>
                    Ver documento
                  </Button>
                  <PayButton invoice={invoice} size="sm" />
                </Group>
              </Group>
              <FiscalNotice />
              {payable(invoice) && (
                <Alert icon={<IconInfoCircle />} variant="light">
                  O pagamento é feito no ambiente seguro do provedor de pagamentos. Seus dados de cartão não passam por esta
                  página.
                </Alert>
              )}
              <Paper withBorder p="md" radius="md">
                <SimpleGrid cols={{ base: 2, md: 4 }}>
                  <KeyValue label="Emissão">{formatInstantDate(invoice.issued_at, portalTimeZone)}</KeyValue>
                  <KeyValue label="Vencimento">
                    <CivilDate value={invoice.due_date} />
                  </KeyValue>
                  <KeyValue label="Total">
                    <Money value={invoice.total} currency={invoice.currency} />
                  </KeyValue>
                  <KeyValue label="A pagar">
                    <Money value={invoice.amount_due} currency={invoice.currency} strong />
                  </KeyValue>
                </SimpleGrid>
              </Paper>
              <Table aria-label="Itens da fatura">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Descrição</Table.Th>
                    <Table.Th ta="right">Valor</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {invoice.items.map((item, index) => (
                    <Table.Tr key={`${index}-${item.description}`}>
                      <Table.Td>{item.description}</Table.Td>
                      <Table.Td ta="right">
                        <Money value={item.amount} currency={invoice.currency} />
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
              <DocumentViewer
                opened={doc.opened}
                onClose={() => setDoc((d) => ({ ...d, opened: false }))}
                title={`Fatura ${invoice.number}`}
                html={doc.html}
                loading={doc.loading}
                error={doc.error}
                filename={`fatura-${invoice.number}.html`}
              />
            </>
          )}
        </QueryContent>
      )}
    </Stack>
  );
}

export function PortalPreferencesPage() {
  const session = usePortalSession();
  const queryClient = useQueryClient();
  const [values, setValues] = useState<Record<string, boolean>>(() =>
    Object.fromEntries(session.contacts.map((c) => [c.id, c.receives_billing_email])),
  );
  const [saving, setSaving] = useState(false);
  const save = async () => {
    setSaving(true);
    try {
      const updated = await portalInvoicesApi.updatePreferences(session.contacts.map((c) => ({ id: c.id, receives_billing_email: !!values[c.id] })));
      queryClient.setQueryData(PORTAL_SESSION_KEY, updated);
      notifySuccess('Preferências gravadas.');
    } catch (error) {
      handleMutationError(error);
    } finally {
      setSaving(false);
    }
  };
  return (
    <Stack maw={720}>
      <Title order={1} size="h2">
        Preferências de contato
      </Title>
      <Text c="dimmed" size="sm">
        Escolha quais contatos recebem por e-mail as faturas e os lembretes de pagamento.
      </Text>
      {session.contacts.length === 0 ? (
        <EmptyState title="Nenhum contato cadastrado" description="Peça à empresa que emitiu a fatura para cadastrar seus contatos." />
      ) : (
        <Paper withBorder p="md" radius="md">
          <Stack>
            {session.contacts.map((c) => (
              <Checkbox
                key={c.id}
                label={`${c.name} (${c.email})`}
                description={c.role || undefined}
                checked={!!values[c.id]}
                onChange={(e) => {
                  const checked = e.currentTarget.checked;
                  setValues((v) => ({ ...v, [c.id]: checked }));
                }}
              />
            ))}
            <Group justify="flex-end">
              <Button onClick={() => void save()} loading={saving}>
                Salvar preferências
              </Button>
            </Group>
          </Stack>
        </Paper>
      )}
    </Stack>
  );
}
