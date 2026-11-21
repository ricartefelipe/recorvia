import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { AuditEvent } from '../api/types';
import { renderPage } from '../test/render';
import { actorText, AuditEventList } from './AuditEventList';

function event(overrides: Partial<AuditEvent>): AuditEvent {
  return {
    id: 'e1',
    occurred_at: '2026-11-20T12:00:00Z',
    actor_type: 'user',
    actor_id: '0195f1a0-0000-7000-8000-00000000aaaa',
    actor_label: 'Ana Owner',
    action: 'member.invited',
    resource_type: 'invitation',
    resource_id: 'r1',
    request_id: null,
    details: {},
    ...overrides,
  } as AuditEvent;
}

describe('autor dos eventos de auditoria', () => {
  it('identifica quem é da equipe da plataforma, que não é membro da organização', async () => {
    renderPage(
      <AuditEventList
        events={[
          event({}),
          event({ id: 'e2', action: 'platform.addon_completed', actor_label: 'Equipe Recorvia', details: { platform_staff: true } }),
        ]}
      />,
    );
    expect(await screen.findByText('Ana Owner')).toBeInTheDocument();
    expect(screen.getByText('Equipe Recorvia (equipe da plataforma Recorvia)')).toBeInTheDocument();
  });

  it('sem nome, usa o tipo do autor', () => {
    expect(actorText(event({ actor_label: '', details: { platform_staff: true } }))).toBe('Equipe da plataforma Recorvia 0195f1a0');
    expect(actorText(event({ actor_label: '', actor_id: undefined, actor_type: 'system' }))).not.toContain('equipe');
  });
});
