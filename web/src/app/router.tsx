import { createBrowserRouter, type RouteObject } from 'react-router';
import { AcceptInvitationPage } from '../features/auth/AcceptInvitationPage';
import { LoginPage } from '../features/auth/LoginPage';
import { OnboardingPage } from '../features/auth/OnboardingPage';
import { SelectOrganizationPage } from '../features/auth/SelectOrganizationPage';
import { AppLayout } from './AppLayout';
import { RouteErrorBoundary } from './RouteErrorBoundary';
import { NotFoundPage, RequireTenant, SessionGate } from './SessionGate';

/** Rotas da área interna (dentro do AppShell, com organização ativa). */
export const appRoutes: RouteObject[] = [
  { index: true, lazy: async () => ({ Component: (await import('../features/dashboard/DashboardPage')).DashboardPage }) },
  { path: '*', Component: NotFoundPage },
];

export const routes: RouteObject[] = [
  { path: '/entrar', Component: LoginPage, ErrorBoundary: RouteErrorBoundary },
  {
    element: <SessionGate />,
    ErrorBoundary: RouteErrorBoundary,
    children: [
      { path: '/organizacoes', Component: SelectOrganizationPage },
      { path: '/organizacoes/nova', Component: OnboardingPage },
      { path: '/convite', Component: AcceptInvitationPage },
      {
        path: '/',
        element: (
          <RequireTenant>
            <AppLayout />
          </RequireTenant>
        ),
        ErrorBoundary: RouteErrorBoundary,
        children: appRoutes,
      },
    ],
  },
];

export function createAppRouter() {
  return createBrowserRouter(routes);
}
