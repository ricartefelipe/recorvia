import { IconGauge, type Icon } from '@tabler/icons-react';
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

export const NAVIGATION: NavEntry[] = [{ label: 'Painel', to: '/', icon: IconGauge, anyOf: ['dashboard.read'], end: true }];

export function visibleNavigation(can: (p: Permission) => boolean): NavEntry[] {
  const allowed = (item: NavItem) => item.anyOf.some(can);
  return NAVIGATION.flatMap((entry): NavEntry[] => {
    if (!isGroup(entry)) return allowed(entry) ? [entry] : [];
    const children = entry.children.filter(allowed);
    return children.length ? [{ ...entry, children }] : [];
  });
}
