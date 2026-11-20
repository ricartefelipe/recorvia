import { Alert, Button, Group, Stack, Text } from '@mantine/core';
import { IconAlertTriangle, IconMailOpened } from '@tabler/icons-react';
import { useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link, useNavigate, useSearchParams } from 'react-router';
import { describeError } from '../../lib/errors';
import { labelOf, roleLabels } from '../../lib/labels';
import { notifySuccess } from '../../lib/notify';
import { useSession } from '../../session/SessionContext';
import { acceptInvitation, performTenantSwitch } from '../../session/sessionApi';
import { AuthLayout } from './AuthLayout';

export function AcceptInvitationPage() {
  const [params] = useSearchParams();
  const token = params.get('token') ?? '';
  const { session } = useSession();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [state, setState] = useState<{ loading: boolean; error: string | null }>({ loading: false, error: null });
  const validToken = token.length >= 20 && token.length <= 200;

  const accept = async () => {
    setState({ loading: true, error: null });
    try {
      const result = await acceptInvitation(token);
      await performTenantSwitch(queryClient, result.tenant_id);
      notifySuccess(`Convite aceito. Seu papel: ${labelOf(roleLabels, result.role)}.`, 'Bem-vindo');
      navigate('/', { replace: true });
    } catch (error) {
      setState({ loading: false, error: describeError(error).message });
    }
  };

  return (
    <AuthLayout title="Convite para organização" subtitle={`Conectado como ${session.user.email}.`}>
      {!validToken ? (
        <Alert color="red" icon={<IconAlertTriangle />} title="Link de convite inválido">
          O endereço não contém um convite válido. Peça a quem convidou para enviar o link novamente.
        </Alert>
      ) : (
        <Stack>
          <Group gap="sm" wrap="nowrap">
            <IconMailOpened size={28} aria-hidden />
            <Text size="sm">
              Ao aceitar, você passa a fazer parte da organização com o papel definido no convite. O convite vale uma única
              vez e expira após o prazo informado.
            </Text>
          </Group>
          {state.error && (
            <Alert color="red" icon={<IconAlertTriangle />} title="Não foi possível aceitar o convite" role="alert">
              {state.error}
            </Alert>
          )}
          <Group justify="space-between">
            <Button component={Link} to="/organizacoes" variant="subtle">
              Agora não
            </Button>
            <Button onClick={() => void accept()} loading={state.loading}>
              Aceitar convite
            </Button>
          </Group>
        </Stack>
      )}
    </AuthLayout>
  );
}
