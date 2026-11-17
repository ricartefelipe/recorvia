import { screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { CustomerForm } from './CustomerForm';

function created(body: Record<string, unknown>) {
  return {
    id: '0195f1a0-0000-7000-8000-00000000d777',
    document_type: body.kind === 'company' ? 'cnpj' : 'cpf',
    status: 'active',
    created_at: '2026-11-16T12:00:00Z',
    updated_at: '2026-11-16T12:00:00Z',
    version: 1,
    contacts: [],
    ...body,
  };
}

describe('formulário de cliente', () => {
  it('valida o formato do CPF para pessoa física', async () => {
    const { user } = renderPage(<CustomerForm onSaved={vi.fn()} />);
    await user.type(screen.getByLabelText(/Nome completo/), 'Maria Silva');
    await user.type(screen.getByLabelText(/^CPF/), '123.456');
    await user.click(screen.getByRole('button', { name: 'Cadastrar cliente' }));
    expect(await screen.findByText('CPF deve ter 11 dígitos, por exemplo 123.456.789-09.')).toBeInTheDocument();
    expect(screen.getByLabelText(/^CPF/)).toHaveAttribute('aria-invalid', 'true');
  });

  it('troca para CNPJ e razão social ao escolher pessoa jurídica', async () => {
    const { user } = renderPage(<CustomerForm onSaved={vi.fn()} />);
    await user.click(screen.getByLabelText('Pessoa jurídica'));
    expect(screen.getByLabelText(/Razão social/)).toBeInTheDocument();
    await user.type(screen.getByLabelText(/Nome fantasia/), 'Acme');
    await user.type(screen.getByLabelText(/^CNPJ/), '11.222.333/0001');
    await user.type(screen.getByLabelText('E-mail'), 'financeiro@');
    await user.click(screen.getByRole('button', { name: 'Cadastrar cliente' }));
    expect(await screen.findByText(/CNPJ deve ter 14 caracteres/)).toBeInTheDocument();
    expect(screen.getByText('Informe um e-mail válido.')).toBeInTheDocument();
  });

  it('aceita CNPJ alfanumérico e envia o documento normalizado', async () => {
    const requests: Record<string, unknown>[] = [];
    server.use(
      http.post('/api/v1/customers', async ({ request }) => {
        const body = (await request.json()) as Record<string, unknown>;
        requests.push(body);
        return HttpResponse.json(created(body), { status: 201 });
      }),
    );
    const onSaved = vi.fn();
    const { user } = renderPage(<CustomerForm onSaved={onSaved} />);
    await user.click(screen.getByLabelText('Pessoa jurídica'));
    await user.type(screen.getByLabelText(/Nome fantasia/), 'Acme Software');
    await user.type(screen.getByLabelText(/Razão social/), 'Acme Software Ltda');
    await user.type(screen.getByLabelText(/^CNPJ/), '12.abc.345/01de-35');
    await user.click(screen.getByRole('button', { name: 'Cadastrar cliente' }));
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(requests[0]).toMatchObject({
      kind: 'company',
      name: 'Acme Software',
      legal_name: 'Acme Software Ltda',
      document_number: '12ABC34501DE35',
      payment_terms_days: null,
    });
  });

  it('mostra no campo o erro devolvido pelo servidor (problem+json)', async () => {
    server.use(
      http.post('/api/v1/customers', () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'Dados inválidos',
            status: 422,
            code: 'validation_failed',
            detail: 'Revise os campos.',
            errors: [{ field: 'document_number', message: 'Dígitos verificadores do CPF não conferem.' }],
          },
          { status: 422, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    );
    const { user } = renderPage(<CustomerForm onSaved={vi.fn()} />);
    await user.type(screen.getByLabelText(/Nome completo/), 'João Souza');
    await user.type(screen.getByLabelText(/^CPF/), '111.444.777-00');
    await user.click(screen.getByRole('button', { name: 'Cadastrar cliente' }));
    expect(await screen.findByText('Dígitos verificadores do CPF não conferem.')).toBeInTheDocument();
    expect(screen.getByLabelText(/^CPF/)).toHaveAttribute('aria-invalid', 'true');
  });
});
