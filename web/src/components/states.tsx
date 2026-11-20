import { Alert, Button, Center, Code, Group, Loader, Paper, Skeleton, Stack, Text, ThemeIcon, Title } from '@mantine/core';
import { IconAlertTriangle, IconFileUnknown, IconInbox, IconLock, IconRefresh } from '@tabler/icons-react';
import type { UseQueryResult } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { Link } from 'react-router';
import { isApiError } from '../api/http';
import { describeError } from '../lib/errors';

export function LoadingState({ variant = 'table', label = 'Carregando…' }: { variant?: 'table' | 'cards' | 'detail'; label?: string }) {
  return (
    <Stack gap="sm" aria-busy="true" aria-label={label}>
      {variant === 'cards' && (
        <Group grow>
          {[0, 1, 2, 3].map((i) => (
            <Skeleton key={i} height={96} radius="md" />
          ))}
        </Group>
      )}
      {variant === 'detail' && (
        <>
          <Skeleton height={28} width="40%" radius="sm" />
          <Skeleton height={120} radius="md" />
        </>
      )}
      {[0, 1, 2, 3, 4].map((i) => (
        <Skeleton key={i} height={variant === 'table' ? 36 : 20} radius="sm" />
      ))}
    </Stack>
  );
}

export function EmptyState({
  title,
  description,
  action,
  icon,
}: {
  title: string;
  description?: ReactNode;
  action?: ReactNode;
  icon?: ReactNode;
}) {
  return (
    <Paper withBorder p="xl" radius="md">
      <Stack align="center" gap="xs" ta="center">
        <ThemeIcon variant="light" size={48} radius="xl" color="gray" aria-hidden>
          {icon ?? <IconInbox size={26} />}
        </ThemeIcon>
        <Text fw={600}>{title}</Text>
        {description && (
          <Text size="sm" c="dimmed" maw={520}>
            {description}
          </Text>
        )}
        {action}
      </Stack>
    </Paper>
  );
}

export function AccessDenied({ message }: { message?: string }) {
  return (
    <Paper withBorder p="xl" radius="md" role="alert">
      <Stack align="center" gap="xs" ta="center">
        <ThemeIcon variant="light" color="orange" size={48} radius="xl" aria-hidden>
          <IconLock size={26} />
        </ThemeIcon>
        <Title order={3}>Acesso negado</Title>
        <Text size="sm" c="dimmed" maw={520}>
          {message ?? 'Seu papel nesta organização não permite acessar este conteúdo. Fale com um administrador.'}
        </Text>
      </Stack>
    </Paper>
  );
}

export function NotFoundState({ message }: { message?: string }) {
  return (
    <Paper withBorder p="xl" radius="md" role="alert">
      <Stack align="center" gap="xs" ta="center">
        <ThemeIcon variant="light" color="gray" size={48} radius="xl" aria-hidden>
          <IconFileUnknown size={26} />
        </ThemeIcon>
        <Title order={3}>Registro não encontrado</Title>
        <Text size="sm" c="dimmed" maw={520}>
          {message ?? 'O endereço não existe ou o registro não pertence à organização ativa.'}
        </Text>
        <Button component={Link} to="/" variant="light">
          Ir para o painel
        </Button>
      </Stack>
    </Paper>
  );
}

export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  if (isApiError(error, 403)) return <AccessDenied message={error.detail} />;
  if (isApiError(error, 404)) return <NotFoundState />;
  if (isApiError(error, 401)) {
    return (
      <Center py="xl">
        <Group gap="xs">
          <Loader size="sm" />
          <Text>Sessão expirada. Redirecionando para a entrada…</Text>
        </Group>
      </Center>
    );
  }
  const description = describeError(error);
  return (
    <Alert color="red" variant="light" title={description.title} icon={<IconAlertTriangle />} role="alert">
      <Stack gap="xs">
        <Text size="sm">{description.message}</Text>
        {description.requestId && (
          <Text size="sm">
            Código da requisição: <Code>{description.requestId}</Code>
          </Text>
        )}
        {onRetry && (
          <Group>
            <Button size="xs" variant="white" color="red" leftSection={<IconRefresh size={16} />} onClick={onRetry}>
              Tentar novamente
            </Button>
          </Group>
        )}
      </Stack>
    </Alert>
  );
}

/** Estados padrão de uma consulta: carregando, erro (403/404/5xx) e sucesso. */
export function QueryContent<T>({
  query,
  loading,
  children,
}: {
  query: UseQueryResult<T>;
  loading?: ReactNode;
  children: (data: T) => ReactNode;
}) {
  if (query.isPending) return <>{loading ?? <LoadingState />}</>;
  if (query.isError) return <ErrorState error={query.error} onRetry={() => void query.refetch()} />;
  return <>{children(query.data)}</>;
}

export function FullPageLoader({ label = 'Carregando…' }: { label?: string }) {
  return (
    <Center h="100vh" aria-busy="true">
      <Group gap="sm">
        <Loader />
        <Text>{label}</Text>
      </Group>
    </Center>
  );
}
