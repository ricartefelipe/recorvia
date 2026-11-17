import { Alert, Button, Center, Code, Stack, Text } from '@mantine/core';
import { IconAlertTriangle } from '@tabler/icons-react';
import { isRouteErrorResponse, useRouteError } from 'react-router';

/** Falhas inesperadas de renderização ou de carregamento de uma página. */
export function RouteErrorBoundary() {
  const error = useRouteError();
  const chunkFailed = error instanceof Error && /dynamically imported module|Importing a module script failed/i.test(error.message);
  const message = isRouteErrorResponse(error)
    ? `${error.status} ${error.statusText}`
    : chunkFailed
      ? 'Uma nova versão da aplicação foi publicada. Recarregue a página para continuar.'
      : 'Ocorreu um erro inesperado ao exibir esta página.';
  return (
    <Center p="xl">
      <Stack maw={560} w="100%">
        <Alert color="red" icon={<IconAlertTriangle />} title="Algo deu errado" role="alert">
          <Stack gap="xs">
            <Text size="sm">{message}</Text>
            {import.meta.env.DEV && error instanceof Error && <Code block>{error.message}</Code>}
            <Button variant="white" color="red" size="xs" onClick={() => window.location.reload()} w="fit-content">
              Recarregar página
            </Button>
          </Stack>
        </Alert>
      </Stack>
    </Center>
  );
}
