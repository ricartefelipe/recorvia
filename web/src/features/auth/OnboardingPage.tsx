import { zodResolver } from '@hookform/resolvers/zod';
import { Alert, Button, Group, NumberInput, Select, SimpleGrid, Stack, TextInput } from '@mantine/core';
import { IconInfoCircle } from '@tabler/icons-react';
import { useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { Link, useNavigate } from 'react-router';
import { z } from 'zod';
import type { OrganizationInput } from '../../api/types';
import { accentInsensitiveFilter } from '../../components/inputs';
import { timeZoneOptions } from '../../lib/dates';
import { isCnpjFormat, normalizeDocument } from '../../lib/documents';
import { handleMutationError } from '../../lib/errors';
import { notifySuccess } from '../../lib/notify';
import { optionalInt, optionalText, requiredText } from '../../lib/validation';
import { useSession } from '../../session/SessionContext';
import { createOrganization } from '../../session/sessionApi';
import { AuthLayout } from './AuthLayout';

export function slugify(value: string): string {
  return value
    .normalize('NFD')
    .replace(/[̀-ͯ]/g, '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 63);
}

export const organizationSchema = z.object({
  name: requiredText(2, 200, 'O nome'),
  slug: z
    .string()
    .trim()
    .regex(/^[a-z0-9][a-z0-9-]{1,62}$/, 'Use de 2 a 63 letras minúsculas, números ou hífens, começando por letra ou número.'),
  legal_name: optionalText(200),
  document_number: z
    .string()
    .trim()
    .refine((v) => v === '' || isCnpjFormat(v), 'Informe um CNPJ com 14 caracteres (aceita o formato alfanumérico).')
    .transform((v) => (v === '' ? undefined : normalizeDocument(v))),
  timezone: z.string().min(1, 'Selecione o fuso horário.'),
  invoice_prefix: z
    .string()
    .trim()
    .refine((v) => v === '' || /^[A-Z0-9]{1,10}$/.test(v), 'Use até 10 letras maiúsculas ou números.')
    .transform((v) => (v === '' ? undefined : v)),
  payment_terms_days: optionalInt(0, 120),
});

type FormInput = z.input<typeof organizationSchema>;

export function OnboardingPage() {
  const { session } = useSession();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [slugEdited, setSlugEdited] = useState(false);
  const timeZones = useMemo(() => timeZoneOptions(), []);

  const form = useForm<FormInput, unknown, OrganizationInput>({
    resolver: zodResolver(organizationSchema),
    defaultValues: {
      name: '',
      slug: '',
      legal_name: '',
      document_number: '',
      timezone: 'America/Sao_Paulo',
      invoice_prefix: '',
      payment_terms_days: '',
    },
  });
  const { register, control, handleSubmit, setValue, formState } = form;

  const onSubmit = handleSubmit(async (input) => {
    try {
      const created = await createOrganization(queryClient, input);
      notifySuccess(`${created.active_tenant?.name ?? input.name} criada. O plano inicial está em período de avaliação.`, 'Organização criada');
      navigate('/', { replace: true });
    } catch (error) {
      handleMutationError(error, { form });
    }
  });

  const hasOrganizations = session.organizations.length > 0;

  return (
    <AuthLayout
      title="Crie sua organização"
      subtitle="Você será o proprietário e poderá convidar sua equipe depois."
      width={640}
    >
      {!hasOrganizations && (
        <Alert variant="light" icon={<IconInfoCircle />}>
          Você ainda não participa de nenhuma organização. Se recebeu um convite, abra o link do convite; caso contrário,
          crie sua organização abaixo.
        </Alert>
      )}
      <form onSubmit={onSubmit} noValidate>
        <Stack>
          <TextInput
            label="Nome da organização"
            required
            {...register('name', {
              onChange: (e) => {
                if (!slugEdited) setValue('slug', slugify(e.target.value));
              },
            })}
            error={formState.errors.name?.message}
          />
          <TextInput
            label="Identificador (slug)"
            description="Usado em endereços. Letras minúsculas, números e hífens."
            required
            {...register('slug', { onChange: () => setSlugEdited(true) })}
            error={formState.errors.slug?.message}
          />
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <TextInput label="Razão social" {...register('legal_name')} error={formState.errors.legal_name?.message} />
            <TextInput label="CNPJ" {...register('document_number')} error={formState.errors.document_number?.message} />
          </SimpleGrid>
          <Controller
            control={control}
            name="timezone"
            render={({ field, fieldState }) => (
              <Select
                label="Fuso horário"
                description="Datas de vencimento, lembretes e indicadores usam este fuso."
                data={timeZones}
                filter={accentInsensitiveFilter}
                searchable
                required
                value={field.value}
                onChange={(v) => field.onChange(v ?? '')}
                onBlur={field.onBlur}
                error={fieldState.error?.message}
              />
            )}
          />
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <TextInput
              label="Prefixo da numeração de faturas"
              placeholder="ACME"
              {...register('invoice_prefix', {
                onChange: (e) => setValue('invoice_prefix', String(e.target.value).toUpperCase()),
              })}
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
                  onBlur={field.onBlur}
                  error={fieldState.error?.message}
                />
              )}
            />
          </SimpleGrid>
          <Group justify="space-between">
            {hasOrganizations ? (
              <Button component={Link} to="/organizacoes" variant="subtle">
                Voltar
              </Button>
            ) : (
              <span />
            )}
            <Button type="submit" loading={formState.isSubmitting}>
              Criar organização
            </Button>
          </Group>
        </Stack>
      </form>
    </AuthLayout>
  );
}
