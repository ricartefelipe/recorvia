import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { PERMISSIONS } from '../../session/permissions';
import { makeSession } from '../../test/fixtures';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { CustomerDetailPage } from './CustomerDetailPage';

const ID = '0195f1a0-0000-7000-8000-00000000d001';

const customer = {
  id: ID,
  kind: 'individual',
  name: 'Marina Teles',
  legal_name: '',
  document_type: 'cpf',
  document_number: '52998224725',
  email: 'marina@titular.test',
  phone: '+5511988887777',
  external_id: 'crm-1',
  status: 'active',
  payment_terms_days: null,
  anonymized_at: null,
  created_at: '2026-11-01T12:00:00Z',
  updated_at: '2026-11-01T12:00:00Z',
  version: 3,
  contacts: [],
  billing_address: null,
};

const anonymized = {
  ...customer,
  name: 'Cliente anonimizado',
  document_number: '00000000000',
  email: '',
  phone: '',
  external_id: '',
  status: 'archived',
  anonymized_at: '2026-11-20T15:30:00Z',
  version: 4,
};

const anonymization = {
  customer: anonymized,
  already_anonymized: false,
  provider_customers: [{ provider: 'asaas', provider_customer_id: 'cus_000123' }],
  removed: { contacts: 2, billing_address: true, portal_links_revoked: 1, notifications: 5, provider_webhook_payloads: 3 },
};

/**
 * Renderiza a ficha. O GET devolve o estado corrente do cliente no servidor
 * simulado: `backend.current` muda quando o POST de anonimização é aceito,
 * como na API real, e `backend.reads` conta as leituras da ficha. Com
 * `backend.failing`, a leitura da ficha responde 503.
 */
function render(initial: typeof customer | typeof anonymized, permissions = PERMISSIONS as readonly (typeof PERMISSIONS)[number][]) {
  const backend = { current: initial, reads: 0, failing: false };
  server.use(
    http.get(`/api/v1/customers/${ID}`, () => {
      backend.reads += 1;
      if (backend.failing) {
        return HttpResponse.json(
          { type: 'about:blank', title: 'Serviço indisponível', status: 503, code: 'unavailable', detail: 'Tente novamente em instantes.' },
          { status: 503, headers: { 'Content-Type': 'application/problem+json' } },
        );
      }
      return HttpResponse.json(backend.current);
    }),
  );
  const page = renderPage(<CustomerDetailPage />, {
    path: '/clientes/:id',
    route: `/clientes/${ID}`,
    session: makeSession({ permissions }),
  });
  return { ...page, backend };
}

async function submitAnonymization(user: ReturnType<typeof render>['user'], reason: string) {
  await user.click(await screen.findByRole('button', { name: 'Anonimizar' }));
  const dialog = await screen.findByRole('dialog', { name: 'Anonimizar cliente' });
  await user.type(within(dialog).getByLabelText(/Justificativa/), reason);
  await user.click(within(dialog).getByRole('button', { name: 'Continuar' }));
  await user.click(await screen.findByRole('button', { name: 'Anonimizar definitivamente' }));
}

describe('anonimização de cliente', () => {
  it('sem a permissão customers.anonymize a ação não aparece', async () => {
    render(
      customer,
      PERMISSIONS.filter((p) => p !== 'customers.anonymize'),
    );
    expect(await screen.findByRole('heading', { name: 'Marina Teles' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Editar' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Anonimizar' })).not.toBeInTheDocument();
  });

  it('exige justificativa e confirmação, envia Idempotency-Key e mostra os identificadores do provedor', async () => {
    const requests: { key: string | null; body: unknown }[] = [];
    const { user, backend } = render(customer);
    server.use(
      http.post(`/api/v1/customers/${ID}/anonymize`, async ({ request }) => {
        requests.push({ key: request.headers.get('Idempotency-Key'), body: await request.json() });
        backend.current = anonymized;
        return HttpResponse.json(anonymization);
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Anonimizar' }));
    const dialog = await screen.findByRole('dialog', { name: 'Anonimizar cliente' });
    expect(within(dialog).getByText('Esta ação é irreversível')).toBeInTheDocument();
    expect(within(dialog).getByText(/Faturas emitidas, pagamentos e a auditoria são mantidos/)).toBeInTheDocument();

    // Justificativa curta não é enviada.
    await user.type(within(dialog).getByLabelText(/Justificativa/), 'pedido');
    await user.click(within(dialog).getByRole('button', { name: 'Continuar' }));
    expect(await within(dialog).findByText('A justificativa deve ter pelo menos 10 caracteres.')).toBeInTheDocument();
    expect(requests).toHaveLength(0);

    await user.clear(within(dialog).getByLabelText(/Justificativa/));
    await user.type(within(dialog).getByLabelText(/Justificativa/), 'Pedido de eliminação, protocolo 2026/0042');
    await user.click(within(dialog).getByRole('button', { name: 'Continuar' }));
    // Nada é enviado antes da confirmação.
    const confirm = await screen.findByRole('dialog', { name: 'Confirmar anonimização' });
    expect(within(confirm).getByText(/não poderão ser recuperados/)).toBeInTheDocument();
    expect(requests).toHaveLength(0);
    await user.click(within(confirm).getByRole('button', { name: 'Anonimizar definitivamente' }));

    const done = await screen.findByRole('dialog', { name: 'Cliente anonimizado' });
    expect(within(done).getByLabelText('Identificador no provedor asaas')).toHaveValue('cus_000123');
    expect(requests).toHaveLength(1);
    expect(requests[0].body).toEqual({ reason: 'Pedido de eliminação, protocolo 2026/0042' });
    expect(requests[0].key).toMatch(/^[0-9a-f-]{36}$/);
  });

  it('o diálogo com os identificadores do provedor continua aberto depois que a ficha recarrega com o cliente anonimizado', async () => {
    const { user, backend } = render(customer);
    server.use(
      http.post(`/api/v1/customers/${ID}/anonymize`, () => {
        backend.current = anonymized;
        return HttpResponse.json(anonymization);
      }),
    );
    await submitAnonymization(user, 'Pedido de eliminação, protocolo 2026/0042');

    // A ficha é recarregada depois do sucesso e passa a mostrar o cliente anonimizado.
    expect(await screen.findByText(/Dados pessoais removidos em 20\/11\/2026/)).toBeInTheDocument();
    expect(screen.getByText('Anonimizado')).toBeInTheDocument();
    expect(backend.reads).toBeGreaterThanOrEqual(2);
    expect(screen.queryByRole('button', { name: 'Anonimizar' })).not.toBeInTheDocument();
    expect(screen.queryByRole('dialog', { name: 'Anonimizar cliente' })).not.toBeInTheDocument();

    // Os identificadores já não estão no cadastro: o diálogo que os mostra não pode sumir com a recarga.
    const done = screen.getByRole('dialog', { name: 'Cliente anonimizado' });
    expect(within(done).getByLabelText('Identificador no provedor asaas')).toHaveValue('cus_000123');
    expect(within(done).getByText(/continuam registrados na auditoria da anonimização/)).toBeInTheDocument();

    // Só fecha quando a pessoa fecha.
    await user.click(within(done).getByRole('button', { name: 'Fechar' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Cliente anonimizado' })).not.toBeInTheDocument());
    expect(screen.queryByLabelText('Identificador no provedor asaas')).not.toBeInTheDocument();
  });

  it('repetição depois de resposta perdida reutiliza a chave e mostra os identificadores devolvidos de novo', async () => {
    const keys: (string | null)[] = [];
    const { user, backend } = render(customer);
    server.use(
      http.post(`/api/v1/customers/${ID}/anonymize`, ({ request }) => {
        keys.push(request.headers.get('Idempotency-Key'));
        if (keys.length === 1) {
          // A anonimização foi confirmada, mas a resposta não chegou.
          backend.current = anonymized;
          return HttpResponse.error();
        }
        return HttpResponse.json({
          ...anonymization,
          already_anonymized: true,
          removed: { contacts: 0, billing_address: false, portal_links_revoked: 0, notifications: 0, provider_webhook_payloads: 0 },
        });
      }),
    );
    await submitAnonymization(user, 'Pedido de eliminação, protocolo 2026/0042');
    await waitFor(() => expect(keys).toHaveLength(1));
    const dialog = await screen.findByRole('dialog', { name: 'Anonimizar cliente' });
    expect(screen.queryByRole('dialog', { name: 'Cliente anonimizado' })).not.toBeInTheDocument();

    await user.click(within(dialog).getByRole('button', { name: 'Continuar' }));
    await user.click(await screen.findByRole('button', { name: 'Anonimizar definitivamente' }));

    const done = await screen.findByRole('dialog', { name: 'Cliente anonimizado' });
    expect(within(done).getByLabelText('Identificador no provedor asaas')).toHaveValue('cus_000123');
    expect(keys).toHaveLength(2);
    expect(keys[1]).toBe(keys[0]);
    expect(await screen.findByText(/Dados pessoais removidos em 20\/11\/2026/)).toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'Cliente anonimizado' })).toBeInTheDocument();
  });

  it('o diálogo com os identificadores continua aberto quando a recarga da ficha falha e a ficha vira a tela de erro', async () => {
    const { user, backend } = render(customer);
    server.use(
      http.post(`/api/v1/customers/${ID}/anonymize`, () => {
        backend.current = anonymized;
        // A recarga disparada pelo sucesso (ou pela volta à janela) falha.
        backend.failing = true;
        return HttpResponse.json(anonymization);
      }),
    );
    await submitAnonymization(user, 'Pedido de eliminação, protocolo 2026/0042');

    // A ficha dá lugar à tela de erro, mesmo com os dados anteriores em cache.
    const retry = await screen.findByRole('button', { name: 'Tentar novamente', hidden: true });
    expect(backend.reads).toBeGreaterThanOrEqual(2);
    expect(screen.queryByRole('heading', { name: 'Marina Teles', hidden: true })).not.toBeInTheDocument();

    // Os identificadores já não estão no cadastro: o diálogo não pode sumir com a ficha.
    const done = screen.getByRole('dialog', { name: 'Cliente anonimizado' });
    expect(within(done).getByLabelText('Identificador no provedor asaas')).toHaveValue('cus_000123');

    // Nem com a nova tentativa de leitura, que traz o cliente já anonimizado.
    backend.failing = false;
    await user.click(retry);
    expect(await screen.findByText(/Dados pessoais removidos em 20\/11\/2026/)).toBeInTheDocument();
    expect(within(screen.getByRole('dialog', { name: 'Cliente anonimizado' })).getByLabelText('Identificador no provedor asaas')).toHaveValue(
      'cus_000123',
    );

    await user.click(within(screen.getByRole('dialog', { name: 'Cliente anonimizado' })).getByRole('button', { name: 'Fechar' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Cliente anonimizado' })).not.toBeInTheDocument());
  });

  it('resposta perdida e ficha recarregada antes da nova tentativa: o formulário continua aberto, explica e a repetição devolve os identificadores', async () => {
    const keys: (string | null)[] = [];
    const { user, backend, queryClient } = render(customer);
    server.use(
      http.post(`/api/v1/customers/${ID}/anonymize`, ({ request }) => {
        keys.push(request.headers.get('Idempotency-Key'));
        if (keys.length === 1) {
          // A anonimização foi confirmada, mas a resposta não chegou.
          backend.current = anonymized;
          return HttpResponse.error();
        }
        return HttpResponse.json({
          ...anonymization,
          already_anonymized: true,
          removed: { contacts: 0, billing_address: false, portal_links_revoked: 0, notifications: 0, provider_webhook_payloads: 0 },
        });
      }),
    );
    await submitAnonymization(user, 'Pedido de eliminação, protocolo 2026/0042');
    await waitFor(() => expect(keys).toHaveLength(1));
    expect(await screen.findByRole('dialog', { name: 'Anonimizar cliente' })).toBeInTheDocument();
    expect(screen.queryByText('Este cliente já consta como anonimizado')).not.toBeInTheDocument();

    // A ficha é recarregada antes da nova tentativa (volta à janela ou outra invalidação de clientes).
    await queryClient.invalidateQueries();
    expect(await screen.findByText(/Dados pessoais removidos em 20\/11\/2026/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Anonimizar', hidden: true })).not.toBeInTheDocument();

    // O formulário não some: avisa o que aconteceu e onde os identificadores ficam registrados.
    const dialog = screen.getByRole('dialog', { name: 'Anonimizar cliente' });
    expect(within(dialog).getByText('Este cliente já consta como anonimizado')).toBeInTheDocument();
    expect(within(dialog).getByText(/Envie novamente para receber os identificadores do cliente no provedor/)).toBeInTheDocument();
    expect(within(dialog).getByText(/evento de anonimização, na aba Histórico/)).toBeInTheDocument();
    expect(within(dialog).getByLabelText(/Justificativa/)).toHaveValue('Pedido de eliminação, protocolo 2026/0042');
    expect(screen.queryByRole('dialog', { name: 'Cliente anonimizado' })).not.toBeInTheDocument();

    // A repetição não pede nova confirmação (nada mais é alterado) e reutiliza a chave.
    await user.click(within(dialog).getByRole('button', { name: 'Enviar novamente' }));
    const done = await screen.findByRole('dialog', { name: 'Cliente anonimizado' });
    expect(within(done).getByLabelText('Identificador no provedor asaas')).toHaveValue('cus_000123');
    expect(screen.queryByRole('dialog', { name: 'Confirmar anonimização' })).not.toBeInTheDocument();
    expect(keys).toHaveLength(2);
    expect(keys[1]).toBe(keys[0]);
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Anonimizar cliente' })).not.toBeInTheDocument());
  });

  it('cancelar a confirmação não envia o pedido', async () => {
    let calls = 0;
    const { user } = render(customer);
    server.use(
      http.post(`/api/v1/customers/${ID}/anonymize`, () => {
        calls += 1;
        return HttpResponse.json({}, { status: 500 });
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Anonimizar' }));
    const dialog = await screen.findByRole('dialog', { name: 'Anonimizar cliente' });
    await user.type(within(dialog).getByLabelText(/Justificativa/), 'Pedido de eliminação, protocolo 7');
    await user.click(within(dialog).getByRole('button', { name: 'Continuar' }));
    const confirm = await screen.findByRole('dialog', { name: 'Confirmar anonimização' });
    await user.click(within(confirm).getByRole('button', { name: 'Cancelar' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Confirmar anonimização' })).not.toBeInTheDocument());
    expect(calls).toBe(0);
  });

  it('recusa da API (cliente com fatura em aberto) é exibida e o cliente continua como estava', async () => {
    const { user } = render(customer);
    server.use(
      http.post(`/api/v1/customers/${ID}/anonymize`, () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'Conflito',
            status: 409,
            code: 'customer_has_obligations',
            detail: 'O cliente possui assinaturas ativas ou faturas em aberto.',
          },
          { status: 409, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    );
    await user.click(await screen.findByRole('button', { name: 'Anonimizar' }));
    const dialog = await screen.findByRole('dialog', { name: 'Anonimizar cliente' });
    await user.type(within(dialog).getByLabelText(/Justificativa/), 'Pedido de eliminação, protocolo 7');
    await user.click(within(dialog).getByRole('button', { name: 'Continuar' }));
    await user.click(await screen.findByRole('button', { name: 'Anonimizar definitivamente' }));
    expect(await screen.findByText('O cliente possui assinaturas ativas ou faturas em aberto.')).toBeInTheDocument();
    expect(screen.queryByRole('dialog', { name: 'Cliente anonimizado' })).not.toBeInTheDocument();
  });

  it('cliente anonimizado é identificado e não oferece edição nem nova anonimização', async () => {
    render(anonymized);
    expect(await screen.findByRole('heading', { name: 'Cliente anonimizado' })).toBeInTheDocument();
    expect(screen.getByText('Anonimizado')).toBeInTheDocument();
    expect(screen.getByText(/Dados pessoais removidos em 20\/11\/2026/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Anonimizar' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Editar' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Arquivar' })).not.toBeInTheDocument();
  });

  it('cliente anonimizado indica que os identificadores do provedor estão no histórico e leva até ele', async () => {
    const { user } = render(anonymized);
    server.use(
      http.get(`/api/v1/customers/${ID}/history`, () =>
        HttpResponse.json({
          data: [
            {
              id: '0195f1a0-0000-7000-8000-00000000e001',
              occurred_at: '2026-11-20T15:30:00Z',
              actor_type: 'user',
              actor_label: 'Ana Prado',
              action: 'customer.anonymized',
              resource_type: 'customer',
              resource_id: ID,
              details: { provider_customers: [{ provider: 'asaas', provider_customer_id: 'cus_000123' }] },
            },
          ],
        }),
      ),
    );
    expect(await screen.findByText(/ficam registrados no\s+evento de anonimização, na aba/)).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'Dados' })).toHaveAttribute('aria-selected', 'true');

    await user.click(screen.getByRole('button', { name: 'Histórico' }));
    expect(await screen.findByText('customer.anonymized')).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'Histórico' })).toHaveAttribute('aria-selected', 'true');
    await user.click(screen.getByRole('button', { name: 'Mostrar detalhes do evento' }));
    expect(screen.getByText(/"provider_customer_id": "cus_000123"/)).toBeInTheDocument();
  });

  it('cliente não anonimizado não mostra o aviso dos identificadores', async () => {
    render(customer);
    expect(await screen.findByRole('heading', { name: 'Marina Teles' })).toBeInTheDocument();
    expect(screen.queryByText(/evento de anonimização/)).not.toBeInTheDocument();
  });
});
