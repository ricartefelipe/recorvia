import { zodResolver } from '@hookform/resolvers/zod';
import { ActionIcon, Badge, Button, Code, Group, Modal, MultiSelect, NumberInput, Stack, Table, Text, TextInput, Tooltip } from '@mantine/core';
import { IconKey, IconPlus, IconRotate, IconTrash } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { z } from 'zod';
import type { ApiCredential, ApiCredentialWithSecret } from '../../api/types';
import { DateTime } from '../../components/format';
import { EmptyState, QueryContent } from '../../components/states';
import { confirmAction, SecretRevealModal } from '../../components/ui';
import { useApiMutation } from '../../lib/hooks';
import { optionalInt, requiredText } from '../../lib/validation';
import { PERMISSIONS, permissionLabel } from '../../session/permissions';
import { Can, usePermission, useTenant } from '../../session/SessionContext';
import { useRoles } from '../members/MembersPage';
import { integrationsApi } from './api';

const credentialSchema = z.object({
  name: requiredText(3, 100, 'O nome'),
  scopes: z.array(z.string()).min(1, 'Escolha pelo menos um escopo.'),
  expires_in_days: optionalInt(1, 730),
});
type CredentialForm = z.input<typeof credentialSchema>;
type CredentialInput = z.output<typeof credentialSchema>;

function credentialState(c: ApiCredential): { label: string; color: string } {
  if (c.revoked_at) return { label: 'Revogada', color: 'gray' };
  if (c.expires_at && new Date(c.expires_at).getTime() < Date.now()) return { label: 'Expirada', color: 'orange' };
  return { label: 'Ativa', color: 'green' };
}

function CreateCredentialModal({ opened, onClose, onCreated }: { opened: boolean; onClose: () => void; onCreated: (c: ApiCredentialWithSecret) => void }) {
  const roles = useRoles();
  const available = new Set((roles.data?.data ?? []).flatMap((r) => r.permissions));
  const scopes = [...PERMISSIONS.filter((p) => available.has(p)), ...[...available].filter((p) => !(PERMISSIONS as readonly string[]).includes(p))];
  const form = useForm<CredentialForm, unknown, CredentialInput>({
    resolver: zodResolver(credentialSchema),
    defaultValues: { name: '', scopes: [], expires_in_days: 365 },
  });
  const mutation = useApiMutation<CredentialInput, ApiCredentialWithSecret, CredentialForm>({
    mutationFn: integrationsApi.createCredential,
    invalidate: ['api-credentials'],
    successMessage: 'Credencial criada.',
    form,
    onSuccess: (c) => {
      form.reset();
      onClose();
      onCreated(c);
    },
  });
  return (
    <Modal opened={opened} onClose={onClose} title="Nova credencial de API" size="lg">
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
        <Stack>
          <TextInput label="Nome" placeholder="ERP interno" required {...form.register('name')} error={form.formState.errors.name?.message} />
          <Controller
            control={form.control}
            name="scopes"
            render={({ field, fieldState }) => (
              <MultiSelect
                label="Escopos"
                description="Conceda apenas o necessário para a integração."
                required
                searchable
                data={scopes.map((p) => ({ value: p, label: `${permissionLabel(p)} (${p})` }))}
                value={field.value}
                onChange={field.onChange}
                error={fieldState.error?.message}
                placeholder={roles.isPending ? 'Carregando…' : 'Selecione'}
              />
            )}
          />
          <Controller
            control={form.control}
            name="expires_in_days"
            render={({ field, fieldState }) => (
              <NumberInput
                label="Validade (dias)"
                description="De 1 a 730 dias. Vazio para sem expiração."
                min={1}
                max={730}
                allowDecimal={false}
                value={field.value}
                onChange={(v) => field.onChange(v === '' ? '' : Number(v))}
                error={fieldState.error?.message}
              />
            )}
          />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Criar credencial
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

export function CredentialsTab() {
  const { keys } = useTenant();
  const canManage = usePermission('api_credentials.manage');
  const [creating, setCreating] = useState(false);
  const [secret, setSecret] = useState<{ title: string; value: string } | null>(null);
  const query = useQuery({ queryKey: keys('api-credentials', 'list'), queryFn: ({ signal }) => integrationsApi.credentials(signal) });
  const rotate = useApiMutation<ApiCredential, ApiCredentialWithSecret>({
    mutationFn: (c) => integrationsApi.rotateCredential(c.id),
    invalidate: ['api-credentials'],
    successMessage: 'Credencial rotacionada. A anterior vale por mais 24 horas.',
    onSuccess: (r) => setSecret({ title: `Novo segredo de ${r.credential.name}`, value: r.secret }),
  });
  const revoke = useApiMutation<ApiCredential, unknown>({
    mutationFn: (c) => integrationsApi.revokeCredential(c.id),
    invalidate: ['api-credentials'],
    successMessage: 'Credencial revogada.',
  });
  return (
    <Stack>
      <Group justify="space-between">
        <Text size="sm" c="dimmed" maw={640}>
          Integrações autenticam com <Code>Authorization: Bearer rcv_…</Code>. Cada credencial pertence a esta organização e
          tem escopos próprios.
        </Text>
        <Can permission="api_credentials.manage">
          <Button leftSection={<IconPlus size={16} />} onClick={() => setCreating(true)}>
            Nova credencial
          </Button>
        </Can>
      </Group>
      <QueryContent query={query}>
        {({ data }) =>
          data.length === 0 ? (
            <EmptyState title="Nenhuma credencial" description="Crie uma credencial para integrar seu ERP, CRM ou o envio de eventos de consumo." icon={<IconKey size={26} />} />
          ) : (
            <Table.ScrollContainer minWidth={900}>
              <Table aria-label="Credenciais de API">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Nome</Table.Th>
                    <Table.Th>Prefixo</Table.Th>
                    <Table.Th>Escopos</Table.Th>
                    <Table.Th>Situação</Table.Th>
                    <Table.Th>Criada em</Table.Th>
                    <Table.Th>Expira em</Table.Th>
                    <Table.Th>Último uso</Table.Th>
                    {canManage && <Table.Th aria-label="Ações" />}
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {data.map((c) => {
                    const state = credentialState(c);
                    return (
                      <Table.Tr key={c.id}>
                        <Table.Td fw={500}>{c.name}</Table.Td>
                        <Table.Td>
                          <Code>{c.prefix}…</Code>
                        </Table.Td>
                        <Table.Td>
                          <Tooltip label={c.scopes.join(', ')} multiline w={320}>
                            <Badge variant="light" tabIndex={0}>
                              {c.scopes.length} escopo(s)
                            </Badge>
                          </Tooltip>
                        </Table.Td>
                        <Table.Td>
                          <Badge variant="light" color={state.color}>
                            {state.label}
                          </Badge>
                        </Table.Td>
                        <Table.Td>
                          <DateTime value={c.created_at} />
                        </Table.Td>
                        <Table.Td>{c.expires_at ? <DateTime value={c.expires_at} /> : 'Sem expiração'}</Table.Td>
                        <Table.Td>{c.last_used_at ? <DateTime value={c.last_used_at} /> : 'Nunca usada'}</Table.Td>
                        {canManage && (
                          <Table.Td>
                            {!c.revoked_at && (
                              <Group gap={4} wrap="nowrap">
                                <Tooltip label="Rotacionar segredo">
                                  <ActionIcon
                                    variant="subtle"
                                    aria-label={`Rotacionar credencial ${c.name}`}
                                    onClick={async () => {
                                      const ok = await confirmAction({
                                        title: 'Rotacionar credencial',
                                        message: 'Uma nova credencial será criada e exibida uma única vez. A atual continua válida por 24 horas para a troca sem indisponibilidade.',
                                        confirmLabel: 'Rotacionar',
                                      });
                                      if (ok) rotate.mutate(c);
                                    }}
                                  >
                                    <IconRotate size={16} />
                                  </ActionIcon>
                                </Tooltip>
                                <Tooltip label="Revogar credencial">
                                  <ActionIcon
                                    variant="subtle"
                                    color="red"
                                    aria-label={`Revogar credencial ${c.name}`}
                                    onClick={async () => {
                                      const ok = await confirmAction({
                                        title: 'Revogar credencial',
                                        message: `Revogar ${c.name}? Integrações que a usam deixam de funcionar imediatamente.`,
                                        confirmLabel: 'Revogar',
                                        danger: true,
                                      });
                                      if (ok) revoke.mutate(c);
                                    }}
                                  >
                                    <IconTrash size={16} />
                                  </ActionIcon>
                                </Tooltip>
                              </Group>
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
      <CreateCredentialModal opened={creating} onClose={() => setCreating(false)} onCreated={(c) => setSecret({ title: `Segredo de ${c.credential.name}`, value: c.secret })} />
      <SecretRevealModal opened={!!secret} title={secret?.title ?? ''} value={secret?.value ?? null} valueLabel="Segredo da credencial" onClose={() => setSecret(null)} />
    </Stack>
  );
}
