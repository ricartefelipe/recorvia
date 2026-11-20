import { z } from 'zod';
import pt from 'zod/v4/locales/pt.js';
import { parseMoneyToNumber, parsePercentToBasisPoints, parseQuantity, parseUnitAmountToCentsDecimal } from './money';

/** Mensagens padrão do Zod em português. */
z.config(pt());

/** Texto opcional: espaços aparados e vazio vira undefined. */
export function optionalText(max: number, message?: string) {
  return z
    .string()
    .trim()
    .max(max, message ?? `Use no máximo ${max} caracteres.`)
    .transform((v) => (v === '' ? undefined : v));
}

export function requiredText(min: number, max: number, label = 'Campo') {
  return z
    .string()
    .trim()
    .min(min, min <= 1 ? `${label} é obrigatório.` : `${label} deve ter pelo menos ${min} caracteres.`)
    .max(max, `${label} deve ter no máximo ${max} caracteres.`);
}

export const optionalEmail = z
  .string()
  .trim()
  .max(320, 'Use no máximo 320 caracteres.')
  .refine((v) => v === '' || z.email().safeParse(v).success, 'Informe um e-mail válido.')
  .transform((v) => (v === '' ? undefined : v));

export const requiredEmail = z.string().trim().max(320, 'Use no máximo 320 caracteres.').pipe(z.email('Informe um e-mail válido.'));

/** Telefone opcional: dígitos com DDD, aceita +55, parênteses, espaços e hífen. */
export const optionalPhone = z
  .string()
  .trim()
  .refine((v) => v === '' || /^\+?[\d\s()-]{10,20}$/.test(v), 'Informe o telefone com DDD, por exemplo (11) 91234-5678.')
  .transform((v) => (v === '' ? undefined : v));

/** Valor em reais digitado ("1.234,56") convertido para centavos inteiros. */
export function moneyField(options: { min?: number; max?: number; minMessage?: string; maxMessage?: string } = {}) {
  const min = options.min ?? 1;
  return z.string().transform((value, ctx) => {
    const parsed = parseMoneyToNumber(value);
    if (!parsed.ok) {
      ctx.addIssue({ code: 'custom', message: parsed.error });
      return z.NEVER;
    }
    if (parsed.value < min) {
      ctx.addIssue({
        code: 'custom',
        message: options.minMessage ?? (min <= 1 ? 'Informe um valor maior que zero.' : 'Valor abaixo do mínimo.'),
      });
      return z.NEVER;
    }
    if (options.max !== undefined && parsed.value > options.max) {
      ctx.addIssue({ code: 'custom', message: options.maxMessage ?? 'Valor acima do permitido.' });
      return z.NEVER;
    }
    return parsed.value;
  });
}

/** Preço unitário em reais com até 12 casas → unit_amount_decimal em centavos. */
export const unitAmountField = z.string().transform((value, ctx) => {
  const parsed = parseUnitAmountToCentsDecimal(value);
  if (!parsed.ok) {
    ctx.addIssue({ code: 'custom', message: parsed.error });
    return z.NEVER;
  }
  if (parsed.value === '0') {
    ctx.addIssue({ code: 'custom', message: 'Informe um preço maior que zero.' });
    return z.NEVER;
  }
  const fraction = parsed.value.split('.')[1] ?? '';
  if (fraction.length > 10) {
    ctx.addIssue({ code: 'custom', message: 'Use no máximo 12 casas decimais em reais.' });
    return z.NEVER;
  }
  return parsed.value;
});

/** Quantidade decimal ("2,5") → texto decimal da API ("2.5"). */
export function quantityField(options: { allowZero?: boolean } = {}) {
  return z.string().transform((value, ctx) => {
    const parsed = parseQuantity(value, options);
    if (!parsed.ok) {
      ctx.addIssue({ code: 'custom', message: parsed.error });
      return z.NEVER;
    }
    return parsed.value;
  });
}

/** Percentual digitado ("4,5") → pontos-base (450). */
export function percentField(options: { min: number; max: number; allowNegative?: boolean }) {
  return z.string().transform((value, ctx) => {
    const parsed = parsePercentToBasisPoints(value, { allowNegative: options.allowNegative });
    if (!parsed.ok) {
      ctx.addIssue({ code: 'custom', message: parsed.error });
      return z.NEVER;
    }
    if (parsed.value < options.min || parsed.value > options.max) {
      ctx.addIssue({
        code: 'custom',
        message: `Informe um percentual entre ${options.min / 100}% e ${options.max / 100}%.`,
      });
      return z.NEVER;
    }
    return parsed.value;
  });
}

/** Inteiro opcional vindo de NumberInput ('' quando vazio). */
export function optionalInt(min: number, max: number) {
  return z
    .union([z.number(), z.literal('')])
    .refine((v) => v === '' || (Number.isInteger(v) && v >= min && v <= max), `Informe um número inteiro entre ${min} e ${max}.`)
    .transform((v) => (v === '' ? undefined : v));
}

export function requiredInt(min: number, max: number, message?: string) {
  return z
    .union([z.number(), z.literal('')])
    .refine((v) => v !== '' && Number.isInteger(v) && v >= min && v <= max, message ?? `Informe um número inteiro entre ${min} e ${max}.`)
    .transform((v) => v as number);
}

export const reasonField = requiredText(3, 500, 'O motivo');
