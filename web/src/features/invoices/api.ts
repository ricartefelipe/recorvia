import { api } from '../../api/client';
import type { TextWithHeaders } from '../../api/http';
import type {
  BillingRunResult,
  Charge,
  InvoiceDetails,
  InvoicePage,
  InvoiceStatus,
  ManualPaymentInput,
  Payment,
} from '../../api/types';

const enc = encodeURIComponent;
const base = (id: string) => `/api/v1/invoices/${enc(id)}`;

export interface InvoiceListParams {
  status?: InvoiceStatus;
  customer_id?: string;
  subscription_id?: string;
  issued_from?: string;
  issued_to?: string;
  overdue?: boolean;
  /** Somente com saldo em aberto (abertas ou parcialmente pagas), a base dos recebíveis em aberto do painel. */
  open_balance?: boolean;
  cursor?: string;
  limit?: number;
}

/**
 * Uma parte da exportação CSV. Cada arquivo tem no máximo `limit` faturas;
 * quando há mais, `truncated` é verdadeiro e `nextCursor` pede a parte
 * seguinte com os mesmos filtros.
 */
export interface InvoiceExportPart {
  csv: string;
  truncated: boolean;
  nextCursor?: string;
  limit?: number;
}

export const invoicesApi = {
  list: (params: InvoiceListParams, signal?: AbortSignal) =>
    api.get<InvoicePage>('/api/v1/invoices', { query: { ...params }, signal }),
  exportCsv: async (
    params: Pick<InvoiceListParams, 'status' | 'issued_from' | 'issued_to' | 'cursor'> & { part?: number },
  ): Promise<InvoiceExportPart> => {
    const { text, headers } = await api.get<TextWithHeaders>('/api/v1/invoices/export', {
      query: { ...params },
      responseType: 'textWithHeaders',
    });
    const limit = Number(headers.get('X-Recorvia-Export-Limit'));
    return {
      csv: text,
      truncated: headers.get('X-Recorvia-Export-Truncated') === 'true',
      nextCursor: headers.get('X-Recorvia-Export-Next-Cursor') || undefined,
      limit: Number.isFinite(limit) && limit > 0 ? limit : undefined,
    };
  },
  get: (id: string, signal?: AbortSignal) => api.get<InvoiceDetails>(base(id), { signal }),
  document: (id: string) => api.get<string>(`${base(id)}/document`, { responseType: 'text' }),
  void: (invoice: InvoiceDetails, reason: string) =>
    api.post<InvoiceDetails>(`${base(invoice.id)}/void`, { reason }, { ifMatch: invoice.version }),
  markUncollectible: (invoice: InvoiceDetails, reason: string) =>
    api.post<InvoiceDetails>(`${base(invoice.id)}/mark-uncollectible`, { reason }, { ifMatch: invoice.version }),
  creditNote: (id: string, input: { amount: number; reason: string }, idempotencyKey: string) =>
    api.post<InvoiceDetails>(`${base(id)}/credit-notes`, input, { idempotencyKey }),
  recordPayment: (id: string, input: ManualPaymentInput, idempotencyKey: string) =>
    api.post<Payment>(`${base(id)}/payments`, input, { idempotencyKey }),
  retryCharge: (id: string, idempotencyKey: string) =>
    api.post<Charge>(`${base(id)}/charges`, undefined, { idempotencyKey }),
  runBilling: (idempotencyKey: string) =>
    api.post<BillingRunResult>('/api/v1/billing-runs', undefined, { idempotencyKey }),
};

/** Baixa um conteúdo textual gerado sob demanda como arquivo. */
export function downloadText(content: string, filename: string, type: string): void {
  const blob = new Blob([content], { type });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
