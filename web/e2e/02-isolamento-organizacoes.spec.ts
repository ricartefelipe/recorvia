import { expect, test } from '@playwright/test';
import { login, selectTenant, TENANTS, USERS } from './helpers';

test('(c) Beta Consultoria não vê clientes nem faturas da Acme', async ({ browser }) => {
  // Coleta, como Acme, nomes de clientes e uma fatura existente.
  const acmeContext = await browser.newContext();
  const acme = await acmeContext.newPage();
  await login(acme, USERS.ana);
  await selectTenant(acme, TENANTS.acme);
  await acme.goto('/clientes');
  const acmeCustomers = acme.getByRole('table', { name: 'Clientes' }).getByRole('link');
  await expect(acmeCustomers.first(), 'a Acme precisa ter ao menos um cliente (rode o seed ou o roteiro 01)').toBeVisible();
  const customerNames = (await acmeCustomers.allTextContents()).slice(0, 5);
  await acme.goto('/faturas');
  const invoiceLink = acme.getByRole('table', { name: 'Faturas' }).getByRole('link').first();
  await expect(invoiceLink, 'a Acme precisa ter ao menos uma fatura').toBeVisible();
  const acmeInvoicePath = (await invoiceLink.getAttribute('href')) ?? '';
  expect(acmeInvoicePath).toMatch(/^\/faturas\/[0-9a-f-]{36}$/);
  await acmeContext.close();

  // Como Beta, nada disso aparece.
  const betaContext = await browser.newContext();
  const beta = await betaContext.newPage();
  await login(beta, USERS.carla);
  await selectTenant(beta, TENANTS.beta);

  await beta.goto('/clientes');
  await expect(beta.getByRole('table', { name: 'Clientes' }).or(beta.getByText('Nenhum cliente cadastrado'))).toBeVisible();
  for (const name of customerNames) {
    await expect(beta.getByRole('table', { name: 'Clientes' }).getByRole('link', { name, exact: true })).toHaveCount(0);
    await beta.getByLabel('Buscar').fill(name);
    await expect(beta.getByText('Nenhum cliente encontrado')).toBeVisible();
  }

  await beta.goto(acmeInvoicePath);
  await expect(beta.getByText(/não encontrad[oa]/i).first()).toBeVisible();
  await betaContext.close();
});
