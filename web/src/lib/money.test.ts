import { describe, expect, it } from 'vitest';
import {
  centsDecimalToInput,
  centsToInput,
  formatBasisPoints,
  formatMoney,
  formatQuantity,
  formatUnitAmountDecimal,
  parseMoney,
  parseMoneyToNumber,
  parsePercentToBasisPoints,
  parseQuantity,
  parseUnitAmountToCentsDecimal,
  shiftDecimalString,
} from './money';

const NBSP = '\u00a0';

describe('formatMoney', () => {
  it('formata centavos em reais no padrão pt-BR', () => {
    expect(formatMoney(123456)).toBe(`R$${NBSP}1.234,56`);
    expect(formatMoney(1)).toBe(`R$${NBSP}0,01`);
    expect(formatMoney(0)).toBe(`R$${NBSP}0,00`);
  });

  it('formata valores negativos', () => {
    expect(formatMoney(-123456)).toBe(`-R$${NBSP}1.234,56`);
    expect(formatMoney(-5)).toBe(`-R$${NBSP}0,05`);
  });

  it('formata valores grandes sem perder precisão (BigInt e texto)', () => {
    expect(formatMoney(9007199254740993n)).toBe(`R$${NBSP}90.071.992.547.409,93`);
    expect(formatMoney('-922337203685477580')).toBe(`-R$${NBSP}9.223.372.036.854.775,80`);
    expect(formatMoney(Number.MAX_SAFE_INTEGER)).toBe(`R$${NBSP}90.071.992.547.409,91`);
  });

  it('respeita a moeda e suas casas decimais', () => {
    expect(formatMoney(1500, 'USD')).toBe(`US$${NBSP}15,00`);
    expect(formatMoney(1500, 'JPY')).toBe(`JP¥${NBSP}1.500`);
  });

  it('rejeita números não inteiros ou fora do intervalo seguro', () => {
    expect(() => formatMoney(10.5)).toThrow(RangeError);
    expect(() => formatMoney(2 ** 60)).toThrow(RangeError);
    expect(formatMoney(null)).toBe('—');
  });
});

describe('parseMoney', () => {
  it.each([
    ['1.234,56', 123456n],
    ['1234,56', 123456n],
    ['1234,5', 123450n],
    ['1234', 123400n],
    ['0,01', 1n],
    ['R$ 1.234,56', 123456n],
    [`R$${NBSP}10,00`, 1000n],
    ['  7  ', 700n],
    [',99', 99n],
    ['1.000.000,00', 100000000n],
  ])('converte "%s" para %s centavos', (text, expected) => {
    expect(parseMoney(text)).toEqual({ ok: true, value: expected });
  });

  it('converte valores muito grandes com BigInt', () => {
    expect(parseMoney('92.233.720.368.547.758,07')).toEqual({ ok: true, value: 9223372036854775807n });
    expect(parseMoneyToNumber('92.233.720.368.547.758,07')).toEqual({
      ok: false,
      error: 'Valor acima do limite suportado.',
    });
    expect(parseMoneyToNumber('90.071.992.547.409,91')).toEqual({ ok: true, value: Number.MAX_SAFE_INTEGER });
  });

  it('aceita negativos somente quando permitido', () => {
    expect(parseMoney('-1.234,56')).toEqual({ ok: false, error: 'Informe um valor positivo.' });
    expect(parseMoney('-1.234,56', { allowNegative: true })).toEqual({ ok: true, value: -123456n });
    expect(parseMoney('-R$ 0,50', { allowNegative: true })).toEqual({ ok: true, value: -50n });
  });

  it.each(['', 'abc', '1,234.56', '12.34', '1,2,3', '1,234', '1.23,00', '10,', '1-2', '- -1'])(
    'rejeita "%s"',
    (text) => {
      expect(parseMoney(text).ok).toBe(false);
    },
  );

  it('nunca usa ponto flutuante: 0,10 + 0,20 resulta em 30 centavos exatos', () => {
    const a = parseMoney('0,10');
    const b = parseMoney('0,20');
    expect(a.ok && b.ok && a.value + b.value).toBe(30n);
  });
});

describe('preço unitário fracionário', () => {
  it('converte reais com até 12 casas para centavos decimais', () => {
    expect(parseUnitAmountToCentsDecimal('0,0015')).toEqual({ ok: true, value: '0.15' });
    expect(parseUnitAmountToCentsDecimal('49,90')).toEqual({ ok: true, value: '4990' });
    expect(parseUnitAmountToCentsDecimal('1.000,123456789012')).toEqual({ ok: true, value: '100012.3456789012' });
    expect(parseUnitAmountToCentsDecimal('0,0000000000001').ok).toBe(false);
  });

  it('formata unit_amount_decimal em reais', () => {
    expect(formatUnitAmountDecimal('0.15')).toBe(`R$${NBSP}0,0015`);
    expect(formatUnitAmountDecimal('4990')).toBe(`R$${NBSP}49,90`);
    expect(centsDecimalToInput('0.15')).toBe('0,0015');
    expect(centsDecimalToInput('4990')).toBe('49,9');
  });

  it('desloca casas decimais em texto', () => {
    expect(shiftDecimalString('1.5', 2)).toBe('150');
    expect(shiftDecimalString('0.0015', 2)).toBe('0.15');
    expect(shiftDecimalString('15', -3)).toBe('0.015');
    expect(shiftDecimalString('-0.00', 2)).toBe('0');
  });
});

describe('outros formatos', () => {
  it('converte pontos-base para percentual', () => {
    expect(formatBasisPoints(450)).toBe('4,50%');
    expect(formatBasisPoints(10000)).toBe('100,00%');
    expect(formatBasisPoints(-25)).toBe('-0,25%');
    expect(formatBasisPoints(0)).toBe('0,00%');
  });

  it('lê percentual em pontos-base', () => {
    expect(parsePercentToBasisPoints('4,5')).toEqual({ ok: true, value: 450 });
    expect(parsePercentToBasisPoints('10%')).toEqual({ ok: true, value: 1000 });
    expect(parsePercentToBasisPoints('-3', { allowNegative: true })).toEqual({ ok: true, value: -300 });
  });

  it('lê e exibe quantidades decimais', () => {
    expect(parseQuantity('2,5')).toEqual({ ok: true, value: '2.5' });
    expect(parseQuantity('1000')).toEqual({ ok: true, value: '1000' });
    expect(parseQuantity('0').ok).toBe(false);
    expect(parseQuantity('0', { allowZero: true })).toEqual({ ok: true, value: '0' });
    expect(parseQuantity('1.000,5').ok).toBe(false);
    expect(formatQuantity('1234567.5')).toBe('1.234.567,5');
  });

  it('gera texto editável a partir de centavos', () => {
    expect(centsToInput(123456)).toBe('1.234,56');
    expect(centsToInput(5)).toBe('0,05');
  });
});
