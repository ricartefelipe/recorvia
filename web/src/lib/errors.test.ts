import { renderHook } from '@testing-library/react';
import { useForm } from 'react-hook-form';
import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/http';
import { applyFieldErrors, describeError, normalizeFieldPath } from './errors';

describe('erros de campo do problem+json', () => {
  it('normaliza caminhos de campo', () => {
    expect(normalizeFieldPath('price.flat_amount')).toBe('price.flat_amount');
    expect(normalizeFieldPath('/price/flat_amount')).toBe('price.flat_amount');
    expect(normalizeFieldPath('reminder_offsets_days[2]')).toBe('reminder_offsets_days.2');
  });

  it('aplica mensagens do servidor nos campos e devolve as desconhecidas', () => {
    const { result } = renderHook(() =>
      useForm({ defaultValues: { name: '', document_number: '', price: { flat_amount: '' } } }),
    );
    const unmatched = applyFieldErrors(
      [
        { field: 'document_number', message: 'CPF com dígitos inválidos' },
        { field: 'price.flat_amount', message: 'Obrigatório para valor fixo' },
        { field: 'campo_inexistente', message: 'Erro solto' },
      ],
      result.current,
    );
    expect(result.current.getFieldState('document_number').error?.message).toBe('CPF com dígitos inválidos');
    expect(result.current.getFieldState('price.flat_amount').error?.message).toBe('Obrigatório para valor fixo');
    expect(unmatched).toEqual([{ field: 'campo_inexistente', message: 'Erro solto' }]);
  });

  it('descreve erros 5xx com o código da requisição', () => {
    const description = describeError(new ApiError({ status: 500, code: 'internal', requestId: 'req-1' }));
    expect(description.title).toBe('Erro no servidor');
    expect(description.requestId).toBe('req-1');
    expect(describeError(new ApiError({ status: 412, code: 'precondition_failed' })).title).toBe('Alteração concorrente');
  });
});
