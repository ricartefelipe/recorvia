import { renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { useReturnFromEmptyPage } from './hooks';

describe('useReturnFromEmptyPage', () => {
  it('volta à página anterior quando uma página depois da primeira fica vazia', () => {
    const previous = vi.fn();
    renderHook(() => useReturnFromEmptyPage({ hasPrevious: true, previous }, []));
    expect(previous).toHaveBeenCalledTimes(1);
  });

  it('não sai da primeira página, da página com itens nem da página ainda carregando', () => {
    const previous = vi.fn();
    renderHook(() => useReturnFromEmptyPage({ hasPrevious: false, previous }, []));
    renderHook(() => useReturnFromEmptyPage({ hasPrevious: true, previous }, [1]));
    renderHook(() => useReturnFromEmptyPage({ hasPrevious: true, previous }, undefined));
    renderHook(() => useReturnFromEmptyPage({ hasPrevious: true, previous }, [], true));
    expect(previous).not.toHaveBeenCalled();
  });
});
