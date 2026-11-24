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
  Tooltip,
  UnstyledButton,
} from '@mantine/core';
import { useDisclosure, useMediaQuery } from '@mantine/hooks';
import { IconBuilding, IconChevronDown, IconDeviceDesktopOff, IconHeadset, IconLogout, IconPlus } from '@tabler/icons-react';
import { useEffect, useRef, useState } from 'react';
import { Link, NavLink as RouterNavLink, Outlet, useLocation } from 'react-router';
import { confirmAction } from '../components/ui';
import { handleMutationError } from '../lib/errors';
import { roleLabels } from '../lib/labels';
import { useSession } from '../session/SessionContext';
import { logout, logoutAll } from '../session/sessionApi';
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

/**
 * Seletor da organização ativa. No cabeçalho, a largura acompanha o nome mais
 * longo (até um limite) e o nome completo aparece na dica; no celular, o
 * seletor fica no menu lateral, com a largura toda.
 */
function TenantSelector({ fullWidth }: { fullWidth?: boolean }) {
  const { session, tenant } = useSession();
  const { switchTo, switching } = useTenantSwitch();
  if (!tenant) return null;
  const longest = Math.max(...session.organizations.map((o) => o.name.length));
  const select = (
    <Select
      aria-label="Organização ativa"
      label={fullWidth ? 'Organização ativa' : undefined}
      data={session.organizations.map((o) => ({ value: o.id, label: o.name }))}
      value={tenant.id}
      onChange={(value) => {
        if (value && value !== tenant.id) void switchTo(value);
      }}
      allowDeselect={false}
      leftSection={switching ? <Loader size={14} /> : <IconBuilding size={16} aria-hidden />}
      disabled={!!switching}
      w={fullWidth ? '100%' : `min(calc(${longest}ch + 4.5rem), 24rem)`}
      comboboxProps={{ withinPortal: true }}
    />
  );
  if (fullWidth) return select;
  return (
    <Tooltip label={tenant.name} openDelay={400} withArrow>
      <div>{select}</div>
    </Tooltip>
  );
}

/** Menu do usuário; assign permite substituir a navegação do navegador (testes). */
export function UserMenu({ assign }: { assign?: (url: string) => void }) {
  const { user, tenant, session } = useSession();
  const [leaving, setLeaving] = useState(false);
  const leave = async (action: () => Promise<void>) => {
    setLeaving(true);
    try {
      await action();
    } catch (error) {
      setLeaving(false);
      handleMutationError(error);
    }
  };
  const onLogout = () => leave(() => logout(assign));
  const onLogoutAll = async () => {
    const ok = await confirmAction({
      title: 'Encerrar todas as sessões',
      message:
        'Encerrar todas as suas sessões no Recorvia, em todos os navegadores e dispositivos, inclusive esta? Será preciso entrar de novo em cada um deles.',
      confirmLabel: 'Encerrar todas',
      danger: true,
    });
    if (ok) await leave(() => logoutAll(assign));
  };
  return (
    <Menu position="bottom-end" withArrow>
      <Menu.Target>
        {/* O nome acessível contém o texto visível (WCAG 2.5.3): sem aria-label. */}
        <UnstyledButton>
          <Group gap="xs" wrap="nowrap">
            <Avatar radius="xl" size="sm" color="blue" aria-hidden>
              {initials(user.display_name || user.email)}
            </Avatar>
            <span className="visually-hidden">Menu do usuário </span>
            <Text size="sm" visibleFrom="sm" lineClamp={1} aria-hidden>
              {user.display_name}
            </Text>
            <span className="visually-hidden">{user.display_name}</span>
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
        {session.platform_staff && (
          <Menu.Item component={Link} to="/admin/servicos" leftSection={<IconHeadset size={16} />}>
            Atendimento de serviços
          </Menu.Item>
        )}
        <Menu.Divider />
        <Menu.Item leftSection={<IconDeviceDesktopOff size={16} />} onClick={() => void onLogoutAll()} disabled={leaving}>
          Encerrar todas as sessões
        </Menu.Item>
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
  // Abaixo do breakpoint sm, a troca de organização vai para o menu lateral.
  const wide = useMediaQuery('(min-width: 48em)', true, { getInitialValueInEffect: false });

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
            <Text component={Link} to="/" fw={750} size="lg" c="blue" style={{ textDecoration: 'none' }}>
              Recorvia
            </Text>
          </Group>
          <Group gap="sm" wrap="nowrap">
            {wide && <TenantSelector />}
            <UserMenu />
          </Group>
        </Group>
      </AppShell.Header>
      <AppShell.Navbar p="xs" aria-label="Navegação principal">
        {!wide && (
          <AppShell.Section p="xs" pb="sm">
            <TenantSelector fullWidth />
          </AppShell.Section>
        )}
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
