import { Button, Stack, Text } from '@mantine/core';
import { IconLogin } from '@tabler/icons-react';
import { useSearchParams } from 'react-router';
import { loginUrl } from '../../api/http';
import { AuthLayout } from './AuthLayout';

export function LoginPage() {
  const [params] = useSearchParams();
  const returnTo = params.get('return_to') ?? '/';
  return (
    <AuthLayout
      title="Entrar"
      subtitle="Assinaturas, faturamento recorrente, cobranças, inadimplência e conciliação em um só lugar."
    >
      <Stack gap="sm">
        <Button component="a" href={loginUrl(returnTo)} size="md" leftSection={<IconLogin size={18} />} fullWidth>
          Entrar
        </Button>
        <Text size="xs" c="dimmed">
          Você será levado ao provedor de identidade da sua empresa. O Recorvia não recebe sua senha.
        </Text>
      </Stack>
    </AuthLayout>
  );
}
