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
  { path: 'assinaturas', lazy: async () => ({ Component: (await import('../features/subscriptions/SubscriptionsPage')).SubscriptionsPage }) },
  {
    path: 'assinaturas/:id',
    lazy: async () => ({ Component: (await import('../features/subscriptions/SubscriptionDetailPage')).SubscriptionDetailPage }),
  },
  { path: 'faturas', lazy: async () => ({ Component: (await import('../features/invoices/InvoicesPage')).InvoicesPage }) },
  { path: 'faturas/:id', lazy: async () => ({ Component: (await import('../features/invoices/InvoiceDetailPage')).InvoiceDetailPage }) },
  { path: 'cobrancas', lazy: async () => ({ Component: (await import('../features/payments/ChargesPage')).ChargesPage }) },
  { path: 'cobrancas/:id', lazy: async () => ({ Component: (await import('../features/payments/ChargesPage')).ChargeDetailPage }) },
  { path: 'recebimentos', lazy: async () => ({ Component: (await import('../features/payments/PaymentListsPages')).PaymentsPage }) },
  { path: 'reembolsos', lazy: async () => ({ Component: (await import('../features/payments/PaymentListsPages')).RefundsPage }) },
  { path: 'disputas', lazy: async () => ({ Component: (await import('../features/payments/PaymentListsPages')).DisputesPage }) },
  {
    path: 'operacoes-pagamento',
    lazy: async () => ({ Component: (await import('../features/payments/PaymentListsPages')).PaymentOperationsPage }),
  },
  { path: 'ledger', lazy: async () => ({ Component: (await import('../features/ledger/LedgerPage')).LedgerPage }) },
  { path: 'conciliacao', lazy: async () => ({ Component: (await import('../features/reconciliation/ReconciliationPages')).ReconciliationPage }) },
  {
    path: 'conciliacao/divergencias/:id',
    lazy: async () => ({ Component: (await import('../features/reconciliation/ReconciliationPages')).DiscrepancyDetailPage }),
  },
  { path: 'cobranca', lazy: async () => ({ Component: (await import('../features/collections/CollectionsPage')).CollectionsPage }) },
  { path: 'usuarios', lazy: async () => ({ Component: (await import('../features/members/MembersPage')).MembersPage }) },
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
