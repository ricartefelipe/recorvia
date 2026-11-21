import { describe, expect, it } from 'vitest';
import { canonicalJSON } from './json';

describe('canonicalJSON', () => {
  it('produz os mesmos bytes para o mesmo conteúdo inserido em outra ordem, em qualquer nível', () => {
    const a = { plan_id: 'p1', customer_id: 'c1', quantity: 1, terms: { days: 10, mode: 'net' }, items: [{ b: 2, a: 1 }] };
    const b = { items: [{ a: 1, b: 2 }], terms: { mode: 'net', days: 10 }, quantity: 1, customer_id: 'c1', plan_id: 'p1' };
    expect(canonicalJSON(a)).toBe(canonicalJSON(b));
    expect(canonicalJSON(a)).toBe('{"customer_id":"c1","items":[{"a":1,"b":2}],"plan_id":"p1","quantity":1,"terms":{"days":10,"mode":"net"}}');
  });

  it('segue JSON.stringify fora a ordem: toJSON, undefined omitido, null e listas preservadas', () => {
    const value = { z: undefined, when: new Date('2026-11-21T12:00:00Z'), none: null, list: [3, 1, 2] };
    expect(canonicalJSON(value)).toBe('{"list":[3,1,2],"none":null,"when":"2026-11-21T12:00:00.000Z"}');
    expect(JSON.parse(canonicalJSON(value))).toEqual(JSON.parse(JSON.stringify(value)));
    expect(canonicalJSON('texto')).toBe('"texto"');
  });
});
