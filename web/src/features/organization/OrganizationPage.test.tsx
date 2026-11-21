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
    expect(await req.json()).toMatchObject({ name: 'Acme Sistemas', document_number: '11222333000181', invoice_prefix: 'ACME' });
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
