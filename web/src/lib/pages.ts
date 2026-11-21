import type { CursorPage } from '../api/types';

/** Página pedida a uma listagem paginada por cursor. */
export interface PageRequest {
  limit?: number;
  cursor?: string;
}

/** Maior página aceita pela API (`limit` de 1 a 100). */
export const MAX_PAGE_SIZE = 100;

/**
 * Percorre todas as páginas de uma listagem pelo `next_cursor`, com o maior
 * limite da API, e devolve os itens na ordem da listagem. Os campos de seleção
 * (plano da assinatura, métrica do preço, produto do plano, planos do cupom)
 * precisam de todos os itens, e não só da primeira página (ADR-085). Um
 * cursor repetido encerra a leitura, em vez de repetir páginas sem fim.
 */
export async function fetchAllPages<T>(fetchPage: (page: PageRequest) => Promise<CursorPage<T>>): Promise<T[]> {
  const items: T[] = [];
  const seen = new Set<string>();
  let cursor: string | undefined;
  for (;;) {
    const page = await fetchPage({ limit: MAX_PAGE_SIZE, cursor });
    items.push(...page.data);
    const next = page.next_cursor ?? undefined;
    if (!next || seen.has(next)) return items;
    seen.add(next);
    cursor = next;
  }
}

/** Ordena pelo nome, como o usuário procura nos campos de seleção. */
export function byName<T extends { name: string }>(items: T[]): T[] {
  return [...items].sort((a, b) => a.name.localeCompare(b.name, 'pt-BR', { sensitivity: 'base' }));
}
