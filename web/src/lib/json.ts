/**
 * JSON canônico: os campos de cada objeto saem em ordem fixa (a ordem das
 * chaves ordenadas), em qualquer nível, independentemente da ordem em que
 * foram inseridos. O corpo dos pedidos usa esta serialização para que a
 * mesma Idempotency-Key acompanhe sempre os mesmos bytes: a API compara a
 * impressão do pedido pelos bytes do corpo (ADR-075). Fora a ordem, o
 * resultado é o de JSON.stringify (toJSON, campos undefined omitidos).
 */
export function canonicalJSON(value: unknown): string {
  return JSON.stringify(value, (_key, v: unknown) => {
    if (v !== null && typeof v === 'object' && !Array.isArray(v)) {
      const obj = v as Record<string, unknown>;
      return Object.fromEntries(
        Object.keys(obj)
          .sort(byCodeUnit)
          .map((k) => [k, obj[k]]),
      );
    }
    return v;
  });
}

/**
 * Compara por unidade de código UTF-16, como o servidor ordena bytes: a ordem
 * não pode depender do idioma do navegador (localeCompare), ou a mesma chave
 * acompanharia bytes diferentes em máquinas diferentes.
 */
function byCodeUnit(a: string, b: string): number {
  if (a < b) return -1;
  return a > b ? 1 : 0;
}
