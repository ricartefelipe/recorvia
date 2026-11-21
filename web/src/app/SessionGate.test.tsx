import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { makeSession } from '../test/fixtures';
import { renderPage } from '../test/render';
import { RequireTenant } from './SessionGate';

describe('área interna sem organização', () => {
  it('leva quem não tem organização ao onboarding', async () => {
    const session = { ...makeSession({ tenantId: null }), organizations: [] };
    const { router } = renderPage(
      <RequireTenant>
        <div>Painel</div>
      </RequireTenant>,
      { session },
    );
    expect(await screen.findByText('Outra rota')).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/organizacoes/nova');
  });

  it('leva a equipe da plataforma sem organização ao atendimento de serviços', async () => {
    const session = { ...makeSession({ tenantId: null }), organizations: [], platform_staff: true };
    const { router } = renderPage(
      <RequireTenant>
        <div>Painel</div>
      </RequireTenant>,
      { session },
    );
    expect(await screen.findByText('Outra rota')).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/admin/servicos');
    expect(screen.queryByText('Painel')).not.toBeInTheDocument();
  });

  it('com organização ativa mostra a área interna', async () => {
    renderPage(
      <RequireTenant>
        <div>Painel</div>
      </RequireTenant>,
    );
    expect(await screen.findByText('Painel')).toBeInTheDocument();
  });
});
