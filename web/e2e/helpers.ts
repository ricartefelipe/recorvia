import { expect, type Locator, type Page } from '@playwright/test';

/** Senha dos usuários de demonstração do realm local do Keycloak. */
export const DEMO_PASSWORD = 'Recorvia#Demo2026';

export const USERS = {
  ana: 'ana.owner@acme.test',
  carla: 'carla.owner@beta.test',
  davi: 'davi.auditor@acme.test',
} as const;

/** Nomes das organizações criadas pelo seed de demonstração. */
export const TENANTS = {
  acme: 'Acme Software (demonstração)',
  beta: 'Beta Consultoria (demonstração)',
} as const;

/** Entra pela tela do Recorvia e autentica no formulário do Keycloak. */
export async function login(page: Page, email: string, password: string = DEMO_PASSWORD): Promise<void> {
  await page.goto('/entrar');
  const appOrigin = new URL(page.url()).origin;
  await page.getByRole('link', { name: 'Entrar' }).click();
  await page.locator('#username').fill(email);
  await page.locator('#password').fill(password);
  await page.locator('#kc-login').click();
  await page.waitForURL((url) => url.origin === appOrigin && !url.pathname.startsWith('/auth/'), { timeout: 30_000 });
  await expect(page.getByText('Carregando sua sessão…')).toHaveCount(0, { timeout: 30_000 });
}

/** Garante a organização ativa, pela tela de seleção ou pelo seletor do cabeçalho. */
export async function selectTenant(page: Page, name: string): Promise<void> {
  // O Select expõe o campo e a lista de opções com o mesmo rótulo.
  const selector = page.getByRole('textbox', { name: 'Organização ativa' });
  const chooser = page.getByRole('heading', { name: 'Escolha a organização' });
  await expect(selector.or(chooser)).toBeVisible({ timeout: 30_000 });
  if (await chooser.isVisible()) {
    await page.getByRole('button', { name: `Entrar em ${name}` }).click();
  } else if ((await selector.inputValue()) !== name) {
    await selector.click();
    await page.getByRole('option', { name, exact: true }).click();
  }
  await expect(selector).toHaveValue(name, { timeout: 30_000 });
}

/** Escolhe uma opção num Select do Mantine pelo rótulo do campo. */
export async function chooseOption(scope: Page | Locator, label: string | RegExp, option: string | RegExp): Promise<void> {
  const page = 'page' in scope ? scope.page() : scope;
  await scope.getByLabel(label).first().click();
  await page.getByRole('option', { name: option }).first().click();
}

/** Busca e escolhe em um Select com busca no servidor (por exemplo, cliente). */
export async function searchAndChoose(scope: Page | Locator, label: string | RegExp, text: string): Promise<void> {
  const page = 'page' in scope ? scope.page() : scope;
  const input = scope.getByLabel(label).first();
  await input.click();
  await input.fill(text);
  await page.getByRole('option', { name: text }).first().click();
}

/** CPF estruturalmente válido (dígitos verificadores corretos) para cadastros de teste. */
export function validCpf(seed: number = Date.now()): string {
  let base = String(seed).slice(-9).padStart(9, '1').split('').map(Number);
  if (base.every((d) => d === base[0])) base = [1, 2, 3, 4, 5, 6, 7, 8, 9];
  const check = (digits: number[]) => {
    const sum = digits.reduce((acc, d, i) => acc + d * (digits.length + 1 - i), 0);
    const rest = (sum * 10) % 11;
    return rest === 10 ? 0 : rest;
  };
  const d1 = check(base);
  const d2 = check([...base, d1]);
  return [...base, d1, d2].join('');
}

export function uniqueSuffix(): string {
  return `${Date.now().toString(36)}${Math.floor(Math.random() * 1000).toString(36)}`;
}
