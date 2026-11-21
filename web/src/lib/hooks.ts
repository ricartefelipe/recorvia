import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useCallback, useEffect, useState } from 'react';
import type { FieldValues } from 'react-hook-form';
import { useSearchParams } from 'react-router';
import type { QueryDomain } from '../api/queryKeys';
import { useTenant } from '../session/SessionContext';
import { handleMutationError, type FormErrorTarget } from './errors';
import { discardsIdempotencyKey, type IdempotencyKeeper } from './idempotency';
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

/**
 * Volta à página anterior quando uma página depois da primeira fica vazia,
 * por exemplo quando uma ação na própria lista tira dela os últimos itens da
 * página. Sem isso, a tela mostraria o estado de lista vazia e esconderia o
 * paginador, embora as páginas anteriores ainda tenham itens. Enquanto a
 * página nova carrega (dados da página anterior em exibição), nada muda.
 */
export function useReturnFromEmptyPage(
  pagination: { hasPrevious: boolean; previous: () => void },
  items: readonly unknown[] | undefined,
  isPlaceholderData = false,
) {
  const empty = pagination.hasPrevious && !isPlaceholderData && items?.length === 0;
  const { previous } = pagination;
  useEffect(() => {
    if (empty) previous();
  }, [empty, previous]);
}

/** Filtros de lista guardados na URL (permitem voltar e compartilhar o link). */
export function useUrlFilters<K extends string>(keys: readonly K[]) {
  const [params, setParams] = useSearchParams();
  const values = Object.fromEntries(keys.map((k) => [k, params.get(k) ?? ''])) as Record<K, string>;
  const update = useCallback(
    (patch: Partial<Record<K, string | null | undefined>>) => {
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
    },
    [setParams],
  );
  return [values, update] as const;
}

export interface ApiMutationOptions<TVars, TData, TForm extends FieldValues> {
  mutationFn: (vars: TVars) => Promise<TData>;
  /** Domínios de consulta a recarregar após sucesso (e após 412). */
  invalidate?: QueryDomain[];
  successMessage?: string | ((data: TData, vars: TVars) => string);
  onSuccess?: (data: TData, vars: TVars) => void;
  onError?: (error: unknown) => void;
  /**
   * Executado quando, depois de um 412, a pessoa escolhe recarregar os dados.
   * Os modais de edição que guardam o recurso aberto fecham aqui: a lista
   * recarregada mostra a versão atual, e a alteração é refeita sobre ela, sem
   * que uma recarga automática troque a versão por baixo do formulário.
   */
  onConcurrentReload?: () => void;
  form?: FormErrorTarget<TForm>;
  fieldMap?: Record<string, string>;
  /**
   * Chave de idempotência do envio (ADR-022): descartada depois do sucesso e
   * da recusa 4xx da API, porque o próximo envio é outra intenção; mantida
   * depois de erro de rede, 5xx ou pedido com a mesma chave ainda em
   * processamento, para que a repetição não duplique o efeito.
   */
  idempotency?: IdempotencyKeeper;
}

/**
 * Mutation com notificação de sucesso, invalidação por domínio, tratamento de
 * erros e, quando informada, a regra de descarte da chave de idempotência.
 */
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
      options.idempotency?.reset();
      if (options.successMessage) {
        notifySuccess(
          typeof options.successMessage === 'function' ? options.successMessage(data, vars) : options.successMessage,
        );
      }
      options.onSuccess?.(data, vars);
      await invalidateAll();
    },
    onError: (error) => {
      if (discardsIdempotencyKey(error)) options.idempotency?.reset();
      handleMutationError(error, {
        form: options.form,
        fieldMap: options.fieldMap,
        onConcurrentChange: () => {
          void invalidateAll();
          options.onConcurrentReload?.();
        },
      });
      options.onError?.(error);
    },
  });
}
