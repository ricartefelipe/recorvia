import type { FieldPath, FieldValues, UseFormGetValues, UseFormSetError } from 'react-hook-form';
import { ApiError, NetworkError, type FieldError } from '../api/http';
import { notifyError } from './notify';

export interface ErrorDescription {
  title: string;
  message: string;
  requestId?: string;
}

/** Texto amigável para qualquer erro de chamada. */
export function describeError(error: unknown): ErrorDescription {
  if (error instanceof NetworkError) {
    return { title: 'Sem conexão', message: error.message };
  }
  if (error instanceof ApiError) {
    const base = error.detail || error.title;
    switch (error.status) {
      case 401:
        return { title: 'Sessão expirada', message: 'Redirecionando para a entrada…' };
      case 403:
        return { title: 'Acesso negado', message: base || 'Você não tem permissão para esta ação.' };
      case 404:
        return { title: 'Não encontrado', message: base || 'O registro não existe ou não pertence a esta organização.' };
      case 409:
        return { title: 'Conflito', message: base || 'A operação não é permitida no estado atual do registro.' };
      case 412:
        return {
          title: 'Alteração concorrente',
          message: 'Este registro foi alterado por outra pessoa. Recarregue os dados antes de tentar novamente.',
        };
      case 422:
        return { title: 'Dados inválidos', message: base || 'Revise os campos destacados.' };
      case 429:
        return { title: 'Muitas requisições', message: base || 'Aguarde alguns instantes e tente novamente.' };
      default:
        if (error.status >= 500) {
          return {
            title: 'Erro no servidor',
            message: base || 'Ocorreu um erro inesperado. Tente novamente em instantes.',
            requestId: error.requestId,
          };
        }
        return { title: 'Não foi possível concluir', message: base || error.message };
    }
  }
  return {
    title: 'Erro inesperado',
    message: error instanceof Error ? error.message : 'Tente novamente.',
  };
}

/** Converte o nome de campo do backend para o caminho do formulário. */
export function normalizeFieldPath(field: string): string {
  return field
    .replace(/^\$\.?/, '')
    .replace(/^\//, '')
    .replace(/\//g, '.')
    .replace(/\[(\d+)\]/g, '.$1');
}

function hasPath(values: unknown, path: string): boolean {
  let current: unknown = values;
  for (const part of path.split('.')) {
    if (current === null || typeof current !== 'object' || !Object.prototype.hasOwnProperty.call(current, part)) {
      return false;
    }
    current = (current as Record<string, unknown>)[part];
  }
  return true;
}

export interface FormErrorTarget<T extends FieldValues> {
  setError: UseFormSetError<T>;
  getValues: UseFormGetValues<T>;
}

/**
 * Aplica errors[] do problem+json nos campos do formulário. Campos que não
 * existem no formulário são devolvidos para exibição na notificação.
 */
export function applyFieldErrors<T extends FieldValues>(
  errors: FieldError[],
  form: FormErrorTarget<T>,
  fieldMap: Record<string, string> = {},
): FieldError[] {
  const values = form.getValues();
  const unmatched: FieldError[] = [];
  let focused = false;
  for (const e of errors) {
    const normalized = normalizeFieldPath(e.field);
    const path = fieldMap[normalized] ?? normalized;
    if (path && hasPath(values, path)) {
      form.setError(path as FieldPath<T>, { type: 'server', message: e.message }, { shouldFocus: !focused });
      focused = true;
    } else {
      unmatched.push(e);
    }
  }
  return unmatched;
}

export interface MutationErrorOptions<T extends FieldValues> {
  form?: FormErrorTarget<T>;
  fieldMap?: Record<string, string>;
  /** Recarrega os dados após 412 (alteração concorrente). */
  onConcurrentChange?: () => void;
}

type ConcurrencyHandler = (onReload: () => void) => void;
let concurrencyHandler: ConcurrencyHandler | undefined;

/** Registrado pela aplicação para abrir o aviso de alteração concorrente. */
export function setConcurrencyHandler(handler: ConcurrencyHandler | undefined): void {
  concurrencyHandler = handler;
}

/** Tratamento padrão de erro em ações (mutations). */
export function handleMutationError<T extends FieldValues>(error: unknown, options: MutationErrorOptions<T> = {}): void {
  if (error instanceof ApiError) {
    if (error.status === 401) return; // o cliente já redireciona para a entrada
    if (error.status === 409 && error.code === 'tenant_mismatch') return; // tratado globalmente
    if (error.status === 412) {
      const reload = options.onConcurrentChange ?? (() => undefined);
      if (concurrencyHandler) concurrencyHandler(reload);
      else notifyError(describeError(error).message, { title: 'Alteração concorrente' });
      return;
    }
    let unmatched = error.fieldErrors;
    if (options.form && error.fieldErrors.length > 0) {
      unmatched = applyFieldErrors(error.fieldErrors, options.form, options.fieldMap);
    }
    const description = describeError(error);
    const extra = unmatched.map((e) => `${e.field}: ${e.message}`).join('; ');
    notifyError(extra ? `${description.message} ${extra}` : description.message, {
      title: description.title,
      requestId: error.isServerError ? error.requestId : undefined,
    });
    return;
  }
  const description = describeError(error);
  notifyError(description.message, { title: description.title });
}
