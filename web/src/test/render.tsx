import { QueryClient } from '@tanstack/react-query';
import { render } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ReactElement } from 'react';
import { createMemoryRouter, RouterProvider } from 'react-router';
import { applySessionToClient } from '../api/client';
import { SESSION_KEY } from '../api/queryKeys';
import type { Session } from '../api/types';
import { AppProviders } from '../app/providers';
import { SessionProvider } from '../session/SessionContext';
import { makeSession } from './fixtures';

export function createTestQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity }, mutations: { retry: false } },
  });
}

/** Renderiza uma página com provedores, sessão e roteador em memória. */
export function renderPage(
  ui: ReactElement,
  options: { session?: Session; path?: string; route?: string; queryClient?: QueryClient } = {},
) {
  const session = options.session ?? makeSession();
  const queryClient = options.queryClient ?? createTestQueryClient();
  queryClient.setQueryData(SESSION_KEY, session);
  applySessionToClient(session);
  const router = createMemoryRouter(
    [
      { path: options.path ?? '/', element: <SessionProvider session={session}>{ui}</SessionProvider> },
      { path: '*', element: <div>Outra rota</div> },
    ],
    { initialEntries: [options.route ?? options.path ?? '/'] },
  );
  const user = userEvent.setup();
  const result = render(
    <AppProviders queryClient={queryClient} env="test">
      <RouterProvider router={router} />
    </AppProviders>,
  );
  return { ...result, user, queryClient, router, session };
}
