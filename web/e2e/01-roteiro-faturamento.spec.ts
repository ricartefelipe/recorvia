import { expect, test } from '@playwright/test';
import { chooseOption, login, searchAndChoose, selectTenant, TENANTS, uniqueSuffix, USERS, validCpf } from './helpers';

/**
 * Roteiro demonstrativo: cadastro, faturamento, pagamento pelo checkout
 * hospedado do simulador e conferência no ledger e na auditoria.
 */
test.describe.serial('roteiro de faturamento da Acme', () => {
  const suffix = uniqueSuffix();
  const customerName = `Cliente E2E ${suffix}`;
  const productName = `Produto E2E ${suffix}`;
  const planName = `Plano E2E ${suffix}`;
  let invoicePath = '';
  let checkoutUrl = '';

  test('(a) cria cliente, plano e assinatura, executa o faturamento e vê a fatura com cobrança e checkout', async ({ page }) => {
    await login(page, USERS.ana);
    await selectTenant(page, TENANTS.acme);

    await test.step('cadastra o cliente', async () => {
      await page.goto('/clientes');
      await page.getByRole('button', { name: 'Novo cliente' }).click();
      const dialog = page.getByRole('dialog', { name: 'Novo cliente' });
      await dialog.getByLabel(/Nome completo/).fill(customerName);
      await dialog.getByLabel(/^CPF/).fill(validCpf());
      await dialog.getByLabel('E-mail').fill(`e2e-${suffix}@example.test`);
      await dialog.getByRole('button', { name: 'Cadastrar cliente' }).click();
      await expect(page.getByRole('heading', { name: customerName })).toBeVisible();
    });
    const customerPath = new URL(page.url()).pathname;

    await test.step('cria produto e plano com preço fixo', async () => {
      await page.goto('/catalogo/produtos');
      await page.getByRole('button', { name: 'Novo produto' }).click();
      const productDialog = page.getByRole('dialog', { name: 'Novo produto' });
      await productDialog.getByLabel(/^Nome/).fill(productName);
      await productDialog.getByRole('button', { name: 'Salvar' }).click();
      await expect(page.getByRole('cell', { name: productName })).toBeVisible();

      await page.goto('/catalogo/planos');
      await page.getByRole('button', { name: 'Novo plano' }).click();
      const planDialog = page.getByRole('dialog', { name: 'Novo plano' });
      await chooseOption(planDialog, /^Produto/, productName);
      await planDialog.getByLabel(/^Código/).fill(`e2e-${suffix}`);
      await planDialog.getByLabel(/^Nome/).fill(planName);
      await planDialog.getByLabel(/Valor fixo por período/).fill('149,90');
      await planDialog.getByRole('button', { name: 'Criar plano' }).click();
      await expect(page.getByRole('heading', { name: planName })).toBeVisible();
    });

    await test.step('cria a assinatura', async () => {
      await page.goto('/assinaturas');
      await page.getByRole('button', { name: 'Nova assinatura' }).click();
      const dialog = page.getByRole('dialog', { name: 'Nova assinatura' });
      await searchAndChoose(dialog, /^Cliente/, customerName);
      await chooseOption(dialog, /^Plano/, planName);
      await dialog.getByRole('button', { name: 'Criar assinatura' }).click();
      await expect(page.getByRole('heading', { name: new RegExp(planName) })).toBeVisible();
    });

    await test.step('executa o faturamento', async () => {
      await page.goto('/faturas');
      await page.getByRole('button', { name: 'Executar faturamento agora' }).click();
      await page.getByRole('dialog', { name: 'Executar faturamento agora' }).getByRole('button', { name: 'Executar faturamento' }).click();
      const result = page.getByRole('dialog', { name: 'Faturamento executado' });
      await expect(result).toBeVisible();
      await result.getByRole('button', { name: 'Fechar' }).click();
    });

    await test.step('abre a fatura emitida e confere cobrança e checkout', async () => {
      await expect(async () => {
        await page.goto(`${customerPath}?aba=faturas`);
        await page.getByRole('table', { name: 'Faturas' }).getByRole('link').first().click({ timeout: 3_000 });
      }).toPass({ timeout: 60_000 });
      await expect(page.getByRole('heading', { name: /^Fatura / })).toBeVisible();
      await expect(page.getByRole('table', { name: 'Totais' })).toContainText('R$ 149,90');
      invoicePath = new URL(page.url()).pathname;

      // A cobrança é solicitada ao provedor de forma assíncrona após a emissão.
      await expect(async () => {
        await page.reload();
        await expect(page.getByRole('table', { name: 'Cobranças' }).getByRole('link', { name: /Abrir checkout/ })).toBeVisible({ timeout: 3_000 });
      }).toPass({ timeout: 90_000 });
      checkoutUrl = (await page.getByRole('link', { name: /Abrir checkout/ }).first().getAttribute('href')) ?? '';
      expect(checkoutUrl).toMatch(/^https?:\/\//);
    });
  });

  test('(b) paga pelo checkout hospedado do simulador e vê a fatura paga, o ledger e a auditoria', async ({ page }) => {
    expect(checkoutUrl, 'o cenário (a) precisa ter gerado a cobrança').not.toBe('');
    await login(page, USERS.ana);
    await selectTenant(page, TENANTS.acme);

    await test.step('confirma o pagamento no checkout do simulador', async () => {
      await page.goto(checkoutUrl);
      await page.getByRole('button', { name: 'Confirmar pagamento' }).click();
    });

    await test.step('a fatura fica paga após o webhook do provedor', async () => {
      await expect(async () => {
        await page.goto(invoicePath);
        await expect(page.getByRole('table', { name: 'Totais' }).getByRole('row', { name: /^Saldo devedor/ })).toContainText('R$ 0,00', {
          timeout: 3_000,
        });
      }).toPass({ timeout: 90_000 });
      await expect(page.getByText('Paga', { exact: true }).first()).toBeVisible();
    });

    await test.step('confere os lançamentos no ledger', async () => {
      await page.getByRole('link', { name: 'Ver no ledger' }).click();
      await expect(page.getByRole('heading', { name: 'Ledger' })).toBeVisible();
      await expect(page.getByRole('table', { name: /^Partidas de / }).first()).toBeVisible();
      await page.getByRole('tab', { name: 'Contas e saldos' }).click();
      await expect(page.getByRole('table', { name: 'Contas do ledger' })).toBeVisible();
    });

    await test.step('confere a trilha de auditoria da fatura', async () => {
      const invoiceId = invoicePath.split('/').pop() ?? '';
      await page.goto(`/auditoria?id=${invoiceId}`);
      await expect(page.getByRole('heading', { name: 'Auditoria' })).toBeVisible();
      await expect(page.locator('code', { hasText: /^invoice\./ }).first()).toBeVisible();
    });
  });
});
