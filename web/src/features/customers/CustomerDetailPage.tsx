import { Alert, Anchor, Badge, Breadcrumbs, Button, Group, Modal, Paper, SimpleGrid, Stack, Tabs, Text } from '@mantine/core';
import { IconArchive, IconInfoCircle, IconLink, IconPencil, IconUserOff } from '@tabler/icons-react';
import { keepPreviousData, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link, useParams } from 'react-router';
import type { CustomerDetails } from '../../api/types';
import { AuditEventList } from '../../components/AuditEventList';
import { DateTime, StatusBadge } from '../../components/format';
import { LoadingState, QueryContent } from '../../components/states';
import { confirmAction, CopyField, CursorPager, KeyValue, PageHeader } from '../../components/ui';
import { formatDateTime } from '../../lib/dates';
import { formatDocument } from '../../lib/documents';
import { useApiMutation, useCursorPagination, useUrlFilters } from '../../lib/hooks';
import { customerKindLabels, customerStatusLabels, labelOf } from '../../lib/labels';
import { Can, usePermission, useTenant } from '../../session/SessionContext';
import { AddressPanel } from './AddressPanel';
import { AnonymizeCustomerDialogs } from './AnonymizeCustomer';
import { customersApi } from './api';
import { ContactsPanel } from './ContactsPanel';
import { CreditsPanel } from './CreditsPanel';
import { PortalLinksPanel } from './PortalLinksPanel';
import { RelatedInvoices } from '../invoices/InvoicesTable';
import { CustomerSubscriptions } from '../subscriptions/CustomerSubscriptions';
import { CustomerForm } from './CustomerForm';

/** Histórico do cliente, do evento mais recente para o mais antigo, em páginas. */
function HistoryPanel({ customerId }: { customerId: string }) {
  const { keys } = useTenant();
  const pagination = useCursorPagination(customerId);
  const query = useQuery({
    queryKey: keys('customers', 'history', customerId, pagination.cursor ?? null),
    queryFn: ({ signal }) => customersApi.history(customerId, { cursor: pagination.cursor }, signal),
    placeholderData: keepPreviousData,
  });
  return (
    <QueryContent query={query}>
      {(page) => (
        <>
          <AuditEventList events={page.data} showResource={false} />
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
  );
}

function PortalLinkAction({ customer }: { customer: CustomerDetails }) {
  const { keys, timeZone } = useTenant();
  const queryClient = useQueryClient();
  const [link, setLink] = useState<{ url: string; expires_at: string } | null>(null);
  const mutation = useApiMutation<void, { url: string; expires_at: string }>({
    mutationFn: () => customersApi.createPortalLink(customer.id),
    onSuccess: (created) => {
      setLink(created);
      // Recarrega a lista de links (novo link e substituição do anterior) e o
      // histórico (a geração é auditada no cliente), nunca a ficha: este modal
      // fica dentro do conteúdo da consulta da ficha e uma falha na recarga dela
      // tiraria da tela o link, exibido uma única vez. As duas consultas
      // recarregadas têm estado de erro próprio, fora do caminho do modal.
      void queryClient.invalidateQueries({ queryKey: keys('customers', 'portal-links', customer.id) });
      void queryClient.invalidateQueries({ queryKey: keys('customers', 'history', customer.id) });
    },
  });
  return (
    <>
      <Button variant="default" leftSection={<IconLink size={16} />} onClick={() => mutation.mutate()} loading={mutation.isPending}>
        Link do portal
      </Button>
      <Modal opened={!!link} onClose={() => setLink(null)} title="Link do portal do cliente" size="lg">
        {link && (
          <Stack>
            <Text size="sm">
              Envie este link para {customer.name}. Ele é de uso único e expira em {formatDateTime(link.expires_at, timeZone)}.
              Ao abri-lo, o cliente acessa suas faturas e preferências de contato.
            </Text>
            <Text size="sm" c="dimmed">
              O link é exibido somente agora. Gerar um novo link invalida o anterior e encerra a sessão do portal aberta com ele.
              Para encerrar o acesso sem gerar outro link, revogue-o na aba Portal.
            </Text>
            <CopyField label="Link do portal" value={link.url} />
            <Group justify="flex-end">
              <Button onClick={() => setLink(null)}>Fechar</Button>
            </Group>
          </Stack>
        )}
      </Modal>
    </>
  );
}

function CustomerHeader({
  customer,
  onEdit,
  onAnonymize,
}: {
  customer: CustomerDetails;
  onEdit: () => void;
  onAnonymize: () => void;
}) {
  const { timeZone } = useTenant();
  const archive = useApiMutation<void, CustomerDetails>({
    mutationFn: () => customersApi.archive(customer.id),
    invalidate: ['customers'],
    successMessage: 'Cliente arquivado.',
  });
  const onArchive = async () => {
    const ok = await confirmAction({
      title: 'Arquivar cliente',
      message: `Arquivar ${customer.name}? Clientes arquivados não recebem novas assinaturas. O histórico e as faturas são mantidos.`,
      confirmLabel: 'Arquivar',
      danger: true,
    });
    if (ok) archive.mutate();
  };
  const active = customer.status === 'active';
  const anonymized = !!customer.anonymized_at;
  return (
    <PageHeader
      title={customer.name}
      badge={
        <Group gap={6}>
          <StatusBadge status={customer.status} labels={customerStatusLabels} />
          {anonymized && (
            <Badge variant="light" color="red">
              Anonimizado
            </Badge>
          )}
          <Badge variant="outline" color="gray">
            {labelOf(customerKindLabels, customer.kind)}
          </Badge>
        </Group>
      }
      description={
        customer.anonymized_at
          ? `Dados pessoais removidos em ${formatDateTime(customer.anonymized_at, timeZone)}`
          : `${customer.document_type.toUpperCase()} ${formatDocument(customer.document_number, customer.document_type)}`
      }
      actions={
        <>
          <Can permission="customers.portal_links">{active && <PortalLinkAction customer={customer} />}</Can>
          <Can permission="customers.write">
            {active && (
              <>
                <Button variant="default" leftSection={<IconPencil size={16} />} onClick={onEdit}>
                  Editar
                </Button>
                <Button color="red" variant="light" leftSection={<IconArchive size={16} />} onClick={() => void onArchive()} loading={archive.isPending}>
                  Arquivar
                </Button>
              </>
            )}
          </Can>
          <Can permission="customers.anonymize">
            {!anonymized && (
              <Button color="red" variant="subtle" leftSection={<IconUserOff size={16} />} onClick={onAnonymize}>
                Anonimizar
              </Button>
            )}
          </Can>
        </>
      }
    />
  );
}

function Overview({ customer }: { customer: CustomerDetails }) {
  return (
    <Stack>
      <Paper withBorder p="md" radius="md">
        <SimpleGrid cols={{ base: 1, xs: 2, md: 4 }} spacing="lg">
          <KeyValue label="E-mail">{customer.email || '—'}</KeyValue>
          <KeyValue label="Telefone">{customer.phone || '—'}</KeyValue>
          <KeyValue label="Identificador externo">{customer.external_id || '—'}</KeyValue>
          <KeyValue label="Prazo de pagamento">
            {customer.payment_terms_days === null || customer.payment_terms_days === undefined
              ? 'Padrão da organização'
              : `${customer.payment_terms_days} dias`}
          </KeyValue>
          {customer.kind === 'company' && <KeyValue label="Razão social">{customer.legal_name || '—'}</KeyValue>}
          <KeyValue label="Cadastrado em">
            <DateTime value={customer.created_at} />
          </KeyValue>
          <KeyValue label="Atualizado em">
            <DateTime value={customer.updated_at} />
          </KeyValue>
        </SimpleGrid>
      </Paper>
      <AddressPanel key={customer.version} customer={customer} />
    </Stack>
  );
}

const TABS = ['dados', 'contatos', 'assinaturas', 'faturas', 'creditos', 'portal', 'historico'] as const;

export function CustomerDetailPage() {
  const { id = '' } = useParams();
  const { keys } = useTenant();
  const [editing, setEditing] = useState(false);
  // Cliente cujo formulário de anonimização está aberto (o da rota em que foi aberto).
  const [anonymizingId, setAnonymizingId] = useState<string | null>(null);
  const [filters, setFilters] = useUrlFilters(['aba'] as const);
  // A aba Portal lista e revoga links: exige a mesma permissão da geração.
  const canManagePortalLinks = usePermission('customers.portal_links');
  const known = (TABS as readonly string[]).includes(filters.aba) && (filters.aba !== 'portal' || canManagePortalLinks);
  const tab = known ? filters.aba : 'dados';
  const query = useQuery({
    queryKey: keys('customers', 'detail', id),
    queryFn: ({ signal }) => customersApi.get(id, signal),
  });

  return (
    <Stack gap="md">
      <Breadcrumbs>
        <Anchor component={Link} to="/clientes">
          Clientes
        </Anchor>
        <Text>{query.data?.name ?? 'Cliente'}</Text>
      </Breadcrumbs>
      <QueryContent query={query} loading={<LoadingState variant="detail" />}>
        {(customer) => (
          <>
            <CustomerHeader customer={customer} onEdit={() => setEditing(true)} onAnonymize={() => setAnonymizingId(customer.id)} />
            {customer.anonymized_at && (
              <Alert variant="light" color="gray" icon={<IconInfoCircle />}>
                Os identificadores que este cliente tinha no provedor de pagamento saíram do cadastro e ficam registrados no
                evento de anonimização, na aba{' '}
                <Anchor component="button" type="button" size="sm" onClick={() => setFilters({ aba: 'historico' })}>
                  Histórico
                </Anchor>
                . O cadastro no provedor não é alterado pelo Recorvia.
              </Alert>
            )}
            <Tabs value={tab} onChange={(v) => setFilters({ aba: v === 'dados' ? null : v })} keepMounted={false}>
              <Tabs.List mb="md">
                <Tabs.Tab value="dados">Dados</Tabs.Tab>
                <Tabs.Tab value="contatos">Contatos ({customer.contacts.length})</Tabs.Tab>
                <Tabs.Tab value="assinaturas">Assinaturas</Tabs.Tab>
                <Tabs.Tab value="faturas">Faturas</Tabs.Tab>
                <Tabs.Tab value="creditos">Créditos</Tabs.Tab>
                {canManagePortalLinks && <Tabs.Tab value="portal">Portal</Tabs.Tab>}
                <Tabs.Tab value="historico">Histórico</Tabs.Tab>
              </Tabs.List>
              <Tabs.Panel value="dados">
                <Overview customer={customer} />
              </Tabs.Panel>
              <Tabs.Panel value="contatos">
                <ContactsPanel customer={customer} />
              </Tabs.Panel>
              <Tabs.Panel value="assinaturas">
                <CustomerSubscriptions customer={customer} />
              </Tabs.Panel>
              <Tabs.Panel value="faturas">
                <RelatedInvoices params={{ customer_id: customer.id }} emptyText="Este cliente ainda não tem faturas." />
              </Tabs.Panel>
              <Tabs.Panel value="creditos">
                <CreditsPanel customer={customer} />
              </Tabs.Panel>
              {canManagePortalLinks && (
                <Tabs.Panel value="portal">
                  <PortalLinksPanel customer={customer} />
                </Tabs.Panel>
              )}
              <Tabs.Panel value="historico">
                <HistoryPanel customerId={customer.id} />
              </Tabs.Panel>
            </Tabs>
            <Modal opened={editing} onClose={() => setEditing(false)} title="Editar cliente" size="lg">
              <CustomerForm customer={customer} onCancel={() => setEditing(false)} onSaved={() => setEditing(false)} />
            </Modal>
          </>
        )}
      </QueryContent>
      {/*
        Fora do QueryContent: a recarga da ficha (inclusive a que falha e troca a ficha pela tela de erro)
        não pode desmontar o formulário nem o diálogo com os identificadores do provedor.
      */}
      <Can permission="customers.anonymize">
        <AnonymizeCustomerDialogs
          key={id}
          customerId={id}
          customer={query.data}
          opened={anonymizingId === id}
          onClose={() => setAnonymizingId(null)}
        />
      </Can>
    </Stack>
  );
}
