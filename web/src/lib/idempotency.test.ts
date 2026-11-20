import { describe, expect, it } from 'vitest';
import { ApiError, NetworkError } from '../api/http';
import { createIdempotencyKeeper, discardsIdempotencyKey, stableStringify } from './idempotency';

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

describe('descarte da chave depois de erro', () => {
  it.each([
    ['conflito registrado na chave', new ApiError({ status: 409, code: 'reconciliation_running' }), true],
    ['provedor não configurado', new ApiError({ status: 409, code: 'provider_not_configured' }), true],
    ['dados inválidos', new ApiError({ status: 422, code: 'invalid_refund' }), true],
    ['mesma chave em processamento', new ApiError({ status: 409, code: 'idempotency_in_progress' }), false],
    ['erro no servidor', new ApiError({ status: 503, code: 'provider_unavailable' }), false],
    ['erro de rede', new NetworkError(), false],
    ['erro desconhecido', new Error('x'), false],
  ])('%s', (_name, error, discard) => {
    expect(discardsIdempotencyKey(error)).toBe(discard);
  });
});
