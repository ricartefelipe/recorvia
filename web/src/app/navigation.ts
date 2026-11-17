import {
  IconAlertOctagon,
  IconBellRinging,
  IconBook2,
  IconBuilding,
  IconBuildingStore,
  IconCash,
  IconCreditCard,
  IconFileInvoice,
  IconGauge,
  IconGavel,
  IconHistory,
  IconPackage,
  IconPlugConnected,
  IconReceiptRefund,
  IconRepeat,
  IconScale,
  IconShieldLock,
  IconTool,
  IconTransfer,
  IconUsers,
  type Icon,
} from '@tabler/icons-react';
import type { Permission } from '../session/permissions';

export interface NavItem {
  label: string;
  to: string;
  icon: Icon;
  /** Basta uma das permissões para exibir o item. */
  anyOf: Permission[];
  end?: boolean;
}

export interface NavGroup {
  label: string;
  icon: Icon;
  children: NavItem[];
}

export type NavEntry = NavItem | NavGroup;

export function isGroup(entry: NavEntry): entry is NavGroup {
  return 'children' in entry;
}

export const NAVIGATION: NavEntry[] = [
  { label: 'Painel', to: '/', icon: IconGauge, anyOf: ['dashboard.read'], end: true },
  { label: 'Clientes', to: '/clientes', icon: IconUsers, anyOf: ['customers.read'] },
  { label: 'Catálogo', to: '/catalogo', icon: IconPackage, anyOf: ['catalog.read'] },
  { label: 'Assinaturas', to: '/assinaturas', icon: IconRepeat, anyOf: ['subscriptions.read'] },
  { label: 'Faturas', to: '/faturas', icon: IconFileInvoice, anyOf: ['invoices.read'] },
  {
    label: 'Pagamentos',
    icon: IconCreditCard,
    children: [
      { label: 'Cobranças', to: '/cobrancas', icon: IconTransfer, anyOf: ['payments.read'] },
      { label: 'Recebimentos', to: '/recebimentos', icon: IconCash, anyOf: ['payments.read'] },
      { label: 'Reembolsos', to: '/reembolsos', icon: IconReceiptRefund, anyOf: ['payments.read'] },
      { label: 'Disputas', to: '/disputas', icon: IconGavel, anyOf: ['payments.read'] },
      { label: 'Operações de pagamento', to: '/operacoes-pagamento', icon: IconAlertOctagon, anyOf: ['payments.read'] },
    ],
  },
  { label: 'Ledger', to: '/ledger', icon: IconBook2, anyOf: ['ledger.read'] },
  { label: 'Conciliação', to: '/conciliacao', icon: IconScale, anyOf: ['reconciliation.read'] },
  { label: 'Políticas de cobrança', to: '/cobranca', icon: IconBellRinging, anyOf: ['collections.read'] },
  { label: 'Usuários e permissões', to: '/usuarios', icon: IconShieldLock, anyOf: ['members.read'] },
  {
    label: 'Integrações',
    to: '/integracoes',
    icon: IconPlugConnected,
    anyOf: ['integrations.read', 'webhooks.read', 'api_credentials.manage'],
  },
  { label: 'Auditoria', to: '/auditoria', icon: IconHistory, anyOf: ['audit.read'] },
  { label: 'Operações', to: '/operacoes', icon: IconTool, anyOf: ['operations.read'] },
  { label: 'Organização', to: '/organizacao', icon: IconBuilding, anyOf: ['organization.read'] },
  { label: 'Plano Recorvia', to: '/plataforma', icon: IconBuildingStore, anyOf: ['platform_billing.read'] },
];

export function visibleNavigation(can: (p: Permission) => boolean): NavEntry[] {
  const allowed = (item: NavItem) => item.anyOf.some(can);
  return NAVIGATION.flatMap((entry): NavEntry[] => {
    if (!isGroup(entry)) return allowed(entry) ? [entry] : [];
    const children = entry.children.filter(allowed);
    return children.length ? [{ ...entry, children }] : [];
  });
}
