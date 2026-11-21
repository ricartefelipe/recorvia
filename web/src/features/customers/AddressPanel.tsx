import { zodResolver } from '@hookform/resolvers/zod';
import { Button, Grid, Group, Paper, Select, Stack, Text, TextInput, Title } from '@mantine/core';
import { Controller, useForm } from 'react-hook-form';
import { z } from 'zod';
import type { Address, CustomerDetails } from '../../api/types';
import { isPostalCode } from '../../lib/documents';
import { useApiMutation } from '../../lib/hooks';
import { optionalText, requiredText } from '../../lib/validation';
import { usePermission } from '../../session/SessionContext';
import { customersApi } from './api';

const UFS = ['AC', 'AL', 'AP', 'AM', 'BA', 'CE', 'DF', 'ES', 'GO', 'MA', 'MT', 'MS', 'MG', 'PA', 'PB', 'PR', 'PE', 'PI', 'RJ', 'RN', 'RS', 'RO', 'RR', 'SC', 'SP', 'SE', 'TO'];

const addressSchema = z.object({
  street: requiredText(1, 200, 'O logradouro'),
  number: requiredText(1, 20, 'O número'),
  complement: optionalText(100),
  district: requiredText(1, 100, 'O bairro'),
  city: requiredText(1, 100, 'A cidade'),
  state: z.string().regex(/^[A-Za-z]{2}$/, 'Selecione a UF.'),
  postal_code: z
    .string()
    .trim()
    .refine(isPostalCode, 'Informe o CEP no formato 00000-000.')
    .transform((v) => v.replace(/\D/g, '').replace(/^(\d{5})(\d{3})$/, '$1-$2')),
  country: z.string().trim().regex(/^[A-Z]{2}$/, 'Use o código do país com 2 letras (BR).'),
});

type AddressForm = z.input<typeof addressSchema>;

export function AddressPanel({ customer }: { customer: CustomerDetails }) {
  const canWrite = usePermission('customers.write') && customer.status === 'active';
  const a = customer.billing_address;
  const form = useForm<AddressForm, unknown, Address>({
    resolver: zodResolver(addressSchema),
    defaultValues: {
      street: a?.street ?? '',
      number: a?.number ?? '',
      complement: a?.complement ?? '',
      district: a?.district ?? '',
      city: a?.city ?? '',
      state: a?.state?.toUpperCase() ?? '',
      postal_code: a?.postal_code ?? '',
      country: a?.country ?? 'BR',
    },
  });
  const mutation = useApiMutation<Address, Address, AddressForm>({
    mutationFn: (address) => customersApi.setAddress(customer.id, customer.version, address),
    invalidate: ['customers'],
    successMessage: 'Endereço de cobrança gravado.',
    form,
  });
  const { register, control, formState } = form;

  return (
    <Paper withBorder p="md" radius="md">
      <form onSubmit={form.handleSubmit((address) => mutation.mutate(address))} noValidate>
        <Stack>
          <div>
            <Title order={3} size="h5">
              Endereço de cobrança
            </Title>
            <Text size="sm" c="dimmed">
              Aparece no documento da fatura.
            </Text>
          </div>
          <fieldset disabled={!canWrite} style={{ border: 0, padding: 0, margin: 0 }}>
            <Grid>
              <Grid.Col span={{ base: 12, sm: 8 }}>
                <TextInput label="Logradouro" required {...register('street')} error={formState.errors.street?.message} />
              </Grid.Col>
              <Grid.Col span={{ base: 6, sm: 4 }}>
                <TextInput label="Número" required {...register('number')} error={formState.errors.number?.message} />
              </Grid.Col>
              <Grid.Col span={{ base: 6, sm: 4 }}>
                <TextInput label="Complemento" {...register('complement')} error={formState.errors.complement?.message} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 8 }}>
                <TextInput label="Bairro" required {...register('district')} error={formState.errors.district?.message} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 6 }}>
                <TextInput label="Cidade" required {...register('city')} error={formState.errors.city?.message} />
              </Grid.Col>
              <Grid.Col span={{ base: 4, sm: 2 }}>
                <Controller
                  control={control}
                  name="state"
                  render={({ field, fieldState }) => (
                    <Select
                      label="UF"
                      required
                      data={UFS}
                      searchable
                      value={field.value || null}
                      onChange={(v) => field.onChange(v ?? '')}
                      onBlur={field.onBlur}
                      error={fieldState.error?.message}
                      disabled={!canWrite}
                    />
                  )}
                />
              </Grid.Col>
              <Grid.Col span={{ base: 8, sm: 2 }}>
                <TextInput label="CEP" required inputMode="numeric" {...register('postal_code')} error={formState.errors.postal_code?.message} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 2 }}>
                <TextInput label="País" {...register('country')} error={formState.errors.country?.message} />
              </Grid.Col>
            </Grid>
          </fieldset>
          {canWrite && (
            <Group justify="flex-end">
              <Button type="submit" loading={mutation.isPending}>
                Salvar endereço
              </Button>
            </Group>
          )}
        </Stack>
      </form>
    </Paper>
  );
}
