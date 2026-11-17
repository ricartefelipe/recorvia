import { Alert, Select, SimpleGrid, Stack, Text, TextInput } from '@mantine/core';
import { IconInfoCircle } from '@tabler/icons-react';
import { Controller, useFormContext, useWatch } from 'react-hook-form';
import { z } from 'zod';
import type { Metric, PriceInput, PriceVersion, PricingModel } from '../../api/types';
import { MoneyInput } from '../../components/inputs';
import { aggregationLabels, labelOf, pricingModelLabels } from '../../lib/labels';
import {
  centsDecimalToInput,
  centsToInput,
  formatMoney,
  formatQuantity,
  formatUnitAmountDecimal,
  parseMoneyToNumber,
  parseQuantity,
  parseUnitAmountToCentsDecimal,
} from '../../lib/money';

export const PRICING_MODELS: PricingModel[] = ['flat', 'per_unit', 'usage', 'flat_plus_overage'];

const MODEL_HELP: Record<PricingModel, string> = {
  flat: 'Valor fixo cobrado a cada período, independentemente de quantidade ou consumo.',
  per_unit: 'Preço por unidade multiplicado pela quantidade contratada na assinatura (por exemplo, usuários).',
  usage: 'Cobrança apenas pelo consumo medido no período, agregado conforme a métrica.',
  flat_plus_overage: 'Valor fixo com uma franquia incluída; o consumo acima da franquia é cobrado por unidade.',
};

export const needs = {
  flat: (m: PricingModel) => m === 'flat' || m === 'flat_plus_overage',
  unit: (m: PricingModel) => m !== 'flat',
  metric: (m: PricingModel) => m === 'usage' || m === 'flat_plus_overage',
  included: (m: PricingModel) => m === 'flat_plus_overage',
};

export interface PriceFormValues {
  pricing_model: PricingModel;
  flat_amount: string;
  unit_amount: string;
  metric_id: string;
  included_quantity: string;
}

export const emptyPrice: PriceFormValues = {
  pricing_model: 'flat',
  flat_amount: '',
  unit_amount: '',
  metric_id: '',
  included_quantity: '',
};

/** Valores iniciais a partir de uma versão existente (para criar a próxima). */
export function priceToForm(price: PriceVersion | null | undefined): PriceFormValues {
  if (!price) return emptyPrice;
  return {
    pricing_model: price.pricing_model,
    flat_amount: price.flat_amount !== null && price.flat_amount !== undefined ? centsToInput(price.flat_amount, price.currency) : '',
    unit_amount: price.unit_amount_decimal ? centsDecimalToInput(price.unit_amount_decimal) : '',
    metric_id: price.metric_id ?? '',
    included_quantity: price.included_quantity ? price.included_quantity.replace('.', ',') : '',
  };
}

/** Validação e montagem do PriceInput conforme o modelo escolhido. */
export const priceSchema = z
  .object({
    pricing_model: z.enum(['flat', 'per_unit', 'usage', 'flat_plus_overage']),
    flat_amount: z.string(),
    unit_amount: z.string(),
    metric_id: z.string(),
    included_quantity: z.string(),
  })
  .transform((v, ctx): PriceInput => {
    const out: PriceInput = { pricing_model: v.pricing_model };
    const model = v.pricing_model;
    if (needs.flat(model)) {
      const r = parseMoneyToNumber(v.flat_amount);
      if (!r.ok) ctx.addIssue({ code: 'custom', path: ['flat_amount'], message: r.error });
      else if (model === 'flat' && r.value < 1) ctx.addIssue({ code: 'custom', path: ['flat_amount'], message: 'Informe um valor maior que zero.' });
      else out.flat_amount = r.value;
    }
    if (needs.unit(model)) {
      const r = parseUnitAmountToCentsDecimal(v.unit_amount);
      if (!r.ok) ctx.addIssue({ code: 'custom', path: ['unit_amount'], message: r.error });
      else if (r.value === '0') ctx.addIssue({ code: 'custom', path: ['unit_amount'], message: 'Informe um preço maior que zero.' });
      else out.unit_amount_decimal = r.value;
    }
    if (needs.metric(model)) {
      if (!v.metric_id) ctx.addIssue({ code: 'custom', path: ['metric_id'], message: 'Selecione a métrica de consumo.' });
      else out.metric_id = v.metric_id;
    }
    if (needs.included(model)) {
      const r = parseQuantity(v.included_quantity, { allowZero: true });
      if (!r.ok) ctx.addIssue({ code: 'custom', path: ['included_quantity'], message: r.error });
      else out.included_quantity = r.value;
    }
    return out;
  });

export function describePrice(price: PriceVersion | null | undefined, metrics: Metric[] = [], interval?: 'month' | 'year'): string {
  if (!price) return 'Sem preço definido';
  const per = interval === 'year' ? 'por ano' : interval === 'month' ? 'por mês' : 'por período';
  const metric = metrics.find((m) => m.id === price.metric_id);
  const unit = metric ? metric.unit : 'unidade';
  const unitPrice = formatUnitAmountDecimal(price.unit_amount_decimal, price.currency);
  switch (price.pricing_model) {
    case 'flat':
      return `${formatMoney(price.flat_amount, price.currency)} ${per}`;
    case 'per_unit':
      return `${unitPrice} por unidade ${per}`;
    case 'usage':
      return `${unitPrice} por ${unit} consumido`;
    case 'flat_plus_overage':
      return `${formatMoney(price.flat_amount, price.currency)} ${per} com ${formatQuantity(price.included_quantity)} ${unit} incluídos; excedente a ${unitPrice} por ${unit}`;
    default:
      return labelOf(pricingModelLabels, price.pricing_model);
  }
}

type WithPrice = { price: PriceFormValues };

/** Campos de preço que mudam conforme o modelo (flat, per_unit, usage, flat_plus_overage). */
export function PriceFields({ metrics }: { metrics: Metric[] }) {
  const { control, register, formState } = useFormContext<WithPrice>();
  const model = useWatch({ control, name: 'price.pricing_model' });
  const errors = formState.errors.price;
  const activeMetrics = metrics.filter((m) => m.status === 'active');

  return (
    <Stack gap="sm">
      <Controller
        control={control}
        name="price.pricing_model"
        render={({ field }) => (
          <Select
            label="Modelo de preço"
            required
            data={PRICING_MODELS.map((m) => ({ value: m, label: pricingModelLabels[m] }))}
            value={field.value}
            onChange={(v) => v && field.onChange(v)}
            allowDeselect={false}
            description={MODEL_HELP[model]}
          />
        )}
      />
      <SimpleGrid cols={{ base: 1, sm: 2 }}>
        {needs.flat(model) && (
          <MoneyInput
            label={model === 'flat' ? 'Valor fixo por período' : 'Valor fixo (inclui a franquia)'}
            required
            {...register('price.flat_amount')}
            error={errors?.flat_amount?.message}
          />
        )}
        {needs.metric(model) && (
          <Controller
            control={control}
            name="price.metric_id"
            render={({ field, fieldState }) => (
              <Select
                label="Métrica de consumo"
                required
                placeholder={activeMetrics.length ? 'Selecione' : 'Cadastre uma métrica primeiro'}
                data={activeMetrics.map((m) => ({
                  value: m.id,
                  label: `${m.name} (${m.unit}, ${labelOf(aggregationLabels, m.aggregation).toLowerCase()})`,
                }))}
                value={field.value || null}
                onChange={(v) => field.onChange(v ?? '')}
                onBlur={field.onBlur}
                error={fieldState.error?.message}
                searchable
              />
            )}
          />
        )}
        {needs.included(model) && (
          <TextInput
            label="Franquia incluída"
            description="Quantidade consumida sem custo adicional no período."
            required
            inputMode="decimal"
            {...register('price.included_quantity')}
            error={errors?.included_quantity?.message}
          />
        )}
        {needs.unit(model) && (
          <MoneyInput
            label={
              model === 'per_unit'
                ? 'Preço por unidade'
                : model === 'usage'
                  ? 'Preço por unidade consumida'
                  : 'Preço por unidade excedente'
            }
            description="Aceita frações de centavo, por exemplo 0,0015."
            required
            {...register('price.unit_amount')}
            error={errors?.unit_amount?.message}
          />
        )}
      </SimpleGrid>
      {needs.metric(model) && activeMetrics.length === 0 && (
        <Alert color="orange" icon={<IconInfoCircle />}>
          <Text size="sm">Não há métricas de consumo ativas. Cadastre uma na aba Métricas antes de usar este modelo.</Text>
        </Alert>
      )}
    </Stack>
  );
}
