import { Anchor, AppShell, Button, Center, Container, Group, Paper, Stack, Text, ThemeIcon, Title } from '@mantine/core';
import { IconLink, IconLogout } from '@tabler/icons-react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';
import { NavLink as RouterNavLink, Outlet } from 'react-router';
import { portalApi, setPortalCsrfToken, setPortalUnauthorizedHandler } from '../../api/client';
import { isApiError } from '../../api/http';
import { PORTAL_SESSION_KEY } from '../../api/queryKeys';
import type { PortalSession } from '../../api/types';
import { ErrorState, FullPageLoader } from '../../components/states';
import { handleMutationError } from '../../lib/errors';

export async function fetchPortalSession(): Promise<PortalSession> {
  const session = await portalApi.get<PortalSession>('/api/portal/v1/session');
  setPortalCsrfToken(session.csrf_token);
  return session;
}

const PortalContext = createContext<PortalSession | null>(null);

export function usePortalSession(): PortalSession {
  const ctx = useContext(PortalContext);
  if (!ctx) throw new Error('usePortalSession fora do portal');
  return ctx;
}

/** Fuso usado no portal: o do navegador do cliente (datas civis não são convertidas). */
export const portalTimeZone = (() => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'America/Sao_Paulo';
  } catch {
    return 'America/Sao_Paulo';
  }
})();

function PortalMessage({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Center mih="100vh" p="md" bg="gray.0" component="main">
      <Paper withBorder radius="lg" p="xl" maw={480} w="100%">
        <Stack align="center" ta="center" gap="sm">
          <ThemeIcon size={48} radius="xl" variant="light" aria-hidden>
            <IconLink size={26} />
          </ThemeIcon>
          <Title order={1} size="h3">
            {title}
          </Title>
          {children}
        </Stack>
      </Paper>
    </Center>
  );
}

export function PortalLayout() {
  const queryClient = useQueryClient();
  const [ended, setEnded] = useState(false);
  const [expired, setExpired] = useState(false);
  const [leaving, setLeaving] = useState(false);
  const query = useQuery({ queryKey: PORTAL_SESSION_KEY, queryFn: fetchPortalSession, retry: false, staleTime: 60_000 });

  useEffect(() => {
    setPortalUnauthorizedHandler(() => setExpired(true));
    return () => setPortalUnauthorizedHandler(undefined);
  }, []);

  const logout = async () => {
    setLeaving(true);
    try {
      await portalApi.post('/api/portal/v1/logout');
      setPortalCsrfToken(undefined);
      queryClient.removeQueries({ queryKey: ['portal'] });
      setEnded(true);
    } catch (error) {
      handleMutationError(error);
    } finally {
      setLeaving(false);
    }
  };

  if (ended) {
    return (
      <PortalMessage title="Sessão encerrada">
        <Text c="dimmed">Para acessar novamente, use o link recebido por e-mail.</Text>
      </PortalMessage>
    );
  }
  if (expired || isApiError(query.error, 401)) {
    return (
      <PortalMessage title="Acesse pelo link recebido">
        <Text c="dimmed">
          Não encontramos uma sessão ativa do portal. Acesse pelo link recebido por e-mail; ele é de uso único e tem
          validade limitada. Se o link expirou, peça um novo à empresa que emitiu a fatura.
        </Text>
      </PortalMessage>
    );
  }
  if (query.isPending) return <FullPageLoader label="Carregando o portal…" />;
  if (query.isError) {
    return (
      <Center mih="100vh" p="md">
        <Stack maw={560} w="100%">
          <ErrorState error={query.error} onRetry={() => void query.refetch()} />
        </Stack>
      </Center>
    );
  }

  const session = query.data;
  return (
    <PortalContext.Provider value={session}>
      <AppShell header={{ height: 60 }} padding="lg">
        <AppShell.Header>
          <Container size="lg" h="100%">
            <Group h="100%" justify="space-between" wrap="nowrap">
              <Stack gap={0}>
                <Text fw={700}>{session.organization.name}</Text>
                <Text size="xs" c="dimmed">
                  Portal do cliente · {session.customer.name}
                </Text>
              </Stack>
              <Group gap="sm" wrap="nowrap" component="nav" aria-label="Portal">
                <Anchor component={RouterNavLink} to="/portal" end size="sm">
                  Faturas
                </Anchor>
                <Anchor component={RouterNavLink} to="/portal/preferencias" size="sm">
                  Preferências de contato
                </Anchor>
                <Button variant="subtle" size="xs" leftSection={<IconLogout size={14} />} onClick={() => void logout()} loading={leaving}>
                  Sair
                </Button>
              </Group>
            </Group>
          </Container>
        </AppShell.Header>
        <AppShell.Main>
          <Container size="lg" component="main" id="conteudo">
            <Outlet />
          </Container>
        </AppShell.Main>
      </AppShell>
    </PortalContext.Provider>
  );
}
