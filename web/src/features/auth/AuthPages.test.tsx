import { screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { Session } from '../../api/types';
import { makeSession, TENANT_A, TENANT_B } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { AcceptInvitationPage } from './AcceptInvitationPage';
import { OnboardingPage, slugify } from './OnboardingPage';
import { SelectOrganizationPage } from './SelectOrganizationPage';

// Formulário com a lista completa de fusos IANA: sob carga, a renderização e
// a digitação passam do limite padrão de 15 s.
const FORM_TEST_TIMEOUT = 40_000;

const TOKEN = 'abcdefghijklmnopqrstuvwxyz0123456789';

function problem(status: number, code: string, detail: string, errors?: { field: string; message: string }[]) {
  return HttpResponse.json(
    { type: 'about:blank', title: 'Erro', status, code, detail, ...(errors ? { errors } : {}) },
    { status, headers: { 'Content-Type': 'application/problem+json' } },
  );
}

const noOrganizations = (): Session => ({ ...makeSession({ tenantId: null }), organizations: [] });

describe('onboarding', () => {
  it('gera o slug pelo nome, cria a organização sem header de tenant e segue para o painel', async () => {
    const created = makeSession({ tenantId: TENANT_A });
    const requests: Request[] = [];
    server.use(
      http.post('/api/v1/organizations', ({ request }) => {
        requests.push(request.clone());
        return HttpResponse.json(created, { status: 201 });
      }),
      http.get('/api/v1/session', () => HttpResponse.json(created)),
    );
    const { user } = renderPage(<OnboardingPage />, { path: '/organizacoes/nova', session: noOrganizations() });
    expect(screen.getByText(/Você ainda não participa de nenhuma organização/)).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Voltar' })).not.toBeInTheDocument();

    await user.click(screen.getByLabelText(/Nome da organização/));
    await user.paste('Padaria São João');
    expect(screen.getByLabelText(/Identificador/)).toHaveValue('padaria-sao-joao');
    await user.click(screen.getByLabelText(/Prefixo da numeração/));
    await user.paste('pad');
    await user.click(screen.getByRole('button', { name: 'Criar organização' }));

    expect(await screen.findByText('Outra rota')).toBeInTheDocument();
    const [req] = requests;
    expect(req.headers.get('X-Recorvia-Tenant')).toBeNull();
    expect(req.headers.get('X-CSRF-Token')).toBe('csrf-123');
    expect(await req.json()).toMatchObject({ name: 'Padaria São João', slug: 'padaria-sao-joao', timezone: 'America/Sao_Paulo', invoice_prefix: 'PAD' });
  }, FORM_TEST_TIMEOUT);

  it('valida o formulário e mostra no campo o erro devolvido pelo servidor', async () => {
    server.use(
      http.post('/api/v1/organizations', () =>
        problem(422, 'validation_failed', 'Revise os campos.', [{ field: 'slug', message: 'Este identificador já está em uso.' }]),
      ),
    );
    const { user } = renderPage(<OnboardingPage />, { path: '/organizacoes/nova' });
    expect(screen.getByRole('link', { name: 'Voltar' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Criar organização' }));
    expect(await screen.findByText('O nome deve ter pelo menos 2 caracteres.')).toBeInTheDocument();

    await user.click(screen.getByLabelText(/Nome da organização/));
    await user.paste('Acme');
    await user.click(screen.getByRole('button', { name: 'Criar organização' }));
    expect(await screen.findByText('Este identificador já está em uso.')).toBeInTheDocument();
    expect(screen.getByLabelText(/Identificador/)).toHaveAttribute('aria-invalid', 'true');
  }, FORM_TEST_TIMEOUT);

  it('slugify remove acentos e caracteres fora do padrão', () => {
    expect(slugify('  Árvore & Cia. Ltda  ')).toBe('arvore-cia-ltda');
  });
});

describe('aceite de convite', () => {
  it('aceita o convite, troca para a organização do convite e segue para o painel', async () => {
    const accepts: Request[] = [];
    const switches: unknown[] = [];
    const after = makeSession({ tenantId: TENANT_B });
    server.use(
      http.post('/api/v1/invitations/accept', ({ request }) => {
        accepts.push(request.clone());
        return HttpResponse.json({ tenant_id: TENANT_B, role: 'finance' });
      }),
      http.put('/api/v1/session/tenant', async ({ request }) => {
        switches.push(await request.json());
        return HttpResponse.json(after);
      }),
      http.get('/api/v1/session', () => HttpResponse.json(after)),
    );
    const { user } = renderPage(<AcceptInvitationPage />, { path: '/convite', route: `/convite?token=${TOKEN}` });
    expect(screen.getByText(/Conectado como ana.owner@acme.test/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Aceitar convite' }));

    expect(await screen.findByText('Outra rota')).toBeInTheDocument();
    expect(await accepts[0].json()).toEqual({ token: TOKEN });
    expect(accepts[0].headers.get('X-Recorvia-Tenant')).toBeNull();
    expect(switches).toEqual([{ tenant_id: TENANT_B }]);
    expect(await screen.findByText('Convite aceito. Seu papel: Financeiro.')).toBeInTheDocument();
  });

  it('mostra o motivo da recusa e permanece na página', async () => {
    server.use(
      http.post('/api/v1/invitations/accept', () =>
        problem(403, 'platform_user_limit', 'O plano da plataforma atingiu o limite de usuários.'),
      ),
    );
    const { user } = renderPage(<AcceptInvitationPage />, { path: '/convite', route: `/convite?token=${TOKEN}` });
    await user.click(screen.getByRole('button', { name: 'Aceitar convite' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Não foi possível aceitar o convite');
    expect(alert).toHaveTextContent('O plano da plataforma atingiu o limite de usuários.');
    expect(screen.getByRole('button', { name: 'Aceitar convite' })).toBeEnabled();
  });

  it('link sem token válido não oferece o aceite', () => {
    renderPage(<AcceptInvitationPage />, { path: '/convite', route: '/convite?token=curto' });
    expect(screen.getByText('Link de convite inválido')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Aceitar convite' })).not.toBeInTheDocument();
  });
});

describe('seleção de organização', () => {
  it('lista as organizações, marca a atual e troca para outra', async () => {
    const switches: unknown[] = [];
    const after = makeSession({ tenantId: TENANT_B });
    server.use(
      http.put('/api/v1/session/tenant', async ({ request }) => {
        switches.push(await request.json());
        return HttpResponse.json(after);
      }),
      http.get('/api/v1/session', () => HttpResponse.json(after)),
    );
    const { user } = renderPage(<SelectOrganizationPage />, { path: '/organizacoes' });
    expect(screen.getByText('Acme Software')).toBeInTheDocument();
    expect(screen.getByText('Atual')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Continuar' })).toHaveAttribute('href', '/');
    expect(screen.queryByRole('link', { name: 'Atendimento de serviços' })).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Entrar em Beta Consultoria' }));
    expect(await screen.findByText('Outra rota')).toBeInTheDocument();
    expect(switches).toEqual([{ tenant_id: TENANT_B }]);
  });

  it('troca recusada mostra o erro e mantém a página', async () => {
    server.use(http.put('/api/v1/session/tenant', () => problem(403, 'no_membership', 'Você não tem acesso a esta organização.')));
    const { user } = renderPage(<SelectOrganizationPage />, { path: '/organizacoes' });
    await user.click(screen.getByRole('button', { name: 'Entrar em Beta Consultoria' }));
    expect(await screen.findByText('Você não tem acesso a esta organização.')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Entrar em Beta Consultoria' })).toBeEnabled());
  });

  it('sem organizações, leva ao onboarding', () => {
    renderPage(<SelectOrganizationPage />, { path: '/organizacoes', session: noOrganizations() });
    expect(screen.getByText('Outra rota')).toBeInTheDocument();
  });

  it('equipe da plataforma sem organização chega ao atendimento de serviços', () => {
    renderPage(<SelectOrganizationPage />, { path: '/organizacoes', session: { ...noOrganizations(), platform_staff: true } });
    expect(screen.getByText('Você ainda não participa de nenhuma organização.')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Atendimento de serviços' })).toHaveAttribute('href', '/admin/servicos');
    expect(screen.getByRole('link', { name: 'Criar nova organização' })).toBeInTheDocument();
  });
});
