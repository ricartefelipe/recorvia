/**
 * CPF e CNPJ: validação apenas de formato no navegador. Os dígitos
 * verificadores são conferidos pelo backend, que devolve o erro no campo.
 * O CNPJ aceita o formato alfanumérico (12 caracteres alfanuméricos + 2 dígitos).
 */

export type DocumentKind = 'cpf' | 'cnpj';

/** Remove pontuação usual e converte para maiúsculas. */
export function normalizeDocument(value: string): string {
  return value.toUpperCase().replace(/[.\-/\s]/g, '');
}

export function isCpfFormat(value: string): boolean {
  return /^\d{11}$/.test(normalizeDocument(value));
}

export function isCnpjFormat(value: string): boolean {
  return /^[0-9A-Z]{12}\d{2}$/.test(normalizeDocument(value));
}

export function documentKindForCustomer(kind: 'individual' | 'company'): DocumentKind {
  return kind === 'individual' ? 'cpf' : 'cnpj';
}

/** Formata para exibição: 000.000.000-00 ou AA.AAA.AAA/AAAA-00. */
export function formatDocument(value: string | null | undefined, kind?: DocumentKind): string {
  if (!value) return '—';
  const n = normalizeDocument(value);
  const k = kind ?? (n.length === 11 ? 'cpf' : 'cnpj');
  if (k === 'cpf' && /^\d{11}$/.test(n)) {
    return `${n.slice(0, 3)}.${n.slice(3, 6)}.${n.slice(6, 9)}-${n.slice(9)}`;
  }
  if (k === 'cnpj' && n.length === 14) {
    return `${n.slice(0, 2)}.${n.slice(2, 5)}.${n.slice(5, 8)}/${n.slice(8, 12)}-${n.slice(12)}`;
  }
  return value;
}

/** CEP: aceita 00000-000 ou 00000000. */
export function isPostalCode(value: string): boolean {
  return /^\d{5}-?\d{3}$/.test(value.trim());
}

/** CEP para exibição: "80000000" → "80000-000"; outros formatos ficam como vieram. */
export function formatPostalCode(value: string | null | undefined): string {
  if (!value) return '';
  const digits = value.replace(/\D/g, '');
  return digits.length === 8 ? `${digits.slice(0, 5)}-${digits.slice(5)}` : value;
}
