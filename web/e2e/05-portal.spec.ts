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

/** Números das faturas emitidas do cliente, lidos na ficha (espera haver count faturas). */
async function invoiceNumbers(page: Page, customerPath: string, count: number): Promise<string[]> {
  let numbers: string[] = [];
  await expect(async () => {
    await page.goto(`${customerPath}?aba=faturas`);
    const rows = page.getByRole('table', { name: 'Faturas' }).locator('tbody tr');
    await expect(rows).toHaveCount(count, { timeout: 3_000 });
    numbers = (await rows.getByRole('link').allTextContents()).map((n) => n.trim());
    expect(numbers).toHaveLength(count);
    for (const number of numbers) expect(number).toMatch(/^[A-Z0-9]+-\d+/);
  }).toPass({ timeout: 60_000 });
  return numbers;
}

/** Gera um link do portal na ficha aberta e devolve o caminho de acesso. */
async function generatePortalLink(page: Page): Promise<string> {
  await page.getByRole('button', { name: 'Link do portal', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: 'Link do portal do cliente' });
  await expect(dialog).toContainText('uso único');
  const url = await dialog.getByRole('textbox', { name: 'Link do portal', exact: true }).inputValue();
  expect(url).toMatch(/\/portal\/access\?token=[A-Za-z0-9_-]{43}$/);
  await dialog.getByRole('button', { name: 'Fechar' }).click();
  await expect(dialog).toBeHidden();
  // O link pode ser absoluto (RECORVIA_PUBLIC_URL); o roteiro usa o endereço do ambiente em teste.
  const parsed = new URL(url, page.url());
  return parsed.pathname + parsed.search;
}

/** Revoga, na aba Portal da ficha, o link mais recente do cliente. */
async function revokeLatestLink(page: Page, customerPath: string, expectedStatus: 'Pendente' | 'Utilizado'): Promise<void> {
  await page.goto(`${customerPath}?aba=portal`);
  const latest = page.getByRole('table', { name: 'Links do portal' }).locator('tbody tr').first();
  await expect(latest).toContainText(expectedStatus);
  await latest.getByRole('button', { name: 'Revogar', exact: true }).click();
  const confirm = page.getByRole('dialog', { name: 'Revogar link do portal' });
  await confirm.getByRole('button', { name: 'Revogar link', exact: true }).click();
  await expect(page.getByText(/^Link revogado\./).first()).toBeVisible();
  await expect(latest).toContainText('Revogado pela organização');
  await expect(latest.getByRole('button', { name: 'Revogar', exact: true })).toHaveCount(0);
}

/**
 * Portal do cliente: a organização gera, lista e revoga links na ficha do
 * cliente; o cliente, sem sessão do painel, vê somente as próprias faturas,
 * em páginas, e sai. Revogar o link em uso encerra a sessão do portal.
 */
test.describe.serial('portal do cliente da Acme', () => {
  const suffix = uniqueSuffix();
  const seed = Date.now();
  const customerName = `Cliente Portal ${suffix}`;
  const otherName = `Outro Cliente Portal ${suffix}`;
  const productName = `Produto Portal ${suffix}`;
  const planName = `Plano Portal ${suffix}`;
  let customerPath = '';
  let ownInvoices: string[] = [];
  let otherInvoice = '';
  let revokedPath = '';
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

    await test.step('assina o plano (duas assinaturas para o cliente do portal) e executa o faturamento', async () => {
      // Duas assinaturas geram duas faturas, o bastante para paginar a lista do portal.
      await subscribe(page, customerName, planName);
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
      ownInvoices = await invoiceNumbers(page, customerPath, 2);
      [otherInvoice] = await invoiceNumbers(page, otherPath, 1);
      expect(ownInvoices).not.toContain(otherInvoice);
    });
  });

  test('(b) gera o link na ficha do cliente, revoga pela lista e gera outro', async ({ page }) => {
    expect(customerPath, 'o cenário (a) precisa ter cadastrado o cliente').not.toBe('');
    await login(page, USERS.ana);
    await selectTenant(page, TENANTS.acme);
    await page.goto(customerPath);

    await test.step('gera um link e o revoga antes de ser aberto', async () => {
      revokedPath = await generatePortalLink(page);
      await revokeLatestLink(page, customerPath, 'Pendente');
    });

    await test.step('gera o link que o cliente vai usar', async () => {
      portalPath = await generatePortalLink(page);
      expect(portalPath).not.toBe(revokedPath);
      const rows = page.getByRole('table', { name: 'Links do portal' }).locator('tbody tr');
      await expect(rows).toHaveCount(2);
      await expect(rows.first()).toContainText('Pendente');
      await expect(rows.nth(1)).toContainText('Revogado');
      // A lista nunca mostra o endereço do link.
      await expect(page.getByRole('table', { name: 'Links do portal' })).not.toContainText('token=');
    });
  });

  test('(c) abre o portal em outro contexto, vê somente as faturas do cliente e sai', async ({ browser }) => {
    expect(portalPath, 'o cenário (b) precisa ter gerado o link').not.toBe('');
    // Contexto novo: sem a sessão do painel, apenas com o link recebido.
    const context = await browser.newContext();
    const portal = await context.newPage();
    // Página de uma fatura por vez: as duas faturas do cliente exercitam o cursor.
    await portal.route(/\/api\/portal\/v1\/invoices(\?.*)?$/, async (route) => {
      const url = new URL(route.request().url());
      url.searchParams.set('limit', '1');
      await route.continue({ url: url.toString() });
    });

    await test.step('o link revogado não abre o portal', async () => {
      const revoked = await portal.goto(revokedPath);
      expect(revoked?.status()).toBe(401);
    });

    await test.step('troca o link por uma sessão do portal', async () => {
      await portal.goto(portalPath);
      await expect(portal).toHaveURL(/\/portal$/);
      await expect(portal.getByText(`Portal do cliente · ${customerName}`)).toBeVisible();
      const cookies = await context.cookies();
      expect(cookies.some((c) => c.name.endsWith('recorvia_portal') && c.httpOnly && c.sameSite === 'Strict')).toBe(true);
      expect(cookies.some((c) => c.name.endsWith('recorvia_session'))).toBe(false);
    });

    await test.step('vê somente as faturas do cliente do link, carregando a página seguinte', async () => {
      const table = portal.getByRole('table', { name: 'Faturas' });
      const rows = table.locator('tbody tr');
      await expect(rows).toHaveCount(1);
      await expect(portal.getByText('1 fatura exibida; há faturas mais antigas.')).toBeVisible();
      await portal.getByRole('button', { name: 'Carregar mais faturas' }).click();
      await expect(rows).toHaveCount(2);
      for (const number of ownInvoices) {
        await expect(table.getByRole('link', { name: number, exact: true })).toHaveCount(1);
      }
      await expect(portal.getByRole('button', { name: 'Carregar mais faturas' })).toHaveCount(0);
      await expect(portal.getByText('2 faturas exibidas; não há mais faturas.')).toBeVisible();
      await expect(table.getByRole('link', { name: otherInvoice, exact: true })).toHaveCount(0);
      await expect(table).not.toContainText(otherName);
    });

    await test.step('abre a fatura e o documento comercial', async () => {
      const [ownInvoice] = ownInvoices;
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

  test('(d) revogar o link em uso encerra a sessão do portal', async ({ page, browser }) => {
    expect(customerPath, 'o cenário (a) precisa ter cadastrado o cliente').not.toBe('');
    await login(page, USERS.ana);
    await selectTenant(page, TENANTS.acme);
    await page.goto(customerPath);
    const path = await generatePortalLink(page);

    const context = await browser.newContext();
    const portal = await context.newPage();
    await test.step('o cliente abre o portal com o novo link', async () => {
      await portal.goto(path);
      await expect(portal).toHaveURL(/\/portal$/);
      await expect(portal.getByRole('table', { name: 'Faturas' }).locator('tbody tr')).toHaveCount(2);
    });

    await test.step('a organização revoga o link em uso', async () => {
      await revokeLatestLink(page, customerPath, 'Utilizado');
    });

    await test.step('a sessão do portal deixa de valer', async () => {
      const response = await portal.request.get('/api/portal/v1/invoices');
      expect(response.status()).toBe(401);
      await portal.reload();
      await expect(portal.getByRole('heading', { name: 'Acesse pelo link recebido' })).toBeVisible();
    });

    await context.close();
  });
});
