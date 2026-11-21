import { describe, expect, it, vi } from 'vitest';
import { byName, fetchAllPages, MAX_PAGE_SIZE } from './pages';

describe('fetchAllPages', () => {
  it('segue o next_cursor até a última página, com o maior limite da API', async () => {
    const fetchPage = vi.fn(async ({ cursor }: { limit?: number; cursor?: string }) => {
      if (!cursor) return { data: [1, 2], next_cursor: 'c1' };
      if (cursor === 'c1') return { data: [3], next_cursor: 'c2' };
      return { data: [4], next_cursor: null };
    });
    await expect(fetchAllPages(fetchPage)).resolves.toEqual([1, 2, 3, 4]);
    expect(fetchPage.mock.calls.map(([page]) => page)).toEqual([
      { limit: MAX_PAGE_SIZE, cursor: undefined },
      { limit: MAX_PAGE_SIZE, cursor: 'c1' },
      { limit: MAX_PAGE_SIZE, cursor: 'c2' },
    ]);
  });

  it('para numa resposta sem next_cursor e num cursor repetido', async () => {
    await expect(fetchAllPages(async () => ({ data: ['a'] }))).resolves.toEqual(['a']);
    const repeating = vi.fn(async () => ({ data: ['b'], next_cursor: 'mesmo' }));
    await expect(fetchAllPages(repeating)).resolves.toEqual(['b', 'b']);
    expect(repeating).toHaveBeenCalledTimes(2);
  });

  it('propaga a falha de qualquer página', async () => {
    const fetchPage = vi.fn(async ({ cursor }: { cursor?: string }) => {
      if (cursor) throw new Error('indisponível');
      return { data: [1], next_cursor: 'c1' };
    });
    await expect(fetchAllPages(fetchPage)).rejects.toThrow('indisponível');
  });
});

describe('byName', () => {
  it('ordena pelo nome sem diferenciar maiúsculas e acentos, sem alterar a lista recebida', () => {
    const items = [{ name: 'Ética' }, { name: 'beta' }, { name: 'Alfa' }];
    expect(byName(items).map((i) => i.name)).toEqual(['Alfa', 'beta', 'Ética']);
    expect(items[0].name).toBe('Ética');
  });
});
