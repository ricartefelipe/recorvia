import { ActionIcon, Anchor, Breadcrumbs, Collapse, Grid, Paper, SimpleGrid, Stack, Table, Text, Title } from '@mantine/core';
import { IconChevronDown, IconChevronRight } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { Fragment, useState } from 'react';
import { Link, useParams } from 'react-router';
import type { InvoiceDetails } from '../../api/types';
import { FiscalNotice } from '../../components/FiscalNotice';
import { CivilDate, DateTime, Money, StatusBadge } from '../../components/format';
import { EmptyState, LoadingState, QueryContent } from '../../components/states';
import { KeyValue, PageHeader } from '../../components/ui';
import {
  actorTypeLabels,
  invoiceHistoryReasonLabels,
  invoiceItemKindLabels,
  invoiceKindLabels,
  invoiceStatusLabels,
  labelOf,
  systemActorLabels,
} from '../../lib/labels';
import { formatQuantity, formatUnitAmountDecimal } from '../../lib/money';
import { useTenant } from '../../session/SessionContext';
import { ChargesTable } from '../payments/ChargesTable';
import { invoicesApi } from './api';
import { InvoiceActions } from './InvoiceActions';
import { InvoiceStatusBadges } from './InvoicesTable';
import { CustomerSnapshotView, PriceSnapshotView } from './Snapshots';
import { ResponsiveTable } from '../../components/ResponsiveTable';

export function InvoiceTotals({ invoice }: { invoice: InvoiceDetails }) {
  const c = invoice.currency;
  const rows: { label: string; value: number; strong?: boolean; hint?: string }[] = [
    { label: 'Subtotal', value: invoice.subtotal },
    { label: 'Desconto', value: invoice.discount_total, hint: 'Cupons e descontos aplicados' },
    { label: 'Total', value: invoice.total, strong: true },
    { label: 'Crédito aplicado', value: invoice.credit_applied, hint: 'Saldo de crédito do cliente usado nesta fatura' },
    { label: 'Pago', value: invoice.amount_paid },
    { label: 'Creditado', value: invoice.amount_credited, hint: 'Notas de crédito emitidas' },
    { label: 'Baixado como incobrável', value: invoice.amount_written_off },
    { label: 'Saldo devedor', value: invoice.amount_due, strong: true },
  ];
  return (
    <Paper withBorder p="md" radius="md">
      <Title order={2} size="h5" mb="xs" id="invoice-totals">
        Totais
      </Title>
      <Table aria-labelledby="invoice-totals" withRowBorders={false} verticalSpacing={4} highlightOnHover={false}>
        <Table.Tbody>
          {rows.map((r) => (
            <Table.Tr key={r.label}>
              <Table.Th scope="row" fw={r.strong ? 700 : 400} title={r.hint}>
                {r.label}
              </Table.Th>
              <Table.Td ta="right">
                <Money value={r.value} currency={c} strong={r.strong} />
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
      <Text size="xs" c="dimmed" mt="xs">
        Valores em {currencyName(c)}.
      </Text>
    </Paper>
  );
}

function currencyName(code: string): string {
  try {
    const name = new Intl.DisplayNames('pt-BR', { type: 'currency' }).of(code);
    return name && name !== code ? `${name.charAt(0).toLowerCase()}${name.slice(1)} (${code})` : code;
  } catch {
    return code;
  }
}

function ItemsTable({ invoice }: { invoice: InvoiceDetails }) {
  const [open, setOpen] = useState<string | null>(null);
  const items = [...invoice.items].sort((a, b) => a.position - b.position);
  return (
    <ResponsiveTable minWidth={900}>
      <Table aria-label="Itens da fatura">
        <Table.Thead>
          <Table.Tr>
            <Table.Th w={40}>
              <span className="visually-hidden">Detalhes</span>
            </Table.Th>
            <Table.Th>Descrição</Table.Th>
            <Table.Th>Tipo</Table.Th>
            <Table.Th>Período</Table.Th>
            <Table.Th ta="right">Quantidade</Table.Th>
            <Table.Th ta="right">Preço unitário</Table.Th>
            <Table.Th ta="right">Desconto</Table.Th>
            <Table.Th ta="right">Valor</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {items.map((item) => {
            const expanded = open === item.id;
            return (
              <Fragment key={item.id}>
                <Table.Tr>
                  <Table.Td>
                    <ActionIcon
                      variant="subtle"
                      size="sm"
                      aria-expanded={expanded}
                      aria-label={expanded ? 'Ocultar preço registrado' : 'Mostrar preço registrado no item'}
                      onClick={() => setOpen(expanded ? null : item.id)}
                    >
                      {expanded ? <IconChevronDown size={16} /> : <IconChevronRight size={16} />}
                    </ActionIcon>
                  </Table.Td>
                  <Table.Td>{item.description}</Table.Td>
                  <Table.Td>{labelOf(invoiceItemKindLabels, item.kind)}</Table.Td>
                  <Table.Td>
                    {item.period_start ? (
                      <Text size="sm">
                        <DateTime value={item.period_start} dateOnly /> – <DateTime value={item.period_end} dateOnly />
                      </Text>
                    ) : (
                      '—'
                    )}
                  </Table.Td>
                  <Table.Td ta="right" className="numeric">
                    {formatQuantity(item.quantity)}
                  </Table.Td>
                  <Table.Td ta="right" className="numeric">
                    {formatUnitAmountDecimal(item.unit_amount_decimal, invoice.currency)}
                  </Table.Td>
                  <Table.Td ta="right">
                    <Money value={item.discount_amount} currency={invoice.currency} />
                  </Table.Td>
                  <Table.Td ta="right">
                    <Money value={item.amount} currency={invoice.currency} />
                  </Table.Td>
                </Table.Tr>
                <Table.Tr style={{ display: expanded ? undefined : 'none' }}>
                  <Table.Td colSpan={8} bg="gray.0">
                    <Collapse in={expanded}>
                      <Text size="xs" c="dimmed" mb="xs">
                        Preço registrado no momento da emissão. Mudanças posteriores no catálogo não alteram este item.
                      </Text>
                      <PriceSnapshotView snapshot={(item.price_snapshot ?? {}) as Record<string, unknown>} currency={invoice.currency} />
                    </Collapse>
                  </Table.Td>
                </Table.Tr>
              </Fragment>
            );
          })}
        </Table.Tbody>
      </Table>
    </ResponsiveTable>
  );
}

/** Rotinas automáticas pelo nome; pessoas e integrações pelo tipo de ator. */
function actorName(type: string, id?: string | null): string {
  if ((type === 'system' || type === 'provider') && id && systemActorLabels[id]) return systemActorLabels[id];
  return labelOf(actorTypeLabels, type);
}

function HistoryTable({ invoice }: { invoice: InvoiceDetails }) {
  if (invoice.history.length === 0) return <Text size="sm" c="dimmed">Sem transições.</Text>;
  return (
    <ResponsiveTable minWidth={640}>
      <Table aria-label="Histórico da fatura">
        <Table.Thead>
          <Table.Tr>
            <Table.Th>Quando</Table.Th>
            <Table.Th>Transição</Table.Th>
            <Table.Th>Ator</Table.Th>
            <Table.Th>Motivo</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {invoice.history.map((h) => (
            <Table.Tr key={h.id}>
              <Table.Td>
                <DateTime value={h.occurred_at} />
              </Table.Td>
              <Table.Td>
                {h.from_status ? labelOf(invoiceStatusLabels, h.from_status) : '—'} → <StatusBadge status={h.to_status} labels={invoiceStatusLabels} />
              </Table.Td>
              <Table.Td>{actorName(h.actor_type, h.actor_id)}</Table.Td>
              <Table.Td>{h.reason ? labelOf(invoiceHistoryReasonLabels, h.reason) : '—'}</Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </ResponsiveTable>
  );
}

function CreditNotesTable({ invoice }: { invoice: InvoiceDetails }) {
  const notes = invoice.credit_notes ?? [];
  if (notes.length === 0) return <Text size="sm" c="dimmed">Nenhuma nota de crédito emitida.</Text>;
  return (
    <ResponsiveTable minWidth={560}>
      <Table aria-label="Notas de crédito">
        <Table.Thead>
          <Table.Tr>
            <Table.Th scope="col">Número</Table.Th>
            <Table.Th scope="col">Emitida em</Table.Th>
            <Table.Th scope="col">Motivo</Table.Th>
            <Table.Th scope="col" ta="right">
              Valor
            </Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {notes.map((n) => (
            <Table.Tr key={n.id}>
              <Table.Td>{n.number}</Table.Td>
              <Table.Td>
                <DateTime value={n.created_at} />
              </Table.Td>
              <Table.Td>{n.reason}</Table.Td>
              <Table.Td ta="right">
                <Money value={n.amount} currency={invoice.currency} />
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </ResponsiveTable>
  );
}

export function InvoiceDetailView({ invoice }: { invoice: InvoiceDetails }) {
  const snapshot = invoice.customer_snapshot ?? {};
  return (
    <Stack gap="lg">
      <PageHeader
        title={`Fatura ${invoice.number ?? '(rascunho)'}`}
        badge={<InvoiceStatusBadges invoice={invoice} />}
        description={labelOf(invoiceKindLabels, invoice.kind)}
        actions={<InvoiceActions invoice={invoice} />}
      />
      <FiscalNotice />
      <Grid align="flex-start">
        <Grid.Col span={{ base: 12, md: 8 }}>
          <Paper withBorder p="md" radius="md">
            <SimpleGrid cols={{ base: 1, xs: 2, lg: 3 }} spacing="lg">
              <KeyValue label="Cliente">
                <Anchor component={Link} to={`/clientes/${invoice.customer_id}`}>
                  {invoice.customer_name || String(snapshot.name ?? 'Ver cliente')}
                </Anchor>
              </KeyValue>
              <KeyValue label="Assinatura">
                {invoice.subscription_id ? (
                  <Anchor component={Link} to={`/assinaturas/${invoice.subscription_id}`}>
                    Ver assinatura
                  </Anchor>
                ) : (
                  '—'
                )}
              </KeyValue>
              <KeyValue label="Período faturado">
                {invoice.billing_period_start ? (
                  <>
                    <DateTime value={invoice.billing_period_start} dateOnly /> – <DateTime value={invoice.billing_period_end} dateOnly />
                  </>
                ) : (
                  '—'
                )}
              </KeyValue>
              <KeyValue label="Emissão">{invoice.issued_at ? <DateTime value={invoice.issued_at} /> : 'Não emitida'}</KeyValue>
              <KeyValue label="Vencimento">
                <CivilDate value={invoice.due_date} />
              </KeyValue>
              <KeyValue label="Pagamento">{invoice.paid_at ? <DateTime value={invoice.paid_at} /> : '—'}</KeyValue>
              {invoice.voided_at && (
                <KeyValue label="Cancelada em">
                  <DateTime value={invoice.voided_at} />
                </KeyValue>
              )}
              {invoice.void_reason && <KeyValue label="Motivo do cancelamento">{invoice.void_reason}</KeyValue>}
            </SimpleGrid>
          </Paper>
        </Grid.Col>
        <Grid.Col span={{ base: 12, md: 4 }}>
          <InvoiceTotals invoice={invoice} />
        </Grid.Col>
      </Grid>
      <Stack gap="xs">
        <Title order={2} size="h4">
          Itens
        </Title>
        {invoice.items.length === 0 ? <EmptyState title="Fatura sem itens" /> : <ItemsTable invoice={invoice} />}
      </Stack>
      <Stack gap="xs">
        <Title order={2} size="h4">
          Cobranças
        </Title>
        <Text size="sm" c="dimmed">
          Cada tentativa é uma cobrança avulsa no provedor. O pagamento acontece no checkout hospedado do provedor; o
          Recorvia não recebe dados de cartão.
        </Text>
        {invoice.charges.length === 0 ? (
          <EmptyState title="Nenhuma cobrança" description="A cobrança é solicitada ao provedor quando a fatura é emitida." />
        ) : (
          <ChargesTable charges={invoice.charges} />
        )}
      </Stack>
      <Stack gap="xs">
        <Title order={2} size="h4">
          Notas de crédito
        </Title>
        <Text size="sm" c="dimmed">
          Reduzem o saldo em aberto sem reescrever a fatura emitida; cada nota gera lançamento no ledger.
        </Text>
        <CreditNotesTable invoice={invoice} />
      </Stack>
      <Stack gap="xs">
        <Title order={2} size="h4">
          Histórico
        </Title>
        <HistoryTable invoice={invoice} />
      </Stack>
      <Stack gap="xs">
        <Title order={2} size="h4">
          Dados do cliente na emissão
        </Title>
        <Text size="sm" c="dimmed">
          Cópia do cadastro usada no documento da fatura; alterações posteriores no cliente não mudam estes dados.
        </Text>
        <Paper withBorder p="md" radius="md">
          <CustomerSnapshotView snapshot={snapshot as Record<string, unknown>} />
        </Paper>
      </Stack>
    </Stack>
  );
}

export function InvoiceDetailPage() {
  const { id = '' } = useParams();
  const { keys } = useTenant();
  const query = useQuery({ queryKey: keys('invoices', 'detail', id), queryFn: ({ signal }) => invoicesApi.get(id, signal) });
  return (
    <Stack gap="md">
      <Breadcrumbs>
        <Anchor component={Link} to="/faturas">
          Faturas
        </Anchor>
        <Text>{query.data?.number ?? 'Fatura'}</Text>
      </Breadcrumbs>
      <QueryContent query={query} loading={<LoadingState variant="detail" />}>
        {(invoice) => <InvoiceDetailView invoice={invoice} />}
      </QueryContent>
    </Stack>
  );
}
