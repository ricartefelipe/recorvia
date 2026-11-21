import { expect, test } from '@playwright/test';
import { chooseOption, login, selectTenant, TENANTS, uniqueSuffix, USERS } from './helpers';

/**
 * Cadastro de endpoint de webhook na Beta: os webhooks enviados exigem o
 * recurso outbound_webhooks, do plano Profissional, e as organizações do seed
 * começam no Essencial. A proprietária troca o plano do Recorvia (se ainda não
 * for o Profissional), cadastra o endpoint com um tipo de evento, vê o segredo
 * de assinatura exibido uma única vez, confere o endpoint na lista e o remove
 * no fim. O cadastro resolve o host do destino e recusa redes privadas fora
 * das permitidas (RECORVIA_WEBHOOK_ALLOWED_PRIVATE_CIDRS, que no ambiente
 * local cobre a rede do Docker Compose): a URL aponta para o simulador de
 * pagamentos, que resolve dentro do ambiente, num caminho único por execução
 * que não trata entregas.
 */
test('(h) cadastra e remove um endpoint de webhook', async ({ page }) => {
  const url = `http://paysim:8090/e2e-webhooks/${uniqueSuffix()}`;
  await login(page, USERS.carla);
  await selectTenant(page, TENANTS.beta);

  await test.step('garante o plano Profissional', async () => {
    await page.goto('/plataforma');
    await expect(page.getByRole('heading', { name: 'Plano Recorvia' })).toBeVisible();
    const professional = page.getByRole('region', { name: 'Plano Profissional', exact: true });
    await expect(professional).toBeVisible();
    if ((await professional.getByText('Plano atual', { exact: true }).count()) === 0) {
      await professional.getByRole('button', { name: 'Mudar para este plano' }).click();
      await page.getByRole('dialog', { name: 'Trocar plano do Recorvia' }).getByRole('button', { name: 'Trocar plano' }).click();
      await expect(page.getByText('Plano do Recorvia alterado para Profissional.').first()).toBeVisible();
    }
    await expect(professional.getByText('Plano atual', { exact: true })).toBeVisible();
  });

  await test.step('cadastra o endpoint e vê o segredo uma única vez', async () => {
    await page.goto('/integracoes?aba=webhooks');
    await expect(page.getByRole('tab', { name: 'Webhooks enviados' })).toHaveAttribute('aria-selected', 'true');
    await page.getByRole('button', { name: 'Novo endpoint' }).click();
    const dialog = page.getByRole('dialog', { name: 'Novo endpoint de webhook' });
    await dialog.getByLabel(/^URL/).fill(url);
    await dialog.getByLabel('Descrição').fill('ERP financeiro (roteiro E2E)');
    await chooseOption(dialog, /^Tipos de evento/, /^invoice\.paid$/);
    // A lista de opções continua aberta depois da escolha; o clique no campo
    // acima dela a fecha sem cobrir o botão de salvar.
    await dialog.getByLabel(/^URL/).click();
    await expect(page.getByRole('option', { name: /^invoice\.paid$/ })).toBeHidden();
    await dialog.getByRole('button', { name: 'Salvar' }).click();
    await expect(page.getByText('Endpoint criado.').first()).toBeVisible();

    const secret = page.getByRole('dialog', { name: 'Segredo de assinatura do webhook' });
    await expect(secret).toContainText(url);
    await expect(secret.getByRole('textbox', { name: 'Segredo', exact: true })).not.toHaveValue('');
    await secret.getByRole('button', { name: 'Já copiei, fechar' }).click();
    await expect(secret).toBeHidden();
  });

  const row = page.getByRole('table', { name: 'Endpoints de webhook' }).getByRole('row').filter({ hasText: url });

  await test.step('confere o endpoint na lista', async () => {
    await expect(row).toHaveCount(1);
    await expect(row).toContainText('ERP financeiro (roteiro E2E)');
    await expect(row).toContainText('1 tipo(s)');
    await expect(row).toContainText('1 ativo(s)');
    await expect(row.getByRole('link', { name: `Ver entregas de ${url}` })).toBeVisible();
  });

  await test.step('remove o endpoint', async () => {
    await row.getByRole('button', { name: `Remover endpoint ${url}` }).click();
    await page.getByRole('dialog', { name: 'Remover endpoint' }).getByRole('button', { name: 'Remover' }).click();
    await expect(page.getByText('Endpoint removido.').first()).toBeVisible();
    await expect(row).toHaveCount(0);
  });
});
