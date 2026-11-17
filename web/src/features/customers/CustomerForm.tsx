import { zodResolver } from '@hookform/resolvers/zod';
import { Button, Group, NumberInput, SegmentedControl, SimpleGrid, Stack, Text, TextInput } from '@mantine/core';
import { Controller, useForm, useWatch } from 'react-hook-form';
import { z } from 'zod';
import type { CustomerDetails, CustomerInput } from '../../api/types';
import { formatDocument, isCnpjFormat, isCpfFormat, normalizeDocument } from '../../lib/documents';
import { useApiMutation } from '../../lib/hooks';
import { optionalEmail, optionalPhone, optionalText, requiredText } from '../../lib/validation';
import { customersApi } from './api';

const always = { when: () => true };

export const customerSchema = z
  .object({
    kind: z.enum(['individual', 'company']),
    name: requiredText(2, 200, 'O nome'),
    legal_name: optionalText(200),
    document_number: z.string().trim().min(1, 'Informe o documento.'),
    email: optionalEmail,
    phone: optionalPhone,
    external_id: optionalText(128),
    payment_terms_days: z
      .union([z.number(), z.literal('')])
      .refine((v) => v === '' || (Number.isInteger(v) && v >= 0 && v <= 120), 'Informe um prazo entre 0 e 120 dias.'),
  })
  .refine((v) => v.kind !== 'individual' || !v.document_number || isCpfFormat(v.document_number), {
    path: ['document_number'],
    message: 'CPF deve ter 11 dígitos, por exemplo 123.456.789-09.',
    ...always,
  })
  .refine((v) => v.kind !== 'company' || !v.document_number || isCnpjFormat(v.document_number), {
    path: ['document_number'],
    message: 'CNPJ deve ter 14 caracteres, por exemplo 12.345.678/0001-95 (os 12 primeiros podem ser letras).',
    ...always,
  })
  .transform(
    (v): CustomerInput => ({
      kind: v.kind,
      name: v.name,
      legal_name: v.kind === 'company' ? v.legal_name : undefined,
      document_number: normalizeDocument(v.document_number),
      email: v.email,
      phone: v.phone,
      external_id: v.external_id,
      payment_terms_days: v.payment_terms_days === '' ? null : v.payment_terms_days,
    }),
  );

type FormInput = z.input<typeof customerSchema>;

function defaults(customer?: CustomerDetails): FormInput {
  return {
    kind: customer?.kind ?? 'individual',
    name: customer?.name ?? '',
    legal_name: customer?.legal_name ?? '',
    document_number: customer ? formatDocument(customer.document_number, customer.document_type) : '',
    email: customer?.email ?? '',
    phone: customer?.phone ?? '',
    external_id: customer?.external_id ?? '',
    payment_terms_days: customer?.payment_terms_days ?? '',
  };
}

export function CustomerForm({
  customer,
  onSaved,
  onCancel,
}: {
  customer?: CustomerDetails;
  onSaved: (customer: CustomerDetails) => void;
  onCancel?: () => void;
}) {
  const form = useForm<FormInput, unknown, CustomerInput>({
    resolver: zodResolver(customerSchema),
    defaultValues: defaults(customer),
    mode: 'onTouched',
  });
  const { register, control, handleSubmit, formState } = form;
  const kind = useWatch({ control, name: 'kind' });

  const mutation = useApiMutation<CustomerInput, CustomerDetails, FormInput>({
    mutationFn: (input) => (customer ? customersApi.update(customer.id, customer.version, input) : customersApi.create(input)),
    invalidate: ['customers'],
    successMessage: customer ? 'Cliente atualizado.' : 'Cliente cadastrado.',
    form,
    onSuccess: onSaved,
  });

  return (
    <form onSubmit={handleSubmit((input) => mutation.mutate(input))} noValidate aria-label={customer ? 'Editar cliente' : 'Novo cliente'}>
      <Stack>
        <Controller
          control={control}
          name="kind"
          render={({ field }) => (
            <Stack gap={4}>
              <Text size="sm" fw={500} id="customer-kind-label">
                Tipo de cliente
              </Text>
              <SegmentedControl
                aria-labelledby="customer-kind-label"
                value={field.value}
                onChange={(v) => field.onChange(v)}
                data={[
                  { value: 'individual', label: 'Pessoa física' },
                  { value: 'company', label: 'Pessoa jurídica' },
                ]}
                disabled={!!customer}
              />
            </Stack>
          )}
        />
        <TextInput label={kind === 'company' ? 'Nome fantasia' : 'Nome completo'} required {...register('name')} error={formState.errors.name?.message} />
        {kind === 'company' && (
          <TextInput label="Razão social" {...register('legal_name')} error={formState.errors.legal_name?.message} />
        )}
        <TextInput
          label={kind === 'company' ? 'CNPJ' : 'CPF'}
          required
          description="Validamos o formato aqui; os dígitos verificadores são conferidos ao salvar."
          inputMode={kind === 'company' ? 'text' : 'numeric'}
          {...register('document_number')}
          error={formState.errors.document_number?.message}
        />
        <SimpleGrid cols={{ base: 1, sm: 2 }}>
          <TextInput label="E-mail" type="email" {...register('email')} error={formState.errors.email?.message} />
          <TextInput label="Telefone" type="tel" {...register('phone')} error={formState.errors.phone?.message} />
        </SimpleGrid>
        <SimpleGrid cols={{ base: 1, sm: 2 }}>
          <TextInput
            label="Identificador externo"
            description="Código do cliente no seu sistema (único na organização)."
            {...register('external_id')}
            error={formState.errors.external_id?.message}
          />
          <Controller
            control={control}
            name="payment_terms_days"
            render={({ field, fieldState }) => (
              <NumberInput
                label="Prazo de pagamento (dias)"
                description="Vazio usa o prazo padrão da organização."
                min={0}
                max={120}
                allowDecimal={false}
                allowNegative={false}
                value={field.value}
                onChange={(v) => field.onChange(v === '' ? '' : Number(v))}
                onBlur={field.onBlur}
                error={fieldState.error?.message}
              />
            )}
          />
        </SimpleGrid>
        <Group justify="flex-end">
          {onCancel && (
            <Button variant="default" onClick={onCancel}>
              Cancelar
            </Button>
          )}
          <Button type="submit" loading={mutation.isPending}>
            {customer ? 'Salvar alterações' : 'Cadastrar cliente'}
          </Button>
        </Group>
      </Stack>
    </form>
  );
}
