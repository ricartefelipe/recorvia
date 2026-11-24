import { Center, Paper, Stack, Text, Title } from '@mantine/core';
import type { ReactNode } from 'react';

export function AuthLayout({ title, subtitle, children, width = 460 }: { title: string; subtitle?: ReactNode; children: ReactNode; width?: number }) {
  return (
    <Center mih="100vh" p="md" bg="gray.0" component="main">
      <Paper withBorder shadow="sm" radius="lg" p="xl" w="100%" maw={width}>
        <Stack gap="lg">
          <Stack gap={4}>
            <Text fw={750} c="blue" size="lg">
              Recorvia
            </Text>
            <Title order={1} size="h2">
              {title}
            </Title>
            {subtitle && (
              <Text c="dimmed" size="sm">
                {subtitle}
              </Text>
            )}
          </Stack>
          {children}
        </Stack>
      </Paper>
    </Center>
  );
}
