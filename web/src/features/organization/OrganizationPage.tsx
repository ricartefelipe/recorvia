import { zodResolver } from '@hookform/resolvers/zod';
import { Alert, Button, Code, Grid, Group, NumberInput, Paper, Select, SimpleGrid, Stack, Text, TextInput, Title } from '@mantine/core';
import { IconInfoCircle } from '@tabler/icons-react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { z } from 'zod';
import { api } from '../../api/client';
import type { Organization, OrganizationUpdate } from '../../api/types';
import { DateTime, StatusBadge } from '../../components/format';
import { LoadingState, QueryContent } from '../../components/states';
import { KeyValue, PageHeader } from '../../components/ui';
import { supportedTimeZones } from '../../lib/dates';
import { formatDocument, isCnpjFormat, normalizeDocument } from '../../lib/documents';
import { useApiMutation } from '../../lib/hooks';
import { organizationStatusLabels } from '../../lib/labels';
import { optionalInt, optionalText, requiredText } from '../../lib/validation';
import { reloadSession } from '../../session/sessionApi';
import { usePermission, useTenant } from '../../session/SessionContext';
import { addressSchema, UFS } from '../customers/AddressPanel';

const organizationApi = {
  get: (signal?: AbortSignal) => api.get<Organization>('/api/v1/organization', { signal }),
  update: (org: Organization, input: OrganizationUpdate) => api.patch<Organization>('/api/v1/organization', input, { ifMatch: org.version }),
};

const ADDRESS_KEYS = ['street', 'number', 'complement', 'district', 'city', 'state', 'postal_code', 'country'] as const;

/**
 * Endereço da organização, no formato do endereço de cobrança do cliente.
 * Enquanto não há endereço cadastrado, os campos em branco não enviam nada;
 * depois do cadastro, o endereço só é substituído (a API não o remove).
 */
function organizationAddress(registered: boolean) {
  return z
    .object(Object.fromEntries(ADDRESS_KEYS.map((k) => [k, z.string()])) as Record<(typeof ADDRESS_KEYS)[number], z.ZodString>)
    .transform((value, ctx) => {
      const blank = ADDRESS_KEYS.every((k) => k === 'country' || value[k].trim() === '');
      if (!registered && blank) return undefined;
      const parsed = addressSchema.safeParse(value);
      if (!parsed.success) {
        for (const issue of parsed.error.issues) ctx.addIssue({ code: 'custom', message: issue.message, path: issue.path });
        return z.NEVER;
      }
      return parsed.data;
    });
}

const makeSchema = (registeredAddress: boolean) => z.object({
  name: requiredText(2, 200, 'O nome'),
  legal_name: optionalText(200),
  document_number: z
    .string()
    .trim()
    .refine((v) => v === '' || isCnpjFormat(v), 'Informe um CNPJ com 14 caracteres (aceita o formato alfanumérico).')
    .transform((v) => (v === '' ? undefined : normalizeDocument(v))),
  timezone: z.string().min(1, 'Selecione o fuso horário.'),
  invoice_prefix: z.string().trim().regex(/^[A-Z0-9]{1,10}$/, 'Use até 10 letras maiúsculas ou números.'),
  payment_terms_days: optionalInt(0, 120),
  address: organizationAddress(registeredAddress),
});
type FormValues = z.input<ReturnType<typeof makeSchema>>;

function OrganizationForm({ org }: { org: Organization }) {
  const queryClient = useQueryClient();
  const canEdit = usePermission('organization.update');
  const timeZones = useMemo(() => supportedTimeZones().map((tz) => ({ value: tz, label: tz })), []);
  const schema = useMemo(() => makeSchema(!!org.address), [org.address]);
  const a = org.address;
  const form = useForm<FormValues, unknown, OrganizationUpdate>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: org.name,
      legal_name: org.legal_name ?? '',
      document_number: org.document_number ? formatDocument(org.document_number, 'cnpj') : '',
      timezone: org.timezone,
      invoice_prefix: org.invoice_prefix,
      payment_terms_days: org.payment_terms_days,
      address: {
        street: a?.street ?? '',
        number: a?.number ?? '',
        complement: a?.complement ?? '',
        district: a?.district ?? '',
        city: a?.city ?? '',
        state: a?.state?.toUpperCase() ?? '',
        postal_code: a?.postal_code ? a.postal_code.replace(/^(\d{5})(\d{3})$/, '$1-$2') : '',
        country: a?.country ?? 'BR',
      },
    },
  });
  const mutation = useApiMutation<OrganizationUpdate, Organization, FormValues>({
    mutationFn: (input) => organizationApi.update(org, input),
    invalidate: ['organization'],
    successMessage: 'Dados da organização atualizados.',
    form,
    onSuccess: () => void reloadSession(queryClient),
  });
  const { register, control, formState } = form;
  const addressErrors = formState.errors.address;
  return (
    <Paper withBorder p="md" radius="md">
      <form onSubmit={form.handleSubmit((v) => mutation.mutate(v))} noValidate>
        <fieldset disabled={!canEdit} style={{ border: 0, padding: 0, margin: 0 }}>
          <Stack>
            <SimpleGrid cols={{ base: 1, sm: 2 }}>
              <TextInput label="Nome" required {...register('name')} error={formState.errors.name?.message} />
              <TextInput label="Razão social" {...register('legal_name')} error={formState.errors.legal_name?.message} />
              <TextInput label="CNPJ" {...register('document_number')} error={formState.errors.document_number?.message} />
              <Controller
                control={control}
                name="timezone"
                render={({ field, fieldState }) => (
                  <Select
                    label="Fuso horário"
                    description="Afeta vencimentos, lembretes e indicadores."
                    data={timeZones}
                    searchable
                    required
                    value={field.value}
                    onChange={(v) => field.onChange(v ?? '')}
                    error={fieldState.error?.message}
                    disabled={!canEdit}
                  />
                )}
              />
              <TextInput
                label="Prefixo da numeração de faturas"
                required
                {...register('invoice_prefix', { onChange: (e) => form.setValue('invoice_prefix', String(e.target.value).toUpperCase()) })}
                error={formState.errors.invoice_prefix?.message}
              />
              <Controller
                control={control}
                name="payment_terms_days"
                render={({ field, fieldState }) => (
                  <NumberInput
                    label="Prazo padrão de pagamento (dias)"
                    min={0}
                    max={120}
                    allowDecimal={false}
                    value={field.value}
                    onChange={(v) => field.onChange(v === '' ? '' : Number(v))}
                    error={fieldState.error?.message}
                    disabled={!canEdit}
                  />
                )}
              />
            </SimpleGrid>
            <Stack gap={4} mt="sm">
              <Title order={2} size="h5">
                Endereço da organização
              </Title>
              <Text size="sm" c="dimmed">
                Aparece como endereço do emitente no documento das faturas emitidas depois da alteração; as já emitidas mantêm o
                endereço da emissão.
                {!a && ' Opcional: ao preencher, informe logradouro, número, bairro, cidade, UF e CEP.'}
              </Text>
            </Stack>
            <Grid>
              <Grid.Col span={{ base: 12, sm: 8 }}>
                <TextInput label="Logradouro" required={!!a} {...register('address.street')} error={addressErrors?.street?.message} />
              </Grid.Col>
              <Grid.Col span={{ base: 6, sm: 4 }}>
                <TextInput label="Número" required={!!a} {...register('address.number')} error={addressErrors?.number?.message} />
              </Grid.Col>
              <Grid.Col span={{ base: 6, sm: 4 }}>
                <TextInput label="Complemento" {...register('address.complement')} error={addressErrors?.complement?.message} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 8 }}>
                <TextInput label="Bairro" required={!!a} {...register('address.district')} error={addressErrors?.district?.message} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 6 }}>
                <TextInput label="Cidade" required={!!a} {...register('address.city')} error={addressErrors?.city?.message} />
              </Grid.Col>
              <Grid.Col span={{ base: 4, sm: 2 }}>
                <Controller
                  control={control}
                  name="address.state"
                  render={({ field, fieldState }) => (
                    <Select
                      label="UF"
                      required={!!a}
                      data={UFS}
                      searchable
                      value={field.value || null}
                      onChange={(v) => field.onChange(v ?? '')}
                      onBlur={field.onBlur}
                      error={fieldState.error?.message}
                      disabled={!canEdit}
                    />
                  )}
                />
              </Grid.Col>
              <Grid.Col span={{ base: 8, sm: 2 }}>
                <TextInput label="CEP" required={!!a} inputMode="numeric" {...register('address.postal_code')} error={addressErrors?.postal_code?.message} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 2 }}>
                <TextInput label="País" {...register('address.country')} error={addressErrors?.country?.message} />
              </Grid.Col>
            </Grid>
            {canEdit && (
              <Group justify="flex-end">
                <Button type="submit" loading={mutation.isPending}>
                  Salvar
                </Button>
              </Group>
            )}
          </Stack>
        </fieldset>
      </form>
    </Paper>
  );
}

export function OrganizationPage() {
  const { keys } = useTenant();
  const query = useQuery({ queryKey: keys('organization', 'detail'), queryFn: ({ signal }) => organizationApi.get(signal) });
  return (
    <Stack gap={0}>
      <PageHeader title="Organização" description="Dados cadastrais e padrões de faturamento da organização." />
      <QueryContent query={query} loading={<LoadingState variant="detail" />}>
        {(org) => (
          <Stack>
            <Paper withBorder p="md" radius="md">
              <SimpleGrid cols={{ base: 2, md: 4 }}>
                <KeyValue label="Identificador">
                  <Code>{org.slug}</Code>
                </KeyValue>
                <KeyValue label="Situação">
                  <StatusBadge status={org.status} labels={organizationStatusLabels} />
                </KeyValue>
                <KeyValue label="Moeda">{org.currency}</KeyValue>
                <KeyValue label="Criada em">
                  <DateTime value={org.created_at} />
                </KeyValue>
              </SimpleGrid>
            </Paper>
            <Alert variant="light" icon={<IconInfoCircle />}>
              Alterar o fuso ou o prazo vale para novas faturas; faturas emitidas mantêm as datas registradas.
            </Alert>
            <OrganizationForm key={org.version} org={org} />
          </Stack>
        )}
      </QueryContent>
    </Stack>
  );
}
