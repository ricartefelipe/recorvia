import { zodResolver } from '@hookform/resolvers/zod';
import { ActionIcon, Badge, Button, Code, Group, Modal, Select, Stack, Table, Tabs, Text, TextInput, ThemeIcon, Tooltip } from '@mantine/core';
import { IconCheck, IconMinus, IconTrash, IconUserPlus, IconX } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { z } from 'zod';
import { api } from '../../api/client';
import type { CursorPage, DataList, Invitation, InvitationCreated, Member, Role, RolePermissions } from '../../api/types';
import { DateTime, StatusBadge } from '../../components/format';
import { EmptyState, QueryContent } from '../../components/states';
import { confirmAction, CursorPager, PageHeader, SecretRevealModal } from '../../components/ui';
import { useApiMutation, useCursorPagination, useUrlFilters } from '../../lib/hooks';
import { invitationStateLabels, labelOf, memberStatusLabels, roleLabels } from '../../lib/labels';
import { requiredEmail } from '../../lib/validation';
import { PERMISSIONS, permissionLabel } from '../../session/permissions';
import { usePermission, useSession, useTenant } from '../../session/SessionContext';

const enc = encodeURIComponent;
export const ROLES: Role[] = ['owner', 'admin', 'finance', 'operator', 'auditor'];
const roleOptions = ROLES.map((r) => ({ value: r, label: roleLabels[r] }));

export const membersApi = {
  members: (signal?: AbortSignal) => api.get<DataList<Member>>('/api/v1/members', { signal }),
  updateRole: (member: Member, role: Role) =>
    api.patch<Member>(`/api/v1/members/${enc(member.user_id)}`, { role }, { ifMatch: member.version }),
  remove: (member: Member) => api.delete(`/api/v1/members/${enc(member.user_id)}`, { ifMatch: member.version }),
  invitations: (params: { cursor?: string; limit?: number }, signal?: AbortSignal) =>
    api.get<CursorPage<Invitation>>('/api/v1/invitations', { query: params, signal }),
  invite: (input: { email: string; role: Role }) => api.post<InvitationCreated>('/api/v1/invitations', input),
  revoke: (id: string) => api.delete(`/api/v1/invitations/${enc(id)}`),
  roles: (signal?: AbortSignal) => api.get<DataList<RolePermissions>>('/api/v1/roles', { signal }),
};

export function useRoles() {
  const { keys } = useTenant();
  return useQuery({ queryKey: keys('roles', 'list'), queryFn: ({ signal }) => membersApi.roles(signal), staleTime: 5 * 60_000 });
}

function MembersTab() {
  const { keys } = useTenant();
  const { user } = useSession();
  const canManage = usePermission('members.manage');
  const query = useQuery({ queryKey: keys('members', 'list'), queryFn: ({ signal }) => membersApi.members(signal) });
  const changeRole = useApiMutation<{ member: Member; role: Role }, Member>({
    mutationFn: ({ member, role }) => membersApi.updateRole(member, role),
    invalidate: ['members'],
    successMessage: (m) => `Papel de ${m.display_name} alterado para ${labelOf(roleLabels, m.role)}.`,
  });
  const remove = useApiMutation<Member, unknown>({
    mutationFn: (m) => membersApi.remove(m),
    invalidate: ['members'],
    successMessage: 'Membro removido.',
  });
  const onRole = async (member: Member, role: Role) => {
    if (role === member.role) return;
    const self = member.user_id === user.id;
    const ok = await confirmAction({
      title: 'Alterar papel',
      message: `Alterar o papel de ${member.display_name} de ${labelOf(roleLabels, member.role)} para ${labelOf(roleLabels, role)}?${self ? ' Você está alterando o seu próprio papel e pode perder acessos.' : ''}`,
      confirmLabel: 'Alterar papel',
      danger: self,
    });
    if (ok) changeRole.mutate({ member, role });
  };
  const onRemove = async (member: Member) => {
    const ok = await confirmAction({
      title: 'Remover membro',
      message: `Remover ${member.display_name} (${member.email}) da organização? O acesso é encerrado imediatamente. O último proprietário não pode ser removido.`,
      confirmLabel: 'Remover',
      danger: true,
    });
    if (ok) remove.mutate(member);
  };
  return (
    <QueryContent query={query}>
      {({ data }) =>
        data.length === 0 ? (
          <EmptyState title="Nenhum membro" />
        ) : (
          <Table.ScrollContainer minWidth={760}>
            <Table aria-label="Membros">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Nome</Table.Th>
                  <Table.Th>E-mail</Table.Th>
                  <Table.Th>Papel</Table.Th>
                  <Table.Th>Situação</Table.Th>
                  <Table.Th>Membro desde</Table.Th>
                  {canManage && <Table.Th aria-label="Ações" />}
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {data.map((m) => {
                  const active = m.status === 'active';
                  return (
                    <Table.Tr key={m.user_id}>
                      <Table.Td>
                        <Group gap={6}>
                          <Text size="sm" fw={500}>
                            {m.display_name}
                          </Text>
                          {m.user_id === user.id && (
                            <Badge size="xs" variant="outline">
                              Você
                            </Badge>
                          )}
                        </Group>
                      </Table.Td>
                      <Table.Td>{m.email}</Table.Td>
                      <Table.Td>
                        {canManage && active ? (
                          <Select
                            aria-label={`Papel de ${m.display_name}`}
                            data={roleOptions}
                            value={m.role}
                            onChange={(v) => v && void onRole(m, v as Role)}
                            allowDeselect={false}
                            size="xs"
                            w={170}
                            disabled={changeRole.isPending && changeRole.variables?.member.user_id === m.user_id}
                          />
                        ) : (
                          labelOf(roleLabels, m.role)
                        )}
                      </Table.Td>
                      <Table.Td>
                        <StatusBadge status={m.status} labels={memberStatusLabels} />
                      </Table.Td>
                      <Table.Td>
                        <DateTime value={m.created_at} dateOnly />
                      </Table.Td>
                      {canManage && (
                        <Table.Td>
                          {active && (
                            <Tooltip label="Remover da organização">
                              <ActionIcon variant="subtle" color="red" aria-label={`Remover ${m.display_name}`} onClick={() => void onRemove(m)}>
                                <IconTrash size={16} />
                              </ActionIcon>
                            </Tooltip>
                          )}
                        </Table.Td>
                      )}
                    </Table.Tr>
                  );
                })}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )
      }
    </QueryContent>
  );
}

const inviteSchema = z.object({ email: requiredEmail, role: z.enum(['owner', 'admin', 'finance', 'operator', 'auditor']) });
type InviteForm = z.input<typeof inviteSchema>;

function InviteModal({ opened, onClose, onCreated }: { opened: boolean; onClose: () => void; onCreated: (c: InvitationCreated) => void }) {
  const form = useForm<InviteForm>({ resolver: zodResolver(inviteSchema), defaultValues: { email: '', role: 'operator' } });
  const mutation = useApiMutation<InviteForm, InvitationCreated, InviteForm>({
    mutationFn: membersApi.invite,
    invalidate: ['invitations'],
    successMessage: 'Convite criado.',
    form,
    onSuccess: (created) => {
      form.reset();
      onClose();
      onCreated(created);
    },
  });
  return (
    <Modal opened={opened} onClose={onClose} title="Convidar membro">
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
        <Stack>
          <TextInput label="E-mail" type="email" required {...form.register('email')} error={form.formState.errors.email?.message} />
          <Controller
            control={form.control}
            name="role"
            render={({ field }) => (
              <Select label="Papel" required data={roleOptions} value={field.value} onChange={(v) => v && field.onChange(v)} allowDeselect={false} />
            )}
          />
          <Text size="xs" c="dimmed">
            O convite expira e vale uma única vez. O limite de usuários do seu plano do Recorvia se aplica.
          </Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Criar convite
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

function InvitationsTab() {
  const { keys } = useTenant();
  const canManage = usePermission('members.manage');
  const [inviting, setInviting] = useState(false);
  const [created, setCreated] = useState<InvitationCreated | null>(null);
  const pagination = useCursorPagination('invitations');
  const query = useQuery({
    queryKey: keys('invitations', 'list', pagination.cursor),
    queryFn: ({ signal }) => membersApi.invitations({ cursor: pagination.cursor, limit: 25 }, signal),
    placeholderData: keepPreviousData,
  });
  const revoke = useApiMutation<Invitation, unknown>({
    mutationFn: (i) => membersApi.revoke(i.id),
    invalidate: ['invitations'],
    successMessage: 'Convite revogado.',
  });
  const onRevoke = async (i: Invitation) => {
    const ok = await confirmAction({ title: 'Revogar convite', message: `Revogar o convite para ${i.email}? O link deixa de funcionar.`, confirmLabel: 'Revogar', danger: true });
    if (ok) revoke.mutate(i);
  };
  return (
    <Stack>
      {canManage && (
        <Group justify="flex-end">
          <Button leftSection={<IconUserPlus size={16} />} onClick={() => setInviting(true)}>
            Convidar membro
          </Button>
        </Group>
      )}
      <QueryContent query={query}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhum convite" description="Convide pessoas da sua equipe escolhendo o papel de cada uma." />
          ) : (
            <>
              <Table.ScrollContainer minWidth={700}>
                <Table aria-label="Convites">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>E-mail</Table.Th>
                      <Table.Th>Papel</Table.Th>
                      <Table.Th>Situação</Table.Th>
                      <Table.Th>Expira em</Table.Th>
                      <Table.Th>Criado em</Table.Th>
                      {canManage && <Table.Th aria-label="Ações" />}
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.data.map((i) => (
                      <Table.Tr key={i.id}>
                        <Table.Td>{i.email}</Table.Td>
                        <Table.Td>{labelOf(roleLabels, i.role)}</Table.Td>
                        <Table.Td>
                          <StatusBadge status={i.state} labels={invitationStateLabels} />
                        </Table.Td>
                        <Table.Td>
                          <DateTime value={i.expires_at} />
                        </Table.Td>
                        <Table.Td>
                          <DateTime value={i.created_at} />
                        </Table.Td>
                        {canManage && (
                          <Table.Td>
                            {i.state === 'pending' && (
                              <Tooltip label="Revogar convite">
                                <ActionIcon variant="subtle" color="red" aria-label={`Revogar convite de ${i.email}`} onClick={() => void onRevoke(i)}>
                                  <IconX size={16} />
                                </ActionIcon>
                              </Tooltip>
                            )}
                          </Table.Td>
                        )}
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
      <InviteModal opened={inviting} onClose={() => setInviting(false)} onCreated={setCreated} />
      <SecretRevealModal
        opened={!!created}
        title="Link do convite"
        description={created ? `Envie este link para ${created.invitation.email}. Ao abri-lo, a pessoa entra com a própria conta e aceita o convite.` : undefined}
        value={created?.accept_url ?? null}
        valueLabel="Link de aceite"
        onClose={() => setCreated(null)}
      />
    </Stack>
  );
}

export function RoleMatrix({ roles }: { roles: RolePermissions[] }) {
  const byRole = new Map(roles.map((r) => [r.role, new Set(r.permissions)]));
  const all = new Set(roles.flatMap((r) => r.permissions));
  const ordered = [...PERMISSIONS.filter((p) => all.has(p)), ...[...all].filter((p) => !(PERMISSIONS as readonly string[]).includes(p)).sort()];
  const columns = ROLES.filter((r) => byRole.has(r));
  return (
    <Table.ScrollContainer minWidth={820}>
      <Table aria-label="Matriz de papéis e permissões" striped highlightOnHover>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>Permissão</Table.Th>
            {columns.map((r) => (
              <Table.Th key={r} ta="center">
                {labelOf(roleLabels, r)}
              </Table.Th>
            ))}
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {ordered.map((p) => (
            <Table.Tr key={p}>
              <Table.Th scope="row" fw={400}>
                <Text size="sm">{permissionLabel(p)}</Text>
                <Code fz="xs">{p}</Code>
              </Table.Th>
              {columns.map((r) => {
                const allowed = byRole.get(r)?.has(p) ?? false;
                return (
                  <Table.Td key={r} ta="center">
                    <ThemeIcon
                      variant={allowed ? 'light' : 'subtle'}
                      color={allowed ? 'green' : 'gray'}
                      size="sm"
                      radius="xl"
                      role="img"
                      aria-label={allowed ? 'Permitido' : 'Não permitido'}
                    >
                      {allowed ? <IconCheck size={14} /> : <IconMinus size={14} />}
                    </ThemeIcon>
                  </Table.Td>
                );
              })}
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
  );
}

function RolesTab() {
  const query = useRoles();
  return <QueryContent query={query}>{({ data }) => <RoleMatrix roles={data} />}</QueryContent>;
}

export function MembersPage() {
  const [filters, setFilters] = useUrlFilters(['aba'] as const);
  const tab = ['convites', 'papeis'].includes(filters.aba) ? filters.aba : 'membros';
  return (
    <Stack gap={0}>
      <PageHeader title="Usuários e permissões" description="Quem acessa a organização e o que cada papel pode fazer. O último proprietário não pode ser removido nem rebaixado." />
      <Tabs value={tab} onChange={(v) => setFilters({ aba: v === 'membros' ? null : v })} keepMounted={false}>
        <Tabs.List mb="md">
          <Tabs.Tab value="membros">Membros</Tabs.Tab>
          <Tabs.Tab value="convites">Convites</Tabs.Tab>
          <Tabs.Tab value="papeis">Papéis e permissões</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="membros">
          <MembersTab />
        </Tabs.Panel>
        <Tabs.Panel value="convites">
          <InvitationsTab />
        </Tabs.Panel>
        <Tabs.Panel value="papeis">
          <RolesTab />
        </Tabs.Panel>
      </Tabs>
    </Stack>
  );
}
