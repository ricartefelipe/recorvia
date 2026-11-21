import { expect, test } from '@playwright/test';
import { chooseOption, login, uniqueSuffix } from './helpers';

/** Usuário de demonstração sem organização no seed (docs/roteiro-demo.md). */
const NEW_COMPANY_USER = 'nova.empresa@exemplo.test';

/**
 * Onboarding: um usuário sem organização cria a própria em
 * /organizacoes/nova (nome, slug gerado do nome, razão social, prefixo da
 * numeração e prazo de pagamento), cai no painel com a organização nova ativa,
 * confere os dados cadastrados na tela da organização e cadastra o endereço da
 * organização (emitente do documento da fatura). Cada execução cria
 * uma organização com nome e slug únicos; nas repetições o usuário já tem as
 * organizações anteriores e chega ao formulário pelo endereço direto.
 */
test('(f) usuário sem organização cria a sua e passa a trabalhar nela', async ({ page }) => {
  const suffix = uniqueSuffix();
  const name = `Nova Empresa E2E ${suffix}`;
  const slug = `nova-empresa-e2e-${suffix}`;
  const legalName = `Nova Empresa E2E ${suffix} Ltda`;

  await login(page, NEW_COMPANY_USER);

  await test.step('preenche o cadastro da organização', async () => {
    await page.goto('/organizacoes/nova');
    await expect(page.getByRole('heading', { name: 'Crie sua organização' })).toBeVisible();
    await page.getByLabel(/^Nome da organização/).fill(name);
    // O identificador acompanha o nome enquanto não for editado.
    await expect(page.getByLabel(/^Identificador \(slug\)/)).toHaveValue(slug);
    await page.getByLabel('Razão social').fill(legalName);
    await page.getByLabel(/^Prefixo da numeração de faturas/).fill('NOVA');
    await page.getByLabel('Prazo padrão de pagamento (dias)').fill('15');
    await page.getByRole('button', { name: 'Criar organização' }).click();
  });

  await test.step('cai no painel com a organização nova ativa', async () => {
    await expect(page.getByText(/criada\. O plano inicial está em período de avaliação\./).first()).toBeVisible();
    await expect(page.getByRole('textbox', { name: 'Organização ativa' })).toHaveValue(name, { timeout: 30_000 });
    await expect(page.getByRole('heading', { name: 'Painel financeiro' })).toBeVisible();
  });

  await test.step('confere os dados na tela da organização', async () => {
    await page.goto('/organizacao');
    await expect(page.getByRole('heading', { name: 'Organização', exact: true })).toBeVisible();
    await expect(page.locator('code', { hasText: slug })).toBeVisible();
    await expect(page.getByLabel(/^Nome/)).toHaveValue(name);
    await expect(page.getByLabel('Razão social')).toHaveValue(legalName);
    await expect(page.getByLabel(/^Prefixo da numeração de faturas/)).toHaveValue('NOVA');
    await expect(page.getByLabel('Prazo padrão de pagamento (dias)')).toHaveValue('15');
    // O criador é o proprietário: o formulário fica editável.
    await expect(page.getByRole('button', { name: 'Salvar' })).toBeVisible();
  });

  await test.step('cadastra o endereço da organização', async () => {
    await expect(page.getByRole('heading', { name: 'Endereço da organização' })).toBeVisible();
    await page.getByLabel(/^Logradouro/).fill('Av. Paulista');
    await page.getByLabel(/^Número/).fill('1000');
    await page.getByLabel('Complemento').fill('10º andar');
    await page.getByLabel(/^Bairro/).fill('Bela Vista');
    await page.getByLabel(/^Cidade/).fill('São Paulo');
    await chooseOption(page, /^UF/, 'SP');
    await page.getByLabel(/^CEP/).fill('01310-100');
    await page.getByRole('button', { name: 'Salvar' }).click();
    await expect(page.getByText('Dados da organização atualizados.').first()).toBeVisible();
    // Gravado na API e normalizado: a tela recarregada mostra o mesmo endereço.
    await page.reload();
    await expect(page.getByLabel(/^Logradouro/)).toHaveValue('Av. Paulista');
    await expect(page.getByLabel(/^Bairro/)).toHaveValue('Bela Vista');
    await expect(page.getByLabel(/^CEP/)).toHaveValue('01310-100');
  });
});
