import { zodResolver } from '@hookform/resolvers/zod';
import { Button } from '@mantine/core';
import { screen, waitFor } from '@testing-library/react';
import { FormProvider, useForm } from 'react-hook-form';
import { describe, expect, it, vi } from 'vitest';
import { z } from 'zod';
import type { Metric, PriceInput } from '../../api/types';
import { renderPage } from '../../test/render';
import { emptyPrice, PriceFields, priceSchema, type PriceFormValues } from './PriceFields';

const metric: Metric = {
  id: '0195f1a0-0000-7000-8000-0000000cafe1',
  code: 'api.calls',
  name: 'Chamadas de API',
  unit: 'api_calls',
  aggregation: 'sum',
  max_quantity_per_event: '10000',
  late_grace_hours: 24,
  status: 'active',
  created_at: '2026-10-01T12:00:00Z',
};

const schema = z.object({ price: priceSchema });

function Harness({ onSubmit, metrics = [metric] }: { onSubmit: (price: PriceInput) => void; metrics?: Metric[] }) {
  const form = useForm<{ price: PriceFormValues }, unknown, { price: PriceInput }>({
    resolver: zodResolver(schema),
    defaultValues: { price: emptyPrice },
  });
  return (
    <FormProvider {...form}>
      <form onSubmit={form.handleSubmit((v) => onSubmit(v.price))} noValidate>
        <PriceFields metrics={metrics} />
        <Button type="submit">Salvar preço</Button>
      </form>
    </FormProvider>
  );
}

async function chooseModel(user: ReturnType<typeof renderPage>['user'], label: string) {
  await user.click(screen.getByRole('textbox', { name: /Modelo de preço/ }));
  await user.click(await screen.findByRole('option', { name: label }));
}

describe('formulário de preço por modelo', () => {
  it('valor fixo mostra apenas o valor por período e envia centavos', async () => {
    const onSubmit = vi.fn();
    const { user } = renderPage(<Harness onSubmit={onSubmit} />);
    expect(screen.getByLabelText(/Valor fixo por período/)).toBeInTheDocument();
    expect(screen.queryByLabelText(/Métrica de consumo/)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/Preço por unidade/)).not.toBeInTheDocument();
    await user.type(screen.getByLabelText(/Valor fixo por período/), '1.299,90');
    await user.click(screen.getByRole('button', { name: 'Salvar preço' }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledWith({ pricing_model: 'flat', flat_amount: 129990 }));
  });

  it('por quantidade pede só o preço unitário (aceita fração de centavo)', async () => {
    const onSubmit = vi.fn();
    const { user } = renderPage(<Harness onSubmit={onSubmit} />);
    await chooseModel(user, 'Por quantidade (unidades ou usuários)');
    expect(screen.queryByLabelText(/Valor fixo/)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/Métrica de consumo/)).not.toBeInTheDocument();
    await user.type(screen.getByLabelText(/^Preço por unidade/), '49,9');
    await user.click(screen.getByRole('button', { name: 'Salvar preço' }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledWith({ pricing_model: 'per_unit', unit_amount_decimal: '4990' }));
  });

  it('fixo com excedente exige valor, métrica, franquia e preço do excedente', async () => {
    const onSubmit = vi.fn();
    const { user } = renderPage(<Harness onSubmit={onSubmit} />);
    await chooseModel(user, 'Fixo com excedente de consumo');
    expect(screen.getByLabelText(/Valor fixo \(inclui a franquia\)/)).toBeInTheDocument();
    expect(screen.getByLabelText(/Franquia incluída/)).toBeInTheDocument();
    expect(screen.getByLabelText(/Preço por unidade excedente/)).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Salvar preço' }));
    expect(await screen.findByText('Selecione a métrica de consumo.')).toBeInTheDocument();
    expect(onSubmit).not.toHaveBeenCalled();

    await user.type(screen.getByLabelText(/Valor fixo \(inclui a franquia\)/), '199');
    await user.click(screen.getByRole('textbox', { name: /Métrica de consumo/ }));
    await user.click(await screen.findByRole('option', { name: /Chamadas de API/ }));
    await user.type(screen.getByLabelText(/Franquia incluída/), '10000');
    await user.type(screen.getByLabelText(/Preço por unidade excedente/), '0,0015');
    await user.click(screen.getByRole('button', { name: 'Salvar preço' }));
    await waitFor(() =>
      expect(onSubmit).toHaveBeenCalledWith({
        pricing_model: 'flat_plus_overage',
        flat_amount: 19900,
        metric_id: metric.id,
        included_quantity: '10000',
        unit_amount_decimal: '0.15',
      }),
    );
  });

  it('por consumo sem métricas ativas orienta o cadastro', async () => {
    const { user } = renderPage(<Harness onSubmit={vi.fn()} metrics={[]} />);
    await chooseModel(user, 'Por consumo medido');
    expect(screen.getByText(/Não há métricas de consumo ativas/)).toBeInTheDocument();
    expect(screen.getByLabelText(/Preço por unidade consumida/)).toBeInTheDocument();
  });
});
