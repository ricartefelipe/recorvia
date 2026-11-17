import { useState } from 'react';

/** Serialização determinística (chaves ordenadas) para comparar intenções. */
export function stableStringify(value: unknown): string {
  if (value === undefined) return 'undefined';
  if (value === null || typeof value !== 'object') {
    return typeof value === 'bigint' ? `${value}n` : JSON.stringify(value);
  }
  if (Array.isArray(value)) return `[${value.map(stableStringify).join(',')}]`;
  const entries = Object.entries(value as Record<string, unknown>)
    .filter(([, v]) => v !== undefined)
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  return `{${entries.map(([k, v]) => `${JSON.stringify(k)}:${stableStringify(v)}`).join(',')}}`;
}

export interface IdempotencyKeeper {
  /** Chave para o envio: reaproveitada enquanto os dados forem os mesmos. */
  keyFor(payload: unknown): string;
  /** Descarta a chave após sucesso; o próximo envio é uma nova intenção. */
  reset(): void;
}

export function createIdempotencyKeeper(generate: () => string = () => crypto.randomUUID()): IdempotencyKeeper {
  let current: { key: string; fingerprint: string } | null = null;
  return {
    keyFor(payload) {
      const fingerprint = stableStringify(payload);
      if (!current || current.fingerprint !== fingerprint) {
        current = { key: generate(), fingerprint };
      }
      return current.key;
    },
    reset() {
      current = null;
    },
  };
}

/**
 * Uma chave de idempotência por intenção de envio: repetir o mesmo envio após
 * erro de rede reutiliza a chave; alterar os dados gera outra.
 */
export function useIdempotencyKey(): IdempotencyKeeper {
  const [keeper] = useState(() => createIdempotencyKeeper());
  return keeper;
}
