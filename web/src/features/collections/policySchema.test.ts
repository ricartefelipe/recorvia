import { describe, expect, it } from 'vitest';
import { formatOffsets, policySchema } from './CollectionsPage';

const base = {
  name: 'Padrão',
  is_default: true,
  offsets: [{ value: 7 }, { value: -3 }, { value: 0 }],
  send_window_start_hour: 8,
  send_window_end_hour: 20,
  max_messages_per_day: 1,
  suspend: false,
  suspend_after_days: 30,
  reactivate_on_payment: true,
} as const;

describe('política de cobrança', () => {
  it('ordena os lembretes e envia suspensão nula quando desativada', () => {
    const parsed = policySchema.parse({ ...base, offsets: [...base.offsets] });
    expect(parsed.reminder_offsets_days).toEqual([-3, 0, 7]);
    expect(parsed.suspend_after_days).toBeNull();
  });

  it('valida janela de envio, dias de suspensão e lembretes repetidos', () => {
    const result = policySchema.safeParse({
      ...base,
      offsets: [{ value: 3 }, { value: 3 }],
      send_window_start_hour: 20,
      send_window_end_hour: 8,
      suspend: true,
      suspend_after_days: 0,
    });
    expect(result.success).toBe(false);
    const messages = result.error?.issues.map((i) => `${i.path.join('.')}: ${i.message}`) ?? [];
    expect(messages).toContain('send_window_end_hour: O fim da janela deve ser depois do início.');
    expect(messages).toContain('suspend_after_days: Informe de 1 a 180 dias.');
    expect(messages).toContain('offsets: Há lembretes repetidos.');
  });

  it('descreve os dias relativos ao vencimento', () => {
    expect(formatOffsets([3, -2, 0])).toBe('2 dia(s) antes, no vencimento, 3 dia(s) depois');
  });
});
