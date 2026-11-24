import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';
import { login, selectTenant, TENANTS, USERS } from './helpers';

/**
 * (j) Auditoria automática de acessibilidade (ADR-090): passa pelas telas
 * principais logado como a Ana, em desktop e em celular, e falha em violações
 * "serious" ou "critical" das regras WCAG 2.1 A e AA do axe-core. Confere
 * também a navegação por teclado: o Tab alcança o menu e os botões com foco
 * visível.
 */

const WCAG_TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'];
const BLOCKING = new Set(['serious', 'critical']);

const PAGES: [string, string][] = [
  ['Painel', '/'],
  ['Clientes', '/clientes'],
  ['Catálogo: planos', '/catalogo/planos'],
  ['Catálogo: produtos', '/catalogo/produtos'],
  ['Catálogo: métricas', '/catalogo/metricas'],
  ['Catálogo: cupons', '/catalogo/cupons'],
  ['Assinaturas', '/assinaturas'],
  ['Faturas', '/faturas'],
  ['Cobranças', '/cobrancas'],
  ['Recebimentos', '/recebimentos'],
  ['Reembolsos', '/reembolsos'],
  ['Disputas', '/disputas'],
  ['Operações de pagamento', '/operacoes-pagamento'],
  ['Ledger', '/ledger'],
  ['Ledger: lançamentos', '/ledger?aba=lancamentos'],
  ['Conciliação', '/conciliacao'],
  ['Conciliação: divergências', '/conciliacao?aba=divergencias'],
  ['Políticas de cobrança', '/cobranca'],
  ['Usuários e permissões', '/usuarios'],
  ['Integrações', '/integracoes'],
  ['Auditoria', '/auditoria'],
  ['Operações', '/operacoes'],
  ['Organização', '/organizacao'],
  ['Plano Recorvia', '/plataforma'],
];

/** Listas cujo primeiro item abre uma tela de detalhe. */
const DETAILS: [string, string][] = [
  ['Detalhe do cliente', '/clientes'],
  ['Detalhe da assinatura', '/assinaturas'],
  ['Detalhe da fatura', '/faturas'],
  ['Detalhe do plano', '/catalogo/planos'],
  ['Detalhe da cobrança', '/cobrancas'],
];

interface Finding {
  screen: string;
  rule: string;
  impact: string;
  help: string;
  targets: string[];
}

async function settle(page: Page): Promise<void> {
  await page.waitForLoadState('networkidle').catch(() => undefined);
  await expect(page.locator('[aria-busy="true"]')).toHaveCount(0, { timeout: 20_000 });
  await expect(page.getByRole('heading', { level: 1 }).first()).toBeVisible();
}

async function audit(page: Page, screen: string, findings: Finding[]): Promise<void> {
  await settle(page);
  const result = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  for (const v of result.violations) {
    if (!BLOCKING.has(v.impact ?? '')) continue;
    findings.push({
      screen,
      rule: v.id,
      impact: v.impact ?? '',
      help: v.help,
      targets: v.nodes.slice(0, 5).map((n) => n.target.join(' ')),
    });
  }
}

function report(findings: Finding[]): string {
  return findings
    .map((f) => `- [${f.impact}] ${f.screen}: ${f.rule} (${f.help})\n    ${f.targets.join('\n    ')}`)
    .join('\n');
}

async function openFirstDetail(page: Page, listPath: string): Promise<boolean> {
  await page.goto(listPath);
  await settle(page);
  const prefix = listPath.replace(/\?.*$/, '');
  const link = page.locator(`main a[href^="${prefix}/"]`).first();
  if (!(await link.count())) return false;
  await link.click();
  await page.waitForURL((url) => url.pathname.startsWith(`${prefix}/`));
  return true;
}

for (const viewport of [
  { name: 'desktop', width: 1440, height: 900 },
  { name: 'celular', width: 390, height: 844 },
] as const) {
  test(`(j) telas principais sem violações graves de acessibilidade (${viewport.name})`, async ({ page }) => {
    test.setTimeout(6 * 60_000);
    const findings: Finding[] = [];

    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto('/entrar');
    await expect(page.getByRole('link', { name: 'Entrar' })).toBeVisible();
    await page.setViewportSize({ width: viewport.width, height: viewport.height });
    const entry = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
    for (const v of entry.violations.filter((x) => BLOCKING.has(x.impact ?? ''))) {
      findings.push({ screen: 'Entrar', rule: v.id, impact: v.impact ?? '', help: v.help, targets: v.nodes.slice(0, 5).map((n) => n.target.join(' ')) });
    }

    // O seletor de organização fica no cabeçalho no desktop: entra e escolhe a
    // organização nessa largura e só então muda para a do teste.
    await page.setViewportSize({ width: 1440, height: 900 });
    await login(page, USERS.ana);
    await selectTenant(page, TENANTS.acme);
    await page.setViewportSize({ width: viewport.width, height: viewport.height });

    for (const [screen, path] of PAGES) {
      await test.step(screen, async () => {
        await page.goto(path);
        await audit(page, screen, findings);
      });
    }
    for (const [screen, list] of DETAILS) {
      await test.step(screen, async () => {
        if (await openFirstDetail(page, list)) await audit(page, screen, findings);
      });
    }
    if (viewport.name === 'celular') {
      await test.step('Menu lateral aberto', async () => {
        await page.goto('/');
        await settle(page);
        await page.getByRole('button', { name: 'Abrir ou fechar navegação' }).click();
        await expect(page.getByRole('textbox', { name: 'Organização ativa' })).toBeVisible();
        await audit(page, 'Menu lateral', findings);
      });
    }

    expect(findings, `Violações graves ou críticas (WCAG 2.1 AA):\n${report(findings)}`).toEqual([]);
  });
}

test('(j) teclado: Tab passa pelo salto de conteúdo, menu e botões com foco visível', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page, USERS.ana);
  await selectTenant(page, TENANTS.acme);
  await page.goto('/clientes');
  await settle(page);
  await page.locator('body').focus();

  const focused = async () =>
    page.evaluate(() => {
      const el = document.activeElement as HTMLElement | null;
      if (!el || el === document.body) return null;
      const style = getComputedStyle(el);
      const outline = style.outlineStyle !== 'none' && parseFloat(style.outlineWidth) > 0;
      const ring = style.boxShadow !== 'none';
      return {
        name: (el.getAttribute('aria-label') || el.textContent || '').trim().replace(/\s+/g, ' '),
        tag: el.tagName,
        visible: outline || ring,
        inNav: !!el.closest('nav'),
      };
    });

  // O primeiro Tab mostra o atalho "Pular para o conteúdo".
  await page.keyboard.press('Tab');
  const skip = await focused();
  expect(skip?.name).toBe('Pular para o conteúdo');
  await expect(page.getByRole('link', { name: 'Pular para o conteúdo' })).toBeInViewport();

  const seen: NonNullable<Awaited<ReturnType<typeof focused>>>[] = [];
  for (let i = 0; i < 40; i++) {
    await page.keyboard.press('Tab');
    const f = await focused();
    if (f) seen.push(f);
  }
  const invisible = seen.filter((f) => !f.visible);
  expect(invisible, `Elementos sem foco visível: ${invisible.map((f) => `${f.tag} "${f.name}"`).join(', ')}`).toEqual([]);
  expect(seen.some((f) => f.inNav && f.name === 'Clientes')).toBe(true);
  expect(seen.some((f) => f.inNav && f.name === 'Faturas')).toBe(true);
  expect(seen.some((f) => f.tag === 'BUTTON' && f.name.includes('Novo cliente'))).toBe(true);

  // Enter no item do menu navega e leva o foco ao conteúdo.
  await page.getByRole('link', { name: 'Faturas', exact: true }).focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { level: 1, name: 'Faturas' })).toBeVisible();
  await expect(page.locator('main')).toBeFocused();
});
