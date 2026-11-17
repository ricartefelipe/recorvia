import { expect, test } from '@playwright/test';
import { login, selectTenant, TENANTS, USERS } from './helpers';

test('(d) auditor da Acme não vê botões de escrita', async ({ page }) => {
  await login(page, USERS.davi);
  await selectTenant(page, TENANTS.acme);

  await test.step('clientes', async () => {
    await page.goto('/clientes');
    await expect(page.getByRole('heading', { name: 'Clientes' })).toBeVisible();
    await expect(page.getByRole('table', { name: 'Clientes' }).or(page.getByText('Nenhum cliente cadastrado'))).toBeVisible();
    await expect(page.getByRole('button', { name: 'Novo cliente' })).toHaveCount(0);
    const first = page.getByRole('table', { name: 'Clientes' }).getByRole('link').first();
    if (await first.isVisible()) {
      await first.click();
      await expect(page.getByRole('tab', { name: 'Dados' })).toBeVisible();
      await expect(page.getByRole('button', { name: 'Editar' })).toHaveCount(0);
      await expect(page.getByRole('button', { name: 'Arquivar' })).toHaveCount(0);
      await expect(page.getByRole('button', { name: 'Salvar endereço' })).toHaveCount(0);
    }
  });

  await test.step('catálogo e assinaturas', async () => {
    await page.goto('/catalogo/planos');
    await expect(page.getByRole('tab', { name: 'Planos e preços' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Novo plano' })).toHaveCount(0);
    await page.goto('/assinaturas');
    await expect(page.getByRole('heading', { name: 'Assinaturas' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Nova assinatura' })).toHaveCount(0);
  });

  await test.step('faturas', async () => {
    await page.goto('/faturas');
    await expect(page.getByRole('heading', { name: 'Faturas' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Executar faturamento agora' })).toHaveCount(0);
    const first = page.getByRole('table', { name: 'Faturas' }).getByRole('link').first();
    if (await first.isVisible()) {
      await first.click();
      await expect(page.getByRole('table', { name: 'Totais' })).toBeVisible();
      await expect(page.getByRole('button', { name: 'Abrir documento' })).toBeVisible();
      await expect(page.getByRole('button', { name: 'Ações' })).toHaveCount(0);
      await expect(page.getByRole('button', { name: 'Reembolsar' })).toHaveCount(0);
    }
  });

  await test.step('usuários e integrações', async () => {
    await page.goto('/usuarios?aba=convites');
    await expect(page.getByRole('heading', { name: 'Usuários e permissões' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Convidar membro' })).toHaveCount(0);
    await page.goto('/integracoes?aba=credenciais');
    await expect(page.getByRole('button', { name: 'Nova credencial' })).toHaveCount(0);
  });

  await test.step('auditoria é acessível ao auditor', async () => {
    await page.goto('/auditoria');
    await expect(page.getByRole('heading', { name: 'Auditoria' })).toBeVisible();
    await expect(page.getByText('Acesso negado')).toHaveCount(0);
  });
});
