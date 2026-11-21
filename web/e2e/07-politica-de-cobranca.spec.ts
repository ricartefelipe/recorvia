import { expect, test } from '@playwright/test';
import { login, selectTenant, TENANTS, uniqueSuffix, USERS } from './helpers';

/**
 * Política de cobrança da Acme: a proprietária cria uma política com um
 * lembrete a mais que o padrão e suspensão por atraso, confere na tabela os
 * lembretes em ordem, o limite diário e a suspensão, e depois edita a
 * política (remove um lembrete e aumenta o limite diário). A política não é
 * marcada como padrão da organização, para não mudar a régua que os outros
 * roteiros usam; o nome é único por execução.
 */
test('(g) cria e edita uma política de cobrança', async ({ page }) => {
  const policyName = `Régua E2E ${uniqueSuffix()}`;
  await login(page, USERS.ana);
  await selectTenant(page, TENANTS.acme);

  const table = page.getByRole('table', { name: 'Políticas de cobrança' });
  const row = table.getByRole('row').filter({ hasText: policyName });

  await test.step('cria a política com lembrete extra e suspensão', async () => {
    await page.goto('/cobranca');
    await expect(page.getByRole('tab', { name: 'Políticas', exact: true })).toHaveAttribute('aria-selected', 'true');
    await page.getByRole('button', { name: 'Nova política' }).click();
    const dialog = page.getByRole('dialog', { name: 'Nova política de cobrança' });
    await dialog.getByLabel(/^Nome/).fill(policyName);
    // O padrão do formulário traz lembretes em -3, 0, 3 e 7 dias.
    await dialog.getByRole('button', { name: 'Adicionar lembrete' }).click();
    await dialog.getByLabel('Lembrete 5 (dias)').fill('15');
    // O input do interruptor do Mantine é transparente e cobre o controle
    // inteiro: check() clica nele e confere que ficou marcado.
    await dialog.getByRole('switch', { name: 'Suspender a assinatura após atraso' }).check();
    await dialog.getByLabel('Suspender após quantos dias de atraso').fill('45');
    await expect(dialog.getByRole('switch', { name: 'Política padrão da organização' })).not.toBeChecked();
    await dialog.getByRole('button', { name: 'Salvar política' }).click();
    await expect(page.getByText('Política criada.').first()).toBeVisible();
    await expect(dialog).toBeHidden();
  });

  await test.step('confere a política na tabela', async () => {
    await expect(row).toHaveCount(1);
    await expect(row).toContainText('3 dia(s) antes, no vencimento, 3 dia(s) depois, 7 dia(s) depois, 15 dia(s) depois');
    await expect(row).toContainText('8h–20h');
    await expect(row).toContainText('1 e-mail(s)');
    await expect(row).toContainText('Após 45 dia(s)');
    await expect(row.getByText('Padrão', { exact: true })).toHaveCount(0);
  });

  await test.step('edita a política', async () => {
    await row.getByRole('button', { name: `Editar política ${policyName}` }).click();
    const dialog = page.getByRole('dialog', { name: 'Editar política de cobrança' });
    await expect(dialog.getByLabel(/^Nome/)).toHaveValue(policyName);
    await dialog.getByRole('button', { name: 'Remover lembrete 1' }).click();
    await dialog.getByLabel('Máximo de e-mails por dia').fill('2');
    await dialog.getByRole('button', { name: 'Salvar política' }).click();
    await expect(page.getByText('Política atualizada.').first()).toBeVisible();
    await expect(dialog).toBeHidden();
    await expect(row).toContainText('no vencimento, 3 dia(s) depois, 7 dia(s) depois, 15 dia(s) depois');
    await expect(row).not.toContainText('3 dia(s) antes');
    await expect(row).toContainText('2 e-mail(s)');
    await expect(row).toContainText('Após 45 dia(s)');
  });
});
