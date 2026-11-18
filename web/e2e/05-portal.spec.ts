import { expect, test, type Page } from '@playwright/test';
import { chooseOption, login, searchAndChoose, selectTenant, TENANTS, uniqueSuffix, USERS, validCpf } from './helpers';

/** Cadastra um cliente pessoa física e devolve o caminho da ficha. */
async function createCustomer(page: Page, name: string, seed: number): Promise<string> {
  await page.goto('/clientes');
  await page.getByRole('button', { name: 'Novo cliente' }).click();
  const dialog = page.getByRole('dialog', { name: 'Novo cliente' });
  await dialog.getByLabel(/Nome completo/).fill(name);
  await dialog.getByLabel(/^CPF/).fill(validCpf(seed));
  await dialog.getByLabel('E-mail').fill(`portal-${seed}@example.test`);
  await dialog.getByRole('button', { name: 'Cadastrar cliente' }).click();
  await expect(page.getByRole('heading', { name })).toBeVisible();
  return new URL(page.url()).pathname;
}

/** Cria a assinatura do cliente no plano informado. */
async function subscribe(page: Page, customerName: string, planName: string): Promise<void> {
  await page.goto('/assinaturas');
  await page.getByRole('button', { name: 'Nova assinatura' }).click();
  const dialog = page.getByRole('dialog', { name: 'Nova assinatura' });
  await searchAndChoose(dialog, /^Cliente/, customerName);
  await chooseOption(dialog, /^Plano/, planName);
  await dialog.getByRole('button', { name: 'Criar assinatura' }).click();
  await expect(page.getByRole('heading', { name: new RegExp(planName) })).toBeVisible();
}

/** Número da fatura emitida mais recente do cliente, lido na ficha. */
async function latestInvoiceNumber(page: Page, customerPath: string): Promise<string> {
  let number = '';
  await expect(async () => {
    await page.goto(`${customerPath}?aba=faturas`);
    const link = page.getByRole('table', { name: 'Faturas' }).getByRole('link').first();
    number = (await link.textContent({ timeout: 3_000 }))?.trim() ?? '';
    expect(number).toMatch(/^[A-Z0-9]+-\d+/);
  }).toPass({ timeout: 60_000 });
  return number;
}

/**
 * Portal do cliente: a organização gera o link na ficha do cliente e o
 * cliente, sem sessão do painel, vê somente as próprias faturas e sai.
 */
test.describe.serial('portal do cliente da Acme', () => {
  const suffix = uniqueSuffix();
  const seed = Date.now();
  const customerName = `Cliente Portal ${suffix}`;
  const otherName = `Outro Cliente Portal ${suffix}`;
  const productName = `Produto Portal ${suffix}`;
  const planName = `Plano Portal ${suffix}`;
  let customerPath = '';
  let ownInvoice = '';
  let otherInvoice = '';
  let portalPath = '';

  test('(a) prepara dois clientes com faturas emitidas', async ({ page }) => {
    await login(page, USERS.ana);
    await selectTenant(page, TENANTS.acme);

    let otherPath = '';
    await test.step('cadastra os clientes', async () => {
      customerPath = await createCustomer(page, customerName, seed);
      otherPath = await createCustomer(page, otherName, seed + 7);
    });

    await test.step('cria produto e plano com preço fixo', async () => {
      await page.goto('/catalogo/produtos');
      await page.getByRole('button', { name: 'Novo produto' }).click();
      const productDialog = page.getByRole('dialog', { name: 'Novo produto' });
      await productDialog.getByLabel(/^Nome/).fill(productName);
      await productDialog.getByRole('button', { name: 'Salvar' }).click();
      await expect(page.getByRole('cell', { name: productName, exact: true })).toBeVisible();

      await page.goto('/catalogo/planos');
      await page.getByRole('button', { name: 'Novo plano' }).click();
      const planDialog = page.getByRole('dialog', { name: 'Novo plano' });
      await chooseOption(planDialog, /^Produto/, productName);
      await planDialog.getByLabel(/^Código/).fill(`portal-${suffix}`);
      await planDialog.getByLabel(/^Nome/).fill(planName);
      await planDialog.getByLabel(/Valor fixo por período/).fill('89,90');
      await planDialog.getByRole('button', { name: 'Criar plano' }).click();
      await expect(page.getByRole('heading', { name: planName })).toBeVisible();
    });

    await test.step('assina o plano para os dois clientes e executa o faturamento', async () => {
      await subscribe(page, customerName, planName);
      await subscribe(page, otherName, planName);
      await page.goto('/faturas');
      await page.getByRole('button', { name: 'Executar faturamento agora' }).click();
      await page.getByRole('dialog', { name: 'Executar faturamento agora' }).getByRole('button', { name: 'Executar faturamento' }).click();
      const result = page.getByRole('dialog', { name: 'Faturamento executado' });
      await expect(result).toBeVisible();
      await result.getByRole('button', { name: 'Fechar' }).click();
    });

    await test.step('lê o número das faturas de cada cliente', async () => {
      ownInvoice = await latestInvoiceNumber(page, customerPath);
      otherInvoice = await latestInvoiceNumber(page, otherPath);
      expect(otherInvoice).not.toBe(ownInvoice);
    });
  });

  test('(b) gera o link do portal na ficha do cliente', async ({ page }) => {
    expect(customerPath, 'o cenário (a) precisa ter cadastrado o cliente').not.toBe('');
    await login(page, USERS.ana);
    await selectTenant(page, TENANTS.acme);
    await page.goto(customerPath);
    await page.getByRole('button', { name: 'Link do portal' }).click();
    const dialog = page.getByRole('dialog', { name: 'Link do portal do cliente' });
    await expect(dialog).toContainText('uso único');
    const url = await dialog.getByRole('textbox', { name: 'Link do portal', exact: true }).inputValue();
    expect(url).toMatch(/\/portal\/access\?token=[A-Za-z0-9_-]{43}$/);
    // O link pode ser absoluto (RECORVIA_PUBLIC_URL); o roteiro usa o endereço do ambiente em teste.
    const parsed = new URL(url, page.url());
    portalPath = parsed.pathname + parsed.search;
    await dialog.getByRole('button', { name: 'Fechar' }).click();
  });

  test('(c) abre o portal em outro contexto, vê somente as faturas do cliente e sai', async ({ browser }) => {
    expect(portalPath, 'o cenário (b) precisa ter gerado o link').not.toBe('');
    // Contexto novo: sem a sessão do painel, apenas com o link recebido.
    const context = await browser.newContext();
    const portal = await context.newPage();

    await test.step('troca o link por uma sessão do portal', async () => {
      await portal.goto(portalPath);
      await expect(portal).toHaveURL(/\/portal$/);
      await expect(portal.getByText(`Portal do cliente · ${customerName}`)).toBeVisible();
      const cookies = await context.cookies();
      expect(cookies.some((c) => c.name.endsWith('recorvia_portal') && c.httpOnly && c.sameSite === 'Strict')).toBe(true);
      expect(cookies.some((c) => c.name.endsWith('recorvia_session'))).toBe(false);
    });

    await test.step('vê somente as faturas do cliente do link', async () => {
      const table = portal.getByRole('table', { name: 'Faturas' });
      await expect(table.getByRole('link', { name: ownInvoice, exact: true })).toBeVisible();
      await expect(table.getByRole('link', { name: otherInvoice, exact: true })).toHaveCount(0);
      await expect(table).not.toContainText(otherName);
    });

    await test.step('abre a fatura e o documento comercial', async () => {
      await portal.getByRole('table', { name: 'Faturas' }).getByRole('link', { name: ownInvoice, exact: true }).click();
      await expect(portal.getByRole('heading', { name: `Fatura ${ownInvoice}` })).toBeVisible();
      await expect(portal.getByRole('table', { name: 'Itens da fatura' })).toContainText(planName);
      await portal.getByRole('button', { name: 'Ver documento' }).click();
      const viewer = portal.getByRole('dialog', { name: `Fatura ${ownInvoice}` });
      await expect(viewer.frameLocator('iframe').getByRole('heading', { name: `Fatura ${ownInvoice}` })).toBeVisible();
      await portal.keyboard.press('Escape');
    });

    await test.step('sai do portal', async () => {
      await portal.getByRole('button', { name: 'Sair' }).click();
      await expect(portal.getByRole('heading', { name: 'Sessão encerrada' })).toBeVisible();
      await portal.goto('/portal');
      await expect(portal.getByRole('heading', { name: 'Acesse pelo link recebido' })).toBeVisible();
    });

    await test.step('o link é de uso único', async () => {
      const reused = await portal.goto(portalPath);
      expect(reused?.status()).toBe(401);
    });

    await context.close();
  });
});
