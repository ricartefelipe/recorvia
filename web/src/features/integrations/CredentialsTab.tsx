import { zodResolver } from '@hookform/resolvers/zod';
import { ActionIcon, Badge, Button, Code, Group, Modal, MultiSelect, NumberInput, Stack, Table, Text, TextInput, Tooltip } from '@mantine/core';
import { IconKey, IconPlus, IconRotate, IconTrash } from '@tabler/icons-react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { z } from 'zod';
import type { ApiCredential, ApiCredentialWithSecret } from '../../api/types';
import { DateTime } from '../../components/format';
import { EmptyState, QueryContent } from '../../components/states';
import { confirmAction, CursorPager, SecretRevealModal } from '../../components/ui';
import { useApiMutation, useCursorPagination, useReturnFromEmptyPage } from '../../lib/hooks';
import { optionalInt, requiredText } from '../../lib/validation';
import { PERMISSIONS, permissionLabel, type Permission } from '../../session/permissions';
import { usePermission, useTenant } from '../../session/SessionContext';
import { integrationsApi } from './api';

/**
 * Permissões que nunca viram escopo de credencial: exigem uma pessoa
 * identificada (a API recusa esses escopos com 422).
 */
export const NON_DELEGABLE_SCOPES: readonly string[] = [
  'members.manage',
  'platform_billing.manage',
  'api_credentials.manage',
  'customers.anonymize',
] satisfies Permission[];

/**
 * Escopos que o membro atual pode conceder: as permissões do próprio papel,
 * exceto as não delegáveis, na ordem do catálogo. A API recusa escopos além
 * das permissões de quem emite (403 scope_exceeds_permissions).
 */
export function grantableScopes(granted: readonly string[]): string[] {
  const own = new Set(granted.filter((p) => !NON_DELEGABLE_SCOPES.includes(p)));
  const known = PERMISSIONS.filter((p) => own.has(p));
  const extra = [...own].filter((p) => !(PERMISSIONS as readonly string[]).includes(p));
  return [...known, ...extra];
}

const credentialSchema = z.object({
  name: requiredText(3, 100, 'O nome'),
  scopes: z.array(z.string()).min(1, 'Escolha pelo menos um escopo.'),
  expires_in_days: optionalInt(1, 730),
});
type CredentialForm = z.input<typeof credentialSchema>;
type CredentialInput = z.output<typeof credentialSchema>;

function credentialState(c: ApiCredential): { label: string; color: string; active: boolean } {
  if (c.revoked_at) return { label: 'Revogada', color: 'gray', active: false };
  if (c.expires_at && new Date(c.expires_at).getTime() <= Date.now()) return { label: 'Expirada', color: 'orange', active: false };
  return { label: 'Ativa', color: 'green', active: true };
}

function CreateCredentialModal({ opened, onClose, onCreated }: { opened: boolean; onClose: () => void; onCreated: (c: ApiCredentialWithSecret) => void }) {
  const { tenant } = useTenant();
  const scopes = grantableScopes(tenant.permissions);
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
                description="Conceda apenas o necessário para a integração. Somente permissões do seu papel podem ser delegadas; a gestão de membros, do plano e de credenciais exige uma pessoa."
                required
                searchable
                data={scopes.map((p) => ({ value: p, label: `${permissionLabel(p)} (${p})` }))}
                value={field.value}
                onChange={field.onChange}
                error={fieldState.error?.message}
                placeholder="Selecione"
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

function CredentialActions({ credential, onRotate, onRevoke }: { credential: ApiCredential; onRotate: () => void; onRevoke: () => void }) {
  if (credential.revoked_at) return null;
  const { active } = credentialState(credential);
  return (
    <Group gap={4} wrap="nowrap">
      {active && (
        <Tooltip label="Rotacionar segredo">
          <ActionIcon
            variant="subtle"
            aria-label={`Rotacionar credencial ${credential.name}`}
            onClick={async () => {
              const ok = await confirmAction({
                title: 'Rotacionar segredo',
                message: `Um novo segredo de ${credential.name} será gerado e exibido uma única vez. O prefixo e os escopos são mantidos, e o segredo atual continua válido por 24 horas para a troca sem indisponibilidade.`,
                confirmLabel: 'Rotacionar',
              });
              if (ok) onRotate();
            }}
          >
            <IconRotate size={16} />
          </ActionIcon>
        </Tooltip>
      )}
      <Tooltip label="Revogar credencial">
        <ActionIcon
          variant="subtle"
          color="red"
          aria-label={`Revogar credencial ${credential.name}`}
          onClick={async () => {
            const ok = await confirmAction({
              title: 'Revogar credencial',
              message: `Revogar ${credential.name}? Integrações que a usam deixam de funcionar imediatamente, inclusive com o segredo anterior a uma rotação.`,
              confirmLabel: 'Revogar',
              danger: true,
            });
            if (ok) onRevoke();
          }}
        >
          <IconTrash size={16} />
        </ActionIcon>
      </Tooltip>
    </Group>
  );
}

export function CredentialsTab() {
  const { keys } = useTenant();
  const canManage = usePermission('api_credentials.manage');
  const [creating, setCreating] = useState(false);
  const [secret, setSecret] = useState<{ title: string; value: string } | null>(null);
  // A listagem exige api_credentials.manage: sem ela, a aba nem consulta a API.
  // Páginas por cursor: as ativas primeiro, depois revogadas e expiradas.
  const pagination = useCursorPagination('api-credentials');
  const query = useQuery({
    queryKey: keys('api-credentials', 'list', pagination.cursor),
    queryFn: ({ signal }) => integrationsApi.credentials({ cursor: pagination.cursor, limit: 25 }, signal),
    enabled: canManage,
    placeholderData: keepPreviousData,
  });
  useReturnFromEmptyPage(pagination, query.data?.data, query.isPlaceholderData);
  const rotate = useApiMutation<ApiCredential, ApiCredentialWithSecret>({
    mutationFn: (c) => integrationsApi.rotateCredential(c),
    invalidate: ['api-credentials'],
    successMessage: 'Segredo rotacionado. O segredo anterior continua válido por 24 horas.',
    onSuccess: (r) => setSecret({ title: `Novo segredo de ${r.credential.name}`, value: r.secret }),
  });
  const revoke = useApiMutation<ApiCredential, unknown>({
    mutationFn: (c) => integrationsApi.revokeCredential(c),
    invalidate: ['api-credentials'],
    successMessage: 'Credencial revogada.',
  });
  if (!canManage) {
    return (
      <EmptyState
        title="Acesso restrito"
        description="A gestão de credenciais de API exige a permissão Gerenciar credenciais de API."
        icon={<IconKey size={26} />}
      />
    );
  }
  return (
    <Stack>
      <Group justify="space-between">
        <Text size="sm" c="dimmed" maw={640}>
          Integrações autenticam com <Code>Authorization: Bearer rcv_…</Code>, sem cookie nem token CSRF. Cada credencial
          pertence a esta organização e tem escopos próprios.
        </Text>
        <Button leftSection={<IconPlus size={16} />} onClick={() => setCreating(true)}>
          Nova credencial
        </Button>
      </Group>
      <QueryContent query={query}>
        {(page) =>
          page.data.length === 0 ? (
            <EmptyState title="Nenhuma credencial" description="Crie uma credencial para integrar seu ERP, CRM ou o envio de eventos de consumo." icon={<IconKey size={26} />} />
          ) : (
            <>
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
                      <Table.Th aria-label="Ações" />
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {page.data.map((c) => {
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
                          <Table.Td>
                            <CredentialActions credential={c} onRotate={() => rotate.mutate(c)} onRevoke={() => revoke.mutate(c)} />
                          </Table.Td>
                        </Table.Tr>
                      );
                    })}
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
      <CreateCredentialModal opened={creating} onClose={() => setCreating(false)} onCreated={(c) => setSecret({ title: `Segredo de ${c.credential.name}`, value: c.secret })} />
      <SecretRevealModal opened={!!secret} title={secret?.title ?? ''} value={secret?.value ?? null} valueLabel="Segredo da credencial" onClose={() => setSecret(null)} />
    </Stack>
  );
}
