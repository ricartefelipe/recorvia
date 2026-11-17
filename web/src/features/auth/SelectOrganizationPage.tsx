import { Badge, Button, Group, Paper, Stack, Text } from '@mantine/core';
import { IconArrowRight, IconPlus } from '@tabler/icons-react';
import { Link, Navigate } from 'react-router';
import { labelOf, organizationStatusLabels, roleLabels } from '../../lib/labels';
import { useSession } from '../../session/SessionContext';
import { useTenantSwitch } from '../../session/useTenantSwitch';
import { AuthLayout } from './AuthLayout';

export function SelectOrganizationPage() {
  const { session, tenant } = useSession();
  const { switchTo, switching } = useTenantSwitch();

  if (session.organizations.length === 0) return <Navigate to="/organizacoes/nova" replace />;

  return (
    <AuthLayout
      title="Escolha a organização"
      subtitle={`Olá, ${session.user.display_name}. Selecione com qual organização deseja trabalhar.`}
      width={560}
    >
      <Stack gap="sm" component="ul" style={{ listStyle: 'none', padding: 0, margin: 0 }}>
        {session.organizations.map((org) => {
          const active = tenant?.id === org.id;
          return (
            <Paper key={org.id} withBorder p="md" radius="md" component="li">
              <Group justify="space-between" wrap="nowrap">
                <Stack gap={2}>
                  <Text fw={600}>{org.name}</Text>
                  <Group gap={6}>
                    <Badge variant="light" size="sm">
                      {labelOf(roleLabels, org.role)}
                    </Badge>
                    {org.status !== 'active' && (
                      <Badge variant="light" color="orange" size="sm">
                        {labelOf(organizationStatusLabels, org.status)}
                      </Badge>
                    )}
                    {active && (
                      <Badge variant="filled" size="sm">
                        Atual
                      </Badge>
                    )}
                  </Group>
                </Stack>
                {active ? (
                  <Button component={Link} to="/" variant="light" rightSection={<IconArrowRight size={16} />}>
                    Continuar
                  </Button>
                ) : (
                  <Button
                    onClick={() => void switchTo(org.id)}
                    loading={switching === org.id}
                    disabled={!!switching && switching !== org.id}
                    aria-label={`Entrar em ${org.name}`}
                  >
                    Entrar
                  </Button>
                )}
              </Group>
            </Paper>
          );
        })}
      </Stack>
      <Button component={Link} to="/organizacoes/nova" variant="subtle" leftSection={<IconPlus size={16} />}>
        Criar nova organização
      </Button>
    </AuthLayout>
  );
}
