/**
 * Dinheiro no frontend: valores trafegam em unidades mínimas inteiras
 * (centavos) e nunca passam por ponto flutuante. A formatação usa o
 * Intl.NumberFormat com strings decimais exatas.
 */

export type MinorUnits = number | bigint | string;

const DEFAULT_CURRENCY = 'BRL';
const formatterCache = new Map<string, Intl.NumberFormat>();

function currencyFormatter(currency: string, maxFractionDigits?: number): Intl.NumberFormat {
  const key = `${currency}:${maxFractionDigits ?? ''}`;
  let nf = formatterCache.get(key);
  if (!nf) {
    nf =
      maxFractionDigits === undefined
        ? new Intl.NumberFormat('pt-BR', { style: 'currency', currency })
        : new Intl.NumberFormat('pt-BR', {
            style: 'currency',
            currency,
            maximumFractionDigits: maxFractionDigits,
          });
    formatterCache.set(key, nf);
  }
  return nf;
}

/** Casas decimais da moeda (BRL = 2, JPY = 0). */
export function currencyDigits(currency: string = DEFAULT_CURRENCY): number {
  return currencyFormatter(currency).resolvedOptions().maximumFractionDigits ?? 2;
}

/** Converte unidades mínimas para BigInt, rejeitando valores não inteiros. */
export function toBigInt(value: MinorUnits): bigint {
  if (typeof value === 'bigint') return value;
  if (typeof value === 'number') {
    if (!Number.isSafeInteger(value)) {
      throw new RangeError(`Valor monetário fora do intervalo seguro: ${value}`);
    }
    return BigInt(value);
  }
  const trimmed = value.trim();
  if (!/^-?\d+$/.test(trimmed)) {
    throw new RangeError(`Valor monetário inválido: ${value}`);
  }
  return BigInt(trimmed);
}

/** Desloca a vírgula decimal de um inteiro (em texto) `digits` casas à esquerda. */
function shiftLeft(integer: bigint, digits: number): string {
  const negative = integer < 0n;
  const abs = (negative ? -integer : integer).toString();
  if (digits === 0) return (negative ? '-' : '') + abs;
  const padded = abs.padStart(digits + 1, '0');
  const intPart = padded.slice(0, padded.length - digits);
  const fracPart = padded.slice(padded.length - digits);
  return `${negative ? '-' : ''}${intPart}.${fracPart}`;
}

/** Formata centavos como moeda pt-BR, por exemplo 123456 → "R$ 1.234,56". */
export function formatMoney(value: MinorUnits | null | undefined, currency: string = DEFAULT_CURRENCY): string {
  if (value === null || value === undefined || value === '') return '—';
  const digits = currencyDigits(currency);
  const decimal = shiftLeft(toBigInt(value), digits);
  return currencyFormatter(currency).format(decimal as Intl.StringNumericLiteral);
}

/**
 * Formata um preço unitário em centavos com casas fracionárias
 * (`unit_amount_decimal`, até 10 casas), por exemplo "0.15" → "R$ 0,0015".
 */
export function formatUnitAmountDecimal(value: string | null | undefined, currency: string = DEFAULT_CURRENCY): string {
  if (value === null || value === undefined || value.trim() === '') return '—';
  const normalized = normalizeDecimalText(value);
  if (normalized === null) return value;
  const digits = currencyDigits(currency);
  const reais = shiftDecimalString(normalized, -digits);
  return currencyFormatter(currency, 12).format(reais as Intl.StringNumericLiteral);
}

/** Aceita "123", "-1.5" (ponto como separador decimal). Retorna null se inválido. */
function normalizeDecimalText(value: string): string | null {
  const trimmed = value.trim();
  return /^-?\d+(\.\d+)?$/.test(trimmed) ? trimmed : null;
}

/**
 * Desloca a vírgula de um decimal em texto: `places` positivo multiplica por
 * 10^places e negativo divide. Resultado sem zeros supérfluos.
 */
export function shiftDecimalString(value: string, places: number): string {
  const match = /^(-?)(\d+)(?:\.(\d+))?$/.exec(value.trim());
  if (!match) throw new RangeError(`Decimal inválido: ${value}`);
  const [, sign, intDigits, fracDigits = ''] = match;
  let digits = intDigits + fracDigits;
  let pointIndex = intDigits.length + places;
  if (pointIndex < 0) {
    digits = '0'.repeat(-pointIndex) + digits;
    pointIndex = 0;
  }
  if (pointIndex > digits.length) {
    digits = digits + '0'.repeat(pointIndex - digits.length);
  }
  const intPart = digits.slice(0, pointIndex).replace(/^0+(?=\d)/, '') || '0';
  const fracPart = digits.slice(pointIndex).replace(/0+$/, '');
  if (intPart === '0' && fracPart === '') return '0';
  return `${sign}${fracPart ? `${intPart}.${fracPart}` : intPart}`;
}

export type ParseResult<T> = { ok: true; value: T } | { ok: false; error: string };

export interface ParseMoneyOptions {
  allowNegative?: boolean;
  /** Casas decimais aceitas (padrão: casas da moeda). */
  fractionDigits?: number;
}

/**
 * Lê texto digitado no padrão brasileiro ("1.234,56", "1234,5", "R$ 10")
 * e devolve o valor em unidades mínimas como BigInt.
 */
export function parseMoney(text: string, options: ParseMoneyOptions = {}): ParseResult<bigint> {
  const fractionDigits = options.fractionDigits ?? 2;
  const parsed = parseBrazilianDecimal(text, fractionDigits);
  if (!parsed.ok) return parsed;
  const { negative, intPart, fracPart } = parsed.value;
  if (negative && !options.allowNegative) {
    return { ok: false, error: 'Informe um valor positivo.' };
  }
  const cents = BigInt(intPart + fracPart.padEnd(fractionDigits, '0'));
  return { ok: true, value: negative ? -cents : cents };
}

interface DecimalParts {
  negative: boolean;
  intPart: string;
  fracPart: string;
}

function parseBrazilianDecimal(text: string, maxFractionDigits: number): ParseResult<DecimalParts> {
  let s = text.replace(/\s/g, '').replace(/^R\$/i, '');
  if (s === '') return { ok: false, error: 'Informe um valor.' };
  let negative = false;
  if (s.startsWith('-')) {
    negative = true;
    s = s.slice(1).replace(/^R\$/i, '');
  }
  if (s.includes('-') || s === '') return { ok: false, error: 'Valor inválido.' };
  const pieces = s.split(',');
  if (pieces.length > 2) return { ok: false, error: 'Use apenas uma vírgula para os centavos.' };
  const [rawInt, fracPart = ''] = pieces;
  if (pieces.length === 2 && fracPart === '') return { ok: false, error: 'Informe os centavos após a vírgula.' };
  if (!/^\d*$/.test(fracPart)) return { ok: false, error: 'Valor inválido.' };
  if (fracPart.length > maxFractionDigits) {
    return { ok: false, error: `Use no máximo ${maxFractionDigits} casas decimais.` };
  }
  let intPart: string;
  if (rawInt.includes('.')) {
    if (!/^\d{1,3}(\.\d{3})+$/.test(rawInt)) {
      return { ok: false, error: 'Separador de milhar inválido. Exemplo: 1.234,56' };
    }
    intPart = rawInt.replace(/\./g, '');
  } else {
    if (!/^\d*$/.test(rawInt)) return { ok: false, error: 'Valor inválido.' };
    intPart = rawInt;
  }
  if (intPart === '' && fracPart === '') return { ok: false, error: 'Informe um valor.' };
  intPart = intPart.replace(/^0+(?=\d)/, '') || '0';
  return { ok: true, value: { negative, intPart, fracPart } };
}

/** Converte BigInt para number apenas quando cabe com segurança no JSON. */
export function centsToSafeNumber(cents: bigint): ParseResult<number> {
  if (cents > BigInt(Number.MAX_SAFE_INTEGER) || cents < BigInt(Number.MIN_SAFE_INTEGER)) {
    return { ok: false, error: 'Valor acima do limite suportado.' };
  }
  return { ok: true, value: Number(cents) };
}

/** Lê o texto e retorna centavos como number (para o corpo JSON) ou erro. */
export function parseMoneyToNumber(text: string, options: ParseMoneyOptions = {}): ParseResult<number> {
  const parsed = parseMoney(text, options);
  if (!parsed.ok) return parsed;
  return centsToSafeNumber(parsed.value);
}

/** Texto editável para um valor em centavos, por exemplo 123456 → "1.234,56". */
export function centsToInput(value: MinorUnits, currency: string = DEFAULT_CURRENCY): string {
  const digits = currencyDigits(currency);
  const decimal = shiftLeft(toBigInt(value), digits);
  return new Intl.NumberFormat('pt-BR', {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  }).format(decimal as Intl.StringNumericLiteral);
}

/**
 * Converte um preço unitário digitado em reais com até 12 casas
 * ("0,0015") para `unit_amount_decimal` em centavos ("0.15").
 */
export function parseUnitAmountToCentsDecimal(text: string): ParseResult<string> {
  const parsed = parseBrazilianDecimal(text, 12);
  if (!parsed.ok) return parsed;
  if (parsed.value.negative) return { ok: false, error: 'Informe um valor positivo.' };
  const { intPart, fracPart } = parsed.value;
  const reais = fracPart ? `${intPart}.${fracPart}` : intPart;
  return { ok: true, value: shiftDecimalString(reais, 2) };
}

/** Inverso de parseUnitAmountToCentsDecimal: "0.15" → "0,0015". */
export function centsDecimalToInput(value: string): string {
  const reais = shiftDecimalString(value, -2);
  return reais.replace('.', ',');
}

/** Pontos-base para percentual pt-BR com duas casas: 450 → "4,50%". */
export function formatBasisPoints(bp: number | null | undefined): string {
  if (bp === null || bp === undefined) return '—';
  const decimal = shiftLeft(BigInt(Math.trunc(bp)), 2);
  return (
    new Intl.NumberFormat('pt-BR', { minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(
      decimal as Intl.StringNumericLiteral,
    ) + '%'
  );
}

/** Percentual digitado ("4,5") para pontos-base (450). */
export function parsePercentToBasisPoints(text: string, options: { allowNegative?: boolean } = {}): ParseResult<number> {
  const parsed = parseMoney(text.replace('%', ''), { allowNegative: options.allowNegative, fractionDigits: 2 });
  if (!parsed.ok) return parsed;
  return centsToSafeNumber(parsed.value);
}

/**
 * Quantidade decimal digitada ("1500", "2,5" ou "2.5") para o texto decimal
 * esperado pela API ("2.5"). Não aceita separador de milhar.
 */
export function parseQuantity(text: string, options: { allowZero?: boolean } = {}): ParseResult<string> {
  const s = text.trim();
  if (!/^\d+([.,]\d+)?$/.test(s)) {
    return { ok: false, error: 'Use apenas números, com vírgula para decimais (ex.: 2,5).' };
  }
  const normalized = shiftDecimalString(s.replace(',', '.'), 0);
  if (!options.allowZero && normalized === '0') return { ok: false, error: 'Informe um valor maior que zero.' };
  return { ok: true, value: normalized };
}

/** Exibe uma quantidade decimal da API ("1234.5") como "1.234,5". */
export function formatQuantity(value: string | number | null | undefined): string {
  if (value === null || value === undefined || value === '') return '—';
  const text = String(value);
  if (!/^-?\d+(\.\d+)?$/.test(text)) return text;
  return new Intl.NumberFormat('pt-BR', { maximumFractionDigits: 10 }).format(text as Intl.StringNumericLiteral);
}
