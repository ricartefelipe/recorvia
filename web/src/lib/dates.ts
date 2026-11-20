/**
 * Datas e instantes. Instantes (RFC 3339, UTC) são exibidos no fuso da
 * organização ativa. Datas civis (AAAA-MM-DD) são exibidas como estão, sem
 * conversão de fuso.
 */

const CIVIL_DATE = /^(\d{4})-(\d{2})-(\d{2})$/;
const dtfCache = new Map<string, Intl.DateTimeFormat>();

function dtf(timeZone: string, options: Intl.DateTimeFormatOptions, locale = 'pt-BR'): Intl.DateTimeFormat {
  const key = `${locale}|${timeZone}|${JSON.stringify(options)}`;
  let f = dtfCache.get(key);
  if (!f) {
    f = new Intl.DateTimeFormat(locale, { ...options, timeZone });
    dtfCache.set(key, f);
  }
  return f;
}

function toDate(value: string | Date): Date | null {
  const d = value instanceof Date ? value : new Date(value);
  return Number.isNaN(d.getTime()) ? null : d;
}

/** Instante com data e hora no fuso informado: "15/11/2026, 23:30". */
export function formatDateTime(value: string | null | undefined, timeZone: string): string {
  if (!value) return '—';
  const d = toDate(value);
  if (!d) return value;
  return dtf(timeZone, { dateStyle: 'short', timeStyle: 'short' }).format(d);
}

/** Somente a data de um instante no fuso informado: "15/11/2026". */
export function formatInstantDate(value: string | null | undefined, timeZone: string): string {
  if (!value) return '—';
  const d = toDate(value);
  if (!d) return value;
  return dtf(timeZone, { dateStyle: 'short' }).format(d);
}

/** Data civil "2026-11-16" → "16/11/2026", sem conversão de fuso. */
export function formatCivilDate(value: string | null | undefined): string {
  if (!value) return '—';
  const m = CIVIL_DATE.exec(value);
  if (!m) return value;
  return `${m[3]}/${m[2]}/${m[1]}`;
}

/** Mês "2026-11" → "novembro de 2026". */
export function formatMonth(value: string): string {
  const m = /^(\d{4})-(\d{2})$/.exec(value);
  if (!m) return value;
  const d = new Date(Date.UTC(Number(m[1]), Number(m[2]) - 1, 1, 12));
  return dtf('UTC', { month: 'long', year: 'numeric' }).format(d);
}

interface ZonedParts {
  year: number;
  month: number;
  day: number;
  hour: number;
  minute: number;
  second: number;
}

function zonedParts(date: Date, timeZone: string): ZonedParts {
  const parts = dtf(
    timeZone,
    {
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hourCycle: 'h23',
    },
    'en-US',
  ).formatToParts(date);
  const get = (type: Intl.DateTimeFormatPartTypes) => Number(parts.find((p) => p.type === type)?.value ?? '0');
  return {
    year: get('year'),
    month: get('month'),
    day: get('day'),
    hour: get('hour') % 24,
    minute: get('minute'),
    second: get('second'),
  };
}

const pad = (n: number, size = 2) => String(n).padStart(size, '0');

/** Data civil de hoje no fuso informado (AAAA-MM-DD). */
export function todayInTimeZone(timeZone: string, now: Date = new Date()): string {
  const p = zonedParts(now, timeZone);
  return `${p.year}-${pad(p.month)}-${pad(p.day)}`;
}

/** Mês corrente no fuso informado (AAAA-MM). */
export function currentMonthInTimeZone(timeZone: string, now: Date = new Date()): string {
  return todayInTimeZone(timeZone, now).slice(0, 7);
}

/** Soma dias a uma data civil (AAAA-MM-DD). */
export function addDaysToCivilDate(value: string, days: number): string {
  const m = CIVIL_DATE.exec(value);
  if (!m) throw new RangeError(`Data inválida: ${value}`);
  const d = new Date(Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3]) + days));
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}`;
}

/**
 * Converte um horário de parede ("2026-11-16 09:30:00") no fuso informado
 * para um instante RFC 3339 em UTC.
 */
export function zonedWallTimeToUtc(wallTime: string, timeZone: string): string {
  const m = /^(\d{4})-(\d{2})-(\d{2})(?:[ T](\d{2}):(\d{2})(?::(\d{2}))?)?$/.exec(wallTime.trim());
  if (!m) throw new RangeError(`Data e hora inválidas: ${wallTime}`);
  const [y, mo, d, h = '0', mi = '0', s = '0'] = m.slice(1).map((x) => x ?? '0');
  const target = Date.UTC(Number(y), Number(mo) - 1, Number(d), Number(h), Number(mi), Number(s));
  // Ajuste iterativo do deslocamento do fuso (resolve horário de verão).
  let guess = target;
  for (let i = 0; i < 3; i++) {
    const p = zonedParts(new Date(guess), timeZone);
    const asUtc = Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute, p.second);
    const diff = asUtc - target;
    if (diff === 0) break;
    guess -= diff;
  }
  return new Date(guess).toISOString().replace('.000Z', 'Z');
}

/** Lista de fusos IANA suportados pelo navegador (com fallback). */
export function supportedTimeZones(): string[] {
  try {
    return Intl.supportedValuesOf('timeZone');
  } catch {
    return ['America/Sao_Paulo', 'America/Manaus', 'America/Fortaleza', 'America/Recife', 'UTC'];
  }
}
