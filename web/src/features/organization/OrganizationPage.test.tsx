import { screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { Organization } from '../../api/types';
import { AUDITOR_PERMISSIONS, makeSession, TENANT_A } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { OrganizationPage } from './OrganizationPage';

// Formulário com a lista completa de fusos IANA: sob carga, a renderização e
// a digitação passam do limite padrão de 15 s.
const FORM_TEST_TIMEOUT = 40_000;

const org: Organization = {
  id: TENANT_A,
  name: 'Acme Software',
  slug: 'acme',
  legal_name: 'Acme Software Ltda',
  document_number: '11222333000181',
  timezone: 'America/Sao_Paulo',
  currency: 'BRL',
  invoice_prefix: 'ACME',
  payment_terms_days: 10,
  address: null,
  status: 'active',
  created_at: '2026-10-01T12:00:00Z',
  version: 3,
};

describe('organização', () => {
  it('mostra os dados e salva com If-Match e a organização ativa', async () => {
    const patches: Request[] = [];
    server.use(
      http.get('/api/v1/organization', () => HttpResponse.json(org)),
      http.patch('/api/v1/organization', async ({ request }) => {
        patches.push(request.clone());
        return HttpResponse.json({ ...org, name: 'Acme Sistemas', version: 4 });
      }),
      http.get('/api/v1/session', () => HttpResponse.json(makeSession())),
    );
    const { user } = renderPage(<OrganizationPage />, { path: '/organizacao' });
    expect(await screen.findByText('acme')).toBeInTheDocument();
    expect(screen.getByLabelText(/^Nome/)).toHaveValue('Acme Software');
    expect(screen.getByLabelText(/CNPJ/)).toHaveValue('11.222.333/0001-81');

    await user.clear(screen.getByLabelText(/^Nome/));
    await user.click(screen.getByLabelText(/^Nome/));
    await user.paste('Acme Sistemas');
    await user.click(screen.getByRole('button', { name: 'Salvar' }));

    expect(await screen.findByText('Dados da organização atualizados.')).toBeInTheDocument();
    const [req] = patches;
    expect(req.headers.get('If-Match')).toBe('"3"');
    expect(req.headers.get('X-Recorvia-Tenant')).toBe(TENANT_A);
    const body = await req.json();
    expect(body).toMatchObject({ name: 'Acme Sistemas', document_number: '11222333000181', invoice_prefix: 'ACME' });
    // Sem endereço cadastrado e com os campos em branco, nada é enviado.
    expect(body).not.toHaveProperty('address');
  }, FORM_TEST_TIMEOUT);

  it('cadastra o endereço da organização no formato do endereço de cobrança', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.get('/api/v1/organization', () => HttpResponse.json(org)),
      http.patch('/api/v1/organization', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ ...org, version: 4 });
      }),
      http.get('/api/v1/session', () => HttpResponse.json(makeSession())),
    );
    const { user } = renderPage(<OrganizationPage />, { path: '/organizacao' });
    expect(await screen.findByRole('heading', { name: 'Endereço da organização' })).toBeInTheDocument();
    await user.type(screen.getByLabelText('Logradouro'), 'Av. Paulista');
    await user.type(screen.getByLabelText('Número'), '1000');
    await user.type(screen.getByLabelText('Complemento'), '10º andar');
    await user.type(screen.getByLabelText('Bairro'), 'Bela Vista');
    await user.type(screen.getByLabelText('Cidade'), 'São Paulo');
    await user.click(screen.getByRole('textbox', { name: 'UF' }));
    await user.click(await screen.findByRole('option', { name: 'SP' }));
    await user.type(screen.getByLabelText('CEP'), '01310100');
    await user.click(screen.getByRole('button', { name: 'Salvar' }));

    expect(await screen.findByText('Dados da organização atualizados.')).toBeInTheDocument();
    expect(bodies[0]).toMatchObject({
      address: { street: 'Av. Paulista', number: '1000', complement: '10º andar', district: 'Bela Vista', city: 'São Paulo', state: 'SP', postal_code: '01310-100', country: 'BR' },
    });
  }, FORM_TEST_TIMEOUT);

  it('endereço incompleto aponta os campos e não envia', async () => {
    let calls = 0;
    server.use(
      http.get('/api/v1/organization', () => HttpResponse.json(org)),
      http.patch('/api/v1/organization', () => {
        calls++;
        return HttpResponse.json(org);
      }),
    );
    const { user } = renderPage(<OrganizationPage />, { path: '/organizacao' });
    await user.type(await screen.findByLabelText('Logradouro'), 'Av. Paulista');
    await user.click(screen.getByRole('button', { name: 'Salvar' }));
    expect(await screen.findByText('O número é obrigatório.')).toBeInTheDocument();
    expect(screen.getByText('Selecione a UF.')).toBeInTheDocument();
    expect(screen.getByText('Informe o CEP no formato 00000-000.')).toBeInTheDocument();
    expect(calls).toBe(0);
  }, FORM_TEST_TIMEOUT);

  it('mostra o endereço cadastrado e o reenvia ao salvar; apagá-lo não é aceito', async () => {
    const bodies: unknown[] = [];
    const registered: Organization = {
      ...org,
      address: { street: 'Av. Paulista', number: '1000', district: 'Bela Vista', city: 'São Paulo', state: 'SP', postal_code: '01310100', country: 'BR' },
    };
    server.use(
      http.get('/api/v1/organization', () => HttpResponse.json(registered)),
      http.patch('/api/v1/organization', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ ...registered, version: 4 });
      }),
      http.get('/api/v1/session', () => HttpResponse.json(makeSession())),
    );
    const { user } = renderPage(<OrganizationPage />, { path: '/organizacao' });
    expect(await screen.findByLabelText(/^Logradouro/)).toHaveValue('Av. Paulista');
    expect(screen.getByLabelText(/^CEP/)).toHaveValue('01310-100');
    expect(screen.getByRole('textbox', { name: /^UF/ })).toHaveValue('SP');

    await user.clear(screen.getByLabelText(/^Logradouro/));
    await user.click(screen.getByRole('button', { name: 'Salvar' }));
    expect(await screen.findByText('O logradouro é obrigatório.')).toBeInTheDocument();
    expect(bodies).toHaveLength(0);

    await user.type(screen.getByLabelText(/^Logradouro/), 'Rua Nova');
    await user.click(screen.getByRole('button', { name: 'Salvar' }));
    expect(await screen.findByText('Dados da organização atualizados.')).toBeInTheDocument();
    expect(bodies[0]).toMatchObject({ address: { street: 'Rua Nova', number: '1000', state: 'SP', postal_code: '01310-100' } });
  }, FORM_TEST_TIMEOUT);

  it('valida o prefixo antes de enviar', async () => {
    let calls = 0;
    server.use(
      http.get('/api/v1/organization', () => HttpResponse.json(org)),
      http.patch('/api/v1/organization', () => {
        calls++;
        return HttpResponse.json(org);
      }),
    );
    const { user } = renderPage(<OrganizationPage />, { path: '/organizacao' });
    const prefix = await screen.findByLabelText(/Prefixo da numeração/);
    await user.clear(prefix);
    await user.click(screen.getByRole('button', { name: 'Salvar' }));
    expect(await screen.findByText('Use até 10 letras maiúsculas ou números.')).toBeInTheDocument();
    expect(calls).toBe(0);
  }, FORM_TEST_TIMEOUT);

  it('sem permissão de alteração, o formulário fica somente leitura', async () => {
    server.use(http.get('/api/v1/organization', () => HttpResponse.json(org)));
    renderPage(<OrganizationPage />, { path: '/organizacao', session: makeSession({ permissions: AUDITOR_PERMISSIONS }) });
    expect(await screen.findByLabelText(/^Nome/)).toBeDisabled();
    expect(screen.queryByRole('button', { name: 'Salvar' })).not.toBeInTheDocument();
  });
});
