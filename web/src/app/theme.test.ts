import { describe, expect, it } from 'vitest';
import {
  ACCESSIBLE_COLORS,
  contrastRatio,
  DIMMED_COLOR,
  FIELD_BORDER_COLOR,
  PLACEHOLDER_COLOR,
  accessibleShade,
} from './theme';

describe('tema acessível', () => {
  it('calcula a razão de contraste WCAG', () => {
    expect(contrastRatio('#000000', '#ffffff')).toBeCloseTo(21, 0);
    expect(contrastRatio('#868e96', '#ffffff')).toBeCloseTo(3.32, 1);
  });

  it.each(Object.entries(ACCESSIBLE_COLORS))('%s: texto, badge claro e preenchido com pelo menos 4,5:1', (_, c) => {
    expect(contrastRatio(c.text, '#ffffff')).toBeGreaterThanOrEqual(4.5);
    expect(contrastRatio(c.text, c.light)).toBeGreaterThanOrEqual(4.5);
    expect(contrastRatio('#ffffff', c.filled)).toBeGreaterThanOrEqual(4.5);
    expect(contrastRatio('#ffffff', c.filledHover)).toBeGreaterThanOrEqual(4.5);
  });

  it('textos secundários, placeholders e bordas de campo atendem ao mínimo', () => {
    expect(contrastRatio(DIMMED_COLOR, '#ffffff')).toBeGreaterThanOrEqual(4.5);
    // Fundo cinza mais claro do tema (gray.0), usado em linhas expandidas.
    expect(contrastRatio(DIMMED_COLOR, '#f8f9fa')).toBeGreaterThanOrEqual(4.5);
    expect(contrastRatio(PLACEHOLDER_COLOR, '#ffffff')).toBeGreaterThanOrEqual(4.5);
    expect(contrastRatio(FIELD_BORDER_COLOR, '#ffffff')).toBeGreaterThanOrEqual(3);
  });

  it('escurece a cor quando nenhum tom da paleta basta', () => {
    const pale = ['#fff', '#fff', '#fff', '#fff', '#fff', '#fff', '#ffe066', '#ffd43b', '#fcc419', '#fab005'];
    expect(contrastRatio(accessibleShade(pale, '#ffffff'), '#ffffff')).toBeGreaterThanOrEqual(4.6);
  });
});
