import { describe, expect, it } from 'vitest';
import {
  addDaysToCivilDate,
  currentMonthInTimeZone,
  formatCivilDate,
  formatDateTime,
  formatInstantDate,
  timeZoneLabel,
  timeZoneLabelInSentence,
  timeZoneName,
  timeZoneOptions,
  todayInTimeZone,
  zonedWallTimeToUtc,
} from './dates';

describe('datas e fusos', () => {
  it('exibe instantes no fuso da organização', () => {
    expect(formatDateTime('2026-11-16T02:30:00Z', 'America/Sao_Paulo')).toBe('15/11/2026, 23:30');
    expect(formatDateTime('2026-11-16T02:30:00Z', 'Asia/Tokyo')).toBe('16/11/2026, 11:30');
    expect(formatInstantDate('2026-11-16T02:30:00Z', 'America/Sao_Paulo')).toBe('15/11/2026');
  });

  it('exibe datas civis sem conversão de fuso', () => {
    expect(formatCivilDate('2026-11-16')).toBe('16/11/2026');
    expect(formatCivilDate('2026-01-01')).toBe('01/01/2026');
    expect(formatCivilDate(null)).toBe('—');
  });

  it('calcula hoje e o mês corrente no fuso informado', () => {
    const instant = new Date('2026-12-01T01:00:00Z');
    expect(todayInTimeZone('America/Sao_Paulo', instant)).toBe('2026-11-30');
    expect(currentMonthInTimeZone('America/Sao_Paulo', instant)).toBe('2026-11');
    expect(currentMonthInTimeZone('UTC', instant)).toBe('2026-12');
  });

  it('converte horário de parede para UTC', () => {
    expect(zonedWallTimeToUtc('2026-11-16 09:30:00', 'America/Sao_Paulo')).toBe('2026-11-16T12:30:00Z');
    expect(zonedWallTimeToUtc('2026-07-01 00:00:00', 'Europe/Lisbon')).toBe('2026-06-30T23:00:00Z');
    expect(zonedWallTimeToUtc('2026-01-15', 'UTC')).toBe('2026-01-15T00:00:00Z');
  });

  it('soma dias a datas civis', () => {
    expect(addDaysToCivilDate('2026-02-28', 1)).toBe('2026-03-01');
    expect(addDaysToCivilDate('2026-01-01', -366)).toBe('2024-12-31');
  });
});

describe('nomes de fuso', () => {
  it('mostra nomes amigáveis em português, com a cidade quando ajuda', () => {
    expect(timeZoneLabel('America/Sao_Paulo')).toBe('Horário de Brasília');
    expect(timeZoneLabel('America/Fortaleza')).toBe('Horário de Brasília (Fortaleza)');
    expect(timeZoneLabel('America/Manaus')).toBe('Horário do Amazonas (Manaus)');
    expect(timeZoneLabel('America/Cuiaba')).toMatch(/\(Cuiabá\)$/);
    expect(timeZoneLabel('UTC')).toBe('Tempo universal coordenado (UTC)');
  });

  it('usa o nome vigente do fuso, não o de 1970', () => {
    // Em 1970 Lisboa estava no horário da Europa Central; hoje, no da Europa Ocidental.
    expect(timeZoneName('Europe/Lisbon')).toBe('Horário da Europa Ocidental');
    expect(timeZoneName('Europe/Lisbon', new Date('2026-07-01T12:00:00Z'))).toBe('Horário da Europa Ocidental');
  });

  it('no meio da frase, só o nome amigável começa em minúscula', () => {
    expect(timeZoneLabelInSentence('America/Sao_Paulo')).toBe('horário de Brasília');
    expect(timeZoneLabelInSentence('America/Manaus')).toBe('horário do Amazonas (Manaus)');
    expect(timeZoneLabelInSentence('Etc/GMT+3')).toBe('Etc/GMT+3');
  });

  it('cai no código quando o fuso é desconhecido', () => {
    expect(timeZoneName('Nao/Existe')).toBe('Nao/Existe');
    expect(timeZoneLabel('Nao/Existe')).toBe('Nao/Existe');
  });

  it('gera opções com o código IANA como valor', () => {
    expect(timeZoneOptions(['America/Sao_Paulo'])).toEqual([{ value: 'America/Sao_Paulo', label: 'Horário de Brasília' }]);
  });
});
