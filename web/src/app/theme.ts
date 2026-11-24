import { createTheme, DEFAULT_THEME, type CSSVariablesResolver, type MantineColorsTuple } from '@mantine/core';

/**
 * Tema do Mantine com cores de texto e de preenchimento que atendem ao
 * contraste mínimo de 4,5:1 (WCAG 2.1 AA, critério 1.4.3) no esquema claro.
 * As paletas padrão do Mantine usam o tom 6 para texto colorido, badges claros
 * e botões preenchidos, o que fica entre 1,8:1 (amarelo) e 3,6:1 (azul).
 * Aqui cada cor recebe um tom de texto e um de preenchimento calculados a
 * partir da própria paleta (ADR-090).
 */

/** Contraste mínimo almejado, com folga sobre os 4,5:1 do critério AA. */
export const MIN_TEXT_CONTRAST = 4.6;

/** Cinza dos textos secundários (c="dimmed"): 6,1:1 sobre branco. */
export const DIMMED_COLOR = '#5c636a';
/** Placeholders: 4,7:1 sobre o fundo branco dos campos. */
export const PLACEHOLDER_COLOR = '#6c757d';
/** Borda de campos e botões neutros: 3,3:1, acima dos 3:1 de componentes (1.4.11). */
export const FIELD_BORDER_COLOR = '#868e96';

type Rgb = [number, number, number];

function toRgb(hex: string): Rgb {
  const h = hex.replace('#', '');
  return [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16)) as Rgb;
}

function toHex(rgb: Rgb): string {
  return `#${rgb.map((c) => Math.round(c).toString(16).padStart(2, '0')).join('')}`;
}

function luminance([r, g, b]: Rgb): number {
  const channel = (c: number) => {
    const v = c / 255;
    return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
}

/** Razão de contraste WCAG entre duas cores hexadecimais. */
export function contrastRatio(a: string, b: string): number {
  const [hi, lo] = [luminance(toRgb(a)), luminance(toRgb(b))].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

/** Mistura `color` sobre `background` com opacidade `alpha` (como o rgba do Mantine). */
export function blend(color: string, alpha: number, background = '#ffffff'): string {
  const c = toRgb(color);
  const bg = toRgb(background);
  return toHex(c.map((v, i) => v * alpha + bg[i] * (1 - alpha)) as Rgb);
}

/**
 * Primeiro tom da paleta, a partir do 6, com contraste suficiente sobre
 * `background`; se nenhum bastar, escurece o tom 9 até atingir o mínimo.
 */
export function accessibleShade(palette: readonly string[], background: string, min = MIN_TEXT_CONTRAST): string {
  for (let shade = 6; shade < palette.length; shade++) {
    if (contrastRatio(palette[shade], background) >= min) return palette[shade];
  }
  let rgb = toRgb(palette[palette.length - 1]);
  while (contrastRatio(toHex(rgb), background) < min) {
    rgb = rgb.map((c) => c * 0.94) as Rgb;
  }
  return toHex(rgb);
}

export interface AccessibleColor {
  /** Texto colorido, links, badges claros e contornos. */
  text: string;
  /** Fundo do variant light (o mesmo do Mantine: tom 6 a 10%). */
  light: string;
  /** Fundo dos variants filled, com texto branco. */
  filled: string;
  filledHover: string;
}

export function accessibleColor(palette: readonly string[]): AccessibleColor {
  const light = blend(palette[6], 0.1);
  const filled = accessibleShade(palette, '#ffffff');
  const index = palette.indexOf(filled);
  return {
    text: accessibleShade(palette, light),
    light,
    filled,
    filledHover: index >= 0 && index < palette.length - 1 ? palette[index + 1] : blend(filled, 0.85, '#000000'),
  };
}

const colors = DEFAULT_THEME.colors as Record<string, MantineColorsTuple>;

/** Tons acessíveis de todas as cores do tema, calculados uma vez. */
export const ACCESSIBLE_COLORS: Record<string, AccessibleColor> = Object.fromEntries(
  Object.entries(colors)
    .filter(([name]) => name !== 'dark')
    .map(([name, palette]) => [name, accessibleColor(palette)]),
);

export const cssVariablesResolver: CSSVariablesResolver = () => {
  const light: Record<string, string> = {
    '--mantine-color-dimmed': DIMMED_COLOR,
    '--mantine-color-placeholder': PLACEHOLDER_COLOR,
    '--mantine-color-default-border': FIELD_BORDER_COLOR,
    '--mantine-color-anchor': ACCESSIBLE_COLORS.blue.text,
    '--mantine-color-error': ACCESSIBLE_COLORS.red.text,
  };
  for (const [name, c] of Object.entries(ACCESSIBLE_COLORS)) {
    light[`--mantine-color-${name}-text`] = c.text;
    light[`--mantine-color-${name}-light-color`] = c.text;
    light[`--mantine-color-${name}-outline`] = c.text;
    light[`--mantine-color-${name}-filled`] = c.filled;
    light[`--mantine-color-${name}-filled-hover`] = c.filledHover;
  }
  return { variables: {}, light, dark: {} };
};

export const theme = createTheme({
  primaryColor: 'blue',
  defaultRadius: 'md',
  fontFamily: 'Inter, system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif',
  headings: { fontWeight: '650' },
  components: {
    Table: { defaultProps: { highlightOnHover: true, verticalSpacing: 'sm' } },
    Modal: { defaultProps: { centered: true } },
    // Badges não encolhem em células estreitas (evita "CONCLUÍ…").
    Badge: { styles: { root: { flexShrink: 0, minWidth: 'max-content' } } },
  },
});
