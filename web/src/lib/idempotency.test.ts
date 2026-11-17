import { describe, expect, it } from 'vitest';
import { createIdempotencyKeeper, stableStringify } from './idempotency';

describe('chave de idempotência por intenção', () => {
  it('reutiliza a chave quando o mesmo envio é repetido (erro de rede)', () => {
    let n = 0;
    const keeper = createIdempotencyKeeper(() => `chave-${++n}`);
    const first = keeper.keyFor({ amount: 1000, reason: 'Ajuste' });
    const retry = keeper.keyFor({ reason: 'Ajuste', amount: 1000 });
    expect(retry).toBe(first);
  });

  it('gera nova chave quando os dados mudam ou após sucesso', () => {
    let n = 0;
    const keeper = createIdempotencyKeeper(() => `chave-${++n}`);
    const a = keeper.keyFor({ amount: 1000 });
    const b = keeper.keyFor({ amount: 2000 });
    expect(b).not.toBe(a);
    keeper.reset();
    expect(keeper.keyFor({ amount: 2000 })).not.toBe(b);
  });

  it('serializa de forma determinística', () => {
    expect(stableStringify({ b: 1, a: [1, { d: 2, c: 3 }], z: undefined })).toBe('{"a":[1,{"c":3,"d":2}],"b":1}');
  });
});
