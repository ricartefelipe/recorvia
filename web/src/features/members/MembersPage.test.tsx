import { screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { AUDITOR_PERMISSIONS, makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { MembersPage, RoleMatrix } from './MembersPage';

describe('usuários e permissões', () => {
  it('matriz mostra cada papel × permissão com rótulo acessível', () => {
    renderPage(
      <RoleMatrix
        roles={[
          { role: 'owner', permissions: ['customers.read', 'customers.write', 'audit.read'] },
          { role: 'auditor', permissions: ['customers.read', 'audit.read'] },
        ]}
      />,
    );
    const row = screen.getByRole('row', { name: /Cadastrar e alterar clientes/ });
    const cells = within(row).getAllByRole('img');
    expect(cells.map((c) => c.getAttribute('aria-label'))).toEqual(['Permitido', 'Não permitido']);
  });

  it('convite exibe o link de aceite uma única vez', async () => {
    server.use(
      http.get('/api/v1/members', () => HttpResponse.json({ data: [] })),
      http.get('/api/v1/invitations', () => HttpResponse.json({ data: [] })),
      http.post('/api/v1/invitations', async ({ request }) => {
        const body = (await request.json()) as { email: string; role: string };
        return HttpResponse.json(
          {
            invitation: { id: 'i1', email: body.email, role: body.role, state: 'pending', expires_at: '2026-11-23T12:00:00Z', created_at: '2026-11-16T12:00:00Z' },
            accept_url: 'https://app.recorvia.test/convite?token=abcdefghijklmnopqrstuvwxyz',
          },
          { status: 201 },
        );
      }),
    );
    const { user } = renderPage(<MembersPage />, { path: '/usuarios', route: '/usuarios?aba=convites' });
    await user.click(await screen.findByRole('button', { name: 'Convidar membro' }));
    await user.type(screen.getByLabelText(/E-mail/), 'nova.pessoa@acme.test');
    await user.click(screen.getByRole('button', { name: 'Criar convite' }));
    expect(await screen.findByText('Exibido uma única vez')).toBeInTheDocument();
    expect(screen.getByLabelText('Link de aceite')).toHaveValue('https://app.recorvia.test/convite?token=abcdefghijklmnopqrstuvwxyz');
    await user.click(screen.getByRole('button', { name: 'Já copiei, fechar' }));
    expect(screen.queryByLabelText('Link de aceite')).not.toBeInTheDocument();
  });

  it('auditor vê membros sem controles de gestão', async () => {
    server.use(
      http.get('/api/v1/members', () =>
        HttpResponse.json({
          data: [{ user_id: 'u2', email: 'bruno@acme.test', display_name: 'Bruno', role: 'finance', status: 'active', created_at: '2026-10-01T12:00:00Z', version: 1 }],
        }),
      ),
    );
    renderPage(<MembersPage />, { path: '/usuarios', session: makeSession({ permissions: AUDITOR_PERMISSIONS }) });
    expect(await screen.findByText('Bruno')).toBeInTheDocument();
    expect(screen.queryByRole('textbox', { name: /Papel de Bruno/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Remover Bruno/ })).not.toBeInTheDocument();
  });
});
