import { Button, Center, Stack, Text, Title } from '@mantine/core';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Fragment, useEffect, useRef, type ReactNode } from 'react';
import { Navigate, Outlet, useNavigate } from 'react-router';
import { setTenantMismatchHandler } from '../api/client';
import { isApiError } from '../api/http';
import { SESSION_KEY } from '../api/queryKeys';
import { ErrorState, FullPageLoader } from '../components/states';
import { notifyInfo } from '../lib/notify';
import { SessionProvider, useSession } from '../session/SessionContext';
import { fetchSession, recoverFromTenantMismatch } from '../session/sessionApi';
import { shouldRetry } from './providers';

/** Carrega a sessão; 401 leva o navegador ao login (feito pelo cliente HTTP). */
export function SessionGate() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const recovering = useRef(false);

  const query = useQuery({
    queryKey: SESSION_KEY,
    queryFn: fetchSession,
    staleTime: 60_000,
    retry: (count, error) => !isApiError(error, 401) && shouldRetry(count, error),
  });

  useEffect(() => {
    setTenantMismatchHandler(() => {
      if (recovering.current) return;
      recovering.current = true;
      void recoverFromTenantMismatch(queryClient)
        .then(() => {
          notifyInfo('A organização ativa foi trocada em outra aba. Os dados foram recarregados.', 'Organização alterada');
          navigate('/', { replace: true });
        })
        .finally(() => {
          recovering.current = false;
        });
    });
    return () => setTenantMismatchHandler(undefined);
  }, [queryClient, navigate]);

  if (query.isPending) return <FullPageLoader label="Carregando sua sessão…" />;
  if (query.isError) {
    return (
      <Center mih="100vh" p="md">
        <Stack maw={560} w="100%">
          <ErrorState error={query.error} onRetry={() => void query.refetch()} />
        </Stack>
      </Center>
    );
  }
  return (
    <SessionProvider session={query.data}>
      <Outlet />
    </SessionProvider>
  );
}

/**
 * Rotas internas exigem organização ativa; sem organização vai ao onboarding,
 * exceto a equipe da plataforma, que segue para o atendimento de serviços.
 */
export function RequireTenant({ children }: { children: ReactNode }) {
  const { session, tenant } = useSession();
  if (session.organizations.length === 0) {
    return <Navigate to={session.platform_staff ? '/admin/servicos' : '/organizacoes/nova'} replace />;
  }
  if (!tenant) return <Navigate to="/organizacoes" replace />;
  // A troca de organização remonta toda a área interna (sem estado residual).
  return <Fragment key={`${session.user.id}:${tenant.id}`}>{children}</Fragment>;
}

export function NotFoundPage() {
  return (
    <Center py="xl">
      <Stack align="center" gap="xs">
        <Title order={2}>Página não encontrada</Title>
        <Text c="dimmed">O endereço acessado não existe.</Text>
        <Button component="a" href="/" variant="light">
          Ir para o início
        </Button>
      </Stack>
    </Center>
  );
}
