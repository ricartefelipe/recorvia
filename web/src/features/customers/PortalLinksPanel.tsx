import { Alert, Button, Stack, Table, Text } from '@mantine/core';
import { IconBan, IconInfoCircle } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import type { CustomerDetails, PortalLink } from '../../api/types';
import { DateTime, StatusBadge } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { confirmAction, CursorPager } from '../../components/ui';
import { useApiMutation, useCursorPagination, useReturnFromEmptyPage } from '../../lib/hooks';
import { actorTypeLabels, labelOf, portalLinkStatusLabels } from '../../lib/labels';
import { useTenant } from '../../session/SessionContext';
import { customersApi } from './api';

const revokeReasonLabels: Record<string, string> = {
  superseded: 'Substituído por um link mais novo',
  revoked: 'Revogado pela organização',
};

/** Um link ainda dá acesso enquanto não foi revogado nem expirou sem uso. */
function revocable(link: PortalLink): boolean {
  return link.status === 'pending' || link.status === 'used';
}

function CreatedBy({ link }: { link: PortalLink }) {
  const type = labelOf(actorTypeLabels, link.created_by.type);
  return (
    <>
      {link.created_by.name || '—'}
      <Text size="xs" c="dimmed">
        {type}
      </Text>
    </>
  );
}

/**
 * Links do portal de um cliente: situação, datas e quem gerou. O endereço do
 * link nunca aparece aqui (é exibido uma única vez, na geração).
 */
export function PortalLinksPanel({ customer }: { customer: CustomerDetails }) {
  const { keys } = useTenant();
  const pagination = useCursorPagination(customer.id);
  const query = useQuery({
    queryKey: keys('customers', 'portal-links', customer.id, pagination.cursor ?? null),
    queryFn: ({ signal }) => customersApi.portalLinks(customer.id, { cursor: pagination.cursor }, signal),
    placeholderData: keepPreviousData,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);
  const revoke = useApiMutation<PortalLink, unknown>({
    mutationFn: (link) => customersApi.revokePortalLink(customer.id, link.id),
    invalidate: ['customers'],
    successMessage: (_, link) =>
      link.status === 'used'
        ? 'Link revogado. A sessão do portal aberta com ele foi encerrada.'
        : 'Link revogado. Ele não pode mais ser aberto.',
  });
  const onRevoke = async (link: PortalLink) => {
    const ok = await confirmAction({
      title: 'Revogar link do portal',
      message:
        link.status === 'used'
          ? `Revogar o link do portal de ${customer.name}? A sessão do portal aberta com ele é encerrada agora. Para dar acesso de novo, gere outro link.`
          : `Revogar o link do portal de ${customer.name}? Ele deixa de funcionar e não poderá ser aberto. Para dar acesso de novo, gere outro link.`,
      confirmLabel: 'Revogar link',
      danger: true,
    });
    if (ok) revoke.mutate(link);
  };

  return (
    <Stack>
      <Alert variant="light" icon={<IconInfoCircle />} p="xs">
        Cada cliente tem no máximo um link em vigor: gerar um novo link substitui o anterior. O endereço do link é exibido
        somente no momento da geração.
      </Alert>
      <QueryContent query={query} loading={<LoadingState />}>
        {(page) =>
          page.data.length === 0 && !pagination.hasPrevious ? (
            <EmptyState
              title="Nenhum link gerado"
              description="Use o botão “Link do portal” para dar ao cliente acesso às faturas e às preferências de contato."
            />
          ) : (
            <>
              <Table.ScrollContainer minWidth={760}>
                <Table aria-label="Links do portal">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>Situação</Table.Th>
                      <Table.Th>Gerado em</Table.Th>
                      <Table.Th>Gerado por</Table.Th>
                      <Table.Th>Expira em</Table.Th>
                      <Table.Th>Aberto em</Table.Th>
                      <Table.Th>Revogado em</Table.Th>
                      <Table.Th aria-label="Ações" />
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.data.map((link) => (
                      <Table.Tr key={link.id}>
                        <Table.Td>
                          <StatusBadge status={link.status} labels={portalLinkStatusLabels} />
                        </Table.Td>
                        <Table.Td>
                          <DateTime value={link.created_at} />
                        </Table.Td>
                        <Table.Td>
                          <CreatedBy link={link} />
                        </Table.Td>
                        <Table.Td>
                          <DateTime value={link.expires_at} />
                        </Table.Td>
                        <Table.Td>
                          <DateTime value={link.used_at} />
                        </Table.Td>
                        <Table.Td>
                          <DateTime value={link.revoked_at} />
                          {link.revoke_reason && (
                            <Text size="xs" c="dimmed">
                              {labelOf(revokeReasonLabels, link.revoke_reason)}
                            </Text>
                          )}
                        </Table.Td>
                        <Table.Td>
                          {revocable(link) && (
                            <Button
                              size="xs"
                              color="red"
                              variant="light"
                              leftSection={<IconBan size={14} />}
                              onClick={() => void onRevoke(link)}
                              loading={revoke.isPending && revoke.variables?.id === link.id}
                            >
                              Revogar
                            </Button>
                          )}
                        </Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
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
    </Stack>
  );
}
