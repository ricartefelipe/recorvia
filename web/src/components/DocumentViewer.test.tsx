import { screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { renderPage } from '../test/render';
import { DocumentViewer } from './DocumentViewer';

function viewer(props: Partial<Parameters<typeof DocumentViewer>[0]> = {}) {
  return (
    <DocumentViewer
      opened
      onClose={() => undefined}
      title="Fatura ACME-000123"
      html="<!doctype html><html><body><h1>Fatura ACME-000123</h1></body></html>"
      filename="fatura-ACME-000123.html"
      {...props}
    />
  );
}

describe('visualizador do documento da fatura', () => {
  it('avisa, fora do documento, que é documento comercial e não fiscal', async () => {
    renderPage(viewer());
    const dialog = await screen.findByRole('dialog', { name: 'Fatura ACME-000123' });
    expect(within(dialog).getByText('Documento comercial. Não é documento fiscal.')).toBeInTheDocument();
    expect(within(dialog).getByTitle('Fatura ACME-000123')).toHaveAttribute('sandbox', 'allow-same-origin allow-modals');
  });

  it('mantém o aviso enquanto carrega e quando o documento falha', async () => {
    const { unmount } = renderPage(viewer({ html: null, loading: true }));
    const loading = await screen.findByRole('dialog', { name: 'Fatura ACME-000123' });
    expect(within(loading).getByText('Documento comercial. Não é documento fiscal.')).toBeInTheDocument();
    expect(within(loading).getByRole('button', { name: 'Imprimir' })).toBeDisabled();
    unmount();

    renderPage(viewer({ html: null, error: new Error('falha de rede') }));
    const failed = await screen.findByRole('dialog', { name: 'Fatura ACME-000123' });
    expect(within(failed).getByText('Documento comercial. Não é documento fiscal.')).toBeInTheDocument();
    expect(within(failed).getByText('Não foi possível carregar o documento')).toBeInTheDocument();
    expect(within(failed).getByRole('button', { name: 'Baixar HTML' })).toBeDisabled();
  });
});
