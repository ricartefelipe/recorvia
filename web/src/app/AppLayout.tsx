import {
  Anchor,
  AppShell,
  Avatar,
  Burger,
  Group,
  Loader,
  Menu,
  NavLink,
  ScrollArea,
  Select,
  Text,
  UnstyledButton,
} from '@mantine/core';
import { useDisclosure } from '@mantine/hooks';
import { IconBuilding, IconChevronDown, IconLogout, IconPlus } from '@tabler/icons-react';
import { useEffect, useRef, useState } from 'react';
import { Link, NavLink as RouterNavLink, Outlet, useLocation } from 'react-router';
import { handleMutationError } from '../lib/errors';
import { roleLabels } from '../lib/labels';
import { useSession } from '../session/SessionContext';
import { logout } from '../session/sessionApi';
import { useTenantSwitch } from '../session/useTenantSwitch';
import { isGroup, visibleNavigation, type NavItem } from './navigation';

function initials(name: string): string {
  return name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((p) => p[0]?.toUpperCase())
    .join('');
}

function TenantSelector() {
  const { session, tenant } = useSession();
  const { switchTo, switching } = useTenantSwitch();
  if (!tenant) return null;
  return (
    <Select
      aria-label="Organização ativa"
      data={session.organizations.map((o) => ({ value: o.id, label: o.name }))}
      value={tenant.id}
      onChange={(value) => {
        if (value && value !== tenant.id) void switchTo(value);
      }}
      allowDeselect={false}
      leftSection={switching ? <Loader size={14} /> : <IconBuilding size={16} />}
      disabled={!!switching}
      w={{ base: 180, sm: 260 }}
      comboboxProps={{ withinPortal: true }}
    />
  );
}

function UserMenu() {
  const { user, tenant } = useSession();
  const [leaving, setLeaving] = useState(false);
  const onLogout = async () => {
    setLeaving(true);
    try {
      await logout();
    } catch (error) {
      setLeaving(false);
      handleMutationError(error);
    }
  };
  return (
    <Menu position="bottom-end" withArrow>
      <Menu.Target>
        <UnstyledButton aria-label={`Menu do usuário ${user.display_name}`}>
          <Group gap="xs" wrap="nowrap">
            <Avatar radius="xl" size="sm" color="blue">
              {initials(user.display_name || user.email)}
            </Avatar>
            <Text size="sm" visibleFrom="sm" lineClamp={1}>
              {user.display_name}
            </Text>
            <IconChevronDown size={14} aria-hidden />
          </Group>
        </UnstyledButton>
      </Menu.Target>
      <Menu.Dropdown>
        <Menu.Label>
          {user.email}
          {tenant && ` · ${roleLabels[tenant.role] ?? tenant.role}`}
        </Menu.Label>
        <Menu.Item component={Link} to="/organizacoes" leftSection={<IconBuilding size={16} />}>
          Organizações
        </Menu.Item>
        <Menu.Item component={Link} to="/organizacoes/nova" leftSection={<IconPlus size={16} />}>
          Criar organização
        </Menu.Item>
        <Menu.Divider />
        <Menu.Item
          color="red"
          leftSection={leaving ? <Loader size={14} /> : <IconLogout size={16} />}
          onClick={() => void onLogout()}
          disabled={leaving}
        >
          Sair
        </Menu.Item>
      </Menu.Dropdown>
    </Menu>
  );
}

function NavItemLink({ item, onNavigate }: { item: NavItem; onNavigate: () => void }) {
  const Icon = item.icon;
  return (
    <NavLink
      component={RouterNavLink}
      to={item.to}
      end={item.end}
      label={item.label}
      leftSection={<Icon size={18} stroke={1.6} aria-hidden />}
      onClick={onNavigate}
    />
  );
}

export function AppLayout() {
  const { can } = useSession();
  const [opened, { toggle, close }] = useDisclosure(false);
  const location = useLocation();
  const mainRef = useRef<HTMLElement>(null);
  const firstRender = useRef(true);
  const nav = visibleNavigation(can);

  // Foco no conteúdo a cada troca de página (leitores de tela e teclado).
  useEffect(() => {
    if (firstRender.current) {
      firstRender.current = false;
      return;
    }
    mainRef.current?.focus({ preventScroll: true });
  }, [location.pathname]);

  return (
    <AppShell header={{ height: 60 }} navbar={{ width: 264, breakpoint: 'md', collapsed: { mobile: !opened } }} padding="lg">
      <Anchor
        href="#conteudo"
        className="skip-link"
        onClick={(e) => {
          e.preventDefault();
          mainRef.current?.focus();
        }}
      >
        Pular para o conteúdo
      </Anchor>
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between" wrap="nowrap">
          <Group gap="sm" wrap="nowrap">
            <Burger opened={opened} onClick={toggle} hiddenFrom="md" size="sm" aria-label="Abrir ou fechar navegação" />
            <Text component={Link} to="/" fw={750} size="lg" c="blue.7" style={{ textDecoration: 'none' }}>
              Recorvia
            </Text>
          </Group>
          <Group gap="sm" wrap="nowrap">
            <TenantSelector />
            <UserMenu />
          </Group>
        </Group>
      </AppShell.Header>
      <AppShell.Navbar p="xs" aria-label="Navegação principal">
        <AppShell.Section grow component={ScrollArea}>
          <nav aria-label="Seções">
            {nav.map((entry) =>
              isGroup(entry) ? (
                <NavLink
                  key={entry.label}
                  label={entry.label}
                  leftSection={<entry.icon size={18} stroke={1.6} aria-hidden />}
                  defaultOpened={entry.children.some((c) => location.pathname.startsWith(c.to))}
                  childrenOffset={28}
                >
                  {entry.children.map((child) => (
                    <NavItemLink key={child.to} item={child} onNavigate={close} />
                  ))}
                </NavLink>
              ) : (
                <NavItemLink key={entry.to} item={entry} onNavigate={close} />
              ),
            )}
          </nav>
        </AppShell.Section>
      </AppShell.Navbar>
      <AppShell.Main id="conteudo" ref={mainRef} tabIndex={-1} style={{ outline: 'none' }}>
        <Outlet />
      </AppShell.Main>
    </AppShell>
  );
}
