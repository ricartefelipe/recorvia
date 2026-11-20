import { api } from '../../api/client';
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
  cursor?: string;
  limit?: number;
}

export const invoicesApi = {
  list: (params: InvoiceListParams, signal?: AbortSignal) =>
    api.get<InvoicePage>('/api/v1/invoices', { query: { ...params }, signal }),
  exportCsv: (params: Pick<InvoiceListParams, 'status' | 'issued_from' | 'issued_to'>) =>
    api.get<string>('/api/v1/invoices/export', { query: { ...params }, responseType: 'text' }),
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
