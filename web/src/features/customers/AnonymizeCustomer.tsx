import { zodResolver } from '@hookform/resolvers/zod';
import { Alert, Button, Group, List, Modal, Stack, Text, Textarea } from '@mantine/core';
import { IconAlertTriangle, IconInfoCircle } from '@tabler/icons-react';
import { useState } from 'react';
import { useForm } from 'react-hook-form';
import { z } from 'zod';
import type { CustomerAnonymization, CustomerDetails } from '../../api/types';
import { confirmAction, CopyField } from '../../components/ui';
import { useApiMutation } from '../../lib/hooks';
import { useIdempotencyKey } from '../../lib/idempotency';
import { notifySuccess } from '../../lib/notify';
import { requiredText } from '../../lib/validation';
import { customersApi } from './api';

const anonymizeSchema = z.object({ reason: requiredText(10, 500, 'A justificativa') });
type AnonymizeForm = z.input<typeof anonymizeSchema>;
type AnonymizeInput = z.output<typeof anonymizeSchema>;

/**
 * Diálogos da anonimização do cliente: irreversível, com justificativa
 * obrigatória e confirmação. A permissão (customers.anonymize) é conferida por
 * quem renderiza; o backend continua sendo a autoridade.
 *
 * O componente é montado pela página, fora do conteúdo que depende da consulta
 * da ficha: o formulário, a chave de idempotência e o diálogo com os
 * identificadores do cliente no provedor de pagamento não podem sumir quando a
 * ficha é recarregada, nem quando a recarga falha e a ficha dá lugar à tela de
 * erro. Os identificadores já não estão no cadastro; o diálogo só fecha quando
 * a pessoa o fecha.
 *
 * Se o cliente passa a constar como anonimizado com o formulário aberto (a
 * resposta do envio se perdeu e a ficha recarregou, ou outra pessoa concluiu o
 * pedido), o formulário continua aberto e explica que repetir o envio devolve
 * os identificadores, sem alterar mais nada.
 */
export function AnonymizeCustomerDialogs({
  customerId,
  customer,
  opened,
  onClose,
}: {
  customerId: string;
  /** Ficha em cache; indefinida enquanto carrega ou quando nunca carregou. */
  customer: CustomerDetails | undefined;
  opened: boolean;
  onClose: () => void;
}) {
  const anonymized = !!customer?.anonymized_at;
  const [result, setResult] = useState<CustomerAnonymization | null>(null);
  const idempotency = useIdempotencyKey();
  const form = useForm<AnonymizeForm, unknown, AnonymizeInput>({
    resolver: zodResolver(anonymizeSchema),
    defaultValues: { reason: '' },
  });
  const close = () => {
    onClose();
    form.reset();
  };
  const mutation = useApiMutation<AnonymizeInput, CustomerAnonymization, AnonymizeForm>({
    mutationFn: (input) => customersApi.anonymize(customerId, input, idempotency.keyFor(input)),
    invalidate: ['customers'],
    form,
    idempotency,
    onSuccess: (data) => {
      close();
      if (data.provider_customers.length > 0) setResult(data);
      else notifySuccess('Cliente anonimizado.');
    },
  });
  const onSubmit = form.handleSubmit(async (input) => {
    // Cliente já anonimizado: repetir o envio só devolve os identificadores.
    const ok =
      anonymized ||
      (await confirmAction({
        title: 'Confirmar anonimização',
        message: `Anonimizar ${customer?.name ?? 'este cliente'}? Os dados pessoais serão substituídos e não poderão ser recuperados.`,
        confirmLabel: 'Anonimizar definitivamente',
        danger: true,
      }));
    if (ok) mutation.mutate(input);
  });

  return (
    <>
      <Modal opened={opened} onClose={close} title="Anonimizar cliente" size="lg">
        <form onSubmit={onSubmit} noValidate>
          <Stack>
            {anonymized && (
              <Alert color="yellow" variant="light" icon={<IconInfoCircle />} title="Este cliente já consta como anonimizado">
                <Text size="sm">
                  Se o envio anterior falhou sem resposta, ele foi concluído mesmo assim. Envie novamente para receber os
                  identificadores do cliente no provedor de pagamento: nada mais é alterado.
                </Text>
                <Text size="sm" mt={4}>
                  Os identificadores também ficam registrados no evento de anonimização, na aba Histórico do cliente.
                </Text>
              </Alert>
            )}
            <Alert color="red" variant="light" icon={<IconAlertTriangle />} title="Esta ação é irreversível">
              <List size="sm" spacing={4}>
                <List.Item>
                  Nome, documento, e-mail, telefone e identificador externo são substituídos por valores neutros, e o
                  cliente fica arquivado.
                </List.Item>
                <List.Item>
                  Contatos, endereço de cobrança e notificações enviadas são excluídos; os links do portal são revogados.
                </List.Item>
                <List.Item>
                  Faturas emitidas, pagamentos e a auditoria são mantidos. As faturas já emitidas continuam exibindo os
                  dados do cliente no momento da emissão.
                </List.Item>
                <List.Item>
                  Só é possível anonimizar clientes sem assinatura ativa, fatura em aberto nem faturamento pendente
                  (consumo ou itens ainda a faturar).
                </List.Item>
              </List>
            </Alert>
            <Textarea
              label="Justificativa"
              description="Informe o protocolo do pedido e o motivo. Não inclua dados pessoais do cliente: o texto fica na auditoria."
              required
              autosize
              minRows={2}
              maxLength={500}
              {...form.register('reason')}
              error={form.formState.errors.reason?.message}
            />
            <Group justify="flex-end">
              <Button variant="default" onClick={close}>
                Cancelar
              </Button>
              <Button type="submit" color="red" loading={mutation.isPending}>
                {anonymized ? 'Enviar novamente' : 'Continuar'}
              </Button>
            </Group>
          </Stack>
        </form>
      </Modal>
      <Modal opened={!!result} onClose={() => setResult(null)} title="Cliente anonimizado" size="lg">
        {result && (
          <Stack>
            <Text size="sm">
              Os dados pessoais foram removidos do Recorvia. O cadastro do cliente no provedor de pagamento não é alterado
              por aqui: trate-o no próprio provedor, usando os identificadores abaixo.
            </Text>
            <Text size="sm" c="dimmed">
              Os identificadores foram removidos do cadastro do cliente. Eles continuam registrados na auditoria da
              anonimização, no histórico do cliente.
            </Text>
            {result.provider_customers.map((ref) => (
              <CopyField
                key={ref.provider}
                label={`Identificador no provedor ${ref.provider}`}
                value={ref.provider_customer_id}
                styles={{ input: { fontFamily: 'monospace' } }}
              />
            ))}
            <Group justify="flex-end">
              <Button onClick={() => setResult(null)}>Fechar</Button>
            </Group>
          </Stack>
        )}
      </Modal>
    </>
  );
}
