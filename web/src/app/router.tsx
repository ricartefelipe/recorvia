import { createBrowserRouter, Navigate, type RouteObject } from 'react-router';
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
  { path: 'clientes', lazy: async () => ({ Component: (await import('../features/customers/CustomersPage')).CustomersPage }) },
  { path: 'clientes/:id', lazy: async () => ({ Component: (await import('../features/customers/CustomerDetailPage')).CustomerDetailPage }) },
  {
    path: 'catalogo',
    lazy: async () => ({ Component: (await import('../features/catalog/CatalogLayout')).CatalogLayout }),
    children: [
      { index: true, element: <Navigate to="planos" replace /> },
      { path: 'planos', lazy: async () => ({ Component: (await import('../features/catalog/PlansPage')).PlansPage }) },
      { path: 'planos/:id', lazy: async () => ({ Component: (await import('../features/catalog/PlanDetailPage')).PlanDetailPage }) },
      { path: 'produtos', lazy: async () => ({ Component: (await import('../features/catalog/ProductsPage')).ProductsPage }) },
      { path: 'metricas', lazy: async () => ({ Component: (await import('../features/catalog/MetricsPage')).MetricsPage }) },
      { path: 'cupons', lazy: async () => ({ Component: (await import('../features/catalog/CouponsPage')).CouponsPage }) },
    ],
  },
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
