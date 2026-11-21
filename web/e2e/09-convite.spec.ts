import { expect, type Page, test } from '@playwright/test';
import { chooseOption, login, selectTenant, TENANTS, USERS } from './helpers';

/**
 * Retira o convidado da Beta (membro ativo e convites pendentes), para que o
 * roteiro possa ser repetido no mesmo ambiente: o aceite de quem já é membro
 * é recusado, e convites pendentes contam no limite de usuários do plano.
 */
async function removeInvitee(page: Page): Promise<void> {
  await page.goto('/usuarios');
  const members = page.getByRole('table', { name: 'Membros' });
  await expect(members).toBeVisible();
  const member = members.getByRole('row').filter({ hasText: USERS.gabi });
  if ((await member.count()) > 0) {
    await member.getByRole('button', { name: /^Remover / }).click();
    await page.getByRole('dialog', { name: 'Remover membro' }).getByRole('button', { name: 'Remover', exact: true }).click();
    await expect(page.getByText('Membro removido.').first()).toBeVisible();
    await expect(member).toHaveCount(0);
  }

  await page.goto('/usuarios?aba=convites');
  await expect(page.getByRole('table', { name: 'Convites' }).or(page.getByText('Nenhum convite', { exact: true }))).toBeVisible();
  const revoke = page.getByRole('button', { name: `Revogar convite de ${USERS.gabi}` });
  for (let pending = await revoke.count(); pending > 0; pending--) {
    await revoke.first().click();
    await page.getByRole('dialog', { name: 'Revogar convite' }).getByRole('button', { name: 'Revogar', exact: true }).click();
    await expect(revoke).toHaveCount(pending - 1);
  }
}

/**
 * Convite com aceite: a proprietária da Beta convida uma pessoa sem
 * organização com o papel Financeiro e lê o link de aceite exibido uma única
 * vez; a pessoa convidada entra com a própria conta, abre o link e aceita,
 * passando a trabalhar na Beta; a proprietária vê a nova integrante entre os
 * membros e o convite como aceito. A Beta recebe o convite porque a Acme já
 * tem os três usuários do plano Essencial.
 */
test('(i) convite criado pela proprietária e aceito pela pessoa convidada', async ({ browser }) => {
  const ownerContext = await browser.newContext();
  const owner = await ownerContext.newPage();
  await login(owner, USERS.carla);
  await selectTenant(owner, TENANTS.beta);

  await test.step('limpa convites e associação de execuções anteriores', async () => {
    await removeInvitee(owner);
  });

  let acceptPath = '';
  await test.step('cria o convite e lê o link de aceite', async () => {
    await owner.getByRole('button', { name: 'Convidar membro' }).click();
    const dialog = owner.getByRole('dialog', { name: 'Convidar membro' });
    await dialog.getByLabel(/^E-mail/).fill(USERS.gabi);
    await chooseOption(dialog, /^Papel/, 'Financeiro');
    await dialog.getByRole('button', { name: 'Criar convite' }).click();

    const reveal = owner.getByRole('dialog', { name: 'Link do convite' });
    await expect(reveal).toContainText(USERS.gabi);
    const link = await reveal.getByRole('textbox', { name: 'Link de aceite', exact: true }).inputValue();
    expect(link).toMatch(/\/convite\?token=[A-Za-z0-9_-]{43}$/);
    // O link pode ser absoluto (RECORVIA_PUBLIC_URL); o roteiro usa o endereço do ambiente em teste.
    const parsed = new URL(link, owner.url());
    acceptPath = parsed.pathname + parsed.search;
    await reveal.getByRole('button', { name: 'Já copiei, fechar' }).click();
    await expect(reveal).toBeHidden();

    const latest = owner.getByRole('table', { name: 'Convites' }).getByRole('row').filter({ hasText: USERS.gabi }).first();
    await expect(latest).toContainText('Financeiro');
    await expect(latest).toContainText('Pendente');
  });

  await test.step('a pessoa convidada entra e aceita o convite', async () => {
    const inviteeContext = await browser.newContext();
    const invitee = await inviteeContext.newPage();
    await login(invitee, USERS.gabi);
    await invitee.goto(acceptPath);
    await expect(invitee.getByRole('heading', { name: 'Convite para organização' })).toBeVisible();
    await expect(invitee.getByText(`Conectado como ${USERS.gabi}.`)).toBeVisible();
    await invitee.getByRole('button', { name: 'Aceitar convite' }).click();
    await expect(invitee.getByText('Convite aceito. Seu papel: Financeiro.').first()).toBeVisible();
    await expect(invitee.getByRole('textbox', { name: 'Organização ativa' })).toHaveValue(TENANTS.beta, { timeout: 30_000 });
    await inviteeContext.close();
  });

  await test.step('a proprietária vê a nova integrante e o convite aceito', async () => {
    await owner.goto('/usuarios');
    const member = owner.getByRole('table', { name: 'Membros' }).getByRole('row').filter({ hasText: USERS.gabi });
    await expect(member).toHaveCount(1);
    await expect(member.getByRole('textbox', { name: /^Papel de / })).toHaveValue('Financeiro');
    await expect(member).toContainText('Ativo');

    await owner.goto('/usuarios?aba=convites');
    const latest = owner.getByRole('table', { name: 'Convites' }).getByRole('row').filter({ hasText: USERS.gabi }).first();
    await expect(latest).toContainText('Aceito');
  });

  await ownerContext.close();
});
