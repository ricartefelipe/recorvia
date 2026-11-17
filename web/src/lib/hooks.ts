import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import type { FieldValues } from 'react-hook-form';
import { useSearchParams } from 'react-router';
import type { QueryDomain } from '../api/queryKeys';
import { useTenant } from '../session/SessionContext';
import { handleMutationError, type FormErrorTarget } from './errors';
import { notifySuccess } from './notify';

/**
 * Paginação por cursor: mantém a pilha de cursores visitados e volta à
 * primeira página quando os filtros mudam.
 */
export function useCursorPagination(filterKey: string) {
  const [state, setState] = useState<{ key: string; cursors: string[] }>({ key: filterKey, cursors: [] });
  const current = state.key === filterKey ? state : { key: filterKey, cursors: [] };
  if (state.key !== filterKey) setState(current);
  return {
    cursor: current.cursors.at(-1),
    page: current.cursors.length + 1,
    hasPrevious: current.cursors.length > 0,
    next: (cursor: string) => setState({ key: filterKey, cursors: [...current.cursors, cursor] }),
    previous: () => setState({ key: filterKey, cursors: current.cursors.slice(0, -1) }),
  };
}

/** Filtros de lista guardados na URL (permitem voltar e compartilhar o link). */
export function useUrlFilters<K extends string>(keys: readonly K[]) {
  const [params, setParams] = useSearchParams();
  const values = Object.fromEntries(keys.map((k) => [k, params.get(k) ?? ''])) as Record<K, string>;
  const update = (patch: Partial<Record<K, string | null | undefined>>) => {
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        for (const [k, v] of Object.entries(patch) as [K, string | null | undefined][]) {
          if (v === null || v === undefined || v === '') next.delete(k);
          else next.set(k, v);
        }
        return next;
      },
      { replace: true },
    );
  };
  return [values, update] as const;
}

export interface ApiMutationOptions<TVars, TData, TForm extends FieldValues> {
  mutationFn: (vars: TVars) => Promise<TData>;
  /** Domínios de consulta a recarregar após sucesso (e após 412). */
  invalidate?: QueryDomain[];
  successMessage?: string | ((data: TData, vars: TVars) => string);
  onSuccess?: (data: TData, vars: TVars) => void;
  onError?: (error: unknown) => void;
  form?: FormErrorTarget<TForm>;
  fieldMap?: Record<string, string>;
}

/** Mutation com notificação de sucesso, invalidação por domínio e tratamento de erros. */
export function useApiMutation<TVars = void, TData = unknown, TForm extends FieldValues = FieldValues>(
  options: ApiMutationOptions<TVars, TData, TForm>,
) {
  const queryClient = useQueryClient();
  const { keys } = useTenant();
  const invalidateAll = () =>
    Promise.all((options.invalidate ?? []).map((domain) => queryClient.invalidateQueries({ queryKey: keys(domain) })));

  return useMutation<TData, unknown, TVars>({
    mutationFn: options.mutationFn,
    onSuccess: async (data, vars) => {
      if (options.successMessage) {
        notifySuccess(
          typeof options.successMessage === 'function' ? options.successMessage(data, vars) : options.successMessage,
        );
      }
      options.onSuccess?.(data, vars);
      await invalidateAll();
    },
    onError: (error) => {
      handleMutationError(error, {
        form: options.form,
        fieldMap: options.fieldMap,
        onConcurrentChange: () => void invalidateAll(),
      });
      options.onError?.(error);
    },
  });
}
