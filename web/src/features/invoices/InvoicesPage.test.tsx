import { screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { renderPage } from '../../test/render';
import { server } from '../../test/server';
import { InvoicesPage } from './InvoicesPage';

interface Download {
  name: string;
  content: string;
}

let downloads: Download[];
const blobs = new Map<string, Blob>();

beforeEach(() => {
  downloads = [];
  blobs.clear();
  let seq = 0;
  URL.createObjectURL = vi.fn((blob: Blob) => {
    seq += 1;
    const url = `blob:teste/${seq}`;
    blobs.set(url, blob);
    return url;
  });
  URL.revokeObjectURL = vi.fn();
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
    const blob = blobs.get(this.href);
    if (blob) void blob.text().then((content) => downloads.push({ name: this.download, content }));
  });
  server.use(http.get('/api/v1/invoices', () => HttpResponse.json({ data: [] })));
});

afterEach(() => vi.restoreAllMocks());

function csvResponse(body: string, headers: Record<string, string>) {
  return new HttpResponse(body, { headers: { 'Content-Type': 'text/csv; charset=utf-8', 'X-Recorvia-Export-Limit': '50000', ...headers } });
}

describe('exportação de faturas', () => {
  it('arquivo completo: baixa um único CSV sem aviso de partes', async () => {
    const requests: URL[] = [];
    server.use(
      http.get('/api/v1/invoices/export', ({ request }) => {
        requests.push(new URL(request.url));
        return csvResponse('numero;cliente\nACME-1;Cliente\n', { 'X-Recorvia-Export-Truncated': 'false' });
      }),
    );
    const { user } = renderPage(<InvoicesPage />, { path: '/faturas', route: '/faturas?status=open' });
    await user.click(await screen.findByRole('button', { name: 'Exportar CSV' }));
    expect(await screen.findByText('Arquivo CSV gerado.')).toBeInTheDocument();
    await waitFor(() => expect(downloads).toHaveLength(1));
    expect(downloads[0].name).toMatch(/^faturas-\d{4}-\d{2}-\d{2}\.csv$/);
    expect(downloads[0].content).toBe('numero;cliente\nACME-1;Cliente\n');
    expect(requests[0].searchParams.get('status')).toBe('open');
    expect(requests[0].searchParams.has('cursor')).toBe(false);
    expect(screen.queryByText('Exportação em partes')).not.toBeInTheDocument();
  });

  it('corte informado pela API: avisa e baixa as partes seguintes com os mesmos filtros e o cursor', async () => {
    const requests: URL[] = [];
    server.use(
      http.get('/api/v1/invoices/export', ({ request }) => {
        const url = new URL(request.url);
        requests.push(url);
        switch (url.searchParams.get('cursor')) {
          case null:
            return csvResponse('numero\nA\n', { 'X-Recorvia-Export-Truncated': 'true', 'X-Recorvia-Export-Next-Cursor': 'cursor-2' });
          case 'cursor-2':
            return csvResponse('numero\nB\n', { 'X-Recorvia-Export-Truncated': 'true', 'X-Recorvia-Export-Next-Cursor': 'cursor-3' });
          default:
            return csvResponse('numero\nC\n', { 'X-Recorvia-Export-Truncated': 'false' });
        }
      }),
    );
    const { user } = renderPage(<InvoicesPage />, { path: '/faturas', route: '/faturas?status=paid&de=2026-01-01' });
    await user.click(await screen.findByRole('button', { name: 'Exportar CSV' }));

    expect(await screen.findByText('Exportação em partes')).toBeInTheDocument();
    expect(
      screen.getByText(/Cada arquivo tem no máximo 50\.000 faturas e ainda há faturas a exportar com os filtros aplicados\. O arquivo baixado é a parte 1/),
    ).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Baixar parte 2' }));
    await user.click(await screen.findByRole('button', { name: 'Baixar parte 3' }));
    expect(await screen.findByText('Parte 3 do CSV gerada. Exportação concluída.')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText('Exportação em partes')).not.toBeInTheDocument());

    await waitFor(() => expect(downloads).toHaveLength(3));
    expect(downloads.map((d) => d.content)).toEqual(['numero\nA\n', 'numero\nB\n', 'numero\nC\n']);
    expect(downloads.map((d) => d.name.replace(/\d{4}-\d{2}-\d{2}/, 'DATA'))).toEqual([
      'faturas-DATA-parte-1.csv',
      'faturas-DATA-parte-2.csv',
      'faturas-DATA-parte-3.csv',
    ]);
    expect(requests.map((u) => u.searchParams.get('cursor'))).toEqual([null, 'cursor-2', 'cursor-3']);
    expect(requests.map((u) => u.searchParams.get('part'))).toEqual([null, '2', '3']);
    for (const u of requests) {
      expect(u.searchParams.get('status')).toBe('paid');
      expect(u.searchParams.get('issued_from')).toBe('2026-01-01');
    }
  });
});
