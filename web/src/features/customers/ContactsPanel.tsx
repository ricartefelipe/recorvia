import { zodResolver } from '@hookform/resolvers/zod';
import { ActionIcon, Alert, Badge, Button, Checkbox, Group, Modal, SimpleGrid, Stack, Table, TextInput, Tooltip } from '@mantine/core';
import { IconInfoCircle, IconPlus, IconTrash } from '@tabler/icons-react';
import { useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { z } from 'zod';
import type { Contact, ContactInput, CustomerDetails } from '../../api/types';
import { EmptyState } from '../../components/states';
import { confirmAction } from '../../components/ui';
import { useApiMutation } from '../../lib/hooks';
import { optionalPhone, optionalText, requiredEmail, requiredText } from '../../lib/validation';
import { Can, usePermission } from '../../session/SessionContext';
import { customersApi } from './api';

const contactSchema = z.object({
  name: requiredText(1, 200, 'O nome'),
  email: requiredEmail,
  phone: optionalPhone,
  role: optionalText(100),
  receives_billing_email: z.boolean(),
});

type ContactForm = z.input<typeof contactSchema>;

function AddContactModal({ customerId, opened, onClose }: { customerId: string; opened: boolean; onClose: () => void }) {
  const form = useForm<ContactForm, unknown, ContactInput>({
    resolver: zodResolver(contactSchema),
    defaultValues: { name: '', email: '', phone: '', role: '', receives_billing_email: true },
  });
  const mutation = useApiMutation<ContactInput, Contact, ContactForm>({
    mutationFn: (input) => customersApi.addContact(customerId, input),
    invalidate: ['customers'],
    successMessage: 'Contato adicionado.',
    form,
    onSuccess: () => {
      form.reset();
      onClose();
    },
  });
  const { register, control, formState } = form;
  return (
    <Modal opened={opened} onClose={onClose} title="Adicionar contato" size="lg">
      <form onSubmit={form.handleSubmit((input) => mutation.mutate(input))} noValidate>
        <Stack>
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <TextInput label="Nome" required {...register('name')} error={formState.errors.name?.message} />
            <TextInput label="E-mail" type="email" required {...register('email')} error={formState.errors.email?.message} />
            <TextInput label="Telefone" type="tel" {...register('phone')} error={formState.errors.phone?.message} />
            <TextInput label="Função" placeholder="Financeiro, compras…" {...register('role')} error={formState.errors.role?.message} />
          </SimpleGrid>
          <Controller
            control={control}
            name="receives_billing_email"
            render={({ field }) => (
              <Checkbox
                label="Recebe e-mails de cobrança (faturas e lembretes)"
                checked={field.value}
                onChange={(e) => field.onChange(e.currentTarget.checked)}
              />
            )}
          />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancelar
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              Adicionar
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

export function ContactsPanel({ customer }: { customer: CustomerDetails }) {
  const [adding, setAdding] = useState(false);
  const canWrite = usePermission('customers.write');
  const remove = useApiMutation<Contact, unknown>({
    mutationFn: (contact) => customersApi.removeContact(customer.id, contact.id),
    invalidate: ['customers'],
    successMessage: 'Contato removido.',
  });

  const onRemove = async (contact: Contact) => {
    const ok = await confirmAction({
      title: 'Remover contato',
      message: `Remover ${contact.name} (${contact.email})? Ele deixará de receber e-mails de cobrança.`,
      confirmLabel: 'Remover',
      danger: true,
    });
    if (ok) remove.mutate(contact);
  };

  return (
    <Stack>
      <Group justify="space-between">
        <Alert variant="light" icon={<IconInfoCircle />} p="xs" style={{ flex: 1 }}>
          A preferência de e-mail de cobrança é definida ao adicionar o contato. O próprio cliente pode alterá-la pelo
          portal; aqui, remova e adicione o contato novamente.
        </Alert>
        <Can permission="customers.write">
          <Button leftSection={<IconPlus size={16} />} onClick={() => setAdding(true)} disabled={customer.status !== 'active'}>
            Adicionar contato
          </Button>
        </Can>
      </Group>
      {customer.contacts.length === 0 ? (
        <EmptyState
          title="Nenhum contato"
          description="Sem contatos, os e-mails de cobrança vão para o e-mail principal do cliente, se houver."
        />
      ) : (
        <Table.ScrollContainer minWidth={640}>
          <Table aria-label="Contatos">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Nome</Table.Th>
                <Table.Th>E-mail</Table.Th>
                <Table.Th>Telefone</Table.Th>
                <Table.Th>Função</Table.Th>
                <Table.Th>E-mail de cobrança</Table.Th>
                {canWrite && <Table.Th aria-label="Ações" />}
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {customer.contacts.map((c) => (
                <Table.Tr key={c.id}>
                  <Table.Td>{c.name}</Table.Td>
                  <Table.Td>{c.email}</Table.Td>
                  <Table.Td>{c.phone || '—'}</Table.Td>
                  <Table.Td>{c.role || '—'}</Table.Td>
                  <Table.Td>
                    <Badge variant="light" color={c.receives_billing_email ? 'green' : 'gray'}>
                      {c.receives_billing_email ? 'Recebe' : 'Não recebe'}
                    </Badge>
                  </Table.Td>
                  {canWrite && (
                    <Table.Td>
                      <Tooltip label="Remover contato">
                        <ActionIcon
                          variant="subtle"
                          color="red"
                          aria-label={`Remover contato ${c.name}`}
                          onClick={() => void onRemove(c)}
                          loading={remove.isPending && remove.variables?.id === c.id}
                        >
                          <IconTrash size={16} />
                        </ActionIcon>
                      </Tooltip>
                    </Table.Td>
                  )}
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
      <AddContactModal customerId={customer.id} opened={adding} onClose={() => setAdding(false)} />
    </Stack>
  );
}
