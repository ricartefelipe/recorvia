import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { makeSession } from '../test/fixtures';
import { renderPage } from '../test/render';
import { server } from '../test/server';
import { logoutAll } from '../session/sessionApi';
import { UserMenu } from './AppLayout';

const END_SESSION = 'https://idp.test/logout?client_id=recorvia-web&post_logout_redirect_uri=https%3A%2F%2Fapp.test%2F';

describe('menu do usuário', () => {
  it('encerra todas as sessões somente depois da confirmação e segue para o provedor', async () => {
    const requests: Request[] = [];
    server.use(
      http.post('/auth/logout-all', ({ request }) => {
        requests.push(request.clone());
        return HttpResponse.json({ redirect_url: END_SESSION });
      }),
    );
    const assign = vi.fn();
    const { user } = renderPage(<UserMenu assign={assign} />);
    await user.click(screen.getByRole('button', { name: 'Menu do usuário Ana Owner' }));
    expect(await screen.findByRole('menuitem', { name: 'Organizações' })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: 'Atendimento de serviços' })).not.toBeInTheDocument();

    await user.click(screen.getByRole('menuitem', { name: 'Encerrar todas as sessões' }));
    let dialog = await screen.findByRole('dialog', { name: 'Encerrar todas as sessões' });
    expect(dialog).toHaveTextContent('em todos os navegadores e dispositivos');
    await user.click(within(dialog).getByRole('button', { name: 'Cancelar' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(requests).toHaveLength(0);

    await user.click(screen.getByRole('button', { name: 'Menu do usuário Ana Owner' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Encerrar todas as sessões' }));
    dialog = await screen.findByRole('dialog', { name: 'Encerrar todas as sessões' });
    await user.click(within(dialog).getByRole('button', { name: 'Encerrar todas' }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(END_SESSION));
    expect(requests).toHaveLength(1);
    expect(requests[0].headers.get('X-CSRF-Token')).toBe('csrf-123');
    expect(requests[0].headers.get('X-Recorvia-Tenant')).toBeNull();
  });

  it('sair encerra apenas a sessão atual', async () => {
    const paths: string[] = [];
    server.use(
      http.post('/auth/logout', ({ request }) => {
        paths.push(new URL(request.url).pathname);
        return HttpResponse.json({ redirect_url: END_SESSION });
      }),
    );
    const assign = vi.fn();
    const { user } = renderPage(<UserMenu assign={assign} />);
    await user.click(screen.getByRole('button', { name: 'Menu do usuário Ana Owner' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Sair' }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(END_SESSION));
    expect(paths).toEqual(['/auth/logout']);
  });

  it('a equipe da plataforma vê o atendimento de serviços', async () => {
    const { user } = renderPage(<UserMenu />, { session: { ...makeSession(), platform_staff: true } });
    await user.click(screen.getByRole('button', { name: 'Menu do usuário Ana Owner' }));
    expect(await screen.findByRole('menuitem', { name: 'Atendimento de serviços' })).toHaveAttribute('href', '/admin/servicos');
  });

  it('logoutAll usa a entrada quando o backend não devolve endereço', async () => {
    server.use(http.post('/auth/logout-all', () => HttpResponse.json({ redirect_url: '' })));
    const assign = vi.fn();
    await logoutAll(assign);
    expect(assign).toHaveBeenCalledWith('/entrar');
  });
});
