import { SimpleGrid, Text } from '@mantine/core';
import type { ReactNode } from 'react';
import { DateTime, JsonView } from '../../components/format';
import { KeyValue } from '../../components/ui';
import { formatDocument } from '../../lib/documents';
import { billingIntervalLabels, labelOf, pricingModelLabels } from '../../lib/labels';
import { formatMoney, formatQuantity, formatUnitAmountDecimal } from '../../lib/money';

type Snapshot = Record<string, unknown>;

const text = (value: unknown): string | undefined => (typeof value === 'string' && value.trim() ? value : undefined);
const num = (value: unknown): number | undefined => (typeof value === 'number' && Number.isFinite(value) ? value : undefined);

/** Endereço do snapshot em linhas ("Rua X, 10 — Sala 2", "Centro, Cidade/UF", "CEP 00000-000"). */
export function formatSnapshotAddress(address: unknown): string[] {
  if (!address || typeof address !== 'object') return [];
  const a = address as Snapshot;
  const street = [text(a.street), text(a.number)].filter(Boolean).join(', ');
  const line1 = [street, text(a.complement)].filter(Boolean).join(' — ');
  const cityState = [text(a.city), text(a.state)].filter(Boolean).join('/');
  const line2 = [text(a.district), cityState].filter(Boolean).join(', ');
  const postal = text(a.postal_code);
  const line3 = [postal && `CEP ${postal}`, text(a.country) && text(a.country) !== 'BR' ? text(a.country) : undefined]
    .filter(Boolean)
    .join(' · ');
  return [line1, line2, line3].filter((l): l is string => !!l);
}

/**
 * Cópia do cadastro do cliente gravada na emissão, como lista descritiva:
 * nome, razão social, documento formatado, e-mail e endereço.
 */
export function CustomerSnapshotView({ snapshot }: { snapshot: Snapshot }) {
  const docType = text(snapshot.document_type);
  const document = text(snapshot.document_number);
  const address = formatSnapshotAddress(snapshot.address);
  const capturedAt = text(snapshot.captured_at);
  return (
    <SimpleGrid cols={{ base: 1, xs: 2, md: 3 }} spacing="lg">
      <Entry label="Nome">{text(snapshot.name) ?? '—'}</Entry>
      {text(snapshot.legal_name) && <Entry label="Razão social">{text(snapshot.legal_name)}</Entry>}
      <Entry label={docType === 'cnpj' ? 'CNPJ' : docType === 'cpf' ? 'CPF' : 'Documento'}>
        {document ? formatDocument(document, docType === 'cpf' || docType === 'cnpj' ? docType : undefined) : '—'}
      </Entry>
      <Entry label="E-mail">{text(snapshot.email) ?? '—'}</Entry>
      <Entry label="Endereço">
        {address.length ? address.map((line) => <div key={line}>{line}</div>) : 'Não informado'}
      </Entry>
      {capturedAt && (
        <Entry label="Registrado em">
          <DateTime value={capturedAt} />
        </Entry>
      )}
    </SimpleGrid>
  );
}

/** Preço registrado no item da fatura, em linguagem comum; o restante fica em "Dados técnicos". */
export function PriceSnapshotView({ snapshot, currency }: { snapshot: Snapshot; currency: string }) {
  const planName = text(snapshot.plan_name);
  const version = num(snapshot.price_version);
  const model = text(snapshot.pricing_model);
  const interval = text(snapshot.billing_interval);
  const flat = num(snapshot.flat_amount);
  const unit = text(snapshot.unit_amount_decimal);
  const included = text(snapshot.included_quantity);
  const coupon = text(snapshot.coupon_code);
  const metric = text(snapshot.metric_code);
  const quantity = text(snapshot.aggregated_quantity);
  const known = planName || model || flat !== undefined || unit || coupon || metric;
  return (
    <>
      {known ? (
        <SimpleGrid cols={{ base: 1, xs: 2, md: 4 }} spacing="md">
          {planName && (
            <Entry label="Plano">
              {planName}
              {version !== undefined && ` (versão ${version} do preço)`}
            </Entry>
          )}
          {model && <Entry label="Modelo de preço">{labelOf(pricingModelLabels, model)}</Entry>}
          {interval && <Entry label="Periodicidade">{labelOf(billingIntervalLabels, interval)}</Entry>}
          {flat !== undefined && <Entry label="Valor fixo">{formatMoney(flat, currency)}</Entry>}
          {unit && <Entry label="Preço unitário">{formatUnitAmountDecimal(unit, currency)}</Entry>}
          {included && <Entry label="Quantidade incluída">{formatQuantity(included)}</Entry>}
          {metric && <Entry label="Métrica">{metric}</Entry>}
          {quantity && <Entry label="Consumo apurado">{formatQuantity(quantity)}</Entry>}
          {coupon && <Entry label="Cupom">{coupon}</Entry>}
        </SimpleGrid>
      ) : (
        <Text size="sm" c="dimmed">
          Sem dados de preço registrados para este item.
        </Text>
      )}
      <details style={{ marginTop: 'var(--mantine-spacing-xs)' }}>
        <summary style={{ cursor: 'pointer', fontSize: 'var(--mantine-font-size-xs)' }}>Dados técnicos do preço</summary>
        <JsonView value={snapshot} />
      </details>
    </>
  );
}

function Entry({ label, children }: { label: string; children: ReactNode }) {
  return <KeyValue label={label}>{children}</KeyValue>;
}
