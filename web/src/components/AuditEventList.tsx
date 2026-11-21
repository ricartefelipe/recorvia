import { ActionIcon, Badge, Code, Collapse, Group, Stack, Table, Text } from '@mantine/core';
import { IconChevronDown, IconChevronRight } from '@tabler/icons-react';
import { Fragment, useState } from 'react';
import type { AuditEvent } from '../api/types';
import { actorTypeLabels, labelOf } from '../lib/labels';
import { DateTime, JsonView } from './format';
import { EmptyState } from './states';

/**
 * Quem executou a ação. Ações da equipe da plataforma (atendimento de
 * serviços adicionais) são feitas por quem não é membro da organização: o
 * nome vem identificado como da equipe, para não parecer um usuário
 * desconhecido.
 */
export function actorText(event: AuditEvent): string {
  const staff = (event.details as Record<string, unknown> | null | undefined)?.platform_staff === true;
  if (event.actor_label) return staff ? `${event.actor_label} (equipe da plataforma Recorvia)` : event.actor_label;
  const type = staff ? 'Equipe da plataforma Recorvia' : labelOf(actorTypeLabels, event.actor_type);
  return event.actor_id ? `${type} ${event.actor_id.slice(0, 8)}` : type;
}

/** Eventos de auditoria com detalhes JSON legíveis sob demanda. */
export function AuditEventList({ events, showResource = true }: { events: AuditEvent[]; showResource?: boolean }) {
  const [open, setOpen] = useState<string | null>(null);
  if (events.length === 0) {
    return <EmptyState title="Nenhum evento registrado" description="As alterações feitas neste recurso aparecerão aqui." />;
  }
  return (
    <Table.ScrollContainer minWidth={720}>
      <Table>
        <Table.Thead>
          <Table.Tr>
            <Table.Th w={40}>
              <span className="visually-hidden">Detalhes</span>
            </Table.Th>
            <Table.Th>Quando</Table.Th>
            <Table.Th>Ação</Table.Th>
            {showResource && <Table.Th>Recurso</Table.Th>}
            <Table.Th>Ator</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {events.map((event) => {
            const expanded = open === event.id;
            const detailsId = `audit-details-${event.id}`;
            return (
              <Fragment key={event.id}>
                <Table.Tr>
                  <Table.Td>
                    <ActionIcon
                      variant="subtle"
                      size="sm"
                      onClick={() => setOpen(expanded ? null : event.id)}
                      aria-expanded={expanded}
                      aria-controls={detailsId}
                      aria-label={expanded ? 'Ocultar detalhes do evento' : 'Mostrar detalhes do evento'}
                    >
                      {expanded ? <IconChevronDown size={16} /> : <IconChevronRight size={16} />}
                    </ActionIcon>
                  </Table.Td>
                  <Table.Td>
                    <DateTime value={event.occurred_at} />
                  </Table.Td>
                  <Table.Td>
                    <Code>{event.action}</Code>
                  </Table.Td>
                  {showResource && (
                    <Table.Td>
                      <Group gap={6} wrap="nowrap">
                        <Badge variant="light" color="gray">
                          {event.resource_type}
                        </Badge>
                        {event.resource_id && (
                          <Text size="xs" c="dimmed" ff="monospace" title={event.resource_id}>
                            {event.resource_id.slice(0, 8)}
                          </Text>
                        )}
                      </Group>
                    </Table.Td>
                  )}
                  <Table.Td>{actorText(event)}</Table.Td>
                </Table.Tr>
                <Table.Tr id={detailsId} style={{ display: expanded ? undefined : 'none' }}>
                  <Table.Td colSpan={showResource ? 5 : 4} bg="gray.0">
                    <Collapse in={expanded}>
                      <Stack gap="xs" py="xs">
                        {event.request_id && (
                          <Text size="xs" c="dimmed">
                            Requisição: <Code>{event.request_id}</Code>
                          </Text>
                        )}
                        <JsonView value={event.details} />
                      </Stack>
                    </Collapse>
                  </Table.Td>
                </Table.Tr>
              </Fragment>
            );
          })}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
  );
}
