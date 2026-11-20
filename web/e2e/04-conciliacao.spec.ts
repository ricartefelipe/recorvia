import { expect, type Page, test } from '@playwright/test';
import { chooseOption, login, searchAndChoose, selectTenant, TENANTS, uniqueSuffix, USERS, validCpf } from './helpers';

/**
 * Roteiro de recebíveis: reembolso parcial de uma cobrança paga pela
 * interface e divergência de taxa criada no simulador (ação diverge),
 * detectada pela conciliação e resolvida com justificativa.
 */
test.describe.serial('reembolso e conciliação da Acme', () => {
  const suffix = uniqueSuffix();
  const customerName = `Cliente Conciliação ${suffix}`;
  const productName = `Produto Conciliação ${suffix}`;
  const planName = `Plano Conciliação ${suffix}`;
  const refundReason = `Reembolso parcial E2E ${suffix}`;
  const justification = `Taxa do simulador conferida no extrato (${suffix}).`;
  let invoicePath = '';
  let chargePath = '';
  let checkoutUrl = '';

  /** Cria cliente, plano e assinatura, emite a fatura e aguarda o checkout da cobrança. */
  async function issueInvoiceWithCheckout(page: Page): Promise<void> {
    await page.goto('/clientes');
    await page.getByRole('button', { name: 'Novo cliente' }).click();
    const customerDialog = page.getByRole('dialog', { name: 'Novo cliente' });
    await customerDialog.getByLabel(/Nome completo/).fill(customerName);
    await customerDialog.getByLabel(/^CPF/).fill(validCpf());
    await customerDialog.getByLabel('E-mail').fill(`conciliacao-${suffix}@example.test`);
    await customerDialog.getByRole('button', { name: 'Cadastrar cliente' }).click();
    await expect(page.getByRole('heading', { name: customerName })).toBeVisible();
    const customerPath = new URL(page.url()).pathname;

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
    await planDialog.getByLabel(/^Código/).fill(`conc-${suffix}`);
    await planDialog.getByLabel(/^Nome/).fill(planName);
    await planDialog.getByLabel(/Valor fixo por período/).fill('149,90');
    await planDialog.getByRole('button', { name: 'Criar plano' }).click();
    await expect(page.getByRole('heading', { name: planName })).toBeVisible();

    await page.goto('/assinaturas');
    await page.getByRole('button', { name: 'Nova assinatura' }).click();
    const subscriptionDialog = page.getByRole('dialog', { name: 'Nova assinatura' });
    await searchAndChoose(subscriptionDialog, /^Cliente/, customerName);
    await chooseOption(subscriptionDialog, /^Plano/, planName);
    await subscriptionDialog.getByRole('button', { name: 'Criar assinatura' }).click();
    await expect(page.getByRole('heading', { name: new RegExp(planName) })).toBeVisible();

    await page.goto('/faturas');
    await page.getByRole('button', { name: 'Executar faturamento agora' }).click();
    await page.getByRole('dialog', { name: 'Executar faturamento agora' }).getByRole('button', { name: 'Executar faturamento' }).click();
    const result = page.getByRole('dialog', { name: 'Faturamento executado' });
    await expect(result).toBeVisible();
    await result.getByRole('button', { name: 'Fechar' }).click();

    await expect(async () => {
      await page.goto(`${customerPath}?aba=faturas`);
      await page.getByRole('table', { name: 'Faturas' }).getByRole('link').first().click({ timeout: 3_000 });
    }).toPass({ timeout: 60_000 });
    await expect(page.getByRole('heading', { name: /^Fatura / })).toBeVisible();
    invoicePath = new URL(page.url()).pathname;
    // A cobrança é solicitada ao provedor de forma assíncrona após a emissão.
    await expect(async () => {
      await page.reload();
      await expect(page.getByRole('table', { name: 'Cobranças' }).getByRole('link', { name: /Abrir checkout/ })).toBeVisible({ timeout: 3_000 });
    }).toPass({ timeout: 90_000 });
    checkoutUrl = (await page.getByRole('link', { name: /Abrir checkout/ }).first().getAttribute('href')) ?? '';
    expect(checkoutUrl).toMatch(/^https?:\/\//);
    chargePath = (await page.getByRole('table', { name: 'Cobranças' }).getByRole('link', { name: '#1' }).getAttribute('href')) ?? '';
    expect(chargePath).toMatch(/^\/cobrancas\//);
  }

  test('(a) paga a fatura pelo checkout e faz o reembolso parcial pela interface', async ({ page }) => {
    await login(page, USERS.ana);
    await selectTenant(page, TENANTS.acme);

    await test.step('emite a fatura com cobrança no simulador', async () => {
      await issueInvoiceWithCheckout(page);
    });

    await test.step('confirma o pagamento no checkout hospedado e aguarda o webhook', async () => {
      await page.goto(checkoutUrl);
      await page.getByRole('button', { name: 'Confirmar pagamento' }).click();
      await expect(async () => {
        await page.goto(invoicePath);
        await expect(page.getByRole('table', { name: 'Totais' }).getByRole('row', { name: /^Saldo devedor/ })).toContainText('R$ 0,00', {
          timeout: 3_000,
        });
      }).toPass({ timeout: 90_000 });
    });

    await test.step('reembolsa R$ 50,00 com motivo e confirmação explícita', async () => {
      await page.goto(chargePath);
      await expect(page.getByRole('heading', { name: 'Cobrança #1' })).toBeVisible();
      await page.getByRole('button', { name: 'Reembolsar' }).click();
      const modal = page.getByRole('dialog', { name: 'Reembolsar cobrança' });
      await expect(modal).toContainText('Reembolsável: R$ 149,90');
      await modal.getByLabel(/Valor do reembolso/).fill('50,00');
      await modal.getByLabel(/^Motivo/).fill(refundReason);
      await modal.getByRole('button', { name: 'Continuar' }).click();
      const confirm = page.getByRole('dialog', { name: 'Confirmar reembolso' });
      await expect(confirm).toContainText('R$ 50,00');
      await confirm.getByRole('button', { name: 'Reembolsar' }).click();
      await expect(page.getByText('Reembolso concluído.').first()).toBeVisible({ timeout: 30_000 });
    });

    await test.step('a cobrança fica parcialmente reembolsada e a fatura continua paga', async () => {
      await page.goto(chargePath);
      await expect(page.getByText('Parcialmente reembolsada').first()).toBeVisible();
      await expect(page.getByText('Reembolsável', { exact: true }).locator('..')).toContainText('R$ 99,90');
      await page.goto('/reembolsos');
      const row = page.getByRole('table', { name: 'Reembolsos' }).getByRole('row', { name: new RegExp(refundReason) });
      await expect(row).toContainText('R$ 50,00');
      await expect(row).toContainText('Concluído');
      await page.goto(invoicePath);
      await expect(page.getByRole('table', { name: 'Totais' }).getByRole('row', { name: /^Saldo devedor/ })).toContainText('R$ 0,00');
    });
  });

  test('(b) divergência de taxa criada no simulador é detectada e resolvida com justificativa', async ({ page }) => {
    expect(checkoutUrl, 'o cenário (a) precisa ter gerado a cobrança paga').not.toBe('');
    const providerChargeId = new URL(checkoutUrl).pathname.split('/').pop() ?? '';
    const simulator = new URL(checkoutUrl).origin;
    await login(page, USERS.ana);
    await selectTenant(page, TENANTS.acme);

    await test.step('altera a taxa no provedor sem notificar (ação diverge)', async () => {
      const response = await page.request.post(`${simulator}/sim/charges/${providerChargeId}/actions`, { data: { action: 'diverge', fee: 999 } });
      expect(response.ok()).toBeTruthy();
    });

    await test.step('executa a conciliação manual', async () => {
      await page.goto('/conciliacao');
      await page.getByRole('button', { name: 'Executar conciliação agora' }).click();
      await page.getByRole('dialog', { name: 'Executar conciliação agora' }).getByRole('button', { name: 'Executar conciliação' }).click();
      await expect(page.getByText(/^Conciliação concluída: /).first()).toBeVisible({ timeout: 60_000 });
      await expect(page.getByRole('table', { name: 'Execuções de conciliação' }).getByRole('row').nth(1)).toContainText('Concluída');
    });

    await test.step('a divergência de taxa aparece aberta e mostra local × provedor', async () => {
      await page.getByRole('tab', { name: 'Divergências' }).click();
      const row = page.getByRole('table', { name: 'Divergências' }).getByRole('row').filter({ hasText: providerChargeId }).filter({ hasText: 'Taxa divergente' });
      await expect(row).toHaveCount(1);
      await expect(row).toContainText('Aberta');
      await row.getByRole('link', { name: 'Analisar' }).click();
      await expect(page.getByRole('heading', { name: 'Taxa divergente' })).toBeVisible();
      const fee = page.getByRole('table', { name: 'Comparação local e provedor' }).getByRole('row').filter({ hasText: /^fee/ });
      await expect(fee).toContainText('R$ 9,99');
      await expect(fee).toContainText('(diferente)');
    });

    await test.step('resolve com ajuste no ledger, responsável e justificativa', async () => {
      const form = page.getByRole('form', { name: 'Resolver divergência' });
      await expect(form).toContainText(USERS.ana);
      await form.getByRole('radio', { name: 'Ajustar o ledger com lançamento compensatório' }).check();
      await form.getByLabel(/^Justificativa/).fill('curta');
      await form.getByRole('button', { name: 'Continuar' }).click();
      await expect(form).toContainText('A justificativa deve ter pelo menos 10 caracteres.');
      await form.getByLabel(/^Justificativa/).fill(justification);
      await form.getByRole('button', { name: 'Continuar' }).click();
      await page.getByRole('dialog', { name: 'Confirmar resolução' }).getByRole('button', { name: 'Resolver divergência' }).click();
      await expect(page.getByText('Divergência resolvida.').first()).toBeVisible({ timeout: 30_000 });
    });

    await test.step('a resolução fica registrada com justificativa e histórico', async () => {
      await page.reload();
      await expect(page.getByText(`Justificativa: ${justification}`)).toBeVisible();
      await expect(page.getByText('Resolução: Ajustar o ledger com lançamento compensatório')).toBeVisible();
      await expect(page.locator('code', { hasText: 'reconciliation.discrepancy_resolved' })).toBeVisible();
      await expect(page.locator('code', { hasText: 'reconciliation.discrepancy_detected' })).toBeVisible();
      await expect(page.getByRole('form', { name: 'Resolver divergência' })).toHaveCount(0);
    });

    await test.step('nova execução não reabre a divergência resolvida', async () => {
      await page.goto('/conciliacao');
      await page.getByRole('button', { name: 'Executar conciliação agora' }).click();
      await page.getByRole('dialog', { name: 'Executar conciliação agora' }).getByRole('button', { name: 'Executar conciliação' }).click();
      await expect(page.getByText(/^Conciliação concluída: /).first()).toBeVisible({ timeout: 60_000 });
      await page.getByRole('tab', { name: 'Divergências' }).click();
      await expect(
        page.getByRole('table', { name: 'Divergências' }).getByRole('row').filter({ hasText: providerChargeId }).filter({ hasText: 'Taxa divergente' }),
      ).toHaveCount(0);
    });
  });
});
