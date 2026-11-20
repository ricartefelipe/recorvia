import { createTheme, MantineProvider } from '@mantine/core';
import { DatesProvider } from '@mantine/dates';
import { ModalsProvider } from '@mantine/modals';
import { Notifications } from '@mantine/notifications';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import 'dayjs/locale/pt-br';
import { useEffect, type ReactNode } from 'react';
import { ApiError, NetworkError } from '../api/http';
import { LiveRegion, openConcurrencyModal } from '../components/ui';
import { setConcurrencyHandler } from '../lib/errors';
import '../lib/validation';

export const theme = createTheme({
  primaryColor: 'blue',
  defaultRadius: 'md',
  fontFamily: 'Inter, system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif',
  headings: { fontWeight: '650' },
  components: {
    Table: { defaultProps: { highlightOnHover: true, verticalSpacing: 'sm' } },
    Modal: { defaultProps: { centered: true } },
  },
});

/** Não repete erros de cliente (4xx); repete falhas de rede e 5xx até 2 vezes. */
export function shouldRetry(failureCount: number, error: unknown): boolean {
  if (error instanceof ApiError) return error.status >= 500 && failureCount < 2;
  if (error instanceof NetworkError) return failureCount < 2;
  return false;
}

export function createAppQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { staleTime: 30_000, retry: shouldRetry, refetchOnWindowFocus: true },
      mutations: { retry: false },
    },
  });
}

export function AppProviders({
  children,
  queryClient,
  env,
}: {
  children: ReactNode;
  queryClient: QueryClient;
  /** "test" desativa transições e portais do Mantine nos testes. */
  env?: 'default' | 'test';
}) {
  useEffect(() => {
    setConcurrencyHandler(openConcurrencyModal);
    return () => setConcurrencyHandler(undefined);
  }, []);

  return (
    <MantineProvider theme={theme} defaultColorScheme="light" env={env}>
      <DatesProvider settings={{ locale: 'pt-br', firstDayOfWeek: 0 }}>
        <QueryClientProvider client={queryClient}>
          <ModalsProvider labels={{ confirm: 'Confirmar', cancel: 'Cancelar' }}>
            <Notifications position="top-right" limit={4} />
            <LiveRegion />
            {children}
          </ModalsProvider>
        </QueryClientProvider>
      </DatesProvider>
    </MantineProvider>
  );
}
